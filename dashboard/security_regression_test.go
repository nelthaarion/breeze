package dashboard

// security_regression_test.go — regression tests for the defects found in the dashboard review.
// Each test names the defect it holds shut; most of them failed against the code as it was.

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nelthaarion/breeze/v2"
)

// callRoute drives one request through the router exactly as the server dispatches it.
func callRoute(t *testing.T, router *breeze.Router, method breeze.Method, path string, header map[string]string, body string) *breeze.Context {
	t.Helper()
	if header == nil {
		header = map[string]string{}
	}
	req := &breeze.HTTPRequest{Method: method, Path: path, Header: header, Body: []byte(body)}
	handler, middlewares, params := router.Find(req)
	if handler == nil {
		t.Fatalf("no route resolves %s %s", method, path)
	}
	ctx := breeze.NewContext(method, path)
	ctx.Req = req
	ctx.SetParams(params)
	ctx.SetMiddlewareChain(middlewares, handler)
	ctx.Next()
	return ctx
}

func resBody(ctx *breeze.Context) string {
	if ctx.Res == nil {
		return ""
	}
	return string(ctx.Res.Body)
}

func resStatus(ctx *breeze.Context) int {
	if ctx.Res == nil {
		return 0
	}
	return ctx.Res.Status
}

func installed2(t *testing.T, mutate func(*Config)) (*breeze.Router, *Collector) {
	t.Helper()
	router := breeze.NewRouter()
	cfg := DefaultConfig()
	if mutate != nil {
		mutate(&cfg)
	}
	c := Install(nil, router, cfg)
	t.Cleanup(c.Close)
	return router, c
}

// ─── Authentication ──────────────────────────────────────────────────────

// The default Config has no credentials. The dashboard is meant to fail closed with a 503 —
// but the API handlers ran anyway and returned their data under that status.
func TestAPIRefusesWithoutCredentialsAndLeaksNothing(t *testing.T) {
	router, c := installed2(t, nil)
	c.RecordLog("app", LogEntry{Time: now(), Message: "marker-secret-line"})
	for _, p := range []string{"/dashboard/api/overview", "/dashboard/api/requests", "/dashboard/api/logs", "/dashboard/api/routes", "/dashboard/api/health"} {
		ctx := callRoute(t, router, breeze.GET, p, nil, "")
		if resStatus(ctx) != 503 {
			t.Errorf("%s: status %d, want 503", p, resStatus(ctx))
		}
		if b := resBody(ctx); strings.Contains(b, "marker-secret-line") || strings.Contains(b, "goroutines") || strings.Contains(b, `"metrics"`) {
			t.Errorf("%s: a refused request still carries dashboard data: %.160s", p, b)
		}
	}
}

// With no credentials the page routes answered 503 and then rendered the dashboard over it.
func TestPagesAreNotRenderedWhenAuthRefuses(t *testing.T) {
	router, _ := installed2(t, nil)
	for _, p := range []string{"/dashboard", "/dashboard/requests", "/dashboard/database"} {
		ctx := callRoute(t, router, breeze.GET, p, map[string]string{"accept": "text/html"}, "")
		if resStatus(ctx) != 503 || strings.Contains(strings.ToLower(resBody(ctx)), "<html") {
			t.Errorf("%s: status %d, body %.80q — want the 503 notice, not a page", p, resStatus(ctx), resBody(ctx))
		}
	}
}

// A throttled client (429) fell through to the handler too, so eight bad Basic-auth attempts
// bought a response containing the data.
func TestThrottledClientDoesNotReceiveData(t *testing.T) {
	router, c := installed2(t, func(cfg *Config) { cfg.Username, cfg.Password = "admin", "correct-horse" })
	c.RecordLog("app", LogEntry{Time: now(), Message: "marker-secret-line"})
	bad := map[string]string{"authorization": "Basic YWRtaW46d3Jvbmc="} // admin:wrong
	var last *breeze.Context
	for i := 0; i < maxLoginFailures+3; i++ {
		last = callRoute(t, router, breeze.GET, "/dashboard/api/overview", map[string]string{"authorization": bad["authorization"]}, "")
	}
	if resStatus(last) != 429 {
		t.Fatalf("status %d after repeated bad credentials, want 429", resStatus(last))
	}
	if strings.Contains(resBody(last), "goroutines") || strings.Contains(resBody(last), `"requests"`) {
		t.Errorf("a throttled response carries dashboard data: %.160s", resBody(last))
	}
}

func TestAuthenticatedRequestsStillWork(t *testing.T) {
	router, c := installed2(t, func(cfg *Config) { cfg.Username, cfg.Password = "admin", "correct-horse" })
	token, err := c.sessions.create("admin")
	if err != nil {
		t.Fatal(err)
	}
	ctx := callRoute(t, router, breeze.GET, "/dashboard/api/overview", map[string]string{"cookie": sessionCookieName + "=" + token}, "")
	if resStatus(ctx) != 0 && resStatus(ctx) != 200 {
		t.Fatalf("status %d for a valid session: %s", resStatus(ctx), resBody(ctx))
	}
	if !strings.Contains(resBody(ctx), "goroutines") && !strings.Contains(resBody(ctx), "metrics") {
		t.Errorf("an authenticated overview has no data: %.120s", resBody(ctx))
	}
	// Basic auth for API clients still works, and its answer is not a redirect.
	ctx = callRoute(t, router, breeze.GET, "/dashboard/api/overview", map[string]string{"authorization": "Basic YWRtaW46Y29ycmVjdC1ob3JzZQ=="}, "")
	if s := resStatus(ctx); s != 0 && s != 200 {
		t.Errorf("Basic auth status %d", s)
	}
}

