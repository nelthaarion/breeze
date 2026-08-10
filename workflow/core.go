package workflow

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"sync"
	"time"
)

// --- Errors ---

// Sentinel errors returned by the engine. Every error the package
// produces wraps one of these, so callers branch with [errors.Is]
// rather than on strings.
var (
	ErrWorkflowNotFound     = errors.New("workflow: workflow not found")
	ErrDuplicateWorkflow    = errors.New("workflow: workflow already registered")
	ErrWorkflowAlreadyExist = errors.New("workflow: execution already exists")
	ErrWorkflowCancelled    = errors.New("workflow: execution cancelled")
	ErrWorkflowTimeout      = errors.New("workflow: execution timed out")
	ErrStepTimeout          = errors.New("workflow: step timed out")
	ErrNonRetryable         = errors.New("workflow: non-retryable error")
	ErrInvalidWorkflow      = errors.New("workflow: invalid definition")
	ErrWorkflowCycle        = errors.New("workflow: dependency cycle")
	ErrDuplicateStep        = errors.New("workflow: duplicate step name")
	ErrUnknownDependency    = errors.New("workflow: unknown dependency")
	ErrNoSteps              = errors.New("workflow: definition has no steps")
	ErrPersistenceFailure   = errors.New("workflow: persistence failure")
	ErrEngineClosed         = errors.New("workflow: engine is closed")
	ErrStepPanicked         = errors.New("workflow: step panicked")
)

// StepError identifies which step failed, on which attempt, in which
// execution. The engine wraps every step failure in one so that a
// caller holding only the returned error still knows where it came
// from.
type StepError struct {
	Workflow    string
	ExecutionID string
	Step        string
	Attempt     int
	Err         error
}

func (e *StepError) Error() string {
	return fmt.Sprintf("workflow %q step %q (attempt %d): %v", e.Workflow, e.Step, e.Attempt, e.Err)
}

// Unwrap exposes the underlying error so errors.Is and errors.As reach
// the failure the step actually returned.
func (e *StepError) Unwrap() error { return e.Err }

// ValidationError reports one problem found while validating a
// definition. Step is empty when the problem is with the workflow
// itself rather than a particular step.
type ValidationError struct {
	Workflow string
	Step     string
	Err      error
}

func (e *ValidationError) Error() string {
	if e.Step == "" {
		return fmt.Sprintf("workflow %q: %v", e.Workflow, e.Err)
	}
	return fmt.Sprintf("workflow %q step %q: %v", e.Workflow, e.Step, e.Err)
}

func (e *ValidationError) Unwrap() error { return e.Err }

// nonRetryable marks an error as final.
//
// It unwraps to two errors so that both the caller's own sentinel and
// [ErrNonRetryable] are reachable from the same value: a handler can
// return NonRetryable(ErrCardDeclined) and the caller can still ask
// errors.Is(err, ErrCardDeclined). The multi-error Unwrap form is what
// makes that possible without losing either identity.
type nonRetryable struct{ err error }

func (n nonRetryable) Error() string   { return n.err.Error() }
func (n nonRetryable) Unwrap() []error { return []error{n.err, ErrNonRetryable} }

// NonRetryable marks err so that the engine abandons the step
// immediately instead of applying its retry policy. Returning nil
// returns nil, so it is safe to wrap unconditionally.
func NonRetryable(err error) error {
	if err == nil {
		return nil
	}
	return nonRetryable{err: err}
}

// IsNonRetryable reports whether err, or anything it wraps, has been
// marked with [NonRetryable].
func IsNonRetryable(err error) bool { return errors.Is(err, ErrNonRetryable) }

// --- Retry ---

// Backoff selects how the delay between attempts grows.
type Backoff uint8

const (
	// BackoffFixed waits InitialDelay before every retry.
	BackoffFixed Backoff = iota

	// BackoffExponential doubles the delay after each failure, capped
	// at MaxDelay.
	BackoffExponential
)

// String returns the policy name, as persisted and displayed.
func (b Backoff) String() string {
	if b == BackoffExponential {
		return "exponential"
	}
	return "fixed"
}

// Default retry bounds, applied when a policy leaves them at zero.
const (
	defaultInitialDelay = 100 * time.Millisecond
	defaultMaxDelay     = 30 * time.Second
)

// RetryPolicy describes how a failed step is retried. The zero value
// means "do not retry": a step runs exactly once unless it asks for
// more.
type RetryPolicy struct {
	// MaxAttempts is the total number of attempts including the first.
	// Zero or one means no retry.
	MaxAttempts int

	// Backoff selects the delay curve. Defaults to [BackoffFixed].
	Backoff Backoff

	// InitialDelay is the wait before the second attempt. Defaults to
	// 100ms when a retry is actually configured.
	InitialDelay time.Duration

	// MaxDelay caps the computed delay. Defaults to 30s.
	MaxDelay time.Duration

	// Jitter randomises the delay by up to this fraction, in either
	// direction. Zero disables it.
	Jitter float64

	// Retryable decides whether a given error is worth another
	// attempt. Nil means every error is, except those marked with
	// [NonRetryable].
	Retryable func(error) bool
}

// enabled reports whether the policy permits more than one attempt.
func (p RetryPolicy) enabled() bool { return p.MaxAttempts > 1 }

