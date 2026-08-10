package dashboard

import (
	"sort"
	"sync"
	"time"

	"github.com/nelthaarion/breeze/events"
)

// This file tracks workflow executions while they are still running.
//
// The observability ring buffer cannot answer "what is running right
// now": the engine publishes one signal per execution, and it publishes
// it when the execution *ends*. Every row the Events page reads is
// therefore already terminal, which is fine for history and useless for
// a progress view — a step can never be shown mid-flight.
//
// The engine does emit step events on the bus as they happen, so the
// live picture is assembled from those instead. This keeps the engine
// unaware of the dashboard, and keeps in-flight state out of the ring
// buffer, where a half-finished execution would masquerade as history.

// liveStepState is the state of one step in a running execution.
type liveStepState string

const (
	liveStepPending  liveStepState = "pending"
	liveStepRunning  liveStepState = "running"
	liveStepDone     liveStepState = "done"
	liveStepFailed   liveStepState = "failed"
	liveStepRetrying liveStepState = "retrying"
	liveStepRolled   liveStepState = "compensated"
)

// liveStepMax bounds how many executions are tracked at once. A busy
// application can start executions faster than a browser reads them, and
// an unbounded map would be a memory leak dressed up as a feature.
const liveExecMax = 64

// liveStep is one step of a tracked execution.
type liveStep struct {
	Name    string        `json:"name"`
	State   liveStepState `json:"state"`
	Attempt int           `json:"attempt,omitempty"`
	Err     string        `json:"error,omitempty"`

	// DurationMS is set once the step finishes. A running step has no
	// duration yet, and reporting a partial one would make the number
	// change meaning depending on when it was read.
	DurationMS float64 `json:"duration_ms,omitempty"`
}

// liveExecution is one execution in flight.
type liveExecution struct {
	ExecutionID string     `json:"execution_id"`
	Workflow    string     `json:"workflow"`
	Trigger     string     `json:"trigger,omitempty"`
	StartedAt   time.Time  `json:"started_at"`
	Steps       []liveStep `json:"steps"`

	// Compensating reports that the execution failed and is rolling
	// back, which is a different thing to look at than a plain failure.
	Compensating bool `json:"compensating,omitempty"`

	// Done marks an execution whose terminal event has arrived. It is
	// kept briefly so the final frame reaches the browser, then swept.
	Done   bool      `json:"done,omitempty"`
	doneAt time.Time `json:"-"`
	seq    uint64    `json:"-"`

	// stepIdx maps a step name to its slot in Steps, so an event names
	// its step rather than the tracker scanning for it.
	stepIdx map[string]int
}


// workflowLive tracks in-flight executions.
//
// It is a separate type from Collector so the locking stays obvious: one
// mutex, held only for map and slice updates, never across a callback.
type workflowLive struct {
	mu    sync.RWMutex
	execs map[string]*liveExecution
	seq   uint64
}

func newWorkflowLive() *workflowLive {
	return &workflowLive{execs: make(map[string]*liveExecution, 8)}
}

// Snapshot returns the tracked executions, newest first.
func (w *workflowLive) Snapshot() []liveExecution {
	if w == nil {
		return nil
	}
	w.mu.RLock()
	defer w.mu.RUnlock()

	out := make([]liveExecution, 0, len(w.execs))
	for _, e := range w.execs {
		// Copy the steps: a caller marshalling this must not race with
		// the next event mutating the slice underneath it.
		cp := *e
		cp.Steps = append([]liveStep(nil), e.Steps...)
		cp.stepIdx = nil
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].seq > out[j].seq })
	return out
}

// get returns a tracked execution, or nil when it is not tracked. An
// untracked execution is normal rather than exceptional: the dashboard
// may have attached midway through a run, and events for it must be
// ignored rather than resurrecting a partial execution with no plan.
func (w *workflowLive) get(execID string) *liveExecution {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.execs[execID]
}

