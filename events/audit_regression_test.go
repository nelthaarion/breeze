package events

// Regression tests for the defects found in the event-bus audit.
//
// Each test here failed against v2.4.0 and passes with the fixes, except the two
// marked "guard", which pin behaviour the fixes must not disturb. Each one names
// the contract it pins (usually a sentence in a doc comment or in
// events/README.md), so a future change that breaks it points at the promise
// that was broken rather than at an implementation detail.

import (
	"errors"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type auditEvent struct{}

func auditNoop(*Context, auditEvent) error { return nil }

// auditObserver counts the callbacks it receives. Start and end are counted
// separately because the bug was an imbalance between them.
type auditObserver struct {
	starts, ends atomic.Int64
}

func (o *auditObserver) OnEventStart(DispatchInfo)     { o.starts.Add(1) }
func (o *auditObserver) OnEventEnd(DispatchResult)     { o.ends.Add(1) }
func (o *auditObserver) OnListenerStart(ListenerCall)  {}
func (o *auditObserver) OnListenerEnd(ListenerOutcome) {}

// ─── once-listeners ───────────────────────────────────────────────────────

// README: "Once listeners: run at most once, then remove automatically."
// pruneFired existed but was never called, so a spent listener stayed in the
// snapshot forever: Count/Has reported it, the closure stayed reachable, and
// every later dispatch walked past it.
func TestAuditOnceListenerIsRemovedAfterFiring(t *testing.T) {
	b := New(quiet())
	defer b.Close()

	sub := OnceBus(b, auditEvent{}, auditNoop)
	if got := Count[auditEvent](b); got != 1 {
		t.Fatalf("Count=%d before the emit, want 1", got)
	}
	if err := EmitBus(b, auditEvent{}); err != nil {
		t.Fatal(err)
	}
	if got := Count[auditEvent](b); got != 0 {
		t.Fatalf("Count=%d after the once-listener fired, want 0", got)
	}
	if Has[auditEvent](b) || b.ListenerCount() != 0 {
		t.Fatal("a spent once-listener is still reported as registered")
	}
	if sub.Active() {
		t.Fatal("Subscription.Active() is true for a once-listener that already ran")
	}
}

func TestAuditOnceListenersDoNotAccumulate(t *testing.T) {
	b := New(quiet())
	defer b.Close()

	const n = 500
	var ran atomic.Int64
	for i := 0; i < n; i++ {
		OnceBus(b, auditEvent{}, func(*Context, auditEvent) error { ran.Add(1); return nil })
	}
	_ = EmitBus(b, auditEvent{})
	if ran.Load() != n {
		t.Fatalf("ran=%d, want %d", ran.Load(), n)
	}
	if got := Count[auditEvent](b); got != 0 {
		t.Fatalf("%d spent once-listeners are still registered", got)
	}
}

func TestAuditOnceListenerIsRemovedAfterAsyncFiring(t *testing.T) {
	b := New(quiet())
	defer b.Close()

	OnceBus(b, auditEvent{}, auditNoop)
	if err := EmitAsyncWaitBus(b, auditEvent{}); err != nil {
		t.Fatal(err)
	}
	if got := Count[auditEvent](b); got != 0 {
		t.Fatalf("Count=%d after an async once-listener fired, want 0", got)
	}
}

// ─── async metrics ────────────────────────────────────────────────────────

// Metrics.Listeners is "the number of listener invocations". The async path
// added the scheduled count in finish() and then 1 more per listener in
// runAsyncListener, so every async invocation was counted twice.
func TestAuditAsyncListenersAreCountedOnce(t *testing.T) {
	b := New(quiet())
	defer b.Close()

	for i := 0; i < 3; i++ {
		OnBus(b, auditEvent{}, auditNoop)
	}
	if err := EmitAsyncWaitBus(b, auditEvent{}); err != nil {
		t.Fatal(err)
	}
	m := MetricsFor[auditEvent](b)
	if m.Dispatches != 1 || m.Listeners != 3 {
		t.Fatalf("Dispatches=%d Listeners=%d, want 1 and 3", m.Dispatches, m.Listeners)
	}
}

// Metrics.Stopped counts *dispatches* halted early. Under async it was bumped
// once per listener that returned Stop and again by finish() whenever the
// emitter happened to observe the cancellation, so the number depended on
// goroutine timing.
func TestAuditAsyncStopIsCountedOncePerDispatch(t *testing.T) {
	b := New(quiet())
	defer b.Close()

	stop := func(*Context, auditEvent) error { return Stop }
	OnBus(b, auditEvent{}, stop)
	OnBus(b, auditEvent{}, stop)
	OnBus(b, auditEvent{}, stop)
	if err := EmitAsyncWaitBus(b, auditEvent{}); err != nil {
		t.Fatal(err)
	}
	if got := MetricsFor[auditEvent](b).Stopped; got != 1 {
		t.Fatalf("Stopped=%d for one stopped dispatch, want 1", got)
	}
}

// A zero-length sample (common where the clock is coarse, e.g. Windows) was
// indistinguishable from "no sample yet", so the next sample overwrote the
// minimum with a larger value.
func TestAuditMinDurationIsNotLostToAZeroSample(t *testing.T) {
	var s eventStats
	s.observe(0, time.Now())
	s.observe(5*time.Millisecond, time.Now())
	if got := s.snapshot().MinDuration; got != 0 {
		t.Fatalf("MinDuration=%v after samples {0, 5ms}, want 0", got)
	}

	var a, c eventStats
	a.observe(0, time.Now())
	c.observe(5*time.Millisecond, time.Now())
	var total Metrics
	total.add(a.snapshot())
	total.add(c.snapshot())
	if total.MinDuration != 0 {
		t.Fatalf("total MinDuration=%v after merging {0} and {5ms}, want 0", total.MinDuration)
	}

	// An event that never dispatched must not drag the minimum to zero.
	var idle eventStats
	var m Metrics
	m.add(c.snapshot())
	m.add(idle.snapshot())
	if m.MinDuration != 5*time.Millisecond {
		t.Fatalf("an idle event changed the total minimum to %v", m.MinDuration)
	}
}

// ─── worker pool lifecycle ────────────────────────────────────────────────

// The pool is created lazily inside sync.Once, but PoolStats (called by the
// diagnostics probe from another goroutine) and Close read b.pool directly.
// Run with -race.
func TestAuditPoolStatsDoesNotRaceWithLazyPoolStart(t *testing.T) {
	b := New(Config{Async: AsyncWorkerPool, Workers: 2, OnPanic: func(*PanicError) {}})
	OnBus(b, auditEvent{}, auditNoop)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = b.PoolStats()
				runtime.Gosched()
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = EmitAsyncBus(b, auditEvent{})
		}
	}()
	time.Sleep(5 * time.Millisecond)
	b.Close() // concurrent with the emitter above
	close(stop)
	wg.Wait()
}

