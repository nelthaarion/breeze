package events

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Test events. Distinct types keep tests independent of one another,
// since registrations are keyed by type.
type (
	testEvent  struct{ N int }
	otherEvent struct{ S string }
	orderEvent struct{}
	asyncEvent struct{ N int }
	panicEvent struct{}
	errEvent   struct{}
	filterEven struct{ N int }
	onceEvent  struct{}
	metaEvent  struct{}
	stopEvent  struct{}
)

// quiet returns a Config with the default stderr panic handler replaced,
// so tests that exercise panics do not spam the test log.
func quiet() Config {
	return Config{OnPanic: func(*PanicError) {}}
}

func TestEmitCallsListener(t *testing.T) {
	b := New()
	var got int
	OnBus(b, testEvent{}, func(_ *Context, e testEvent) error {
		got = e.N
		return nil
	})

	if err := EmitBus(b, testEvent{N: 42}); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if got != 42 {
		t.Fatalf("listener got N=%d, want 42", got)
	}
}

func TestEmitNoListeners(t *testing.T) {
	b := New()
	if err := EmitBus(b, testEvent{N: 1}); err != nil {
		t.Fatalf("emit with no listeners: %v", err)
	}
}

func TestEmitIsTypeScoped(t *testing.T) {
	b := New()
	var a, o int
	OnBus(b, testEvent{}, func(*Context, testEvent) error { a++; return nil })
	OnBus(b, otherEvent{}, func(*Context, otherEvent) error { o++; return nil })

	EmitBus(b, testEvent{})
	if a != 1 || o != 0 {
		t.Fatalf("cross-type delivery: a=%d o=%d, want 1,0", a, o)
	}
}

func TestMultipleListenersAllRun(t *testing.T) {
	b := New()
	var n atomic.Int64
	for i := 0; i < 10; i++ {
		OnBus(b, testEvent{}, func(*Context, testEvent) error { n.Add(1); return nil })
	}
	EmitBus(b, testEvent{})
	if n.Load() != 10 {
		t.Fatalf("ran %d listeners, want 10", n.Load())
	}
}

func TestPriorityOrder(t *testing.T) {
	b := New()
	var order []string

	OnBus(b, orderEvent{}, func(*Context, orderEvent) error {
		order = append(order, "low")
		return nil
	}).Priority(PriorityLow)

	OnBus(b, orderEvent{}, func(*Context, orderEvent) error {
		order = append(order, "high")
		return nil
	}).Priority(PriorityHigh)

	OnBus(b, orderEvent{}, func(*Context, orderEvent) error {
		order = append(order, "normal")
		return nil
	}).Priority(PriorityNormal)

	EmitBus(b, orderEvent{})

	want := "high,normal,low"
	if got := strings.Join(order, ","); got != want {
		t.Fatalf("order = %q, want %q", got, want)
	}
}

func TestEqualPriorityKeepsRegistrationOrder(t *testing.T) {
	b := New()
	var order []int
	for i := 0; i < 5; i++ {
		i := i
		OnBus(b, orderEvent{}, func(*Context, orderEvent) error {
			order = append(order, i)
			return nil
		})
	}
	EmitBus(b, orderEvent{})

	for i, v := range order {
		if v != i {
			t.Fatalf("order = %v, want ascending", order)
		}
	}
}

func TestBeforeAndAfterPhases(t *testing.T) {
	b := New()
	var order []string

	// The normal listener is given the highest priority to prove that
	// phase dominates priority.
	OnBus(b, orderEvent{}, func(*Context, orderEvent) error {
		order = append(order, "normal")
		return nil
	}).Priority(PriorityHighest)

	AfterBus(b, orderEvent{}, func(*Context, orderEvent) error {
		order = append(order, "after")
		return nil
	})

	BeforeBus(b, orderEvent{}, func(*Context, orderEvent) error {
		order = append(order, "before")
		return nil
	}).Priority(PriorityLowest)

	EmitBus(b, orderEvent{})

	want := "before,normal,after"
	if got := strings.Join(order, ","); got != want {
		t.Fatalf("phase order = %q, want %q", got, want)
	}
}

func TestUnsubscribe(t *testing.T) {
	b := New()
	var n int
	sub := OnBus(b, testEvent{}, func(*Context, testEvent) error { n++; return nil })

	EmitBus(b, testEvent{})
	sub.Unsubscribe()
	EmitBus(b, testEvent{})

	if n != 1 {
		t.Fatalf("listener ran %d times, want 1", n)
	}
	if sub.Active() {
		t.Fatal("Active() true after Unsubscribe")
	}
	if Count[testEvent](b) != 0 {
		t.Fatalf("Count = %d, want 0", Count[testEvent](b))
	}
}

