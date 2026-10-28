package middleware

import (
	"testing"

	"github.com/nelthaarion/breeze/v2"
)

var etagBody = []byte(`[{"id":1,"name":"User Number 1","email":"user1@example.com","active":true,"role":"member"},{"id":2,"name":"User Number 2","email":"user2@example.com","active":false,"role":"member"},{"id":3,"name":"User Number 3","email":"user3@example.com","active":true,"role":"member"}]`)

func BenchmarkETagMiddlewareSteadyState(b *testing.B) {
	cache := NewETagCache()
	mw := cache.ETagMiddleware()
	handler := func(c *breeze.Context) error { c.Res = &breeze.HTTPResponse{Status: 200, Body: etagBody}; return nil }
	chain := []breeze.HandlerFunc{mw}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ctx := breeze.NewContext(breeze.GET, "/users")
		ctx.SetMiddlewareChain(chain, handler)
		if err := ctx.Next(); err != nil {
			b.Fatal(err)
		}
	}
}
