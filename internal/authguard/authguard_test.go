package authguard

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestEqual(t *testing.T) {
	cases := []struct {
		name      string
		a, b      string
		wantEqual bool
	}{
		{"identical", "s3cret-token-value", "s3cret-token-value", true},
		{"different same length", "s3cret-token-valuE", "s3cret-token-value", false},
		{"prefix of the real token", "s3cret", "s3cret-token-value", false},
		{"longer than the real token", "s3cret-token-value-extra", "s3cret-token-value", false},
		{"empty presented", "", "s3cret-token-value", false},
		{"both empty", "", "", true}, // callers must never configure an empty token; see ValidateRemote
	}
	for _, tc := range cases {
		if got := Equal(tc.a, tc.b); got != tc.wantEqual {
			t.Errorf("%s: Equal(%q, %q) = %v, want %v", tc.name, tc.a, tc.b, got, tc.wantEqual)
		}
	}
}

func TestValidateRemote(t *testing.T) {
	good := strings.Repeat("a", MinRemoteTokenLength)
	if err := ValidateRemote(good); err != nil {
		t.Errorf("a %d-character printable token was refused: %v", len(good), err)
	}
	if err := ValidateRemote(strings.Repeat("a", MinRemoteTokenLength-1)); err == nil {
		t.Error("a token one character short of the minimum was accepted")
	}
	for _, bad := range []string{
		"aaaaaaaa aaaaaaaa", "aaaaaaaa\taaaaaaaa", "aaaaaaaa\naaaaaaaa", "aaaaaaaa\r\naaaaaaaa", "aaaaaaaaaaaaaaaa\x00", "aaaaaaaaaaaaaaaa\x7f",
	} {
		if err := ValidateRemote(bad); err == nil {
			t.Errorf("token %q contains whitespace or a control character and was accepted", bad)
		}
	}
}

func TestClientKeyDropsThePort(t *testing.T) {
	for remote, want := range map[string]string{
		"10.1.2.3:53551":         "10.1.2.3",
		"[::ffff:10.1.2.3]:5":    "10.1.2.3", // an IPv4 peer on a dual-stack listener
		"[2001:db8::1]:443":      "2001:db8::/64",
		"[2001:db8::ffff:1]:443": "2001:db8::/64", // same /64, same client
		"[2001:db8:0:1::1]:443":  "2001:db8:0:1::/64",
		"unix-socket":            "unix-socket",
		"":                       "",
	} {
		r := &http.Request{RemoteAddr: remote}
		if got := ClientKey(r); got != want {
			t.Errorf("ClientKey(%q) = %q, want %q", remote, got, want)
		}
	}
}

func TestThrottleableOnlyForPublicPeers(t *testing.T) {
	for remote, want := range map[string]bool{
		"203.0.113.7:4444":    true, // documentation range: routable as far as this check can tell
		"8.8.8.8:1":           true,
		"[2001:db8::1]:443":   true,
		"127.0.0.1:5000":      false, // loopback
		"[::1]:5000":          false,
		"10.1.2.3:1":          false, // RFC 1918
		"172.17.0.1:1":        false, // a container runtime's bridge gateway
		"192.168.1.20:1":      false,
		"[fd12:3456::1]:1":    false, // unique local
		"169.254.1.1:1":       false, // link-local
		"100.64.0.5:1":        false, // carrier-grade NAT / Tailscale
		"100.127.255.255:1":   false,
		"100.128.0.1:1":       true, // just outside 100.64.0.0/10
		"[::ffff:10.0.0.1]:1": false,
		"unix-socket":         false,
		"":                    false,
	} {
		r := &http.Request{RemoteAddr: remote}
		if got := Throttleable(r); got != want {
			t.Errorf("Throttleable(%q) = %v, want %v", remote, got, want)
		}
	}
}

// clock is a controllable time source.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestLimiter(c *clock) *Limiter {
	l := NewLimiter()
	l.now = c.now
	return l
}

