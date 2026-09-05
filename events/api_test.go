package events

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestPackageLevelOnAndEmit verifies the default bus delegation works.
func TestPackageLevelOnAndEmit(t *testing.T) {
	Reset()
	defer Reset()

	type TestEvent struct{ Val int }

	var called bool
	On(TestEvent{}, func(c *Context, e TestEvent) error {
		called = true
		if e.Val != 42 {
			t.Errorf("got Val=%d, want 42", e.Val)
		}
		return nil
	})

	if err := Emit(TestEvent{Val: 42}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("listener not called")
	}
}

func TestPackageLevelOnType(t *testing.T) {
	Reset()
	defer Reset()

	type E struct{}
	var ran bool
	OnType[E](func(c *Context, e E) error {
		ran = true
		return nil
	})
	_ = Emit(E{})
	if !ran {
		t.Fatal("OnType listener did not run")
	}
}

func TestPackageLevelOnce(t *testing.T) {
	Reset()
	defer Reset()

	type E struct{}
	count := 0
	Once(E{}, func(c *Context, e E) error {
		count++
		return nil
	})
	_ = Emit(E{})
	_ = Emit(E{})
	if count != 1 {
		t.Fatalf("Once ran %d times, want 1", count)
	}
}

func TestPackageLevelOnceType(t *testing.T) {
	Reset()
	defer Reset()

	type E struct{}
	count := 0
	OnceType[E](func(c *Context, e E) error {
		count++
		return nil
	})
	_ = Emit(E{})
	_ = Emit(E{})
	if count != 1 {
		t.Fatalf("OnceType ran %d times, want 1", count)
	}
}

func TestPackageLevelBefore(t *testing.T) {
	Reset()
	defer Reset()

	type E struct{}
	var order []string
	Before(E{}, func(c *Context, e E) error {
		order = append(order, "before")
		return nil
	})
	On(E{}, func(c *Context, e E) error {
		order = append(order, "normal")
		return nil
	})
	_ = Emit(E{})
	if len(order) != 2 || order[0] != "before" || order[1] != "normal" {
		t.Fatalf("order=%v, want [before normal]", order)
	}
}

func TestPackageLevelBeforeType(t *testing.T) {
	Reset()
	defer Reset()

	type E struct{}
	var order []string
	BeforeType[E](func(c *Context, e E) error {
		order = append(order, "before")
		return nil
	})
	On(E{}, func(c *Context, e E) error {
		order = append(order, "normal")
		return nil
	})
	_ = Emit(E{})
	if len(order) != 2 || order[0] != "before" || order[1] != "normal" {
		t.Fatalf("order=%v, want [before normal]", order)
	}
}

func TestPackageLevelAfter(t *testing.T) {
	Reset()
	defer Reset()

	type E struct{}
	var order []string
	On(E{}, func(c *Context, e E) error {
		order = append(order, "normal")
		return nil
	})
	After(E{}, func(c *Context, e E) error {
		order = append(order, "after")
		return nil
	})
	_ = Emit(E{})
	if len(order) != 2 || order[0] != "normal" || order[1] != "after" {
		t.Fatalf("order=%v, want [normal after]", order)
	}
}

func TestPackageLevelAfterType(t *testing.T) {
	Reset()
	defer Reset()

	type E struct{}
	var order []string
	On(E{}, func(c *Context, e E) error {
		order = append(order, "normal")
		return nil
	})
	AfterType[E](func(c *Context, e E) error {
		order = append(order, "after")
		return nil
	})
	_ = Emit(E{})
	if len(order) != 2 || order[0] != "normal" || order[1] != "after" {
		t.Fatalf("order=%v, want [normal after]", order)
	}
}

func TestPackageLevelOff(t *testing.T) {
	Reset()
	defer Reset()

	type E struct{}
	sub := On(E{}, func(c *Context, e E) error { return nil })
	if !Off[E](sub.id) {
		t.Fatal("Off returned false")
	}
	if CountOf[E]() != 0 {
		t.Fatal("listener still registered")
	}
}

