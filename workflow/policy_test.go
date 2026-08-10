package workflow

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nelthaarion/breeze/events"
)

// Retry timing and the remaining builder options are covered here.
// Backoff arithmetic is the kind of code that looks obviously right and
// overflows on the 63rd attempt, so the curve is checked at its edges
// rather than only in the middle.

func TestExponentialBackoffGrowsAndCaps(t *testing.T) {
	p := RetryPolicy{
		MaxAttempts:  10,
		Backoff:      BackoffExponential,
		InitialDelay: 10 * time.Millisecond,
		MaxDelay:     100 * time.Millisecond,
	}

	// Doubling, then flat at the cap.
	for _, tc := range []struct {
		attempt int
		want    time.Duration
	}{
		{1, 10 * time.Millisecond},
		{2, 20 * time.Millisecond},
		{3, 40 * time.Millisecond},
		{4, 80 * time.Millisecond},
		{5, 100 * time.Millisecond}, // 160ms would exceed MaxDelay
		{6, 100 * time.Millisecond},
	} {
		if got := p.Delay(tc.attempt); got != tc.want {
			t.Errorf("Delay(%d) = %v, want %v", tc.attempt, got, tc.want)
		}
	}
}

func TestBackoffNeverOverflows(t *testing.T) {
	// A large attempt number shifts past the width of int64. Without a
	// guard the shift wraps negative and the retry fires immediately —
	// turning a backoff into a hot loop.
	p := RetryPolicy{
		MaxAttempts:  1000,
		Backoff:      BackoffExponential,
		InitialDelay: time.Second,
		MaxDelay:     time.Minute,
	}
	for _, attempt := range []int{62, 63, 64, 200, 1000} {
		got := p.Delay(attempt)
		if got <= 0 {
			t.Errorf("Delay(%d) = %v, want a positive delay (overflow)", attempt, got)
		}
		if got > time.Minute {
			t.Errorf("Delay(%d) = %v, want <= MaxDelay", attempt, got)
		}
	}
}

func TestFixedBackoffIsConstant(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 5, InitialDelay: 25 * time.Millisecond}
	for attempt := 1; attempt <= 4; attempt++ {
		if got := p.Delay(attempt); got != 25*time.Millisecond {
			t.Errorf("Delay(%d) = %v, want 25ms on every attempt", attempt, got)
		}
	}
}

func TestDelayFallsBackToDefaults(t *testing.T) {
	// An unset policy still has to produce a usable delay, or a retry
	// with no configured wait would spin.
	var p RetryPolicy
	if got := p.Delay(1); got <= 0 {
		t.Errorf("Delay(1) on a zero policy = %v, want a positive default", got)
	}
}

func TestJitterStaysWithinBounds(t *testing.T) {
	p := RetryPolicy{
		MaxAttempts:  5,
		InitialDelay: 100 * time.Millisecond,
		MaxDelay:     100 * time.Millisecond,
		Jitter:       0.5,
	}

	var varied bool
	first := p.Delay(1)
	for i := 0; i < 200; i++ {
		got := p.Delay(1)
		// ±50% of 100ms, and never negative: a negative delay would
		// mean an immediate retry.
		if got < 50*time.Millisecond || got > 150*time.Millisecond {
			t.Fatalf("Delay with 0.5 jitter = %v, want within 50-150ms", got)
		}
		if got != first {
			varied = true
		}
	}
	if !varied {
		t.Error("jitter produced an identical delay 200 times; it is not being applied")
	}
}

func TestJitterIsClamped(t *testing.T) {
	// A jitter above 1 would allow a negative offset larger than the
	// delay itself. It is clamped, so the result stays non-negative.
	p := RetryPolicy{
		MaxAttempts:  5,
		InitialDelay: 50 * time.Millisecond,
		MaxDelay:     50 * time.Millisecond,
		Jitter:       10,
	}
	for i := 0; i < 200; i++ {
		if got := p.Delay(1); got < 0 {
			t.Fatalf("Delay = %v, want >= 0", got)
		}
	}
}

func TestRetryableHookDecides(t *testing.T) {
	sentinel := errors.New("transient")
	p := RetryPolicy{
		MaxAttempts: 5,
		Retryable:   func(err error) bool { return errors.Is(err, sentinel) },
	}
	if !p.ShouldRetry(1, sentinel) {
		t.Error("the hook approved the error but it was not retried")
	}
	if p.ShouldRetry(1, errors.New("other")) {
		t.Error("the hook rejected the error but it was retried anyway")
	}
}

func TestCancelledContextIsNeverRetried(t *testing.T) {
	// Retrying after cancellation would ignore the caller's request to
	// stop, and every attempt would fail the same way.
	p := RetryPolicy{MaxAttempts: 5}
	if p.ShouldRetry(1, context.Canceled) {
		t.Error("retried a cancelled context")
	}
}

