package breeze

import (
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"

	"github.com/nelthaarion/breeze/v2/diag"
	"github.com/panjf2000/gnet/v2"
)

// ─── Per-connection WebSocket state ──────────────────────────────────────────

// wsConnState holds all WebSocket-specific state for a single connection.
// It is stored in Breeze.wsConns (keyed by fd) and looked up on every
// OnTraffic call for a connection that has been promoted. HTTP traffic skips
// the lookup entirely — see wsCount.
type wsConnState struct {
	wc      *WSConn
	handler WSHandler
	// maxPayload is per-connection so different routes can set different limits.
	maxPayload int

	// dispatch delivers to the handler in arrival order. See wsDispatchQueue.
	dispatch *wsDispatchQueue
}

// ─── Ordered per-connection dispatch ─────────────────────────────────────────

// wsEvent is one thing to hand a handler: a complete message, or the close.
//
// Close travels the same queue as messages rather than being submitted separately,
// because the ordering guarantee is worthless if OnClose can overtake the last
// OnMessage — an application that flushes per-connection state in OnClose would
// then be handed a message after it had already torn that state down.
type wsEvent struct {
	isClose bool

	opcode  byte
	payload []byte

	code   uint16
	reason string
}

// wsDispatchQueue delivers a connection's events to its handler strictly in the
// order they arrived.
//
// # The bug this exists to fix
//
// dispatchMessage used to Submit each message to the worker pool as its own task.
// The pool has many workers, so two messages from one connection could be picked up
// by two workers and run in either order — and did: ten sequentially numbered
// messages sent back-to-back arrived as [0 4 1 2 3 5 8 6 7 9] on one run and in
// order on the next. It also made the server the only asymmetric half of Breeze's
// WebSocket support, since websocket_client.go's read pump has always delivered in
// order, and the gnet transport's per-connection loop does too.
//
// # Why this preserves order, and why a mutex would not
//
// A per-connection mutex held across the handler call prevents two messages running
// at once and does nothing about order: with both already queued on different pool
// workers, whichever reaches the lock first wins, and that is a scheduling
// coincidence.
//
// This is a single-consumer queue instead. running is true for exactly as long as
// one drain task exists for this connection, so at most one consumer is ever
// popping, and it pops from the front. Two properties make it airtight:
//
//   - No two drains overlap. running is set under the lock by whichever push
//     found it false, and cleared only by a drain that has found the queue empty,
//     also under the lock.
//   - No wakeup is lost. push appends under the lock. A push that sees
//     running == true is guaranteed to be seen, because the live drain re-acquires
//     the lock before every pop and only exits after observing an empty queue
//     while holding it — so an append that happened before that observation was
//     already drained, and one that happens after finds running == false and
//     starts a new drain.
//
// A drain occupies a pool worker only while it has something to deliver, which is
// why this is a queue plus a flag rather than a goroutine per connection: a server
// with ten thousand idle WebSocket connections keeps no goroutines for them.
type wsDispatchQueue struct {
	wc      *WSConn
	handler WSHandler
	pool    *WorkerPool

	mu    sync.Mutex
	queue []wsEvent

	// running means one drain task exists for this connection.
	running bool

	// sealed means the close event has been queued. Nothing follows a close, so
	// a late message is dropped rather than delivered after the handler has been
	// told the connection is gone.
	sealed bool
}

// wsDispatchQueueDepth is how many undelivered events one connection may hold.
//
// Bounded because it is a buffer a peer fills and a handler drains: unbounded, a
// peer that sends faster than the handler consumes grows it until the process dies.
// The real memory bound is this times maxPayload, so the number is deliberately
// modest — a handler that has fallen 256 messages behind is not going to catch up,
// and the connection is closed instead of being allowed to consume the server.
const wsDispatchQueueDepth = 256

