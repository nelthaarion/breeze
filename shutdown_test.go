package breeze

// shutdown_test.go — Breeze.Stop.
//
// Every test here runs a real server on a real port and stops it, because the
// property under test is not "Stop returned" but "the server is actually gone":
// the port is free, Run's goroutine has exited, and every WebSocket handler was
// told its connection closed. None of that is observable from a mocked engine.
//
// The HTTP requests are written to a raw socket rather than sent with net/http.
// Go's client keeps connections in a pool and may reuse or re-dial one at a
// moment of its choosing, which in a shutdown test shows up as a retry against a
// listener that is already closed — a client behaviour being reported as a server
// bug. A socket this file opened and wrote to has no such second opinion.

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ─── Helpers ──────────────────────────────────────────────────────────────────

// stopTestServer starts a server on a free port and returns it with that port.
//
// Run's error is captured rather than discarded: a test whose Stop reports
// success while Run failed to bind at all would otherwise pass on a machine
// where the port was taken.
func stopTestServer(t *testing.T, configure func(app *Breeze)) (app *Breeze, port int, runErr func() error) {
	t.Helper()

	port = wsTestPort(t)
	app = New(NewRouter(), NewEventLoopWorkerPool(runtime.NumCPU()))
	if configure != nil {
		configure(app)
	}

	var (
		mu   sync.Mutex
		err  error
		done = make(chan struct{})
	)
	go func() {
		defer close(done)
		e := app.Run(port, false)
		mu.Lock()
		err = e
		mu.Unlock()
	}()
	wsWaitForListener(t, port)

	return app, port, func() error {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("Run did not return within 10s of Stop")
		}
		mu.Lock()
		defer mu.Unlock()
		return err
	}
}

