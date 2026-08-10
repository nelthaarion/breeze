package workflow

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nelthaarion/breeze/events"
	"github.com/nelthaarion/breeze/observability"
)

// This file covers the paths the behavioural tests in workflow_test.go
// leave open: introspection accessors, the store's error branches, and
// what the engine does when persistence itself fails. A workflow engine
// that assumes its store always works is the kind that loses executions
// quietly, so those branches are exercised deliberately here.

// --- fault-injecting store ---

// faultyStore wraps a real store and fails a chosen method. Wrapping
// rather than reimplementing keeps the successful paths honest: only the
// injected call behaves differently.
type faultyStore struct {
	*MemoryStore

	mu       sync.Mutex
	failOn   string // method name to fail
	failWith error
	calls    map[string]int
}

func newFaultyStore(failOn string, err error) *faultyStore {
	return &faultyStore{
		MemoryStore: NewMemoryStore(),
		failOn:      failOn,
		failWith:    err,
		calls:       map[string]int{},
	}
}

func (f *faultyStore) shouldFail(method string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[method]++
	if f.failOn == method {
		return f.failWith
	}
	return nil
}

func (f *faultyStore) callCount(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

func (f *faultyStore) CreateWorkflow(ctx context.Context, rec WorkflowRecord) error {
	if err := f.shouldFail("CreateWorkflow"); err != nil {
		return err
	}
	return f.MemoryStore.CreateWorkflow(ctx, rec)
}

func (f *faultyStore) UpdateWorkflow(ctx context.Context, rec WorkflowRecord) error {
	if err := f.shouldFail("UpdateWorkflow"); err != nil {
		return err
	}
	return f.MemoryStore.UpdateWorkflow(ctx, rec)
}

func (f *faultyStore) SaveStep(ctx context.Context, rec StepRecord) error {
	if err := f.shouldFail("SaveStep"); err != nil {
		return err
	}
	return f.MemoryStore.SaveStep(ctx, rec)
}

func (f *faultyStore) PendingWorkflows(ctx context.Context) ([]WorkflowRecord, error) {
	if err := f.shouldFail("PendingWorkflows"); err != nil {
		return nil, err
	}
	return f.MemoryStore.PendingWorkflows(ctx)
}

func (f *faultyStore) FindByIdempotencyKey(ctx context.Context, workflow, key string) (WorkflowRecord, bool, error) {
	if err := f.shouldFail("FindByIdempotencyKey"); err != nil {
		return WorkflowRecord{}, false, err
	}
	return f.MemoryStore.FindByIdempotencyKey(ctx, workflow, key)
}

// --- persistence failures ---

func TestCreateFailureAbortsRun(t *testing.T) {
	boom := errors.New("disk on fire")
	store := newFaultyStore("CreateWorkflow", boom)
	engine := NewEngine(Config{Store: store, Bus: events.New(), DisableObservability: true})

	var ran bool
	def := New("wf").Step("s", func(*Context) error { ran = true; return nil })
	if err := engine.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}

	_, err := engine.Run(context.Background(), "wf", nil)
	if !errors.Is(err, ErrPersistenceFailure) {
		t.Fatalf("err = %v, want ErrPersistenceFailure", err)
	}
	// A workflow whose first write failed must not have side effects:
	// the run never started, so no step may have run.
	if ran {
		t.Error("step ran even though the execution could not be persisted")
	}
}

func TestStepPersistenceFailureDoesNotFailTheRun(t *testing.T) {
	// A step's own work succeeding but its bookkeeping failing should
	// not roll back real side effects. Losing the audit trail is bad;
	// undoing a completed payment because of it is worse.
	store := newFaultyStore("SaveStep", errors.New("write failed"))
	engine := NewEngine(Config{Store: store, Bus: events.New(), DisableObservability: true})

	def := New("wf").Step("s", func(*Context) error { return nil })
	if err := engine.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}

	res, err := engine.Run(context.Background(), "wf", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.State != StateCompleted {
		t.Errorf("State = %v, want completed", res.State)
	}
	if store.callCount("SaveStep") == 0 {
		t.Error("SaveStep was never attempted")
	}
}

