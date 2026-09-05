package workflow

import (
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/nelthaarion/breeze/v2/events"
)

// StepFunc is the work a step performs. Returning an error fails the
// step, which then retries or fails the workflow according to its
// policy.
type StepFunc func(ctx *Context) error

// CompensateFunc undoes what a step did. It runs during rollback, in
// reverse order, and receives the same execution context the step saw.
type CompensateFunc func(ctx *Context) error

// CondFunc decides whether a step runs at all. A step whose condition
// returns false is skipped, and its dependants still run.
type CondFunc func(ctx *Context) bool

// Step is one unit of work. Steps are built by [Definition.Step] and are
// immutable once the definition is registered.
type Step struct {
	name        string
	fn          StepFunc
	timeout     time.Duration
	retry       RetryPolicy
	retrySet    bool
	compensate  CompensateFunc
	compTimeout time.Duration
	compRetry   RetryPolicy
	cond        CondFunc
	dependsOn   []string
	meta        map[string]string
	index       int
}

// Name returns the step's name, unique within its workflow.
func (s *Step) Name() string { return s.name }

// DependsOn returns the names of the steps that must finish first.
func (s *Step) DependsOn() []string { return append([]string(nil), s.dependsOn...) }

// HasCompensation reports whether the step can be rolled back.
func (s *Step) HasCompensation() bool { return s.compensate != nil }

// StepOption configures a step at declaration time.
type StepOption func(*Step)

// WithTimeout bounds how long one attempt of the step may run. When it
// elapses the step's context is cancelled and the attempt fails with
// [ErrStepTimeout].
func WithTimeout(d time.Duration) StepOption {
	return func(s *Step) { s.timeout = d }
}

// WithRetry sets the step's retry policy, overriding the workflow
// default.
func WithRetry(p RetryPolicy) StepOption {
	return func(s *Step) { s.retry, s.retrySet = p, true }
}

// WithCompensation registers the handler that undoes this step if a
// later step fails.
func WithCompensation(fn CompensateFunc) StepOption {
	return func(s *Step) { s.compensate = fn }
}

// WithCompensationTimeout bounds one compensation attempt.
func WithCompensationTimeout(d time.Duration) StepOption {
	return func(s *Step) { s.compTimeout = d }
}

// WithCompensationRetry sets the retry policy for the compensation
// handler. Rollback is usually worth retrying harder than the forward
// path, because giving up leaves a side effect behind.
func WithCompensationRetry(p RetryPolicy) StepOption {
	return func(s *Step) { s.compRetry = p }
}

// WithCondition makes the step conditional. This is how a workflow
// branches without a separate branching construct: declare both
// branches and give each the condition that selects it.
func WithCondition(fn CondFunc) StepOption {
	return func(s *Step) { s.cond = fn }
}

// WithDependsOn declares the steps that must complete before this one.
// Declaring dependencies opts the step out of implicit sequential
// chaining, which is what allows steps to run in parallel.
func WithDependsOn(steps ...string) StepOption {
	return func(s *Step) { s.dependsOn = append(s.dependsOn, steps...) }
}

// WithMeta attaches a label to the step for inspection and dashboards.
// Values are masked before they are persisted or published, so a
// mistakenly attached secret does not leak; do not rely on that as
// permission to attach one.
func WithMeta(key, value string) StepOption {
	return func(s *Step) {
		if s.meta == nil {
			s.meta = make(map[string]string, 2)
		}
		s.meta[key] = value
	}
}

// StepInfo is a read-only view of a step for inspection.
type StepInfo struct {
	Name         string            `json:"name"`
	DependsOn    []string          `json:"depends_on,omitempty"`
	Timeout      time.Duration     `json:"timeout,omitempty"`
	MaxAttempts  int               `json:"max_attempts"`
	Compensating bool              `json:"compensating"`
	Conditional  bool              `json:"conditional"`
	Meta         map[string]string `json:"meta,omitempty"`
	Index        int               `json:"index"`
}

// trigger couples an event type to the code that subscribes to it.
//
// The subscribe closure exists because the engine cannot instantiate a
// generic listener from a reflect.Type: only [On], which still knows T
// at compile time, can call events.OnTypeBus[T]. Capturing that call in
// a closure is what lets the engine wire a trigger it only knows
// dynamically, with no reflection on the dispatch path.
type trigger struct {
	typ       reflect.Type
	name      string
	subscribe func(bus *events.Bus, start func(payload any)) func()
}

// Definition describes a workflow: its steps, their order, and the
// defaults they inherit. Build one with [New], then register it with an
// [Engine].
type Definition struct {
	name    string
	version int
	timeout time.Duration
	retry   RetryPolicy
	steps   []*Step
	trig    *trigger

	// validated caches the outcome so Register and a caller's own
	// Validate call do not repeat the work.
	validated bool
	validErr  error
	layers    [][]*Step
}

