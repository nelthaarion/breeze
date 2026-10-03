package middleware

// Regression tests for the audit findings in the middleware category:
//
//   - the rate limiter panicked on a context with no connection (Auto-MCP calls);
//   - its key was the formatted "ip:port" string (an allocation per request) and
//     IPv6 clients were not grouped, so one subscriber could rotate through a /64;
//   - entries were pruned on a fixed 10-minute cutoff even when the window was
//     longer, resetting a client's count mid-window;
//   - CORS accepted one origin only and sent no Vary header;
//   - the default JWT lookup threw the refresh token away, so refresh did nothing.

import (
	"net"
	"testing"
	"time"

	jwtlib "github.com/golang-jwt/jwt/v5"
	"github.com/nelthaarion/breeze/v2"
	"github.com/nelthaarion/gnet/v2"
)

// addrConn is a connection that only knows its peer address.
type addrConn struct {
	gnet.Conn
	addr net.Addr
}

func (c addrConn) RemoteAddr() net.Addr { return c.addr }

func ctxFrom(ip string, port int) *breeze.Context {
	c := breeze.NewContext(breeze.GET, "/x")
	c.Conn = addrConn{addr: &net.TCPAddr{IP: net.ParseIP(ip), Port: port}}
	return c
}

func run(mw breeze.HandlerFunc, ctx *breeze.Context) int {
	ctx.SetMiddlewareChain([]breeze.HandlerFunc{mw}, func(c *breeze.Context) error {
		return c.WriteString("ok")
	})
	_ = ctx.Next()
	if ctx.Res == nil {
		return 0
	}
	return ctx.Res.Status
}

func TestRateLimiterDoesNotPanicWithoutAConnection(t *testing.T) {
	mw := NewRateLimiter(RateLimiterOptions{Requests: 2, Per: time.Minute})
	for i := 0; i < 3; i++ {
		ctx := breeze.NewContext(breeze.GET, "/x") // Conn is nil, as for an MCP tool call
		got := run(mw, ctx)
		want := 200
		if i == 2 {
			want = 429 // they share one bucket, so the limit still applies
		}
		if got != want {
			t.Fatalf("call %d: status %d, want %d", i, got, want)
		}
	}
}

func TestRateLimiterIgnoresSourcePort(t *testing.T) {
	mw := NewRateLimiter(RateLimiterOptions{Requests: 2, Per: time.Minute})
	var last int
	for i := 0; i < 3; i++ {
		last = run(mw, ctxFrom("203.0.113.7", 40000+i)) // a new connection each time
	}
	if last != 429 {
		t.Fatalf("a client reconnecting on new ports must share one counter, third request got %d", last)
	}
	if got := run(mw, ctxFrom("203.0.113.8", 40000)); got != 200 {
		t.Fatalf("a different client must not be limited, got %d", got)
	}
}

func TestRateLimiterGroupsAnIPv6SubscriberByPrefix(t *testing.T) {
	mw := NewRateLimiter(RateLimiterOptions{Requests: 2, Per: time.Minute})
	run(mw, ctxFrom("2001:db8:abcd:12::1", 1000))
	run(mw, ctxFrom("2001:db8:abcd:12:aaaa:bbbb:cccc:dddd", 1000))
	if got := run(mw, ctxFrom("2001:db8:abcd:12::99", 1000)); got != 429 {
		t.Fatalf("addresses in one /64 must share a counter, got %d", got)
	}
	if got := run(mw, ctxFrom("2001:db8:abcd:13::1", 1000)); got != 200 {
		t.Fatalf("a different /64 must not be limited, got %d", got)
	}
}

func TestRateLimiterKeyAllocatesNothing(t *testing.T) {
	NewRateLimiter(RateLimiterOptions{Requests: 10, Per: time.Minute})
	rl := rateLimiterHandle.Load()
	ctx := ctxFrom("198.51.100.4", 5555)
	if n := testing.AllocsPerRun(1000, func() { _ = rl.keyFor(ctx) }); n != 0 {
		t.Fatalf("deriving the client key allocated %v times per request, want 0", n)
	}
}

func TestRateLimiterKeyFunc(t *testing.T) {
	mw := NewRateLimiter(RateLimiterOptions{
		Requests: 1, Per: time.Minute,
		KeyFunc: func(c *breeze.Context) string { return c.Req.Header["x-api-key"] },
	})
	mk := func(key string) *breeze.Context {
		c := ctxFrom("10.0.0.1", 1) // every request comes from the same proxy
		c.Req = &breeze.HTTPRequest{Header: map[string]string{"x-api-key": key}}
		return c
	}
	if run(mw, mk("a")) != 200 || run(mw, mk("b")) != 200 {
		t.Fatal("distinct keys behind one proxy must have distinct counters")
	}
	if run(mw, mk("a")) != 429 {
		t.Fatal("the same key must be limited")
	}
}