func TestResumePropagatesStoreFailure(t *testing.T) {
	boom := errors.New("query failed")
	store := newFaultyStore("PendingWorkflows", boom)
	engine := NewEngine(Config{Store: store, Bus: events.New(), DisableObservability: true})

	n, err := engine.Resume(context.Background())
	if !errors.Is(err, ErrPersistenceFailure) {
		t.Fatalf("err = %v, want ErrPersistenceFailure", err)
	}
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap the store's error", err)
	}
	if n != 0 {
		t.Errorf("resumed = %d, want 0", n)
	}
}

func TestResumeIgnoresUnregisteredWorkflows(t *testing.T) {
	// An execution whose definition no longer exists must not be
	// resurrected, or removing a workflow would be impossible.
	store := NewMemoryStore()
	if err := store.CreateWorkflow(context.Background(), WorkflowRecord{
		ExecutionID: "orphan-1",
		Workflow:    "deleted-workflow",
		State:       StateRunning,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	engine := NewEngine(Config{Store: store, Bus: events.New(), DisableObservability: true})
	n, err := engine.Resume(context.Background())
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if n != 0 {
		t.Errorf("resumed = %d, want 0 for an unregistered workflow", n)
	}
}

func TestResumeRejectedAfterShutdown(t *testing.T) {
	engine := NewEngine(Config{Bus: events.New(), DisableObservability: true})
	if err := engine.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if _, err := engine.Resume(context.Background()); !errors.Is(err, ErrEngineClosed) {
		t.Errorf("err = %v, want ErrEngineClosed", err)
	}
	// Shutdown is idempotent, so a second call is not an error.
	if err := engine.Shutdown(context.Background()); err != nil {
		t.Errorf("second Shutdown: %v", err)
	}
}

func TestIdempotencyLookupFailureStillRuns(t *testing.T) {
	// If the key cannot be checked, running is the safer default: a
	// duplicate execution is recoverable, a silently dropped one is
	// not.
	store := newFaultyStore("FindByIdempotencyKey", errors.New("lookup failed"))
	engine := NewEngine(Config{Store: store, Bus: events.New(), DisableObservability: true})

	var runs int
	def := New("wf").Step("s", func(*Context) error { runs++; return nil })
	if err := engine.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := engine.Run(context.Background(), "wf", nil, WithIdempotencyKey("k")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runs != 1 {
		t.Errorf("runs = %d, want 1", runs)
	}
}

// --- run options ---

func TestRunOptionsReachTheContext(t *testing.T) {
	engine := NewEngine(Config{Bus: events.New(), DisableObservability: true})

	var (
		gotExecID string
		gotCorrID string
		gotMeta   string
	)
	def := New("wf").Step("s", func(c *Context) error {
		gotExecID = c.ExecutionID()
		gotCorrID = c.CorrelationID()
		gotMeta, _ = c.MetaString("tenant")
		return nil
	})
	if err := engine.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}

	res, err := engine.Run(context.Background(), "wf", nil,
		WithExecutionID("exec-42"),
		WithCorrelationID("corr-7"),
		WithMetadata(map[string]any{"tenant": "acme"}),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if gotExecID != "exec-42" {
		t.Errorf("ExecutionID = %q, want \"exec-42\"", gotExecID)
	}
	if res.ExecutionID != "exec-42" {
		t.Errorf("Result.ExecutionID = %q, want \"exec-42\"", res.ExecutionID)
	}
	if gotCorrID != "corr-7" {
		t.Errorf("CorrelationID = %q, want \"corr-7\"", gotCorrID)
	}
	if gotMeta != "acme" {
		t.Errorf("MetaString(tenant) = %q, want \"acme\"", gotMeta)
	}
}

func TestContextAccessors(t *testing.T) {
	engine := NewEngine(Config{Bus: events.New(), DisableObservability: true})

	type order struct{ ID int }
	var checked bool

	def := New("wf").Step("only", func(c *Context) error {
		checked = true
		if got := c.Workflow(); got != "wf" {
			t.Errorf("Workflow() = %q, want \"wf\"", got)
		}
		if got := c.Step(); got != "only" {
			t.Errorf("Step() = %q, want \"only\"", got)
		}
		if got := c.Attempt(); got != 1 {
			t.Errorf("Attempt() = %d, want 1", got)
		}
		// Done() and Deadline() pass straight through to the parent
		// context. This workflow has no timeout and was started from
		// Background, so a nil channel and no deadline are correct:
		// there is genuinely nothing that can cancel it.
		if err := c.Err(); err != nil {
			t.Errorf("Err() = %v, want nil", err)
		}
		if _, ok := c.Deadline(); ok {
			t.Error("Deadline() reported a deadline on a context without one")
		}

		// The generic accessor is the typed way in; the untyped one
		// must agree with it.
		got, ok := Payload[order](c)
		if !ok || got.ID != 9 {
			t.Errorf("Payload[order] = (%v, %v), want ({9}, true)", got, ok)
		}
		if _, ok := Payload[string](c); ok {
			t.Error("Payload[string] succeeded on an order payload")
		}

		c.Set("k", "v")
		if v, ok := c.Get("k"); !ok || v != "v" {
			t.Errorf("Get(k) = (%v, %v), want (v, true)", v, ok)
		}
		if _, ok := c.Get("absent"); ok {
			t.Error("Get(absent) reported found")
		}
		if _, ok := c.MetaString("k2"); ok {
			t.Error("MetaString(k2) reported found before it was set")
		}
		c.Set("k2", 5)
		if _, ok := c.MetaString("k2"); ok {
			t.Error("MetaString(k2) returned a string for an int value")
		}

		// Metadata returns a copy: mutating it must not reach back.
		meta := c.Metadata()
		meta["injected"] = true
		if _, ok := c.Get("injected"); ok {
			t.Error("Metadata() exposed the live map; a caller can corrupt execution state")
		}
		return nil
	})
	if err := engine.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := engine.Run(context.Background(), "wf", order{ID: 9}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !checked {
		t.Fatal("step never ran")
	}
}

func TestPayloadOnNil(t *testing.T) {
	// This is the resume case: a resumed execution carries no payload,
	// and the typed accessor must report that rather than panic.
	c := newContext(context.Background(), "wf", "e1", "", nil, nil)
	if got, ok := Payload[int](c); ok {
		t.Errorf("Payload[int] on nil = (%v, true), want (0, false)", got)
	}
}

// --- introspection ---

func TestEngineIntrospection(t *testing.T) {
	engine := NewEngine(Config{Bus: events.New(), DisableObservability: true})
	def := New("wf").Version(3).
		Step("a", func(*Context) error { return nil }).
		Step("b", func(*Context) error { return nil },
			WithDependsOn("a"), WithCompensation(func(*Context) error { return nil }))
	if err := engine.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}

	names := engine.Definitions()
	if len(names) != 1 || names[0] != "wf" {
		t.Errorf("Definitions() = %v, want [wf]", names)
	}
	got, ok := engine.Definition("wf")
	if !ok || got.Name() != "wf" {
		t.Fatalf("Definition(wf) = (%v, %v)", got, ok)
	}
	if _, ok := engine.Definition("absent"); ok {
		t.Error("Definition(absent) reported found")
	}

	if got.Len() != 2 {
		t.Errorf("Len() = %d, want 2", got.Len())
	}
	steps := got.Steps()
	if len(steps) != 2 {
		t.Fatalf("Steps() returned %d entries, want 2", len(steps))
	}

	if got.steps[0].Name() != "a" {
		t.Errorf("step[0].Name() = %q, want \"a\"", got.steps[0].Name())
	}
	if got.steps[0].HasCompensation() {
		t.Error("step a reports a compensation it does not have")
	}
	if !got.steps[1].HasCompensation() {
		t.Error("step b reports no compensation though one was set")
	}
	deps := got.steps[1].DependsOn()
	if len(deps) != 1 || deps[0] != "a" {
		t.Errorf("DependsOn() = %v, want [a]", deps)
	}
	// The slice is a copy, so a caller cannot rewrite the DAG.
	deps[0] = "hacked"
	if got.steps[1].DependsOn()[0] != "a" {
		t.Error("DependsOn() exposed the live slice; the dependency graph is mutable from outside")
	}
}

func TestRegisterRejectsNilAndClosedEngine(t *testing.T) {
	engine := NewEngine(Config{Bus: events.New(), DisableObservability: true})
	if err := engine.Register(nil); !errors.Is(err, ErrInvalidWorkflow) {
		t.Errorf("Register(nil) = %v, want ErrInvalidWorkflow", err)
	}
	if err := engine.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	def := New("late").Step("s", func(*Context) error { return nil })
	if err := engine.Register(def); !errors.Is(err, ErrEngineClosed) {
		t.Errorf("Register after shutdown = %v, want ErrEngineClosed", err)
	}
}

func TestRunUnknownWorkflow(t *testing.T) {
	engine := NewEngine(Config{Bus: events.New(), DisableObservability: true})
	if _, err := engine.Run(context.Background(), "nope", nil); !errors.Is(err, ErrWorkflowNotFound) {
		t.Errorf("err = %v, want ErrWorkflowNotFound", err)
	}
}

// --- retry policy and errors ---

func TestRetryPolicyEdges(t *testing.T) {
	// A disabled policy must not retry, whatever the error.
	var off RetryPolicy
	if off.enabled() {
		t.Error("zero RetryPolicy reports enabled")
	}
	if off.ShouldRetry(1, errors.New("x")) {
		t.Error("zero RetryPolicy retried")
	}

	p := RetryPolicy{MaxAttempts: 3, InitialDelay: 10 * time.Millisecond}
	if p.ShouldRetry(3, errors.New("x")) {
		t.Error("retried on the final attempt")
	}
	if !p.ShouldRetry(1, errors.New("x")) {
		t.Error("did not retry on the first attempt")
	}
	// A nil error is success, and success is never retried.
	if p.ShouldRetry(1, nil) {
		t.Error("retried a nil error")
	}
	// Non-retryable overrides the attempt budget entirely.
	if p.ShouldRetry(1, NonRetryable(errors.New("fatal"))) {
		t.Error("retried a non-retryable error")
	}

	// A zero or negative attempt is out of contract but must not panic
	// or produce a negative delay.
	if d := p.Delay(0); d < 0 {
		t.Errorf("Delay(0) = %v, want >= 0", d)
	}
}

func TestNonRetryableWrapping(t *testing.T) {
	if NonRetryable(nil) != nil {
		t.Error("NonRetryable(nil) != nil")
	}
	base := errors.New("root cause")
	err := NonRetryable(base)
	if !IsNonRetryable(err) {
		t.Error("IsNonRetryable = false")
	}
	// The original error must stay matchable, or callers lose the
	// ability to branch on why something failed.
	if !errors.Is(err, base) {
		t.Error("wrapped error no longer matches its cause")
	}
	if err.Error() != base.Error() {
		t.Errorf("Error() = %q, want %q", err.Error(), base.Error())
	}
	if IsNonRetryable(base) {
		t.Error("a plain error reported as non-retryable")
	}
}

func TestStepErrorFormatting(t *testing.T) {
	cause := errors.New("boom")
	err := &StepError{
		Workflow: "wf", ExecutionID: "e1", Step: "charge", Attempt: 2, Err: cause,
	}
	msg := err.Error()
	for _, want := range []string{"wf", "charge", "boom"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Error() = %q, missing %q", msg, want)
		}
	}
	if !errors.Is(err, cause) {
		t.Error("StepError does not unwrap to its cause")
	}
}

func TestBackoffString(t *testing.T) {
	// Every declared backoff must render as something other than the
	// fallback, or the dashboard shows a meaningless label.
	seen := map[string]bool{}
	for _, b := range []Backoff{BackoffFixed, BackoffExponential} {
		s := b.String()
		if s == "" {
			t.Errorf("Backoff(%d).String() is empty", b)
		}
		if seen[s] {
			t.Errorf("Backoff(%d).String() = %q, duplicated", b, s)
		}
		seen[s] = true
	}
	if got := Backoff(99).String(); got == "" {
		t.Error("unknown Backoff renders as empty rather than a fallback")
	}
}

func TestStateStringAndTerminal(t *testing.T) {
	all := []State{
		StatePending, StateRunning, StateWaiting, StateCompleted, StateFailed,
		StateCancelled, StateCompensating, StateCompensated, StateTimedOut,
	}
	for _, s := range all {
		if s.String() == "" {
			t.Errorf("State(%d).String() is empty", s)
		}
	}
	if got := State(99).String(); got == "" {
		t.Error("unknown State renders as empty rather than a fallback")
	}

	// A terminal state accepts no transition to a *different* state;
	// that is what stops a late write resurrecting a finished
	// execution. Re-writing the same state stays allowed so that a
	// duplicate update is idempotent rather than an error.
	for _, s := range all {
		if !s.Terminal() {
			continue
		}
		if !s.CanTransitionTo(s) {
			t.Errorf("%v cannot transition to itself; a duplicate write would be rejected", s)
		}
		for _, next := range all {
			if next == s {
				continue
			}
			if s.CanTransitionTo(next) {
				t.Errorf("%v is terminal but allows a transition to %v", s, next)
			}
		}
	}
	if !StatePending.CanTransitionTo(StateRunning) {
		t.Error("pending cannot become running")
	}
	if StatePending.CanTransitionTo(StateCompleted) {
		t.Error("pending jumped straight to completed, skipping running")
	}
}

// --- store ---

func TestMemoryStoreCRUD(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()

	rec := WorkflowRecord{ExecutionID: "e1", Workflow: "wf", State: StateRunning}
	if err := s.CreateWorkflow(ctx, rec); err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}
	if err := s.CreateWorkflow(ctx, rec); !errors.Is(err, ErrWorkflowAlreadyExist) {
		t.Errorf("duplicate create = %v, want ErrWorkflowAlreadyExist", err)
	}
	if s.Len() != 1 {
		t.Errorf("Len() = %d, want 1", s.Len())
	}

	got, err := s.GetWorkflow(ctx, "e1")
	if err != nil {
		t.Fatalf("GetWorkflow: %v", err)
	}
	if got.Workflow != "wf" {
		t.Errorf("Workflow = %q, want \"wf\"", got.Workflow)
	}
	if _, err := s.GetWorkflow(ctx, "absent"); !errors.Is(err, ErrWorkflowNotFound) {
		t.Errorf("GetWorkflow(absent) = %v, want ErrWorkflowNotFound", err)
	}

	rec.State = StateCompleted
	if err := s.UpdateWorkflow(ctx, rec); err != nil {
		t.Fatalf("UpdateWorkflow: %v", err)
	}
	if err := s.UpdateWorkflow(ctx, WorkflowRecord{ExecutionID: "absent"}); !errors.Is(err, ErrWorkflowNotFound) {
		t.Errorf("UpdateWorkflow(absent) = %v, want ErrWorkflowNotFound", err)
	}

	step := StepRecord{ExecutionID: "e1", Step: "s1", State: StateCompleted}
	if err := s.SaveStep(ctx, step); err != nil {
		t.Fatalf("SaveStep: %v", err)
	}
	gotStep, err := s.GetStep(ctx, "e1", "s1")
	if err != nil {
		t.Fatalf("GetStep: %v", err)
	}
	if gotStep.State != StateCompleted {
		t.Errorf("step State = %v, want completed", gotStep.State)
	}
	if _, err := s.GetStep(ctx, "e1", "absent"); !errors.Is(err, ErrWorkflowNotFound) {
		t.Errorf("GetStep(absent step) = %v, want ErrWorkflowNotFound", err)
	}
	if _, err := s.GetStep(ctx, "absent", "s1"); !errors.Is(err, ErrWorkflowNotFound) {
		t.Errorf("GetStep(absent execution) = %v, want ErrWorkflowNotFound", err)
	}

	steps, err := s.ListSteps(ctx, "e1")
	if err != nil || len(steps) != 1 {
		t.Fatalf("ListSteps = (%v, %v), want 1 step", steps, err)
	}

	// Deleting an execution must take its steps with it, or the store
	// leaks step records forever.
	if err := s.DeleteWorkflow(ctx, "e1"); err != nil {
		t.Fatalf("DeleteWorkflow: %v", err)
	}
	if _, err := s.GetWorkflow(ctx, "e1"); !errors.Is(err, ErrWorkflowNotFound) {
		t.Errorf("execution survived deletion: %v", err)
	}
	if left, _ := s.ListSteps(ctx, "e1"); len(left) != 0 {
		t.Errorf("steps survived their execution's deletion: %v", left)
	}
	// Deleting something already gone is not an error: the caller's
	// intent is satisfied either way, which keeps cleanup retryable.
	if err := s.DeleteWorkflow(ctx, "absent"); err != nil {
		t.Errorf("DeleteWorkflow(absent) = %v, want nil (delete is idempotent)", err)
	}

	if err := s.CreateWorkflow(ctx, WorkflowRecord{ExecutionID: "e2", Workflow: "wf"}); err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}
	s.Reset()
	if s.Len() != 0 {
		t.Errorf("Len() after Reset = %d, want 0", s.Len())
	}
}

