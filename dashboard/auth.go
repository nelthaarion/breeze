package dashboard

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"net"
	"net/url"
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
//  2. If no valid session, redirect to the login page (HTTP 302), remembering
//     where the person was going so the login can send them back.
//  3. The login page POSTs to /dashboard/login which validates credentials
//     and sets a session cookie.
//
// When DisableAuth is true, authentication is explicitly disabled. Missing
// credentials never open the dashboard; they fail closed with HTTP 503.
//
// The middleware marks a request it lets through with authPassedKey. Callers that
// run it inline must go through authorize rather than inspect the response: see the
// note on authPassedKey.
//
// Security: password comparison uses a per-process keyed HMAC-SHA-256 plus
// subtle.ConstantTimeCompare, so the comparison runs over fixed-size digests.
func AuthMiddleware(cfg Config, sessions *sessionStore) breeze.HandlerFunc {
	base := strings.TrimSuffix(cfg.BasePath, "/")
	if base == "" {
		base = "/dashboard"
	}
	loginPath := base + "/login"

	if cfg.DisableAuth {
		return func(ctx *breeze.Context) error {
			// No credentials to check, but not a free pass for any web page: with auth off, the
			// browser the developer is using is the only barrier, and a page on another site can make it
			// send a POST (or a simple-request body) to localhost. Cross-origin writes are refused here
			// just as they are behind a login.
			if !requestOriginOK(ctx, false) {
				return respondCrossOrigin(ctx)
			}
			ctx.Set(authPassedKey, true)
			return ctx.Next()
		}
	}
	if strings.TrimSpace(cfg.Username) == "" || cfg.Password == "" {
		// Never interpret missing credentials as "open dashboard". That turns a
		// single omitted environment variable into an administrative bypass.
		return func(ctx *breeze.Context) error {
			return respondAuthUnconfigured(ctx, base)
		}
	}
	wantUser := hashPass(cfg.Username)
	wantPass := hashPass(cfg.Password)

	return func(ctx *breeze.Context) error {
		p := ctx.Req.Path

		// A valid session cookie is the normal way in.
		token := extractCookieValue(ctx.Req.Header["cookie"], sessionCookieName)
		if token != "" {
			if username, ok := sessions.valid(token); ok {
				if !requestOriginOK(ctx, false) {
					return respondCrossOrigin(ctx)
				}
				ctx.Set("breeze.dashboard.user", username)
				ctx.Set(authPassedKey, true)
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
			if ok && credentialsMatch(wantUser, wantPass, user, pass) {
				sessions.clearLoginFailures(peer)
				// A browser can hold Basic credentials and replay them on any request
				// to this origin, so the same cross-origin rule applies here.
				if !requestOriginOK(ctx, false) {
					return respondCrossOrigin(ctx)
				}
				ctx.Set("breeze.dashboard.user", user)

				// If a browser reached the dashboard as http(s)://user:pass@host/…,
				// immediately exchange that Basic-auth credential for a normal
				// HttpOnly session and redirect to the credential-free path. This
				// keeps secrets out of the visible address bar and subsequent
				// navigation/history entries while retaining Basic Auth for API clients.
				accept := ctx.Req.Header["accept"]
				isBrowser := strings.Contains(accept, "text/html") && !strings.Contains(accept, "application/json")
				if isBrowser && p != loginPath {
					token, err := sessions.create(user)
					if err != nil {
						return jsonError(ctx, 500, "could not create login session")
					}
					loc := localPath(p)
					if q := ctx.Req.Query.Encode(); q != "" {
						loc += "?" + q
					}
					ctx.Res = &breeze.HTTPResponse{
						Status: 302,
						Headers: map[string]string{
							"Location":   loc,
							"Set-Cookie": buildSessionCookie(token, base, int(sessionDuration.Seconds()), requestIsSecure(ctx)),
						},
						Body: []byte("redirecting..."),
					}
					ctx.Abort()
					return nil
				}
				ctx.Set(authPassedKey, true)
				return ctx.Next()
			}
			sessions.recordLoginFailure(peer, time.Now())
		}

		// For API requests (JSON), return 401 JSON. For browser requests,
		// redirect to login page.
		accept := ctx.Req.Header["accept"]
		isAPI := strings.Contains(accept, "application/json") || strings.HasPrefix(p, base+"/api/")
		if isAPI {
			body, _ := json.Marshal(map[string]string{"error": "unauthorized", "login": loginPath})
			ctx.Res = &breeze.HTTPResponse{
				Status: 401,
				Headers: map[string]string{
					"Content-Type": "application/json",
				},
				Body: body,
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
		// Browser navigation: redirect to login, remembering the destination so a deep
		// link (a bookmark to /dashboard/requests, a link from a chat) survives the sign-in.
		loc := loginPath
		dest := p
		if q := ctx.Req.Query.Encode(); q != "" {
			dest += "?" + q
		}
		if next := safeNext(base, dest); next != "" {
			loc += "?next=" + url.QueryEscape(next)
		}
		ctx.Res = &breeze.HTTPResponse{
			Status: 302,
			Headers: map[string]string{
				"Location": loc,
			},
			Body: []byte("redirecting to login..."),
		}
		ctx.Abort()

		return nil
	}
}

// respondAuthUnconfigured is the answer when no credentials are configured and auth has
// not been explicitly disabled: a 503 that says how to fix it, in the shape the caller asked for.
func respondAuthUnconfigured(ctx *breeze.Context, base string) error {
	setSecurityHeaders(ctx)
	ctx.Status(503)
	accept := ctx.Req.Header["accept"]
	if strings.Contains(accept, "application/json") || strings.HasPrefix(ctx.Req.Path, base+"/api/") {
		return ctx.JSON(map[string]string{
			"error": "dashboard authentication is not configured",
			"hint":  "set Config.Username and Config.Password, or Config.DisableAuth for a trusted local setup",
		})
	}
	return ctx.WriteString("Dashboard authentication is not configured. " +
		"Set Config.Username and Config.Password (or Config.DisableAuth for a trusted local or reverse-proxy setup) and restart.")
}

// respondCrossOrigin refuses a browser request that another origin caused.
func respondCrossOrigin(ctx *breeze.Context) error {
	setSecurityHeaders(ctx)
	return jsonError(ctx, 403, "cross-origin request blocked")
}

func requestIsSecure(ctx *breeze.Context) bool {
	// X-Forwarded-Proto is a hint from a trusted reverse proxy: take the first
	// comma-delimited value and never treat arbitrary values as secure.
	h := ctx.Req.Header
	if xfp := h["x-forwarded-proto"]; xfp != "" {
		if i := strings.IndexByte(xfp, ','); i >= 0 {
			xfp = xfp[:i]
		}
		if strings.EqualFold(strings.TrimSpace(xfp), "https") {
			return true
		}
	}
	return strings.EqualFold(strings.TrimSpace(h["x-forwarded-ssl"]), "on")
}

// dashboardPeer identifies the client for login throttling. It is the IP only:
// RemoteAddr() is "ip:port", and the source port changes with every new TCP
// connection, so keying on the full string gave an attacker a fresh, unthrottled
// bucket per connection — the lockout never engaged.
func dashboardPeer(ctx *breeze.Context) string {
	if ctx.Conn == nil {
		return "unknown"
	}
	addr := ctx.Conn.RemoteAddr()
	if addr == nil {
		return "unknown"
	}
	return peerKey(addr.String())
}

// peerKey reduces "ip:port" (or "[v6]:port") to a throttle key. IPv6 clients are
// collapsed to their /64, because a single subscriber routinely controls the
// whole /64 and would otherwise rotate through 2^64 source addresses.
func peerKey(remote string) string {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			return ip4.String()
		}
		return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
	}
	return host
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

// hashPass returns a keyed digest of the password for constant-time comparison.
//
// This used to be 210 000 rounds of PBKDF2, which is the right tool for *storing*
// a password someone may steal. Here the configured password already sits in
// memory in the clear, so the work factor protected nothing — while costing tens
// of milliseconds of CPU for every Basic-auth attempt, on the event loop, before
// any throttle could apply. That is a cheap amplification primitive for an
// unauthenticated attacker. A process-keyed HMAC gives the same fixed-size,
// timing-independent comparison at microsecond cost; brute-force resistance is
// the job of the login throttle, not of slowing every legitimate request.
func hashPass(p string) []byte {
	m := hmac.New(sha256.New, dashboardAuthSalt)
	m.Write([]byte(p))
	return m.Sum(nil)
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