// An emit that passed the closed check before Close ran used to create a
// brand-new pool afterwards, and nothing would ever close it.
func TestAuditNoPoolIsStartedAfterClose(t *testing.T) {
	b := New(Config{Async: AsyncWorkerPool, Workers: 4})
	b.Close()

	before := runtime.NumGoroutine()
	if b.schedule(func() {}) {
		t.Fatal("schedule accepted a task on a closed bus")
	}
	time.Sleep(20 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("goroutines grew from %d to %d: a worker pool was started on a closed bus", before, after)
	}
	if st := b.PoolStats(); st.Workers != 0 {
		t.Fatalf("PoolStats().Workers=%d on a closed bus that never ran async work, want 0", st.Workers)
	}
}

// ─── registry ─────────────────────────────────────────────────────────────

// Name() deleted byName[previousName] unconditionally. If another type had
// since taken that name, renaming the first type silently unregistered the
// second type's name.
func TestAuditRenamingOneTypeKeepsAnotherTypesName(t *testing.T) {
	type a struct{}
	type c struct{}
	b := New(quiet())
	defer b.Close()

	OnBus(b, a{}, func(*Context, a) error { return nil })
	OnBus(b, c{}, func(*Context, c) error { return nil })
	Name[a](b, "shared")
	Name[c](b, "shared") // c takes the name over
	Name[a](b, "renamed")

	if !b.HasEvent("shared") {
		t.Fatal(`HasEvent("shared") is false: renaming another type removed c's name`)
	}
	if !b.HasEvent("renamed") {
		t.Fatal(`HasEvent("renamed") is false`)
	}
}

