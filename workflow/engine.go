package workflow

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nelthaarion/breeze/diag"
	"github.com/nelthaarion/breeze/events"
	"github.com/nelthaarion/breeze/observability"
)

// Config configures an [Engine]. The zero value is valid and yields the
// defaults documented on each field, so callers set only what they care
// about.
type Config struct {
	// Store persists execution state. Defaults to a [MemoryStore],
	// which makes the engine usable with no setup but does not survive
	// the process.
	Store Store

	// Bus is the event bus workflow events are published on. Defaults
	// to [events.Default]. The engine never creates a bus of its own.
	Bus *events.Bus

	// Collector receives observability signals. Defaults to
	// [observability.Default]. Set DisableObservability to opt out.
	Collector *observability.Collector

	// DisableObservability stops the engine publishing signals. It
	// exists because the zero value of Collector must mean "use the
	// default", not "publish nothing".
	DisableObservability bool

	// MaxWorkers bounds how many steps run concurrently across all
	// executions. Defaults to runtime.NumCPU(). Parallel steps are
	// scheduled through this pool, so a workflow can never spawn an
	// unbounded number of goroutines.
	MaxWorkers int

	// ShutdownTimeout bounds how long [Engine.Shutdown] waits for
	// running executions. Defaults to 30s.
	ShutdownTimeout time.Duration

	// OnPanic receives a step's recovered panic. Defaults to nil; the
	// panic is converted into a step failure either way.
	OnPanic func(workflow, step string, v any, stack []byte)
}

// Result is the outcome of one execution.
type Result struct {
	ExecutionID string
	Workflow    string
	State       State
	Steps       []StepResult
	Duration    time.Duration
	Err         error
}

// StepResult is the outcome of one step within an execution.
type StepResult struct {
	Name        string
	State       State
	Attempts    int
	Duration    time.Duration
	Err         error
	Skipped     bool
	Compensated bool
}

// Engine runs workflows. It is safe for concurrent use, and a process
// normally has one, though nothing prevents several.
type Engine struct {
	cfg   Config
	store Store
	bus   *events.Bus
	col   *observability.Collector

	mu      sync.RWMutex
	defs    map[string]*Definition
	unsubs  []func()
	closed  atomic.Bool
	running sync.WaitGroup

	// sem bounds concurrent step execution across every execution.
	sem chan struct{}

	// Counters for the diagnostic probe.
	//
	// Unconditional atomics rather than diag.Counter's gated ones, and the
	// reason is the unit of work: one increment happens per *execution* or per
	// *step*, each of which already writes to a Store, emits an event and
	// publishes an observability signal. An atomic add is not measurable against
	// that, and gating it would mean a workflow engine that cannot say how many
	// workflows it ran unless someone thought to enable counting first — which
	// is the exact question this subsystem exists to answer.
	started      atomic.Uint64
	completed    atomic.Uint64
	failed       atomic.Uint64
	compensated  atomic.Uint64
	stepsRun     atomic.Uint64
	stepRetries  atomic.Uint64
	lastRunNanos atomic.Int64

	// inflight guards idempotent starts within the process, covering
	// the window before the store has the record.
	inflight sync.Map // key -> *sync.Once-guarded executionID

	seq atomic.Uint64
}

// NewEngine returns an engine configured by cfg.
func NewEngine(cfg ...Config) *Engine {
	var c Config
	if len(cfg) > 0 {
		c = cfg[0]
	}
	if c.Store == nil {
		c.Store = NewMemoryStore()
	}
	if c.Bus == nil {
		c.Bus = events.Default
	}
	if c.MaxWorkers <= 0 {
		c.MaxWorkers = runtime.NumCPU()
	}
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = 30 * time.Second
	}
	col := c.Collector
	if col == nil && !c.DisableObservability {
		col = observability.Default()
	}
	if c.DisableObservability {
		col = nil
	}
	e := &Engine{
		cfg:   c,
		store: c.Store,
		bus:   c.Bus,
		col:   col,
		defs:  make(map[string]*Definition),
		sem:   make(chan struct{}, c.MaxWorkers),
	}
	RegisterDiagnostics(e)
	return e
}