func TestUnsubscribeIsIdempotent(t *testing.T) {
	b := New()
	sub := OnBus(b, testEvent{}, func(*Context, testEvent) error { return nil })
	sub.Unsubscribe()
	sub.Unsubscribe() // must not panic
}

func TestSelfUnsubscribeDuringDispatch(t *testing.T) {
	b := New()
	var n int
	var sub *Subscription[testEvent]
	sub = OnBus(b, testEvent{}, func(*Context, testEvent) error {
		n++
		sub.Unsubscribe()
		return nil
	})

	EmitBus(b, testEvent{})
	EmitBus(b, testEvent{})

	if n != 1 {
		t.Fatalf("ran %d times, want 1", n)
	}
}

func TestOnceRunsOnlyOnce(t *testing.T) {
	b := New()
	var n atomic.Int64
	OnceBus(b, onceEvent{}, func(*Context, onceEvent) error { n.Add(1); return nil })

	for i := 0; i < 5; i++ {
		EmitBus(b, onceEvent{})
	}
	if n.Load() != 1 {
		t.Fatalf("once listener ran %d times, want 1", n.Load())
	}
}

func TestOnceUnderConcurrentEmit(t *testing.T) {
	b := New()
	var n atomic.Int64
	OnceBus(b, onceEvent{}, func(*Context, onceEvent) error { n.Add(1); return nil })

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); EmitBus(b, onceEvent{}) }()
	}
	wg.Wait()

	if n.Load() != 1 {
		t.Fatalf("once listener ran %d times under concurrency, want 1", n.Load())
	}
}

func TestFilterSkipsNonMatching(t *testing.T) {
	b := New()
	var seen []int
	OnBus(b, filterEven{}, func(_ *Context, e filterEven) error {
		seen = append(seen, e.N)
		return nil
	}).Where(func(e filterEven) bool { return e.N%2 == 0 })

	for i := 1; i <= 6; i++ {
		EmitBus(b, filterEven{N: i})
	}

	want := "2,4,6"
	got := strings.Trim(strings.Join(strings.Fields(fmt.Sprint(seen)), ","), "[]")
	if got != want {
		t.Fatalf("filtered events = %q, want %q", got, want)
	}
}

func TestStopHaltsPropagation(t *testing.T) {
	b := New()
	var ran []string

	OnBus(b, stopEvent{}, func(*Context, stopEvent) error {
		ran = append(ran, "first")
		return nil
	}).Priority(PriorityHigh)

	OnBus(b, stopEvent{}, func(*Context, stopEvent) error {
		ran = append(ran, "stopper")
		return Stop
	})

	OnBus(b, stopEvent{}, func(*Context, stopEvent) error {
		ran = append(ran, "never")
		return nil
	}).Priority(PriorityLow)

	// Stop is normal control flow, so Emit reports success.
	if err := EmitBus(b, stopEvent{}); err != nil {
		t.Fatalf("Emit returned %v, want nil for Stop", err)
	}
	want := "first,stopper"
	if got := strings.Join(ran, ","); got != want {
		t.Fatalf("ran %q, want %q", got, want)
	}
}

func TestContextCancelHaltsPropagation(t *testing.T) {
	b := New()
	var ran []string

	OnBus(b, stopEvent{}, func(ctx *Context, _ stopEvent) error {
		ran = append(ran, "canceller")
		ctx.Cancel()
		return nil
	}).Priority(PriorityHigh)

	OnBus(b, stopEvent{}, func(*Context, stopEvent) error {
		ran = append(ran, "never")
		return nil
	})

	EmitBus(b, stopEvent{})

	if got := strings.Join(ran, ","); got != "canceller" {
		t.Fatalf("ran %q, want %q", got, "canceller")
	}
}

func TestErrorStopsDispatchByDefault(t *testing.T) {
	b := New()
	boom := errors.New("boom")
	var ran int

	OnBus(b, errEvent{}, func(*Context, errEvent) error {
		ran++
		return boom
	}).Priority(PriorityHigh)

	OnBus(b, errEvent{}, func(*Context, errEvent) error {
		ran++
		return nil
	})

	err := EmitBus(b, errEvent{})
	if !errors.Is(err, boom) {
		t.Fatalf("Emit err = %v, want %v", err, boom)
	}
	if ran != 1 {
		t.Fatalf("ran %d listeners, want 1", ran)
	}
}

