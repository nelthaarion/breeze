// Package authguard is the bearer-token handling both MCP transports share.
//
// # Why this is its own package
//
// There are two HTTP transports for MCP in this repository: the control-plane
// transport in internal/mcp (breeze-mcp --port, and the embedded app-runtime
// endpoint) and the Auto-MCP transport in package breeze (EnableMCP). They live in
// different packages for a good reason — the second must not import the first — and
// the cost of that separation is that anything security-relevant they both need would
// otherwise be written twice. Written twice, it drifts: one copy gets the
// constant-time comparison, the other gets it a release later.
//
// So the three pieces both need live here, with no dependencies, and neither
// transport owns a private copy:
//
//   - Equal, the token comparison;
//   - ValidateRemote, the minimum strength of an operator-supplied token that guards a
//     listener reachable beyond loopback;
//   - Limiter, the throttle that turns an online token-guessing attack from "as fast as
//     the network allows" into "a handful of attempts per minute".
package authguard

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// MinRemoteTokenLength is the shortest operator-supplied token accepted for a listener
// that is reachable beyond loopback.
//
// Sixteen characters is the floor, not a recommendation. A generated token is 64 hex
// characters (256 bits); this exists to refuse the values people type — "secret",
// "devtoken", "changeme" — when the only thing between the network and a code generator
// is the token. Loopback is exempt: there the bind is the first lock and the token the
// second, so a short development token costs nothing a local attacker could not already
// do, and refusing it would break working local setups for no gain.
const MinRemoteTokenLength = 16

