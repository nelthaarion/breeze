package breeze

import (
	"fmt"
	"runtime/debug"
	"sync/atomic"

	"github.com/panjf2000/gnet/v2"
)

type Breeze struct {
	*gnet.BuiltinEventEngine
	Router *Router
	Pool   *WorkerPool

	// listenPort is the port passed to Run, recorded so code that has the
	// application but not its bootstrap can learn the address it is answering
	// on. See ListenPort.
	listenPort atomic.Int64

	// inlineExec runs non-blocking routes directly on the gnet event-loop
	// goroutine instead of dispatching them to the worker pool. See
	// SetInlineExecution for the reasoning and the trade-off.
	inlineExec bool

	// zeroCopyHeaders parses headers in place in gnet's read buffer instead of
	// copying the header block. Off by default because it narrows the lifetime
	// of every string on the request. See SetZeroCopyHeaders.
	zeroCopyHeaders bool

	// WebSocket support — initialised lazily by WebSocket().
	wsHubFields

	// Auto-MCP support — the tagged-route endpoint's address, recorded by
	// EnableMCP. Declared in mcp_server.go, beside the code that uses it.
	mcpFields

	// ErrorHandler turns an error returned by a handler or middleware into a
	// response. Nil means the framework default; see error.go.
	//
	// Exported and settable rather than a constructor argument, because an
	// application usually decides its error format after it has routes worth
	// failing, and because a nil field is a legible "I have not chosen" — which
	// handleChainError treats as the default rather than as an absent response.
	ErrorHandler ErrorHandler
}

// compactThreshold: compact the leftover slice when the unused capacity
// exceeds this many bytes, to avoid keeping large receive buffers alive.
const compactThreshold = 512

// Precomputed error responses. Writing one costs a single append-free
// Conn.Write of a package-level slice — no formatting, no allocation.
//
// Conn.Write copies whatever it is given (it either completes the syscall or
// copies the remainder into the connection's outbound buffer), so handing it
// these shared slices is safe: gnet never retains or mutates them.
var (
	resp400 = []byte("HTTP/1.1 400 Bad Request\r\nContent-Length: 11\r\n\r\nBad Request")
	resp404 = []byte("HTTP/1.1 404 Not Found\r\nContent-Length: 9\r\n\r\nNot Found")
	resp500 = []byte(
		"HTTP/1.1 500 Internal Server Error\r\nContent-Length: 21\r\n\r\nInternal Server Error",
	)
)

// New creates a Breeze server with the given router and worker pool.
//
// The pool's OverflowPolicy MUST be OverflowSpawn (or OverflowReject) —
// NOT OverflowBlock — because OnTraffic calls pool.Submit from the gnet
// event-loop goroutine, and a blocking Submit would stall ALL connections
// on that reactor.
//
// Use breeze.NewEventLoopWorkerPool(n) to create a suitable pool. The
// deprecated breeze.NewWorkerPool(n) also works (it uses OverflowSpawn
// for backward compatibility).
func New(router *Router, pool *WorkerPool) *Breeze {
	s := &Breeze{
		BuiltinEventEngine: &gnet.BuiltinEventEngine{},
		Router:             router,
		Pool:               pool,
		inlineExec:         true,
	}
	// Publish the router, pool, Auto-MCP and WebSocket probes. Four registry
	// appends, once, at construction; see diag.go.
	s.registerCoreDiagnostics()
	return s
}

// SetInlineExecution enables or disables inline handler execution.
// It is enabled by default and must be called before Run.
//
// # What inline execution does
//
// With it enabled, a request whose route is not marked blocking runs its whole
// middleware chain on the gnet event-loop goroutine that read the bytes, and
// the response goes out through Conn.Write — a direct write syscall.
//
// With it disabled, every request is handed to the worker pool and answered
// with Conn.AsyncWrite, which queues the write and wakes the poller.
//
// # Why it is the default
//
// gnet already runs one event loop per core and pins each connection to one of
// them, so the parallelism is there before the pool is involved. Routing a
// request through the pool anyway adds, per request: a channel send and
// receive (one mutex, contended by every loop at once), two goroutine
// handoffs, and a poller wakeup for the write. For a handler that just
// serializes some JSON, that scheduling is most of the work being done.
//
// # When to disable it
//
// A handler that blocks — a database round trip, an outbound HTTP call, a file
// read, a lock held across a syscall — must not run on an event loop, because
// it stalls every connection pinned to that loop for the duration.
//
// Prefer marking those individual routes with Router.HandleBlocking, which
// keeps the fast path for everything else. Disable inline execution globally
// only when the whole application is I/O-bound and tagging routes one by one
// is not worth it.
func (s *Breeze) SetInlineExecution(enabled bool) {
	s.inlineExec = enabled
}

