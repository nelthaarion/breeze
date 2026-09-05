package dashboard

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/nelthaarion/breeze"
)

// wsFlushInterval is how often queued live events are flushed to clients.
// Batching at 100ms turns a burst of N events into one frame instead of N,
// which is the difference between a dashboard that keeps up under load and
// one that melts the browser's event loop.
const wsFlushInterval = 100 * time.Millisecond

// wsMaxBatch caps how many events accumulate between flushes. Beyond this
// the oldest are dropped: a developer dashboard showing the most recent
// 512 events is useful, one that queues unboundedly is a memory leak.
const wsMaxBatch = 512

// wsHub multiplexes dashboard WebSocket connections and broadcasts periodic
// snapshots and live events to every connected client.
//
// Two kinds of messages are sent:
//
//  1. "snapshot" — a full state snapshot, sent on connect and every second.
//  2. "batch"    — an array of live records (request / query / log / event),
//     flushed every wsFlushInterval.
//
// The hub never blocks the hot path. Producers append to a pending slice
// under a short mutex; a single goroutine does the JSON encoding and the
// socket writes. A client whose write fails is dropped immediately rather
// than retried, because a dashboard frame is worthless once it is stale.
type wsHub struct {
	mu      sync.RWMutex
	clients map[*breeze.WSConn]struct{}
	c       *Collector
	stop    chan struct{}

	// pending holds live events awaiting the next flush.
	pmu     sync.Mutex
	pending []wsEvent

	closeOnce sync.Once
}

// wsEvent is one live record queued for broadcast.
type wsEvent struct {
	Channel string `json:"channel"`
	Time    string `json:"time"`
	Data    any    `json:"data"`
}

func newWSHub(c *Collector) *wsHub {
	h := &wsHub{
		clients: make(map[*breeze.WSConn]struct{}),
		c:       c,
		stop:    make(chan struct{}),
		pending: make([]wsEvent, 0, 64),
	}
	go h.broadcastLoop()
	return h
}

// close stops the hub's background goroutine. Safe to call more than once.
func (h *wsHub) close() {
	h.closeOnce.Do(func() { close(h.stop) })
}

// register adds a client and immediately sends a snapshot.
func (h *wsHub) register(conn *breeze.WSConn) {
	h.mu.Lock()
	h.clients[conn] = struct{}{}
	h.mu.Unlock()
	_ = conn.SendText(h.snapshotMessage())
}

// unregister removes a client.
func (h *wsHub) unregister(conn *breeze.WSConn) {
	h.mu.Lock()
	delete(h.clients, conn)
	h.mu.Unlock()
}

// broadcast sends a text message to every connected client, dropping any
// client whose write fails. Without the drop, a half-open connection would
// linger in the map forever and be written to on every tick.
func (h *wsHub) broadcast(msg string) {
	h.mu.RLock()
	targets := make([]*breeze.WSConn, 0, len(h.clients))
	for c := range h.clients {
		targets = append(targets, c)
	}
	h.mu.RUnlock()

	var dead []*breeze.WSConn
	for _, c := range targets {
		if err := c.SendText(msg); err != nil {
			dead = append(dead, c)
		}
	}
	if len(dead) == 0 {
		return
	}
	h.mu.Lock()
	for _, c := range dead {
		delete(h.clients, c)
	}
	h.mu.Unlock()
}

// broadcastLoop drives both cadences: a snapshot every second so charts keep
// ticking when idle, and a batch flush every wsFlushInterval so live events
// arrive promptly without one frame per event.
func (h *wsHub) broadcastLoop() {
	snap := time.NewTicker(time.Second)
	defer snap.Stop()
	flush := time.NewTicker(wsFlushInterval)
	defer flush.Stop()

	for {
		select {
		case <-h.stop:
			return
		case <-flush.C:
			h.flush()
		case <-snap.C:
			if h.clientCount() > 0 {
				h.broadcast(h.snapshotMessage())
			}
		}
	}
}