// With no credentials configured the login endpoint compared two empty strings: an empty
// username and password signed in, and the WebSocket guard then honoured the cookie.
func TestEmptyCredentialsCannotSignInWhenUnconfigured(t *testing.T) {
	router, c := installed2(t, nil)
	ctx := callRoute(t, router, breeze.POST, "/dashboard/login", map[string]string{"content-type": "application/json"}, `{"username":"","password":""}`)
	if resStatus(ctx) != 503 {
		t.Errorf("login status %d, want 503", resStatus(ctx))
	}
	if ctx.Res != nil && ctx.Res.Headers["Set-Cookie"] != "" {
		t.Errorf("a session cookie was issued: %q", ctx.Res.Headers["Set-Cookie"])
	}
	// even a session that exists must not open the socket while auth is unconfigured
	token, _ := c.sessions.create("")
	wsCtx := callRoute(t, router, breeze.GET, "/dashboard/api/overview", nil, "")
	_ = wsCtx
	g := breeze.NewContext(breeze.GET, "/dashboard/ws")
	g.Req.Header["cookie"] = sessionCookieName + "=" + token
	if c.wsGuard(g) {
		t.Error("the WebSocket guard admitted a session while no credentials are configured")
	}
}

func TestWebSocketGuard(t *testing.T) {
	_, c := installed2(t, func(cfg *Config) { cfg.Username, cfg.Password = "admin", "correct-horse" })
	token, _ := c.sessions.create("admin")
	mk := func(h map[string]string) *breeze.Context {
		ctx := breeze.NewContext(breeze.GET, "/dashboard/ws")
		for k, v := range h {
			ctx.Req.Header[k] = v
		}
		return ctx
	}
	cookie := sessionCookieName + "=" + token
	cases := []struct {
		name string
		h    map[string]string
		want bool
	}{
		{"no cookie", map[string]string{}, false},
		{"valid cookie, no origin (a script)", map[string]string{"cookie": cookie}, true},
		{"valid cookie, same origin", map[string]string{"cookie": cookie, "origin": "http://localhost:3000", "host": "localhost:3000"}, true},
		{"valid cookie, other port of the same site", map[string]string{"cookie": cookie, "origin": "http://localhost:9999", "host": "localhost:3000"}, false},
		{"valid cookie, cross-site fetch metadata", map[string]string{"cookie": cookie, "sec-fetch-site": "cross-site"}, false},
		{"valid cookie, same-site fetch metadata", map[string]string{"cookie": cookie, "sec-fetch-site": "same-site"}, false},
		{"valid cookie, same-origin fetch metadata", map[string]string{"cookie": cookie, "sec-fetch-site": "same-origin"}, true},
		{"origin null", map[string]string{"cookie": cookie, "origin": "null"}, false},
	}
	for _, tc := range cases {
		if got := c.wsGuard(mk(tc.h)); got != tc.want {
			t.Errorf("%s: guard = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestWebSocketGuardWithAuthDisabled(t *testing.T) {
	_, c := installed2(t, func(cfg *Config) { cfg.DisableAuth = true })
	ctx := breeze.NewContext(breeze.GET, "/dashboard/ws")
	if !c.wsGuard(ctx) {
		t.Error("with authentication disabled the live socket can never connect: it demanded a cookie nobody can obtain")
	}
}

// SameSite=Strict does not stop another origin of the same site (another localhost port).
func TestUnsafeRequestsFromAnotherOriginAreRefused(t *testing.T) {
	router, c := installed2(t, func(cfg *Config) { cfg.Username, cfg.Password = "admin", "correct-horse" })
	token, _ := c.sessions.create("admin")
	cookie := sessionCookieName + "=" + token
	post := func(h map[string]string) int {
		h["cookie"] = cookie
		h["content-type"] = "application/json"
		return resStatus(callRoute(t, router, breeze.POST, "/dashboard/api/cache/clear", h, `{}`))
	}
	if s := post(map[string]string{"sec-fetch-site": "same-site"}); s != 403 {
		t.Errorf("same-site cross-origin POST: %d, want 403", s)
	}
	if s := post(map[string]string{"origin": "http://localhost:9999", "host": "localhost:3000"}); s != 403 {
		t.Errorf("foreign Origin POST: %d, want 403", s)
	}
	if s := post(map[string]string{"sec-fetch-site": "same-origin"}); s == 403 {
		t.Errorf("same-origin POST was refused")
	}
	if s := post(map[string]string{}); s == 403 {
		t.Errorf("a non-browser POST (no Origin, no fetch metadata) was refused")
	}
}

// With authentication switched off the browser is the only barrier: another site must not be able to
// make it write to the dashboard (the Database Browser, the cache, the explorer).
func TestCrossOriginWritesAreRefusedWithAuthDisabled(t *testing.T) {
	router, _ := installed2(t, func(cfg *Config) { cfg.DisableAuth = true })
	post := func(h map[string]string) int {
		h["content-type"] = "text/plain" // a "simple request" body, which needs no preflight
		return resStatus(callRoute(t, router, breeze.POST, "/dashboard/api/cache/clear", h, `{}`))
	}
	if s := post(map[string]string{"sec-fetch-site": "cross-site", "origin": "https://evil.example"}); s != 403 {
		t.Errorf("cross-site POST with auth disabled: %d, want 403", s)
	}
	if s := post(map[string]string{"sec-fetch-site": "same-origin"}); s == 403 {
		t.Errorf("same-origin POST was refused with auth disabled")
	}
	if s := post(map[string]string{}); s == 403 {
		t.Errorf("a non-browser POST was refused with auth disabled")
	}
}

func TestLoginRejectsCrossOriginAndThrottles(t *testing.T) {
	router, _ := installed2(t, func(cfg *Config) { cfg.Username, cfg.Password = "admin", "correct-horse" })
	h := map[string]string{"content-type": "application/json", "sec-fetch-site": "cross-site"}
	if s := resStatus(callRoute(t, router, breeze.POST, "/dashboard/login", h, `{"username":"admin","password":"correct-horse"}`)); s != 403 {
		t.Errorf("cross-site login: %d, want 403", s)
	}
	var last int
	for i := 0; i < maxLoginFailures+2; i++ {
		last = resStatus(callRoute(t, router, breeze.POST, "/dashboard/login", map[string]string{}, `{"username":"admin","password":"nope"}`))
	}
	if last != 429 {
		t.Errorf("login after repeated failures: %d, want 429", last)
	}
}

func TestLoginReturnsOnlyAValidatedNext(t *testing.T) {
	router, _ := installed2(t, func(cfg *Config) { cfg.Username, cfg.Password = "admin", "correct-horse" })
	login := func(next string) string {
		ctx := callRoute(t, router, breeze.POST, "/dashboard/login", map[string]string{}, fmt.Sprintf(`{"username":"admin","password":"correct-horse","next":%q}`, next))
		if resStatus(ctx) != 200 {
			t.Fatalf("login status %d: %s", resStatus(ctx), resBody(ctx))
		}
		return resBody(ctx)
	}
	if b := login("/dashboard/requests?x=1"); !strings.Contains(b, `"redirect":"/dashboard/requests?x=1"`) {
		t.Errorf("a dashboard page was not honoured: %s", b)
	}
	for _, evil := range []string{"//evil.example/x", "https://evil.example/", "/elsewhere", "/dashboard/api/overview", `/dashboard\evil`, "javascript:alert(1)"} {
		if b := login(evil); !strings.Contains(b, `"redirect":"/dashboard"`) {
			t.Errorf("next=%q was not replaced by the home page: %s", evil, b)
		}
	}
}

func TestBrowserNavigationRedirectsToLoginRememberingTheDestination(t *testing.T) {
	router, _ := installed2(t, func(cfg *Config) { cfg.Username, cfg.Password = "admin", "correct-horse" })
	ctx := callRoute(t, router, breeze.GET, "/dashboard/requests", map[string]string{"accept": "text/html"}, "")
	if resStatus(ctx) != 302 || ctx.Res.Headers["Location"] != "/dashboard/login?next=%2Fdashboard%2Frequests" {
		t.Errorf("status %d, location %q", resStatus(ctx), ctx.Res.Headers["Location"])
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"":                           "",
		"/dashboard":                 "/dashboard",
		"/dashboard/logs?level=http": "/dashboard/logs?level=http",
		"/dashboard/login":           "",
		"/dashboard/logout":          "",
		"/dashboard/assets/x.js":     "",
		"//dashboard/x":              "",
		"/dashboardx":                "",
		"http://localhost/dashboard": "",
		"/dashboard/a\r\nSet-Cookie": "",
	} {
		if got := safeNext("/dashboard", in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSecurityHeadersAreSet(t *testing.T) {
	router, c := installed2(t, func(cfg *Config) { cfg.DisableAuth = true })
	_ = c
	ctx := callRoute(t, router, breeze.GET, "/dashboard/api/overview", nil, "")
	h := ctx.Res.Headers
	for _, k := range []string{"X-Content-Type-Options", "X-Frame-Options", "Cache-Control", "Content-Security-Policy", "Referrer-Policy"} {
		if h[k] == "" {
			t.Errorf("response has no %s header: %v", k, h)
		}
	}
}

func TestCredentialsMatch(t *testing.T) {
	wu, wp := hashPass("admin"), hashPass("pw")
	for _, tc := range []struct {
		u, p string
		want bool
	}{{"admin", "pw", true}, {"admin", "x", false}, {"x", "pw", false}, {"", "", false}, {"admin", "", false}} {
		if got := credentialsMatch(wu, wp, tc.u, tc.p); got != tc.want {
			t.Errorf("credentialsMatch(%q,%q) = %v", tc.u, tc.p, got)
		}
	}
}

// ─── Sessions ────────────────────────────────────────────────────────────

// create returned while holding the store's lock when the cap was reached, wedging every
// later authenticated request.
func TestSessionCapacityDoesNotDeadlock(t *testing.T) {
	s := newSessionStore()
	for i := 0; i < maxDashboardSessions; i++ {
		if _, err := s.create("u"); err != nil {
			t.Fatalf("create #%d: %v", i, err)
		}
	}
	if _, err := s.create("u"); err == nil {
		t.Fatal("create past the cap succeeded")
	}
	done := make(chan struct{})
	go func() {
		s.valid("nope")
		s.count(time.Now())
		_, _ = s.create("u")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the session store is locked after hitting its capacity")
	}
}

func TestSessionCount(t *testing.T) {
	s := newSessionStore()
	_, _ = s.create("a")
	tok, _ := s.create("b")
	s.destroy(tok)
	if n := s.count(time.Now()); n != 1 {
		t.Errorf("count = %d, want 1", n)
	}
	if n := s.count(time.Now().Add(sessionDuration + time.Minute)); n != 0 {
		t.Errorf("count after expiry = %d, want 0", n)
	}
}

// ─── Health checks ───────────────────────────────────────────────────────

// RunHealthChecks wrote c.lastHealth under a read lock: two refreshes at once are a fatal
// "concurrent map writes". The test would crash the whole test binary on the old code.
func TestRunHealthChecksConcurrently(t *testing.T) {
	c := newCollector(DefaultConfig(), nil)
	for i := 0; i < 8; i++ {
		i := i
		c.RegisterHealthCheck(fmt.Sprintf("probe-%d", i), func() (string, string) {
			time.Sleep(time.Millisecond)
			return "green", "ok"
		})
	}
	c.RegisterHealthCheck("panics", func() (string, string) { panic("boom") })
	c.RegisterHealthCheck("odd", func() (string, string) { return "ok", "" })
	var wg sync.WaitGroup
	for g := 0; g < 24; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 20; n++ {
				out := c.RunHealthChecks()
				for i := 1; i < len(out); i++ {
					if out[i-1].Name > out[i].Name {
						t.Errorf("results are not in name order: %q before %q", out[i-1].Name, out[i].Name)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	byName := map[string]HealthStatus{}
	for _, h := range c.RunHealthChecks() {
		byName[h.Name] = h
	}
	if byName["panics"].Status != "red" || !strings.Contains(byName["panics"].Message, "panicked") {
		t.Errorf("a panicking probe: %+v", byName["panics"])
	}
	if byName["odd"].Status != "green" {
		t.Errorf(`a probe that answered "ok": %+v`, byName["odd"])
	}
}

func TestHealthStatusIsNormalised(t *testing.T) {
	for in, want := range map[string]string{"green": "green", " RED ": "red", "yellow": "yellow", "ok": "green", "degraded": "yellow", "down": "red", "<img src=x onerror=alert(1)>": "yellow"} {
		if got, _ := normalizeHealth(in, ""); got != want {
			t.Errorf("normalizeHealth(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHungHealthProbeDoesNotBlockTheRefresh(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the probe timeout")
	}
	c := newCollector(DefaultConfig(), nil)
	release := make(chan struct{})
	defer close(release)
	c.RegisterHealthCheck("hung", func() (string, string) { <-release; return "green", "" })
	c.RegisterHealthCheck("fine", func() (string, string) { return "green", "" })
	start := time.Now()
	out := c.RunHealthChecks()
	if time.Since(start) > healthProbeTimeout+2*time.Second {
		t.Fatalf("the refresh took %v", time.Since(start))
	}
	if out[0].Name != "fine" || out[0].Status != "green" || out[1].Status != "red" {
		t.Errorf("results: %+v", out)
	}
	// a second refresh does not start the hung probe again
	if out2 := c.RunHealthChecks(); out2[1].Status != "yellow" {
		t.Errorf("the still-running probe was started again: %+v", out2[1])
	}
}

// ─── Timeline, IDs, ring buffer ──────────────────────────────────────────

func TestTimelineRecorderStaysConsistentWhenFull(t *testing.T) {
	rec := NewTimelineRecorder(Config{MaxTimelineEntries: 3})
	endA := rec.Step("a")
	endB := rec.Step("b")
	endC := rec.Step("c")
	endD := rec.Step("d") // over the cap: not recorded
	endD(nil)
	endC(nil)
	endB(nil)
	endA(nil)
	steps := rec.Build()
	if len(steps) != 3 || rec.Dropped() != 1 {
		t.Fatalf("%d steps, %d dropped", len(steps), rec.Dropped())
	}
	for i, want := range []string{"a", "b", "c"} {
		if steps[i].Name != want || steps[i].Duration < 0 || steps[i].End.IsZero() {
			t.Errorf("step %d: %+v", i, steps[i])
		}
	}
	if len(steps[0].Children) != 1 || steps[0].Children[0].Name != "b" {
		t.Errorf("the tree is scrambled: %+v", steps[0].Children)
	}
}

func TestNewIDIsUniqueAtHighRates(t *testing.T) {
	const workers, each = 8, 40000 // 320k IDs, within a second or two: more than the old 65536 per second
	seen := make([][]string, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids := make([]string, 0, each)
			for i := 0; i < each; i++ {
				ids = append(ids, newID())
			}
			seen[w] = ids
		}()
	}
	wg.Wait()
	all := make(map[string]struct{}, workers*each)
	for _, ids := range seen {
		for _, id := range ids {
			if len(id) != 16 {
				t.Fatalf("id %q has %d characters, want 16", id, len(id))
			}
			if _, dup := all[id]; dup {
				t.Fatalf("duplicate id %q", id)
			}
			all[id] = struct{}{}
		}
	}
}

func TestRingBufferLastFindAndClear(t *testing.T) {
	r := newRingBuffer[int](4)
	for i := 1; i <= 6; i++ {
		r.Push(i)
	}
	if got := r.Last(2); len(got) != 2 || got[0] != 5 || got[1] != 6 {
		t.Errorf("Last(2) = %v", got)
	}
	if got := r.Last(0); len(got) != 4 || got[0] != 3 {
		t.Errorf("Last(0) = %v", got)
	}
	if v, ok := r.Find(func(x *int) bool { return *x%2 == 1 }); !ok || v != 5 {
		t.Errorf("Find newest odd = %v, %v", v, ok)
	}
	if _, ok := r.Find(func(x *int) bool { return *x == 1 }); ok {
		t.Error("Find returned an entry that was overwritten")
	}
	r.Clear()
	if r.Len() != 0 || len(r.Last(3)) != 0 {
		t.Error("Clear left entries")
	}
}

// ─── Data captured from requests ─────────────────────────────────────────

func TestDashboardUserNeverDumpsTheValueItIsGiven(t *testing.T) {
	type account struct {
		ID           int
		Email        string
		PasswordHash string
	}
	mk := func(v any) *breeze.Context {
		ctx := breeze.NewContext(breeze.GET, "/x")
		ctx.Set("user", v)
		return ctx
	}
	if got := dashboardUser(mk(&account{1, "a@b.c", "$2a$10$secretsecret"})); got != "authenticated" {
		t.Errorf("a struct user was recorded as %q", got)
	}
	type claims map[string]any // how a JWT library names its claims: not a map[string]any
	if got := dashboardUser(mk(claims{"sub": "alice", "email": "a@b.c", "exp": 1.0})); got != "alice" {
		t.Errorf("claims user = %q, want alice", got)
	}
	if got := dashboardUser(mk(map[string]any{"user_id": float64(42)})); got != "42" {
		t.Errorf("numeric user_id = %q", got)
	}
	if got := dashboardUser(mk(strings.Repeat("x", 500))); len(got) > maxUserLabel+4 {
		t.Errorf("an unbounded user label of %d bytes", len(got))
	}
}

func TestClientIPIgnoresGarbage(t *testing.T) {
	mk := func(xff string) *breeze.Context {
		ctx := breeze.NewContext(breeze.GET, "/x")
		ctx.Req.Header["x-forwarded-for"] = xff
		return ctx
	}
	if got := clientIP(mk("203.0.113.9, 10.0.0.1")); got != "203.0.113.9" {
		t.Errorf("first hop = %q", got)
	}
	for _, bad := range []string{"<script>alert(1)</script>", strings.Repeat("1", 300), "not-an-ip", ",1.2.3.4"} {
		if got := clientIP(mk(bad)); got != "" {
			t.Errorf("clientIP(%q) = %q, want it ignored", bad, got)
		}
	}
}

func TestHeadersAreMaskedWhateverTheConfigSays(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaskedHeaders = []string{"x-only-this"}
	got := maskHeaders(cfg, map[string]string{"authorization": "Bearer abc", "cookie": "s=1", "x-only-this": "v", "accept": "*/*"})
	for _, k := range []string{"authorization", "cookie", "x-only-this"} {
		if got[k] != maskedValue {
			t.Errorf("%s = %q", k, got[k])
		}
	}
	if got["accept"] != "*/*" {
		t.Errorf("accept was masked")
	}
}

func TestMaskLineCatchesCommonSecretShapes(t *testing.T) {
	cfg := DefaultConfig()
	for _, tc := range []struct{ in, mustNotContain string }{
		{"Authorization: Bearer eyJhbGciOi.payload.sig", "eyJhbGciOi"},
		{"authorization=Bearer abc123xyz ok", "abc123xyz"},
		{"refresh failed refresh_token=r-12345", "r-12345"},
		{"connect client_secret=cs-777 db_password=hunter2", "cs-777"},
		{`{"access_token":"at-999","user":"bob"}`, "at-999"},
		{"GET /cb?token=tk-555&state=1 -> 200", "tk-555"},
	} {
		if got := maskLine(cfg, tc.in); strings.Contains(got, tc.mustNotContain) {
			t.Errorf("maskLine(%q) = %q still holds %q", tc.in, got, tc.mustNotContain)
		}
	}
	for _, keep := range []string{"mytoken=abc", "tokens=3", "12:00:01", "http://host:3000/x"} {
		if got := maskLine(cfg, keep); got != keep {
			t.Errorf("maskLine(%q) = %q, want it left alone", keep, got)
		}
	}
}

func TestReferrerLosesItsQueryString(t *testing.T) {
	ctx := breeze.NewContext(breeze.GET, "/x")
	ctx.Req.Header["referer"] = "https://app.example/reset?token=abc#frag"
	if got := selectHeaders(ctx)["referer"]; got != "https://app.example/reset" {
		t.Errorf("referer = %q", got)
	}
}

// ─── WebSocket hub ───────────────────────────────────────────────────────

func TestHubDropsTheOldestInBulkAndSaysSo(t *testing.T) {
	c := newCollector(DefaultConfig(), nil)
	h := &wsHub{clients: map[*breeze.WSConn]struct{}{{}: {}}, c: c, stop: make(chan struct{}), pending: make([]wsEvent, 0, 8)}
	for i := 0; i < wsMaxBatch+10; i++ {
		pushEvent(h, "request", i)
	}
	h.pmu.Lock()
	n, dropped, first := len(h.pending), h.dropped, h.pending[0].Data
	h.pmu.Unlock()
	if n > wsMaxBatch || dropped != wsMaxBatch/4 {
		t.Errorf("pending %d, dropped %d", n, dropped)
	}
	if first.(int) != wsMaxBatch/4 {
		t.Errorf("the oldest kept event is %v, want %d", first, wsMaxBatch/4)
	}
}

func TestSnapshotMessageIsEmptyRatherThanBroken(t *testing.T) {
	c := newCollector(DefaultConfig(), nil)
	h := &wsHub{c: c, clients: map[*breeze.WSConn]struct{}{}}
	if m := h.snapshotMessage(); !strings.Contains(m, `"type":"snapshot"`) {
		t.Errorf("snapshot = %.60q", m)
	}
	if finite(0) != 0 || finite(1.5) != 1.5 {
		t.Error("finite altered a normal value")
	}
}

// ─── Install ─────────────────────────────────────────────────────────────

func TestInstallIsIdempotentOnARouter(t *testing.T) {
	router := breeze.NewRouter()
	cfg := DefaultConfig()
	cfg.DisableAuth = true
	a := Install(nil, router, cfg)
	b := Install(nil, router, cfg)
	defer a.Close()
	if a != b {
		t.Error("a second Install on the same router built another collector")
	}
}

// A disabled dashboard runs nothing — but its read API keeps answering, because the MCP routes tool
// reads it to tell "switched off" from "no traffic".
func TestDisabledInstallRunsNothingButStillAnswers(t *testing.T) {
	router := breeze.NewRouter()
	cfg := DefaultConfig()
	cfg.Enabled = false
	cfg.DisableAuth = true
	c := Install(nil, router, cfg)
	defer c.Close()
	if c.hub != nil {
		t.Error("a disabled dashboard started a WebSocket hub")
	}
	c.closeMu.Lock()
	stoppers := len(c.stopFns)
	c.closeMu.Unlock()
	if stoppers != 0 {
		t.Errorf("a disabled dashboard started %d background workers", stoppers)
	}
	ctx := callRoute(t, router, breeze.GET, "/dashboard/api/routes", nil, "")
	if s := resStatus(ctx); s != 0 && s != 200 {
		t.Errorf("the read API answered %d while disabled", s)
	}
	if c.wsGuard(breeze.NewContext(breeze.GET, "/dashboard/ws")) {
		t.Error("the live socket accepted a connection while disabled")
	}
}

func TestCloseIsIdempotentAndRunsStoppers(t *testing.T) {
	c := newCollector(DefaultConfig(), nil)
	var n int32
	c.onClose(func() { atomic.AddInt32(&n, 1) })
	c.Close()
	c.Close()
	if n != 1 {
		t.Errorf("stopper ran %d times", n)
	}
}

// ─── Overview numbers ────────────────────────────────────────────────────

// The error rate and the average response time came from the buffer of captured requests —
// which, with nobody watching, holds only the failed and slow ones.
func TestSamplerRatesComeFromCountersNotFromTheCapturedSample(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DisableAuth = true
	c := newCollector(cfg, nil)
	s := &metricsSampler{c: c, stop: make(chan struct{}), lastTime: time.Now().Add(-time.Second)}
	// 100 requests, 2 server errors, 10 ms each: the counters the middleware keeps
	c.requestsTotal.Add(100)
	c.errorsTotal.Add(2)
	c.unmatched.acc.totalDurUS.Add(100 * 10_000)
	// ...while the captured sample holds one slow failure
	c.RecordRequest(RequestRecord{ID: "x", Status: 500, Duration: 900_000})
	s.sample(time.Now())
	m := c.Metrics()
	if m.ErrorRate < 0.0199 || m.ErrorRate > 0.0201 {
		t.Errorf("error rate %.4f, want 0.02", m.ErrorRate)
	}
	if m.AvgRespTimeMS < 9.9 || m.AvgRespTimeMS > 10.1 {
		t.Errorf("average response %.2f ms, want 10", m.AvgRespTimeMS)
	}
}

func TestActiveSessionsAreReported(t *testing.T) {
	c := newCollector(DefaultConfig(), nil)
	c.sessions = newSessionStore()
	_, _ = c.sessions.create("a")
	_, _ = c.sessions.create("b")
	s := &metricsSampler{c: c, stop: make(chan struct{}), lastTime: time.Now().Add(-time.Second)}
	s.sample(time.Now())
	if m := c.Metrics(); m.ActiveSessions != 2 {
		t.Errorf("active sessions = %d, want 2", m.ActiveSessions)
	}
}

// ─── Queue, scheduler, cache, logs ───────────────────────────────────────

func TestQueueSchedulerAndLogCountEndpoints(t *testing.T) {
	router, c := installed2(t, func(cfg *Config) { cfg.DisableAuth = true })
	c.RegisterJob(QueueJob{ID: "j1", Queue: "mail", State: "failed", QueuedAt: now().Add(-time.Minute)})
	c.RegisterJob(QueueJob{ID: "j2", Queue: "mail", State: "completed", QueuedAt: now()})
	c.RegisterTask(SchedulerTask{Name: "nightly", Cron: "0 0 * * *"})
	c.RecordLog("error", LogEntry{Time: now(), Message: "boom"})

	q := resBody(callRoute(t, router, breeze.GET, "/dashboard/api/queue", nil, ""))
	if !strings.Contains(q, `"failed":1`) || !strings.Contains(q, `"completed":1`) || strings.Index(q, `"j2"`) > strings.Index(q, `"j1"`) {
		t.Errorf("queue payload: %s", q)
	}
	if !strings.Contains(q, `"can_retry":false`) {
		t.Errorf("retry advertised without a retrier: %s", q)
	}
	if s := resBody(callRoute(t, router, breeze.GET, "/dashboard/api/scheduler", nil, "")); !strings.Contains(s, "nightly") {
		t.Errorf("scheduler payload: %s", s)
	}
	if l := resBody(callRoute(t, router, breeze.GET, "/dashboard/api/logs/counts", nil, "")); !strings.Contains(l, `"error":1`) {
		t.Errorf("log counts: %s", l)
	}

	// retry: refused without a retrier, honest about it, and only for failed jobs
	post := func(id string) *breeze.Context {
		return callRoute(t, router, breeze.POST, "/dashboard/api/queue/retry", map[string]string{"content-type": "application/json"}, fmt.Sprintf(`{"id":%q}`, id))
	}
	if s := resStatus(post("j1")); s != 501 {
		t.Errorf("retry without a retrier: %d, want 501", s)
	}
	var retried []string
	c.SetJobRetrier(func(id string) error { retried = append(retried, id); return nil })
	if s := resStatus(post("j2")); s != 409 {
		t.Errorf("retrying a completed job: %d, want 409", s)
	}
	if s := resStatus(post("missing")); s != 404 {
		t.Errorf("retrying an unknown job: %d, want 404", s)
	}
	if s := resStatus(post("j1")); s != 0 && s != 200 {
		t.Errorf("retry: %d", s)
	}
	if len(retried) != 1 || retried[0] != "j1" {
		t.Errorf("the application's queue was asked to retry %v", retried)
	}
}

func TestJobTableIsBounded(t *testing.T) {
	c := newCollector(DefaultConfig(), nil)
	for i := 0; i < maxTrackedJobs+50; i++ {
		c.RegisterJob(QueueJob{ID: fmt.Sprint(i), State: "completed", QueuedAt: time.Unix(int64(i), 0)})
	}
	if n := len(c.Jobs()); n != maxTrackedJobs {
		t.Errorf("%d jobs held, want %d", n, maxTrackedJobs)
	}
	for _, j := range c.Jobs() {
		if j.ID == "0" {
			t.Error("the oldest job was kept while newer ones were dropped")
		}
	}
}

func TestCacheClearIsHonest(t *testing.T) {
	router, c := installed2(t, func(cfg *Config) { cfg.DisableAuth = true })
	post := func(body string) *breeze.Context {
		return callRoute(t, router, breeze.POST, "/dashboard/api/cache/clear", map[string]string{"content-type": "application/json"}, body)
	}
	if s := resStatus(post(`{}`)); s != 501 {
		t.Errorf("clear with no cache attached: %d, want 501", s)
	}
	c.cacheHits.Store(5)
	var gotPrefix string
	c.SetCacheClearer(func(prefix string) (int, error) { gotPrefix = prefix; return 3, nil })
	ctx := post(`{"prefix":"users:"}`)
	if gotPrefix != "users:" || !strings.Contains(resBody(ctx), `"cleared":3`) {
		t.Errorf("prefix %q, body %s", gotPrefix, resBody(ctx))
	}
	if c.cacheHits.Load() != 5 {
		t.Error("a prefix clear reset the counters")
	}
	post(`{}`)
	if c.cacheHits.Load() != 0 {
		t.Error("a full clear did not reset the counters")
	}
	c.SetCacheClearer(func(string) (int, error) { return 0, errors.New("redis down") })
	if s := resStatus(post(`{}`)); s != 500 {
		t.Errorf("a failing clear: %d", s)
	}
}

func TestLogsPayloadForUnknownLevelIsAnEmptyList(t *testing.T) {
	c := newCollector(DefaultConfig(), nil)
	if got := c.logsPayload(queryValues(url.Values{"level": {"nonsense"}})); got == nil || len(got) != 0 {
		t.Errorf("logsPayload = %#v, want an empty non-nil slice", got)
	}
}

// ─── Database Browser ────────────────────────────────────────────────────

func TestParsePKStrict(t *testing.T) {
	if pk, err := parsePKStrict("id=7,tenant=a%2Cb"); err != nil || pk["id"] != "7" || pk["tenant"] != "a,b" {
		t.Errorf("good key: %v %v", pk, err)
	}
	for _, bad := range []string{"", "garbage", "id=1,%zz", "=1", "id=1,"} {
		if _, err := parsePKStrict(bad); err == nil {
			t.Errorf("parsePKStrict(%q) was accepted", bad)
		}
	}
}

func TestValidateWriteKeys(t *testing.T) {
	cols := []TableColumn{{Name: "id", PrimaryKey: true}, {Name: "name"}, {Name: "email"}}
	if err := validateWriteKeys(cols, map[string]any{"name": "x"}, map[string]any{"id": "1"}, true); err != nil {
		t.Errorf("a valid update: %v", err)
	}
	for name, tc := range map[string]struct {
		values, pk map[string]any
		need       bool
	}{
		"unknown column":       {map[string]any{"name; DROP TABLE users": 1}, nil, false},
		"unknown key":          {nil, map[string]any{"ghost": 1}, true},
		"not a primary key":    {nil, map[string]any{"name": "x"}, true},
		"incomplete key":       {nil, map[string]any{}, true},
		"values with pk check": {map[string]any{"nope": 1}, map[string]any{"id": "1"}, true},
	} {
		if err := validateWriteKeys(cols, tc.values, tc.pk, tc.need); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// no columns reported: names must at least be plain identifiers
	if err := validateWriteKeys(nil, map[string]any{"ok_name": 1}, map[string]any{"id": 1}, true); err != nil {
		t.Errorf("plain identifiers without columns: %v", err)
	}
	for _, bad := range []string{`na"me`, "a b", "x;y", "1abc", "a-b", ""} {
		if err := validateWriteKeys(nil, map[string]any{bad: 1}, nil, false); err == nil {
			t.Errorf("identifier %q accepted", bad)
		}
	}
	if err := validateWriteKeys([]TableColumn{{Name: "a"}}, nil, map[string]any{"a": 1}, true); err == nil {
		t.Error("a table with no primary key can be addressed by a key")
	}
}

type columnInspector struct{ mockInspector }

func (m *columnInspector) TableData(name string, page, pageSize int, search string) (TableData, error) {
	d, _ := m.mockInspector.TableData(name, page, pageSize, search)
	d.Columns = []TableColumn{{Name: "id", PrimaryKey: true}, {Name: "name"}}
	return d, nil
}

func TestWriteEndpointsValidateColumnNamesAndKeys(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AllowWrites = true
	c := newCollector(cfg, nil)
	c.SetDBInspector(&columnInspector{})
	w := &pkCapturingWriter{}
	c.SetDBWriter(w)

	call := func(h func(*breeze.Context) error, method breeze.Method, pk, body string) int {
		ctx := breeze.NewContext(method, "/x")
		ctx.SetParam("name", "users")
		ctx.SetParam("pk", pk)
		ctx.Req.Body = []byte(body)
		_ = h(ctx)
		return ctx.Res.Status
	}
	if s := call(c.handleDBTableInsert, breeze.POST, "", `{"values":{"evil\"col":1}}`); s != 400 {
		t.Errorf("insert with a hostile column: %d, want 400", s)
	}
	if s := call(c.handleDBTableUpdate, breeze.PUT, "name=Bob", `{"values":{"name":"x"}}`); s != 400 {
		t.Errorf("update addressed by a non-key column: %d, want 400", s)
	}
	if s := call(c.handleDBTableUpdate, breeze.PUT, "garbage", `{"values":{"name":"x"}}`); s != 400 {
		t.Errorf("update with a malformed key: %d, want 400", s)
	}
	if s := call(c.handleDBTableDelete, breeze.DELETE, "", ``); s != 400 {
		t.Errorf("delete with an empty key: %d, want 400", s)
	}
	if w.lastDeletePK != nil || w.lastUpdatePK != nil {
		t.Errorf("the writer was reached with %v / %v", w.lastDeletePK, w.lastUpdatePK)
	}
	if s := call(c.handleDBTableUpdate, breeze.PUT, "id=1", `{"values":{"name":"x"}}`); s != 200 {
		t.Errorf("a valid update: %d", s)
	}
	if w.lastUpdatePK["id"] != "1" {
		t.Errorf("writer key = %v", w.lastUpdatePK)
	}
}

func TestReadingAnUnlistedTableIsRefused(t *testing.T) {
	c := newCollector(DefaultConfig(), nil)
	c.SetDBInspector(&mockInspector{})
	ctx := breeze.NewContext(breeze.GET, "/x")
	ctx.SetParam("name", "sqlite_master")
	_ = c.handleDBTableData(ctx)
	if ctx.Res.Status != 404 {
		t.Errorf("status %d reading a table the inspector does not list, want 404", ctx.Res.Status)
	}
}

func TestPageSizeIsClamped(t *testing.T) {
	for _, tc := range []struct{ p, s, wp, ws int }{{0, 0, 1, 50}, {-5, -1, 1, 50}, {3, 100000, 3, maxDBPageSize}, {2, 25, 2, 25}} {
		if p, s := clampPage(tc.p, tc.s); p != tc.wp || s != tc.ws {
			t.Errorf("clampPage(%d,%d) = %d,%d", tc.p, tc.s, p, s)
		}
	}
}

type flakyInspector struct {
	fail  atomic.Bool
	calls atomic.Int32
}

func (f *flakyInspector) Tables() ([]TableInfo, error) {
	if f.fail.Load() {
		return nil, errors.New("db down")
	}
	return []TableInfo{{Name: "t"}}, nil
}
func (f *flakyInspector) TableData(name string, page, pageSize int, search string) (TableData, error) {
	f.calls.Add(1)
	if f.fail.Load() {
		return TableData{}, errors.New("db down")
	}
	return TableData{Table: name, Page: page, PageSize: pageSize}, nil
}

func TestCachedInspectorDoesNotCacheFailuresAndStaysBounded(t *testing.T) {
	f := &flakyInspector{}
	ci := newCachedDBInspector(f, time.Minute).(*cachedDBInspector)
	f.fail.Store(true)
	if _, err := ci.Tables(); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := ci.TableData("t", 1, 10, ""); err == nil {
		t.Fatal("expected an error")
	}
	f.fail.Store(false)
	if _, err := ci.Tables(); err != nil {
		t.Errorf("a recovered database still reports the cached error: %v", err)
	}
	if _, err := ci.TableData("t", 1, 10, ""); err != nil {
		t.Errorf("a recovered database still reports the cached error: %v", err)
	}
	for i := 0; i < maxCachedPages*3; i++ {
		if _, err := ci.TableData("t", 1, 10, fmt.Sprint("q", i)); err != nil {
			t.Fatal(err)
		}
	}
	ci.dataMu.RLock()
	n := len(ci.data)
	ci.dataMu.RUnlock()
	if n > maxCachedPages {
		t.Errorf("the page cache holds %d entries, cap %d", n, maxCachedPages)
	}
}

// ─── Fleet proxy, explorer, connections ──────────────────────────────────

func TestValidFleetPath(t *testing.T) {
	for _, ok := range []string{"services", "traces/0123abcd", "incidents", "traces/a-b_c.d~e"} {
		if !validFleetPath(ok) {
			t.Errorf("%q was refused", ok)
		}
	}
	for _, bad := range []string{"", "../admin", "a/../b", "a//b", "./x", "a?x=1", "a#b", "a%2e%2e/b", "a b", `a\b`, "a/", strings.Repeat("a", 300)} {
		if validFleetPath(bad) {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestRedactURLDropsCredentials(t *testing.T) {
	if got := redactURL("http://svc:pw@agg.internal:9000/fleet"); strings.Contains(got, "pw") || strings.Contains(got, "svc") {
		t.Errorf("redactURL kept credentials: %q", got)
	}
}

func TestSnippetsAreQuotedForTheirLanguage(t *testing.T) {
	req := APIExplorerExecRequest{
		Method:  "POST",
		URL:     "/a?b='; touch /tmp/pwned; echo '",
		Headers: map[string]string{"X-One": "it's", "Content-Type": "text/plain", "A-First": "1"},
		Body:    "line1\nline2 $notvar {$x} \"q\" 'sq' \\back",
	}
	s := generateSnippetsFor(req, "http://localhost:3000")

	curl := s["curl"]
	if !strings.Contains(curl, `'http://localhost:3000/a?b='\''; touch /tmp/pwned; echo '\'''`) || !strings.Contains(curl, `-H 'X-One: it'\''s'`) {
		t.Errorf("curl is not shell-quoted:\n%s", curl)
	}
	if strings.Index(curl, "A-First") > strings.Index(curl, "X-One") {
		t.Errorf("headers are not in a stable order:\n%s", curl)
	}
	if !strings.Contains(s["go"], `req.Header.Set("X-One", "it's")`) || !strings.Contains(s["go"], `"line1\nline2 $notvar {$x} \"q\" 'sq' \\back"`) {
		t.Errorf("go:\n%s", s["go"])
	}
	if !strings.Contains(s["javascript"], `"line1\nline2 $notvar {$x} \"q\" 'sq' \\back"`) {
		t.Errorf("javascript:\n%s", s["javascript"])
	}
	if strings.Contains(s["python"], "\nline2") || !strings.Contains(s["python"], `\n`) {
		t.Errorf("python body has a raw newline:\n%s", s["python"])
	}
	if !strings.Contains(s["php"], `'line1`) || strings.Contains(s["php"], `"line1`) || !strings.Contains(s["php"], `\'sq\'`) {
		t.Errorf("php is not single-quoted:\n%s", s["php"])
	}
	cs := s["csharp"]
	if strings.Contains(cs, "\nline2") || strings.Contains(cs, "Headers.Add") || !strings.Contains(cs, `"text/plain"`) {
		t.Errorf("csharp:\n%s", cs)
	}
	// a relative URL becomes runnable
	if !strings.Contains(s["curl"], "http://localhost:3000/a") {
		t.Error("the URL was not completed with the origin")
	}
	// a hostile Host header never reaches the generated code
	ctx := breeze.NewContext(breeze.GET, "/x")
	ctx.Req.Header["host"] = "evil'; rm -rf ~ #"
	if b := snippetBase(ctx); b != "" {
		t.Errorf("snippetBase accepted %q", b)
	}
}

func TestExplorerRefusesUnknownMethodsAndHidesNoAppRoutes(t *testing.T) {
	c := newCollector(DefaultConfig(), nil)
	ctx := breeze.NewContext(breeze.POST, "/x")
	ctx.Req.Body = []byte(`{"method":"CONNECT","url":"/x"}`)
	_ = c.handleAPIExplorerExec(ctx)
	if ctx.Res.Status != 400 {
		t.Errorf("CONNECT: %d, want 400", ctx.Res.Status)
	}
}

func TestArchitecturePayloadIsOrderedAndRedacted(t *testing.T) {
	c := newCollector(DefaultConfig(), nil)
	c.RegisterConnection(Connection{ID: "z", Name: "Zeta", Host: "postgres://svc:p4ssw0rd@db:5432/x", Details: map[string]string{"password": "hunter2", "pool": "3"}})
	c.RegisterConnection(Connection{ID: "a", Name: "Alpha", Message: "redis://:x@h failed token=tk-1"})
	for i := 0; i < 20; i++ {
		got := c.Connections()
		if got[0].Name != "Alpha" || got[1].Name != "Zeta" {
			t.Fatalf("order %v", []string{got[0].Name, got[1].Name})
		}
	}
	p := c.architecturePayload()
	conns := p["connections"].([]Connection)
	if strings.Contains(conns[1].Host, "p4ssw0rd") || conns[1].Details["password"] != maskedValue || conns[1].Details["pool"] != "3" {
		t.Errorf("zeta not redacted: %+v", conns[1])
	}
	if strings.Contains(conns[0].Message, "tk-1") {
		t.Errorf("alpha message not masked: %q", conns[0].Message)
	}
}