// Register validates a definition and adds it to the engine. A
// definition with an event trigger is subscribed to the bus here, so
// registration is the single point where a workflow becomes live.
func (e *Engine) Register(d *Definition) error {
	if d == nil {
		return fmt.Errorf("%w: nil definition", ErrInvalidWorkflow)
	}
	if err := d.Validate(); err != nil {
		return err
	}
	if e.closed.Load() {
		return ErrEngineClosed
	}

	e.mu.Lock()
	if _, exists := e.defs[d.name]; exists {
		e.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrDuplicateWorkflow, d.name)
	}
	e.defs[d.name] = d
	e.mu.Unlock()

	if d.trig != nil {
		name := d.name
		trigger := d.trig.name
		unsub := d.trig.subscribe(e.bus, func(payload any) {
			// A trigger starts the execution asynchronously: the
			// emitter of an event must not wait for a workflow, and
			// blocking here would make Emit as slow as the slowest
			// workflow listening to it.
			e.startAsync(name, payload, trigger)
		})
		e.mu.Lock()
		e.unsubs = append(e.unsubs, unsub)
		e.mu.Unlock()
	}
	return nil
}

// Definitions returns the names of every registered workflow.
func (e *Engine) Definitions() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]string, 0, len(e.defs))
	for name := range e.defs {
		out = append(out, name)
	}
	return out
}

// Definition returns a registered definition for inspection.
func (e *Engine) Definition(name string) (*Definition, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	d, ok := e.defs[name]
	return d, ok
}

// RunOption configures a single execution.
type RunOption func(*runOptions)

type runOptions struct {
	idempotencyKey string
	correlationID  string
	executionID    string
	trigger        string
	meta           map[string]any
	resume         *WorkflowRecord
}

// WithIdempotencyKey makes a start a no-op when an execution with the
// same key already exists for the workflow, which is what makes an
// at-least-once event delivery safe.
func WithIdempotencyKey(key string) RunOption {
	return func(o *runOptions) { o.idempotencyKey = key }
}

// WithCorrelationID ties the execution to a wider operation.
func WithCorrelationID(id string) RunOption {
	return func(o *runOptions) { o.correlationID = id }
}

// WithExecutionID sets the execution's identifier instead of letting
// the engine generate one.
func WithExecutionID(id string) RunOption {
	return func(o *runOptions) { o.executionID = id }
}

// WithMetadata seeds the execution context's metadata.
func WithMetadata(meta map[string]any) RunOption {
	return func(o *runOptions) { o.meta = meta }
}

// Run executes a workflow and waits for it to finish.
//
// The returned error is nil only when the execution completed; a failed,
// cancelled, timed-out or compensated execution returns the failure that
// caused it, and Result carries the per-step detail either way.
func (e *Engine) Run(ctx context.Context, name string, payload any, opts ...RunOption) (Result, error) {
	if e.closed.Load() {
		return Result{}, ErrEngineClosed
	}
	e.mu.RLock()
	def, ok := e.defs[name]
	e.mu.RUnlock()
	if !ok {
		return Result{}, fmt.Errorf("%w: %s", ErrWorkflowNotFound, name)
	}

	var o runOptions
	for _, opt := range opts {
		opt(&o)
	}

	e.running.Add(1)
	defer e.running.Done()
	return e.execute(ctx, def, payload, o)
}

// startAsync runs a workflow in the background, for event triggers.
func (e *Engine) startAsync(name string, payload any, triggerName string) {
	if e.closed.Load() {
		return
	}
	e.mu.RLock()
	def, ok := e.defs[name]
	e.mu.RUnlock()
	if !ok {
		return
	}

	e.running.Add(1)
	go func() {
		defer e.running.Done()
		// A triggered execution is not bound to the emitter's context:
		// the emitter has moved on, and cancelling the workflow when
		// its trigger's dispatch ended would defeat the point.
		_, _ = e.execute(context.Background(), def, payload, runOptions{trigger: triggerName})
	}()
}