// SetZeroCopyHeaders enables parsing request headers in place, removing the
// last per-request allocation from the fast path. It is DISABLED by default and
// must be called before Run.
//
// # What it removes
//
// The parser normally copies a request's header block into memory the request
// owns, and points Method, Path, and every header key and value at that copy.
// That copy is one allocation per request — the only one a pipelined GET has
// left, now that the header keys are lowercased in place and the WebSocket
// probe is gated on a counter. Turning this on points those strings straight
// into gnet's read buffer instead, so answering a request allocates nothing at
// all and the garbage collector has no per-request work to do.
//
// That last part is the real win. A few hundred bytes per request is not much
// on its own, but at a million requests a second it is a continuous stream of
// short-lived garbage, and the mark work for it lands as GC assist on the very
// event-loop goroutines that are supposed to be serving connections. Removing
// the allocation removes the assist.
//
// # What it costs
//
// gnet's read buffer is reused for the next read on that connection. So with
// this enabled, every string on the request — Path included — is valid only
// until the handler returns.
//
// Breeze keeps that safe for the handler itself: a request that is dispatched
// to the worker pool, or whose route is marked blocking, is re-parsed into
// owned memory before it leaves the event-loop goroutine (see promoteRequest).
// Inside a handler, ctx.Req is always fully valid.
//
// What is NOT safe is keeping any of those strings after the handler returns:
//
//	var lastPath string
//	app.Handle(GET, "/x", func(c *breeze.Context) error {
//	    lastPath = c.Req.Path          // ✗ dangles once the handler returns
//	    lastPath = strings.Clone(c.Req.Path) // ✓
//	    return nil
//	})
//
// The failure mode is not a crash — the bytes stay mapped and readable — it is
// a string whose contents silently change into part of a later request. Stored
// as a map key, it corrupts the map. So the rule for anything that outlives the
// handler, including a package-level cache or a value handed to another
// goroutine, is strings.Clone.
//
// This applies to middleware too. Breeze's own middlewares are safe under this
// flag; third-party ones written against the default contract may not be.
//
// # When to turn it on
//
// Turn it on for a service whose handlers read the request, write a response,
// and keep nothing — which is most of them, and is where the throughput
// numbers are won. Leave it off if you cannot audit what your handlers and
// middlewares retain, because the default contract is the safe one.
func (s *Breeze) SetZeroCopyHeaders(enabled bool) {
	s.zeroCopyHeaders = enabled
}

