package oauth2

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/nelthaarion/breeze"
)

// ---- PKCE ----

func TestPKCE(t *testing.T) {
	p := newPKCE()
	if p.Method != "S256" {
		t.Fatalf("method = %q, want S256", p.Method)
	}
	if len(p.Verifier) < 43 {
		t.Fatalf("verifier too short: %d", len(p.Verifier))
	}
	if !verifyPKCE(p.Verifier, p.Challenge) {
		t.Fatal("challenge does not verify against verifier")
	}
	if verifyPKCE("wrong", p.Challenge) {
		t.Fatal("wrong verifier must not verify")
	}
	// Two pairs must differ (randomness).
	if newPKCE().Verifier == p.Verifier {
		t.Fatal("PKCE verifier not random")
	}
}

func TestRandomStringUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		s := randomString(16)
		if seen[s] {
			t.Fatal("duplicate random string")
		}
		seen[s] = true
	}
}

// ---- Cookie signing ----

func TestSignedValueRoundTrip(t *testing.T) {
	secret := "s3cr3t"
	v := signedValue(secret, "hello world")
	got, ok := unsignValue(secret, v)
	if !ok || got != "hello world" {
		t.Fatalf("round trip failed: %q ok=%v", got, ok)
	}
	// Tampered payload must fail.
	if _, ok := unsignValue(secret, "AAAA."+strings.SplitN(v, ".", 2)[1]); ok {
		t.Fatal("tampered payload verified")
	}
	// Wrong secret must fail.
	if _, ok := unsignValue("other", v); ok {
		t.Fatal("wrong secret verified")
	}
}

func TestCookieBuildSecurityAttrs(t *testing.T) {
	c := cookieOptions{Name: "sess", Value: "x", Secure: true, HTTPOnly: true, MaxAge: 60}
	s := c.build()
	for _, want := range []string{"sess=x", "HttpOnly", "Secure", "SameSite=Lax", "Max-Age=60", "Path=/"} {
		if !strings.Contains(s, want) {
			t.Errorf("cookie %q missing %q", s, want)
		}
	}
}

func TestReadCookie(t *testing.T) {
	ctx := newCtx("", map[string]string{"a": "1", "b": "2"})
	if v, ok := readCookie(ctx, "b"); !ok || v != "2" {
		t.Fatalf("readCookie b = %q ok=%v", v, ok)
	}
	if _, ok := readCookie(ctx, "z"); ok {
		t.Fatal("missing cookie reported present")
	}
}

// ---- Config defaults ----

func TestConfigDefaults(t *testing.T) {
	restoreGoogle()
	cfg := Config{Provider: Google, ClientID: "id", ClientSecret: "sec"}
	if err := cfg.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if cfg.RedirectURL != "http://localhost:3000/auth/google/callback" {
		t.Errorf("redirect = %q", cfg.RedirectURL)
	}
	if len(cfg.Scopes) == 0 {
		t.Error("scopes not defaulted")
	}
	if cfg.CookieName == "" || cfg.SessionTTL == 0 || cfg.StateTTL == 0 {
		t.Error("defaults not applied")
	}
}

func TestConfigMissingCredentials(t *testing.T) {
	cfg := Config{Provider: Google}
	if err := cfg.normalize(); err != ErrMissingCredentials {
		t.Fatalf("err = %v, want ErrMissingCredentials", err)
	}
}

func TestAutoCallbackPath(t *testing.T) {
	restoreGoogle()
	cfg := Config{Provider: Google, ClientID: "id", ClientSecret: "sec", BaseURL: "https://x.io"}
	cfg.normalize()
	if got := callbackPath(&cfg); got != "/auth/google/callback" {
		t.Fatalf("callbackPath = %q", got)
	}
}

// ---- Provider drivers ----

func TestProviderStringRoundTrip(t *testing.T) {
	for _, p := range []Provider{Google, GitHub, Microsoft, Discord} {
		got, ok := ParseProvider(p.String())
		if !ok || got != p {
			t.Errorf("round trip %v -> %q -> %v ok=%v", p, p.String(), got, ok)
		}
	}
	if _, ok := ParseProvider("nope"); ok {
		t.Error("unknown provider parsed")
	}
}