// push appends an event and starts a drain if none is running.
//
// It reports false when the queue is full, which the caller treats as grounds to
// close the connection. Called from the event loop, so it never blocks: applying
// backpressure by waiting here would stall every other connection on that loop.
func (q *wsDispatchQueue) push(ev wsEvent) bool {
	q.mu.Lock()
	if q.sealed {
		q.mu.Unlock()
		return true // already closed; the event is not an error, just moot
	}
	if ev.isClose {
		q.sealed = true
	} else if len(q.queue) >= wsDispatchQueueDepth {
		q.mu.Unlock()
		return false
	}
	q.queue = append(q.queue, ev)

	start := !q.running
	if start {
		q.running = true
	}
	q.mu.Unlock()

	if start {
		if q.pool != nil {
			q.pool.Submit(q.drain)
		} else {
			go q.drain()
		}
	}
	return true
}

// drain delivers events until the queue is empty.
//
// The handler runs outside the lock: it is application code, it may call back into
// this connection, and holding the lock across it would deadlock a handler that
// sends and then closes.
func (q *wsDispatchQueue) drain() {
	for {
		q.mu.Lock()
		if len(q.queue) == 0 {
			q.running = false
			q.mu.Unlock()
			return
		}
		ev := q.queue[0]
		// The slot is cleared so the event's payload is not kept alive by the
		// backing array after delivery.
		q.queue[0] = wsEvent{}
		q.queue = q.queue[1:]
		q.mu.Unlock()

		q.call(ev)

		if ev.isClose {
			// Nothing is queued after a close, so there is nothing left to drain.
			// running stays true, which is correct: it stops a late push from
			// starting a second drain.
			return
		}
	}
}

// call invokes the handler for one event, containing a panic.
//
// This has to be here rather than relying on WorkerPool.runTask's own recover.
// Under the old design each message was its own pool task, so a panicking handler
// killed that task and nothing else. Now one drain delivers many messages, and a
// panic escaping it would unwind past the loop with running still true — leaving
// the connection permanently undrained, every later message queued behind a
// consumer that no longer exists. That is a worse failure than the panic.
//
// The message matches WorkerPool's own, since this is the same class of event and
// an operator grepping for it should find both.
func (q *wsDispatchQueue) call(ev wsEvent) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("[Breeze][WebSocket][PANIC] %v\n%s\n", r, debug.Stack())
		}
	}()

	if ev.isClose {
		q.handler.OnClose(q.wc, ev.code, ev.reason)
		return
	}
	q.handler.OnMessage(q.wc, ev.opcode, ev.payload)
}

// wsMaxPayloadDefault is 4 MiB — enough for most real-world messages while
// guarding against memory exhaustion attacks.
const wsMaxPayloadDefault = 4 << 20

// WebSocket close status codes (RFC 6455 §7.4.1)
//
// These codes are sent in Close frames to indicate the reason for closing
// the connection. Applications may use these codes or define custom ones
// in the 3000-3999 and 4000-4999 ranges.
const (
	WsCloseNormalClosure           = 1000 // Normal closure; normal shutdown
	WsCloseGoingAway               = 1001 // Endpoint is going away (browser tab closed, etc.)
	WsCloseProtocolError           = 1002 // Protocol error; endpoint terminated due to error
	WsCloseUnsupportedData         = 1003 // Received data of unsupported type
	WsCloseNoStatusRcvd            = 1005 // Reserved; must not be set in Close frame
	WsCloseAbnormalClosure         = 1006 // Reserved; abnormal closure (no Close frame received)
	WsCloseInvalidFramePayloadData = 1007 // Invalid frame payload data (e.g., invalid UTF-8)
	WsClosePolicyViolation         = 1008 // Policy violation (e.g., authorization failure)
	WsCloseMessageTooBig           = 1009 // Message too large for endpoint to process
)

// ─── Breeze extension ────────────────────────────────────────────────────────

// wsConns maps fd → *wsConnState for every active WebSocket connection.
// We use a separate sync.Map (not the HTTP bufs map) so the two code paths
// never interfere and the WebSocket fast path avoids touching HTTP state.
//
// WSHub is the shared hub exposed via s.WSHub for broadcast operations.
//
// wsHandlers maps a route pattern (e.g. "/ws") to the WSHandler registered
// via s.WebSocket(). Looked up once per upgrade to avoid repeated router calls.

