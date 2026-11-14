package dashboard

// bench_capture_test.go — what attributing every request to its route costs.
//
// The middleware used to attribute a request to a route only when it captured a full
// record, so on the path every request takes it did no route work at all. Counting every
// request per route and status class means resolving the route on that path, and the
// price of that is the number to know. These benchmarks use a router with a realistic
// table, because a benchmark with no routes (benchCollector's) resolves nothing and would
// report a cost of zero.
//
// Read each against BenchmarkZZChainOnly, which is the fixture, and against
// BenchmarkZZMiddlewareDisabled, which is the cost of having the middleware installed with
// the dashboard off: that one must not move.
//
//	go test ./dashboard/ -run XXX -bench 'ZZ(Routed|MiddlewareDisabled|ChainOnly)' -benchmem

import (
	"fmt"
	"testing"

	"github.com/nelthaarion/breeze/v2"
)

// routedCollector is a collector enabled over a router with about fifty routes: static
// ones, ones with parameters, and a wildcard mount, in an order where a parameter route
// registered early could shadow a static one registered later.
func routedCollector(tb testing.TB, enabled bool) *Collector {
	tb.Helper()

	router := breeze.NewRouter()
	h := func(ctx *breeze.Context) error { return ctx.JSON(map[string]any{"ok": true}) }
	for i := 0; i < 20; i++ {
		router.Handle(breeze.GET, fmt.Sprintf("/api/v1/resource%d", i), h)
		router.Handle(breeze.POST, fmt.Sprintf("/api/v1/resource%d", i), h)
	}
	for i := 0; i < 10; i++ {
		router.Handle(breeze.GET, fmt.Sprintf("/api/v1/resource%d/:id", i), h)
		router.Handle(breeze.GET, fmt.Sprintf("/api/v1/resource%d/:id/items/:item", i), h)
	}
	router.Handle(breeze.GET, "/static/*filepath", h)
	router.Handle(breeze.GET, "/api/users/:id", h)
	router.Handle(breeze.GET, "/api/users/me", h)

	cfg := DefaultConfig()
	cfg.Enabled = enabled
	return newCollector(cfg, router)
}

// runMiddlewareStatus is runMiddleware with the status the handler answers.
func runMiddlewareStatus(b *testing.B, mw breeze.HandlerFunc, path string, status int) {
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx := benchRequestCtx(path)
		ctx.SetMiddlewareChain([]breeze.HandlerFunc{mw}, func(c *breeze.Context) error {
			c.Status(status)
			return nil
		})
		if err := ctx.Next(); err != nil {
			b.Fatalf("chain failed: %v", err)
		}
	}
}

// BenchmarkZZMiddlewareDisabled is the middleware installed with the dashboard off. It is
// what an application pays for having the line in its setup, and it must stay a flag test
// and a Next.
func BenchmarkZZMiddlewareDisabled(b *testing.B) {
	c := routedCollector(b, false)
	runMiddlewareStatus(b, Middleware(c), "/api/v1/resource7", 200)
}

func BenchmarkZZRoutedStatic200(b *testing.B) {
	c := routedCollector(b, true)
	runMiddlewareStatus(b, Middleware(c), "/api/v1/resource7", 200)
}

func BenchmarkZZRoutedParam200(b *testing.B) {
	c := routedCollector(b, true)
	runMiddlewareStatus(b, Middleware(c), "/api/v1/resource7/12345", 200)
}

func BenchmarkZZRoutedDeepParam200(b *testing.B) {
	c := routedCollector(b, true)
	runMiddlewareStatus(b, Middleware(c), "/api/v1/resource7/12345/items/987", 200)
}

func BenchmarkZZRoutedWildcard200(b *testing.B) {
	c := routedCollector(b, true)
	runMiddlewareStatus(b, Middleware(c), "/static/css/site/main.css", 200)
}

// The two a flood is made of. Neither should cost more than a success once the route
// counters are warm, and neither may allocate without bound.
func BenchmarkZZRoutedUnmatched404(b *testing.B) {
	c := routedCollector(b, true)
	runMiddlewareStatus(b, Middleware(c), "/wp-admin/setup.php", 404)
}

func BenchmarkZZRoutedStatic404Flood(b *testing.B) {
	c := routedCollector(b, true)
	runMiddlewareStatus(b, Middleware(c), "/api/v1/resource7", 404)
}

func BenchmarkZZRoutedStatic500Flood(b *testing.B) {
	c := routedCollector(b, true)
	runMiddlewareStatus(b, Middleware(c), "/api/v1/resource7", 500)
}

// BenchmarkZZRoutedStatic200Parallel runs the success path from every available CPU
// against one route, which is where shared counters would show contention. On a machine
// with one CPU it measures nothing about contention and says so by being the serial number.
func BenchmarkZZRoutedStatic200Parallel(b *testing.B) {
	c := routedCollector(b, true)
	mw := Middleware(c)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			ctx := benchRequestCtx("/api/v1/resource7")
			ctx.SetMiddlewareChain([]breeze.HandlerFunc{mw}, func(c *breeze.Context) error {
				c.Status(200)
				return nil
			})
			if err := ctx.Next(); err != nil {
				b.Errorf("chain failed: %v", err)
				return
			}
		}
	})
}

// BenchmarkZZRouteIndexFind isolates the lookup the middleware added, against the router's own
// Find for the same request: the second is what the application already pays for the same
// question, which is the scale the first should be read against.
func BenchmarkZZRouteIndexFind(b *testing.B) {
	c := routedCollector(b, true)
	ix := c.buildRouteIndex()
	for _, tc := range []struct{ name, path string }{
		{"static", "/api/v1/resource7"},
		{"param", "/api/v1/resource7/12345"},
		{"deepparam", "/api/v1/resource7/12345/items/987"},
		{"wildcard", "/static/css/site/main.css"},
		{"unmatched", "/wp-admin/setup.php"},
	} {
		b.Run("index/"+tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if e := ix.find("GET", tc.path); e == nil && tc.name != "unmatched" {
					b.Fatal("no route")
				}
			}
		})
		b.Run("router/"+tc.name, func(b *testing.B) {
			ctx := breeze.NewContext(breeze.GET, tc.path)
			b.ReportAllocs()
			for b.Loop() {
				c.router.Find(ctx.Req)
			}
		})
	}
}