// compareListeners subtracted priorities, which overflows for extreme values
// and returns the wrong sign. "Any int is accepted" per the docs.
func TestAuditExtremePrioritiesOrderCorrectly(t *testing.T) {
	prios := []int{math.MinInt, 0, math.MaxInt}
	perms := [][]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}}
	for _, perm := range perms {
		b := New(quiet())
		var order []int
		for _, i := range perm {
			p := prios[i]
			OnBus(b, auditEvent{}, func(*Context, auditEvent) error {
				order = append(order, p)
				return nil
			}).Priority(p)
		}
		if err := EmitBus(b, auditEvent{}); err != nil {
			t.Fatal(err)
		}
		want := []int{math.MaxInt, 0, math.MinInt}
		if len(order) != 3 || order[0] != want[0] || order[1] != want[1] || order[2] != want[2] {
			t.Fatalf("registered as %v: ran in order %v, want %v", perm, order, want)
		}
		b.Close()
	}
}

// Subscription.Active is documented as "whether the listener is still
// registered", but it only tracked Unsubscribe.
func TestAuditSubscriptionActiveReflectsRemoval(t *testing.T) {
	b := New(quiet())
	defer b.Close()

	byClear := OnBus(b, auditEvent{}, auditNoop)
	Clear[auditEvent](b)
	if byClear.Active() {
		t.Error("Active() is true after Clear")
	}

	byReset := OnBus(b, auditEvent{}, auditNoop)
	b.Reset()
	if byReset.Active() {
		t.Error("Active() is true after Reset")
	}

	byOff := OnBus(b, auditEvent{}, auditNoop)
	OffBus[auditEvent](b, byOff.ID())
	if byOff.Active() {
		t.Error("Active() is true after OffBus")
	}

	live := OnBus(b, auditEvent{}, auditNoop)
	if !live.Active() {
		t.Error("Active() is false for a registered listener")
	}
	live.Unsubscribe()
	if live.Active() {
		t.Error("Active() is true after Unsubscribe")
	}
}

// register() documents that a nil handler shows up in the inspector as
// "<nil>", but it replaced the handler with a stub *before* deriving the name.
func TestAuditNilHandlerIsNamedNil(t *testing.T) {
	b := New(quiet())
	defer b.Close()

	OnTypeBus[auditEvent](b, nil)
	info := Inspect[auditEvent](b)
	if len(info.Listeners) != 1 || info.Listeners[0].Name != "<nil>" {
		t.Fatalf("listeners=%+v, want one named \"<nil>\"", info.Listeners)
	}
}

// ─── observer pairing ─────────────────────────────────────────────────────

// finish() re-read the observer instead of using the one notifyStart was
// given. Detaching it from inside a listener left the observer with a start
// and no end (observability.EventObserver then keeps that dispatch in its
// in-flight map until the shard overflows).
func TestAuditObserverDetachedMidDispatchStillSeesTheEnd(t *testing.T) {
	b := New(quiet())
	defer b.Close()

	o := &auditObserver{}
	b.SetObserver(o)
	OnBus(b, auditEvent{}, func(*Context, auditEvent) error {
		b.SetObserver(nil)
		return nil
	})
	if err := EmitBus(b, auditEvent{}); err != nil {
		t.Fatal(err)
	}
	if o.starts.Load() != 1 || o.ends.Load() != 1 {
		t.Fatalf("starts=%d ends=%d, want 1 and 1", o.starts.Load(), o.ends.Load())
	}
}

// ...and the mirror image: an observer attached mid-dispatch got an end for a
// dispatch it never saw start.
func TestAuditObserverAttachedMidDispatchDoesNotSeeAnOrphanEnd(t *testing.T) {
	b := New(quiet())
	defer b.Close()

	o := &auditObserver{}
	OnBus(b, auditEvent{}, func(*Context, auditEvent) error {
		b.SetObserver(o)
		return nil
	})
	if err := EmitBus(b, auditEvent{}); err != nil {
		t.Fatal(err)
	}
	if o.starts.Load() != 0 || o.ends.Load() != 0 {
		t.Fatalf("starts=%d ends=%d, want 0 and 0 (the dispatch began before the observer existed)",
			o.starts.Load(), o.ends.Load())
	}
}

// ─── panic containment ────────────────────────────────────────────────────

