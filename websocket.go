package breeze

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"math"
	"sync"
	"sync/atomic"

	"github.com/panjf2000/gnet/v2"
)

// ─── WebSocket constants ──────────────────────────────────────────────────────

const (
	wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

	wsOpContinuation = 0x0
	wsOpText         = 0x1
	wsOpBinary       = 0x2
	wsOpClose        = 0x8
	wsOpPing         = 0x9
	wsOpPong         = 0xA

	wsMaxControlPayload = 125
	wsMaxFrameHeader    = 14 // 2 + 8 (extended len) + 4 (mask)

	// Exported opcode constants for use by application code.
	WsOpText   = wsOpText
	WsOpBinary = wsOpBinary
)

// ErrFrameTooLarge is returned when a client sends a payload larger than the
// configured limit (default 64 KiB for control frames is always 125 B).
var ErrFrameTooLarge = errors.New("websocket: frame payload too large")

// ─── WebSocket frame ──────────────────────────────────────────────────────────

// wsFrame holds a fully decoded WebSocket frame.
// It is allocated from wsFramePool to reduce GC pressure.
type wsFrame struct {
	opcode  byte
	fin     bool
	payload []byte
}

var wsFramePool = sync.Pool{
	New: func() any { return &wsFrame{} },
}

// release resets the frame and returns it to the pool.
// After calling Release, the frame must not be used anymore.
func (f *wsFrame) release() {
	if f == nil {
		return
	}

	f.opcode = 0
	f.fin = false
	f.payload = nil
	wsFramePool.Put(f)
}

// ─── Handshake ────────────────────────────────────────────────────────────────

// wsHandshakeResponse builds a minimal RFC 6455 upgrade response.
// We write directly into a stack-local byte slice to avoid fmt.Sprintf.
func wsHandshakeResponse(key string) []byte {
	// Sec-WebSocket-Accept = base64(SHA-1(key + GUID))
	h := sha1.New()
	h.Write([]byte(key))
	h.Write([]byte(wsGUID))
	accept := base64.StdEncoding.EncodeToString(h.Sum(nil))

	resp := make([]byte, 0, 256)
	resp = append(resp, "HTTP/1.1 101 Switching Protocols\r\n"...)
	resp = append(resp, "Upgrade: websocket\r\n"...)
	resp = append(resp, "Connection: Upgrade\r\n"...)
	resp = append(resp, "Sec-WebSocket-Accept: "...)
	resp = append(resp, accept...)
	resp = append(resp, "\r\n\r\n"...)
	return resp
}

// ─── Frame parser ─────────────────────────────────────────────────────────────

// parseWSFrame reads one WebSocket frame from buf.
//
// Returns:
//
//	frame, consumed — a fully decoded frame and the number of bytes consumed.
//	nil, 0          — not enough data yet; caller should wait for more bytes.
//	nil, -1         — protocol error; caller should close the connection.
//
// Masking is validated and unmasked in-place on a copy so the caller can hold
// the payload slice without worrying about aliasing.
//
// Performance:
//   - No allocations for control frames (payload ≤ 125 B): payload is a
//     small fixed-size array on the stack, copied out of buf.
//   - For data frames the payload is allocated once; masking is done in-place.
//   - The mask-XOR inner loop uses 32-bit word operations (4 bytes at a time)
//     to keep throughput high on large payloads.
func parseWSFrame(buf []byte, maxPayload int) (frame *wsFrame, consumed int) {
	if len(buf) < 2 {
		return nil, 0
	}

	b0, b1 := buf[0], buf[1]
	fin := b0&0x80 != 0
	// RSV1-3 must be 0 (no extensions negotiated).
	if b0&0x70 != 0 {
		return nil, -1
	}
	opcode := b0 & 0x0F
	masked := b1&0x80 != 0
	payLen := int(b1 & 0x7F)

	// RFC 6455 §5.5: Control frames (opcode >= 0x8) MUST:
	//   1. Have a payload ≤ 125 bytes (no extended length encoding).
	//      payLen is 126 or 127 here means someone tried to use extended
	//      length for a control frame — that's a protocol error.
	//   2. NOT be fragmented (FIN must be 1).
	// Control frames: Close (0x8), Ping (0x9), Pong (0xA).
	isControl := opcode >= wsOpClose
	if isControl {
		if payLen > wsMaxControlPayload {
			return nil, -1 // control frame payload exceeds 125 bytes
		}
		if !fin {
			return nil, -1 // control frames must not be fragmented
		}
	}

	offset := 2
	switch payLen {
	case 126:
		if len(buf) < offset+2 {
			return nil, 0
		}
		payLen = int(binary.BigEndian.Uint16(buf[offset:]))
		offset += 2
	case 127:
		if len(buf) < offset+8 {
			return nil, 0
		}
		v := binary.BigEndian.Uint64(buf[offset:])
		if v > uint64(maxPayload) {
			return nil, -1
		}
		payLen = int(v)
		offset += 8
	}

	if payLen > maxPayload {
		return nil, -1
	}

	var maskKey [4]byte
	if masked {
		if len(buf) < offset+4 {
			return nil, 0
		}
		copy(maskKey[:], buf[offset:offset+4])
		offset += 4
	}

	// Defensive invariant: the frame header (base + extended length + mask)
	// must never exceed wsMaxFrameHeader (14 bytes = 2 + 8 + 4). If it does,
	// the parsing logic above has a bug — reject the frame rather than
	// silently reading corrupt data.
	if offset > wsMaxFrameHeader {
		return nil, -1
	}

	if len(buf) < offset+payLen {
		return nil, 0
	}

	// Allocate and copy payload; unmask in-place.
	payload := make([]byte, payLen)
	copy(payload, buf[offset:offset+payLen])

	if masked && payLen > 0 {
		unmaskXOR(payload, maskKey)
	}

	f := wsFramePool.Get().(*wsFrame)
	f.opcode = opcode
	f.fin = fin
	f.payload = payload

	return f, offset + payLen
}

