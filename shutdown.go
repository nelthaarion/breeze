package breeze

// shutdown.go — graceful shutdown for a running Breeze server.
//
// Run wraps gnet.Run, which blocks for the lifetime of the process and used to
// discard the one handle that can end it. This file captures that handle in
// OnBoot, and Stop turns it into the contract net/http.Server.Shutdown has: stop
// accepting, let in-flight work finish while the context allows, then tear the
// listener and the remaining connections down.
//
// The shape is deliberately net/http's rather than gnet's own. gnet.Stop(addr)
// is package-level and keyed by address — it stops "the last engine registered
// for this address", which is the wrong unit when a process runs two Breeze
// instances, and gnet itself has deprecated it for that reason. Engine.Stop is
// per-engine, and the engine here belongs to this *Breeze, so two instances in
// one process stop independently.

import (
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/panjf2000/gnet/v2"
)

// ─── Errors ───────────────────────────────────────────────────────────────────

var (
	// ErrNotRunning reports a Stop on a server that has no engine to stop:
	// Run was never called, or it returned before ever booting (a failed bind
	// is the usual reason).
	//
	// It is not an error worth panicking over — a supervisor that stops
	// everything it started will hit it whenever one of those starts failed —
	// but it is worth distinguishing from a clean stop, because "the server is
	// down" and "the server never came up" call for different logs.
	ErrNotRunning = errors.New("breeze: server is not running")

	// ErrServerStopped reports a Run on a server that has already been stopped.
	//
	// A *Breeze is not reusable after Stop: the WebSocket registry, the hub and
	// the dispatch queues have all been torn down by then, and rebinding the
	// port would serve traffic with half its state already collected. Build a
	// new one instead.
	ErrServerStopped = errors.New("breeze: server is stopped")

	// ErrShutdownIncomplete reports that the engine was told to shut down but
	// Run had not returned within engineExitTimeout.
	//
	// Distinct from a context deadline: a context deadline means application
	// handlers were still working, which is the caller's own timeout choice,
	// while this means gnet's teardown itself did not finish — the connections
	// may or may not be closed, and the port may still be bound.
	ErrShutdownIncomplete = errors.New("breeze: engine did not finish shutting down")
)

// ─── Tunables ─────────────────────────────────────────────────────────────────

const (
	// inflightPollInterval is how often the graceful phase re-checks whether
	// in-flight work has finished.
	//
	// Polling rather than signalling, because the alternative is a branch on
	// the request path: a handler completion would have to test whether a
	// shutdown is waiting before it could notify one. This loop only exists
	// during a shutdown, and a millisecond of latency there is not worth an
	// instruction in OnTraffic's descendants.
	inflightPollInterval = time.Millisecond

	// engineExitTimeout bounds how long Stop waits for Run to return after the
	// engine has been told to shut down.
	//
	// This is a safety net, not a policy knob. gnet's teardown does not wait on
	// application code — it signals its event loops, force-closes what is left
	// and joins — so it either completes in milliseconds or something inside
	// gnet is stuck. Ten seconds is long enough that a loaded machine does not
	// trip it and short enough that a stuck teardown is reported rather than
	// hung on.
	engineExitTimeout = 10 * time.Second

	// wsShutdownReason is the Close frame reason sent to every WebSocket peer.
	// The code is WsCloseGoingAway (1001), which is what RFC 6455 §7.4.1
	// reserves for an endpoint that is disappearing rather than one that
	// objected to something.
	wsShutdownReason = "server shutting down"
)

// ─── State ────────────────────────────────────────────────────────────────────

// stopResult carries Stop's outcome to a second caller.
//
// A struct rather than storing the error in an atomic.Pointer[error] directly,
// because a nil *error and a pointer to a nil error would then mean the same
// thing on load, and "the shutdown succeeded" has to be distinguishable from
// "the shutdown has not finished".
type stopResult struct {
	err error
}