func TestAllDriversRegistered(t *testing.T) {
	restoreGoogle()
	for _, p := range []Provider{Google, GitHub, Microsoft, Discord} {
		if _, ok := lookupDriver(p); !ok {
			t.Errorf("driver for %v not registered", p)
		}
	}
}

func TestDriverDefaultScopes(t *testing.T) {
	cases := map[Provider][]string{
		Google:    {"openid", "profile", "email"},
		GitHub:    {"read:user", "user:email"},
		Discord:   {"identify", "email"},
		Microsoft: {"openid", "profile", "email", "offline_access"},
	}
	for p, want := range cases {
		d, _ := lookupDriver(p)
		got := d.DefaultScopes()
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%v scopes = %v, want %v", p, got, want)
		}
	}
}

func TestAuthURLContainsParams(t *testing.T) {
	restoreGoogle()
	d, _ := lookupDriver(Google)
	cfg := Config{Provider: Google, ClientID: "cid", ClientSecret: "sec", BaseURL: "https://x.io"}
	cfg.normalize()
	u := d.AuthURL(&cfg, "state123", "nonce1", "chal1")
	for _, want := range []string{"client_id=cid", "state=state123", "code_challenge=chal1", "code_challenge_method=S256", "response_type=code"} {
		if !strings.Contains(u, want) {
			t.Errorf("auth URL missing %q: %s", want, u)
		}
	}
}

func TestGitHubPickPrimaryEmail(t *testing.T) {
	emails := []githubEmail{
		{Email: "a@x.com", Verified: false},
		{Email: "b@x.com", Verified: true},
		{Email: "c@x.com", Primary: true, Verified: true},
	}
	if got := pickPrimaryEmail(emails); got != "c@x.com" {
		t.Fatalf("pickPrimaryEmail = %q", got)
	}
}

// ---- JWT session ----

func TestJWTSessionRoundTrip(t *testing.T) {
	restoreGoogle()
	cfg := Config{Provider: Google, ClientID: "id", ClientSecret: "sec", SessionMode: SessionModeJWT}
	cfg.normalize()

	user := &User{ID: "u1", Email: "e@x.com", Provider: Google}
	tok := &Token{AccessToken: "at", RefreshToken: "rt", Expiry: time.Now().Add(time.Hour)}

	jwtStr, err := issueJWT(&cfg, user, tok)
	if err != nil {
		t.Fatalf("issueJWT: %v", err)
	}
	claims, err := parseJWT(&cfg, jwtStr)
	if err != nil {
		t.Fatalf("parseJWT: %v", err)
	}
	if claims.User.ID != "u1" || claims.Token.AccessToken != "at" {
		t.Fatal("claims mismatch")
	}
}

func TestJWTRejectsTampered(t *testing.T) {
	restoreGoogle()
	cfg := Config{Provider: Google, ClientID: "id", ClientSecret: "sec", SessionMode: SessionModeJWT}
	cfg.normalize()
	user := &User{ID: "u1"}
	jwtStr, _ := issueJWT(&cfg, user, &Token{AccessToken: "a"})
	if _, err := parseJWT(&cfg, jwtStr+"x"); err == nil {
		t.Fatal("tampered JWT accepted")
	}
}

// ---- State ----

func TestFlowStateRoundTrip(t *testing.T) {
	mp := newMockProvider(t)
	defer mp.Close()
	cfg := testConfig(t, mp, SessionModeCookie)

	ctx := newCtx("", nil)
	fs := newFlowState(&cfg, "verifier1")
	fs.save(ctx, &cfg)

	cookies := respCookies(ctx)
	ctx2 := newCtx("", cookies)
	got, err := loadFlowState(ctx2, &cfg)
	if err != nil {
		t.Fatalf("loadFlowState: %v", err)
	}
	if got.State != fs.State || got.Verifier != "verifier1" {
		t.Fatal("flow state mismatch")
	}
}

func TestFlowStateExpired(t *testing.T) {
	mp := newMockProvider(t)
	defer mp.Close()
	cfg := testConfig(t, mp, SessionModeCookie)
	cfg.StateTTL = time.Nanosecond
	cfg.ClockSkew = 0

	ctx := newCtx("", nil)
	fs := newFlowState(&cfg, "v")
	fs.IssuedAt = time.Now().Add(-time.Hour).Unix()
	fs.save(ctx, &cfg)

	time.Sleep(2 * time.Millisecond)
	_, err := loadFlowState(newCtx("", respCookies(ctx)), &cfg)
	if err != ErrExpiredState {
		t.Fatalf("err = %v, want ErrExpiredState", err)
	}
}