// start begins tracking an execution, seeding every step as pending so
// the whole chain is drawable from the first frame.
func (w *workflowLive) start(e events.WorkflowStarted) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.seq++
	ex := &liveExecution{
		ExecutionID: e.ExecutionID,
		Workflow:    e.Workflow,
		Trigger:     e.Trigger,
		StartedAt:   e.Time,
		seq:         w.seq,
		stepIdx:     make(map[string]int, len(e.StepNames)),
	}
	for i, name := range e.StepNames {
		ex.Steps = append(ex.Steps, liveStep{Name: name, State: liveStepPending})
		ex.stepIdx[name] = i
	}
	w.execs[e.ExecutionID] = ex
	w.evictLocked()
}

// evictLocked drops the oldest finished executions, then the oldest
// running ones, once the map exceeds its bound. Finished executions go
// first because a running one is what the user is most likely watching.
func (w *workflowLive) evictLocked() {
	if len(w.execs) <= liveExecMax {
		return
	}
	type ref struct {
		id   string
		seq  uint64
		done bool
	}
	refs := make([]ref, 0, len(w.execs))
	for id, e := range w.execs {
		refs = append(refs, ref{id: id, seq: e.seq, done: e.Done})
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].done != refs[j].done {
			return refs[i].done
		}
		return refs[i].seq < refs[j].seq
	})
	for i := 0; i < len(refs) && len(w.execs) > liveExecMax; i++ {
		delete(w.execs, refs[i].id)
	}
}

// touch applies fn to a tracked step. It is the single place that takes
// the write lock for a step update, so every event handler below stays a
// one-liner and cannot forget to unlock.
func (w *workflowLive) touch(execID, step string, fn func(*liveExecution, *liveStep)) {
	w.mu.Lock()
	defer w.mu.Unlock()

	ex := w.execs[execID]
	if ex == nil {
		return
	}
	if step == "" {
		fn(ex, nil)
		return
	}
	i, ok := ex.stepIdx[step]
	if !ok {
		// A step the plan did not mention — a compensation handler, or
		// a definition that changed under a resumed execution. Append
		// it rather than dropping the information.
		ex.Steps = append(ex.Steps, liveStep{Name: step, State: liveStepPending})
		i = len(ex.Steps) - 1
		if ex.stepIdx == nil {
			ex.stepIdx = make(map[string]int, 4)
		}
		ex.stepIdx[step] = i
	}
	fn(ex, &ex.Steps[i])
}

// finish marks an execution terminal and stamps it for sweeping.
func (w *workflowLive) finish(execID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if ex := w.execs[execID]; ex != nil {
		ex.Done = true
		ex.doneAt = time.Now()
		// Any step still shown as running never reported an outcome,
		// which happens when the execution ended around it. Leaving it
		// gold would strand a spinner on the page forever.
		for i := range ex.Steps {
			if ex.Steps[i].State == liveStepRunning || ex.Steps[i].State == liveStepRetrying {
				ex.Steps[i].State = liveStepPending
			}
		}
	}
}

// sweep drops executions that finished more than ttl ago.
func (w *workflowLive) sweep(ttl time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	cutoff := time.Now().Add(-ttl)
	for id, e := range w.execs {
		if e.Done && e.doneAt.Before(cutoff) {
			delete(w.execs, id)
		}
	}
}