// initWS lazily initialises WebSocket fields on the Breeze engine.
// Called by WebSocket() before the server starts, not on the hot path.
func (s *Breeze) initWS() {
	if s.wsHub == nil {
		s.wsHub = newWSHub(s.Pool)
		// The hub only exists once a WebSocket endpoint has been declared, so
		// this is the first moment there is anything to report.
		diag.Register(diagWebSocket, s.webSocketProbe)
	}
	if s.wsHandlers == nil {
		s.wsHandlers = make(map[string]WSHandler)
	}
}

// WebSocket registers a WebSocket endpoint at the given path and returns
// the shared WSHub, which is created on the first call and reused for all
// subsequent WebSocket routes.
func (s *Breeze) WebSocket(path string, handler WSHandler) *WSHub {
	s.initWS()
	s.wsHandlers[path] = handler
	// upgradeHandler reads from s.wsHandlers at call time, so updating the
	// map above is sufficient for re-registrations on the same path.
	// We always append to the router; Find() returns the first match, so only
	// the first registration is actually reachable unless paths differ.
	//
	// Registered as BLOCKING so the handshake never runs on a gnet event-loop
	// goroutine. The handler calls handler.OnConnect(wc) — arbitrary
	// application code that may open a database connection, take a lock, or
	// otherwise block. Running that inline would stall every connection pinned
	// to that reactor for its duration. Upgrades happen once per connection, so
	// the pool hop costs nothing measurable.
	s.Router.HandleBlocking(GET, path, s.upgradeHandler(path, handler))
	return s.wsHub
}

// Hub returns the shared WSHub for broadcast / count operations.
// Returns nil if no WebSocket routes have been registered.
func (s *Breeze) Hub() *WSHub {
	return s.wsHub
}

// ─── Upgrade handler ─────────────────────────────────────────────────────────

// upgradeHandler returns an HTTP HandlerFunc that performs the RFC 6455
// WebSocket opening handshake and transitions the connection to WS mode.
//
// Security checks performed:
//  1. Method must be GET.
//  2. "Upgrade: websocket" header must be present (case-insensitive).
//  3. "Connection: Upgrade" header must be present.
//  4. "Sec-WebSocket-Key" must be present and non-empty.
//
// We deliberately do NOT check the Origin header here — that is application
// policy. Register a CORS/Origin middleware before calling WebSocket() if
// you need it.
func (s *Breeze) upgradeHandler(path string, handler WSHandler) HandlerFunc {
	return func(ctx *Context) error {
		req := ctx.Req

		upgrade := req.Header["upgrade"]
		if upgrade != "websocket" {
			ctx.Status(400)
			return ctx.WriteString("Bad Request: expected Upgrade: websocket")
		}
		conn2 := req.Header["connection"]
		if conn2 != "Upgrade" && conn2 != "keep-alive, Upgrade" {
			ctx.Status(400)
			return ctx.WriteString("Bad Request: expected Connection: Upgrade")
		}
		key := req.Header["sec-websocket-key"]
		if key == "" {
			ctx.Status(400)
			return ctx.WriteString("Bad Request: missing Sec-WebSocket-Key")
		}

		wc := &WSConn{
			conn: ctx.Conn,
			hub:  s.wsHub,
		}
		state := &wsConnState{
			wc:         wc,
			handler:    handler,
			maxPayload: wsMaxPayloadDefault,
			dispatch: &wsDispatchQueue{
				wc:      wc,
				handler: handler,
				pool:    s.Pool,
			},
		}
		s.wsConns.Store(ctx.Conn.Fd(), state)
		s.wsCount.Add(1)
		s.wsHub.register(wc)

		// Send 101 Switching Protocols — suppress normal response path.
		handshake := wsHandshakeResponse(key)
		// The write is queued on the event loop; a failure here means the
		// connection is already gone, which OnClose reports. There is nothing
		// useful to do with the error at this point in the upgrade.
		_ = ctx.Conn.AsyncWrite(handshake, nil)
		ctx.Res = nil // prevent Breeze from writing an additional response

		// Notify the handler (runs in the worker pool via the normal exec path).
		handler.OnConnect(wc)

		return nil
	}
}

// ─── Traffic routing ─────────────────────────────────────────────────────────

