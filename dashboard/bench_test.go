package dashboard

// bench_test.go — microbenchmarks for the dashboard's request path.
//
// Middleware wraps every request in an instrumented application, so its cost is
// paid whether or not anyone has the dashboard open. That makes the "nobody is
// watching" number the important one: it is pure overhead on a production
// server, and it should be a clock read and a couple of atomics.
//
// Run:
//
//	go test ./dashboard/ -run XXX -bench ZZ -benchmem
//
// The interesting comparisons:
//
//	MiddlewareIdle      vs MiddlewareWatched — what capturing a full record costs
//	MiddlewareSkipped                       — the dashboard's own routes
//	PushEventIdle                           — the no-op when nobody is connected
//	SnapshotMessage                         — the once-a-second broadcast payload

import (
	"testing"
	"time"

	"github.com/nelthaarion/breeze/v2"
)

// benchCollector returns a collector with the dashboard enabled and no hub, so
// clientCount() is zero and the middleware takes its fast path.
func benchCollector(tb testing.TB) *Collector {
	tb.Helper()
	cfg := DefaultConfig()
	cfg.Enabled = true
	return newCollector(cfg, nil)
}

// benchRequestCtx builds a Context that looks like a parsed GET, including the
// X-Forwarded-For header that pushes the middleware through trackUniqueIP.
func benchRequestCtx(path string) *breeze.Context {
	ctx := breeze.NewContext(breeze.GET, path)
	ctx.Req.Header["x-forwarded-for"] = "203.0.113.7"
	ctx.Req.Header["user-agent"] = "bombardier"
	return ctx
}

// runMiddleware drives one request through mw with a handler that writes a
// small JSON body, which is what a real route does.
//
// The Context is rebuilt per iteration because the middleware reads ctx.Res
// after the chain returns, and reusing one would measure a request that already
// has a response attached. That fixture is not free, so
// BenchmarkZZChainOnly reports its cost separately — the middleware's own
// overhead is the gap between the two, not the absolute number here.
func runMiddleware(b *testing.B, mw breeze.HandlerFunc, path string) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ctx := benchRequestCtx(path)
		ctx.SetMiddlewareChain([]breeze.HandlerFunc{mw}, func(c *breeze.Context) error {
			c.Status(200)
			return nil
		})
		if err := ctx.Next(); err != nil {
			b.Fatalf("chain failed: %v", err)
		}
	}
}

// BenchmarkZZChainOnly is the fixture cost: build a Context, install a one-entry
// chain, run it. Every number below includes this, so it is what they are read
// against.
func BenchmarkZZChainOnly(b *testing.B) {
	passthrough := func(c *breeze.Context) error { return c.Next() }
	runMiddleware(b, passthrough, "/api/users")
}

// BenchmarkZZMiddlewareIdle is the number that matters in production: the
// dashboard is installed, nobody is watching, and the request is neither slow
// nor failing. Everything this costs is overhead.
func BenchmarkZZMiddlewareIdle(b *testing.B) {
	c := benchCollector(b)
	runMiddleware(b, Middleware(c), "/api/users")
}

// BenchmarkZZMiddlewareSkipped is a request to the dashboard's own routes,
// which are not instrumented. It is the floor the two above are measured
// against — a prefix test and a Next.
func BenchmarkZZMiddlewareSkipped(b *testing.B) {
	c := benchCollector(b)
	runMiddleware(b, Middleware(c), "/dashboard/api/metrics")
}

// BenchmarkZZMiddlewareSlow crosses the SlowRequestMs threshold without a hub,
// so the full record is captured on a server nobody is watching. This is the
// path a struggling production server takes on every slow request.
func BenchmarkZZMiddlewareSlow(b *testing.B) {
	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.SlowRequestMs = 0 // every request counts as slow
	c := newCollector(cfg, nil)
	runMiddleware(b, Middleware(c), "/api/users")
}

// BenchmarkZZTrackDailyCount isolates the per-request tally, which runs ahead of
// the "is anyone watching" check and therefore on every request.
func BenchmarkZZTrackDailyCount(b *testing.B) {
	c := benchCollector(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.trackDailyCount()
	}
}

// BenchmarkZZTrackUniqueIP measures the already-seen case, which is what a
// deployment behind a proxy pays per request.
func BenchmarkZZTrackUniqueIP(b *testing.B) {
	c := benchCollector(b)
	c.trackUniqueIP("203.0.113.7")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.trackUniqueIP("203.0.113.7")
	}
}

// BenchmarkZZPushEventIdle measures pushEvent with no clients attached — the
// call the request path makes on a server whose dashboard nobody has open.
func BenchmarkZZPushEventIdle(b *testing.B) {
	c := benchCollector(b)
	h := newWSHub(c)
	defer h.close()
	rec := RequestRecord{ID: "r", Time: time.Now(), Method: "GET", Path: "/x", Status: 200}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pushEvent(h, "request", rec)
	}
}

// BenchmarkZZSnapshotMessage measures the snapshot envelope the hub broadcasts
// once a second per connected client.
func BenchmarkZZSnapshotMessage(b *testing.B) {
	c := benchCollector(b)
	h := newWSHub(c)
	defer h.close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if s := h.snapshotMessage(); s == "" {
			b.Fatal("empty snapshot")
		}
	}
}