func TestRetryableHookRunsPerAttempt(t *testing.T) {
	// The engine must consult the policy on each failure, not cache the
	// first verdict: an error that becomes fatal must stop the retries.
	engine := NewEngine(Config{Bus: events.New(), DisableObservability: true})

	var calls int
	def := New("wf").Step("s", func(*Context) error {
		return errors.New("fail")
	}, WithRetry(RetryPolicy{
		MaxAttempts:  4,
		InitialDelay: time.Millisecond,
		Retryable: func(error) bool {
			calls++
			return calls < 2 // allow one retry, then refuse
		},
	}))
	if err := engine.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}
	res, err := engine.Run(context.Background(), "wf", nil)
	if err == nil {
		t.Fatal("Run returned nil for a failing step")
	}
	if len(res.Steps) != 1 {
		t.Fatalf("len(Steps) = %d, want 1", len(res.Steps))
	}
	if res.Steps[0].Attempts != 2 {
		t.Errorf("Attempts = %d, want 2 (the hook refused the third)", res.Steps[0].Attempts)
	}
}

// --- remaining builder options ---

func TestStepMetaAndCompensationTimeout(t *testing.T) {
	engine := NewEngine(Config{Bus: events.New(), DisableObservability: true})

	var compensated bool
	def := New("wf").
		Step("a", func(*Context) error { return nil },
			WithMeta("owner", "billing"),
			WithMeta("tier", "gold"),
			WithCompensation(func(*Context) error { compensated = true; return nil }),
			WithCompensationTimeout(time.Second),
		).
		Step("b", func(*Context) error {
			return NonRetryable(errors.New("fail"))
		}, WithDependsOn("a"))
	if err := engine.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}

	got, _ := engine.Definition("wf")
	steps := got.Steps()
	if len(steps) == 0 {
		t.Fatal("Steps() returned nothing")
	}
	if _, err := engine.Run(context.Background(), "wf", nil); err == nil {
		t.Fatal("Run returned nil for a failing workflow")
	}
	if !compensated {
		t.Error("compensation did not run within its timeout")
	}
}

func TestDefinitionRetryAppliesToEveryStep(t *testing.T) {
	// A definition-level policy is the default for steps that do not
	// set their own, so it must reach a step with no WithRetry.
	engine := NewEngine(Config{Bus: events.New(), DisableObservability: true})

	var attempts int
	def := New("wf").
		Retry(RetryPolicy{MaxAttempts: 3, InitialDelay: time.Millisecond}).
		Step("s", func(*Context) error {
			attempts++
			return errors.New("fail")
		})
	if err := engine.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := engine.Run(context.Background(), "wf", nil); err == nil {
		t.Fatal("Run returned nil for a failing step")
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3 from the definition-level policy", attempts)
	}
}

func TestStepRetryOverridesDefinitionRetry(t *testing.T) {
	engine := NewEngine(Config{Bus: events.New(), DisableObservability: true})

	var attempts int
	def := New("wf").
		Retry(RetryPolicy{MaxAttempts: 5, InitialDelay: time.Millisecond}).
		Step("s", func(*Context) error {
			attempts++
			return errors.New("fail")
		}, WithRetry(RetryPolicy{MaxAttempts: 2, InitialDelay: time.Millisecond}))
	if err := engine.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := engine.Run(context.Background(), "wf", nil); err == nil {
		t.Fatal("Run returned nil for a failing step")
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2 from the step's own policy", attempts)
	}
}

// --- triggers ---

type policyTestEvent struct{ ID int }

func TestOnAndOnTypeSubscribeIdentically(t *testing.T) {
	// On takes a sample value purely for inference; it must build the
	// same trigger as the explicit form.
	byValue := On(New("a").Step("s", func(*Context) error { return nil }), policyTestEvent{})
	byType := OnType[policyTestEvent](New("b").Step("s", func(*Context) error { return nil }))

	if byValue.trig == nil || byType.trig == nil {
		t.Fatal("On/OnType did not attach a trigger")
	}
	if byValue.trig.name != byType.trig.name {
		t.Errorf("trigger names differ: %q vs %q", byValue.trig.name, byType.trig.name)
	}
	if byValue.trig.typ != byType.trig.typ {
		t.Errorf("trigger types differ: %v vs %v", byValue.trig.typ, byType.trig.typ)
	}
}

func TestTriggerStartsWorkflowFromEvent(t *testing.T) {
	bus := events.New()
	engine := NewEngine(Config{Bus: bus, DisableObservability: true})

	var (
		mu      sync.Mutex
		gotID   int
		ran     bool
		done    = make(chan struct{})
		trigger string
	)
	def := On(New("on-event").Step("handle", func(c *Context) error {
		mu.Lock()
		defer mu.Unlock()
		if e, ok := Payload[policyTestEvent](c); ok {
			gotID = e.ID
		}
		trigger = c.Workflow()
		ran = true
		close(done)
		return nil
	}), policyTestEvent{})

	if err := engine.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := events.EmitBus(bus, policyTestEvent{ID: 77}); err != nil {
		t.Fatalf("EmitBus: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the triggered workflow never ran")
	}

	mu.Lock()
	defer mu.Unlock()
	if !ran {
		t.Fatal("step did not run")
	}
	if gotID != 77 {
		t.Errorf("payload ID = %d, want 77; the event did not reach the workflow", gotID)
	}
	if trigger != "on-event" {
		t.Errorf("Workflow() = %q, want \"on-event\"", trigger)
	}
}