func TestMemoryStoreIsolatesRecords(t *testing.T) {
	// Records are returned by value with their maps copied, so a caller
	// cannot reach back into stored state.
	ctx := context.Background()
	s := NewMemoryStore()
	if err := s.CreateWorkflow(ctx, WorkflowRecord{
		ExecutionID: "e1", Workflow: "wf", State: StateRunning,
	}); err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}

	got, _ := s.GetWorkflow(ctx, "e1")
	got.State = StateFailed
	got.Workflow = "tampered"

	again, _ := s.GetWorkflow(ctx, "e1")
	if again.State != StateRunning || again.Workflow != "wf" {
		t.Errorf("stored record was mutated through a returned copy: %+v", again)
	}
}

func TestPendingWorkflowsExcludesTerminal(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	seed := []struct {
		id    string
		state State
	}{
		{"running", StateRunning},
		{"pending", StatePending},
		{"waiting", StateWaiting},
		{"done", StateCompleted},
		{"failed", StateFailed},
		{"cancelled", StateCancelled},
	}
	for _, sd := range seed {
		if err := s.CreateWorkflow(ctx, WorkflowRecord{
			ExecutionID: sd.id, Workflow: "wf", State: sd.state,
		}); err != nil {
			t.Fatalf("seed %s: %v", sd.id, err)
		}
	}

	pending, err := s.PendingWorkflows(ctx)
	if err != nil {
		t.Fatalf("PendingWorkflows: %v", err)
	}
	if len(pending) != 3 {
		t.Fatalf("got %d pending, want 3 (running, pending, waiting)", len(pending))
	}
	for _, p := range pending {
		if p.State.Terminal() {
			t.Errorf("terminal execution %q returned as pending", p.ExecutionID)
		}
	}
}

