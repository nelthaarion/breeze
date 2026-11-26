package events

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ─── Worker pool overflow policies ────────────────────────────────────────

func TestConfigNormalizesDocumentedOverflowDefault(t *testing.T) {
	defaultBus := New(Config{Async: AsyncWorkerPool, Workers: 1, QueueSize: 1})
	if got := defaultBus.Config().AsyncOverflow; got != OverflowSpawn {
		t.Fatalf("zero AsyncOverflow normalized to %v, want OverflowSpawn", got)
	}
	defaultBus.Close()

	blockingBus := New(Config{Async: AsyncWorkerPool, AsyncOverflow: OverflowBlock})
	if got := blockingBus.Config().AsyncOverflow; got != OverflowBlock {
		t.Fatalf("explicit AsyncOverflow normalized to %v, want OverflowBlock", got)
	}
	blockingBus.Close()
}

// TestPoolOverflowDrop verifies that OverflowDrop discards work rather
// than blocking or spawning once the queue is saturated.
func TestPoolOverflowDrop(t *testing.T) {
	// One worker, queue of one, so the second submit while the worker is
	// parked has nowhere to go.
	p := newWorkerPool(1, 1, OverflowDrop)
	defer p.close()

	release := make(chan struct{})
	occupied := make(chan struct{})

	// Occupy the single worker.
	if !p.submit(func() {
		close(occupied)
		<-release
	}) {
		t.Fatal("first submit rejected")
	}
	<-occupied

	// Fill the queue slot.
	if !p.submit(func() {}) {
		t.Fatal("second submit rejected while queue had room")
	}

	// This one must be dropped: worker busy, queue full.
	if p.submit(func() { t.Error("dropped task ran") }) {
		t.Fatal("third submit accepted, want dropped")
	}
	if got := p.stats().Dropped; got != 1 {
		t.Fatalf("Dropped=%d, want 1", got)
	}

	close(release)
}

// TestPoolOverflowSpawn verifies that OverflowSpawn runs the task in a
// fresh goroutine instead of dropping it.
func TestPoolOverflowSpawn(t *testing.T) {
	p := newWorkerPool(1, 1, OverflowSpawn)
	defer p.close()

	release := make(chan struct{})
	occupied := make(chan struct{})

	p.submit(func() {
		close(occupied)
		<-release
	})
	<-occupied
	p.submit(func() {}) // fills queue

	spawned := make(chan struct{})
	if !p.submit(func() { close(spawned) }) {
		t.Fatal("spawn submit reported failure")
	}
	select {
	case <-spawned:
	case <-time.After(time.Second):
		t.Fatal("spawned task did not run")
	}
	if got := p.stats().Spawned; got != 1 {
		t.Fatalf("Spawned=%d, want 1", got)
	}

	close(release)
}

// TestPoolCloseWaitsForOverflow verifies that work spawned outside the
// bounded queue is part of shutdown too.
func TestPoolCloseWaitsForOverflow(t *testing.T) {
	p := newWorkerPool(1, 1, OverflowSpawn)
	workerRelease := make(chan struct{})
	workerStarted := make(chan struct{})
	if !p.submit(func() {
		close(workerStarted)
		<-workerRelease
	}) {
		t.Fatal("worker task rejected")
	}
	<-workerStarted
	if !p.submit(func() {}) {
		t.Fatal("queue task rejected")
	}

	overflowStarted := make(chan struct{})
	overflowRelease := make(chan struct{})
	if !p.submit(func() {
		close(overflowStarted)
		<-overflowRelease
	}) {
		t.Fatal("overflow task rejected")
	}
	<-overflowStarted

	close(workerRelease)
	closed := make(chan struct{})
	go func() {
		p.close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("pool close returned while an overflow task was still running")
	case <-time.After(50 * time.Millisecond):
	}
	close(overflowRelease)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("pool close did not wait for overflow task completion")
	}
}

// TestPoolOverflowBlock verifies that OverflowBlock applies backpressure:
// the submit returns only once a worker frees a slot.
func TestPoolOverflowBlock(t *testing.T) {
	p := newWorkerPool(1, 1, OverflowBlock)
	defer p.close()

	release := make(chan struct{})
	occupied := make(chan struct{})

	p.submit(func() {
		close(occupied)
		<-release
	})
	<-occupied
	p.submit(func() {}) // queue now full

	blocked := make(chan struct{})
	go func() {
		p.submit(func() {})
		close(blocked)
	}()

	// The submit must still be parked while the worker is held.
	select {
	case <-blocked:
		t.Fatal("submit returned while queue was full")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatal("submit never unblocked after slot freed")
	}
}

