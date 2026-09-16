package oauth2

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/nelthaarion/breeze/v2"
)

// newCtx builds a Breeze context for a GET request with the given query string
// and cookies, suitable for driving the middlewares directly in tests.
func newCtx(query string, cookies map[string]string) *breeze.Context {
	ctx := breeze.NewContext(breeze.GET, "/")
	if query != "" {
		vals, _ := url.ParseQuery(query)
		ctx.Req.Query = vals
	} else {
		ctx.Req.Query = url.Values{}
	}
	if len(cookies) > 0 {
		parts := make([]string, 0, len(cookies))
		for k, v := range cookies {
			parts = append(parts, k+"="+v)
		}
		ctx.Req.Header["cookie"] = strings.Join(parts, "; ")
	}
	return ctx
}

// setCookieLines returns the raw Set-Cookie header lines of the serialized
// response, in the order they appear on the wire.
func setCookieLines(ctx *breeze.Context) []string {
	if ctx.Res == nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(ctx.Res.Bytes()), "\r\n") {
		if strings.HasPrefix(strings.ToLower(line), "set-cookie:") {
			out = append(out, line)
		}
	}
	return out
}

// cookieName returns the name of the cookie a Set-Cookie line sets, or "" if the
// line has no "name=value" pair where one belongs.
//
// It reads the name from the wire position rather than from anywhere in the line,
// so a line whose value smuggled a "Set-Cookie: " prefix into itself is reported
// under that whole string — which is exactly what a browser would do, and which
// is why such a cookie is lost rather than merely misnamed.
func cookieName(line string) string {
	colon := strings.IndexByte(line, ':')
	if colon < 0 {
		return ""
	}
	pair, _, _ := strings.Cut(strings.TrimSpace(line[colon+1:]), ";")
	name, _, ok := strings.Cut(pair, "=")
	if !ok || name == "" {
		return ""
	}
	return name
}

// respCookies parses the Set-Cookie lines of the serialized response into a
// name->value map so the next simulated request can present them back.
//
// It reads the wire (ctx.Res.Bytes) rather than ctx.GetHeader("Set-Cookie") on
// purpose. The map holds the framework's multi-cookie encoding — every cookie on
// one value, separated by CRLF — and a map-level reader cannot tell a correctly
// written second line from a malformed one. That is not hypothetical: while the
// oauth2 middleware pre-joined its own "Set-Cookie: " prefix, the wire carried
// "Set-Cookie: Set-Cookie: <name>=...", browsers dropped that line, and a
// map-level reader still reported the cookie as present. Reading the bytes is
// what the browser sees, so a malformed line fails the test instead of hiding.
func respCookies(ctx *breeze.Context) map[string]string {
	out := map[string]string{}
	for _, line := range setCookieLines(ctx) {
		name := cookieName(line)
		if name == "" {
			continue
		}
		colon := strings.IndexByte(line, ':')
		pair, _, _ := strings.Cut(strings.TrimSpace(line[colon+1:]), ";")
		if _, value, ok := strings.Cut(pair, "="); ok {
			out[name] = value
		}
	}
	return out
}

// location returns the Location header (redirect target) of a response.
func location(ctx *breeze.Context) string { return ctx.GetHeader("Location") }

// extractQuery returns the value of query parameter key from a full URL.
func extractQuery(rawURL, key string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Query().Get(key)
}

// status returns the response status code, or 0 if none was set.
func status(ctx *breeze.Context) int {
	if ctx.Res == nil {
		return 0
	}
	return ctx.Res.Status
}

// mockProvider spins up an httptest server that emulates a provider's token and
// userinfo endpoints, so the full login→callback flow can be exercised offline.
type mockProvider struct {
	server   *httptest.Server
	tokenURL string
	userURL  string

	// wantCode is the code the token endpoint expects; empty means accept any.
	wantCode string
	// gotVerifier captures the PKCE verifier the client sent (for assertions).
	gotVerifier string
}

// newMockProvider returns a running mock provider. Call Close when done.
func newMockProvider(t *testing.T) *mockProvider {
	t.Helper()
	mp := &mockProvider{}
	mux := http.NewServeMux()

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mp.gotVerifier = r.Form.Get("code_verifier")
		if mp.wantCode != "" && r.Form.Get("code") != mp.wantCode {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at-123","token_type":"Bearer","refresh_token":"rt-456","expires_in":3600,"scope":"openid"}`))
	})

	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer at-123" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sub":"user-1","oid":"user-1","id":42,"login":"octocat","email":"u@example.com","name":"Test User","picture":"http://img","avatar_url":"http://img","avatar":"abc","username":"octocat","global_name":"Testy"}`))
	})

	mp.server = httptest.NewServer(mux)
	mp.tokenURL = mp.server.URL + "/token"
	mp.userURL = mp.server.URL + "/userinfo"
	return mp
}

func (mp *mockProvider) Close() { mp.server.Close() }

// testConfig returns a normalized cookie-mode config wired to the mock provider
// via a stub driver, with a fixed secret so signatures are deterministic.
func testConfig(t *testing.T, mp *mockProvider, mode SessionMode) Config {
	t.Helper()
	// Register a stub driver on an otherwise-unused provider slot by swapping
	// the Google driver's endpoints through a custom driver.
	drv := &stubDriver{tokenURL: mp.tokenURL, userURL: mp.userURL}
	RegisterDriver(drv)

	cfg := Config{
		Provider:     testProvider,
		ClientID:     "cid",
		ClientSecret: "secret",
		BaseURL:      "https://app.test",
		CookieSecret: "test-cookie-secret-0000000000000000",
		SessionMode:  mode,
	}
	if err := cfg.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	return cfg
}
