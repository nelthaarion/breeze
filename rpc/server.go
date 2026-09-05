package rpc

import (
	"fmt"
	"sync/atomic"

	"github.com/panjf2000/gnet/v2"
)

// server.go — the gnet event handler.
//
// This is the layer that makes the package a peer of the HTTP implementation
// rather than a consumer of it. Breeze.OnTraffic reads from gnet, frames HTTP
// requests out of the buffer, dispatches, and writes back; Server.OnTraffic does
// the same for JSON-RPC. Neither one knows about the other, and neither goes
// through net/http.

// defaultMaxMessageBytes bounds how large a single message may become while it
// is being reassembled.
//
// Without a bound, a client can send `[` and stop, and the server will hold the
// partial buffer for the life of the connection. With N connections doing that,
// the memory is the attacker's to choose. 4 MiB matches the WebSocket layer's
// default payload cap in the root package.
const defaultMaxMessageBytes = 4 << 20

// compactThreshold: compact the pending slice when unused capacity exceeds this
// many bytes, so a connection that once received a large batch does not keep the
// buffer for it alive. Matches the root package's threshold.
const compactThreshold = 512

// connState is the per-connection reassembly state.
//
// It lives in gnet's per-connection context (Conn.Context/SetContext) rather
// than in a server-wide map. A gnet connection is pinned to one event-loop
// goroutine, so this is single-threaded storage: no lock, no map hash, and none
// of the interface boxing that made the root package gate its sync.Map lookup
// behind a counter.
type connState struct {
	// pending holds bytes that did not form a complete message. It is Go-owned
	// memory, because it must outlive the read that produced it.
	pending []byte
	// scan is the framer's carry-over, so bytes already classified are not
	// rescanned when the rest of the message arrives.
	scan scanState
}

// Server is a JSON-RPC 2.0 server on gnet's event loop.
//
// It embeds gnet.BuiltinEventEngine and overrides OnTraffic and OnClose, the
// same two callbacks Breeze overrides. Everything else keeps the builtin
// no-op behaviour.
type Server struct {
	*gnet.BuiltinEventEngine

	reg  *Registry
	pool Pool

	// blockingCount is the number of registered blocking methods, maintained so
	// the hot path can skip the pre-scan entirely when there are none. On a
	// server whose methods are all inline — the case the fast path exists for —
	// this is an atomic load instead of a JSON decode per message.
	blockingCount atomic.Int64

	maxMessageBytes int
}

// Pool is the subset of breeze.WorkerPool this package needs.
//
// Taking an interface rather than the concrete type keeps this package from
// importing the root package, which would be an import cycle waiting to happen
// the moment the root package wants to expose an RPC server. *breeze.WorkerPool
// satisfies it as written, so the caller passes one directly.
type Pool interface {
	Submit(func())
}

// NewServer returns a Server serving reg.
//
// Registering methods after the server starts is supported (see Registry), but
// the blocking-method count is refreshed by SetRegistry and by RefreshBlocking
// rather than watched continuously, so a blocking method registered after Run
// requires a RefreshBlocking call to be dispatched off the event loop.
func NewServer(reg *Registry) *Server {
	if reg == nil {
		reg = NewRegistry()
	}
	s := &Server{
		BuiltinEventEngine: &gnet.BuiltinEventEngine{},
		reg:                reg,
		maxMessageBytes:    defaultMaxMessageBytes,
	}
	s.RefreshBlocking()
	s.registerDiagnostics()
	return s
}

// SetPool gives the server a worker pool for blocking methods.
//
// The pool MUST NOT use OverflowBlock: Submit is called from the event-loop
// goroutine, and a blocking Submit stalls every connection on that reactor.
// breeze.NewEventLoopWorkerPool(n) builds a suitable one. Without a pool,
// blocking methods run on a fresh goroutine each — off the event loop, which is
// the part that matters, but with no bound on concurrency.
func (s *Server) SetPool(p Pool) {
	s.pool = p
}

// SetMaxMessageBytes caps the size of a single reassembled message. A value of
// zero or less restores the default.
func (s *Server) SetMaxMessageBytes(n int) {
	if n <= 0 {
		n = defaultMaxMessageBytes
	}
	s.maxMessageBytes = n
}

// Registry returns the server's registry, so methods can be registered through
// the server when that reads better at the call site.
func (s *Server) Registry() *Registry { return s.reg }

// Register registers an inline method, forwarding to the registry.
//
// It exists so the common case is one call on one object, the way
// app.Router.Handle is reached through the app in the root package.
func (s *Server) Register(name string, handler HandlerFunc, middlewares ...HandlerFunc) {
	s.reg.Register(name, handler, middlewares...)
}