// waitPortFree blocks until nothing accepts on port, and reports whether it came
// free.
//
// This is the assertion that matters most in this file: Stop closing the listener
// is the difference between a stoppable server and one that merely stops
// answering. The retry loop is there because the kernel does not necessarily
// refuse the next connect the instant close returns.
func waitPortFree(port int, timeout time.Duration) bool {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			return true
		}
		_ = conn.Close()
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// httpGetRaw sends one GET on its own connection and returns the status line.
func httpGetRaw(port int, path string, timeout time.Duration) (string, error) {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetDeadline(time.Now().Add(timeout))
	req := "GET " + path + " HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		return "", err
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// ─── Stop with no active connections ──────────────────────────────────────────

// TestStopWithNoConnectionsClosesTheListener is the base case: an idle server
// stops, promptly, and gives its port back.
func TestStopWithNoConnectionsClosesTheListener(t *testing.T) {
	app, port, runErr := stopTestServer(t, func(app *Breeze) {
		app.Router.Handle(GET, "/ping", func(ctx *Context) error {
			return ctx.WriteString("pong")
		})
	})

	// A request first, so the test is stopping a server that has actually
	// served rather than one that only bound.
	if status, err := httpGetRaw(port, "/ping", 5*time.Second); err != nil {
		t.Fatalf("GET /ping before Stop: %v", err)
	} else if !strings.Contains(status, "200") {
		t.Fatalf("GET /ping before Stop: status = %q, want 200", status)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	if err := app.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	elapsed := time.Since(start)

	// An idle server has nothing to wait for, so a shutdown that takes seconds
	// means Stop is waiting on a poll interval rather than on real work — the
	// exact reason Stop does not simply return Engine.Stop, whose ticker is
	// half a second.
	if elapsed > 3*time.Second {
		t.Errorf("Stop on an idle server took %v; nothing was in flight to wait for", elapsed)
	}

	if err := runErr(); err != nil {
		t.Errorf("Run returned %v; a stopped server should return cleanly", err)
	}
	if !waitPortFree(port, 5*time.Second) {
		t.Errorf("port %d is still accepting connections after Stop", port)
	}
	if _, err := httpGetRaw(port, "/ping", time.Second); err == nil {
		t.Errorf("a request succeeded after Stop; the listener is still open")
	}
}

// TestRunReturnsAfterStop is requirement 4 stated on its own.
//
// Stop returning is not the same claim as Run's goroutine exiting: gnet's Run
// blocks inside its own event loops, and a Stop that signalled the engine
// without waiting would satisfy every other test here while leaking the
// goroutine that owns the listener.
func TestRunReturnsAfterStop(t *testing.T) {
	app, _, runErr := stopTestServer(t, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := app.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// runErr fails the test itself if Run has not returned, which is the
	// assertion — the value only distinguishes a clean exit from an error.
	if err := runErr(); err != nil {
		t.Errorf("Run returned %v, want nil", err)
	}
}

// ─── Stop with in-flight HTTP requests ────────────────────────────────────────

// TestStopWaitsForInFlightRequests is the graceful half of the contract: a
// request already being handled finishes before Stop returns.
//
// The route is registered blocking, which is what a slow handler must be in
// Breeze — an inline handler occupies its event loop and cannot be in flight
// across a shutdown at all, since the shutdown signal is delivered to that same
// goroutine.
//
// The handler signals when it has started and the test waits for that before
// stopping, so "in flight" is a fact rather than a hope about scheduling.
func TestStopWaitsForInFlightRequests(t *testing.T) {
	const handlerDuration = 300 * time.Millisecond

	var (
		started   = make(chan struct{}, 1)
		completed = make(chan struct{})
		once      sync.Once
	)

	app, port, runErr := stopTestServer(t, func(app *Breeze) {
		app.Router.HandleBlocking(GET, "/slow", func(ctx *Context) error {
			select {
			case started <- struct{}{}:
			default:
			}
			time.Sleep(handlerDuration)
			once.Do(func() { close(completed) })
			return ctx.WriteString("done")
		})
	})

	statuses := make(chan string, 1)
	go func() {
		status, err := httpGetRaw(port, "/slow", 15*time.Second)
		if err != nil {
			statuses <- "error: " + err.Error()
			return
		}
		statuses <- status
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatalf("the handler never started; nothing was in flight to test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := app.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// The point of the test. If Stop returned while the handler was still
	// running, this channel is not yet closed.
	select {
	case <-completed:
	default:
		t.Fatalf("Stop returned while a handler was still running; " +
			"in-flight work must finish before the engine is torn down")
	}

	// And the client got its answer, not a reset — a shutdown that closed the
	// connection out from under a finished handler would still fail here.
	select {
	case status := <-statuses:
		if !strings.Contains(status, "200") {
			t.Errorf("in-flight request finished with %q, want 200", status)
		}
	case <-time.After(5 * time.Second):
		t.Errorf("the in-flight request never received a response")
	}

	if err := runErr(); err != nil {
		t.Errorf("Run returned %v, want nil", err)
	}
	if !waitPortFree(port, 5*time.Second) {
		t.Errorf("port %d is still accepting connections after Stop", port)
	}
}

// TestStopReturnsContextErrorWhenTheDeadlinePasses is the force half: a handler
// that outlasts the context does not hold the shutdown open forever.
//
// The context error is the reported outcome, and the teardown happens anyway —
// which is net/http.Server.Shutdown's behaviour and the reason Stop takes a
// context at all.
func TestStopReturnsContextErrorWhenTheDeadlinePasses(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})

	app, port, runErr := stopTestServer(t, func(app *Breeze) {
		app.Router.HandleBlocking(GET, "/block", func(ctx *Context) error {
			select {
			case started <- struct{}{}:
			default:
			}
			<-release
			return ctx.WriteString("eventually")
		})
	})
	// Released whatever happens, so the worker goroutine does not outlive the
	// test even if an assertion below fails first.
	defer close(release)

	go func() { _, _ = httpGetRaw(port, "/block", 30*time.Second) }()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatalf("the handler never started")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	err := app.Stop(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop = %v, want context.DeadlineExceeded: a handler that "+
			"outlasts the context must be reported, not waited for", err)
	}

	// Reported, but still torn down. This is the assertion that separates
	// "graceful then force" from "graceful then give up".
	if err := runErr(); err != nil {
		t.Errorf("Run returned %v, want nil", err)
	}
	if !waitPortFree(port, 5*time.Second) {
		t.Errorf("port %d is still accepting after a deadline-exceeded Stop; "+
			"the force phase did not run", port)
	}
}

// ─── Stop with active WebSocket connections ───────────────────────────────────

// TestStopClosesActiveWebSocketsAndFiresOnClose is requirement 3.
//
// Three things are asserted, and each has its own failure mode:
//
//   - OnClose ran for every connection, before Stop returned. A shutdown that
//     force-closed the sockets without going through cleanupWS would leave
//     handlers never told, which for an application that flushes state in
//     OnClose means losing it.
//   - The code is 1001, not 1006. 1006 is what a dropped socket produces, so a
//     handler cannot distinguish a deliberate shutdown from a network failure
//     unless the shutdown path sets the code itself.
//   - The peer received a Close frame with 1001, so a conforming client sees an
//     orderly close on the wire rather than a truncated stream.
func TestStopClosesActiveWebSocketsAndFiresOnClose(t *testing.T) {
	const connections = 4

	var (
		mu     sync.Mutex
		codes  []uint16
		closed int
	)
	allClosed := make(chan struct{})
	var closedOnce sync.Once

	app, port, runErr := stopTestServer(t, func(app *Breeze) {
		app.WebSocket("/ws", &WSHandlerFunc{
			Close: func(_ *WSConn, code uint16, _ string) {
				mu.Lock()
				codes = append(codes, code)
				closed++
				full := closed >= connections
				mu.Unlock()
				if full {
					closedOnce.Do(func() { close(allClosed) })
				}
			},
		})
	})

	url := "ws://127.0.0.1:" + strconv.Itoa(port) + "/ws"
	peerCodes := make(chan uint16, connections)
	conns := make([]*WSConn, 0, connections)
	for i := 0; i < connections; i++ {
		conn, err := DialWS(url, WSClientConfig{HandshakeTimeout: 5 * time.Second})
		if err != nil {
			t.Fatalf("DialWS %d: %v", i, err)
		}
		conn.OnClose(func(code uint16, _ string) { peerCodes <- code })
		conns = append(conns, conn)
	}
	defer func() {
		for _, conn := range conns {
			conn.Close(WsCloseNormalClosure, "")
		}
	}()

	// The hub is the server's own count, so this waits for the upgrades to have
	// completed server-side rather than for the dials to have returned.
	deadline := time.Now().Add(5 * time.Second)
	for app.Hub().Count() < connections && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := app.Hub().Count(); got != connections {
		t.Fatalf("hub has %d connections, want %d", got, connections)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := app.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Before Stop returned, not eventually: OnClose is queued on each
	// connection's dispatch queue, and waiting for that queue to drain is part
	// of what Stop waits for.
	select {
	case <-allClosed:
	default:
		mu.Lock()
		got := closed
		mu.Unlock()
		t.Fatalf("Stop returned with OnClose fired for %d of %d connections; "+
			"every handler must be notified before the shutdown completes",
			got, connections)
	}

	mu.Lock()
	gotCodes := append([]uint16(nil), codes...)
	mu.Unlock()

	if len(gotCodes) != connections {
		t.Fatalf("OnClose fired %d times, want %d (once per connection): %v",
			len(gotCodes), connections, gotCodes)
	}
	for i, code := range gotCodes {
		if code != WsCloseGoingAway {
			t.Errorf("OnClose[%d] code = %d, want %d (going away). "+
				"A shutdown reported as %d is indistinguishable from a dropped socket.",
				i, code, WsCloseGoingAway, WsCloseAbnormalClosure)
		}
	}

	// The wire half. The peers were sent a Close frame, so their own close
	// callbacks report 1001 rather than the 1006 a bare socket close produces.
	for i := 0; i < connections; i++ {
		select {
		case code := <-peerCodes:
			if code != WsCloseGoingAway {
				t.Errorf("peer %d saw close code %d, want %d: the shutdown "+
					"Close frame did not reach it", i, code, WsCloseGoingAway)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("peer %d was never told the connection closed", i)
		}
	}

	if err := runErr(); err != nil {
		t.Errorf("Run returned %v, want nil", err)
	}
	if !waitPortFree(port, 5*time.Second) {
		t.Errorf("port %d is still accepting connections after Stop", port)
	}
}

// TestStopDeliversQueuedMessagesBeforeOnClose is the ordering guarantee under a
// shutdown, and the reason Stop closes WebSockets through cleanupWS instead of
// closing them itself.
//
// Messages are sent and then the server is stopped immediately, so a message is
// very likely still queued or being handled when the shutdown begins. A shutdown
// that called OnClose directly — from Stop's goroutine, while a pool worker was
// draining that message — would deliver the close first, which is precisely the
// bug wsDispatchQueue exists to prevent.
func TestStopDeliversQueuedMessagesBeforeOnClose(t *testing.T) {
	const messages = 16

	for iteration := range 5 {
		var (
			mu         sync.Mutex
			seen       int
			afterClose int
			closeSeen  bool
		)
		closedCh := make(chan struct{})
		firstMessage := make(chan struct{}, 1)
		var closeOnce sync.Once

		app, port, runErr := stopTestServer(t, func(app *Breeze) {
			app.WebSocket("/ordered", &WSHandlerFunc{
				Message: func(_ *WSConn, _ byte, _ []byte) {
					// A little work, so the drain is genuinely still running
					// when the shutdown lands.
					time.Sleep(time.Millisecond)
					mu.Lock()
					seen++
					if closeSeen {
						afterClose++
					}
					mu.Unlock()
					select {
					case firstMessage <- struct{}{}:
					default:
					}
				},
				Close: func(_ *WSConn, _ uint16, _ string) {
					mu.Lock()
					closeSeen = true
					mu.Unlock()
					closeOnce.Do(func() { close(closedCh) })
				},
			})
		})

		conn, err := DialWS("ws://127.0.0.1:"+strconv.Itoa(port)+"/ordered",
			WSClientConfig{HandshakeTimeout: 5 * time.Second})
		if err != nil {
			t.Fatalf("iteration %d: DialWS: %v", iteration, err)
		}

		for i := 0; i < messages; i++ {
			if err := conn.SendText("m"); err != nil {
				t.Fatalf("iteration %d: SendText: %v", iteration, err)
			}
		}

		// Stop only once delivery is under way, so the assertion below cannot
		// pass by the messages never having been delivered at all.
		select {
		case <-firstMessage:
		case <-time.After(5 * time.Second):
			t.Fatalf("iteration %d: no message reached the handler", iteration)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := app.Stop(ctx); err != nil {
			cancel()
			t.Fatalf("iteration %d: Stop: %v", iteration, err)
		}
		cancel()

		select {
		case <-closedCh:
		default:
			t.Fatalf("iteration %d: Stop returned before OnClose ran", iteration)
		}

		mu.Lock()
		gotAfter, gotSeen := afterClose, seen
		mu.Unlock()
		if gotSeen == 0 {
			t.Fatalf("iteration %d: no messages were delivered, so the ordering "+
				"assertion would be vacuous", iteration)
		}
		if gotAfter != 0 {
			t.Fatalf("iteration %d: %d of %d delivered message(s) arrived after "+
				"OnClose. The shutdown close must travel the connection's "+
				"dispatch queue, not overtake it.", iteration, gotAfter, gotSeen)
		}

		conn.Close(WsCloseNormalClosure, "")
		if err := runErr(); err != nil {
			t.Errorf("iteration %d: Run returned %v, want nil", iteration, err)
		}
	}
}

// TestStopNotifiesWebSocketHandlersWithAClosedPool covers the ordering it would be
// easy to get wrong the other way round: a caller that shuts the worker pool down
// before the server.
//
// A closed pool accepts no tasks, and the close event Stop queues is delivered by
// one — so without a fallback consumer the handler would never be told, and Stop
// would wait for a drain that cannot start until its context expired. Ordering is
// preserved either way: what guarantees it is that exactly one drain exists, not
// which goroutine runs it.
func TestStopNotifiesWebSocketHandlersWithAClosedPool(t *testing.T) {
	closed := make(chan uint16, 1)

	app, port, runErr := stopTestServer(t, func(app *Breeze) {
		app.WebSocket("/ws", &WSHandlerFunc{
			Close: func(_ *WSConn, code uint16, _ string) { closed <- code },
		})
	})

	conn, err := DialWS("ws://127.0.0.1:"+strconv.Itoa(port)+"/ws",
		WSClientConfig{HandshakeTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("DialWS: %v", err)
	}
	defer conn.Close(WsCloseNormalClosure, "")

	deadline := time.Now().Add(5 * time.Second)
	for app.Hub().Count() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if app.Hub().Count() != 1 {
		t.Fatalf("the connection never registered with the hub")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	app.Pool.Shutdown(ctx)

	if err := app.Stop(ctx); err != nil {
		t.Fatalf("Stop with a closed pool: %v", err)
	}

	select {
	case code := <-closed:
		if code != WsCloseGoingAway {
			t.Errorf("OnClose code = %d, want %d", code, WsCloseGoingAway)
		}
	default:
		t.Fatalf("Stop returned without notifying the handler; a closed pool " +
			"must not silence the shutdown close")
	}

	if err := runErr(); err != nil {
		t.Errorf("Run returned %v, want nil", err)
	}
}

// TestStopRefusesUpgradesOnExistingConnections closes the one hole OnOpen leaves.
//
// OnOpen refuses new connections, but an upgrade can arrive on a keep-alive
// connection that was accepted before the shutdown began. Registering it then
// would put a WebSocket connection into a registry Stop has already swept: its
// handler would hear about the close only from gnet's force-close, with 1006, and
// only after Stop had returned.
//
// The connection here is established and exercised first, so it is provably a
// pre-existing one, and the shutdown is held open by a blocking handler so the
// upgrade lands in the middle of the graceful phase.
func TestStopRefusesUpgradesOnExistingConnections(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseHold := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseHold()

	app, port, runErr := stopTestServer(t, func(app *Breeze) {
		app.WebSocket("/ws", &WSHandlerFunc{})
		app.Router.Handle(GET, "/ok", func(ctx *Context) error {
			return ctx.WriteString("ok")
		})
		app.Router.HandleBlocking(GET, "/hold", func(ctx *Context) error {
			select {
			case started <- struct{}{}:
			default:
			}
			<-release
			return ctx.WriteString("held")
		})
	})

	// A keep-alive connection, established and answered before the shutdown.
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	keepAlive, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = keepAlive.Close() }()
	_ = keepAlive.SetDeadline(time.Now().Add(15 * time.Second))

	reader := bufio.NewReader(keepAlive)
	if _, err := keepAlive.Write([]byte("GET /ok HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n")); err != nil {
		t.Fatalf("write /ok: %v", err)
	}
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read /ok status: %v", err)
	}
	if !strings.Contains(line, "200") {
		t.Fatalf("/ok on the keep-alive connection = %q, want 200", strings.TrimSpace(line))
	}
	// Drain the rest of that response so the next read starts on the next one.
	bodyLen := 0
	for {
		l, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("draining /ok headers: %v", err)
		}
		trimmed := strings.TrimSpace(l)
		if trimmed == "" {
			break
		}
		if name, value, ok := strings.Cut(trimmed, ":"); ok &&
			strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			if n, convErr := strconv.Atoi(strings.TrimSpace(value)); convErr == nil {
				bodyLen = n
			}
		}
	}
	if bodyLen > 0 {
		if _, err := io.ReadFull(reader, make([]byte, bodyLen)); err != nil {
			t.Fatalf("draining /ok body: %v", err)
		}
	}

	// Hold the shutdown open in its graceful phase.
	go func() { _, _ = httpGetRaw(port, "/hold", 30*time.Second) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatalf("the holding handler never started")
	}

	stopReturned := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		stopReturned <- app.Stop(ctx)
	}()
	time.Sleep(100 * time.Millisecond)

	upgrade := "GET /ws HTTP/1.1\r\nHost: 127.0.0.1\r\nUpgrade: websocket\r\n" +
		"Connection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := keepAlive.Write([]byte(upgrade)); err != nil {
		t.Fatalf("write upgrade: %v", err)
	}
	status, err := reader.ReadString('\n')
	if err != nil {
		// A reset here is also the server declining, which is acceptable: what
		// must not happen is a successful upgrade.
		t.Logf("upgrade during shutdown was answered with a closed connection: %v", err)
	} else if strings.Contains(status, "101") {
		t.Errorf("an upgrade succeeded during shutdown (%q); the connection would "+
			"be registered after Stop swept the registry", strings.TrimSpace(status))
	} else if !strings.Contains(status, "503") {
		t.Logf("upgrade during shutdown answered %q", strings.TrimSpace(status))
	}

	if got := app.Hub().Count(); got != 0 {
		t.Errorf("hub has %d connections during shutdown, want 0", got)
	}

	releaseHold()
	if err := <-stopReturned; err != nil {
		t.Errorf("Stop: %v", err)
	}
	if err := runErr(); err != nil {
		t.Errorf("Run returned %v, want nil", err)
	}
}

// ─── Idempotency, lifecycle, isolation ────────────────────────────────────────

// TestStopIsIdempotent is requirement 2's last clause. A second Stop must not
// panic, must not block, and must report what the first one did.
func TestStopIsIdempotent(t *testing.T) {
	app, port, runErr := stopTestServer(t, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := app.Stop(ctx); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := runErr(); err != nil {
		t.Fatalf("Run returned %v, want nil", err)
	}

	// Sequential repeats: promptly, and with the first call's answer.
	for i := 0; i < 3; i++ {
		start := time.Now()
		if err := app.Stop(ctx); err != nil {
			t.Fatalf("Stop call %d = %v, want nil", i+2, err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("Stop call %d took %v; a repeat call must return promptly",
				i+2, elapsed)
		}
	}

	// Concurrent repeats: the interesting case, since they exercise the waiting
	// path rather than the already-recorded one.
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- app.Stop(ctx)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent Stop = %v, want nil", err)
		}
	}

	if !waitPortFree(port, 5*time.Second) {
		t.Errorf("port %d is still accepting connections", port)
	}
}

// TestConcurrentStopPerformsOneShutdown checks that racing callers produce a
// single teardown rather than several — and specifically that the losers wait for
// it instead of reporting success while the listener is still open.
func TestConcurrentStopPerformsOneShutdown(t *testing.T) {
	app, port, runErr := stopTestServer(t, nil)

	const callers = 12
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	release := make(chan struct{})

	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-release
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			errs <- app.Stop(ctx)
		}()
	}
	close(release)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Errorf("Stop = %v, want nil from every concurrent caller", err)
		}
	}
	if err := runErr(); err != nil {
		t.Errorf("Run returned %v, want nil", err)
	}
	if !waitPortFree(port, 5*time.Second) {
		t.Errorf("port %d is still accepting connections after Stop", port)
	}
}

