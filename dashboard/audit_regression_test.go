package dashboard

// Regression tests for the audit findings in the dashboard-security category:
//
//   - the login throttle was keyed on "ip:port", so every new TCP connection got a
//     fresh bucket and the lockout never engaged; the table was also unbounded;
//   - the login POST endpoint had no throttle at all;
//   - every credential check cost ~210k PBKDF2 rounds on the request path;
//   - the unique-IP set accepted any X-Forwarded-For value without bound;
//   - a handler returning an error was invisible to errorsTotal.

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nelthaarion/breeze/v2"
)

func TestPeerKeyIgnoresSourcePort(t *testing.T) {
	a, b := peerKey("203.0.113.9:50001"), peerKey("203.0.113.9:61999")
	if a != b {
		t.Fatalf("same client, different source port must share a throttle key: %q vs %q", a, b)
	}
	if a == peerKey("203.0.113.10:50001") {
		t.Fatalf("different clients must not share a key")
	}
}

func TestPeerKeyCollapsesIPv6SubscriberPrefix(t *testing.T) {
	a := peerKey("[2001:db8:abcd:12::1]:443")
	b := peerKey("[2001:db8:abcd:12:ffff:ffff:ffff:ffff]:8080")
	if a != b {
		t.Fatalf("addresses in one /64 must share a key: %q vs %q", a, b)
	}
	if a == peerKey("[2001:db8:abcd:13::1]:443") {
		t.Fatalf("different /64s must not share a key")
	}
}

func TestLoginThrottleEngagesAcrossNewConnections(t *testing.T) {
	s := newSessionStore()
	now := time.Now()
	// Eight failures, each from a different ephemeral source port — exactly what
	// an attacker opening a new connection per guess looks like.
	for i := 0; i < maxLoginFailures; i++ {
		s.recordLoginFailure(peerKey(fmt.Sprintf("198.51.100.7:%d", 40000+i)), now)
	}
	if s.allowLogin(peerKey("198.51.100.7:59999"), now) {
		t.Fatal("throttle must engage for the address regardless of source port")
	}
	if !s.allowLogin(peerKey("198.51.100.8:59999"), now) {
		t.Fatal("an unrelated address must not be blocked")
	}
}

func TestLoginThrottleTableIsBounded(t *testing.T) {
	s := newSessionStore()
	now := time.Now()
	for i := 0; i < maxTrackedPeers*2; i++ {
		s.recordLoginFailure(fmt.Sprintf("10.%d.%d.%d", i>>16&255, i>>8&255, i&255), now)
	}
	s.mu.Lock()
	n := len(s.failed)
	s.mu.Unlock()
	if n > maxTrackedPeers {
		t.Fatalf("throttle table grew to %d entries, cap is %d", n, maxTrackedPeers)
	}
}

func TestActiveBlockSurvivesASprayOfNewAddresses(t *testing.T) {
	s := newSessionStore()
	now := time.Now()
	for i := 0; i < maxLoginFailures; i++ {
		s.recordLoginFailure("192.0.2.1", now)
	}
	for i := 0; i < maxTrackedPeers*2; i++ {
		s.recordLoginFailure(fmt.Sprintf("172.16.%d.%d", i>>8&255, i&255), now)
	}
	if s.allowLogin("192.0.2.1", now) {
		t.Fatal("a blocked address must not be evictable by flooding the table with new ones")
	}
}

func TestHashPassIsCheapAndDeterministic(t *testing.T) {
	if string(hashPass("hunter2")) != string(hashPass("hunter2")) {
		t.Fatal("same password must hash identically within a process")
	}
	if string(hashPass("hunter2")) == string(hashPass("hunter3")) {
		t.Fatal("different passwords must hash differently")
	}
	start := time.Now()
	for i := 0; i < 500; i++ {
		hashPass("hunter2")
	}
	// 500 PBKDF2(210k) derivations take tens of seconds; a keyed HMAC takes
	// microseconds. The bound is loose enough for a loaded CI box.
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("500 credential checks took %v — the check is a CPU-amplification primitive", d)
	}
}

func TestUniqueIPSetIsBoundedAndValidated(t *testing.T) {
	c := newCollector(Config{}, nil)

	for _, junk := range []string{"", "not-an-ip", "<script>", string(make([]byte, 500))} {
		c.trackUniqueIP(junk)
	}
	if n := c.UniqueIPCount(); n != 0 {
		t.Fatalf("non-address header values were stored as viewers: count=%d", n)
	}

	c.trackUniqueIP("203.0.113.5, 10.0.0.1, 10.0.0.2")
	c.trackUniqueIP("203.0.113.5")
	if n := c.UniqueIPCount(); n != 1 {
		t.Fatalf("only the first X-Forwarded-For hop is the client; count=%d, want 1", n)
	}

	for i := 0; i < maxUniqueIPs+5000; i++ {
		c.trackUniqueIP(fmt.Sprintf("10.%d.%d.%d", i>>16&255, i>>8&255, i&255))
	}
	if n := c.UniqueIPCount(); n > maxUniqueIPs {
		t.Fatalf("unique IP set grew to %d, cap is %d", n, maxUniqueIPs)
	}
}

func TestMiddlewareCountsAReturnedErrorAsAnError(t *testing.T) {
	c := newCollector(Config{Enabled: true}, nil)
	mw := Middleware(c)

	ctx := breeze.NewContext(breeze.GET, "/api/orders")
	ctx.Req = &breeze.HTTPRequest{Method: breeze.GET, Path: "/api/orders", Header: map[string]string{}}
	ctx.SetMiddlewareChain([]breeze.HandlerFunc{mw}, func(*breeze.Context) error {
		return errors.New("db down")
	})
	_ = ctx.Next()

	if got := c.errorsTotal.Load(); got != 1 {
		t.Fatalf("errorsTotal = %d, want 1 for a handler that returned an error", got)
	}
}
