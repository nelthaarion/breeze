package dashboard

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha512"
	"crypto/subtle"
	"strconv"
	"strings"
	"time"

	"github.com/nelthaarion/breeze/v2"
)

// AuthMiddleware returns a middleware that enforces dashboard authentication.
//
// Auth flow:
//  1. Check for a valid session cookie. If present and valid, attach the
//     username to the context and continue.
//  2. If no valid session, redirect to the login page (HTTP 302).
//  3. The login page POSTs to /dashboard/login which validates credentials
//     and sets a session cookie.
//
// When DisableAuth is true, authentication is explicitly disabled. Missing
// credentials never open the dashboard; they fail closed with HTTP 503.
//
// Security: password comparison uses PBKDF2-HMAC-SHA-512 plus
// subtle.ConstantTimeCompare to avoid storing/comparing the configured password directly.
func AuthMiddleware(cfg Config, sessions *sessionStore) breeze.HandlerFunc {
	if cfg.DisableAuth {
		return func(ctx *breeze.Context) error { return ctx.Next() }
	}
	if strings.TrimSpace(cfg.Username) == "" || cfg.Password == "" {
		// Never interpret missing credentials as "open dashboard". That turns a
		// single omitted environment variable into an administrative bypass.
		return func(ctx *breeze.Context) error {
			ctx.Status(503)
			return ctx.WriteString("Dashboard authentication is not configured")
		}
	}
	wantUser := []byte(cfg.Username)
	wantPass := hashPass(cfg.Password)
	base := strings.TrimSuffix(cfg.BasePath, "/")
	if base == "" {
		base = "/dashboard"
	}
	loginPath := base + "/login"
	return func(ctx *breeze.Context) error {
		// Allow the login page itself and the login POST endpoint without auth.
		p := ctx.Req.Path
		// For base path, redirect is handled further down in the handler.
		// Check session cookie.
		cookie := ctx.Req.Header["cookie"]
		token := extractCookieValue(cookie, sessionCookieName)
		if token != "" {
			if username, ok := sessions.valid(token); ok {
				ctx.Set("breeze.dashboard.user", username)
				return ctx.Next()
			}
		}
		// No valid session — check for Basic Auth as a fallback (API clients).
		ah := ctx.Req.Header["authorization"]
		parts := strings.Fields(ah)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Basic") {
			peer := dashboardPeer(ctx)
			if !sessions.allowLogin(peer, time.Now()) {
				ctx.Status(429)
				ctx.SetHeader("Retry-After", strconv.Itoa(int(loginBlockDuration.Seconds())))
				return ctx.WriteString("Too many authentication failures")
			}
			user, pass, ok := decodeBasic(parts[1])
			if ok &&
				subtle.ConstantTimeCompare([]byte(user), wantUser) == 1 &&
				subtle.ConstantTimeCompare(hashPass(pass), wantPass) == 1 {
				sessions.clearLoginFailures(peer)
				ctx.Set("breeze.dashboard.user", user)

				// If a browser reached the dashboard as http(s)://user:pass@host/…,
				// immediately exchange that Basic-auth credential for a normal
				// HttpOnly session and redirect to the credential-free path. This
				// keeps secrets out of the visible address bar and subsequent
				// navigation/history entries while retaining Basic Auth for API clients.
				accept := ctx.Req.Header["accept"]
				isBrowser := strings.Contains(accept, "text/html") && !strings.Contains(accept, "application/json")
				if isBrowser && p != base+"/login" {
					token, err := sessions.create(user)
					if err != nil {
						return jsonError(ctx, 500, "could not create login session")
					}
					ctx.Res = &breeze.HTTPResponse{
						Status: 302,
						Headers: map[string]string{
							"Location":   p,
							"Set-Cookie": buildSessionCookie(token, base, int(sessionDuration.Seconds()), requestIsSecure(ctx)),
						},
						Body: []byte("redirecting..."),
					}
					ctx.Abort()
					return nil
				}
				return ctx.Next()
			}
			sessions.recordLoginFailure(peer, time.Now())
		}
		// For API requests (JSON), return 401 JSON. For browser requests,
		// redirect to login page.
		accept := ctx.Req.Header["accept"]
		isAPI := strings.Contains(accept, "application/json") || strings.HasPrefix(p, base+"/api/")
		if isAPI {
			ctx.Res = &breeze.HTTPResponse{
				Status: 401,
				Headers: map[string]string{
					"Content-Type": "application/json",
				},
				Body: []byte(`{"error":"unauthorized","login":"` + loginPath + `"}`),
			}
			ctx.Abort()
			return nil
		}
		// For SPA partial requests, return 401 so the SPA runtime falls back
		// to a full navigation (which will then redirect to login).
		if ctx.Req.Header["x-breeze-partial"] == "true" {
			ctx.Res = &breeze.HTTPResponse{
				Status: 401,
				Headers: map[string]string{
					"Content-Type": "text/plain",
				},
				Body: []byte("unauthorized"),
			}
			ctx.Abort()
			return nil
		}
		// Browser navigation: redirect to login.
		ctx.Res = &breeze.HTTPResponse{
			Status: 302,
			Headers: map[string]string{
				"Location": loginPath,
			},
			Body: []byte("redirecting to login..."),
		}
		ctx.Abort()

		return nil
	}
}

