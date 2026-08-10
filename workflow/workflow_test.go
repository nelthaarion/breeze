package workflow

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nelthaarion/breeze/events"
)

// newTestEngine returns an engine isolated from the process-wide bus and
// collector, so tests never observe each other's events.
func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	return NewEngine(Config{
		Bus:                  events.New(),
		DisableObservability: true,
	})
}

func mustRegister(t *testing.T, e *Engine, d *Definition) {
	t.Helper()
	if err := e.Register(d); err != nil {
		t.Fatalf("Register: %v", err)
	}
}

func TestRunSequential(t *testing.T) {
	e := newTestEngine(t)
	var order []string
	var mu sync.Mutex
	record := func(name string) StepFunc {
		return func(*Context) error {
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			return nil
		}
	}

	mustRegister(t, e, New("seq").
		Step("a", record("a")).
		Step("b", record("b")).
		Step("c", record("c")))

	res, err := e.Run(context.Background(), "seq", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.State != StateCompleted {
		t.Fatalf("state = %v, want completed", res.State)
	}
	if got := fmt.Sprint(order); got != "[a b c]" {
		t.Fatalf("order = %s, want [a b c]", got)
	}
}

func TestPayloadAndMetadata(t *testing.T) {
	e := newTestEngine(t)
	type order struct{ ID int }

	mustRegister(t, e, New("meta").
		Step("first", func(c *Context) error {
			o, ok := Payload[order](c)
			if !ok || o.ID != 42 {
				return fmt.Errorf("payload = %v, %v", o, ok)
			}
			c.Set("charged", true)
			return nil
		}).
		Step("second", func(c *Context) error {
			if v, ok := c.Get("charged"); !ok || v != true {
				return errors.New("metadata did not carry across steps")
			}
			return nil
		}))

	if _, err := e.Run(context.Background(), "meta", order{ID: 42}); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestRetryUntilSuccess(t *testing.T) {
	e := newTestEngine(t)
	var attempts atomic.Int32

	mustRegister(t, e, New("retry").
		Step("flaky", func(*Context) error {
			if attempts.Add(1) < 3 {
				return errors.New("boom")
			}
			return nil
		}, WithRetry(RetryPolicy{MaxAttempts: 5, InitialDelay: time.Millisecond})))

	res, err := e.Run(context.Background(), "retry", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
	if res.Steps[0].Attempts != 3 {
		t.Fatalf("reported attempts = %d, want 3", res.Steps[0].Attempts)
	}
}

func TestNonRetryableStopsImmediately(t *testing.T) {
	e := newTestEngine(t)
	sentinel := errors.New("card declined")
	var attempts atomic.Int32

	mustRegister(t, e, New("final").
		Step("charge", func(*Context) error {
			attempts.Add(1)
			return NonRetryable(sentinel)
		}, WithRetry(RetryPolicy{MaxAttempts: 5, InitialDelay: time.Millisecond})))

	_, err := e.Run(context.Background(), "final", nil)
	if attempts.Load() != 1 {
		t.Fatalf("attempts = %d, want 1", attempts.Load())
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("error lost its identity: %v", err)
	}
	if !errors.Is(err, ErrNonRetryable) {
		t.Fatalf("error is not marked non-retryable: %v", err)
	}
}

func TestCompensationRunsInReverse(t *testing.T) {
	e := newTestEngine(t)
	var rollback []string
	var mu sync.Mutex
	undo := func(name string) CompensateFunc {
		return func(*Context) error {
			mu.Lock()
			rollback = append(rollback, name)
			mu.Unlock()
			return nil
		}
	}
	ok := func(*Context) error { return nil }

	mustRegister(t, e, New("saga").
		Step("reserve", ok, WithCompensation(undo("reserve"))).
		Step("charge", ok, WithCompensation(undo("charge"))).
		Step("ship", func(*Context) error { return errors.New("no stock") }))

	res, err := e.Run(context.Background(), "saga", nil)
	if err == nil {
		t.Fatal("expected failure")
	}
	if res.State != StateCompensated {
		t.Fatalf("state = %v, want compensated", res.State)
	}
	if got := fmt.Sprint(rollback); got != "[charge reserve]" {
		t.Fatalf("rollback order = %s, want [charge reserve]", got)
	}
}

func TestParallelStepsShareOneLevel(t *testing.T) {
	e := newTestEngine(t)
	var running, peak atomic.Int32

	slow := func(*Context) error {
		cur := running.Add(1)
		for {
			old := peak.Load()
			if cur <= old || peak.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		running.Add(-1)
		return nil
	}

	mustRegister(t, e, New("fanout").
		Step("root", func(*Context) error { return nil }).
		Step("a", slow, WithDependsOn("root")).
		Step("b", slow, WithDependsOn("root")).
		Step("join", func(*Context) error { return nil }, WithDependsOn("a", "b")))

	if _, err := e.Run(context.Background(), "fanout", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if peak.Load() < 2 {
		t.Fatalf("peak concurrency = %d, want at least 2", peak.Load())
	}
}

func TestConditionalStepIsSkipped(t *testing.T) {
	e := newTestEngine(t)
	var ran atomic.Bool

	mustRegister(t, e, New("cond").
		Step("maybe", func(*Context) error {
			ran.Store(true)
			return nil
		}, WithCondition(func(*Context) bool { return false })))

	res, err := e.Run(context.Background(), "cond", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ran.Load() {
		t.Fatal("skipped step still executed")
	}
	if !res.Steps[0].Skipped {
		t.Fatal("step not reported as skipped")
	}
}

func TestPanicBecomesFailure(t *testing.T) {
	var caught atomic.Bool
	e := NewEngine(Config{
		Bus:                  events.New(),
		DisableObservability: true,
		OnPanic: func(_, _ string, _ any, _ []byte) {
			caught.Store(true)
		},
	})

	mustRegister(t, e, New("boom").
		Step("explode", func(*Context) error { panic("kaboom") }))

	_, err := e.Run(context.Background(), "boom", nil)
	if !errors.Is(err, ErrStepPanicked) {
		t.Fatalf("err = %v, want ErrStepPanicked", err)
	}
	if !caught.Load() {
		t.Fatal("OnPanic was not called")
	}
}

func TestStepTimeout(t *testing.T) {
	e := newTestEngine(t)
	mustRegister(t, e, New("slow").
		Step("sleepy", func(c *Context) error {
			<-c.Done()
			return c.Err()
		}, WithTimeout(20*time.Millisecond)))

	res, err := e.Run(context.Background(), "slow", nil)
	if !errors.Is(err, ErrStepTimeout) {
		t.Fatalf("err = %v, want ErrStepTimeout", err)
	}
	if res.State != StateTimedOut {
		t.Fatalf("state = %v, want timed_out", res.State)
	}
}

func TestCancellationStopsRemainingSteps(t *testing.T) {
	e := newTestEngine(t)
	ctx, cancel := context.WithCancel(context.Background())
	var second atomic.Bool

	mustRegister(t, e, New("cancel").
		Step("first", func(*Context) error {
			cancel()
			return nil
		}).
		Step("second", func(*Context) error {
			second.Store(true)
			return nil
		}))

	res, _ := e.Run(ctx, "cancel", nil)
	if second.Load() {
		t.Fatal("step ran after cancellation")
	}
	if res.State != StateCancelled {
		t.Fatalf("state = %v, want cancelled", res.State)
	}
}

func TestEventTriggerStartsWorkflow(t *testing.T) {
	bus := events.New()
	e := NewEngine(Config{Bus: bus, DisableObservability: true})

	type UserRegistered struct{ Email string }
	done := make(chan string, 1)

	def := New("welcome").Step("email", func(c *Context) error {
		u, _ := Payload[UserRegistered](c)
		done <- u.Email
		return nil
	})
	OnType[UserRegistered](def)
	mustRegister(t, e, def)

	if err := events.EmitBus(bus, UserRegistered{Email: "a@b.c"}); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	select {
	case got := <-done:
		if got != "a@b.c" {
			t.Fatalf("payload = %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("workflow was not triggered by the event")
	}
	if err := e.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestIdempotencyKeyRunsOnce(t *testing.T) {
	e := newTestEngine(t)
	var runs atomic.Int32

	mustRegister(t, e, New("once").
		Step("work", func(*Context) error {
			runs.Add(1)
			return nil
		}))

	for range 3 {
		if _, err := e.Run(context.Background(), "once", nil, WithIdempotencyKey("order-1")); err != nil {
			t.Fatalf("Run: %v", err)
		}
	}
	if runs.Load() != 1 {
		t.Fatalf("runs = %d, want 1", runs.Load())
	}
}

func TestResumeSkipsCompletedSteps(t *testing.T) {
	store := NewMemoryStore()
	e := NewEngine(Config{Store: store, Bus: events.New(), DisableObservability: true})

	var firstRuns, secondRuns atomic.Int32
	fail := true

	mustRegister(t, e, New("resumable").
		Step("first", func(*Context) error {
			firstRuns.Add(1)
			return nil
		}).
		Step("second", func(*Context) error {
			secondRuns.Add(1)
			if fail {
				return errors.New("transient")
			}
			return nil
		}))

	if _, err := e.Run(context.Background(), "resumable", nil); err == nil {
		t.Fatal("expected the first run to fail")
	}

	// A failed execution is terminal, so re-open it the way a crash
	// would have left it: still running, with its step history intact.
	pending, _ := store.PendingWorkflows(context.Background())
	if len(pending) != 0 {
		t.Fatalf("failed execution should be terminal, got %d pending", len(pending))
	}
	recs := e.mustRecords(t, store)
	rec := recs[0]
	rec.State = StateRunning
	rec.FinishedAt = time.Time{}
	if err := store.UpdateWorkflow(context.Background(), rec); err != nil {
		t.Fatalf("UpdateWorkflow: %v", err)
	}

	fail = false
	n, err := e.Resume(context.Background())
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if n != 1 {
		t.Fatalf("resumed = %d, want 1", n)
	}
	if err := e.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if firstRuns.Load() != 1 {
		t.Fatalf("completed step re-executed: firstRuns = %d, want 1", firstRuns.Load())
	}
	if secondRuns.Load() != 2 {
		t.Fatalf("failed step not retried: secondRuns = %d, want 2", secondRuns.Load())
	}
}

// mustRecords returns every stored execution, for assertions that need
// the persisted state rather than the returned Result.
func (e *Engine) mustRecords(t *testing.T, store *MemoryStore) []WorkflowRecord {
	t.Helper()
	store.mu.RLock()
	defer store.mu.RUnlock()
	out := make([]WorkflowRecord, 0, len(store.flows))
	for _, rec := range store.flows {
		out = append(out, rec)
	}
	if len(out) == 0 {
		t.Fatal("no execution was persisted")
	}
	return out
}

func TestConcurrentRunsAreIsolated(t *testing.T) {
	e := newTestEngine(t)
	mustRegister(t, e, New("concurrent").
		Step("a", func(c *Context) error {
			c.Set("id", c.ExecutionID())
			return nil
		}).
		Step("b", func(c *Context) error {
			got, _ := c.MetaString("id")
			if got != c.ExecutionID() {
				return fmt.Errorf("context leaked between executions: %q != %q", got, c.ExecutionID())
			}
			return nil
		}))

	var wg sync.WaitGroup
	errCh := make(chan error, 50)
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.Run(context.Background(), "concurrent", nil); err != nil {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent run failed: %v", err)
	}
}

func TestValidation(t *testing.T) {
	noop := func(*Context) error { return nil }

	tests := []struct {
		name string
		def  *Definition
		want error
	}{
		{"no steps", New("empty"), ErrNoSteps},
		{"duplicate", New("dup").Step("a", noop).Step("a", noop), ErrDuplicateStep},
		{
			"unknown dependency",
			New("unknown").Step("a", noop, WithDependsOn("ghost")),
			ErrUnknownDependency,
		},
		{
			"self cycle",
			New("self").Step("a", noop, WithDependsOn("a")),
			ErrWorkflowCycle,
		},
		{
			"cycle",
			New("loop").
				Step("a", noop, WithDependsOn("b")).
				Step("b", noop, WithDependsOn("a")),
			ErrWorkflowCycle,
		},
		{"nil func", New("nilfn").Step("a", nil), ErrInvalidWorkflow},
		{
			"bad jitter",
			New("jitter").Step("a", noop, WithRetry(RetryPolicy{MaxAttempts: 2, Jitter: 2})),
			ErrInvalidWorkflow,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.def.Validate()
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("error is not a *ValidationError: %v", err)
			}
		})
	}
}

func TestRegisterRejectsDuplicateWorkflow(t *testing.T) {
	e := newTestEngine(t)
	noop := func(*Context) error { return nil }
	mustRegister(t, e, New("twice").Step("a", noop))

	err := e.Register(New("twice").Step("a", noop))
	if !errors.Is(err, ErrDuplicateWorkflow) {
		t.Fatalf("err = %v, want ErrDuplicateWorkflow", err)
	}
}

func TestBackoffCurve(t *testing.T) {
	p := RetryPolicy{
		MaxAttempts:  10,
		Backoff:      BackoffExponential,
		InitialDelay: 10 * time.Millisecond,
		MaxDelay:     100 * time.Millisecond,
	}
	want := []time.Duration{10, 20, 40, 80, 100, 100}
	for i, w := range want {
		if got := p.Delay(i + 1); got != w*time.Millisecond {
			t.Fatalf("Delay(%d) = %v, want %v", i+1, got, w*time.Millisecond)
		}
	}
	// A huge attempt number must saturate, not overflow into a
	// negative duration.
	if got := p.Delay(1 << 20); got != 100*time.Millisecond {
		t.Fatalf("Delay(large) = %v, want the cap", got)
	}
}

func TestStateTransitions(t *testing.T) {
	if StateCompleted.CanTransitionTo(StateRunning) {
		t.Fatal("a terminal state must not transition")
	}
	if !StateRunning.CanTransitionTo(StateCompensating) {
		t.Fatal("running must be able to start compensating")
	}
	if !StateFailed.Terminal() {
		t.Fatal("failed must be terminal")
	}
	if got := StateTimedOut.String(); got != "timed_out" {
		t.Fatalf("String() = %q", got)
	}
}

func TestShutdownRejectsNewRuns(t *testing.T) {
	e := newTestEngine(t)
	mustRegister(t, e, New("closing").Step("a", func(*Context) error { return nil }))

	if err := e.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if _, err := e.Run(context.Background(), "closing", nil); !errors.Is(err, ErrEngineClosed) {
		t.Fatalf("err = %v, want ErrEngineClosed", err)
	}
}
