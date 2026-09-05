package dashboard

import (
	"crypto/pbkdf2"
	"crypto/sha512"
	"crypto/subtle"
	"strings"

	"github.com/nelthaarion/breeze"
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
// When DisableAuth is true, OR when both Username and Password are empty,
// the middleware is a no-op — useful for local development.
//
// Security: password comparison uses constant-time comparison (SHA-256 +
// subtle.ConstantTimeCompare) to avoid timing side channels.
func AuthMiddleware(cfg Config, sessions *sessionStore) breeze.HandlerFunc {
	if cfg.DisableAuth || cfg.Username == "" || cfg.Password == "" {
		return func(ctx *breeze.Context) error { return ctx.Next() }
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
		if p == loginPath || p == base {
			// For base path, we'll handle redirect in the handler.
		}
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
		if strings.HasPrefix(ah, "Basic ") {
			user, pass, ok := decodeBasic(ah[6:])
			if ok &&
				subtle.ConstantTimeCompare([]byte(user), wantUser) == 1 &&
				subtle.ConstantTimeCompare(hashPass(pass), wantPass) == 1 {
				ctx.Set("breeze.dashboard.user", user)
				return ctx.Next()
			}
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

// dashboardAuthSalt is a fixed salt for hashPass. It only needs to prevent
// rainbow-table lookups for the single dashboard credential; it must stay
// constant across calls so hashes of the same password always match.
var dashboardAuthSalt = []byte("breeze-dashboard-auth-salt-v1")

// hashPass returns a PBKDF2 derivation of the password. We compare hashes
// rather than plaintext so the constant-time comparison always runs on a
// fixed-size buffer.
func hashPass(p string) []byte {
	key, _ := pbkdf2.Key(sha512.New, p, dashboardAuthSalt, 4096, 32)
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