func TestFindByIdempotencyKeyScopedToWorkflow(t *testing.T) {
	// The same key under two workflows must not collide, or one
	// workflow's key would suppress another's execution.
	ctx := context.Background()
	s := NewMemoryStore()
	if err := s.CreateWorkflow(ctx, WorkflowRecord{
		ExecutionID: "e1", Workflow: "a", IdempotencyKey: "k",
	}); err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}

	if _, found, _ := s.FindByIdempotencyKey(ctx, "a", "k"); !found {
		t.Error("key not found under its own workflow")
	}
	if _, found, _ := s.FindByIdempotencyKey(ctx, "b", "k"); found {
		t.Error("key leaked across workflows")
	}
	if _, found, _ := s.FindByIdempotencyKey(ctx, "a", "other"); found {
		t.Error("unknown key reported found")
	}
}

// --- definition validation ---

func TestValidationRejectsBadRetryPolicies(t *testing.T) {
	cases := []struct {
		name   string
		policy RetryPolicy
	}{
		{"negative attempts", RetryPolicy{MaxAttempts: -1}},
		{"negative delay", RetryPolicy{MaxAttempts: 2, InitialDelay: -time.Second}},
		{"max below initial", RetryPolicy{MaxAttempts: 2, InitialDelay: time.Minute, MaxDelay: time.Second}},
		{"negative jitter", RetryPolicy{MaxAttempts: 2, InitialDelay: time.Second, Jitter: -1}},
		{"jitter above one", RetryPolicy{MaxAttempts: 2, InitialDelay: time.Second, Jitter: 2}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			def := New("wf").Step("s", func(*Context) error { return nil }, WithRetry(tc.policy))
			if err := def.Validate(); err == nil {
				t.Errorf("Validate() accepted %+v", tc.policy)
			}
		})
	}
}