// attachWorkflowLive subscribes the tracker to a bus and returns the
// detach function.
//
// Listeners return nil unconditionally: the dashboard is an observer,
// and a bookkeeping problem here must never fail a workflow step.
func (w *workflowLive) attach(bus *events.Bus) func() {
	if bus == nil || w == nil {
		return func() {}
	}

	subs := []func(){
		events.OnTypeBus[events.WorkflowStarted](bus, func(_ *events.Context, e events.WorkflowStarted) error {
			w.start(e)
			return nil
		}).Unsubscribe,

		events.OnTypeBus[events.WorkflowStepStarted](bus, func(_ *events.Context, e events.WorkflowStepStarted) error {
			w.touch(e.ExecutionID, e.Step, func(_ *liveExecution, s *liveStep) {
				s.State, s.Attempt = liveStepRunning, e.Attempt
				s.Err = ""
			})
			return nil
		}).Unsubscribe,

		events.OnTypeBus[events.WorkflowStepCompleted](bus, func(_ *events.Context, e events.WorkflowStepCompleted) error {
			w.touch(e.ExecutionID, e.Step, func(_ *liveExecution, s *liveStep) {
				s.State, s.Attempt = liveStepDone, e.Attempt
				s.DurationMS = float64(e.Duration) / float64(time.Millisecond)
				s.Err = ""
			})
			return nil
		}).Unsubscribe,

		events.OnTypeBus[events.WorkflowStepFailed](bus, func(_ *events.Context, e events.WorkflowStepFailed) error {
			w.touch(e.ExecutionID, e.Step, func(_ *liveExecution, s *liveStep) {
				// A failure that will be retried is not the step's
				// verdict yet, so it reads as retrying rather than
				// failed. Only the last attempt decides.
				if e.Retryable {
					s.State = liveStepRetrying
				} else {
					s.State = liveStepFailed
				}
				s.Attempt, s.Err = e.Attempt, e.Err
				s.DurationMS = float64(e.Duration) / float64(time.Millisecond)
			})
			return nil
		}).Unsubscribe,

		events.OnTypeBus[events.WorkflowRetrying](bus, func(_ *events.Context, e events.WorkflowRetrying) error {
			w.touch(e.ExecutionID, e.Step, func(_ *liveExecution, s *liveStep) {
				s.State, s.Attempt = liveStepRetrying, e.Attempt
			})
			return nil
		}).Unsubscribe,

		events.OnTypeBus[events.WorkflowCompensationStarted](bus, func(_ *events.Context, e events.WorkflowCompensationStarted) error {
			w.touch(e.ExecutionID, "", func(ex *liveExecution, _ *liveStep) {
				ex.Compensating = true
			})
			return nil
		}).Unsubscribe,

		events.OnTypeBus[events.WorkflowCompensationFailed](bus, func(_ *events.Context, e events.WorkflowCompensationFailed) error {
			w.touch(e.ExecutionID, e.Step, func(_ *liveExecution, s *liveStep) {
				s.State, s.Err = liveStepFailed, e.Err
			})
			return nil
		}).Unsubscribe,

		events.OnTypeBus[events.WorkflowCompensationCompleted](bus, func(_ *events.Context, e events.WorkflowCompensationCompleted) error {
			w.touch(e.ExecutionID, "", func(ex *liveExecution, _ *liveStep) {
				// Rollback succeeded, so the steps that had completed
				// are no longer in effect. Showing them green would
				// claim work that has since been undone.
				for i := range ex.Steps {
					if ex.Steps[i].State == liveStepDone {
						ex.Steps[i].State = liveStepRolled
					}
				}
			})
			return nil
		}).Unsubscribe,

		events.OnTypeBus[events.WorkflowCompleted](bus, func(_ *events.Context, e events.WorkflowCompleted) error {
			w.finish(e.ExecutionID)
			return nil
		}).Unsubscribe,

		events.OnTypeBus[events.WorkflowFailed](bus, func(_ *events.Context, e events.WorkflowFailed) error {
			w.finish(e.ExecutionID)
			return nil
		}).Unsubscribe,

		events.OnTypeBus[events.WorkflowTimedOut](bus, func(_ *events.Context, e events.WorkflowTimedOut) error {
			w.finish(e.ExecutionID)
			return nil
		}).Unsubscribe,

		events.OnTypeBus[events.WorkflowCancelled](bus, func(_ *events.Context, e events.WorkflowCancelled) error {
			w.finish(e.ExecutionID)
			return nil
		}).Unsubscribe,
	}

	// Finished executions are swept on a timer rather than on the next
	// event, so an idle application does not keep its last execution on
	// the page indefinitely.
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				w.sweep(30 * time.Second)
			}
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() {
			close(stop)
			for _, unsub := range subs {
				unsub()
			}
		})
	}
}