func TestContinueOnErrorAggregates(t *testing.T) {
	b := New(Config{ContinueOnError: true})
	e1 := errors.New("one")
	e2 := errors.New("two")
	var ran int

	OnBus(b, errEvent{}, func(*Context, errEvent) error { ran++; return e1 }).Priority(PriorityHigh)
	OnBus(b, errEvent{}, func(*Context, errEvent) error { ran++; return nil })
	OnBus(b, errEvent{}, func(*Context, errEvent) error { ran++; return e2 }).Priority(PriorityLow)

	err := EmitBus(b, errEvent{})
	if err == nil {
		t.Fatal("want aggregate error, got nil")
	}
	if ran != 3 {
		t.Fatalf("ran %d listeners, want 3", ran)
	}
	if !errors.Is(err, e1) || !errors.Is(err, e2) {
		t.Fatalf("aggregate %v does not wrap both causes", err)
	}
}

func TestPanicRecoveredAndContinues(t *testing.T) {
	b := New(quiet())
	var after bool

	OnBus(b, panicEvent{}, func(*Context, panicEvent) error {
		panic("listener exploded")
	}).Priority(PriorityHigh)

	OnBus(b, panicEvent{}, func(*Context, panicEvent) error {
		after = true
		return nil
	})

	if err := EmitBus(b, panicEvent{}); err != nil {
		t.Fatalf("Emit err = %v, want nil under PanicRecoverAndContinue", err)
	}
	if !after {
		t.Fatal("listener after the panicking one did not run")
	}
}

func TestPanicHandlerReceivesDetails(t *testing.T) {
	var pe *PanicError
	b := New(Config{OnPanic: func(p *PanicError) { pe = p }})

	OnBus(b, panicEvent{}, func(*Context, panicEvent) error {
		panic("kaboom")
	})
	EmitBus(b, panicEvent{})

	if pe == nil {
		t.Fatal("panic handler not called")
	}
	if pe.Value != "kaboom" {
		t.Fatalf("Value = %v, want kaboom", pe.Value)
	}
	if len(pe.Stack) == 0 {
		t.Fatal("Stack is empty")
	}
}

func TestPanicRecoverAndFail(t *testing.T) {
	b := New(Config{PanicMode: PanicRecoverAndFail, OnPanic: func(*PanicError) {}})
	OnBus(b, panicEvent{}, func(*Context, panicEvent) error { panic("nope") })

	err := EmitBus(b, panicEvent{})
	var pe *PanicError
	if !errors.As(err, &pe) {
		t.Fatalf("Emit err = %v, want *PanicError", err)
	}
}

func TestContextMetadataSharedAcrossListeners(t *testing.T) {
	b := New()

	OnBus(b, metaEvent{}, func(ctx *Context, _ metaEvent) error {
		ctx.Set("user", "alice")
		return nil
	}).Priority(PriorityHigh)

	var seen string
	OnBus(b, metaEvent{}, func(ctx *Context, _ metaEvent) error {
		if v, ok := ctx.MetaString("user"); ok {
			seen = v
		}
		return nil
	})

	EmitBus(b, metaEvent{})
	if seen != "alice" {
		t.Fatalf("metadata = %q, want alice", seen)
	}
}

func TestGetMetaTypedAndMissing(t *testing.T) {
	b := New()
	var okInt, wrongType, missing bool

	OnBus(b, metaEvent{}, func(ctx *Context, _ metaEvent) error {
		ctx.Set("n", 7)
		_, okInt = GetMeta[int](ctx, "n")
		_, wrongType = GetMeta[string](ctx, "n")
		_, missing = GetMeta[int](ctx, "absent")
		return nil
	})
	EmitBus(b, metaEvent{})

	if !okInt {
		t.Error("GetMeta[int] failed for an int value")
	}
	if wrongType {
		t.Error("GetMeta[string] succeeded for an int value")
	}
	if missing {
		t.Error("GetMeta succeeded for an absent key")
	}
}