// shutdownFields is the shutdown state on Breeze, embedded by value the same way
// wsHubFields and mcpFields are.
type shutdownFields struct {
	// chansOnce guards the lazy creation of the channels below.
	//
	// Lazy because a *Breeze is not always built by New: the error and MCP
	// tests construct &Breeze{} literals, and an engine assembled that way must
	// not deadlock on a nil channel if something calls Stop on it. sync.Once
	// also supplies the happens-before that lets other goroutines read the
	// fields it set.
	chansOnce sync.Once

	// booted is closed once OnBoot has stored the engine.
	booted chan struct{}
	// runExited is closed once Run's gnet.Run call has returned.
	runExited chan struct{}
	// stopped is closed once Stop has finished and recorded its result.
	stopped chan struct{}

	bootOnce    sync.Once
	runExitOnce sync.Once
	stopOnce    sync.Once

	// engine is the gnet handle Run created, captured in OnBoot. gnet passes
	// Engine by value and it is a single pointer wide, so this stores the
	// address of a copy rather than making the field an interface.
	engine atomic.Pointer[gnet.Engine]

	// runCalled distinguishes "Run has not booted yet" from "Run was never
	// called", so Stop can wait for the first and refuse the second instead of
	// blocking until its context expires either way.
	runCalled atomic.Bool

	// stopping is set at the very start of a shutdown, before anything is torn
	// down. OnOpen reads it to refuse connections that arrive between then and
	// the moment the listener actually closes — the "stop accepting" half of
	// the contract, which gnet cannot do on its own because Engine.Stop closes
	// the listener and the live connections in the same step.
	stopping atomic.Bool

	// stopErr is Stop's recorded outcome, so a second caller gets the same
	// answer as the first rather than a second teardown or a nil.
	stopErr atomic.Pointer[stopResult]

	// inflight counts work that a shutdown must wait for: requests dispatched
	// to the worker pool, and WebSocket dispatch drains.
	//
	// Inline requests are deliberately not counted. An inline handler runs on
	// the gnet event-loop goroutine inside OnTraffic, and the shutdown signal
	// is delivered to that same goroutine as a queued task — so it cannot be
	// processed until OnTraffic has returned, and an inline request is always
	// already finished by the time its loop starts tearing down. Counting it
	// would put two atomic read-modify-writes on the one path in this framework
	// that has none.
	inflight atomic.Int64
}

// initShutdownState creates the channels a shutdown coordinates on.
func (s *Breeze) initShutdownState() {
	s.chansOnce.Do(func() {
		s.booted = make(chan struct{})
		s.runExited = make(chan struct{})
		s.stopped = make(chan struct{})
	})
}

// ─── gnet lifecycle hooks ─────────────────────────────────────────────────────

// OnBoot is called by gnet once, before the event loops start, with the handle
// to the engine it just built. Keeping it is the whole reason Stop can exist.
//
// It returns gnet.None even when a shutdown has already been requested. The
// tempting alternative — returning gnet.Shutdown to make a racing Run stop
// itself — makes gnet's run return before it defers its own teardown, which
// leaves the engine in a state where Engine.Stop waits for an inShutdown flag
// nothing will ever set. Run refuses up front instead (see Run), and a Stop that
// arrives during boot is handled by waiting for this hook.
func (s *Breeze) OnBoot(eng gnet.Engine) gnet.Action {
	s.initShutdownState()
	captured := eng
	s.engine.Store(&captured)
	s.bootOnce.Do(func() { close(s.booted) })
	return gnet.None
}

// OnOpen refuses new connections once a shutdown has begun.
//
// This is what makes "stops accepting new connections" true from the first
// instruction of Stop rather than from the moment gnet closes the listener,
// which does not happen until the force phase. The cost on the normal path is
// one relaxed atomic load per accepted connection — not per request.
func (s *Breeze) OnOpen(gnet.Conn) ([]byte, gnet.Action) {
	if s.stopping.Load() {
		return nil, gnet.Close
	}
	return nil, gnet.None
}

// markRunExited records that Run's gnet.Run call has returned.
func (s *Breeze) markRunExited() {
	s.runExitOnce.Do(func() { close(s.runExited) })
}

// ─── Stop ─────────────────────────────────────────────────────────────────────