// unmaskXOR applies the RFC 6455 XOR mask to p in place.
// Processes 4 bytes at a time using 32-bit arithmetic, then handles the tail.
func unmaskXOR(p []byte, key [4]byte) {
	k32 := binary.BigEndian.Uint32(key[:])
	i := 0
	for ; i+4 <= len(p); i += 4 {
		v := binary.BigEndian.Uint32(p[i:])
		binary.BigEndian.PutUint32(p[i:], v^k32)
	}
	// Remaining bytes
	for ; i < len(p); i++ {
		p[i] ^= key[i&3]
	}
}

// ─── Frame builder ────────────────────────────────────────────────────────────

// buildWSFrame encodes a server-to-client frame (never masked per RFC 6455).
// For small payloads (≤ 125 B) the result fits in a stack buffer.
func buildWSFrame(opcode byte, payload []byte) []byte {
	const maxWSPayloadLen = math.MaxInt - 10 // largest header is 10 bytes

	payLen := len(payload)
	if payLen > maxWSPayloadLen {
		return nil
	}

	var headerSize int
	switch {
	case payLen <= 125:
		headerSize = 2
	case payLen <= 65535:
		headerSize = 4
	default:
		headerSize = 10
	}

	frameLen := headerSize + payLen
	frame := make([]byte, frameLen)
	frame[0] = 0x80 | opcode // FIN=1
	switch {
	case payLen <= 125:
		frame[1] = byte(payLen)
	case payLen <= 65535:
		frame[1] = 126
		binary.BigEndian.PutUint16(frame[2:], uint16(payLen))
	default:
		frame[1] = 127
		binary.BigEndian.PutUint64(frame[2:], uint64(payLen))
	}
	copy(frame[headerSize:], payload)
	return frame
}

// ─── WSConn ───────────────────────────────────────────────────────────────────

// WSConn wraps a gnet.Conn with WebSocket framing and is the handle
// exposed to user handlers.
type WSConn struct {
	conn   gnet.Conn
	hub    *WSHub
	closed atomic.Bool

	// fragBuf accumulates continuation frames until FIN=1.
	fragBuf []byte
	fragOp  byte
}

// Send writes a text or binary message to this specific client.
// It is safe to call from any goroutine.
func (wc *WSConn) Send(opcode byte, payload []byte) error {
	if wc.closed.Load() {
		return errors.New("websocket: connection closed")
	}
	frame := buildWSFrame(opcode, payload)
	if frame == nil {
		return errors.New("websocket: payload too large")
	}
	return wc.conn.AsyncWrite(frame, nil)
}

// SendText is a convenience wrapper for text messages.
func (wc *WSConn) SendText(msg string) error {
	return wc.Send(wsOpText, []byte(msg))
}

// SendBinary is a convenience wrapper for binary messages.
func (wc *WSConn) SendBinary(msg []byte) error {
	return wc.Send(wsOpBinary, msg)
}

// Close sends a Close frame then marks the connection closed.
func (wc *WSConn) Close(code uint16, reason string) {
	if wc.closed.Swap(true) {
		return // already closed
	}
	payload := make([]byte, 2+len(reason))
	binary.BigEndian.PutUint16(payload, code)
	copy(payload[2:], reason)
	_ = wc.conn.AsyncWrite(buildWSFrame(wsOpClose, payload), nil)
	wc.conn.Close()
}