func TestContextFieldsPopulated(t *testing.T) {
	b := New()
	Name[testEvent](b, "test.event")

	// The Context is pooled, so its fields are inspected inside the
	// listener rather than retained past the dispatch.
	var id uint64
	var name string
	var haveTime, haveCtx bool

	OnBus(b, testEvent{}, func(ctx *Context, _ testEvent) error {
		id = ctx.EventID
		name = ctx.EventName
		haveTime = !ctx.Time.IsZero()
		haveCtx = ctx.Ctx != nil
		return nil
	})
	EmitBus(b, testEvent{})

	if id == 0 {
		t.Error("EventID is zero")
	}
	if name != "test.event" {
		t.Errorf("EventName = %q, want test.event", name)
	}
	if !haveTime {
		t.Error("Time is zero")
	}
	if !haveCtx {
		t.Error("Ctx is nil")
	}
}

func TestEventIDsAreUnique(t *testing.T) {
	b := New()
	seen := make(map[uint64]bool)
	var mu sync.Mutex

	OnBus(b, testEvent{}, func(ctx *Context, _ testEvent) error {
		mu.Lock()
		defer mu.Unlock()
		if seen[ctx.EventID] {
			t.Errorf("duplicate EventID %d", ctx.EventID)
		}
		seen[ctx.EventID] = true
		return nil
	})

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); EmitBus(b, testEvent{}) }()
	}
	wg.Wait()

	if len(seen) != 100 {
		t.Fatalf("saw %d distinct EventIDs, want 100", len(seen))
	}
}

func TestPooledContextIsReusedCleanly(t *testing.T) {
	b := New()
	// Metadata written by one dispatch must not leak into the next,
	// which is the failure mode a pooled Context invites.
	var leaked bool
	OnBus(b, metaEvent{}, func(ctx *Context, _ metaEvent) error {
		if _, ok := ctx.Get("carry"); ok {
			leaked = true
		}
		ctx.Set("carry", true)
		return nil
	})

	EmitBus(b, metaEvent{})
	EmitBus(b, metaEvent{})

	if leaked {
		t.Fatal("metadata leaked between dispatches")
	}
}

func TestEmitCtxPropagatesStdContext(t *testing.T) {
	b := New()
	type ctxKey string
	key := ctxKey("k")
	parent := context.WithValue(context.Background(), key, "v")

	var got any
	OnBus(b, testEvent{}, func(ctx *Context, _ testEvent) error {
		got = ctx.Ctx.Value(key)
		return nil
	})
	EmitCtxBus(b, parent, testEvent{})

	if got != "v" {
		t.Fatalf("context value = %v, want v", got)
	}
}

func TestMiddlewareWrapsDispatch(t *testing.T) {
	b := New()
	var order []string

	b.Use(func(ctx *Context, next Next) error {
		order = append(order, "outer:before")
		err := next()
		order = append(order, "outer:after")
		return err
	})
	b.Use(func(ctx *Context, next Next) error {
		order = append(order, "inner:before")
		err := next()
		order = append(order, "inner:after")
		return err
	})

	OnBus(b, testEvent{}, func(*Context, testEvent) error {
		order = append(order, "listener")
		return nil
	})
	EmitBus(b, testEvent{})

	want := "outer:before,inner:before,listener,inner:after,outer:after"
	if got := strings.Join(order, ","); got != want {
		t.Fatalf("middleware order = %q, want %q", got, want)
	}
}

func TestMiddlewareCanAbortDispatch(t *testing.T) {
	b := New()
	blocked := errors.New("blocked")
	var ran bool

	b.Use(func(*Context, Next) error { return blocked })
	OnBus(b, testEvent{}, func(*Context, testEvent) error { ran = true; return nil })

	if err := EmitBus(b, testEvent{}); !errors.Is(err, blocked) {
		t.Fatalf("Emit err = %v, want %v", err, blocked)
	}
	if ran {
		t.Fatal("listener ran despite middleware abort")
	}
}

func TestMiddlewareRunsWithoutListeners(t *testing.T) {
	b := New()
	var ran bool
	b.Use(func(_ *Context, next Next) error { ran = true; return next() })

	EmitBus(b, testEvent{})
	if !ran {
		t.Fatal("middleware skipped for an event with no listeners")
	}
}

func TestAsyncEmitRunsListeners(t *testing.T) {
	b := New()
	var n atomic.Int64
	var wg sync.WaitGroup
	wg.Add(3)

	for i := 0; i < 3; i++ {
		OnBus(b, asyncEvent{}, func(*Context, asyncEvent) error {
			n.Add(1)
			wg.Done()
			return nil
		})
	}

	if err := EmitAsyncBus(b, asyncEvent{}); err != nil {
		t.Fatalf("EmitAsync: %v", err)
	}
	wg.Wait()

	if n.Load() != 3 {
		t.Fatalf("ran %d listeners, want 3", n.Load())
	}
}