// isWSConn checks whether the given fd is a promoted WebSocket connection.
//
// Callers on the request path must gate this behind a wsCount check — the
// sync.Map Load boxes fd into an interface and allocates for any descriptor
// above 255.
func (s *Breeze) isWSConn(fd int) (*wsConnState, bool) {
	v, ok := s.wsConns.Load(fd)
	if !ok {
		return nil, false
	}
	return v.(*wsConnState), true
}

// handleWSTraffic processes incoming bytes for an already-upgraded connection.
// It is called from OnTraffic when isWSConn returns true.
//
// Frame handling:
//   - Control frames (Ping/Pong/Close) are handled inline (they are never
//     fragmented per RFC 6455 §5.5 and have a tiny payload ≤ 125 B).
//   - Data frames go through defragmentation (continuation support) and are
//     dispatched to the handler via the worker pool.
//   - A Close frame triggers graceful shutdown: we send a Close echo, call
//     OnClose, and clean up state.
func (s *Breeze) handleWSTraffic(c gnet.Conn, state *wsConnState) gnet.Action {
	fd := c.Fd()
	wc := state.wc

	raw, _ := c.Next(-1)
	if len(raw) == 0 {
		return gnet.None
	}

	// Accumulate in per-connection reassembly buffer (same pattern as HTTP bufs).
	var existing []byte
	if v, ok := s.wsRxBufs.Load(fd); ok {
		existing = v.([]byte)
	}
	buf := append(existing, raw...)

	for len(buf) > 0 {
		frame, consumed := parseWSFrame(buf, state.maxPayload)
		if consumed == -1 {
			// Protocol error — send close and drop.
			wc.Close(WsCloseProtocolError, "protocol error")
			s.cleanupWS(fd, wc, state, WsCloseProtocolError, "protocol error")
			return gnet.Close
		}
		if frame == nil {
			break // wait for more data
		}
		buf = buf[consumed:]

		switch frame.opcode {
		case wsOpPing:
			// RFC 6455 §5.5.2: respond with Pong, same payload.
			pong := buildWSFrame(wsOpPong, frame.payload)
			frame.release()
			_ = c.AsyncWrite(pong, nil)

		case wsOpPong:
			// Unsolicited pong — ignore per spec.
			frame.release()

		case wsOpClose:
			// FIX: Use frame.payload BEFORE returning frame to the pool.
			// The original code called frame.release() and then
			// read frame.payload to build the echo — a use-after-free
			// because parseWSFrame may have reused the pooled *wsFrame
			// and overwritten its payload slice.
			payload := frame.payload
			code, reason := parseClosePayload(payload)
			echo := buildWSFrame(wsOpClose, payload)
			frame.release()
			_ = c.AsyncWrite(echo, nil)
			s.cleanupWS(fd, wc, state, code, reason)
			return gnet.Close

		case wsOpText, wsOpBinary:
			s.handleDataFrame(wc, state, frame)

		case wsOpContinuation:
			s.handleContinuation(wc, state, frame)

		default:
			// Unknown opcode — close with WsCloseUnsupportedData (1003).
			wc.Close(WsCloseUnsupportedData, "unsupported opcode")
			s.cleanupWS(fd, wc, state, WsCloseUnsupportedData, "unsupported opcode")
			frame.release()
			return gnet.Close
		}
	}

	// Persist leftover bytes.
	if len(buf) == 0 {
		s.wsRxBufs.Delete(fd)
	} else {
		if cap(buf)-len(buf) > compactThreshold {
			compact := make([]byte, len(buf))
			copy(compact, buf)
			buf = compact
		}
		s.wsRxBufs.Store(fd, buf)
	}

	return gnet.None
}

// handleDataFrame processes a non-continuation data frame.
// Starts or extends a fragmented message, or dispatches a complete unfragmented one.
func (s *Breeze) handleDataFrame(wc *WSConn, state *wsConnState, frame *wsFrame) {
	if frame.fin {
		// Complete single-frame message — fast path, no fragBuf allocation.
		payload := frame.payload
		opcode := frame.opcode
		frame.release()
		s.dispatchMessage(wc, state, opcode, payload)
		return
	}
	// Begin fragmented message.
	wc.fragOp = frame.opcode
	wc.fragBuf = append(wc.fragBuf[:0], frame.payload...)
	frame.release()
}