// RegisterBlocking registers a blocking method and refreshes the fast-path
// counter, so a method registered through the server is dispatched correctly
// without a separate RefreshBlocking call.
func (s *Server) RegisterBlocking(name string, handler HandlerFunc, middlewares ...HandlerFunc) {
	s.reg.RegisterBlocking(name, handler, middlewares...)
	s.RefreshBlocking()
}

// RefreshBlocking recounts the registry's blocking methods.
//
// Call it after registering a blocking method directly on a Registry that a
// running server is already serving.
func (s *Server) RefreshBlocking() {
	var n int64
	s.reg.mu.RLock()
	for _, m := range s.reg.methods {
		if m.blocking {
			n++
		}
	}
	s.reg.mu.RUnlock()
	s.blockingCount.Store(n)
}

// OnTraffic is called by gnet for every read event.
//
// The shape mirrors Breeze.OnTraffic: take the bytes, prepend anything left over
// from the previous event, frame out as many complete messages as are present,
// and stash the remainder in the connection's own context.
func (s *Server) OnTraffic(c gnet.Conn) gnet.Action {
	data, _ := c.Next(-1)
	if len(data) == 0 {
		return gnet.None
	}

	st, _ := c.Context().(*connState)

	// buf is a view into gnet's inbound buffer, which gnet may overwrite once
	// OnTraffic returns. Nothing derived from it is retained past this call
	// except by an explicit copy: a partial message is copied into st.pending
	// below, and a message headed for a worker is copied by handoff.
	//
	// When bytes are already pending they have to be concatenated, and the
	// result is Go-owned.
	buf := data
	goOwned := false
	if st != nil && len(st.pending) > 0 {
		buf = append(st.pending, data...)
		goOwned = true
	}

	var scan scanState
	if st != nil {
		scan = st.scan
	}

	// Responses for every message in this event accumulate in one pooled buffer
	// and go out in a single Conn.Write. A client that pipelines ten calls gets
	// one syscall, not ten.
	bp := acquireWireBuf()
	out := *bp
	defer func() {
		*bp = out
		releaseWireBuf(bp)
	}()

	consumed := 0
	action := gnet.None

	for {
		start, end, res := nextValue(buf[consumed:], &scan)
		if res == scanInvalid {
			// The stream is not JSON and there is no defined point to resume
			// from. Answer with a parse error and close, rather than spinning
			// on bytes that will never frame.
			out = appendErrorResponse(out, ErrParseError(), nullID)
			consumed = len(buf)
			scan.reset()
			action = gnet.Close
			break
		}
		if res == scanIncomplete {
			break
		}

		msg := buf[consumed+start : consumed+end]
		consumed += end
		scan.reset()

		if s.blockingCount.Load() != 0 && s.messageNeedsWorker(msg) {
			// Hand this message to a worker. Its bytes may be a view into
			// gnet's buffer, which is invalid the moment OnTraffic returns, so
			// the copy is mandatory rather than defensive.
			owned := make([]byte, len(msg))
			copy(owned, msg)
			s.handoff(c, owned)
			continue
		}

		out = s.appendMessage(out, msg, c)
	}

	if len(out) > 0 {
		// Conn.Write has finished with the slice by the time it returns, which
		// is what licenses the pooled buffer here. See wireBufPool.
		_, _ = c.Write(out)
		out = out[:0]
	}

	if action == gnet.Close {
		c.SetContext(nil)
		return action
	}

	return s.saveRemainder(c, st, buf, consumed, goOwned, scan)
}