// OnTraffic is called by gnet for every incoming data event.
//
// Routing strategy (zero-overhead fast path):
//  1. If any connection has been upgraded, check wsConns for this fd and hand
//     a promoted connection to handleWSTraffic (no HTTP parsing whatsoever).
//  2. Otherwise run the normal HTTP parse → route → dispatch pipeline.
//
// So WebSocket connections carry no HTTP overhead after the upgrade, and HTTP
// connections carry no WebSocket overhead at all — not even a map probe, until
// the first upgrade actually happens.
func (s *Breeze) OnTraffic(c gnet.Conn) gnet.Action {
	// ── WebSocket fast path ──────────────────────────────────────────────
	// Gated on the counter rather than going straight to the map. wsConns is a
	// sync.Map keyed by interface{}, so Load(fd) boxes the descriptor, and the
	// runtime only keeps preallocated boxes for 0-255 — above that it heap
	// allocates. That was one allocation per request, plus a hash probe, to
	// answer a question that is "no" for every request on an HTTP-only server.
	if s.wsCount.Load() != 0 {
		if state, ok := s.isWSConn(c.Fd()); ok {
			return s.handleWSTraffic(c, state)
		}
	}

	// ── HTTP path ────────────────────────────────────────────────────────
	data, _ := c.Next(-1)
	if len(data) == 0 {
		return gnet.None
	}

	// The leftover buffer between events lives in gnet's per-connection
	// context (c.Context/SetContext) rather than a global sync.Map. A gnet
	// connection is pinned to exactly one event-loop goroutine, so this
	// storage is accessed single-threaded — no locks, no map hashing, and no
	// sync.Map amortised-atomic overhead on the hot path.
	var existing []byte
	if v := c.Context(); v != nil {
		existing = v.([]byte)
	}

	// data is a view into gnet's internal inbound buffer, which gnet may
	// overwrite once OnTraffic returns.
	//
	// The previous version defended against that by copying every event into a
	// Go-owned slice up front — the whole request, headers and all, on every
	// single event. That copy is now gone. Instead:
	//
	//   - Header bytes are copied by the parser into req.owned, which is what
	//     req.Path and the req.Header strings point into (see request.go) —
	//     unless SetZeroCopyHeaders is on, in which case they are read in place
	//     and nothing is copied at all.
	//   - req.Body is copied only when there is a body AND the bytes are
	//     gnet's, keeping the guarantee in types.go that a handler may hold on
	//     to req.Body.
	//   - Leftover bytes are copied only when a partial request is pending.
	//
	// So a pipelined GET — the shape that decides a throughput benchmark —
	// copies its header block once and nothing else, or with zero-copy headers
	// enabled, copies nothing whatsoever. When leftover bytes already exist we
	// must concatenate anyway, and the result is Go-owned.
	buf := data
	goOwned := false
	if len(existing) > 0 {
		buf = append(existing, data...)
		goOwned = true
	}

	// zeroCopy decides whether this event's requests get their strings pointed
	// straight into buf, or into a per-request copy of the header block. See
	// SetZeroCopyHeaders for the contract and fillHTTPRequest for the mechanics.
	//
	// It is only worth asking for when the fast path can actually keep the
	// bytes alive long enough to use them:
	//
	//   - With inline execution off, every request goes to the worker pool and
	//     would be promoted straight back into owned memory, so parsing owned
	//     once is strictly cheaper than parsing twice.
	//   - goOwned buffers are exempt from that. Those bytes are ours, and the
	//     next event appends past them rather than over them, so views into
	//     them survive the handoff to a worker with nothing to promote.
	zeroCopy := s.zeroCopyHeaders && (s.inlineExec || goOwned)

	for len(buf) > 0 {
		req, consumed, err := parsePooledRequest(buf, !zeroCopy)
		if err != nil {
			c.Write(resp400)
			buf = nil
			break
		}
		if req == nil {
			break // incomplete — wait for more data
		}

		chain, params, blocking := s.Router.findDispatch(req)

		if chain == nil {
			releaseRequest(req)
			if params != nil {
				releaseParams(params)
			}
			c.Write(resp404)
		} else {
			// ── Promotion ────────────────────────────────────────────────
			// A zero-copy request's strings are views into gnet's read
			// buffer, so they are valid exactly as long as this event is.
			// An inline request finishes inside this iteration, so it never
			// outlives them. A blocking one does: gnet reads over those
			// bytes as soon as OnTraffic returns, while the worker is still
			// holding the request. Give it owned memory first.
			//
			// This is the whole point of parsing zero-copy by default and
			// promoting on demand — the requests that pay for isolation are
			// the ones already committed to disk or network I/O, where a
			// second pass over bytes still in L1 does not register.
			if zeroCopy && !goOwned && blocking {
				// Route params are substrings of req.Path, so they point
				// into the old bytes too. They have to be re-derived rather
				// than carried over; the route itself cannot have changed,
				// since the method and path are the same bytes as before.
				if params != nil {
					releaseParams(params)
				}
				if err := promoteRequest(req, buf); err != nil {
					// Unreachable: the same bytes parsed once already.
					releaseRequest(req)
					c.Write(resp400)
					buf = nil
					break
				}
				chain, params, _ = s.Router.findDispatch(req)
			}

			// req.Body aliases gnet's buffer unless buf is already Go-owned.
			// Copy it so the documented contract holds: a handler may keep
			// req.Body for as long as it likes, on any goroutine.
			//
			// After promotion, not before — promoteRequest re-derives Body
			// from buf and would throw the copy away.
			if !goOwned && len(req.Body) > 0 {
				body := make([]byte, len(req.Body))
				copy(body, req.Body)
				req.Body = body
			}

			// chain is the route's PRECOMPUTED [global..., route..., handler]
			// slice (built at registration). It is read-only at request time —
			// Context.Next mutates only ctx.index — so we assign it directly
			// with ZERO per-request allocation.
			ctx := acquireContext()
			ctx.Conn = c
			ctx.Req = req
			ctx.reqPooled = true
			ctx.params = params
			ctx.middlewares = chain
			ctx.index = -1

			if s.inlineExec && !blocking {
				// Run on this event-loop goroutine and answer with a direct
				// write. No channel, no handoff, no poller wakeup.
				s.runInline(c, ctx)
			} else {
				s.dispatch(c, ctx)
			}
		}

		if consumed >= len(buf) {
			buf = nil
			break
		}
		buf = buf[consumed:]
	}

	// Store leftover bytes (a partial next request) in the connection's own
	// gnet context. Clearing it to nil when empty lets the GC reclaim the
	// backing array and keeps the fast-path Load above allocation-free.
	if len(buf) == 0 {
		c.SetContext(nil)
		return gnet.None
	}

	// The leftover has to outlive this call, so it must be Go memory — either
	// because it already is, or by copying it out of gnet's buffer now.
	if !goOwned {
		keep := make([]byte, len(buf))
		copy(keep, buf)
		buf = keep
	} else if cap(buf)-len(buf) > compactThreshold {
		compact := make([]byte, len(buf))
		copy(compact, buf)
		buf = compact
	}
	c.SetContext(buf)

	return gnet.None
}

