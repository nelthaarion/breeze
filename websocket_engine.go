package breeze

import (
	"sync"
	"sync/atomic"

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
// WSHub is the shared hub exposed via b.WSHub for broadcast operations.
//
// wsHandlers maps a route pattern (e.g. "/ws") to the WSHandler registered
// via b.WebSocket(). Looked up once per upgrade to avoid repeated router calls.

// initWS lazily initialises WebSocket fields on the Breeze engine.
// Called by WebSocket() before the server starts, not on the hot path.
func (b *Breeze) initWS() {
	if b.wsHub == nil {
		b.wsHub = newWSHub(b.Pool)
	}
	if b.wsHandlers == nil {
		b.wsHandlers = make(map[string]WSHandler)
	}
}

// WebSocket registers a WebSocket endpoint at the given path and returns
// the shared WSHub, which is created on the first call and reused for all
// subsequent WebSocket routes.
func (b *Breeze) WebSocket(path string, handler WSHandler) *WSHub {
	b.initWS()
	b.wsHandlers[path] = handler
	// upgradeHandler reads from b.wsHandlers at call time, so updating the
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
	b.Router.HandleBlocking(GET, path, b.upgradeHandler(path, handler))
	return b.wsHub
}

// Hub returns the shared WSHub for broadcast / count operations.
// Returns nil if no WebSocket routes have been registered.
func (b *Breeze) Hub() *WSHub {
	return b.wsHub
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
func (b *Breeze) upgradeHandler(path string, handler WSHandler) HandlerFunc {
	return func(ctx *Context) {
		req := ctx.Req

		upgrade := req.Header["upgrade"]
		if upgrade != "websocket" {
			ctx.Status(400)
			ctx.WriteString("Bad Request: expected Upgrade: websocket")
			return
		}
		conn2 := req.Header["connection"]
		if conn2 != "Upgrade" && conn2 != "keep-alive, Upgrade" {
			ctx.Status(400)
			ctx.WriteString("Bad Request: expected Connection: Upgrade")
			return
		}
		key := req.Header["sec-websocket-key"]
		if key == "" {
			ctx.Status(400)
			ctx.WriteString("Bad Request: missing Sec-WebSocket-Key")
			return
		}

		wc := &WSConn{
			conn: ctx.Conn,
			hub:  b.wsHub,
		}
		state := &wsConnState{
			wc:         wc,
			handler:    handler,
			maxPayload: wsMaxPayloadDefault,
		}
		b.wsConns.Store(ctx.Conn.Fd(), state)
		b.wsCount.Add(1)
		b.wsHub.register(wc)

		// Send 101 Switching Protocols — suppress normal response path.
		handshake := wsHandshakeResponse(key)
		ctx.Conn.AsyncWrite(handshake, nil)
		ctx.Res = nil // prevent Breeze from writing an additional response

		// Notify the handler (runs in the worker pool via the normal exec path).
		handler.OnConnect(wc)
	}
}

// ─── Traffic routing ─────────────────────────────────────────────────────────

// isWSConn checks whether the given fd is a promoted WebSocket connection.
//
// Callers on the request path must gate this behind a wsCount check — the
// sync.Map Load boxes fd into an interface and allocates for any descriptor
// above 255.
func (b *Breeze) isWSConn(fd int) (*wsConnState, bool) {
	v, ok := b.wsConns.Load(fd)
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
func (b *Breeze) handleWSTraffic(c gnet.Conn, state *wsConnState) gnet.Action {
	fd := c.Fd()
	wc := state.wc

	raw, _ := c.Next(-1)
	if len(raw) == 0 {
		return gnet.None
	}

	// Accumulate in per-connection reassembly buffer (same pattern as HTTP bufs).
	var existing []byte
	if v, ok := b.wsRxBufs.Load(fd); ok {
		existing = v.([]byte)
	}
	buf := append(existing, raw...)

	for len(buf) > 0 {
		frame, consumed := parseWSFrame(buf, state.maxPayload)
		if consumed == -1 {
			// Protocol error — send close and drop.
			wc.Close(WsCloseProtocolError, "protocol error")
			b.cleanupWS(fd, wc, state.handler, WsCloseProtocolError, "protocol error")
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
			b.cleanupWS(fd, wc, state.handler, code, reason)
			return gnet.Close

		case wsOpText, wsOpBinary:
			b.handleDataFrame(wc, state, frame)

		case wsOpContinuation:
			b.handleContinuation(wc, state, frame)

		default:
			// Unknown opcode — close with WsCloseUnsupportedData (1003).
			wc.Close(WsCloseUnsupportedData, "unsupported opcode")
			b.cleanupWS(fd, wc, state.handler, WsCloseUnsupportedData, "unsupported opcode")
			frame.release()
			return gnet.Close
		}
	}

	// Persist leftover bytes.
	if len(buf) == 0 {
		b.wsRxBufs.Delete(fd)
	} else {
		if cap(buf)-len(buf) > compactThreshold {
			compact := make([]byte, len(buf))
			copy(compact, buf)
			buf = compact
		}
		b.wsRxBufs.Store(fd, buf)
	}

	return gnet.None
}

// handleDataFrame processes a non-continuation data frame.
// Starts or extends a fragmented message, or dispatches a complete unfragmented one.
func (b *Breeze) handleDataFrame(wc *WSConn, state *wsConnState, frame *wsFrame) {
	if frame.fin {
		// Complete single-frame message — fast path, no fragBuf allocation.
		payload := frame.payload
		opcode := frame.opcode
		frame.release()
		b.dispatchMessage(wc, state.handler, opcode, payload)
		return
	}
	// Begin fragmented message.
	wc.fragOp = frame.opcode
	wc.fragBuf = append(wc.fragBuf[:0], frame.payload...)
	frame.release()
}

// handleContinuation appends a continuation frame to the in-progress message.
func (b *Breeze) handleContinuation(wc *WSConn, state *wsConnState, frame *wsFrame) {
	wc.fragBuf = append(wc.fragBuf, frame.payload...)
	if frame.fin {
		payload := make([]byte, len(wc.fragBuf))
		copy(payload, wc.fragBuf)
		wc.fragBuf = wc.fragBuf[:0]
		opcode := wc.fragOp
		frame.release()
		b.dispatchMessage(wc, state.handler, opcode, payload)
		return
	}
	frame.release()
}

// dispatchMessage routes a complete message to the handler via the worker pool.
func (b *Breeze) dispatchMessage(wc *WSConn, handler WSHandler, opcode byte, payload []byte) {
	task := func() { handler.OnMessage(wc, opcode, payload) }
	if b.Pool != nil {
		b.Pool.Submit(task)
	} else {
		go task()
	}
}

// cleanupWS removes a WebSocket connection from all registries and notifies the handler.
func (b *Breeze) cleanupWS(fd int, wc *WSConn, handler WSHandler, code uint16, reason string) {
	wc.closed.Store(true)
	b.wsHub.unregister(wc)
	// LoadAndDelete, not Delete: cleanupWS is reachable both from a Close frame
	// and from OnClose, so the same fd can arrive twice. Decrementing only when
	// an entry was actually removed keeps wsCount from drifting negative and
	// silently re-enabling the map lookup on the HTTP fast path.
	if _, loaded := b.wsConns.LoadAndDelete(fd); loaded {
		b.wsCount.Add(-1)
	}
	b.wsRxBufs.Delete(fd)

	task := func() { handler.OnClose(wc, code, reason) }
	if b.Pool != nil {
		b.Pool.Submit(task)
	} else {
		go task()
	}
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