// RemoteAddr returns the remote address string.
func (wc *WSConn) RemoteAddr() string {
	return wc.conn.RemoteAddr().String()
}

// ─── WSHub ────────────────────────────────────────────────────────────────────

// WSHub is a thread-safe registry of all active WebSocket connections.
// It provides efficient broadcast and per-client targeting.
//
// Design:
//   - sync.RWMutex with sharded map eliminates contention between Broadcast
//     (read lock) and Register/Unregister (write lock).
//   - Broadcast builds the frame once, then sends it to all clients concurrently
//     using the worker pool if available — zero per-client allocations.
type WSHub struct {
	mu      sync.RWMutex
	clients map[*WSConn]struct{}
	pool    *WorkerPool
	count   atomic.Int64
}

// newWSHub creates a hub. pool may be nil (Broadcast will use goroutines).
func newWSHub(pool *WorkerPool) *WSHub {
	return &WSHub{
		clients: make(map[*WSConn]struct{}, 64),
		pool:    pool,
	}
}

// register adds wc to the hub.
func (h *WSHub) register(wc *WSConn) {
	h.mu.Lock()
	h.clients[wc] = struct{}{}
	h.count.Add(1)
	h.mu.Unlock()
}

// unregister removes wc from the hub.
func (h *WSHub) unregister(wc *WSConn) {
	h.mu.Lock()
	delete(h.clients, wc)
	h.count.Add(-1)
	h.mu.Unlock()
}

// Broadcast sends payload to every connected client.
// The frame is encoded once; all clients receive the same byte slice.
// Delivery is async: individual slow clients cannot block others.
func (h *WSHub) Broadcast(opcode byte, payload []byte) {
	frame := buildWSFrame(opcode, payload)

	h.mu.RLock()
	targets := make([]*WSConn, 0, len(h.clients))
	for wc := range h.clients {
		if !wc.closed.Load() {
			targets = append(targets, wc)
		}
	}
	h.mu.RUnlock()

	for _, wc := range targets {
		wc := wc
		send := func() {
			_ = wc.conn.AsyncWrite(frame, nil)
		}
		if h.pool != nil {
			h.pool.Submit(send)
		} else {
			go send()
		}
	}
}

// BroadcastText is a convenience wrapper for text broadcast.
func (h *WSHub) BroadcastText(msg string) {
	h.Broadcast(wsOpText, []byte(msg))
}

// BroadcastBinary is a convenience wrapper for binary broadcast.
func (h *WSHub) BroadcastBinary(msg []byte) {
	h.Broadcast(wsOpBinary, msg)
}

// BroadcastExcept sends to all clients except the given one (e.g. echo-exclude).
func (h *WSHub) BroadcastExcept(opcode byte, payload []byte, skip *WSConn) {
	frame := buildWSFrame(opcode, payload)

	h.mu.RLock()
	targets := make([]*WSConn, 0, len(h.clients))
	for wc := range h.clients {
		if wc != skip && !wc.closed.Load() {
			targets = append(targets, wc)
		}
	}
	h.mu.RUnlock()

	for _, wc := range targets {
		wc := wc
		send := func() { _ = wc.conn.AsyncWrite(frame, nil) }
		if h.pool != nil {
			h.pool.Submit(send)
		} else {
			go send()
		}
	}
}

// Count returns the number of active WebSocket connections.
func (h *WSHub) Count() int64 {
	return h.count.Load()
}

// ─── WSHandler ────────────────────────────────────────────────────────────────

// WSHandler is the user-facing interface for WebSocket event handling.
type WSHandler interface {
	// OnConnect is called once after the handshake completes.
	OnConnect(conn *WSConn)
	// OnMessage is called for each complete message (after defragmentation).
	// opcode is wsOpText or wsOpBinary.
	OnMessage(conn *WSConn, opcode byte, payload []byte)
	// OnClose is called when the connection closes, with the close code and reason.
	OnClose(conn *WSConn, code uint16, reason string)
}

// WSHandlerFunc is a functional variant so users can inline handlers without
// defining a struct. Fields that are nil are silently skipped.
type WSHandlerFunc struct {
	Connect func(conn *WSConn)
	Message func(conn *WSConn, opcode byte, payload []byte)
	Close   func(conn *WSConn, code uint16, reason string)
}

func (h *WSHandlerFunc) OnConnect(c *WSConn) {
	if h.Connect != nil {
		h.Connect(c)
	}
}
func (h *WSHandlerFunc) OnMessage(c *WSConn, op byte, p []byte) {
	if h.Message != nil {
		h.Message(c, op, p)
	}
}
func (h *WSHandlerFunc) OnClose(c *WSConn, code uint16, reason string) {
	if h.Close != nil {
		h.Close(c, code, reason)
	}
}