// errors.go: PanicError "wraps a value recovered from a panicking listener or
// middleware", and the README promises one broken plugin cannot crash the bus.
// Middleware panics were never recovered.
func TestAuditMiddlewarePanicIsContained(t *testing.T) {
	var reported atomic.Int64
	b := New(Config{OnPanic: func(*PanicError) { reported.Add(1) }})
	defer b.Close()

	o := &auditObserver{}
	b.SetObserver(o)
	b.Use(func(*Context, Next) error { panic("middleware bug") })
	OnBus(b, auditEvent{}, auditNoop)

	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("a panicking middleware escaped EmitBus: %v", r)
			}
		}()
		err = EmitBus(b, auditEvent{})
	}()

	var pe *PanicError
	if !errors.As(err, &pe) {
		t.Fatalf("err=%v, want a *PanicError (the dispatch was aborted, the caller must be able to tell)", err)
	}
	if reported.Load() != 1 {
		t.Fatalf("OnPanic called %d times, want 1", reported.Load())
	}
	if m := MetricsFor[auditEvent](b); m.Panics != 1 || m.Dispatches != 1 {
		t.Fatalf("metrics Panics=%d Dispatches=%d, want 1 and 1", m.Panics, m.Dispatches)
	}
	if o.starts.Load() != 1 || o.ends.Load() != 1 {
		t.Fatalf("observer starts=%d ends=%d, want 1 and 1", o.starts.Load(), o.ends.Load())
	}
}

// Guard: asking for PanicPropagate still gets a middleware panic back.
func TestAuditMiddlewarePanicStillPropagatesWhenAskedTo(t *testing.T) {
	b := New(Config{PanicMode: PanicPropagate, OnPanic: func(*PanicError) {}})
	defer b.Close()

	b.Use(func(*Context, Next) error { panic("middleware bug") })
	OnBus(b, auditEvent{}, auditNoop)

	defer func() {
		if recover() == nil {
			t.Fatal("PanicPropagate swallowed a middleware panic")
		}
	}()
	_ = EmitBus(b, auditEvent{})
}

// Sync dispatch recovers a panicking filter (it runs inside invokeListener);
// async evaluated filters on the emitter's goroutine with no recovery at all,
// so one bad filter crashed the caller of EmitAsync.
func TestAuditAsyncPanickingFilterIsContained(t *testing.T) {
	var panics atomic.Int64
	b := New(Config{OnPanic: func(*PanicError) { panics.Add(1) }})
	defer b.Close()

	var ran atomic.Int64
	run := func(*Context, auditEvent) error { ran.Add(1); return nil }
	OnBus(b, auditEvent{}, run).Where(func(auditEvent) bool { panic("bad filter") })
	OnBus(b, auditEvent{}, run)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("a filter panic escaped EmitAsync into the emitting goroutine: %v", r)
		}
	}()
	if err := EmitAsyncWaitBus(b, auditEvent{}); err != nil {
		t.Fatal(err)
	}
	if ran.Load() != 1 {
		t.Fatalf("%d listeners ran, want 1 (the one without the broken filter)", ran.Load())
	}
	if panics.Load() != 1 {
		t.Fatalf("OnPanic called %d times, want 1", panics.Load())
	}
	if got := MetricsFor[auditEvent](b).Panics; got != 1 {
		t.Fatalf("Metrics.Panics=%d, want 1", got)
	}
}

// ─── topics ───────────────────────────────────────────────────────────────

// SubscribeBus registered the listener first and attached the topic filter in
// a second step, so for an instant it was live with no filter and received
// whatever was being published on *any* topic. Subscriptions made at runtime
// while traffic flows are exactly the intended use.
func TestAuditSubscribeNeverReceivesOtherTopics(t *testing.T) {
	b := New(quiet())
	defer b.Close()

	stop := make(chan struct{})
	var pub sync.WaitGroup
	pub.Add(1)
	go func() {
		defer pub.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = PublishBus(b, "other", []byte("x"), nil)
			}
		}
	}()

	var wrong atomic.Int64
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		s := SubscribeBus(b, "mine", func(_ *Context, m RawMessage) error {
			if m.Topic != "mine" {
				wrong.Add(1)
			}
			return nil
		})
		s.Unsubscribe()
	}
	close(stop)
	pub.Wait()

	if n := wrong.Load(); n != 0 {
		t.Fatalf("a subscriber for topic \"mine\" received %d messages for other topics", n)
	}
}