// saveRemainder stores whatever did not frame into the connection's context.
func (s *Server) saveRemainder(
	c gnet.Conn,
	st *connState,
	buf []byte,
	consumed int,
	goOwned bool,
	scan scanState,
) gnet.Action {
	rest := buf[consumed:]

	// A buffer holding only the whitespace between messages is not a partial
	// message. Dropping it keeps a client that heartbeats with newlines from
	// growing the pending buffer until the size guard closes the connection.
	if scan.pendingWhitespaceOnly(len(rest)) {
		rest = nil
		scan.reset()
	}

	if len(rest) == 0 {
		if st != nil {
			// Clearing the context lets the GC reclaim the pending array and
			// keeps the next event's type assertion on the nil fast path.
			c.SetContext(nil)
		}
		return gnet.None
	}

	if len(rest) > s.maxMessageBytes {
		// One message has grown past the cap without completing. There is no
		// way to skip past it — the framer cannot know where it ends — so the
		// connection goes.
		fmt.Printf("[Breeze][RPC] message exceeds %d bytes, closing connection\n", s.maxMessageBytes)
		c.SetContext(nil)
		_, _ = c.Write(appendErrorResponse(nil, NewError(CodeInvalidRequest, "request too large"), nullID))
		return gnet.Close
	}

	if st == nil {
		st = &connState{}
	}

	if !goOwned {
		// The remainder is a view into gnet's buffer and must be copied to
		// survive this call.
		keep := make([]byte, len(rest))
		copy(keep, rest)
		st.pending = keep
	} else if cap(rest)-len(rest) > compactThreshold {
		compact := make([]byte, len(rest))
		copy(compact, rest)
		st.pending = compact
	} else {
		st.pending = rest
	}

	// The scan offsets were relative to the consumed prefix, which is no longer
	// part of pending. Rebasing keeps them pointing at the same bytes.
	scan.scanned -= consumed
	if scan.scanned < 0 {
		scan.scanned = 0
	}
	scan.valueStart -= consumed
	if scan.valueStart < 0 {
		scan.valueStart = 0
	}
	st.scan = scan

	c.SetContext(st)
	return gnet.None
}

// handoff runs a message off the event loop and answers with AsyncWrite.
//
// The bytes are freshly allocated by the response path here, not pooled:
// AsyncWrite returns before the write happens and keeps the slice until the
// poller drains it. This is the same split as Breeze.dispatch.
func (s *Server) handoff(c gnet.Conn, msg []byte) {
	exec := func() {
		defer func() {
			if r := recover(); r != nil {
				// appendMessage already converts a handler panic into -32603,
				// so reaching here means the failure was in the dispatch or
				// serialization path itself. The connection still gets an
				// answer rather than a silent hang.
				fmt.Printf("[Breeze][RPC][PANIC] %v\n", r)
				_ = c.AsyncWrite(appendErrorResponse(nil, ErrInternalError(), nullID), nil)
			}
		}()
		if out := s.appendMessage(nil, msg, c); len(out) > 0 {
			_ = c.AsyncWrite(out, nil)
		}

	}

	if s.pool != nil {
		s.pool.Submit(exec)
	} else {
		go exec()
	}
}

// OnClose releases per-connection state.
//
// The pending buffer lives in gnet's connection context, which gnet discards
// with the connection, so there is nothing to unregister from a server-wide map
// — the reason that state was put there in the first place.
func (s *Server) OnClose(c gnet.Conn, err error) gnet.Action {
	c.SetContext(nil)
	return gnet.None
}

// Run starts the server on the given TCP port.
//
// The gnet options match Breeze.Run: one read buffer per event loop sized for a
// pipelined batch, a matching write buffer cap, TCP_NODELAY on because a
// request-response protocol must not wait for Nagle, and round-robin load
// balancing.
func (s *Server) Run(port int, multiCore bool) error {
	return gnet.Run(
		s,
		fmt.Sprintf("tcp://:%d", port),
		gnet.WithTCPNoDelay(gnet.TCPNoDelay),
		gnet.WithMulticore(multiCore),
		gnet.WithLoadBalancing(gnet.RoundRobin),
		gnet.WithReadBufferCap(64<<10),
		gnet.WithWriteBufferCap(64<<10),
	)
}

// RunAddr starts the server on an explicit gnet address, for callers that need
// a bind host, a Unix socket, or a port chosen by the OS.
func (s *Server) RunAddr(addr string, multiCore bool) error {
	return gnet.Run(
		s,
		addr,
		gnet.WithTCPNoDelay(gnet.TCPNoDelay),
		gnet.WithMulticore(multiCore),
		gnet.WithLoadBalancing(gnet.RoundRobin),
		gnet.WithReadBufferCap(64<<10),
		gnet.WithWriteBufferCap(64<<10),
	)
}

// Handle dispatches one JSON-RPC message and returns the raw response bytes.
//
// It is the transport-independent entry point: no connection, no framing, just
// a message in and a response out. A notification returns nil, matching the
// wire behaviour of sending nothing.
//
// This is what a test asserts against, and what a caller bridging JSON-RPC over
// some other transport — a WebSocket frame, a queue message, an HTTP body they
// are already holding — would call. Because it takes bytes rather than a
// connection, it does not drag the event loop into any of those paths.
//
// The returned slice is freshly allocated and owned by the caller.
func (s *Server) Handle(msg []byte) []byte {
	out := s.appendMessage(nil, msg, nil)

	if len(out) == 0 {
		return nil
	}
	return out
}
