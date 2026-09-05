package dashboard

// explorer_target.go — deciding where an API Explorer request is allowed to go.
//
// # The hole this closes
//
// POST /dashboard/api/api-explorer takes a URL and the *server* fetches it. Before
// this file existed, that URL could be anything: the handler resolved a relative
// path against the request's Host header and passed an absolute one through
// untouched.
//
// A request the server makes is not a request the caller could have made. It comes
// from inside the deployment, so it reaches what the deployment reaches:
//
//   - http://169.254.169.254/latest/meta-data/iam/security-credentials/ — the
//     cloud instance metadata service, which hands out role credentials to
//     anything that asks from the right network position;
//   - http://10.0.0.5:5432, http://postgres:5432, http://redis:6379 — internal
//     hostnames that resolve only inside the cluster;
//   - http://127.0.0.1:9090 — every other port on this host, including admin
//     interfaces bound to loopback precisely because loopback was assumed safe.
//
// The response body came back verbatim in the JSON, so this was a read primitive,
// not a blind one.
//
// # Why the fix is a whitelist and not a blacklist
//
// Blocking "169.254.169.254" and the RFC1918 ranges is the tempting fix and it
// does not work. A hostname the attacker controls can resolve to any address, and
// resolve differently on the second lookup than the first (DNS rebinding), so a
// check on the name is not a check on the destination and a check on the address
// is not a check on the address that gets connected to.
//
// The explorer does not need arbitrary destinations. It exists to call the routes
// listed on the page beside it, which are this service's own. So the destination is
// constructed here — loopback, plus the port this process is listening on — and the
// caller's URL contributes only a path and query. There is nothing for a hostname
// to resolve to, because no caller-supplied hostname is ever dialled.
//
// An absolute URL is still accepted, because the UI's snippet panel shows one and a
// developer will paste one back. It is accepted by *comparison*: parse it, confirm
// the host is a loopback name and the port is ours, and then use the locally built
// target anyway.

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// errExplorerTargetForeign is returned for a URL naming anything but this service.
//
// The message names the constraint rather than the input: someone pasting a URL
// into the explorer needs to know what the field accepts, and echoing their host
// back adds nothing they did not just type.
var errExplorerTargetForeign = errors.New(
	"the API Explorer only calls this service — use a path like /users/1, or a full " +
		"URL on this service's own host and port")

// loopbackHosts are the host spellings accepted in an absolute explorer URL.
//
// "localhost" is included by name rather than resolved. Resolving it would make
// the check depend on /etc/hosts, and a machine that maps localhost to something
// exotic is not a machine where this endpoint should start dialling it.
var loopbackHosts = map[string]bool{
	"localhost": true,
	"127.0.0.1": true,
	"::1":       true,
	"[::1]":     true,
}

// explorerTarget returns the URL the explorer will actually request.
//
// port is this service's listening port, from breeze.ListenPort. raw is the
// caller's url field. hostHeader is the request's Host header, used only to
// recover a port when the listener has not recorded one — never as a hostname.
//
// The returned URL always points at loopback. That is the invariant the whole file
// exists to hold, so it is enforced in one place with one construction rather than
// by validating several shapes of input.
func explorerTarget(raw, hostHeader string, port int) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("url required")
	}

	// A scheme-relative URL ("//evil.example/x") is a host, not a path, and
	// url.Parse agrees — but it starts with "/" and so would pass a naive prefix
	// check. Rejecting it up front keeps the path branch below honestly about
	// paths.
	if strings.HasPrefix(raw, "//") {
		return "", errExplorerTargetForeign
	}

	if port <= 0 {
		// No recorded listener: the Collector was built without an app, or Run has
		// not been called. Fall back to the port in the Host header, which is the
		// port the browser reached this dashboard on and therefore this service's.
		//
		// Only the port is taken. The host is discarded and loopback substituted,
		// so a forged Host header can redirect this to a different port on this
		// machine and nothing else. That is a narrow enough residue to accept in
		// exchange for the explorer working in a test harness.
		port = portFromHostHeader(hostHeader)
	}
	if port <= 0 {
		return "", errors.New("the API Explorer cannot determine this service's port; " +
			"it is available once the application is running")
	}

	if strings.HasPrefix(raw, "/") {
		return explorerURL(port, raw), nil
	}
	return explorerAbsolute(raw, port)
}

// explorerAbsolute validates an absolute URL and rebuilds it against loopback.
func explorerAbsolute(raw string, port int) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		// Covers the schemes that are not HTTP at all — file:, gopher:, dict: — each
		// of which is a distinct exfiltration or protocol-smuggling trick against
		// whatever client happens to support it.
		return "", errExplorerTargetForeign
	}
	if parsed.User != nil {
		// Credentials in the URL are never something the explorer needs, and
		// http://ours@evil.example/ is a classic way to make a host check read the
		// wrong component.
		return "", errExplorerTargetForeign
	}
	if !loopbackHosts[strings.ToLower(parsed.Hostname())] {
		return "", errExplorerTargetForeign
	}
	if p := parsed.Port(); p != "" && p != strconv.Itoa(port) {
		return "", errExplorerTargetForeign
	}

	// Rebuilt rather than forwarded. parsed is only ever used for its path and
	// query from here, so no part of the caller's authority can survive into the
	// address that gets dialled.
	target := parsed.EscapedPath()
	if target == "" {
		target = "/"
	}
	if parsed.RawQuery != "" {
		target += "?" + parsed.RawQuery
	}
	return explorerURL(port, target), nil
}

// explorerURL builds the one address this file will ever produce.
func explorerURL(port int, pathAndQuery string) string {
	return "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) + pathAndQuery
}

// listenPort reports the port the application is serving on, or 0 when the
// Collector has no application (tests, or a Collector used only for recording).
//
// Separate from a direct c.app.ListenPort() call so the nil case lives in one
// place: a nil app is normal in this package, not an error.
func (c *Collector) listenPort() int {
	if c.app == nil {
		return 0
	}
	return c.app.ListenPort()
}

// portFromHostHeader extracts the port from a Host header, or 0.
//
// A Host with no port means the default for the scheme; the dashboard is served
// over plain HTTP by the framework's own listener, so that is 80.
func portFromHostHeader(host string) int {
	host = strings.TrimSpace(host)
	if host == "" {
		return 0
	}
	if _, p, err := net.SplitHostPort(host); err == nil {
		if n, err := strconv.Atoi(p); err == nil && n > 0 && n <= 65535 {
			return n
		}
		return 0
	}
	// No colon at all — a bare hostname. Anything else (a bracketed IPv6 literal
	// without a port, a malformed value) is not something to guess a port from.
	if strings.ContainsAny(host, ":[]") {
		return 0
	}
	return 80
}