func TestEmitAsyncWaitBlocksUntilDone(t *testing.T) {
	b := New()
	var n atomic.Int64
	for i := 0; i < 5; i++ {
		OnBus(b, asyncEvent{}, func(*Context, asyncEvent) error {
			time.Sleep(time.Millisecond)
			n.Add(1)
			return nil
		})
	}

	if err := EmitAsyncWaitBus(b, asyncEvent{}); err != nil {
		t.Fatalf("EmitAsyncWait: %v", err)
	}
	if n.Load() != 5 {
		t.Fatalf("ran %d listeners after wait, want 5", n.Load())
	}
}

func TestAsyncWorkerPoolMode(t *testing.T) {
	b := New(Config{Async: AsyncWorkerPool, Workers: 4})
	defer b.Close()

	var n atomic.Int64
	for i := 0; i < 20; i++ {
		OnBus(b, asyncEvent{}, func(*Context, asyncEvent) error { n.Add(1); return nil })
	}

	EmitAsyncWaitBus(b, asyncEvent{})
	if n.Load() != 20 {
		t.Fatalf("pool ran %d listeners, want 20", n.Load())
	}
}

func TestAsyncPanicIsContained(t *testing.T) {
	b := New(quiet())
	var ok atomic.Bool

	OnBus(b, panicEvent{}, func(*Context, panicEvent) error { panic("async boom") })
	OnBus(b, panicEvent{}, func(*Context, panicEvent) error { ok.Store(true); return nil })

	EmitAsyncWaitBus(b, panicEvent{})
	if !ok.Load() {
		t.Fatal("sibling listener did not run after an async panic")
	}
}

func TestAsyncOnceFiresOnce(t *testing.T) {
	b := New()
	var n atomic.Int64
	OnceBus(b, onceEvent{}, func(*Context, onceEvent) error { n.Add(1); return nil })

	for i := 0; i < 5; i++ {
		EmitAsyncWaitBus(b, onceEvent{})
	}
	if n.Load() != 1 {
		t.Fatalf("async once ran %d times, want 1", n.Load())
	}
}

func TestAsyncFilterApplies(t *testing.T) {
	b := New()
	var n atomic.Int64
	OnBus(b, asyncEvent{}, func(*Context, asyncEvent) error {
		n.Add(1)
		return nil
	}).Where(func(e asyncEvent) bool { return e.N > 10 })

	EmitAsyncWaitBus(b, asyncEvent{N: 1})
	EmitAsyncWaitBus(b, asyncEvent{N: 20})

	if n.Load() != 1 {
		t.Fatalf("filtered async ran %d times, want 1", n.Load())
	}
}

func TestRegistryAndNaming(t *testing.T) {
	b := New()
	Name[testEvent](b, "my.event")

	if got := NameOf[testEvent](b); got != "my.event" {
		t.Fatalf("NameOf = %q, want my.event", got)
	}
	if b.HasEvent("my.event") {
		t.Fatal("HasEvent true before any listener")
	}

	OnBus(b, testEvent{}, func(*Context, testEvent) error { return nil })

	if !Has[testEvent](b) {
		t.Fatal("Has = false after registration")
	}
	if !b.HasEvent("my.event") {
		t.Fatal("HasEvent = false after registration")
	}
	if b.ListenerCount() != 1 {
		t.Fatalf("ListenerCount = %d, want 1", b.ListenerCount())
	}

	names := b.EventNames()
	if len(names) != 1 || names[0] != "my.event" {
		t.Fatalf("EventNames = %v, want [my.event]", names)
	}
}

func TestNameOfUnregisteredFallsBackToType(t *testing.T) {
	b := New()
	if got := NameOf[testEvent](b); !strings.Contains(got, "testEvent") {
		t.Fatalf("NameOf = %q, want the Go type name", got)
	}
}

func TestClearRemovesListeners(t *testing.T) {
	b := New()
	OnBus(b, testEvent{}, func(*Context, testEvent) error { return nil })
	OnBus(b, testEvent{}, func(*Context, testEvent) error { return nil })

	Clear[testEvent](b)
	if Count[testEvent](b) != 0 {
		t.Fatalf("Count = %d after Clear, want 0", Count[testEvent](b))
	}
}