// runInline executes ctx's chain on the calling event-loop goroutine and
// writes the response synchronously.
//
// The wire bytes come from a pooled buffer, which is only sound here: gnet's
// Conn.Write has finished with the slice by the time it returns, either having
// written it or having copied the remainder into the connection's outbound
// buffer. AsyncWrite makes no such promise, so the pooled path below uses
// freshly allocated bytes instead.
func (s *Breeze) runInline(c gnet.Conn, ctx *Context) {
	// Registered first so it runs last — after the recover below has had its
	// chance to write a response from ctx.Res.
	defer releaseContext(ctx)
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("[Breeze][PANIC] %v\n%s\n", r, debug.Stack())
			c.Write(resp500)
		}
	}()

	if err := ctx.Next(); err != nil {
		// The error becomes a response here rather than being logged and dropped.
		// handleChainError always leaves ctx.Res non-nil, so the write below sends
		// it — which is the whole contract Part 1's error return depends on.
		s.handleChainError(ctx, err)
	}

	if ctx.Res != nil {
		bp := acquireWireBuf()
		*bp = ctx.Res.AppendTo(*bp)
		_, _ = c.Write(*bp)
		releaseWireBuf(bp)
	}
}

// dispatch hands ctx's chain to the worker pool (or a bare goroutine when no
// pool is configured) and answers with AsyncWrite, which is the only safe way
// to write to a connection from off its event loop.
func (s *Breeze) dispatch(c gnet.Conn, ctx *Context) {
	exec := func() {
		// Release defer is registered FIRST so it runs LAST (after the
		// recover defer). This ensures the response is fully written before
		// the Context is returned to the pool.
		defer releaseContext(ctx)
		// Recover from panics in handlers so a buggy handler does not crash
		// the worker goroutine.
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("[Breeze][PANIC] %v\n%s\n", r, debug.Stack())
				_ = c.AsyncWrite(resp500, nil)
			}
		}()
		if err := ctx.Next(); err != nil {
			// Same contract as runInline: the error resolves to a response before
			// the write below, so it cannot be lost on the worker path either.
			s.handleChainError(ctx, err)
		}
		if ctx.Res != nil {
			// Bytes allocates: AsyncWrite returns before the write happens
			// and keeps the slice until the poller drains it, so this one
			// must not come from wireBufPool.
			_ = c.AsyncWrite(ctx.Res.Bytes(), nil)
		}
	}

	if s.Pool != nil {
		s.Pool.Submit(exec)
	} else {
		go exec()
	}
}

// OnClose cleans up all per-connection state when a connection closes.
// For WebSocket connections that closed unexpectedly (no Close frame received),
// we still call OnClose so the application can clean up its own state.
func (s *Breeze) OnClose(c gnet.Conn, err error) gnet.Action {
	fd := c.Fd()

	// The HTTP reassembly leftover now lives in the connection's own gnet
	// context, which gnet discards when the connection is torn down — so
	// there is no global map entry to clean up here anymore.

	// WebSocket cleanup on unexpected close (e.g. TCP RST, network drop).
	if state, ok := s.isWSConn(fd); ok {
		s.cleanupWS(fd, state.wc, state.handler, 1006, "abnormal closure")
	}

	return gnet.None
}

func (s *Breeze) Run(port int, multiCore bool) error {
	// Recorded before the bind, so anything reading it during startup sees the
	// port this call is for rather than zero. A failed bind leaves it set, which
	// is harmless: Run returns the error and the process does not go on to serve.
	s.listenPort.Store(int64(port))
	return gnet.Run(
		s,
		fmt.Sprintf("tcp://:%d", port),
		gnet.WithTCPNoDelay(gnet.TCPNoDelay),
		gnet.WithMulticore(multiCore),
		gnet.WithLoadBalancing(gnet.RoundRobin),
		// One reusable read buffer per event loop, sized so a pipelined batch
		// of small requests arrives in a single read. The default is 64 KiB;
		// this is per loop, not per connection, so it is cheap.
		gnet.WithReadBufferCap(64<<10),
		// Cap how much unflushed response data gnet stacks in a connection's
		// ring buffer before spilling to its linked-list buffer.
		gnet.WithWriteBufferCap(64<<10),
	)
}

// ListenPort reports the port passed to Run, or 0 before Run is called.
//
// This exists because a subsystem can be handed the application without being
// handed its bootstrap. The dashboard's API Explorer is the case that needed it:
// it makes a request back into the service it is installed in, and pinning that
// request to the real listener is what stops the endpoint from being a
// server-side request forgery primitive. Deriving the port from the request's
// Host header instead would mean a caller-supplied header decided where the
// server connected — the same hole in a different field.
//
// Atomic because Run is usually the main goroutine while readers are handlers on
// event-loop or worker goroutines.
func (s *Breeze) ListenPort() int {
	return int(s.listenPort.Load())
}