// TestPoolSubmitAfterCloseIsRejected covers the closed-pool branch.
func TestPoolSubmitAfterCloseIsRejected(t *testing.T) {
	p := newWorkerPool(1, 1, OverflowSpawn)
	p.close()
	if p.submit(func() { t.Error("task ran on closed pool") }) {
		t.Fatal("submit accepted after close")
	}
}

// TestPoolCloseIsIdempotent guards the doneOnce/closed guards.
func TestPoolCloseIsIdempotent(t *testing.T) {
	p := newWorkerPool(2, 4, OverflowSpawn)
	p.close()
	p.close() // must not panic on a second close of the channels
}

// TestNewWorkerPoolNormalisesArgs covers the <=0 defaults.
func TestNewWorkerPoolNormalisesArgs(t *testing.T) {
	p := newWorkerPool(0, 0, OverflowSpawn)
	defer p.close()
	s := p.stats()
	if s.Workers != 1 {
		t.Fatalf("Workers=%d, want 1", s.Workers)
	}
	if s.Capacity != 1 {
		t.Fatalf("Capacity=%d, want 1 (defaults to worker count)", s.Capacity)
	}
}

// ─── channel internals ────────────────────────────────────────────────────

// TestChannelHas covers channel.has for both outcomes.
func TestChannelHas(t *testing.T) {
	bus := New(Config{})
	defer bus.Close()

	type E struct{}
	sub := OnBus(bus, E{}, func(c *Context, e E) error { return nil })

	ch := busChannel[E](t, bus)
	if !ch.has(sub.id) {
		t.Fatal("has returned false for a registered listener")
	}
	if ch.has(sub.id + 9999) {
		t.Fatal("has returned true for an unknown id")
	}
}

// TestPruneFiredRemovesSpentOnceListeners covers pruneFired.
//
// A spent once-listener is skipped in O(1) during dispatch, so the prune
// is about occupancy: after it runs, the listener should no longer be
// counted.
func TestPruneFiredRemovesSpentOnceListeners(t *testing.T) {
	bus := New(Config{})
	defer bus.Close()

	type E struct{}
	OnceBus(bus, E{}, func(c *Context, e E) error { return nil })
	// A second, permanent listener so the channel is not emptied outright.
	OnBus(bus, E{}, func(c *Context, e E) error { return nil })

	if got := Count[E](bus); got != 2 {
		t.Fatalf("Count=%d before emit, want 2", got)
	}

	_ = EmitBus(bus, E{}) // fires and spends the once-listener

	ch := busChannel[E](t, bus)
	ch.pruneFired()

	if got := Count[E](bus); got != 1 {
		t.Fatalf("Count=%d after prune, want 1", got)
	}

	// Pruning again must be a no-op rather than removing the survivor.
	ch.pruneFired()
	if got := Count[E](bus); got != 1 {
		t.Fatalf("Count=%d after second prune, want 1", got)
	}
}

// busChannel reaches into the bus registry for T's typed channel. Tests
// live in the same package, so this stays within the package boundary.
func busChannel[T any](t *testing.T, b *Bus) *channel[T] {
	t.Helper()
	e := b.lookup(typeKey[T]())
	if e == nil {
		t.Fatal("no registry entry for event type")
	}
	ch, ok := e.erased.(*channel[T])
	if !ok {
		t.Fatalf("registry holds %T, want *channel[T]", e.erased)
	}
	return ch
}

// ─── async error and panic reporting ──────────────────────────────────────

// TestAsyncListenerErrorReachesOnError covers the async error path, which
// cannot surface through a return value.
func TestAsyncListenerErrorReachesOnError(t *testing.T) {
	want := errors.New("async boom")

	var mu sync.Mutex
	var got error
	done := make(chan struct{})

	bus := New(Config{
		OnError: func(ctx *Context, listener string, err error) {
			mu.Lock()
			got = err
			mu.Unlock()
			close(done)
		},
	})
	defer bus.Close()

	type E struct{}
	OnBus(bus, E{}, func(c *Context, e E) error { return want })
	_ = EmitAsyncBus(bus, E{})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("OnError was never called for an async listener")
	}

	mu.Lock()
	defer mu.Unlock()
	if !errors.Is(got, want) {
		t.Fatalf("OnError got %v, want %v", got, want)
	}
}

// TestAsyncStopIsNotReportedAsError verifies Stop stays control flow, not
// a failure, on the async path too.
func TestAsyncStopIsNotReportedAsError(t *testing.T) {
	var reported atomic.Bool
	ran := make(chan struct{})

	bus := New(Config{
		OnError: func(ctx *Context, listener string, err error) {
			reported.Store(true)
		},
	})
	defer bus.Close()

	type E struct{}
	OnBus(bus, E{}, func(c *Context, e E) error {
		close(ran)
		return Stop
	})
	_ = EmitAsyncBus(bus, E{})

	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("async listener never ran")
	}
	// Give any erroneous report a chance to land before asserting.
	time.Sleep(50 * time.Millisecond)
	if reported.Load() {
		t.Fatal("Stop was reported to OnError")
	}
}