// nextID returns a process-unique execution identifier.
func (e *Engine) nextID(workflow string) string {
	return fmt.Sprintf("%s-%d-%d", workflow, time.Now().UnixNano(), e.seq.Add(1))
}

// execute is the whole lifecycle of one execution.
func (e *Engine) execute(ctx context.Context, def *Definition, payload any, o runOptions) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	// Idempotency: an existing execution for the key wins, and the
	// caller gets its outcome rather than a second run.
	if o.idempotencyKey != "" {
		if rec, found, err := e.store.FindByIdempotencyKey(ctx, def.name, o.idempotencyKey); err == nil && found {
			return Result{
				ExecutionID: rec.ExecutionID,
				Workflow:    def.name,
				State:       rec.State,
				Err:         recErr(rec.Err),
			}, nil
		}
		// Guard the window before the record exists: two concurrent
		// starts with the same key must not both create one.
		if _, loaded := e.inflight.LoadOrStore(def.name+"\x00"+o.idempotencyKey, struct{}{}); loaded {
			return Result{Workflow: def.name, State: StatePending}, nil
		}
		defer e.inflight.Delete(def.name + "\x00" + o.idempotencyKey)
	}

	execID := o.executionID
	if execID == "" {
		execID = e.nextID(def.name)
	}
	start := time.Now()

	// A workflow timeout governs the whole execution; individual step
	// timeouts nest inside it.
	if def.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, def.timeout)
		defer cancel()
	}

	wctx := newContext(ctx, def.name, execID, o.correlationID, payload, o.meta)

	rec := WorkflowRecord{
		ExecutionID:    execID,
		Workflow:       def.name,
		Version:        def.version,
		State:          StatePending,
		IdempotencyKey: o.idempotencyKey,
		CorrelationID:  o.correlationID,
		StartedAt:      start,
		UpdatedAt:      start,
	}
	// completed carries the steps already done by a previous run that
	// is being resumed. It must be read before the record is rewritten.
	completed := map[string]bool{}
	if o.resume != nil {
		if steps, err := e.store.ListSteps(ctx, execID); err == nil {
			for _, s := range steps {
				if s.State == StateCompleted {
					completed[s.Step] = true
				}
			}
		}
		// A resumed execution already has a record; overwriting it
		// keeps the step history that says where to pick up, which
		// deleting and recreating would destroy.
		rec.StartedAt = o.resume.StartedAt
		if err := e.store.UpdateWorkflow(ctx, rec); err != nil {
			return Result{ExecutionID: execID, Workflow: def.name, State: StateFailed},
				fmt.Errorf("%w: %w", ErrPersistenceFailure, err)
		}
	} else if err := e.store.CreateWorkflow(ctx, rec); err != nil {
		return Result{ExecutionID: execID, Workflow: def.name, State: StateFailed},
			fmt.Errorf("%w: %w", ErrPersistenceFailure, err)
	}

	e.emit(events.WorkflowStarted{
		Workflow:    def.name,
		ExecutionID: execID,
		Trigger:     o.trigger,
		Steps:       def.Len(),
		StepNames:   def.StepNames(),
		Time:        start,
	})
	e.started.Add(1)
	e.lastRunNanos.Store(start.UnixNano())
	e.setState(ctx, &rec, StateRunning, nil)

	res, spans := e.runSteps(ctx, def, wctx, execID, completed)
	res.ExecutionID = execID
	res.Workflow = def.name
	res.Duration = time.Since(start)

	// The terminal state is derived by stateFor, the same classifier
	// the steps use, so a step timeout and an execution timeout cannot
	// be categorised differently. Compensation may change it again
	// below.
	if res.Err == nil {
		res.State = StateCompleted
		e.completed.Add(1)
		e.emit(events.WorkflowCompleted{
			Workflow: def.name, ExecutionID: execID,
			Steps: len(res.Steps), Duration: res.Duration,
		})
	} else {
		res.State = stateFor(res.Err)
		e.failed.Add(1)
		switch res.State {
		case StateTimedOut:
			e.emit(events.WorkflowTimedOut{
				Workflow: def.name, ExecutionID: execID,
				Step: failedStep(res), Timeout: def.timeout,
			})
		case StateCancelled:
			e.emit(events.WorkflowCancelled{
				Workflow: def.name, ExecutionID: execID,
				Step: failedStep(res), Reason: res.Err.Error(),
			})
		}
	}

	if res.Err != nil {
		// Compensate whatever succeeded, in reverse order.
		failedState := res.State
		if compensated := e.compensate(def, wctx, execID, res, &rec); compensated {
			res.State = StateCompensated
			e.compensated.Add(1)
		}
		if failedState == StateFailed && res.State == StateFailed {
			e.emit(events.WorkflowFailed{
				Workflow: def.name, ExecutionID: execID,
				Step: failedStep(res), Duration: res.Duration, Err: res.Err.Error(),
			})
		}
	}

	e.setState(context.WithoutCancel(ctx), &rec, res.State, res.Err)
	e.publishSignal(def, res, spans, o)
	return res, res.Err
}