func TestShutdownUnsubscribesTriggers(t *testing.T) {
	// After shutdown an event must not start anything, or a stopped
	// engine would keep doing work.
	bus := events.New()
	engine := NewEngine(Config{Bus: bus, DisableObservability: true})

	var started int32
	var mu sync.Mutex
	def := On(New("stopped").Step("s", func(*Context) error {
		mu.Lock()
		started++
		mu.Unlock()
		return nil
	}), policyTestEvent{})
	if err := engine.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := engine.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := events.EmitBus(bus, policyTestEvent{ID: 1}); err != nil {
		t.Fatalf("EmitBus: %v", err)
	}
	// Give a stray goroutine a chance to run, so the assertion is not
	// merely winning a race.
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if started != 0 {
		t.Errorf("a workflow started %d times after shutdown", started)
	}
}

// --- validation errors ---

func TestValidationErrorMessages(t *testing.T) {
	// A definition-level problem names the workflow; a step-level one
	// names the step too, since "invalid workflow" alone does not say
	// where to look.
	empty := New("no-steps")
	err := empty.Validate()
	if err == nil {
		t.Fatal("Validate() accepted a definition with no steps")
	}
	if !errors.Is(err, ErrNoSteps) {
		t.Errorf("err = %v, want ErrNoSteps", err)
	}
	if !strings.Contains(err.Error(), "no-steps") {
		t.Errorf("Error() = %q, does not name the workflow", err.Error())
	}

	stepErr := New("wf").
		Step("s", func(*Context) error { return nil }, WithDependsOn("ghost")).
		Validate()
	if stepErr == nil {
		t.Fatal("Validate() accepted an unknown dependency")
	}
	if !errors.Is(stepErr, ErrUnknownDependency) {
		t.Errorf("err = %v, want ErrUnknownDependency", stepErr)
	}
	msg := stepErr.Error()
	for _, want := range []string{"wf", "s"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Error() = %q, missing %q", msg, want)
		}
	}

	var ve *ValidationError
	if !errors.As(stepErr, &ve) {
		t.Fatalf("err is not a *ValidationError: %T", stepErr)
	}
	if ve.Unwrap() == nil {
		t.Error("ValidationError.Unwrap() = nil")
	}
}

func TestPlanFailsOnCycle(t *testing.T) {
	// A cycle has no valid order, so planning must refuse rather than
	// silently dropping the steps it cannot place.
	def := New("cyclic").
		Step("a", func(*Context) error { return nil }, WithDependsOn("b")).
		Step("b", func(*Context) error { return nil }, WithDependsOn("a"))

	if _, err := def.plan(); !errors.Is(err, ErrWorkflowCycle) {
		t.Errorf("plan() = %v, want ErrWorkflowCycle", err)
	}
	// The same failure must surface through the public entry point.
	if err := def.Validate(); !errors.Is(err, ErrWorkflowCycle) {
		t.Errorf("Validate() = %v, want ErrWorkflowCycle", err)
	}
}

func TestMetadataSurvivesTheStore(t *testing.T) {
	// Metadata is the resume-safe channel between steps, so it has to
	// round-trip through the store as a copy, not a shared map.
	ctx := context.Background()
	s := NewMemoryStore()
	meta := map[string]string{"tenant": "acme"}
	if err := s.CreateWorkflow(ctx, WorkflowRecord{
		ExecutionID: "e1", Workflow: "wf", State: StateRunning,
		Metadata: meta, Payload: []byte(`{"id":1}`),
	}); err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}

	// Mutating the caller's map must not change what was stored.
	meta["tenant"] = "tampered"

	got, err := s.GetWorkflow(ctx, "e1")
	if err != nil {
		t.Fatalf("GetWorkflow: %v", err)
	}
	if got.Metadata["tenant"] != "acme" {
		t.Errorf("Metadata[tenant] = %q, want \"acme\"; the store kept a reference to the caller's map",
			got.Metadata["tenant"])
	}
	if string(got.Payload) != `{"id":1}` {
		t.Errorf("Payload = %s, want the stored bytes", got.Payload)
	}

	// And mutating what was returned must not change the store.
	got.Metadata["tenant"] = "again"
	got.Payload[0] = 'X'
	again, _ := s.GetWorkflow(ctx, "e1")
	if again.Metadata["tenant"] != "acme" {
		t.Error("a returned record's metadata map is shared with the store")
	}
	if string(again.Payload) != `{"id":1}` {
		t.Error("a returned record's payload slice is shared with the store")
	}
}