// TestEmitAsyncOnClosedBusReturnsErrBusClosed covers the closed guard on
// the async entry point.
func TestEmitAsyncOnClosedBusReturnsErrBusClosed(t *testing.T) {
	bus := New(Config{})
	type E struct{}
	OnBus(bus, E{}, func(c *Context, e E) error { return nil })
	bus.Close()

	if err := EmitAsyncBus(bus, E{}); !errors.Is(err, ErrBusClosed) {
		t.Fatalf("got %v, want ErrBusClosed", err)
	}
}

// TestEmitAsyncWithNoListenersIsNoop covers the empty fast path.
func TestEmitAsyncWithNoListenersIsNoop(t *testing.T) {
	bus := New(Config{})
	defer bus.Close()

	type Unheard struct{}
	if err := EmitAsyncBus(bus, Unheard{}); err != nil {
		t.Fatalf("got %v, want nil", err)
	}
}

// ─── remaining small surfaces ─────────────────────────────────────────────

// TestPhaseString covers the phase stringer, including its default arm.
func TestPhaseString(t *testing.T) {
	tests := []struct {
		p    phase
		want string
	}{
		{phaseBefore, "before"},
		{phaseNormal, "normal"},
		{phaseAfter, "after"},
		{phase(9), "normal"},
	}
	for _, tt := range tests {
		if got := tt.p.String(); got != tt.want {
			t.Errorf("phase(%d).String()=%q, want %q", tt.p, got, tt.want)
		}
	}
}

// TestOffBusUnknownEventType covers the nil-entry arm of OffBus.
func TestOffBusUnknownEventType(t *testing.T) {
	bus := New(Config{})
	defer bus.Close()

	type NeverRegistered struct{}
	if OffBus[NeverRegistered](bus, 1) {
		t.Fatal("OffBus reported a removal for an unregistered type")
	}
}

// TestMetricsForUnknownEventType covers the zero-value return.
func TestMetricsForUnknownEventType(t *testing.T) {
	bus := New(Config{})
	defer bus.Close()

	type NeverRegistered struct{}
	if m := MetricsFor[NeverRegistered](bus); m.Dispatches != 0 {
		t.Fatalf("Dispatches=%d, want 0", m.Dispatches)
	}
}

// TestPoolStatsWithoutPool covers the nil-pool arm.
func TestPoolStatsWithoutPool(t *testing.T) {
	bus := New(Config{Async: AsyncGoroutine})
	defer bus.Close()

	if got := bus.PoolStats(); got.Workers != 0 {
		t.Fatalf("Workers=%d, want 0 for goroutine mode", got.Workers)
	}
}

// TestHasEventUnknownName covers the name-miss arm.
func TestHasEventUnknownName(t *testing.T) {
	bus := New(Config{})
	defer bus.Close()

	if bus.HasEvent("no.such.event") {
		t.Fatal("HasEvent returned true for an unknown name")
	}
}

// TestMetaStringWrongType covers the failed assertion arm.
func TestMetaStringWrongType(t *testing.T) {
	bus := New(Config{})
	defer bus.Close()

	type E struct{}
	OnBus(bus, E{}, func(c *Context, e E) error {
		c.Set("n", 42) // not a string
		if _, ok := c.MetaString("n"); ok {
			t.Error("MetaString succeeded on an int value")
		}
		if _, ok := c.MetaString("absent"); ok {
			t.Error("MetaString succeeded on a missing key")
		}
		return nil
	})
	_ = EmitBus(bus, E{})
}

// TestMetadataEmptyReturnsNil covers the len==0 arm of Metadata.
func TestMetadataEmptyReturnsNil(t *testing.T) {
	bus := New(Config{})
	defer bus.Close()

	type E struct{}
	OnBus(bus, E{}, func(c *Context, e E) error {
		if got := c.Metadata(); got != nil {
			t.Errorf("Metadata()=%v, want nil when unset", got)
		}
		return nil
	})
	_ = EmitBus(bus, E{})
}

// TestDefaultPanicHandlerDoesNotPanic exercises the stderr handler.
func TestDefaultPanicHandlerDoesNotPanic(t *testing.T) {
	// The handler writes to stderr; the contract that matters is that it
	// survives a fully populated PanicError without panicking itself.
	defaultPanicHandler(&PanicError{
		Event:    "TestEvent",
		Listener: "handler",
		Value:    "boom",
		Stack:    []byte("goroutine 1 [running]:\n"),
	})
}