func TestLimiterLocksOutAfterMaxFailures(t *testing.T) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	l := newTestLimiter(c)

	for i := 1; i < l.MaxFailures; i++ {
		if locked := l.Fail("1.2.3.4"); locked {
			t.Fatalf("locked out after %d failures; the limit is %d", i, l.MaxFailures)
		}
		if _, blocked := l.Blocked("1.2.3.4"); blocked {
			t.Fatalf("blocked after %d failures; the limit is %d", i, l.MaxFailures)
		}
	}
	if locked := l.Fail("1.2.3.4"); !locked {
		t.Fatalf("not locked out after %d failures", l.MaxFailures)
	}
	wait, blocked := l.Blocked("1.2.3.4")
	if !blocked || wait <= 0 || wait > l.Lockout {
		t.Fatalf("Blocked = (%v, %v), want a positive wait of at most %v", wait, blocked, l.Lockout)
	}
	// Another client is unaffected.
	if _, blocked := l.Blocked("5.6.7.8"); blocked {
		t.Error("a lockout of one client blocked another")
	}
}

func TestLimiterLockoutExpiresAndAllowanceResets(t *testing.T) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	l := newTestLimiter(c)

	for i := 0; i < l.MaxFailures; i++ {
		l.Fail("k")
	}
	if _, blocked := l.Blocked("k"); !blocked {
		t.Fatal("expected a lockout")
	}
	c.advance(l.Lockout + time.Second)
	if _, blocked := l.Blocked("k"); blocked {
		t.Fatal("still blocked after the lockout period")
	}
	// A client that served its lockout gets a full allowance again, not a single strike.
	for i := 1; i < l.MaxFailures; i++ {
		if l.Fail("k") {
			t.Fatalf("locked out again after only %d failures following a served lockout", i)
		}
	}
}

func TestLimiterWindowForgetsOldFailures(t *testing.T) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	l := newTestLimiter(c)

	for i := 0; i < l.MaxFailures-1; i++ {
		l.Fail("k")
	}
	c.advance(l.Window + time.Second)
	// One more failure, but the earlier ones are outside the window.
	if l.Fail("k") {
		t.Error("failures older than the window still counted toward a lockout")
	}
}

func TestLimiterSucceedClearsFailures(t *testing.T) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	l := newTestLimiter(c)

	for i := 0; i < l.MaxFailures-1; i++ {
		l.Fail("k")
	}
	l.Succeed("k")
	for i := 0; i < l.MaxFailures-1; i++ {
		if l.Fail("k") {
			t.Fatal("a successful authentication did not clear earlier failures")
		}
	}
}

func TestLimiterTableIsBounded(t *testing.T) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	l := newTestLimiter(c)
	l.MaxEntries = 64

	for i := 0; i < 10_000; i++ {
		c.advance(time.Millisecond)
		l.Fail(string(rune('a'+i%26)) + strings.Repeat("x", i%50) + time.Duration(i).String())
	}
	l.mu.Lock()
	n := len(l.entries)
	l.mu.Unlock()
	if n > l.MaxEntries {
		t.Fatalf("table holds %d rows, limit is %d: a flood of distinct addresses grows it without bound", n, l.MaxEntries)
	}
}

func TestLimiterEvictionKeepsActiveLockouts(t *testing.T) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	l := newTestLimiter(c)
	l.MaxEntries = 8

	for i := 0; i < l.MaxFailures; i++ {
		l.Fail("attacker")
	}
	// Many other clients arrive afterwards, each recorded later than the attacker and
	// far more than the table holds, so eviction has to happen repeatedly.
	for i := 0; i < 200; i++ {
		c.advance(time.Millisecond)
		l.Fail("other-" + time.Duration(i).String())
	}
	if _, blocked := l.Blocked("attacker"); !blocked {
		t.Fatal("the locked-out client was evicted while its lockout was still running: " +
			"flooding the table with other addresses would lift the lockout")
	}
	l.mu.Lock()
	n := len(l.entries)
	l.mu.Unlock()
	if n > l.MaxEntries {
		t.Fatalf("table holds %d rows, limit is %d", n, l.MaxEntries)
	}
}