// runSteps walks the plan level by level, running each level with
// bounded parallelism and stopping at the first failure.
func (e *Engine) runSteps(ctx context.Context, def *Definition, wctx *Context, execID string, completed map[string]bool) (Result, []observability.Span) {
	var (
		res   Result
		spans []observability.Span
		mu    sync.Mutex
	)
	layers, err := def.plan()
	if err != nil {
		res.Err = err
		return res, spans
	}

	for level, layer := range layers {
		// A cancelled or expired context ends the walk before starting
		// more work.
		if err := ctx.Err(); err != nil {
			res.Err = translateCtxErr(err, def.timeout)
			return res, spans
		}

		var wg sync.WaitGroup
		for _, step := range layer {
			step := step

			if completed[step.name] {
				// Durable resume: a step that already succeeded is
				// never re-executed.
				mu.Lock()
				res.Steps = append(res.Steps, StepResult{Name: step.name, State: StateCompleted})
				spans = append(spans, observability.Span{
					Name: step.name, Phase: levelLabel(level),
					Index: step.index, Skipped: true,
				})
				mu.Unlock()
				continue
			}

			wg.Add(1)
			e.acquire()
			go func() {
				defer wg.Done()
				defer e.release()

				sr, span := e.runStep(ctx, def, step, wctx, execID)
				// The span's phase carries the DAG level. Steps that
				// share a level ran concurrently, which is what lets
				// the dashboard draw parallel branches without the
				// workflow package knowing anything about rendering.
				span.Phase = levelLabel(level)
				mu.Lock()
				res.Steps = append(res.Steps, sr)
				spans = append(spans, span)
				if sr.Err != nil && res.Err == nil {
					res.Err = sr.Err
				}
				mu.Unlock()
			}()
		}
		wg.Wait()

		if res.Err != nil {
			return res, spans
		}
	}
	return res, spans
}

// acquire takes a worker slot, bounding total concurrency.
func (e *Engine) acquire() { e.sem <- struct{}{} }
func (e *Engine) release() { <-e.sem }