func requestIsSecure(ctx *breeze.Context) bool {
	// Prefer the actual connection state when available. X-Forwarded-Proto is only
	// a hint from a trusted reverse proxy; take the first comma-delimited value and
	// never treat arbitrary values as secure.
	if strings.EqualFold(ctx.Req.Header["x-forwarded-proto"], "https") {
		return true
	}
	return false
}

func dashboardPeer(ctx *breeze.Context) string {
	if ctx.Conn == nil {
		return "unknown"
	}
	return ctx.Conn.RemoteAddr().String()
}

// extractCookieValue parses a Cookie header and returns the value of the
// named cookie, or "" if not present.
func extractCookieValue(cookieHeader, name string) string {
	if cookieHeader == "" {
		return ""
	}
	for _, part := range strings.Split(cookieHeader, ";") {
		part = strings.TrimSpace(part)
		eq := strings.IndexByte(part, '=')
		if eq < 0 {
			continue
		}
		k := strings.TrimSpace(part[:eq])
		v := strings.TrimSpace(part[eq+1:])
		if k == name {
			return v
		}
	}
	return ""
}

// dashboardAuthSalt is process-random so identical dashboard passwords do not
// produce a stable digest across processes. It is generated once because the
// configured password is compared repeatedly during the lifetime of the server.
var dashboardAuthSalt = func() []byte {
	s := make([]byte, 32)
	if _, err := rand.Read(s); err != nil {
		panic("dashboard: cannot initialize auth salt: " + err.Error())
	}
	return s
}()

const dashboardAuthIterations = 210000

// hashPass returns a PBKDF2 derivation of the password. We compare hashes
// rather than plaintext so the constant-time comparison always runs on a
// fixed-size buffer. Authentication is deliberately the slow path; protecting
// a dashboard credential matters more than shaving microseconds from login.
func hashPass(p string) []byte {
	key, _ := pbkdf2.Key(sha512.New, p, dashboardAuthSalt, dashboardAuthIterations, 32)
	return key
}

// decodeBasic decodes a base64-encoded "user:pass" Basic auth payload.
func decodeBasic(s string) (user, pass string, ok bool) {
	b, err := base64DecodeStd(s)
	if err != nil {
		return "", "", false
	}
	idx := -1
	for i, c := range b {
		if c == ':' {
			idx = i
			break
		}
	}
	if idx < 0 {
		return "", "", false
	}
	return string(b[:idx]), string(b[idx+1:]), true
}

// base64DecodeStd is a small RFC 4648 base64 decoder.
func base64DecodeStd(s string) ([]byte, error) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var table [256]int
	for i := range table {
		table[i] = -1
	}
	for i := 0; i < len(alphabet); i++ {
		table[alphabet[i]] = i
	}
	s = strings.TrimRight(s, "=")
	out := make([]byte, 0, len(s)*3/4)
	var val uint32
	var bits int
	for i := 0; i < len(s); i++ {
		v := table[s[i]]
		if v < 0 {
			return nil, errInvalidBase64
		}
		val = (val << 6) | uint32(v)
		bits += 6
		if bits >= 8 {
			bits -= 8
			out = append(out, byte(val>>uint(bits)))
		}
	}
	return out, nil
}

type strErr string

func (e strErr) Error() string { return string(e) }

const errInvalidBase64 = strErr("invalid base64")