// New starts a workflow definition.
func New(name string) *Definition {
	return &Definition{name: name, version: 1}
}

// Step appends a step. With no [WithDependsOn] option the step depends
// on the one declared before it, which makes the common case sequential
// without any ceremony.
func (d *Definition) Step(name string, fn StepFunc, opts ...StepOption) *Definition {
	s := &Step{name: name, fn: fn, index: len(d.steps)}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	if len(s.dependsOn) == 0 && len(d.steps) > 0 {
		s.dependsOn = []string{d.steps[len(d.steps)-1].name}
	}
	d.steps = append(d.steps, s)
	d.invalidate()
	return d
}

// Timeout bounds the whole execution. When it elapses every running
// step is cancelled and the execution ends in [StateTimedOut].
func (d *Definition) Timeout(t time.Duration) *Definition {
	d.timeout = t
	return d
}

// Retry sets the default retry policy for steps that do not set their
// own.
func (d *Definition) Retry(p RetryPolicy) *Definition {
	d.retry = p
	d.invalidate()
	return d
}

// Version records the definition's version. It is persisted with every
// execution so that a resumed execution can be recognised as belonging
// to an older shape of the workflow.
func (d *Definition) Version(v int) *Definition {
	d.version = v
	return d
}

// Name returns the workflow's name.
func (d *Definition) Name() string { return d.name }

// Len returns the number of steps.
func (d *Definition) Len() int { return len(d.steps) }

// StepNames returns every step's name in declaration order.
//
// It exists for consumers that need the plan before the execution has
// produced any results — a live progress view has to know which steps
// are still pending, and a count alone cannot name them.
func (d *Definition) StepNames() []string {
	out := make([]string, 0, len(d.steps))
	for _, s := range d.steps {
		out = append(out, s.name)
	}
	return out
}

// Steps returns a read-only view of the steps, in declaration order.
func (d *Definition) Steps() []StepInfo {
	out := make([]StepInfo, 0, len(d.steps))
	for _, s := range d.steps {
		attempts := d.policyFor(s).MaxAttempts
		if attempts < 1 {
			attempts = 1
		}
		out = append(out, StepInfo{
			Name:         s.name,
			DependsOn:    s.DependsOn(),
			Timeout:      s.timeout,
			MaxAttempts:  attempts,
			Compensating: s.compensate != nil,
			Conditional:  s.cond != nil,
			Meta:         s.meta,
			Index:        s.index,
		})
	}
	return out
}

// policyFor returns the retry policy governing a step: its own when it
// set one, the workflow default otherwise.
func (d *Definition) policyFor(s *Step) RetryPolicy {
	if s.retrySet {
		return s.retry
	}
	return d.retry
}

// invalidate drops the cached validation result after a mutation.
func (d *Definition) invalidate() {
	d.validated = false
	d.validErr = nil
	d.layers = nil
}

// On makes the definition start whenever an event of type T is emitted.
//
// It is a package-level function rather than a method because Go does
// not permit generic methods — the same constraint the events package
// solves the same way:
//
//	def := workflow.New("welcome").Step("email", SendWelcomeEmail)
//	workflow.On(def, UserRegistered{})
//
// The emitted value becomes the execution's payload, readable with
// [Payload].
func On[T any](d *Definition, sample T) *Definition {
	return OnType[T](d)
}

// OnType is [On] with the event type given explicitly.
func OnType[T any](d *Definition) *Definition {
	typ := reflect.TypeOf((*T)(nil)).Elem()
	d.trig = &trigger{
		typ:  typ,
		name: events.GetName[T](),
		subscribe: func(bus *events.Bus, start func(payload any)) func() {
			sub := events.OnTypeBus[T](bus, func(_ *events.Context, e T) error {
				start(e)
				return nil
			})
			return sub.Unsubscribe
		},
	}
	return d
}

// --- Validation ---

// Validate checks the definition and reports the first problem found.
// It is called by [Engine.Register]; calling it yourself is a way to
// fail fast at startup. The result is cached, so calling it twice costs
// nothing.
func (d *Definition) Validate() error {
	if d.validated {
		return d.validErr
	}
	d.validErr = d.validate()
	d.validated = true
	return d.validErr
}

func (d *Definition) fail(step string, err error) error {
	return &ValidationError{Workflow: d.name, Step: step, Err: err}
}

