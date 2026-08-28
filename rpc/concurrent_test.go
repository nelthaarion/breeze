package rpc

import (
	"strconv"
	"sync"
	"testing"
)

// concurrent_test.go — shared state under -race.
//
// The rest of the suite drives one goroutine at a time, which means the race
// detector never has anything to find there. These tests exist to give it
// something: the pooled write buffers, the registry's method table and the
// context pool are all shared across event loops in production, and a real
// server runs several. A data race in any of them would otherwise surface only
// under load, as a torn response or a lost method.

// TestConcurrentHandle drives one server from many goroutines.
//
// Handle is the transport-independent entry point, so this is the path a caller
// bridging JSON-RPC over another transport hits — and unlike OnTraffic, nothing
// pins it to a single goroutine, so it must be safe for concurrent use.
func TestConcurrentHandle(t *testing.T) {
	s := testServer(t)

	const goroutines = 16
	const perGoroutine = 200

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			id := strconv.Itoa(g)
			req := []byte(`{"jsonrpc":"2.0","method":"sum","params":[1,2,3],"id":` + id + `}`)

			for i := 0; i < perGoroutine; i++ {
				out := s.Handle(req)
				if len(out) == 0 {
					t.Errorf("goroutine %d: no response", g)
					return
				}
				// Assert the id came back correct, not merely that bytes came
				// back. A buffer shared across goroutines would produce a
				// well-formed response carrying another goroutine's id, which a
				// length check would happily accept.
				if !containsID(out, id) {
					t.Errorf("goroutine %d: response carried the wrong id: %s", g, out)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}

// TestConcurrentConnections drives separate connections in parallel.
//
// Each connection is serviced by one goroutine, which is gnet's actual model —
// a connection is pinned to an event loop. What is shared is everything behind
// it: the wire-buffer pool, the context pool and the registry.
func TestConcurrentConnections(t *testing.T) {
	s := testServer(t)

	const conns = 16
	const perConn = 100

	var wg sync.WaitGroup
	for n := 0; n < conns; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			c := &fakeConn{}
			id := strconv.Itoa(n)

			for i := 0; i < perConn; i++ {
				feed(s, c, `{"jsonrpc":"2.0","method":"sum","params":[1,2,3],"id":`+id+`}`)
			}

			got := decodeStream(t, c.written.Bytes())
			if len(got) != perConn {
				t.Errorf("connection %d: got %d responses, want %d", n, len(got), perConn)
				return
			}
			// Every response on this connection must carry this connection's id.
			// A shared buffer leaking across connections shows up here as
			// another connection's id in this one's stream.
			want := float64(n)
			for j, m := range got {
				if m["id"] != want {
					t.Errorf("connection %d response %d: id = %v, want %v", n, j, m["id"], want)
					return
				}
			}
		}(n)
	}
	wg.Wait()
}

// TestConcurrentRegistrationAndDispatch registers methods while dispatching.
//
// The registry is guarded by an RWMutex because this is legal: a server may add
// a method while serving traffic. The chain rebuild in Use and the map write in
// Register both mutate state the dispatch path is reading.
func TestConcurrentRegistrationAndDispatch(t *testing.T) {
	reg := NewRegistry()
	reg.Register("stable", func(ctx *Context) { ctx.Result(true) })
	s := NewServer(reg)

	var wg sync.WaitGroup

	// Readers: dispatch against a method that is always present.
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := []byte(`{"jsonrpc":"2.0","method":"stable","id":1}`)
			for i := 0; i < 300; i++ {
				if out := s.Handle(req); len(out) == 0 {
					t.Error("no response for a registered method")
					return
				}
			}
		}()
	}

	// Writers: keep adding methods.
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				name := "dynamic_" + strconv.Itoa(g) + "_" + strconv.Itoa(i)
				reg.Register(name, func(ctx *Context) { ctx.Result(name) })
			}
		}(g)
	}

	// A writer that rebuilds every chain, which is the most invasive mutation
	// the registry supports.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			reg.Use(func(ctx *Context) { ctx.Next() })
		}
	}()

	wg.Wait()
}

// TestConcurrentMethodsIntrospection reads the method list while it is mutated.
//
// Methods() returns a fresh slice for exactly this reason: handing out the
// internal one would let a caller read it while Register appended to it.
func TestConcurrentMethodsIntrospection(t *testing.T) {
	reg := NewRegistry()
	reg.Register("seed", func(ctx *Context) {})

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = reg.Methods()
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			reg.Register("m_"+strconv.Itoa(i), func(ctx *Context) {})
		}
	}()

	wg.Wait()
}

// TestConcurrentBlockingHandoff drives the worker path from several
// connections, so the AsyncWrite branch is exercised concurrently.
func TestConcurrentBlockingHandoff(t *testing.T) {
	reg := NewRegistry()
	reg.RegisterBlocking("slow", func(ctx *Context) {
		ctx.Result("ok")
	})
	s := NewServer(reg)

	var wg sync.WaitGroup
	for n := 0; n < 8; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := &fakeConn{}
			for i := 0; i < 50; i++ {
				feed(s, c, `{"jsonrpc":"2.0","method":"slow","id":1}`)
			}
		}()
	}
	wg.Wait()
}

// containsID reports whether the response carries "id":<id>.
func containsID(resp []byte, id string) bool {
	needle := `"id":` + id
	for i := 0; i+len(needle) <= len(resp); i++ {
		if string(resp[i:i+len(needle)]) == needle {
			// Guard against a prefix match: "id":1 must not satisfy a search
			// for an id of 1 when the actual id is 12.
			end := i + len(needle)
			if end == len(resp) || resp[end] == '}' || resp[end] == ',' {
				return true
			}
		}
	}
	return false
}