func TestPlanIsCachedAndInvalidated(t *testing.T) {
	def := New("wf").Step("a", func(*Context) error { return nil })
	first, err := def.plan()
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	second, err := def.plan()
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	// The plan is computed at registration, not per dispatch, so the
	// second call must reuse the first.
	if &first[0][0] != &second[0][0] {
		t.Error("plan() recomputed instead of returning the cached layers")
	}

	def.Step("b", func(*Context) error { return nil }, WithDependsOn("a"))
	third, err := def.plan()
	if err != nil {
		t.Fatalf("plan after mutation: %v", err)
	}
	if len(third) != 2 {
		t.Errorf("layers = %d, want 2; the cache was not invalidated by a new step", len(third))
	}
}

// --- signals ---

func TestPublishSignalCarriesExecutionDetail(t *testing.T) {
	col := observability.NewCollector(observability.Config{Capacity: 16})
	engine := NewEngine(Config{Bus: events.New(), Collector: col})

	def := New("billing").Version(2).
		Step("a", func(*Context) error { return nil }).
		Step("b", func(*Context) error { return nil }, WithDependsOn("a"))
	if err := engine.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}
	res, err := engine.Run(context.Background(), "billing", nil, WithCorrelationID("corr-1"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	found := col.Find(observability.Query{Source: observability.SourceWorkflow, Limit: 10})
	if len(found) == 0 {
		t.Fatal("no workflow signal was published")
	}
	sig := found[0]
	if sig.Name != "billing" {
		t.Errorf("Name = %q, want \"billing\"", sig.Name)
	}
	if sig.CorrelationID != "corr-1" {
		t.Errorf("CorrelationID = %q, want \"corr-1\"", sig.CorrelationID)
	}
	if sig.Attrs["execution_id"] != res.ExecutionID {
		t.Errorf("execution_id = %q, want %q", sig.Attrs["execution_id"], res.ExecutionID)
	}
	if sig.Attrs["state"] != "completed" {
		t.Errorf("state = %q, want \"completed\"", sig.Attrs["state"])
	}
	if sig.Attrs["version"] != "2" {
		t.Errorf("version = %q, want \"2\"", sig.Attrs["version"])
	}
	if sig.Attrs["attempts"] != "2" {
		t.Errorf("attempts = %q, want \"2\" (one per step)", sig.Attrs["attempts"])
	}
	if len(sig.Spans) != 2 {
		t.Errorf("spans = %d, want 2", len(sig.Spans))
	}
	// Every span must carry a level, since the dashboard groups on it.
	for _, sp := range sig.Spans {
		if sp.Phase == "" {
			t.Errorf("span %q has no phase; it cannot be placed on the timeline", sp.Name)
		}
	}
}

func TestSignalRecordsFailedStep(t *testing.T) {
	col := observability.NewCollector(observability.Config{Capacity: 16})
	engine := NewEngine(Config{Bus: events.New(), Collector: col})

	def := New("wf").
		Step("ok", func(*Context) error { return nil }).
		Step("bad", func(*Context) error {
			return NonRetryable(errors.New("nope"))
		}, WithDependsOn("ok"))
	if err := engine.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := engine.Run(context.Background(), "wf", nil); err == nil {
		t.Fatal("Run returned nil for a failing workflow")
	}

	found := col.Find(observability.Query{Source: observability.SourceWorkflow, Limit: 10})
	if len(found) == 0 {
		t.Fatal("no workflow signal was published")
	}
	sig := found[0]
	if !sig.Failed {
		t.Error("Failed = false for a failing workflow")
	}
	if sig.Attrs["failed_step"] != "bad" {
		t.Errorf("failed_step = %q, want \"bad\"", sig.Attrs["failed_step"])
	}
}

func TestDisableObservabilityPublishesNothing(t *testing.T) {
	col := observability.NewCollector(observability.Config{Capacity: 8})
	// DisableObservability must win even when a collector is supplied,
	// otherwise "off" would depend on how the config was written.
	engine := NewEngine(Config{
		Bus: events.New(), Collector: col, DisableObservability: true,
	})
	def := New("wf").Step("s", func(*Context) error { return nil })
	if err := engine.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := engine.Run(context.Background(), "wf", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := col.Find(observability.Query{Limit: 10}); len(got) != 0 {
		t.Errorf("published %d signals with observability disabled", len(got))
	}
}

// --- compensation retries ---

func TestCompensationRetriesThenGivesUp(t *testing.T) {
	engine := NewEngine(Config{Bus: events.New(), DisableObservability: true})

	var attempts int
	def := New("wf").
		Step("a", func(*Context) error { return nil },
			WithCompensation(func(*Context) error {
				attempts++
				return errors.New("rollback failed")
			}),
			WithCompensationRetry(RetryPolicy{MaxAttempts: 3, InitialDelay: time.Millisecond}),
		).
		Step("b", func(*Context) error {
			return NonRetryable(errors.New("fail"))
		}, WithDependsOn("a"))
	if err := engine.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}

	res, _ := engine.Run(context.Background(), "wf", nil)
	if attempts != 3 {
		t.Errorf("compensation attempts = %d, want 3", attempts)
	}
	// Rollback failing means the execution is not compensated; calling
	// it compensated would claim a cleanup that never happened.
	if res.State == StateCompensated {
		t.Error("State = compensated though every rollback attempt failed")
	}
}

func TestCompensationContinuesAfterOneFails(t *testing.T) {
	// One failing rollback must not abandon the others: the remaining
	// side effects still need undoing.
	engine := NewEngine(Config{Bus: events.New(), DisableObservability: true})

	var firstRan bool
	def := New("wf").
		Step("first", func(*Context) error { return nil },
			WithCompensation(func(*Context) error { firstRan = true; return nil })).
		Step("second", func(*Context) error { return nil },
			WithDependsOn("first"),
			WithCompensation(func(*Context) error { return errors.New("cannot undo") })).
		Step("third", func(*Context) error {
			return NonRetryable(errors.New("fail"))
		}, WithDependsOn("second"))
	if err := engine.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := engine.Run(context.Background(), "wf", nil); err == nil {
		t.Fatal("Run returned nil for a failing workflow")
	}
	if !firstRan {
		t.Error("the first step's compensation was skipped after a later one failed")
	}
}

func TestCompensationRunsAfterCancellation(t *testing.T) {
	// Rollback runs on a detached context, so a cancelled execution
	// still cleans up. Without that, cancelling leaks side effects.
	engine := NewEngine(Config{Bus: events.New(), DisableObservability: true})

	ctx, cancel := context.WithCancel(context.Background())
	var compensated bool
	release := make(chan struct{})

	def := New("wf").
		Step("a", func(*Context) error { return nil },
			WithCompensation(func(c *Context) error {
				if c.Err() != nil {
					t.Error("compensation received a cancelled context")
				}
				compensated = true
				return nil
			})).
		Step("b", func(c *Context) error {
			cancel()
			close(release)
			<-c.Done()
			return c.Err()
		}, WithDependsOn("a"))
	if err := engine.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}

	res, err := engine.Run(ctx, "wf", nil)
	<-release
	if err == nil {
		t.Fatal("Run returned nil for a cancelled workflow")
	}
	if !compensated {
		t.Error("compensation did not run after cancellation")
	}
	if res.State != StateCompensated {
		t.Errorf("State = %v, want compensated", res.State)
	}
}