func (d *Definition) validate() error {
	if d.name == "" {
		return d.fail("", fmt.Errorf("%w: name is empty", ErrInvalidWorkflow))
	}
	if len(d.steps) == 0 {
		return d.fail("", ErrNoSteps)
	}

	byName := make(map[string]*Step, len(d.steps))
	for _, s := range d.steps {
		switch {
		case s.name == "":
			return d.fail("", fmt.Errorf("%w: step %d has no name", ErrInvalidWorkflow, s.index))
		case s.fn == nil:
			return d.fail(s.name, fmt.Errorf("%w: step function is nil", ErrInvalidWorkflow))
		case s.timeout < 0:
			return d.fail(s.name, fmt.Errorf("%w: negative timeout", ErrInvalidWorkflow))
		}
		if _, dup := byName[s.name]; dup {
			return d.fail(s.name, ErrDuplicateStep)
		}
		if err := validatePolicy(d.policyFor(s)); err != nil {
			return d.fail(s.name, err)
		}
		if s.compensate != nil {
			if err := validatePolicy(s.compRetry); err != nil {
				return d.fail(s.name, fmt.Errorf("compensation: %w", err))
			}
			if s.compTimeout < 0 {
				return d.fail(s.name, fmt.Errorf("%w: negative compensation timeout", ErrInvalidWorkflow))
			}
		}
		byName[s.name] = s
	}

	for _, s := range d.steps {
		for _, dep := range s.dependsOn {
			if dep == s.name {
				return d.fail(s.name, fmt.Errorf("%w: %s depends on itself", ErrWorkflowCycle, s.name))
			}
			if _, ok := byName[dep]; !ok {
				return d.fail(s.name, fmt.Errorf("%w: %q", ErrUnknownDependency, dep))
			}
		}
	}

	layers, err := d.buildPlan(byName)
	if err != nil {
		return err
	}
	d.layers = layers
	return nil
}

// validatePolicy rejects retry settings that could not behave sensibly.
func validatePolicy(p RetryPolicy) error {
	switch {
	case p.MaxAttempts < 0:
		return fmt.Errorf("%w: negative MaxAttempts", ErrInvalidWorkflow)
	case p.Jitter < 0 || p.Jitter > 1:
		return fmt.Errorf("%w: Jitter must be within 0..1", ErrInvalidWorkflow)
	case p.InitialDelay < 0 || p.MaxDelay < 0:
		return fmt.Errorf("%w: negative retry delay", ErrInvalidWorkflow)
	case p.InitialDelay > 0 && p.MaxDelay > 0 && p.MaxDelay < p.InitialDelay:
		return fmt.Errorf("%w: MaxDelay is below InitialDelay", ErrInvalidWorkflow)
	default:
		return nil
	}
}

// buildPlan groups steps into levels using Kahn's algorithm: every step
// in level N depends only on steps in earlier levels, so the engine can
// run a whole level concurrently and then move on.
//
// Ordering within a level follows declaration order, so execution is
// reproducible run to run — which matters for tests and for reading a
// timeline.
func (d *Definition) buildPlan(byName map[string]*Step) ([][]*Step, error) {
	indegree := make(map[string]int, len(d.steps))
	dependants := make(map[string][]*Step, len(d.steps))

	for _, s := range d.steps {
		// Deduplicate: naming the same dependency twice must not
		// inflate the in-degree and strand the step forever.
		seen := make(map[string]struct{}, len(s.dependsOn))
		for _, dep := range s.dependsOn {
			if _, dup := seen[dep]; dup {
				continue
			}
			seen[dep] = struct{}{}
			indegree[s.name]++
			dependants[dep] = append(dependants[dep], s)
		}
	}

	ready := make([]*Step, 0, len(d.steps))
	for _, s := range d.steps {
		if indegree[s.name] == 0 {
			ready = append(ready, s)
		}
	}

	var (
		layers    [][]*Step
		processed int
	)
	for len(ready) > 0 {
		sort.SliceStable(ready, func(i, j int) bool { return ready[i].index < ready[j].index })
		layer := ready
		layers = append(layers, layer)
		processed += len(layer)

		var next []*Step
		for _, s := range layer {
			for _, dep := range dependants[s.name] {
				indegree[dep.name]--
				if indegree[dep.name] == 0 {
					next = append(next, dep)
				}
			}
		}
		ready = next
	}

	if processed != len(d.steps) {
		// Anything left has a non-zero in-degree that never reached
		// zero, which by definition means it sits on a cycle.
		var stuck []string
		for _, s := range d.steps {
			if indegree[s.name] > 0 {
				stuck = append(stuck, s.name)
			}
		}
		sort.Strings(stuck)
		return nil, d.fail("", fmt.Errorf("%w: %v", ErrWorkflowCycle, stuck))
	}
	return layers, nil
}

// plan returns the validated execution layers.
func (d *Definition) plan() ([][]*Step, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	return d.layers, nil
}
