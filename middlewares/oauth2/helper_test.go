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

// respCookies parses all Set-Cookie headers written to a response into a
// name->value map so the next simulated request can present them back.
func respCookies(ctx *breeze.Context) map[string]string {
	out := map[string]string{}
	raw := ctx.GetHeader("Set-Cookie")
	if raw == "" {
		return out
	}
	// setCookie joins multiple cookies with "\r\nSet-Cookie: ".
	for _, line := range strings.Split(raw, "\r\nSet-Cookie: ") {
		// The value is the first "k=v" pair before the first ";".
		semi := strings.IndexByte(line, ';')
		pair := line
		if semi >= 0 {
			pair = line[:semi]
		}
		if eq := strings.IndexByte(pair, '='); eq > 0 {
			out[pair[:eq]] = pair[eq+1:]
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