// handleContinuation appends a continuation frame to the in-progress message.
func (s *Breeze) handleContinuation(wc *WSConn, state *wsConnState, frame *wsFrame) {
	wc.fragBuf = append(wc.fragBuf, frame.payload...)
	if frame.fin {
		payload := make([]byte, len(wc.fragBuf))
		copy(payload, wc.fragBuf)
		wc.fragBuf = wc.fragBuf[:0]
		opcode := wc.fragOp
		frame.release()
		s.dispatchMessage(wc, state, opcode, payload)
		return
	}
	frame.release()
}

// dispatchMessage hands a complete message to the connection's ordered queue.
//
// Per-connection FIFO, not merely one-at-a-time: see wsDispatchQueue for why the
// worker pool cannot be handed each message directly, and why a mutex would not
// have been enough.
//
// A full queue closes the connection. The alternative is dropping the message,
// which would silently break a protocol the handler is trying to parse — a peer
// that has outrun its handler by wsDispatchQueueDepth messages is better told than
// quietly given a stream with holes in it.
func (s *Breeze) dispatchMessage(wc *WSConn, state *wsConnState, opcode byte, payload []byte) {
	if state.dispatch.push(wsEvent{opcode: opcode, payload: payload}) {
		return
	}
	wc.Close(WsCloseMessageTooBig, "receive queue full")
}

// cleanupWS removes a WebSocket connection from all registries and notifies the handler.
//
// The close goes through the same ordered queue as messages, so OnClose runs after
// every message already delivered to it rather than racing them on another pool
// worker. An application that tears down per-connection state in OnClose would
// otherwise be handed a message after that state was gone.
func (s *Breeze) cleanupWS(fd int, wc *WSConn, state *wsConnState, code uint16, reason string) {
	wc.closed.Store(true)
	s.wsHub.unregister(wc)
	// LoadAndDelete, not Delete: cleanupWS is reachable both from a Close frame
	// and from OnClose, so the same fd can arrive twice. Decrementing only when
	// an entry was actually removed keeps wsCount from drifting negative and
	// silently re-enabling the map lookup on the HTTP fast path.
	if _, loaded := s.wsConns.LoadAndDelete(fd); loaded {
		s.wsCount.Add(-1)
	}
	s.wsRxBufs.Delete(fd)

	// push seals the queue, so this is idempotent for the same reason
	// LoadAndDelete is: both paths into cleanupWS may run for one connection.
	state.dispatch.push(wsEvent{isClose: true, code: code, reason: reason})
}

// parseClosePayload extracts the close code and reason from a Close frame payload.
// Returns WsCloseNormalClosure (1000) if the payload is empty.
func parseClosePayload(p []byte) (uint16, string) {
	if len(p) < 2 {
		return WsCloseNormalClosure, ""
	}
	code := uint16(p[0])<<8 | uint16(p[1])
	reason := ""
	if len(p) > 2 {
		reason = string(p[2:])
	}
	return code, reason
}

// ─── WSHub fields injected into Breeze ───────────────────────────────────────

// wsHubFields are embedded by value into Breeze to avoid a separate allocation.
// We use a separate unexported struct to keep breeze.go clean.
type wsHubFields struct {
	wsHub      *WSHub
	wsHandlers map[string]WSHandler
	wsConns    sync.Map // fd(int) → *wsConnState
	wsRxBufs   sync.Map // fd(int) → []byte  reassembly buffer

	// wsCount is the number of entries in wsConns, maintained alongside it so
	// OnTraffic can skip the map on a server that has no WebSocket
	// connections — which is every purely-HTTP server, and every other server
	// for the whole request path.
	//
	// The map read is not free the way the old comment claimed. sync.Map keys
	// are interface{}, so Load(fd) boxes an int, and the runtime only has
	// preallocated boxes for values 0-255. On a server busy enough to matter
	// the file descriptors are in the thousands, so that conversion heap
	// allocates — one allocation per request, to look up a key that is almost
	// never there. An atomic load costs nothing and removes it.
	wsCount atomic.Int64
}