// runStep executes one step with its condition, timeout and retries.
func (e *Engine) runStep(ctx context.Context, def *Definition, step *Step, wctx *Context, execID string) (StepResult, observability.Span) {
	sr := StepResult{Name: step.name, State: StateRunning}
	started := time.Now()

	if step.cond != nil && !step.cond(wctx.withCtx(ctx)) {
		sr.State, sr.Skipped = StateCompleted, true
		return sr, observability.Span{Name: step.name, Skipped: true, Index: step.index, Priority: 0}
	}

	policy := def.policyFor(step)
	maxAttempts := policy.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		sr.Attempts = attempt
		wctx.setStep(step.name, attempt)
		e.stepsRun.Add(1)
		if attempt > 1 {
			e.stepRetries.Add(1)
		}

		e.emit(events.WorkflowStepStarted{
			Workflow: def.name, ExecutionID: execID, Step: step.name, Attempt: attempt,
		})
		e.saveStep(ctx, StepRecord{
			ExecutionID: execID, Step: step.name, State: StateRunning,
			Attempt: attempt, StartedAt: started, UpdatedAt: time.Now(),
		})

		attemptStart := time.Now()
		err = e.invoke(ctx, def, step, wctx, step.timeout, step.fn)
		dur := time.Since(attemptStart)

		if err == nil {
			sr.State = StateCompleted
			sr.Duration = time.Since(started)
			e.emit(events.WorkflowStepCompleted{
				Workflow: def.name, ExecutionID: execID,
				Step: step.name, Attempt: attempt, Duration: dur,
			})
			e.saveStep(ctx, StepRecord{
				ExecutionID: execID, Step: step.name, State: StateCompleted,
				Attempt: attempt, StartedAt: started, UpdatedAt: time.Now(), FinishedAt: time.Now(),
			})
			return sr, observability.Span{
				Name: step.name, Duration: dur, DurationMS: msOf(dur), Index: step.index,
			}
		}

		retrying := policy.ShouldRetry(attempt, err)
		e.emit(events.WorkflowStepFailed{
			Workflow: def.name, ExecutionID: execID, Step: step.name,
			Attempt: attempt, Duration: dur, Err: err.Error(), Retryable: retrying,
		})
		if !retrying {
			break
		}

		delay := policy.Delay(attempt)
		e.emit(events.WorkflowRetrying{
			Workflow: def.name, ExecutionID: execID, Step: step.name,
			Attempt: attempt + 1, Delay: delay,
		})
		e.saveStep(ctx, StepRecord{
			ExecutionID: execID, Step: step.name, State: StateWaiting,
			Attempt: attempt, Err: err.Error(), StartedAt: started, UpdatedAt: time.Now(),
		})

		// Waiting must remain cancellable: a shutdown should not have
		// to outlast a 30s backoff.
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			err = translateCtxErr(ctx.Err(), def.timeout)
			sr.State, sr.Err = stateFor(err), e.stepError(def, execID, step, attempt, err)
			sr.Duration = time.Since(started)
			return sr, observability.Span{
				Name: step.name, Duration: sr.Duration, DurationMS: msOf(sr.Duration),
				Err: err.Error(), Failed: true, Index: step.index,
			}
		}
	}

	sr.Duration = time.Since(started)
	sr.State = stateFor(err)
	sr.Err = e.stepError(def, execID, step, sr.Attempts, err)
	e.saveStep(ctx, StepRecord{
		ExecutionID: execID, Step: step.name, State: sr.State, Attempt: sr.Attempts,
		Err: err.Error(), StartedAt: started, UpdatedAt: time.Now(), FinishedAt: time.Now(),
	})
	return sr, observability.Span{
		Name: step.name, Duration: sr.Duration, DurationMS: msOf(sr.Duration),
		Err: err.Error(), Failed: true, Index: step.index,
	}
}