// Stop shuts the server down gracefully, following net/http.Server.Shutdown's
// contract: it stops accepting new connections, gives work already in flight a
// chance to finish while ctx allows, then closes the listener and whatever
// connections are left.
//
// The order is:
//
//  1. New connections are refused immediately, before anything is torn down.
//  2. Every active WebSocket connection is sent a Close frame with code 1001
//     ("going away") and its handler's OnClose is queued on the connection's
//     ordered dispatch queue — the same path a peer-initiated close takes, so a
//     handler is never told the connection is gone before it has been given the
//     last message that arrived on it.
//  3. Stop waits for work already dispatched to the worker pool — blocking
//     routes, WebSocket handler callbacks — to finish, or for ctx to be done.
//  4. The engine is told to shut down, which closes the listener and force-closes
//     any connection still open, and Stop waits for Run to return.
//
// Returns nil when everything finished within ctx, ctx's error when step 3 ran
// out of time (the teardown still happens — that is the "then forcibly close"
// half), ErrNotRunning when there was no running engine to stop, and
// ErrShutdownIncomplete when gnet's own teardown did not finish.
//
// Stop is idempotent. The first call performs the shutdown; later calls return
// its result without repeating any of it, and a call that overlaps the first
// waits for it rather than starting a second teardown.
//
// When Stop returns, Run has returned too — so
//
//	go func() { _ = app.Run(port, true) }()
//	// ...
//	err := app.Stop(ctx)
//
// leaves no goroutine behind.
//
// Stop does not shut down Breeze.Pool. The pool is supplied by the caller and
// may be shared with subsystems that outlive the HTTP server, so ending its
// worker goroutines is the caller's decision: follow Stop with
// Pool.Shutdown(ctx) when the pool is the server's alone.
func (s *Breeze) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.initShutdownState()

	first := false
	s.stopOnce.Do(func() { first = true })
	if !first {
		// A second caller reports the first call's outcome. It waits, because
		// returning nil while the listener is still open would be a lie, but it
		// waits under its own context so it cannot be held longer than the
		// caller allowed.
		select {
		case <-s.stopped:
			if res := s.stopErr.Load(); res != nil {
				return res.err
			}
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	err := s.shutdown(ctx)
	// Stored before the close, so a waiter woken by it always sees the result.
	s.stopErr.Store(&stopResult{err: err})
	close(s.stopped)
	return err
}

// shutdown is the body of the first Stop call.
func (s *Breeze) shutdown(ctx context.Context) error {
	// Before anything else, so no connection is accepted into a server that is
	// on its way down.
	s.stopping.Store(true)

	eng, err := s.awaitEngine(ctx)
	if err != nil {
		return err
	}

	// ── Graceful phase ───────────────────────────────────────────────────
	// WebSocket connections are closed explicitly rather than left to the
	// force phase. They are long-lived by design — a P2P connection is idle
	// for most of its life — so waiting for them to end on their own would
	// mean waiting for the context to expire on every shutdown, and dropping
	// them without a Close frame would make a clean shutdown
	// indistinguishable from a network failure at the peer.
	s.closeWebSockets()
	graceful := s.waitForInflight(ctx)

	// ── Force phase ──────────────────────────────────────────────────────
	select {
	case <-s.runExited:
		// Run has already returned — the engine tore itself down without us,
		// which happens when a bind failed after boot. There is nothing left
		// to stop, and calling Engine.Stop on it would wait for a flag that
		// is already set or never will be.
		if !graceful {
			return ctx.Err()
		}
		return nil
	default:
	}

	// Engine.Stop signals the engine, then polls a flag on a half-second
	// ticker until the teardown has finished. Waiting on its return would put
	// that polling interval into every shutdown, so it runs on its own
	// goroutine for the signal and Run's own exit is what Stop waits on — that
	// is both exact and the condition callers actually care about.
	//
	// The context here bounds only that goroutine's wait, not the teardown:
	// Engine.Stop signals before it starts polling, so cancelling it cannot
	// leave the engine half-stopped.
	engCtx, cancel := context.WithTimeout(context.Background(), engineExitTimeout)
	defer cancel()
	go func() { _ = eng.Stop(engCtx) }()

	timer := time.NewTimer(engineExitTimeout)
	defer timer.Stop()
	select {
	case <-s.runExited:
	case <-timer.C:
		return ErrShutdownIncomplete
	}

	if !graceful {
		// The listener and the connections are gone, but handlers were still
		// working when the deadline passed. Reporting the deadline is what
		// net/http does, and it is the only way a caller can tell a clean
		// shutdown from a truncated one.
		return ctx.Err()
	}
	return nil
}

// awaitEngine returns the engine Run booted, waiting for a boot that is still in
// progress.
//
// The wait matters for the shape the issue asked about: a caller that starts Run
// on a goroutine and stops it a moment later can easily win the race against
// gnet's startup, and answering ErrNotRunning there would leave the server
// running with its owner convinced it had stopped.
func (s *Breeze) awaitEngine(ctx context.Context) (*gnet.Engine, error) {
	if eng := s.engine.Load(); eng != nil {
		return eng, nil
	}
	if !s.runCalled.Load() {
		return nil, ErrNotRunning
	}

	select {
	case <-s.booted:
		if eng := s.engine.Load(); eng != nil {
			return eng, nil
		}
		return nil, ErrNotRunning
	case <-s.runExited:
		// Run returned without ever booting — a failed bind, or an address
		// gnet rejected.
		return nil, ErrNotRunning
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// waitForInflight blocks until dispatched work has drained, or ctx is done.
// It reports whether the drain completed.
//
// The final read on either exit is deliberate: work that finished in the same
// instant the deadline passed did finish, and reporting it as a timeout would
// turn a clean shutdown into a spurious context error.
func (s *Breeze) waitForInflight(ctx context.Context) bool {
	if s.inflight.Load() == 0 {
		return true
	}
	// An already-expired context means the caller allowed no graceful window at
	// all. Answering it here rather than after a tick keeps Stop's promptness a
	// property of the context rather than of the poll interval.
	select {
	case <-ctx.Done():
		return s.inflight.Load() == 0
	default:
	}

	ticker := time.NewTicker(inflightPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if s.inflight.Load() == 0 {
				return true
			}
		case <-ctx.Done():
			return s.inflight.Load() == 0
		}
	}
}

// ─── WebSocket shutdown ───────────────────────────────────────────────────────

// closeWebSockets closes every active WebSocket connection with code 1001.
func (s *Breeze) closeWebSockets() {
	// Same gate the request path uses: a server with no WebSocket connections
	// does not touch the map at all.
	if s.wsCount.Load() == 0 {
		return
	}
	s.wsConns.Range(func(key, value any) bool {
		fd, okFD := key.(int)
		state, okState := value.(*wsConnState)
		if okFD && okState && state != nil {
			s.shutdownWS(fd, state)
		}
		return true
	})
}

// shutdownWS closes one WebSocket connection for a server shutdown: Close frame
// out, handler notified through the ordered queue, socket closed.
//
// The three steps are in that order for a reason each.
//
// The frame goes first because it has to be queued on the connection's event
// loop before the close is, and gnet flushes a connection's outbound buffer
// before it closes the socket — so the peer sees 1001 rather than a truncated
// stream.
//
// cleanupWS goes second, ahead of the socket close, because it is what decides
// the code the handler is told. Closing the socket also reaches cleanupWS, from
// OnClose, but with 1006 "abnormal closure" — correct for a peer that vanished,
// wrong for a server that is shutting down deliberately. Whichever call arrives
// first seals the dispatch queue and wins, and the socket close is asynchronous
// (gnet queues it on the event loop), so the only way to make the code
// deterministic is to seal it from here. The second arrival is a no-op: the
// queue is sealed and the registry entry is gone.
//
// Sealing through cleanupWS rather than calling the handler directly is the
// point of requirement 3. The queue is why OnClose cannot overtake a message
// that arrived just before the shutdown; a direct call from this goroutine
// would race the pool worker draining that message and reintroduce exactly the
// ordering bug wsDispatchQueue exists to prevent.
func (s *Breeze) shutdownWS(fd int, state *wsConnState) {
	wc := state.wc

	// wc.closed is the same guard WSConn.Close uses, so a connection that is
	// already closing by another path is not written to twice.
	if !wc.closed.Swap(true) {
		payload := make([]byte, 2+len(wsShutdownReason))
		binary.BigEndian.PutUint16(payload, WsCloseGoingAway)
		copy(payload[2:], wsShutdownReason)
		if frame := wc.buildFrame(wsOpClose, payload); frame != nil {
			_ = wc.conn.AsyncWrite(frame, nil)
		}
	}

	s.cleanupWS(fd, wc, state, WsCloseGoingAway, wsShutdownReason)

	_ = wc.conn.Close()
}