// An entry is only expendable once its window is over. lastRequest is the window
// start, so with a one-hour window nothing 30 minutes old may be pruned.
func TestPruneKeepsEntriesInsideTheirWindow(t *testing.T) {
	NewRateLimiter(RateLimiterOptions{Requests: 5, Per: time.Hour})
	rl := rateLimiterHandle.Load()
	start := time.Now()
	k := rlKey{custom: "client"}
	sh := &rl.shards[k.shard()]
	sh.mu.Lock()
	sh.clients[k] = &clientData{lastRequest: start, requests: 5}
	rl.pruneLocked(sh, start.Add(30*time.Minute))
	_, kept := sh.clients[k]
	sh.mu.Unlock()
	if !kept {
		t.Fatal("an entry still inside its 1h window was pruned; the client's count would reset")
	}

	sh.mu.Lock()
	sh.lastPrune = time.Time{}
	rl.pruneLocked(sh, start.Add(61*time.Minute))
	_, kept = sh.clients[k]
	sh.mu.Unlock()
	if kept {
		t.Fatal("an entry whose window ended must be pruned")
	}
}

func corsRun(opts CORSOptions, method breeze.Method, origin string) *breeze.Context {
	ctx := breeze.NewContext(method, "/x")
	ctx.Req = &breeze.HTTPRequest{Method: method, Header: map[string]string{}}
	if origin != "" {
		ctx.Req.Header["origin"] = origin
	}
	run(CORSMiddleware(opts), ctx)
	return ctx
}

func TestCORSAllowlistEchoesOnlyListedOrigins(t *testing.T) {
	opts := CORSOptions{AllowOrigins: "https://app.example, https://admin.example", AllowMethods: "GET"}

	ok := corsRun(opts, breeze.GET, "https://admin.example")
	if got := ok.GetHeader("Access-Control-Allow-Origin"); got != "https://admin.example" {
		t.Errorf("listed origin must be echoed, got %q", got)
	}
	if ok.GetHeader("Vary") != "Origin" {
		t.Error("an origin-dependent response must say Vary: Origin")
	}

	bad := corsRun(opts, breeze.GET, "https://evil.example")
	if got := bad.GetHeader("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("an unlisted origin must not be allowed, got %q", got)
	}
	if bad.GetHeader("Vary") != "Origin" {
		t.Error("Vary: Origin must be sent even when the origin is refused")
	}
}

func TestCORSSingleOriginAndWildcardUnchanged(t *testing.T) {
	for _, o := range []string{"*", "https://one.example"} {
		ctx := corsRun(CORSOptions{AllowOrigins: o}, breeze.OPTIONS, "https://whoever.example")
		if got := ctx.GetHeader("Access-Control-Allow-Origin"); got != o {
			t.Errorf("AllowOrigins %q: header = %q", o, got)
		}
		if ctx.Res == nil || ctx.Res.Status != 204 {
			t.Errorf("AllowOrigins %q: preflight must still answer 204", o)
		}
	}
}

func TestJWTDefaultLookupHonoursTheRefreshToken(t *testing.T) {
	const access, refresh = "access-secret", "refresh-secret"
	mw := JWTAuthMiddleware(JWTOptions{AccessSecret: access, RefreshSecret: refresh, EnableRefreshToken: true})

	good, err := GenerateRefreshToken(refresh, jwtlib.MapClaims{"user_id": "u1", "role": "user"}, time.Hour, jwtlib.SigningMethodHS256)
	if err != nil {
		t.Fatal(err)
	}
	expired, _ := GenerateJWT(access, jwtlib.MapClaims{"user_id": "u1"}, -time.Minute, jwtlib.SigningMethodHS256)

	ctx := breeze.NewContext(breeze.GET, "/x")
	ctx.Req = &breeze.HTTPRequest{Header: map[string]string{
		"authorization":   "Bearer " + expired,
		"x-refresh-token": good,
	}}
	if got := run(mw, ctx); got != 200 {
		t.Fatalf("an expired access token with a valid refresh token must be refreshed, got %d", got)
	}
	if ctx.GetHeader("X-New-Access-Token") == "" {
		t.Fatal("a refreshed request must hand back the new access token")
	}

	// And a refresh token is still never accepted as an access token.
	ctx = breeze.NewContext(breeze.GET, "/x")
	ctx.Req = &breeze.HTTPRequest{Header: map[string]string{"authorization": "Bearer " + good}}
	if got := run(mw, ctx); got != 401 {
		t.Fatalf("a refresh token presented as an access token must be refused, got %d", got)
	}
}

func TestDefaultTokenLookupParsing(t *testing.T) {
	cases := []struct {
		header string
		token  string
		ok     bool
	}{
		{"Bearer abc", "abc", true},
		{"bearer abc", "abc", true},
		{"BEARER abc", "abc", true},
		{"Bearer  abc", "", false}, // two spaces
		{"Bearer a b", "", false},
		{"Bearer ", "", false},
		{"Bearer", "", false},
		{"Basic abc", "", false},
		{"Bearerabc", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		ctx := breeze.NewContext(breeze.GET, "/x")
		ctx.Req = &breeze.HTTPRequest{Header: map[string]string{"authorization": tc.header}}
		tok, _, err := DefaultTokenLookup(ctx)
		if (err == nil) != tc.ok || tok != tc.token {
			t.Errorf("header %q: token=%q err=%v, want token=%q ok=%v", tc.header, tok, err, tc.token, tc.ok)
		}
	}
}