// Delay returns how long to wait before the attempt following the given
// 1-based attempt number.
//
// Jitter is applied last, as a random offset in [-J,+J] where J is
// Jitter × delay. Spreading retries matters more than it looks: when a
// dependency fails, every in-flight execution fails at nearly the same
// instant, and without jitter they would all retry in the same instant
// too, reproducing the load spike that caused the failure.
func (p RetryPolicy) Delay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}

	initial := p.InitialDelay
	if initial <= 0 {
		initial = defaultInitialDelay
	}
	maxDelay := p.MaxDelay
	if maxDelay <= 0 {
		maxDelay = defaultMaxDelay
	}

	delay := initial
	if p.Backoff == BackoffExponential {
		// Shift only while it cannot overflow: 1<<62 already exceeds
		// any sane delay, and time.Duration is a signed int64.
		shift := attempt - 1
		if shift > 62 {
			delay = maxDelay
		} else {
			scaled := initial << uint(shift)
			if scaled <= 0 || scaled > maxDelay {
				delay = maxDelay
			} else {
				delay = scaled
			}
		}
	}
	if delay > maxDelay {
		delay = maxDelay
	}

	if p.Jitter > 0 {
		j := p.Jitter
		if j > 1 {
			j = 1
		}
		span := float64(delay) * j
		// rand/v2's top-level source is safe for concurrent use and
		// needs no seeding.
		offset := (rand.Float64()*2 - 1) * span
		delay = time.Duration(math.Max(0, float64(delay)+offset))
	}
	return delay
}

// ShouldRetry reports whether a step that failed on the given 1-based
// attempt should be tried again.
func (p RetryPolicy) ShouldRetry(attempt int, err error) bool {
	switch {
	case err == nil:
		return false
	case attempt >= p.MaxAttempts:
		return false
	case IsNonRetryable(err):
		return false
	case errors.Is(err, context.Canceled):
		return false
	case p.Retryable != nil && !p.Retryable(err):
		return false
	default:
		return true
	}
}

// --- Context ---

// Context is what a step receives. It carries the execution's identity,
// the deadline governing it, and the metadata steps use to hand values
// to the steps that follow them.
//
// It is not an HTTP request context: a workflow outlives the request
// that started it, so binding one to a request context would cancel the
// workflow the moment the client disconnected.
type Context struct {
	// Ctx governs cancellation and deadlines for the current step. It
	// is never nil, and it is replaced for each step so that a step
	// timeout cancels only that step.
	Ctx context.Context

	workflow    string
	executionID string
	correlation string
	payloadVal  any

	// step and attempt change as execution advances, so they are
	// guarded like the metadata is.
	mu      *sync.RWMutex
	step    *string
	attempt *int
	meta    map[string]any
}

// newContext builds the root context for one execution. Every step
// context is derived from it with withCtx, so they all share one
// metadata map and one mutex.
func newContext(ctx context.Context, workflow, executionID, correlationID string, payload any, meta map[string]any) *Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if meta == nil {
		meta = make(map[string]any, 4)
	}
	step := ""
	attempt := 0
	return &Context{
		Ctx:         ctx,
		workflow:    workflow,
		executionID: executionID,
		correlation: correlationID,
		payloadVal:  payload,
		mu:          new(sync.RWMutex),
		step:        &step,
		attempt:     &attempt,
		meta:        meta,
	}
}

// withCtx returns a shallow copy governed by a different context.
//
// The copy shares the metadata map and the mutex by pointer, which is
// the entire point: a value stored by one step must be visible to the
// next, and to steps running beside it in the same level.
func (c *Context) withCtx(ctx context.Context) *Context {
	cp := *c
	cp.Ctx = ctx
	return &cp
}

// setStep records which step is running and on which attempt.
func (c *Context) setStep(name string, attempt int) {
	c.mu.Lock()
	*c.step = name
	*c.attempt = attempt
	c.mu.Unlock()
}

// payload returns the untyped value the execution was started with.
func (c *Context) payload() any { return c.payloadVal }

// Workflow returns the name of the workflow being executed.
func (c *Context) Workflow() string { return c.workflow }

// ExecutionID returns the unique identifier of this execution.
func (c *Context) ExecutionID() string { return c.executionID }

// CorrelationID ties this execution to a wider logical operation, such
// as the request that started it.
func (c *Context) CorrelationID() string { return c.correlation }

// Step returns the name of the step currently running.
func (c *Context) Step() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return *c.step
}

// Attempt returns the 1-based attempt number of the current step.
func (c *Context) Attempt() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return *c.attempt
}

// Set stores a value for later steps to read. It is safe to call from
// steps running concurrently.
func (c *Context) Set(key string, value any) {
	c.mu.Lock()
	c.meta[key] = value
	c.mu.Unlock()
}

// Get returns a value stored by an earlier step.
func (c *Context) Get(key string) (any, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.meta[key]
	return v, ok
}

// MetaString returns a stored value when it is a string.
func (c *Context) MetaString(key string) (string, bool) {
	v, ok := c.Get(key)
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// Metadata returns a copy of everything stored so far. The copy means a
// caller can range over it while other steps keep writing.
func (c *Context) Metadata() map[string]any {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]any, len(c.meta))
	for k, v := range c.meta {
		out[k] = v
	}
	return out
}

// Done returns the channel that closes when the current step's context
// is cancelled, so a step can select on it.
func (c *Context) Done() <-chan struct{} { return c.Ctx.Done() }

// Err reports why the current step's context was cancelled, if it was.
func (c *Context) Err() error { return c.Ctx.Err() }

// Deadline returns the current step's deadline, when it has one.
func (c *Context) Deadline() (time.Time, bool) { return c.Ctx.Deadline() }

// Payload returns the value the execution was started with, converted
// to T. It reports false when the execution carries no payload or the
// payload is of a different type, so a step can decide what to do
// rather than panicking on a bad assertion.
func Payload[T any](c *Context) (T, bool) {
	var zero T
	if c == nil {
		return zero, false
	}
	v, ok := c.payload().(T)
	if !ok {
		return zero, false
	}
	return v, true
}