func TestInspectReportsListeners(t *testing.T) {
	b := New()
	Name[orderEvent](b, "inspect.me")

	OnBus(b, orderEvent{}, func(*Context, orderEvent) error { return nil }).
		Priority(PriorityHigh).Named("first")
	OnceBus(b, orderEvent{}, func(*Context, orderEvent) error { return nil }).Named("once")
	BeforeBus(b, orderEvent{}, func(*Context, orderEvent) error { return nil }).Named("pre")
	OnBus(b, orderEvent{}, func(*Context, orderEvent) error { return nil }).
		Where(func(orderEvent) bool { return true }).Named("filtered")

	// Inspect before the emit: a once-listener is removed as soon as it has
	// fired ("run at most once, then remove automatically"), so listing all
	// four registrations is only possible while it is still pending.
	info := Inspect[orderEvent](b)
	if info.Name != "inspect.me" {
		t.Errorf("Name = %q, want inspect.me", info.Name)
	}
	if info.ListenerCount != 4 {
		t.Fatalf("ListenerCount = %d, want 4", info.ListenerCount)
	}
	if info.Listeners[0].Name != "pre" || info.Listeners[0].Phase != "before" {
		t.Errorf("first listener = %+v, want the before-hook", info.Listeners[0])
	}
	for i, l := range info.Listeners {
		if l.Order != i {
			t.Errorf("listener %d has Order %d", i, l.Order)
		}
	}

	EmitBus(b, orderEvent{})

	info = Inspect[orderEvent](b)
	if info.Metrics.Dispatches != 1 {
		t.Errorf("Dispatches = %d, want 1", info.Metrics.Dispatches)
	}
	if info.ListenerCount != 3 {
		t.Errorf("ListenerCount = %d after the emit, want 3 (the once-listener has fired and been removed)", info.ListenerCount)
	}
	for _, l := range info.Listeners {
		if l.Name == "once" {
			t.Errorf("the spent once-listener is still listed: %+v", l)
		}
	}
}

func TestInspectUnregisteredEvent(t *testing.T) {
	b := New()
	info := Inspect[testEvent](b)
	if info.ListenerCount != 0 || len(info.Listeners) != 0 {
		t.Fatalf("unregistered inspect = %+v, want empty", info)
	}
}

func TestMetricsAccumulate(t *testing.T) {
	b := New()
	// The sleep makes the measured duration exceed the wall clock's
	// granularity, which on Windows is far coarser than a nanosecond and
	// would otherwise round a trivial dispatch down to zero.
	OnBus(b, testEvent{}, func(*Context, testEvent) error {
		time.Sleep(time.Millisecond)
		return nil
	})
	OnBus(b, testEvent{}, func(*Context, testEvent) error { return errors.New("x") })

	for i := 0; i < 3; i++ {
		EmitBus(b, testEvent{})
	}

	m := MetricsFor[testEvent](b)
	if m.Dispatches != 3 {
		t.Errorf("Dispatches = %d, want 3", m.Dispatches)
	}
	if m.Failures != 3 {
		t.Errorf("Failures = %d, want 3", m.Failures)
	}
	if m.AvgDuration <= 0 {
		t.Error("AvgDuration is not positive")
	}
	if m.LastDispatch.IsZero() {
		t.Error("LastDispatch is zero")
	}

	total := b.TotalMetrics()
	if total.Dispatches != 3 {
		t.Errorf("TotalMetrics.Dispatches = %d, want 3", total.Dispatches)
	}
}

func TestMetricsCountPanicsAndStops(t *testing.T) {
	b := New(quiet())
	OnBus(b, panicEvent{}, func(*Context, panicEvent) error { panic("x") })
	EmitBus(b, panicEvent{})

	if m := MetricsFor[panicEvent](b); m.Panics != 1 {
		t.Errorf("Panics = %d, want 1", m.Panics)
	}

	OnBus(b, stopEvent{}, func(*Context, stopEvent) error { return Stop })
	EmitBus(b, stopEvent{})

	if m := MetricsFor[stopEvent](b); m.Stopped != 1 {
		t.Errorf("Stopped = %d, want 1", m.Stopped)
	}
}

func TestMetricsDisabled(t *testing.T) {
	b := New(Config{DisableMetrics: true})
	OnBus(b, testEvent{}, func(*Context, testEvent) error { return nil })
	EmitBus(b, testEvent{})

	if m := MetricsFor[testEvent](b); m.Dispatches != 0 {
		t.Fatalf("Dispatches = %d with metrics disabled, want 0", m.Dispatches)
	}
}