func TestPackageLevelEmitCtx(t *testing.T) {
	Reset()
	defer Reset()

	type E struct{}
	var gotCancel bool
	On(E{}, func(c *Context, e E) error {
		_, gotCancel = c.Ctx.Deadline()
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = EmitCtx(ctx, E{})
	if !gotCancel {
		t.Fatal("context not propagated")
	}
}

func TestPackageLevelEmitAsync(t *testing.T) {
	Reset()
	defer Reset()

	type E struct{}
	done := make(chan struct{})
	On(E{}, func(c *Context, e E) error {
		close(done)
		return nil
	})
	_ = EmitAsync(E{})
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("async listener did not run")
	}
}

func TestPackageLevelEmitAsyncCtx(t *testing.T) {
	Reset()
	defer Reset()

	type E struct{}
	done := make(chan bool)
	On(E{}, func(c *Context, e E) error {
		_, ok := c.Ctx.Deadline()
		done <- ok
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = EmitAsyncCtx(ctx, E{})
	if !<-done {
		t.Fatal("context not propagated to async")
	}
}

func TestPackageLevelEmitAsyncWait(t *testing.T) {
	Reset()
	defer Reset()

	type E struct{}
	var ran bool
	On(E{}, func(c *Context, e E) error {
		time.Sleep(10 * time.Millisecond)
		ran = true
		return nil
	})
	_ = EmitAsyncWait(E{})
	if !ran {
		t.Fatal("EmitAsyncWait did not wait")
	}
}

func TestPackageLevelUse(t *testing.T) {
	Reset()
	defer Reset()

	type E struct{}
	var ran bool
	Use(func(ctx *Context, next Next) error {
		ran = true
		return next()
	})
	On(E{}, func(c *Context, e E) error { return nil })
	_ = Emit(E{})
	if !ran {
		t.Fatal("middleware not invoked")
	}
}

func TestPackageLevelSetNameAndGetName(t *testing.T) {
	Reset()
	defer Reset()

	type E struct{}
	SetName[E]("custom.event")
	if got := GetName[E](); got != "custom.event" {
		t.Fatalf("got %q, want custom.event", got)
	}
}

func TestPackageLevelCountOf(t *testing.T) {
	Reset()
	defer Reset()

	type E struct{}
	if CountOf[E]() != 0 {
		t.Fatal("initial count not zero")
	}
	On(E{}, func(c *Context, e E) error { return nil })
	if CountOf[E]() != 1 {
		t.Fatal("count after On not 1")
	}
}

func TestPackageLevelHasListeners(t *testing.T) {
	Reset()
	defer Reset()

	type E struct{}
	if HasListeners[E]() {
		t.Fatal("HasListeners true before registration")
	}
	On(E{}, func(c *Context, e E) error { return nil })
	if !HasListeners[E]() {
		t.Fatal("HasListeners false after registration")
	}
}

func TestPackageLevelClearListeners(t *testing.T) {
	Reset()
	defer Reset()

	type E struct{}
	On(E{}, func(c *Context, e E) error { return nil })
	ClearListeners[E]()
	if HasListeners[E]() {
		t.Fatal("listeners not cleared")
	}
}

func TestPackageLevelList(t *testing.T) {
	Reset()
	defer Reset()

	type A struct{}
	type B struct{}
	before := len(List())
	On(A{}, func(c *Context, e A) error { return nil })
	On(B{}, func(c *Context, e B) error { return nil })
	names := List()
	if len(names) != before+2 {
		t.Fatalf("got %d events, want %d", len(names), before+2)
	}
}

func TestPackageLevelCountEvents(t *testing.T) {
	Reset()
	defer Reset()

	type A struct{}
	type B struct{}
	before := CountEvents()
	On(A{}, func(c *Context, e A) error { return nil })
	On(B{}, func(c *Context, e B) error { return nil })
	if CountEvents() != before+2 {
		t.Fatalf("got %d, want %d", CountEvents(), before+2)
	}
}

func TestPackageLevelCountListeners(t *testing.T) {
	Reset()
	defer Reset()

	type A struct{}
	On(A{}, func(c *Context, e A) error { return nil })
	On(A{}, func(c *Context, e A) error { return nil })
	if CountListeners() != 2 {
		t.Fatalf("got %d, want 2", CountListeners())
	}
}

func TestPackageLevelHasEvent(t *testing.T) {
	Reset()
	defer Reset()

	type E struct{}
	SetName[E]("test.event")
	On(E{}, func(c *Context, e E) error { return nil })
	if !HasEvent("test.event") {
		t.Fatal("HasEvent returned false")
	}
}

func TestPackageLevelInspectEvent(t *testing.T) {
	Reset()
	defer Reset()

	type E struct{}
	On(E{}, func(c *Context, e E) error { return nil })
	info := InspectEvent[E]()
	if info.ListenerCount != 1 {
		t.Fatalf("got %d listeners, want 1", info.ListenerCount)
	}
}

func TestPackageLevelInspectAll(t *testing.T) {
	Reset()
	defer Reset()

	type A struct{}
	type B struct{}
	before := len(InspectAll())
	On(A{}, func(c *Context, e A) error { return nil })
	On(B{}, func(c *Context, e B) error { return nil })
	all := InspectAll()
	if len(all) != before+2 {
		t.Fatalf("got %d infos, want %d", len(all), before+2)
	}
}

func TestPackageLevelMetricsOf(t *testing.T) {
	Reset()
	defer Reset()

	type E struct{}
	On(E{}, func(c *Context, e E) error { return nil })
	_ = Emit(E{})
	m := MetricsOf[E]()
	if m.Dispatches != 1 {
		t.Fatalf("got %d dispatches, want 1", m.Dispatches)
	}
}

func TestPackageLevelTotalMetrics(t *testing.T) {
	Reset()
	defer Reset()

	type A struct{}
	type B struct{}
	before := TotalMetrics().Dispatches
	On(A{}, func(c *Context, e A) error { return nil })
	On(B{}, func(c *Context, e B) error { return nil })
	_ = Emit(A{})
	_ = Emit(B{})
	m := TotalMetrics()
	if m.Dispatches != before+2 {
		t.Fatalf("got %d dispatches, want %d", m.Dispatches, before+2)
	}
}

func TestPackageLevelEnableRecorder(t *testing.T) {
	Reset()
	defer Reset()

	EnableRecorder()
	if !Default.RecorderEnabled() {
		t.Fatal("recorder not enabled")
	}
}

func TestPackageLevelEnableRecorderWithPayload(t *testing.T) {
	Reset()
	defer Reset()

	EnableRecorderWithPayload()
	if !Default.RecorderEnabled() {
		t.Fatal("recorder not enabled")
	}
}

func TestPackageLevelDisableRecorder(t *testing.T) {
	Reset()
	defer Reset()

	EnableRecorder()
	DisableRecorder()
	if Default.RecorderEnabled() {
		t.Fatal("recorder still enabled")
	}
}

func TestPackageLevelHistory(t *testing.T) {
	Reset()
	defer Reset()

	type E struct{}
	EnableRecorder()
	On(E{}, func(c *Context, e E) error { return nil })
	_ = Emit(E{})
	h := History()
	if len(h) != 1 {
		t.Fatalf("got %d records, want 1", len(h))
	}
}

func TestPackageLevelClearHistory(t *testing.T) {
	Reset()
	defer Reset()

	type E struct{}
	EnableRecorder()
	On(E{}, func(c *Context, e E) error { return nil })
	_ = Emit(E{})
	ClearHistory()
	if len(History()) != 0 {
		t.Fatal("history not cleared")
	}
}

// TestEnumStringers exercises the String() methods.
func TestAsyncModeString(t *testing.T) {
	tests := []struct {
		mode AsyncMode
		want string
	}{
		{AsyncGoroutine, "goroutine"},
		{AsyncWorkerPool, "worker-pool"},
		{AsyncMode(99), "AsyncMode(99)"},
	}
	for _, tt := range tests {
		if got := tt.mode.String(); got != tt.want {
			t.Errorf("%v.String()=%q, want %q", tt.mode, got, tt.want)
		}
	}
}

func TestOverflowPolicyString(t *testing.T) {
	tests := []struct {
		p    OverflowPolicy
		want string
	}{
		{OverflowBlock, "block"},
		{OverflowSpawn, "spawn"},
		{OverflowDrop, "drop"},
		{OverflowPolicy(99), "OverflowPolicy(99)"},
	}
	for _, tt := range tests {
		if got := tt.p.String(); got != tt.want {
			t.Errorf("%v.String()=%q, want %q", tt.p, got, tt.want)
		}
	}
}

func TestPanicModeString(t *testing.T) {
	tests := []struct {
		m    PanicMode
		want string
	}{
		{PanicRecoverAndContinue, "recover-and-continue"},
		{PanicRecoverAndFail, "recover-and-fail"},
		{PanicPropagate, "propagate"},
		{PanicMode(99), "PanicMode(99)"},
	}
	for _, tt := range tests {
		if got := tt.m.String(); got != tt.want {
			t.Errorf("%v.String()=%q, want %q", tt.m, got, tt.want)
		}
	}
}

// TestErrorTypes exercises Error() and Unwrap() methods.
func TestPanicErrorError(t *testing.T) {
	e := &PanicError{Event: "UserCreated", Listener: "SendEmail", Value: "boom"}
	got := e.Error()
	if got == "" {
		t.Fatal("Error() returned empty")
	}
	if got != `events: panic in listener "SendEmail" for event "UserCreated": boom` {
		t.Errorf("got %q", got)
	}
}

func TestPanicErrorUnwrap(t *testing.T) {
	base := errors.New("base")
	e := &PanicError{Value: base}
	if !errors.Is(e, base) {
		t.Fatal("Unwrap did not expose base error")
	}
}

func TestListenerErrorError(t *testing.T) {
	e := &ListenerError{Listener: "handler", Err: errors.New("fail")}
	got := e.Error()
	if got != "handler: fail" {
		t.Errorf("got %q", got)
	}
}

func TestMultiErrorError(t *testing.T) {
	tests := []struct {
		name string
		errs []error
		want string
	}{
		{"zero", nil, "events: no errors"},
		{"one", []error{errors.New("a")}, "a"},
		{
			"two",
			[]error{errors.New("a"), errors.New("b")},
			"events: 2 listeners failed: a (and 1 more)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &MultiError{Errors: tt.errs}
			if got := m.Error(); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestContextDelete exercises Delete.
func TestContextDelete(t *testing.T) {
	Reset()
	defer Reset()

	type E struct{}
	On(E{}, func(c *Context, e E) error {
		c.Set("key", "val")
		c.Delete("key")
		_, ok := c.Get("key")
		if ok {
			t.Fatal("key still present after Delete")
		}
		return nil
	})
	_ = Emit(E{})
}

// TestContextListenerIndex exercises ListenerIndex.
func TestContextListenerIndex(t *testing.T) {
	Reset()
	defer Reset()

	type E struct{}
	var indices []int
	On(E{}, func(c *Context, e E) error {
		indices = append(indices, c.ListenerIndex())
		return nil
	})
	On(E{}, func(c *Context, e E) error {
		indices = append(indices, c.ListenerIndex())
		return nil
	})
	_ = Emit(E{})
	if len(indices) != 2 || indices[0] != 0 || indices[1] != 1 {
		t.Fatalf("indices=%v, want [0 1]", indices)
	}
}

// TestBusConfig exercises Config().
func TestBusConfig(t *testing.T) {
	cfg := Config{Async: AsyncWorkerPool}
	bus := New(cfg)
	defer bus.Close()
	got := bus.Config()
	if got.Async != AsyncWorkerPool {
		t.Fatal("Config did not return configured value")
	}
}

// TestBusEventCount exercises EventCount.
func TestBusEventCount(t *testing.T) {
	bus := New(Config{})
	defer bus.Close()
	type A struct{}
	type B struct{}
	OnBus(bus, A{}, func(c *Context, e A) error { return nil })
	OnBus(bus, B{}, func(c *Context, e B) error { return nil })
	if bus.EventCount() != 2 {
		t.Fatalf("got %d, want 2", bus.EventCount())
	}
}

// TestPoolStats exercises PoolStats when pool exists.
func TestPoolStats(t *testing.T) {
	bus := New(Config{Async: AsyncWorkerPool, Workers: 2, QueueSize: 10})
	defer bus.Close()

	type E struct{}
	done := make(chan struct{})
	OnBus(bus, E{}, func(c *Context, e E) error {
		<-done
		return nil
	})
	_ = EmitAsyncBus(bus, E{})
	time.Sleep(10 * time.Millisecond)
	stats := bus.PoolStats()
	if stats.Workers != 2 {
		t.Fatalf("got Workers=%d, want 2", stats.Workers)
	}
	close(done)
	time.Sleep(20 * time.Millisecond)
}
