package middleware

// bench_test.go — microbenchmarks for the middleware chain.
//
// These middlewares run on every request of an application that installs them,
// and they all work the same way: a series of ctx.SetHeader calls followed by
// ctx.Next(). The response header map is what that costs, so these benchmarks
// exist to measure it rather than assume it.
//
// Run:
//
//	go test ./middlewares/ -run XXX -bench ZZ -benchmem

import (
	"testing"

	"github.com/nelthaarion/breeze/v2"
)

// benchCtx returns a Context that looks like a parsed GET.
func benchCtx(method breeze.Method) *breeze.Context {
	return breeze.NewContext(method, "/api/users")
}

// runMW drives one request through mw with a handler that sets a 200.
func runMW(b *testing.B, mw breeze.HandlerFunc, method breeze.Method) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ctx := benchCtx(method)
		ctx.SetMiddlewareChain([]breeze.HandlerFunc{mw}, func(c *breeze.Context) error {
			return c.WriteString("ok")
		})
		if err := ctx.Next(); err != nil {
			b.Fatalf("chain failed: %v", err)
		}
	}
}

// BenchmarkZZChainOnly is the fixture cost every number below includes: build a
// Context, install a one-entry chain, run it, write a small body.
func BenchmarkZZChainOnly(b *testing.B) {
	runMW(b, func(c *breeze.Context) error { return c.Next() }, breeze.GET)
}

// BenchmarkZZSecurityDefault is DefaultSecurityMiddleware, which sets twelve
// headers. Twelve SetHeader calls into a map the first one allocates is the
// worst case for the response header map's growth.
func BenchmarkZZSecurityDefault(b *testing.B) {
	runMW(b, DefaultSecurityMiddleware(), breeze.GET)
}

// BenchmarkZZCORS is the ordinary (non-preflight) CORS path: three headers and
// on to the handler.
func BenchmarkZZCORS(b *testing.B) {
	mw := CORSMiddleware(CORSOptions{
		AllowOrigins: "*",
		AllowMethods: "GET,POST,PUT,DELETE",
		AllowHeaders: "Content-Type,Authorization",
	})
	runMW(b, mw, breeze.GET)
}

// BenchmarkZZCORSPreflight is the OPTIONS short-circuit, which never reaches
// the handler.
func BenchmarkZZCORSPreflight(b *testing.B) {
	mw := CORSMiddleware(CORSOptions{
		AllowOrigins: "*",
		AllowMethods: "GET,POST,PUT,DELETE",
		AllowHeaders: "Content-Type,Authorization",
		MaxAge:       "600",
	})
	runMW(b, mw, breeze.OPTIONS)
}

// BenchmarkZZCORSPlusSecurity is what a real application installs: both, in
// order. Fifteen headers land on one response.
func BenchmarkZZCORSPlusSecurity(b *testing.B) {
	cors := CORSMiddleware(CORSOptions{
		AllowOrigins: "*",
		AllowMethods: "GET,POST",
		AllowHeaders: "Content-Type",
	})
	sec := DefaultSecurityMiddleware()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ctx := benchCtx(breeze.GET)
		ctx.SetMiddlewareChain([]breeze.HandlerFunc{cors, sec}, func(c *breeze.Context) error {
			return c.WriteString("ok")
		})
		if err := ctx.Next(); err != nil {
			b.Fatalf("chain failed: %v", err)
		}
	}
}