// A nil MessageHandler became a closure that called the nil func, so every
// matching publish panicked (and was recovered and reported) — noise from a
// registration mistake rather than a clear failure at registration.
func TestAuditSubscribeWithNilHandlerIsInert(t *testing.T) {
	var panics atomic.Int64
	b := New(Config{OnPanic: func(*PanicError) { panics.Add(1) }})
	defer b.Close()

	SubscribeBus(b, "t", nil)
	if err := PublishBus(b, "t", nil, nil); err != nil {
		t.Fatal(err)
	}
	if panics.Load() != 0 {
		t.Fatalf("publishing to a nil-handler subscription panicked %d time(s)", panics.Load())
	}
}

// Backend.Close is specified as "Subscriptions made through it stop receiving
// messages". The in-memory implementation was a no-op, so swapping it for a
// network backend (whose Close does stop delivery) changed behaviour.
func TestAuditInMemoryBackendCloseStopsItsSubscriptions(t *testing.T) {
	bus := New(quiet())
	defer bus.Close()
	be := NewInMemoryBackend(bus)

	var got atomic.Int64
	if _, err := be.Subscribe("t", func(*Context, RawMessage) error { got.Add(1); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := PublishBus(bus, "t", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got.Load() != 1 {
		t.Fatalf("got=%d before Close, want 1", got.Load())
	}

	if err := be.Close(); err != nil {
		t.Fatal(err)
	}
	if err := PublishBus(bus, "t", nil, nil); err != nil {
		t.Fatal(err) // the bus itself keeps working
	}
	if got.Load() != 1 {
		t.Fatal("a subscription made through a closed backend still received a message")
	}
	if _, err := be.Subscribe("t", func(*Context, RawMessage) error { return nil }); err == nil {
		t.Fatal("Subscribe on a closed backend succeeded")
	}
	if err := be.Publish("t", nil, nil); err == nil {
		t.Fatal("Publish on a closed backend succeeded")
	}
}

// Guard: PanicPropagate must stay a pass-through. A listener panic is reported
// by invokeListener before it re-panics, and the middleware recovery added for
// the other modes must not catch it a second time and report it again.
func TestAuditPanicPropagateReportsAListenerPanicOnce(t *testing.T) {
	var reported atomic.Int64
	b := New(Config{PanicMode: PanicPropagate, OnPanic: func(*PanicError) { reported.Add(1) }})
	defer b.Close()

	b.Use(func(_ *Context, next Next) error { return next() })
	OnBus(b, auditEvent{}, func(*Context, auditEvent) error { panic("listener bug") })

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("PanicPropagate swallowed a listener panic")
			}
		}()
		_ = EmitBus(b, auditEvent{})
	}()
	if got := reported.Load(); got != 1 {
		t.Fatalf("OnPanic called %d times for one panic, want 1", got)
	}
	if got := MetricsFor[auditEvent](b).Panics; got != 1 {
		t.Fatalf("Metrics.Panics=%d for one panic, want 1", got)
	}
}

// A once-listener claimed by an async emit that the pool then rejects is rolled
// back (fired=false) so it can fire later. If another dispatch sweeps "every
// fired listener" in the window between the claim and the rollback, the
// listener is removed and then un-fired: lost for good. A dispatch therefore
// removes only the once-listeners it claimed itself, never one it merely found
// already claimed.
func TestAuditDispatchDoesNotRemoveAnotherDispatchsClaim(t *testing.T) {
	for name, emit := range map[string]func(*Bus) error{
		"sync":  func(b *Bus) error { return EmitBus(b, auditEvent{}) },
		"async": func(b *Bus) error { return EmitAsyncWaitBus(b, auditEvent{}) },
	} {
		t.Run(name, func(t *testing.T) {
			b := New(quiet())
			defer b.Close()

			var fired atomic.Int64
			OnceBus(b, auditEvent{}, func(*Context, auditEvent) error { fired.Add(1); return nil })
			ch := channelFor[auditEvent](b)
			once := ch.load()[0]

			once.fired.Store(true)          // another dispatch has claimed it, not yet scheduled
			if err := emit(b); err != nil { // walks past it
				t.Fatal(err)
			}
			if !ch.has(once.id) {
				t.Fatal("a dispatch that only walked past another dispatch's claim removed the listener")
			}

			once.fired.Store(false) // the claimant's rollback
			if err := emit(b); err != nil {
				t.Fatal(err)
			}
			if fired.Load() != 1 {
				t.Fatalf("the once-listener ran %d times after its claim was rolled back, want 1", fired.Load())
			}
			if ch.has(once.id) {
				t.Fatal("the dispatch that ran the once-listener did not remove it")
			}
		})
	}
}