// Equal reports whether presented is the expected token.
//
// Both values are hashed first and the digests compared in constant time. Comparing the
// raw strings with subtle.ConstantTimeCompare is constant-time only for equal lengths —
// it returns immediately when the lengths differ, so a caller timing responses learns
// how long the token is. A fixed-size digest removes that: the comparison always looks
// at 32 bytes, whatever was presented.
func Equal(presented, expected string) bool {
	a := sha256.Sum256([]byte(presented))
	b := sha256.Sum256([]byte(expected))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

// ValidateRemote checks an operator-supplied token that will guard a listener reachable
// beyond loopback.
//
// It refuses a token that is too short, and one containing whitespace or control
// characters: the value travels in an Authorization header, where an embedded space or a
// newline is either silently trimmed (so the operator's token and the client's differ) or
// a header-injection hazard further along a proxy chain.
func ValidateRemote(token string) error {
	if len(token) < MinRemoteTokenLength {
		return fmt.Errorf("the bearer token is %d characters; a listener reachable beyond loopback "+
			"needs at least %d. Leave the token unset on loopback to have a 256-bit one generated, "+
			"or generate one with `openssl rand -hex 32`", len(token), MinRemoteTokenLength)
	}
	for _, r := range token {
		if r <= ' ' || r == 0x7f {
			return errors.New("the bearer token contains whitespace or a control character; " +
				"use only printable characters, for example the output of `openssl rand -hex 32`")
		}
	}
	return nil
}

// ClientKey is the throttle key for a request: the peer's IP address, without the port,
// with an IPv6 address reduced to its /64.
//
// The port is dropped because it changes with every connection, and a limiter keyed on
// it would count every attempt as a new client. The /64 is used for IPv6 because a single
// subscriber routinely controls a whole /64 and can rotate through 2^64 source addresses
// — a per-address lockout would never engage against one. The dashboard's own login
// throttle does the same. An address that does not parse is used whole, which is still a
// stable key for a Unix socket or a test recorder.
func ClientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = strings.TrimSpace(r.RemoteAddr)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return host
	}
	if ip4 := ip.To4(); ip4 != nil {
		return ip4.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// internalNets are address ranges that are not publicly routable but are not covered by
// net.IP.IsPrivate: 100.64.0.0/10 is carrier-grade NAT, which Tailscale and similar
// overlay networks use for their own addresses.
var internalNets = func() []*net.IPNet {
	var nets []*net.IPNet
	for _, cidr := range []string{"100.64.0.0/10"} {
		if _, n, err := net.ParseCIDR(cidr); err == nil {
			nets = append(nets, n)
		}
	}
	return nets
}()

// Throttleable reports whether failed authentication from r's peer should be counted
// toward a lockout: true only for a publicly routable address.
//
// A loopback, private, link-local or overlay-network peer is exempt, and that is the
// point rather than a gap. Such a peer is almost always not the attacker but something
// standing in front of one — a TLS-terminating reverse proxy, a container runtime's
// bridge gateway, a load balancer — and every client behind it shares its address.
// Counting failures per address there would let anybody who can reach the proxy lock
// every legitimate user out by sending ten wrong tokens a minute: the throttle would
// become the denial of service it exists to prevent. Where it can do its job — a
// listener exposed directly to the internet, where each attacker has an address of
// their own — the peer is public and the count applies.
func Throttleable(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = strings.TrimSpace(r.RemoteAddr)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false // a Unix socket or a test recorder: there is no remote peer to count
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	for _, n := range internalNets {
		if n.Contains(ip) {
			return false
		}
	}
	return true
}

// Limiter throttles repeated authentication failures from one client.
//
// After MaxFailures failures inside Window the client is locked out for Lockout, and
// every request from it in that period is refused — including one carrying the right
// token. That last part is the point: a lockout that still answered a correct guess
// would let an attacker simply keep guessing and read the result off the status code.
//
// It is deliberately small. Per-client state is a few words, the table is bounded, and
// nothing in it can grow with attacker-chosen input beyond MaxEntries rows.
type Limiter struct {
	// MaxFailures is how many failures are tolerated inside Window.
	MaxFailures int
	// Window is the period over which failures are counted.
	Window time.Duration
	// Lockout is how long a client that reached MaxFailures is refused.
	Lockout time.Duration
	// MaxEntries bounds the table, so a flood of distinct source addresses cannot
	// grow it without limit.
	MaxEntries int

	mu      sync.Mutex
	entries map[string]*limiterEntry
	now     func() time.Time
}

type limiterEntry struct {
	failures     int
	windowStart  time.Time
	blockedUntil time.Time
	lastSeen     time.Time
}

// NewLimiter returns a Limiter with the defaults used by both transports: ten failures
// in a minute lock a client out for a minute. That is generous to a person who mistyped
// a token and hopeless to a guessing attack — ten tries a minute against a 128-bit or
// larger secret is not a search.
func NewLimiter() *Limiter {
	return &Limiter{
		MaxFailures: 10,
		Window:      time.Minute,
		Lockout:     time.Minute,
		MaxEntries:  4096,
		entries:     make(map[string]*limiterEntry),
		now:         time.Now,
	}
}

// Blocked reports whether key is locked out, and for how much longer.
func (l *Limiter) Blocked(key string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	e, ok := l.entries[key]
	if !ok {
		return 0, false
	}
	now := l.now()
	if now.Before(e.blockedUntil) {
		return e.blockedUntil.Sub(now), true
	}
	return 0, false
}

// Fail records one failed attempt by key and reports whether it caused a lockout.
func (l *Limiter) Fail(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	e, ok := l.entries[key]
	if !ok {
		l.makeRoomLocked(now)
		e = &limiterEntry{windowStart: now}
		l.entries[key] = e
	}
	e.lastSeen = now

	// A window that has run out starts again; so does one whose lockout has ended, so
	// a client that served its lockout gets a fresh allowance rather than one strike.
	if now.Sub(e.windowStart) > l.Window || (!e.blockedUntil.IsZero() && !now.Before(e.blockedUntil)) {
		e.failures = 0
		e.windowStart = now
		e.blockedUntil = time.Time{}
	}

	e.failures++
	if e.failures >= l.MaxFailures {
		e.blockedUntil = now.Add(l.Lockout)
		return true
	}
	return false
}

// Succeed clears key's failure count. A client that authenticates has shown it holds the
// credential, so earlier typos stop counting against it.
func (l *Limiter) Succeed(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}

// makeRoomLocked keeps the table within MaxEntries.
//
// Expired rows go first. If the table is still full, the least recently seen row that is
// not currently locking anyone out is dropped: a row serving a lockout is the one an
// attacker would most like to see evicted, so flooding the table with other addresses
// must not be a way to lift it. Only when every row is a live lockout does the one that
// ends soonest go.
func (l *Limiter) makeRoomLocked(now time.Time) {
	if len(l.entries) < l.MaxEntries {
		return
	}
	var (
		idleKey, lockedKey string
		idleAt, lockedEnd  time.Time
	)
	for key, e := range l.entries {
		locked := now.Before(e.blockedUntil)
		if !locked && now.Sub(e.windowStart) > l.Window {
			delete(l.entries, key)
			continue
		}
		if locked {
			if lockedKey == "" || e.blockedUntil.Before(lockedEnd) {
				lockedKey, lockedEnd = key, e.blockedUntil
			}
			continue
		}
		if idleKey == "" || e.lastSeen.Before(idleAt) {
			idleKey, idleAt = key, e.lastSeen
		}
	}
	if len(l.entries) < l.MaxEntries {
		return
	}
	if idleKey != "" {
		delete(l.entries, idleKey)
		return
	}
	if lockedKey != "" {
		delete(l.entries, lockedKey)
	}
}