// invoke runs a step function under its timeout, converting a panic into
// an error so that one bad step cannot take the process down.
func (e *Engine) invoke(ctx context.Context, def *Definition, step *Step, wctx *Context, timeout time.Duration, fn StepFunc) (err error) {
	stepCtx := ctx
	var cancel context.CancelFunc
	if timeout > 0 {
		stepCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	defer func() {
		if r := recover(); r != nil {
			stack := debug.Stack()
			if e.cfg.OnPanic != nil {
				e.cfg.OnPanic(def.name, step.name, r, stack)
			}
			err = fmt.Errorf("%w: %v", ErrStepPanicked, r)
		}
	}()

	err = fn(wctx.withCtx(stepCtx))

	// A step that returns its context's error, or none at all, after
	// its deadline passed has timed out; report that rather than the
	// generic cancellation.
	if stepCtx.Err() != nil && ctx.Err() == nil {
		if errors.Is(stepCtx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("%w: %s after %s", ErrStepTimeout, step.name, timeout)
		}
	}
	if err != nil {
		// A step that surfaces its context's own error did not fail on
		// its own terms — the execution's deadline or cancellation did.
		// Translating here keeps context sentinels from leaking out in
		// place of the package's vocabulary, which is what decides
		// whether the run is classified as timed out or merely failed.
		if ctx.Err() != nil &&
			(errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) {
			return translateCtxErr(ctx.Err(), def.timeout)
		}
		return err
	}
	if ctx.Err() != nil {
		return translateCtxErr(ctx.Err(), def.timeout)
	}
	return nil
}

// compensate rolls back the completed steps in reverse order. It
// reports whether rollback finished cleanly.
func (e *Engine) compensate(def *Definition, wctx *Context, execID string, res Result, rec *WorkflowRecord) bool {
	// Compensation runs on a context detached from the failure: the
	// execution's context may already be cancelled or expired, and
	// rollback still has to happen.
	ctx := context.WithoutCancel(wctx.Ctx)

	done := make([]*Step, 0, len(res.Steps))
	byName := make(map[string]StepResult, len(res.Steps))
	for _, sr := range res.Steps {
		byName[sr.Name] = sr
	}
	for _, s := range def.steps {
		sr, ok := byName[s.name]
		if ok && sr.State == StateCompleted && !sr.Skipped && s.compensate != nil {
			done = append(done, s)
		}
	}
	if len(done) == 0 {
		return false
	}

	start := time.Now()
	e.emit(events.WorkflowCompensationStarted{
		Workflow: def.name, ExecutionID: execID, Steps: len(done), Cause: res.Err.Error(),
	})
	e.setState(ctx, rec, StateCompensating, res.Err)

	ok := true
	for i := len(done) - 1; i >= 0; i-- {
		s := done[i]
		policy := s.compRetry
		attempts := policy.MaxAttempts
		if attempts < 1 {
			attempts = 1
		}

		var err error
		for attempt := 1; attempt <= attempts; attempt++ {
			wctx.setStep(s.name, attempt)
			err = e.invoke(ctx, def, s, wctx, s.compTimeout, StepFunc(s.compensate))
			if err == nil || !policy.ShouldRetry(attempt, err) {
				break
			}
			timer := time.NewTimer(policy.Delay(attempt))
			<-timer.C
		}

		if err != nil {
			ok = false
			e.emit(events.WorkflowCompensationFailed{
				Workflow: def.name, ExecutionID: execID, Step: s.name, Err: err.Error(),
			})
			// A failed rollback does not stop the remaining ones: the
			// other side effects still need undoing, and stopping
			// would leave strictly more damage behind.
			continue
		}
		e.saveStep(ctx, StepRecord{
			ExecutionID: execID, Step: s.name, State: StateCompensated,
			UpdatedAt: time.Now(), FinishedAt: time.Now(), Compensated: true,
		})
	}

	if ok {
		e.emit(events.WorkflowCompensationCompleted{
			Workflow: def.name, ExecutionID: execID, Steps: len(done), Duration: time.Since(start),
		})
	}
	return ok
}

// Resume continues the executions that were interrupted by a restart.
// Steps that already completed are not run again.
//
// # Payloads are not restored
//
// The store persists execution and step state, not the payload the
// execution was started with. A resumed execution therefore runs with a
// nil payload, and [Context.Payload] returns nil in its steps.
//
// This matters whenever a step reads the payload. A workflow like
//
//	Step("charge", func(c *workflow.Context) error {
//	    order := c.Payload().(Order) // panics after a resume
//	    ...
//	})
//
// is safe on its first run and broken on resume. Write steps that
// survive it in one of two ways:
//
//   - Put what later steps need into the execution's metadata as it is
//     computed, and read it back with [Context.Get]. Metadata is
//     persisted with the execution.
//   - Or re-fetch from the system of record using an identifier, rather
//     than trusting the in-memory payload.
//
// Steps that only read metadata or re-fetch their own inputs resume
// correctly. Steps that dereference the payload must guard against nil.
//
// Resume is a no-op for workflows that are no longer registered, so
// removing a definition cannot resurrect its executions.
func (e *Engine) Resume(ctx context.Context) (int, error) {
	if e.closed.Load() {
		return 0, ErrEngineClosed
	}
	pending, err := e.store.PendingWorkflows(ctx)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrPersistenceFailure, err)
	}

	var resumed int
	for _, rec := range pending {
		e.mu.RLock()
		def, ok := e.defs[rec.Workflow]
		e.mu.RUnlock()
		if !ok {
			continue
		}

		// The record is reused rather than recreated: its step history
		// is what tells the engine which steps are already done, so
		// deleting it would forget exactly what resume needs.
		old := rec
		e.running.Add(1)
		go func(d *Definition, r WorkflowRecord) {
			defer e.running.Done()
			_, _ = e.execute(context.Background(), d, nil, runOptions{
				executionID:   r.ExecutionID,
				correlationID: r.CorrelationID,
				resume:        &old,
			})
		}(def, rec)
		resumed++
	}
	return resumed, nil
}