// TestStopOnAServerThatWasNeverRun reports rather than blocks.
//
// A supervisor that stops everything it created will reach this whenever one of
// those creations never got as far as Run, and blocking until the context expired
// would turn that into a slow shutdown of the whole process.
func TestStopOnAServerThatWasNeverRun(t *testing.T) {
	app := New(NewRouter(), NewEventLoopWorkerPool(2))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	err := app.Stop(ctx)
	if !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Stop = %v, want ErrNotRunning", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Stop on a server that never ran took %v; it must not wait "+
			"for a boot that is not coming", elapsed)
	}
}

// TestRunAfterStopIsRefused — a stopped *Breeze is not reusable, and says so
// rather than rebinding the port with its WebSocket state already torn down.
func TestRunAfterStopIsRefused(t *testing.T) {
	app, port, runErr := stopTestServer(t, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := app.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := runErr(); err != nil {
		t.Fatalf("Run returned %v, want nil", err)
	}

	if err := app.Run(port, false); !errors.Is(err, ErrServerStopped) {
		t.Fatalf("Run after Stop = %v, want ErrServerStopped", err)
	}
}

// TestStopDuringStartup covers the race the issue's usage pattern invites: Run on
// a goroutine, Stop immediately after, before gnet has finished booting.
//
// Stop has to wait for the boot it is about to undo. Answering ErrNotRunning here
// would leave the server running while its owner believed it had stopped, which
// is the worst available outcome — the next test binding that port fails instead.
//
// The loop waits for Run to have been entered before stopping, so this is the
// "booting" window rather than the "not started yet" one; the latter is
// TestStopOnAServerThatWasNeverRun.
func TestStopDuringStartup(t *testing.T) {
	for iteration := range 10 {
		port := wsTestPort(t)
		app := New(NewRouter(), NewEventLoopWorkerPool(runtime.NumCPU()))

		runDone := make(chan error, 1)
		go func() { runDone <- app.Run(port, false) }()

		// Deliberately no wsWaitForListener: the point is to race the bind.
		// runCalled is set by Run's first statements, so this waits only for
		// the goroutine to be inside Run.
		deadline := time.Now().Add(5 * time.Second)
		for !app.runCalled.Load() && time.Now().Before(deadline) {
			runtime.Gosched()
		}
		if !app.runCalled.Load() {
			t.Fatalf("iteration %d: Run was never entered", iteration)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		stopErr := app.Stop(ctx)
		cancel()

		var runErr error
		select {
		case runErr = <-runDone:
		case <-time.After(10 * time.Second):
			t.Fatalf("iteration %d: Run did not return after Stop", iteration)
		}

		// A bind failure is possible — wsTestPort released the port, so
		// something else may have taken it — and is not this test's subject.
		if runErr != nil {
			continue
		}
		if stopErr != nil {
			t.Fatalf("iteration %d: Stop during startup = %v, want nil "+
				"(Run booted, so there was an engine to stop)", iteration, stopErr)
		}
		if !waitPortFree(port, 5*time.Second) {
			t.Fatalf("iteration %d: port %d still accepting after Stop", iteration, port)
		}
	}
}

// TestStopAffectsOnlyItsOwnInstance is requirement 5.
//
// gnet's package-level Stop is keyed by address and stops "the last engine
// registered" for it, which is why Stop holds a per-instance Engine instead. Two
// servers on two ports, one stopped: the other must still be serving.
func TestStopAffectsOnlyItsOwnInstance(t *testing.T) {
	handler := func(ctx *Context) error { return ctx.WriteString("ok") }

	appA, portA, runErrA := stopTestServer(t, func(app *Breeze) {
		app.Router.Handle(GET, "/ok", handler)
	})
	appB, portB, runErrB := stopTestServer(t, func(app *Breeze) {
		app.Router.Handle(GET, "/ok", handler)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := appA.Stop(ctx); err != nil {
		t.Fatalf("Stop A: %v", err)
	}
	if err := runErrA(); err != nil {
		t.Errorf("Run A returned %v, want nil", err)
	}
	if !waitPortFree(portA, 5*time.Second) {
		t.Errorf("port %d (A) is still accepting after Stop", portA)
	}

	// B is untouched — the assertion the whole test exists for.
	status, err := httpGetRaw(portB, "/ok", 5*time.Second)
	if err != nil {
		t.Fatalf("B stopped serving when A was stopped: %v", err)
	}
	if !strings.Contains(status, "200") {
		t.Errorf("B answered %q after A was stopped, want 200", status)
	}

	if err := appB.Stop(ctx); err != nil {
		t.Fatalf("Stop B: %v", err)
	}
	if err := runErrB(); err != nil {
		t.Errorf("Run B returned %v, want nil", err)
	}
	if !waitPortFree(portB, 5*time.Second) {
		t.Errorf("port %d (B) is still accepting after Stop", portB)
	}
}

// TestStopRefusesNewConnectionsImmediately — "stops accepting" has to hold from
// the start of Stop, not from the moment gnet closes the listener, which does not
// happen until the force phase.
//
// The handler holds the shutdown in its graceful phase, so the connection below
// is attempted while the listener is provably still bound. It may be refused at
// connect or reset on write; both are the server declining to serve it, and
// either is correct. What must not happen is a 200.
func TestStopRefusesNewConnectionsImmediately(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})

	app, port, runErr := stopTestServer(t, func(app *Breeze) {
		app.Router.HandleBlocking(GET, "/hold", func(ctx *Context) error {
			select {
			case started <- struct{}{}:
			default:
			}
			<-release
			return ctx.WriteString("held")
		})
		app.Router.Handle(GET, "/ok", func(ctx *Context) error {
			return ctx.WriteString("ok")
		})
	})

	go func() { _, _ = httpGetRaw(port, "/hold", 30*time.Second) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatalf("the holding handler never started")
	}

	stopReturned := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		stopReturned <- app.Stop(ctx)
	}()

	// Stop is now in its graceful phase, waiting for /hold.
	time.Sleep(100 * time.Millisecond)

	status, err := httpGetRaw(port, "/ok", 2*time.Second)
	if err == nil && strings.Contains(status, "200") {
		t.Errorf("a new connection was served (%q) after Stop began; "+
			"new connections must be refused from the start of the shutdown", status)
	}

	close(release)
	if err := <-stopReturned; err != nil {
		t.Errorf("Stop: %v", err)
	}
	if err := runErr(); err != nil {
		t.Errorf("Run returned %v, want nil", err)
	}
}