// ---- Full flow: Login -> Callback -> Auth ----

func TestFullFlowCookieMode(t *testing.T) {
	mp := newMockProvider(t)
	defer mp.Close()
	cfg := testConfig(t, mp, SessionModeCookie)

	// 1. Login: sets a flow-state cookie and redirects to the provider.
	loginCtx := newCtx("", nil)
	Login(cfg)(loginCtx)
	if status(loginCtx) != 302 {
		t.Fatalf("login status = %d", status(loginCtx))
	}
	stateCookies := respCookies(loginCtx)
	authURL := location(loginCtx)
	state := extractQuery(authURL, "state")
	if state == "" {
		t.Fatal("no state in auth URL")
	}

	// 2. Callback: presents the state cookie + matching state/code.
	cbCtx := newCtx("code=abc&state="+state, stateCookies)
	Callback(cfg)(cbCtx)
	if status(cbCtx) != 302 {
		t.Fatalf("callback status = %d", status(cbCtx))
	}
	if u := CurrentUser(cbCtx); u == nil || u.Email != "u@example.com" {
		t.Fatalf("callback did not set user: %+v", u)
	}
	sessionCookies := respCookies(cbCtx)

	// 3. Auth: a protected request with the session cookie succeeds. We build a
	// real two-element chain [Auth, handler] and drive it with ctx.Next(), the
	// same way the router does.
	protected := newCtx("", sessionCookies)
	called := false
	protected.SetMiddlewareChain(
		[]breeze.HandlerFunc{Auth(cfg)},
		func(*breeze.Context) error {
			called = true
			return nil
		},
	)
	protected.Next()
	if !called {
		t.Fatal("Auth did not call next handler")
	}
	if CurrentUser(protected) == nil {
		t.Fatal("Auth did not populate user")
	}
}

func TestCallbackRejectsBadState(t *testing.T) {
	mp := newMockProvider(t)
	defer mp.Close()
	cfg := testConfig(t, mp, SessionModeCookie)

	loginCtx := newCtx("", nil)
	Login(cfg)(loginCtx)
	stateCookies := respCookies(loginCtx)

	// Wrong state value.
	cbCtx := newCtx("code=abc&state=WRONG", stateCookies)
	Callback(cfg)(cbCtx)
	if status(cbCtx) != 401 {
		t.Fatalf("status = %d, want 401", status(cbCtx))
	}
}

func TestCallbackMissingCode(t *testing.T) {
	mp := newMockProvider(t)
	defer mp.Close()
	cfg := testConfig(t, mp, SessionModeCookie)
	cbCtx := newCtx("state=x", nil)
	Callback(cfg)(cbCtx)
	if status(cbCtx) != 400 {
		t.Fatalf("status = %d, want 400", status(cbCtx))
	}
}

// ---- Auth / Optional ----

func TestAuthRejectsNoSession(t *testing.T) {
	mp := newMockProvider(t)
	defer mp.Close()
	cfg := testConfig(t, mp, SessionModeCookie)

	ctx := newCtx("", nil)
	Auth(cfg)(ctx)
	if status(ctx) != 401 {
		t.Fatalf("status = %d, want 401", status(ctx))
	}
}

func TestOptionalContinuesWithoutSession(t *testing.T) {
	mp := newMockProvider(t)
	defer mp.Close()
	cfg := testConfig(t, mp, SessionModeCookie)

	ctx := newCtx("", nil)
	next := false
	ctx.SetMiddlewareChain(
		[]breeze.HandlerFunc{Optional(cfg)},
		func(*breeze.Context) error {
			next = true
			return nil
		},
	)
	ctx.Next()
	if !next {
		t.Fatal("Optional blocked the request")
	}

	if CurrentUser(ctx) != nil {
		t.Fatal("Optional set a user without a session")
	}
}

// ---- Logout ----

