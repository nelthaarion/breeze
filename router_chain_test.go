package breeze

// router_chain_test.go — the precomputed middleware chain.
//
// findChain composes [global..., route..., handler] once at registration time so
// the request path does not build a slice per request. That makes two things
// testable that were previously implicit in the benchmark file these came from:
// the composed order, and the fact that a global middleware registered *after* a
// route still reaches it.
//
// Split from the benchmarks because they answer different questions. A benchmark
// that allocates zero proves nothing about whether the chain it built is in the
// right order, and these two tests are the only thing that does.

import "testing"

// TestFindChainComposition verifies the precomputed chain is
// [global..., route..., handler] in the correct order and length.
func TestFindChainComposition(t *testing.T) {
	r := NewRouter()
	r.autoServeRoot = false

	// Each middleware appends its tag then calls Next to advance the chain,
	// exactly like real middlewares do. The final handler does not call Next.
	var order []string
	r.Use(
		func(c *Context) error { order = append(order, "g1"); return c.Next() },
		func(c *Context) error { order = append(order, "g2"); return c.Next() },
	)
	r.Handle(GET, "/x", func(*Context) error {
		order = append(order, "handler")
		return nil
	},
		func(c *Context) error { order = append(order, "r1"); return c.Next() },
	)

	req := &HTTPRequest{Method: GET, Path: "/x"}
	chain, _ := r.findChain(req)
	if chain == nil {
		t.Fatal("expected a match for /x")
	}
	// 2 global + 1 route mw + 1 handler = 4
	if len(chain) != 4 {
		t.Fatalf("expected chain length 4, got %d", len(chain))
	}

	ctx := &Context{middlewares: chain, index: -1}
	ctx.Next()

	want := []string{"g1", "g2", "r1", "handler"}
	if len(order) != len(want) {
		t.Fatalf("expected %v, got %v", want, order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("at %d expected %q, got %q (full: %v)", i, want[i], order[i], order)
		}
	}
}

// TestUseRebuildsExistingChains verifies that calling Use AFTER routes are
// registered still applies the new global middleware to those routes (the
// chain is rebuilt), so ordering guarantees hold regardless of call order.
func TestUseRebuildsExistingChains(t *testing.T) {
	r := NewRouter()
	r.autoServeRoot = false

	var order []string
	r.Handle(GET, "/y", func(*Context) error {
		order = append(order, "handler")
		return nil
	})
	// Global middleware added AFTER the route. It calls Next to advance to
	// the handler, exactly like a real middleware.
	r.Use(func(c *Context) error { order = append(order, "g-late"); return c.Next() })

	req := &HTTPRequest{Method: GET, Path: "/y"}
	chain, _ := r.findChain(req)
	if chain == nil {
		t.Fatal("expected a match for /y")
	}
	ctx := &Context{middlewares: chain, index: -1}
	ctx.Next()

	want := []string{"g-late", "handler"}
	if len(order) != len(want) || order[0] != want[0] || order[1] != want[1] {
		t.Fatalf("expected %v, got %v", want, order)
	}
}
