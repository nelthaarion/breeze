package breeze

// websocket_order_test.go — inbound per-connection message ordering.
//
// This is a regression test for a bug that a single passing run cannot detect.
// dispatchMessage used to Submit each message to the worker pool as its own task,
// so two messages from one connection could be picked up by two workers and run in
// either order. It came out correctly ordered by chance often enough that an
// unrepeated test would have called it fixed.
//
// So every ordering test here runs the exchange many times within one test, and
// reports the first run that came back out of order along with the sequence it saw.
// A one-in-ten reordering is then a near-certain failure rather than a coin flip.

import (
	"encoding/binary"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"
)

// wsOrderMessages is how many messages one iteration sends.
//
// Enough that the old pool-per-message dispatch had many pairs to reorder — the
// reported repro used ten and reordered reliably — and small enough that the whole
// test stays under a second.
const wsOrderMessages = 32

// wsOrderIterations is how many times each ordering test repeats the exchange.
//
// The property is probabilistic in the failure case, so a single iteration proves
// nothing. Thirty is enough that a reordering happening on one run in ten fails
// this test with probability better than 1 - 10^-13, while `go test` still finishes
// in about a second.
const wsOrderIterations = 30

// wsOrderRecorder collects the sequence numbers a handler was given, in the order
// it was given them.
type wsOrderRecorder struct {
	mu   sync.Mutex
	seen []int

	// done is closed once the expected count has arrived, so a test waits on
	// delivery rather than on a sleep.
	done  chan struct{}
	want  int
	fired bool
}

func newWSOrderRecorder(want int) *wsOrderRecorder {
	return &wsOrderRecorder{done: make(chan struct{}), want: want}
}

// record appends a sequence number.
func (r *wsOrderRecorder) record(seq int) {
	r.mu.Lock()
	r.seen = append(r.seen, seq)
	if len(r.seen) >= r.want && !r.fired {
		r.fired = true
		close(r.done)
	}
	r.mu.Unlock()
}

// snapshot returns a copy of what has been seen so far.
func (r *wsOrderRecorder) snapshot() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]int, len(r.seen))
	copy(out, r.seen)
	return out
}

