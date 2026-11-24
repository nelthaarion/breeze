package dashboard

// security.go — the small, shared pieces the dashboard's request guards are built from.
//
// Kept in one file so the rules read together: who counts as authenticated, which
// requests a browser may make on the dashboard's behalf, and what every dashboard
// response carries so a browser does not cache it or let another site frame it.

import (
	"crypto/subtle"
	"net"
	"net/url"
	"strings"

	"github.com/nelthaarion/breeze/v2"
)

// authPassedKey is set on the request context by AuthMiddleware when — and only when —
// it lets the request through.
//
// Callers that run the middleware inline (wrap, the page routes) used to infer the
// outcome from the status the middleware had written: anything other than 302, 401 or
// 403 meant "authenticated". That guessed wrong for the answers the middleware also
// produces — 429 (login throttled), 503 (credentials not configured) and 500 — so
// those requests fell through to the real handler, which then wrote the dashboard's
// data into a response that merely *carried* the refusal's status code. The data was
// readable by anyone, in the body, whatever the status said. An explicit pass mark makes
// the guard fail closed: a middleware outcome nobody anticipated is a refusal.
const authPassedKey = "breeze.dashboard.authz"

// authorize runs auth against ctx and reports whether the request may proceed.
//
// A false result means the middleware has already written the response (a redirect, a
// 401, a 429, a 503, ...) and the caller must return without running its handler.
func authorize(auth breeze.HandlerFunc, ctx *breeze.Context) (bool, error) {
	// Never trust a mark left on a recycled context: clear it before asking.
	ctx.Set(authPassedKey, false)
	if err := auth(ctx); err != nil {
		return false, err
	}
	v, _ := ctx.Get(authPassedKey)
	pass, _ := v.(bool)
	return pass, nil
}

// credentialsMatch compares a presented username and password with the configured
// ones. wantUser and wantPass are hashPass digests.
//
// Both comparisons always run, over fixed-size digests, and their results are combined
// without branching. Checking the username first and skipping the password on a miss —
// which the login handler did — made a wrong username measurably faster to reject than
// a right one with a wrong password, and ConstantTimeCompare on the raw username
// returns early when the lengths differ, which leaks the length.
func credentialsMatch(wantUser, wantPass []byte, user, pass string) bool {
	u := subtle.ConstantTimeCompare(hashPass(user), wantUser)
	p := subtle.ConstantTimeCompare(hashPass(pass), wantPass)
	return u&p == 1
}

// requestOriginOK reports whether a browser-originated request may act on the
// dashboard.
//
// The session cookie is SameSite=Strict, which stops other *sites*. It does not stop
// another origin on the same site — a different port on localhost, a sibling
// subdomain — and a developer machine runs several of those. Such a page can make the
// browser send the cookie along with a POST (clear the cache, retry jobs, write to the
// database, run an API Explorer request from inside the process), or open the
// WebSocket and read every request the service handles. Fetch Metadata and Origin say
// where a request came from, and a browser always sends them, so:
//
//   - Sec-Fetch-Site, when present, must be same-origin (or none, a typed URL);
//   - otherwise an Origin header, when present, must name this host;
//   - a request with neither is not from a browser (curl, a script, the MCP reader),
//     and is judged by its credentials alone.
//
// Safe methods are not checked unless strict is set. The WebSocket handshake is a GET
// that reads everything, so the hub asks for strict.
func requestOriginOK(ctx *breeze.Context, strict bool) bool {
	if ctx == nil || ctx.Req == nil {
		return true
	}
	if !strict {
		switch string(ctx.Req.Method) {
		case "GET", "HEAD", "OPTIONS", "":
			return true
		}
	}
	h := ctx.Req.Header
	if site := strings.ToLower(strings.TrimSpace(h["sec-fetch-site"])); site != "" {
		return site == "same-origin" || site == "none"
	}
	origin := strings.TrimSpace(h["origin"])
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false // "null", or something that is not an origin
	}
	if strings.EqualFold(u.Host, strings.TrimSpace(h["host"])) {
		return true
	}
	// Behind a proxy that rewrites Host, the public name arrives here.
	fwd := h["x-forwarded-host"]
	if i := strings.IndexByte(fwd, ','); i >= 0 {
		fwd = fwd[:i]
	}
	return fwd != "" && strings.EqualFold(u.Host, strings.TrimSpace(fwd))
}

// setSecurityHeaders adds what every dashboard response should carry.
//
// The content-security-policy is deliberately only the directives that cannot break
// the page (no default-src or script-src: the layout has an inline theme bootstrap and
// the SPA runtime injects scripts). It still forbids framing, which is what stops a page
// on another origin from dressing the dashboard up under its own buttons.
func setSecurityHeaders(ctx *breeze.Context) {
	ctx.SetHeader("X-Content-Type-Options", "nosniff")
	ctx.SetHeader("X-Frame-Options", "DENY")
	ctx.SetHeader("Referrer-Policy", "same-origin")
	ctx.SetHeader("Cache-Control", "no-store")
	ctx.SetHeader("Content-Security-Policy", "frame-ancestors 'none'; base-uri 'self'; form-action 'self'; object-src 'none'")
}

// localPath turns p into something safe to use as a redirect target on this host: a
// single leading slash, nothing a browser would read as a different host ("//evil.example"),
// no backslashes and no control characters.
func localPath(p string) string {
	if p == "" {
		return "/"
	}
	for i := 0; i < len(p); i++ {
		if c := p[i]; c < 0x20 || c == 0x7f || c == '\\' {
			return "/"
		}
	}
	return "/" + strings.TrimLeft(p, "/")
}

// safeNext validates a post-login destination. It returns the destination when it is a
// page of this dashboard, and "" for anything else (another host, the login page, the API,
// an asset), in which case the caller goes to the dashboard's home.
func safeNext(base, next string) string {
	next = strings.TrimSpace(next)
	if next == "" || strings.HasPrefix(next, "//") || strings.ContainsAny(next, "\\\r\n\t") {
		return ""
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil {
		return ""
	}
	p := localPath(u.Path)
	if p != base && !strings.HasPrefix(p, base+"/") {
		return ""
	}
	switch {
	case p == base+"/login", p == base+"/logout",
		strings.HasPrefix(p, base+"/api/"), strings.HasPrefix(p, base+"/assets/"):
		return ""
	}
	if u.RawQuery != "" {
		p += "?" + u.RawQuery
	}
	return p
}

// sanitizeIP returns s when it is a textual IP address, and "" otherwise. Client addresses
// come from X-Forwarded-For, which any client can set to anything, and end up in tables,
// JSON and a persisted set.
func sanitizeIP(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 45 || net.ParseIP(s) == nil {
		return ""
	}
	return s
}
