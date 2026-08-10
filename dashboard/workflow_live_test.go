package dashboard

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/nelthaarion/breeze/events"
)

// These tests drive the live tracker through a real bus rather than
// calling its methods directly. The tracker's whole purpose is to turn a
// stream of step events into in-flight state, so the event plumbing is
// part of what needs to be correct — a test that skipped the bus could
// pass while the subscriptions were wired to the wrong types.

// emitStart begins an execution with the given step names.
func emitStart(t *testing.T, bus *events.Bus, execID, wf string, steps ...string) {
	t.Helper()
	if err := events.EmitBus(bus, events.WorkflowStarted{
		ExecutionID: execID,
		Workflow:    wf,
		Trigger:     "test",
		StepNames:   steps,
		Time:        time.Now(),
	}); err != nil {
		t.Fatalf("emit WorkflowStarted: %v", err)
	}
}

// findExec returns the tracked execution with the given ID.
func findExec(t *testing.T, w *workflowLive, execID string) liveExecution {
	t.Helper()
	for _, ex := range w.Snapshot() {
		if ex.ExecutionID == execID {
			return ex
		}
	}
	t.Fatalf("execution %q not tracked; snapshot=%+v", execID, w.Snapshot())
	return liveExecution{}
}

// stepState returns the state of a named step.
func stepState(t *testing.T, ex liveExecution, name string) liveStepState {
	t.Helper()
	for _, s := range ex.Steps {
		if s.Name == name {
			return s.State
		}
	}
	t.Fatalf("step %q missing from %+v", name, ex.Steps)
	return ""
}

func TestWorkflowLiveTracksStepProgress(t *testing.T) {
	bus := events.New(events.Config{})
	defer bus.Close()

	live := newWorkflowLive()
	detach := live.attach(bus)
	defer detach()

	emitStart(t, bus, "e1", "checkout", "reserve", "charge", "ship")

	// Every step is seeded as pending, so the whole chain is drawable
	// from the first frame rather than appearing one node at a time.
	ex := findExec(t, live, "e1")
	if len(ex.Steps) != 3 {
		t.Fatalf("want 3 seeded steps, got %d", len(ex.Steps))
	}
	for _, s := range ex.Steps {
		if s.State != liveStepPending {
			t.Errorf("step %q seeded as %q, want pending", s.Name, s.State)
		}
	}
	if ex.Workflow != "checkout" || ex.Trigger != "test" {
		t.Errorf("identity not captured: %+v", ex)
	}

	// A running step must be visible as running — this is the state the
	// ring buffer can never show, and the reason the tracker exists.
	_ = events.EmitBus(bus, events.WorkflowStepStarted{ExecutionID: "e1", Step: "reserve", Attempt: 1})
	if got := stepState(t, findExec(t, live, "e1"), "reserve"); got != liveStepRunning {
		t.Errorf("reserve = %q, want running", got)
	}

	_ = events.EmitBus(bus, events.WorkflowStepCompleted{
		ExecutionID: "e1", Step: "reserve", Attempt: 1, Duration: 2 * time.Millisecond,
	})
	ex = findExec(t, live, "e1")
	if got := stepState(t, ex, "reserve"); got != liveStepDone {
		t.Errorf("reserve = %q, want done", got)
	}
	// Order is preserved, so the chain renders in execution order.
	if ex.Steps[0].Name != "reserve" || ex.Steps[2].Name != "ship" {
		t.Errorf("step order not preserved: %+v", ex.Steps)
	}
	if ex.Steps[0].DurationMS <= 0 {
		t.Errorf("completed step has no duration: %+v", ex.Steps[0])
	}
	if got := stepState(t, ex, "ship"); got != liveStepPending {
		t.Errorf("untouched step = %q, want pending", got)
	}
}

func TestWorkflowLiveRetryReadsAsRetrying(t *testing.T) {
	bus := events.New(events.Config{})
	defer bus.Close()

	live := newWorkflowLive()
	defer live.attach(bus)()

	emitStart(t, bus, "e2", "sync", "push")

	// A failure that will be retried is not the step's verdict yet.
	// Showing it as failed would report a false outcome mid-run.
	_ = events.EmitBus(bus, events.WorkflowStepFailed{
		ExecutionID: "e2", Step: "push", Attempt: 1, Err: "timeout", Retryable: true,
	})
	if got := stepState(t, findExec(t, live, "e2"), "push"); got != liveStepRetrying {
		t.Errorf("retryable failure = %q, want retrying", got)
	}

	// The final attempt decides.
	_ = events.EmitBus(bus, events.WorkflowStepFailed{
		ExecutionID: "e2", Step: "push", Attempt: 3, Err: "timeout", Retryable: false,
	})
	ex := findExec(t, live, "e2")
	if got := stepState(t, ex, "push"); got != liveStepFailed {
		t.Errorf("terminal failure = %q, want failed", got)
	}
	if ex.Steps[0].Attempt != 3 || ex.Steps[0].Err != "timeout" {
		t.Errorf("attempt/error not carried: %+v", ex.Steps[0])
	}
}