// --- timeouts ---

func TestWorkflowTimeoutIsReportedAsSuch(t *testing.T) {
	engine := NewEngine(Config{Bus: events.New(), DisableObservability: true})
	def := New("wf").Timeout(20 * time.Millisecond).
		Step("slow", func(c *Context) error {
			<-c.Done()
			return c.Err()
		})
	if err := engine.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}

	res, err := engine.Run(context.Background(), "wf", nil)
	if !errors.Is(err, ErrWorkflowTimeout) {
		t.Errorf("err = %v, want ErrWorkflowTimeout", err)
	}
	if res.State != StateTimedOut {
		t.Errorf("State = %v, want timed_out", res.State)
	}
}

func TestRetryWaitIsInterruptedByCancellation(t *testing.T) {
	// A shutdown must not have to outlast a backoff, so the wait
	// between attempts is cancellable.
	engine := NewEngine(Config{Bus: events.New(), DisableObservability: true})
	ctx, cancel := context.WithCancel(context.Background())

	def := New("wf").Step("flaky", func(*Context) error {
		return errors.New("always fails")
	}, WithRetry(RetryPolicy{MaxAttempts: 5, InitialDelay: 30 * time.Second}))
	if err := engine.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}

	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	if _, err := engine.Run(ctx, "wf", nil); err == nil {
		t.Fatal("Run returned nil after cancellation")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Run took %v; the backoff was not interruptible", elapsed)
	}
}