// Shutdown stops accepting work and waits for running executions,
// bounded by the configured timeout.
func (e *Engine) Shutdown(ctx context.Context) error {
	if e.closed.Swap(true) {
		return nil
	}

	// Unsubscribing first means no new execution can be triggered
	// while the running ones drain.
	e.mu.Lock()
	unsubs := e.unsubs
	e.unsubs = nil
	e.mu.Unlock()
	for _, unsub := range unsubs {
		unsub()
	}

	if ctx == nil {
		ctx = context.Background()
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, e.cfg.ShutdownTimeout)
		defer cancel()
	}

	done := make(chan struct{})
	go func() {
		e.running.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("workflow: shutdown timed out: %w", ctx.Err())
	}
}

// --- helpers ---

// emit publishes a framework event, ignoring listener errors: a
// listener's failure is its own business and must not fail a workflow.
func (e *Engine) emit(event any) {
	if e.bus == nil {
		return
	}
	switch ev := event.(type) {
	case events.WorkflowStarted:
		_ = events.EmitBus(e.bus, ev)
	case events.WorkflowStepStarted:
		_ = events.EmitBus(e.bus, ev)
	case events.WorkflowStepCompleted:
		_ = events.EmitBus(e.bus, ev)
	case events.WorkflowStepFailed:
		_ = events.EmitBus(e.bus, ev)
	case events.WorkflowRetrying:
		_ = events.EmitBus(e.bus, ev)
	case events.WorkflowTimedOut:
		_ = events.EmitBus(e.bus, ev)
	case events.WorkflowCancelled:
		_ = events.EmitBus(e.bus, ev)
	case events.WorkflowCompleted:
		_ = events.EmitBus(e.bus, ev)
	case events.WorkflowFailed:
		_ = events.EmitBus(e.bus, ev)
	case events.WorkflowCompensationStarted:
		_ = events.EmitBus(e.bus, ev)
	case events.WorkflowCompensationCompleted:
		_ = events.EmitBus(e.bus, ev)
	case events.WorkflowCompensationFailed:
		_ = events.EmitBus(e.bus, ev)
	}
}

// setState persists a state transition, refusing illegal ones so a late
// update cannot resurrect a terminal execution.
func (e *Engine) setState(ctx context.Context, rec *WorkflowRecord, next State, err error) {
	if !rec.State.CanTransitionTo(next) {
		return
	}
	rec.State = next
	rec.UpdatedAt = time.Now()
	if err != nil {
		rec.Err = err.Error()
	}
	if next.Terminal() {
		rec.FinishedAt = rec.UpdatedAt
	}
	_ = e.store.UpdateWorkflow(ctx, *rec)
}

func (e *Engine) saveStep(ctx context.Context, rec StepRecord) {
	_ = e.store.SaveStep(context.WithoutCancel(ctx), rec)
}