func TestWorkflowLiveCompensationMarksRolledBack(t *testing.T) {
	bus := events.New(events.Config{})
	defer bus.Close()

	live := newWorkflowLive()
	defer live.attach(bus)()

	emitStart(t, bus, "e3", "order", "reserve", "charge")
	_ = events.EmitBus(bus, events.WorkflowStepCompleted{ExecutionID: "e3", Step: "reserve", Attempt: 1})
	_ = events.EmitBus(bus, events.WorkflowCompensationStarted{ExecutionID: "e3"})

	if !findExec(t, live, "e3").Compensating {
		t.Error("Compensating not set; rollback is a distinct state from failure")
	}

	_ = events.EmitBus(bus, events.WorkflowCompensationCompleted{ExecutionID: "e3"})

	// Work that has been undone must not still read as done, or the page
	// would claim an effect that no longer holds.
	if got := stepState(t, findExec(t, live, "e3"), "reserve"); got != liveStepRolled {
		t.Errorf("compensated step = %q, want %q", got, liveStepRolled)
	}
}

func TestWorkflowLiveFinishClearsRunningSteps(t *testing.T) {
	bus := events.New(events.Config{})
	defer bus.Close()

	live := newWorkflowLive()
	defer live.attach(bus)()

	emitStart(t, bus, "e4", "job", "work")
	_ = events.EmitBus(bus, events.WorkflowStepStarted{ExecutionID: "e4", Step: "work", Attempt: 1})
	// The execution ends while a step is still marked running, which
	// happens on timeout and cancellation.
	_ = events.EmitBus(bus, events.WorkflowTimedOut{ExecutionID: "e4"})

	ex := findExec(t, live, "e4")
	if !ex.Done {
		t.Error("Done not set after a terminal event")
	}
	// A stranded spinner would spin forever on the page.
	if got := stepState(t, ex, "work"); got == liveStepRunning {
		t.Error("step still running after execution ended")
	}
}

func TestWorkflowLiveSweepDropsFinished(t *testing.T) {
	bus := events.New(events.Config{})
	defer bus.Close()

	live := newWorkflowLive()
	defer live.attach(bus)()

	emitStart(t, bus, "done", "wf", "a")
	emitStart(t, bus, "running", "wf", "a")
	_ = events.EmitBus(bus, events.WorkflowCompleted{ExecutionID: "done"})

	// A negative TTL puts the cutoff in the future, so everything already
	// finished is unambiguously older than it. A zero TTL would sit exactly
	// on the boundary, where the outcome depends on whether the clock
	// ticked between finishing and sweeping — that is a property of the
	// platform's timer granularity, not of the tracker.
	live.sweep(-time.Second)

	ids := map[string]bool{}
	for _, ex := range live.Snapshot() {
		ids[ex.ExecutionID] = true
	}
	if ids["done"] {
		t.Error("finished execution survived the sweep")
	}
	if !ids["running"] {
		t.Error("sweep dropped an execution that is still running")
	}
}

func TestWorkflowLiveEvictsWhenOverCapacity(t *testing.T) {
	live := newWorkflowLive()

	// Unbounded tracking would be a memory leak: a busy application can
	// start executions faster than a browser reads them.
	for i := 0; i < liveExecMax+20; i++ {
		live.start(events.WorkflowStarted{
			ExecutionID: "e" + strconv.Itoa(i),
			Workflow:    "wf",
			StepNames:   []string{"a"},
		})
	}
	if n := len(live.Snapshot()); n > liveExecMax {
		t.Errorf("tracked %d executions, cap is %d", n, liveExecMax)
	}

	// Newest-first ordering means the most recent execution survives
	// eviction and appears at the top.
	if got := live.Snapshot()[0].ExecutionID; got != "e"+strconv.Itoa(liveExecMax+19) {
		t.Errorf("newest execution not first, got %q", got)
	}
}

func TestWorkflowLiveUnknownExecutionIgnored(t *testing.T) {
	bus := events.New(events.Config{})
	defer bus.Close()

	live := newWorkflowLive()
	defer live.attach(bus)()

	// The dashboard may attach midway through a run. Events for an
	// execution whose plan was never seen must be ignored rather than
	// resurrecting a partial execution with no steps.
	_ = events.EmitBus(bus, events.WorkflowStepStarted{ExecutionID: "ghost", Step: "x", Attempt: 1})
	_ = events.EmitBus(bus, events.WorkflowCompleted{ExecutionID: "ghost"})

	if n := len(live.Snapshot()); n != 0 {
		t.Errorf("untracked execution created state: %+v", live.Snapshot())
	}
}