func TestRecorderCapturesHistory(t *testing.T) {
	b := New()
	b.EnableRecorder()
	Name[testEvent](b, "rec.event")
	OnBus(b, testEvent{}, func(*Context, testEvent) error { return nil })

	EmitBus(b, testEvent{N: 1})
	EmitBus(b, testEvent{N: 2})

	h := b.RecorderHistory()
	if len(h) != 2 {
		t.Fatalf("history has %d records, want 2", len(h))
	}
	if h[0].Name != "rec.event" {
		t.Errorf("Name = %q, want rec.event", h[0].Name)
	}
	if h[0].Listeners != 1 {
		t.Errorf("Listeners = %d, want 1", h[0].Listeners)
	}
	if h[0].EventID >= h[1].EventID {
		t.Error("EventIDs are not increasing")
	}
	if h[0].Payload != nil {
		t.Error("payload retained without EnableRecorderWithPayload")
	}
}

func TestRecorderPayloadCapture(t *testing.T) {
	b := New()
	b.EnableRecorderWithPayload()
	OnBus(b, testEvent{}, func(*Context, testEvent) error { return nil })
	EmitBus(b, testEvent{N: 99})

	h := b.RecorderHistory()
	ev, ok := h[0].Payload.(testEvent)
	if !ok || ev.N != 99 {
		t.Fatalf("Payload = %#v, want testEvent{N:99}", h[0].Payload)
	}
}

func TestRecorderRingBufferEvicts(t *testing.T) {
	b := New(Config{RecorderSize: 3})
	b.EnableRecorder()
	OnBus(b, testEvent{}, func(*Context, testEvent) error { return nil })

	for i := 0; i < 10; i++ {
		EmitBus(b, testEvent{N: i})
	}

	h := b.RecorderHistory()
	if len(h) != 3 {
		t.Fatalf("history has %d records, want 3 (capacity)", len(h))
	}

	st := b.RecorderStats()
	if st.Total != 10 {
		t.Errorf("Total = %d, want 10", st.Total)
	}
	if st.Capacity != 3 || st.Size != 3 {
		t.Errorf("stats = %+v, want size and capacity 3", st)
	}
	// The retained window must be the most recent one.
	if h[0].EventID >= h[2].EventID {
		t.Error("retained records are out of order")
	}
}

func TestRecorderDisabledByDefault(t *testing.T) {
	b := New()
	OnBus(b, testEvent{}, func(*Context, testEvent) error { return nil })
	EmitBus(b, testEvent{})

	if len(b.RecorderHistory()) != 0 {
		t.Fatal("recorder captured events while disabled")
	}
}

func TestRecorderDisableAndClear(t *testing.T) {
	b := New()
	b.EnableRecorder()
	OnBus(b, testEvent{}, func(*Context, testEvent) error { return nil })
	EmitBus(b, testEvent{})

	b.DisableRecorder()
	EmitBus(b, testEvent{})
	if len(b.RecorderHistory()) != 1 {
		t.Fatal("recorder kept capturing after DisableRecorder")
	}

	b.ClearRecorderHistory()
	if len(b.RecorderHistory()) != 0 {
		t.Fatal("history not cleared")
	}
}

func TestCloseRejectsEmit(t *testing.T) {
	b := New()
	OnBus(b, testEvent{}, func(*Context, testEvent) error { return nil })
	b.Close()

	if err := EmitBus(b, testEvent{}); !errors.Is(err, ErrBusClosed) {
		t.Fatalf("Emit after Close = %v, want ErrBusClosed", err)
	}
	if err := EmitAsyncBus(b, testEvent{}); !errors.Is(err, ErrBusClosed) {
		t.Fatalf("EmitAsync after Close = %v, want ErrBusClosed", err)
	}
	if !b.Closed() {
		t.Fatal("Closed() = false after Close")
	}
	b.Close() // idempotent
}

func TestRegistrationOnClosedBusIsInert(t *testing.T) {
	b := New()
	b.Close()

	sub := OnBus(b, testEvent{}, func(*Context, testEvent) error { return nil })
	if sub.Active() {
		t.Fatal("subscription active on a closed bus")
	}
	if Count[testEvent](b) != 0 {
		t.Fatal("listener registered on a closed bus")
	}
}

func TestResetClearsEverything(t *testing.T) {
	b := New()
	b.EnableRecorder()
	b.Use(func(_ *Context, next Next) error { return next() })
	OnBus(b, testEvent{}, func(*Context, testEvent) error { return nil })
	EmitBus(b, testEvent{})

	b.Reset()

	if b.ListenerCount() != 0 {
		t.Error("listeners survived Reset")
	}
	if len(b.RecorderHistory()) != 0 {
		t.Error("history survived Reset")
	}
}