// wait blocks until every expected message has arrived, or the timeout expires.
func (r *wsOrderRecorder) wait(timeout time.Duration) bool {
	select {
	case <-r.done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// firstDisorder returns the index of the first out-of-place element, or -1.
//
// The whole sequence is reported on failure rather than just the mismatching pair,
// because the shape of the disorder is the diagnosis: adjacent swaps mean two
// workers raced, while a value arriving many places early means a queue was drained
// concurrently.
func firstDisorder(seen []int) int {
	for i, seq := range seen {
		if seq != i {
			return i
		}
	}
	return -1
}

// ─── The regression test ──────────────────────────────────────────────────────

// TestInboundMessagesArriveInOrder is the regression test for the reordering bug.
//
// Messages carry their sequence number in the payload and are sent back-to-back, so
// they reach the server as fast as the socket allows and several are in flight at
// once — which is the condition that made the old per-message pool dispatch reorder
// them.
//
// The client is Breeze's own DialWS, so this exercises the real path an application
// uses rather than a hand-written peer.
func TestInboundMessagesArriveInOrder(t *testing.T) {
	for iteration := range wsOrderIterations {
		recorder := newWSOrderRecorder(wsOrderMessages)

		port := wsTestPort(t)
		router := NewRouter()
		app := New(router, NewEventLoopWorkerPool(runtime.NumCPU()))

		app.WebSocket("/order", &WSHandlerFunc{
			Message: func(_ *WSConn, _ byte, payload []byte) {
				if len(payload) < 4 {
					t.Errorf("iteration %d: a message arrived with a %d-byte payload",
						iteration, len(payload))
					return
				}
				recorder.record(int(binary.BigEndian.Uint32(payload)))
			},
		})
		go func() { _ = app.Run(port, false) }()
		wsWaitForListener(t, port)

		conn, err := DialWS("ws://127.0.0.1:"+strconv.Itoa(port)+"/order", WSClientConfig{})
		if err != nil {
			t.Fatalf("iteration %d: DialWS: %v", iteration, err)
		}

		// Back-to-back, with nothing between them: no sleep, no round trip, no
		// waiting for an echo. Anything that let one message be handled before
		// the next was sent would hide exactly the bug being tested.
		for seq := range wsOrderMessages {
			payload := make([]byte, 4)
			binary.BigEndian.PutUint32(payload, uint32(seq))
			if err := conn.SendBinary(payload); err != nil {
				t.Fatalf("iteration %d: SendBinary(%d): %v", iteration, seq, err)
			}
		}

		if !recorder.wait(10 * time.Second) {
			t.Fatalf("iteration %d: only %d of %d messages arrived within 10s: %v",
				iteration, len(recorder.snapshot()), wsOrderMessages, recorder.snapshot())
		}
		conn.Close(WsCloseNormalClosure, "")

		seen := recorder.snapshot()
		if at := firstDisorder(seen); at >= 0 {
			t.Fatalf("iteration %d: message %d arrived at position %d.\n"+
				"  sent  = %v\n  got   = %v\n"+
				"Per-connection delivery must be FIFO: a handler parsing a stream cannot "+
				"recover from messages it was given in the wrong order.",
				iteration, seen[at], at, wsOrderSequence(wsOrderMessages), seen)
		}
	}
}

// wsOrderSequence renders the expected order, for a failure message that shows both
// sides rather than leaving the reader to infer one.
func wsOrderSequence(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

// TestInboundOrderHoldsWithASlowHandler is the same property under the condition
// that makes reordering easiest to produce.
//
// A handler that takes time leaves more messages queued behind it, so more pairs are
// available to be delivered out of order. Under the old design this is where the
// reordering was most visible; under a single-consumer queue the delay changes
// nothing, because there is only ever one consumer.
//
// The delay is deliberately tiny. It has to be long enough that the next message has
// certainly arrived before the current one finishes, and short enough that 32 of
// them do not dominate the suite's runtime.
func TestInboundOrderHoldsWithASlowHandler(t *testing.T) {
	for iteration := range 5 {
		recorder := newWSOrderRecorder(wsOrderMessages)

		port := wsTestPort(t)
		router := NewRouter()
		app := New(router, NewEventLoopWorkerPool(runtime.NumCPU()))

		app.WebSocket("/slow", &WSHandlerFunc{
			Message: func(_ *WSConn, _ byte, payload []byte) {
				time.Sleep(time.Millisecond)
				recorder.record(int(binary.BigEndian.Uint32(payload)))
			},
		})
		go func() { _ = app.Run(port, false) }()
		wsWaitForListener(t, port)

		conn, err := DialWS("ws://127.0.0.1:"+strconv.Itoa(port)+"/slow", WSClientConfig{})
		if err != nil {
			t.Fatalf("iteration %d: DialWS: %v", iteration, err)
		}

		for seq := range wsOrderMessages {
			payload := make([]byte, 4)
			binary.BigEndian.PutUint32(payload, uint32(seq))
			if err := conn.SendBinary(payload); err != nil {
				t.Fatalf("iteration %d: SendBinary(%d): %v", iteration, seq, err)
			}
		}

		if !recorder.wait(15 * time.Second) {
			t.Fatalf("iteration %d: only %d of %d messages arrived: %v",
				iteration, len(recorder.snapshot()), wsOrderMessages, recorder.snapshot())
		}
		conn.Close(WsCloseNormalClosure, "")

		seen := recorder.snapshot()
		if at := firstDisorder(seen); at >= 0 {
			t.Fatalf("iteration %d: with a 1ms handler, message %d arrived at position %d:\n  %v",
				iteration, seen[at], at, seen)
		}
	}
}

// ─── Close ordering ───────────────────────────────────────────────────────────

// TestOnCloseArrivesAfterEveryMessage — the close travels the same queue as the
// messages, so it cannot overtake one.
//
// Under the old design OnClose was its own pool task, so it could run while earlier
// messages were still queued on other workers. That is worse than message
// reordering: an application that releases per-connection state in OnClose would be
// handed a message after tearing that state down, which is a use-after-free in
// everything but name.
func TestOnCloseArrivesAfterEveryMessage(t *testing.T) {
	for iteration := range 10 {
		var (
			mu         sync.Mutex
			afterClose int
			closed     bool
			messages   int
		)
		done := make(chan struct{})
		var doneOnce sync.Once

		port := wsTestPort(t)
		router := NewRouter()
		app := New(router, NewEventLoopWorkerPool(runtime.NumCPU()))

		app.WebSocket("/closeorder", &WSHandlerFunc{
			Message: func(_ *WSConn, _ byte, _ []byte) {
				mu.Lock()
				messages++
				if closed {
					afterClose++
				}
				mu.Unlock()
			},
			Close: func(_ *WSConn, _ uint16, _ string) {
				mu.Lock()
				closed = true
				mu.Unlock()
				doneOnce.Do(func() { close(done) })
			},
		})
		go func() { _ = app.Run(port, false) }()
		wsWaitForListener(t, port)

		conn, err := DialWS("ws://127.0.0.1:"+strconv.Itoa(port)+"/closeorder", WSClientConfig{})
		if err != nil {
			t.Fatalf("iteration %d: DialWS: %v", iteration, err)
		}

		// Messages, then an immediate close, with nothing in between: the close
		// frame arrives while the messages are still being handled, which is the
		// window where OnClose could overtake them.
		for seq := range wsOrderMessages {
			payload := make([]byte, 4)
			binary.BigEndian.PutUint32(payload, uint32(seq))
			if err := conn.SendBinary(payload); err != nil {
				t.Fatalf("iteration %d: SendBinary(%d): %v", iteration, seq, err)
			}
		}
		conn.Close(WsCloseNormalClosure, "done sending")

		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("iteration %d: OnClose never ran", iteration)
		}

		// A moment for anything that was going to arrive late to arrive.
		time.Sleep(50 * time.Millisecond)

		mu.Lock()
		late, total := afterClose, messages
		mu.Unlock()

		if late > 0 {
			t.Fatalf("iteration %d: %d of %d messages were delivered after OnClose. "+
				"A handler that releases per-connection state in OnClose would then be "+
				"handed a message with that state already gone.", iteration, late, total)
		}
	}
}

// TestOrderedDispatchDoesNotStrandAConnectionOnPanic covers the failure mode the
// queue introduces and the old design did not have.
//
// One drain now delivers many messages. A panic escaping it would leave running
// true with nothing consuming, so every later message on that connection would queue
// behind a consumer that no longer exists — the connection would go silent rather
// than lose one message. wsDispatchQueue.call contains the panic for that reason;
// this is the test that the containment works.
func TestOrderedDispatchDoesNotStrandAConnectionOnPanic(t *testing.T) {
	recorder := newWSOrderRecorder(wsOrderMessages - 1)

	port := wsTestPort(t)
	router := NewRouter()
	app := New(router, NewEventLoopWorkerPool(runtime.NumCPU()))

	app.WebSocket("/panics", &WSHandlerFunc{
		Message: func(_ *WSConn, _ byte, payload []byte) {
			seq := int(binary.BigEndian.Uint32(payload))
			// The first message panics. Everything after it must still arrive,
			// and in order.
			if seq == 0 {
				panic("handler panic on the first message")
			}
			recorder.record(seq)
		},
	})
	go func() { _ = app.Run(port, false) }()
	wsWaitForListener(t, port)

	conn, err := DialWS("ws://127.0.0.1:"+strconv.Itoa(port)+"/panics", WSClientConfig{})
	if err != nil {
		t.Fatalf("DialWS: %v", err)
	}
	defer conn.Close(WsCloseNormalClosure, "")

	for seq := range wsOrderMessages {
		payload := make([]byte, 4)
		binary.BigEndian.PutUint32(payload, uint32(seq))
		if err := conn.SendBinary(payload); err != nil {
			t.Fatalf("SendBinary(%d): %v", seq, err)
		}
	}

	if !recorder.wait(10 * time.Second) {
		t.Fatalf("after a handler panic only %d of %d later messages arrived — the "+
			"connection was stranded with no consumer: %v",
			len(recorder.snapshot()), wsOrderMessages-1, recorder.snapshot())
	}

	// Still in order: 1..31, so each value is one more than its index.
	seen := recorder.snapshot()
	for i, seq := range seen {
		if seq != i+1 {
			t.Fatalf("message %d arrived at position %d after the panic:\n  %v", seq, i, seen)
		}
	}
}