// flush encodes and sends any queued events as a single batch frame.
func (h *wsHub) flush() {
	h.pmu.Lock()
	if len(h.pending) == 0 {
		h.pmu.Unlock()
		return
	}
	batch := h.pending
	// Hand off the slice and start a fresh one so producers are never
	// blocked by the encode/write below.
	h.pending = make([]wsEvent, 0, 64)
	h.pmu.Unlock()

	if h.clientCount() == 0 {
		return
	}

	msg, err := json.Marshal(map[string]any{
		"type":   "batch",
		"time":   time.Now().UTC().Format(time.RFC3339),
		"events": batch,
	})
	if err != nil {
		return
	}
	h.broadcast(string(msg))
}

// snapshotMessage builds a JSON envelope containing the dashboard overview.
func (h *wsHub) snapshotMessage() string {
	m := map[string]any{
		"type":    "snapshot",
		"time":    time.Now().UTC().Format(time.RFC3339),
		"metrics": h.c.Metrics(),
		"routes":  h.c.RouteStats(),
		"queue":   h.c.QueueStats(),
		"cache":   h.c.CacheStats(),
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// pushEvent queues a single live event for the next batch flush.
// Called from the hot path (request, query, log, event recorders), so it
// does no encoding and no I/O — just an append under a short mutex.
//
// Generic, and a free function because Go does not permit type parameters on
// methods. The type parameter is what makes the early return free: with a
// `payload any` parameter the caller boxes the value — allocating, because a
// RequestRecord does not fit in a pointer word — before the call can check
// whether anyone is listening. A server whose dashboard nobody has open was
// paying that allocation per slow request to hand a value to a function that
// immediately dropped it. Boxing now happens at the append below, on the path
// that actually keeps the value.
func pushEvent[T any](h *wsHub, kind string, payload T) {
	if h.clientCount() == 0 {
		return // nobody watching → no work
	}
	now := time.Now().UTC().Format(time.RFC3339)
	h.pmu.Lock()
	if len(h.pending) >= wsMaxBatch {
		// Drop the oldest to bound memory under a burst.
		copy(h.pending, h.pending[1:])
		h.pending = h.pending[:len(h.pending)-1]
	}
	h.pending = append(h.pending, wsEvent{
		Channel: kind,
		Time:    now,
		Data:    payload,
	})
	h.pmu.Unlock()
}

func (h *wsHub) clientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// ─── Bridge to Breeze WSHandler ───────────────────────────────────────────

// wsHandler adapts the dashboard hub to Breeze's WSHandler interface.
//
// KNOWN LIMITATION — the dashboard WebSocket is not authenticated.
//
// breeze.WSConn exposes only Send/SendBinary/SendText/Close/RemoteAddr; it
// carries no reference to the handshake request, so the session cookie
// cannot be read from OnConnect. app.WebSocket also registers its upgrade
// handler on the router directly, leaving no seam to wrap it with the
// dashboard's auth middleware.
//
// Consequence: anyone able to reach base+"/ws" can stream dashboard
// metrics, route stats and cache counters without logging in. The HTTP API
// and pages remain gated by AuthMiddleware; only this stream is exposed.
//
// Closing this gap requires a change in the core package — either
// WSConn.Header(name string) or an upgrade hook that can reject the
// handshake. Until then, bind the dashboard to a trusted interface.
type wsHandler struct {
	hub *wsHub
}

func (h *wsHandler) OnConnect(conn *breeze.WSConn) {
	h.hub.register(conn)
}

func (h *wsHandler) OnMessage(conn *breeze.WSConn, opcode byte, payload []byte) {
	// Clients may send "ping" text frames; ignore everything else.
	// The dashboard protocol is strictly server → client, so accepting
	// commands here would only widen the attack surface.
}

func (h *wsHandler) OnClose(conn *breeze.WSConn, code uint16, reason string) {
	h.hub.unregister(conn)
}