func TestLogoutClearsSession(t *testing.T) {
	mp := newMockProvider(t)
	defer mp.Close()
	cfg := testConfig(t, mp, SessionModeCookie)

	ctx := newCtx("", map[string]string{cfg.CookieName: "whatever"})
	Logout(cfg)(ctx)
	if status(ctx) != 302 {
		t.Fatalf("status = %d", status(ctx))
	}
	// The session cookie must be expired (Max-Age=0).
	raw := ctx.GetHeader("Set-Cookie")
	if !strings.Contains(raw, cfg.CookieName+"=;") && !strings.Contains(raw, cfg.CookieName+"=; ") {
		t.Fatalf("logout did not clear cookie: %s", raw)
	}
}

// ---- Open-redirect guard ----

func TestIsLocalRedirect(t *testing.T) {
	cases := map[string]bool{
		"/dash":         true,
		"/a/b":          true,
		"//evil.com":    false,
		"https://x.com": false,
		"":              false,
		"relative":      false,
	}
	for in, want := range cases {
		if got := isLocalRedirect(in); got != want {
			t.Errorf("isLocalRedirect(%q) = %v, want %v", in, got, want)
		}
	}
}

// ---- Race: concurrent driver registration + lookup ----

func TestConcurrentRegistryAccess(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); restoreGoogle() }()
		go func() { defer wg.Done(); lookupDriver(Google) }()
	}
	wg.Wait()
}

// ---- needsRefresh ----

func TestNeedsRefresh(t *testing.T) {
	cfg := &Config{ClockSkew: time.Minute}
	if needsRefresh(cfg, &Token{}) {
		t.Error("token with no expiry should not need refresh")
	}
	if !needsRefresh(cfg, &Token{Expiry: time.Now().Add(30 * time.Second)}) {
		t.Error("token expiring within skew should need refresh")
	}
	if needsRefresh(cfg, &Token{Expiry: time.Now().Add(time.Hour)}) {
		t.Error("token valid for an hour should not need refresh")
	}
}

// ---- Exchange via real HTTP helper hits the mock server ----

func TestExchangeAndUserInfo(t *testing.T) {
	mp := newMockProvider(t)
	defer mp.Close()
	cfg := testConfig(t, mp, SessionModeCookie)

	tok, err := cfg.driver.Exchange(context.Background(), &cfg, "code", "verifier")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if tok.AccessToken != "at-123" || tok.RefreshToken != "rt-456" {
		t.Fatalf("token = %+v", tok)
	}
	if mp.gotVerifier != "verifier" {
		t.Fatalf("verifier not sent: %q", mp.gotVerifier)
	}
	u, err := cfg.driver.UserInfo(context.Background(), &cfg, tok)
	if err != nil {
		t.Fatalf("UserInfo: %v", err)
	}
	if u.Email != "u@example.com" {
		t.Fatalf("user = %+v", u)
	}
}

// ---- ID token nonce ----

func TestIDTokenNonceExtractsClaim(t *testing.T) {
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"nonce": "abc123"})
	signed, err := token.SignedString([]byte("any-secret"))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	got, err := idTokenNonce(signed)
	if err != nil {
		t.Fatalf("idTokenNonce: %v", err)
	}
	if got != "abc123" {
		t.Fatalf("nonce = %q, want abc123", got)
	}
}

func TestIDTokenNonceMissingClaim(t *testing.T) {
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{})
	signed, _ := token.SignedString([]byte("any-secret"))
	got, err := idTokenNonce(signed)
	if err != nil {
		t.Fatalf("idTokenNonce: %v", err)
	}
	if got != "" {
		t.Fatalf("nonce = %q, want empty", got)
	}
}

// ---- Benchmarks ----

func BenchmarkSignVerify(b *testing.B) {
	secret := "benchsecret"
	v := signedValue(secret, "payload-data-here")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := unsignValue(secret, v); !ok {
			b.Fatal("verify failed")
		}
	}
}

func BenchmarkReadSessionCookie(b *testing.B) {
	restoreGoogle()
	cfg := Config{Provider: Google, ClientID: "id", ClientSecret: "sec", CookieSecret: "abc"}
	cfg.normalize()
	ctx := newCtx("", nil)
	writeSession(ctx, &cfg, &User{ID: "u"}, &Token{AccessToken: "a"})
	cookies := respCookies(ctx)
	read := newCtx("", cookies)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := readSession(read, &cfg); err != nil {
			b.Fatal(err)
		}
	}
}