func TestWorkflowLiveUnplannedStepAppended(t *testing.T) {
	bus := events.New(events.Config{})
	defer bus.Close()

	live := newWorkflowLive()
	defer live.attach(bus)()

	emitStart(t, bus, "e5", "wf", "a")
	// A step the plan did not mention still carries information, so it
	// is appended rather than dropped.
	_ = events.EmitBus(bus, events.WorkflowStepStarted{ExecutionID: "e5", Step: "surprise", Attempt: 1})

	ex := findExec(t, live, "e5")
	if len(ex.Steps) != 2 {
		t.Fatalf("want 2 steps, got %+v", ex.Steps)
	}
	if got := stepState(t, ex, "surprise"); got != liveStepRunning {
		t.Errorf("appended step = %q, want running", got)
	}
}

func TestWorkflowLiveSnapshotIsolatesSteps(t *testing.T) {
	bus := events.New(events.Config{})
	defer bus.Close()

	live := newWorkflowLive()
	defer live.attach(bus)()

	emitStart(t, bus, "e6", "wf", "a")
	snap := live.Snapshot()

	// A caller marshalling a snapshot must not race with the next event
	// mutating the slice underneath it.
	_ = events.EmitBus(bus, events.WorkflowStepCompleted{ExecutionID: "e6", Step: "a", Attempt: 1})

	if snap[0].Steps[0].State != liveStepPending {
		t.Error("snapshot shares step storage with the tracker")
	}
}

func TestWorkflowLiveConcurrentEvents(t *testing.T) {
	bus := events.New(events.Config{})
	defer bus.Close()

	live := newWorkflowLive()
	defer live.attach(bus)()

	// Many executions progressing at once is the normal case for a
	// workflow engine, and the tracker is read by HTTP handlers while
	// those events arrive. Run under -race.
	const n = 24
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "c" + strconv.Itoa(i)
			live.start(events.WorkflowStarted{
				ExecutionID: id, Workflow: "wf", StepNames: []string{"a", "b"},
			})
			_ = events.EmitBus(bus, events.WorkflowStepStarted{ExecutionID: id, Step: "a", Attempt: 1})
			_ = events.EmitBus(bus, events.WorkflowStepCompleted{ExecutionID: id, Step: "a", Attempt: 1})
			_ = events.EmitBus(bus, events.WorkflowCompleted{ExecutionID: id})
		}(i)
	}
	// Concurrent readers, as the API handler would be.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 40; j++ {
				live.Snapshot()
			}
		}()
	}
	wg.Wait()

	for _, ex := range live.Snapshot() {
		if !ex.Done {
			t.Errorf("execution %q not marked done", ex.ExecutionID)
		}
	}
}

func TestWorkflowLiveNilSafe(t *testing.T) {
	// A dashboard with no bus attached holds a nil tracker, and every
	// read path must tolerate that rather than panicking.
	var w *workflowLive
	if got := w.Snapshot(); got != nil {
		t.Errorf("nil tracker returned %+v", got)
	}
	if detach := w.attach(nil); detach == nil {
		t.Error("attach returned no detach function")
	} else {
		detach()
	}
}

func TestCollectorLiveWorkflowsWithoutBus(t *testing.T) {
	// The Events payload is requested whether or not a bus is attached,
	// so the accessor must degrade to empty rather than failing.
	c := newCollector(DefaultConfig(), nil)
	if got := c.liveWorkflows(); got != nil {
		t.Errorf("unattached collector returned %+v", got)
	}
}

func TestAttachEventsTracksLiveWorkflows(t *testing.T) {
	bus := events.New(events.Config{})
	defer bus.Close()

	c := newCollector(DefaultConfig(), nil)
	detach := c.AttachEvents(bus)

	emitStart(t, bus, "wired", "checkout", "reserve")
	_ = events.EmitBus(bus, events.WorkflowStepStarted{ExecutionID: "wired", Step: "reserve", Attempt: 1})

	// AttachEvents must wire the tracker, not just the ring buffer,
	// otherwise the Events page has history but no live view.
	live := c.liveWorkflows()
	if len(live) != 1 || live[0].ExecutionID != "wired" {
		t.Fatalf("live executions not exposed: %+v", live)
	}
	if live[0].Steps[0].State != liveStepRunning {
		t.Errorf("step = %q, want running", live[0].Steps[0].State)
	}

	// Detaching must release the tracker along with everything else.
	detach()
	if got := c.liveWorkflows(); got != nil {
		t.Errorf("tracker survived detach: %+v", got)
	}
}