func (e *Engine) stepError(def *Definition, execID string, step *Step, attempt int, err error) error {
	if err == nil {
		return nil
	}
	return &StepError{
		Workflow: def.name, ExecutionID: execID,
		Step: step.name, Attempt: attempt, Err: err,
	}
}

// publishSignal records the execution as one observability signal, with
// its steps as spans — the same shape the event bus publishes, so the
// dashboard renders workflows with no new code.
func (e *Engine) publishSignal(def *Definition, res Result, spans []observability.Span, o runOptions) {
	if e.col == nil {
		return
	}
	attrs := map[string]string{
		"state":        res.State.String(),
		"version":      fmt.Sprintf("%d", def.version),
		"execution_id": res.ExecutionID,
		"steps_total":  fmt.Sprintf("%d", def.Len()),
		"attempts":     fmt.Sprintf("%d", totalAttempts(res)),
	}
	if o.trigger != "" {
		attrs["trigger"] = o.trigger
	}
	if n := compensatedCount(res); n > 0 {
		attrs["compensated"] = fmt.Sprintf("%d", n)
	}
	if s := failedStep(res); s != "" {
		attrs["failed_step"] = s
	}
	sig := observability.Signal{
		SourceID:      e.seq.Add(1),
		Source:        observability.SourceWorkflow,
		Kind:          observability.KindWorkflow,
		Name:          def.name,
		Time:          time.Now().Add(-res.Duration),
		Duration:      res.Duration,
		DurationMS:    msOf(res.Duration),
		Failed:        res.Err != nil,
		Cancelled:     res.State == StateCancelled,
		CorrelationID: o.correlationID,
		Children:      def.Len(),
		Executed:      len(spans),
		Attrs:         observability.MaskAttrs(attrs),
		Spans:         spans,
	}
	if res.Err != nil {
		sig.Err = res.Err.Error()
	}
	e.col.Publish(sig)
}

// translateCtxErr converts a context error into the package's own
// vocabulary, so callers match on ErrWorkflowTimeout rather than having
// to know that a deadline was involved.
func translateCtxErr(err error, timeout time.Duration) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.DeadlineExceeded):
		if timeout > 0 {
			return fmt.Errorf("%w after %s", ErrWorkflowTimeout, timeout)
		}
		return ErrWorkflowTimeout
	default:
		return fmt.Errorf("%w: %v", ErrWorkflowCancelled, err)
	}
}

// stateFor maps a failure to the state it puts a step in.
func stateFor(err error) State {
	switch {
	case err == nil:
		return StateCompleted
	case errors.Is(err, ErrWorkflowTimeout), errors.Is(err, ErrStepTimeout):
		return StateTimedOut
	case errors.Is(err, ErrWorkflowCancelled), errors.Is(err, context.Canceled):
		return StateCancelled
	default:
		return StateFailed
	}
}

// levelLabel names a DAG level for display. Steps sharing a label ran
// in the same parallel batch.
func levelLabel(level int) string { return "L" + fmt.Sprintf("%d", level) }

// totalAttempts sums every attempt across steps, so a retry is visible
// in the execution summary without opening each step.
func totalAttempts(res Result) int {
	var n int
	for _, s := range res.Steps {
		n += s.Attempts
	}
	return n
}

func compensatedCount(res Result) int {
	var n int
	for _, s := range res.Steps {
		if s.Compensated {
			n++
		}
	}
	return n
}

func failedStep(res Result) string {
	for _, s := range res.Steps {
		if s.Err != nil {
			return s.Name
		}
	}
	return ""
}

func recErr(s string) error {
	if s == "" {
		return nil
	}
	return errors.New(s)
}

// msOf renders a step or execution duration as fractional milliseconds for the
// DurationMS fields the dashboard reads.
//
// Delegates to [diag.Milliseconds] so this package and events/diag.go round the
// same way. They did not before: this one divided by float64(time.Millisecond),
// keeping nanosecond noise in a field rendered next to values that had been
// truncated to microseconds.
func msOf(d time.Duration) float64 { return diag.Milliseconds(d) }