func TestNilHandlerIsSafe(t *testing.T) {
	b := New()
	OnTypeBus[testEvent](b, nil)
	if err := EmitBus(b, testEvent{}); err != nil {
		t.Fatalf("emit with a nil handler: %v", err)
	}
}

func TestOffByID(t *testing.T) {
	b := New()
	sub := OnBus(b, testEvent{}, func(*Context, testEvent) error { return nil })

	if !OffBus[testEvent](b, sub.ID()) {
		t.Fatal("OffBus reported no removal")
	}
	if OffBus[testEvent](b, sub.ID()) {
		t.Fatal("OffBus removed the same listener twice")
	}
	if Count[testEvent](b) != 0 {
		t.Fatal("listener still registered")
	}
}

func TestPackageLevelAPI(t *testing.T) {
	t.Cleanup(Reset)

	var got int
	sub := On(testEvent{}, func(_ *Context, e testEvent) error {
		got = e.N
		return nil
	})
	defer sub.Unsubscribe()

	if err := Emit(testEvent{N: 5}); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if got != 5 {
		t.Fatalf("got %d, want 5", got)
	}
	if CountOf[testEvent]() != 1 {
		t.Fatalf("CountOf = %d, want 1", CountOf[testEvent]())
	}
	if !HasListeners[testEvent]() {
		t.Fatal("HasListeners = false")
	}
}

func TestFrameworkEventsAreNamed(t *testing.T) {
	cases := map[string]string{
		"app.started":             GetName[ApplicationStarted](),
		"http.request.finished":   GetName[RequestFinished](),
		"oauth.token.refreshed":   GetName[TokenRefreshed](),
		"ws.client.connected":     GetName[ClientConnected](),
		"scheduler.job.failed":    GetName[JobFailed](),
		"config.reloaded":         GetName[ConfigReloaded](),
		"plugin.installed":        GetName[PluginInstalled](),
		"router.route.registered": GetName[RouteRegistered](),
	}
	for want, got := range cases {
		if got != want {
			t.Errorf("name = %q, want %q", got, want)
		}
	}
}

func TestFrameworkEventRoundTrip(t *testing.T) {
	b := New()
	var seen RequestFinished
	OnBus(b, RequestFinished{}, func(_ *Context, e RequestFinished) error {
		seen = e
		return nil
	})

	EmitBus(b, RequestFinished{Route: "/users/:id", Status: 200, Duration: time.Millisecond})
	if seen.Route != "/users/:id" || seen.Status != 200 {
		t.Fatalf("event = %+v", seen)
	}
}

// --- Concurrency and race tests ---

func TestConcurrentEmit(t *testing.T) {
	b := New()
	var n atomic.Int64
	OnBus(b, testEvent{}, func(*Context, testEvent) error { n.Add(1); return nil })

	var wg sync.WaitGroup
	for g := 0; g < 20; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				EmitBus(b, testEvent{N: i})
			}
		}()
	}
	wg.Wait()

	if n.Load() != 2000 {
		t.Fatalf("ran %d times, want 2000", n.Load())
	}
}

func TestConcurrentRegistrationAndDispatch(t *testing.T) {
	b := New()
	var wg sync.WaitGroup

	// Registering while emitting is the pattern that a copy-on-write
	// snapshot has to survive: dispatchers must never see a torn slice.
	for g := 0; g < 8; g++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				sub := OnBus(b, testEvent{}, func(*Context, testEvent) error { return nil })
				sub.Unsubscribe()
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				EmitBus(b, testEvent{N: i})
			}
		}()
	}
	wg.Wait()
}

func TestConcurrentInspectAndEmit(t *testing.T) {
	b := New()
	b.EnableRecorder()
	OnBus(b, testEvent{}, func(*Context, testEvent) error { return nil })

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			EmitBus(b, testEvent{N: i})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = b.InspectAll()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = b.RecorderHistory()
			_ = b.TotalMetrics()
		}
	}()
	wg.Wait()
}

func TestConcurrentMetadataAccess(t *testing.T) {
	b := New()
	for i := 0; i < 8; i++ {
		i := i
		OnBus(b, metaEvent{}, func(ctx *Context, _ metaEvent) error {
			ctx.Set(fmt.Sprintf("k%d", i), i)
			_, _ = ctx.Get("k0")
			_ = ctx.Metadata()
			return nil
		})
	}
	EmitAsyncWaitBus(b, metaEvent{})
}
