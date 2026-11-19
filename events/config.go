package events

import (
	"fmt"
	"os"
	"runtime"
)

// AsyncMode selects how [EmitAsync] schedules listeners.
type AsyncMode int

const (
	// AsyncGoroutine starts one goroutine per listener. It has the lowest
	// latency and no queueing, but places no ceiling on concurrency —
	// suitable when emit rates are bounded by something else (requests in
	// flight, scheduler ticks).
	AsyncGoroutine AsyncMode = iota

	// AsyncWorkerPool submits listeners to a bounded pool owned by the
	// bus. Concurrency is capped at [Config.Workers], which keeps a burst
	// of events from spawning thousands of goroutines. When the queue is
	// full the behaviour is set by [Config.AsyncOverflow].
	AsyncWorkerPool
)

// String implements fmt.Stringer.
func (m AsyncMode) String() string {
	switch m {
	case AsyncGoroutine:
		return "goroutine"
	case AsyncWorkerPool:
		return "worker-pool"
	default:
		return fmt.Sprintf("AsyncMode(%d)", int(m))
	}
}

// OverflowPolicy controls what the async worker pool does when its queue
// is saturated.
type OverflowPolicy int

const (
	// OverflowBlock makes the emitting goroutine wait for a free slot.
	// This applies backpressure to the producer. Do not use it from a
	// network event-loop goroutine, where blocking stalls every
	// connection on the reactor.
	OverflowBlock OverflowPolicy = iota

	// OverflowSpawn runs the listener in a fresh goroutine when the queue
	// is full. Emit never blocks, at the cost of unbounded goroutines
	// under sustained overload. This is the default: dropping a framework
	// event silently is worse than briefly exceeding the worker budget.
	OverflowSpawn

	// OverflowDrop discards the listener invocation and increments the
	// Dropped metric. Use it for high-volume telemetry events where
	// lagging is worse than losing samples.
	OverflowDrop
)

// String implements fmt.Stringer.
func (p OverflowPolicy) String() string {
	switch p {
	case OverflowBlock:
		return "block"
	case OverflowSpawn:
		return "spawn"
	case OverflowDrop:
		return "drop"
	default:
		return fmt.Sprintf("OverflowPolicy(%d)", int(p))
	}
}

// PanicMode controls how a recovered panic affects the dispatch.
type PanicMode int

const (
	// PanicRecoverAndContinue recovers the panic, reports it to the panic
	// handler, and runs the remaining listeners. One broken plugin cannot
	// then suppress every other listener on the same event.
	PanicRecoverAndContinue PanicMode = iota

	// PanicRecoverAndFail recovers the panic, reports it, stops the
	// dispatch, and returns a [*PanicError] from Emit.
	PanicRecoverAndFail

	// PanicPropagate re-panics after reporting. Reserved for tests and
	// for deployments that prefer to crash on programmer error.
	PanicPropagate
)

// String implements fmt.Stringer.
func (m PanicMode) String() string {
	switch m {
	case PanicRecoverAndContinue:
		return "recover-and-continue"
	case PanicRecoverAndFail:
		return "recover-and-fail"
	case PanicPropagate:
		return "propagate"
	default:
		return fmt.Sprintf("PanicMode(%d)", int(m))
	}
}

// PanicHandler is invoked for every recovered panic. It must not panic.
type PanicHandler func(*PanicError)

// ErrorHandler is invoked for every error a listener returns, including
// errors that stop the dispatch. It is the hook loggers attach to; it does
// not change control flow. It is never called with [Stop].
type ErrorHandler func(ctx *Context, listener string, err error)

// Config configures a [Bus]. The zero value is valid and yields the
// defaults documented on each field, so callers may set only what they
// care about:
//
//	bus := events.New(events.Config{Async: events.AsyncWorkerPool})
type Config struct {
	// ContinueOnError determines what happens when a listener returns a
	// non-nil, non-[Stop] error.
	//
	// False (default): the dispatch stops and Emit returns that error.
	// True: the remaining listeners still run and Emit returns a
	// [*MultiError] aggregating every failure.
	ContinueOnError bool

	// PanicMode selects the panic strategy. Defaults to
	// [PanicRecoverAndContinue].
	PanicMode PanicMode

	// OnPanic receives every recovered panic. Defaults to a handler that
	// writes the message and stack to stderr.
	OnPanic PanicHandler

	// OnError receives every listener error. Defaults to nil (no
	// reporting); Emit still returns the error.
	OnError ErrorHandler

	// Async selects the [EmitAsync] scheduling strategy. Defaults to
	// [AsyncGoroutine].
	Async AsyncMode

	// Workers sets the pool size when Async is [AsyncWorkerPool].
	// Defaults to runtime.NumCPU().
	Workers int

	// QueueSize sets the pool's queue depth when Async is
	// [AsyncWorkerPool]. Defaults to Workers * 64.
	QueueSize int

	// AsyncOverflow selects the policy applied when the async queue is
	// full. Defaults to [OverflowSpawn].
	AsyncOverflow OverflowPolicy

	// Metrics enables per-event statistics collection. Defaults to true.
	// Disable it via DisableMetrics; the counters cost two atomic adds
	// and a duration store per dispatch.
	Metrics bool

	// DisableMetrics turns metrics off. It exists because the zero value
	// of Metrics must mean "on" for a usable zero-value Config.
	DisableMetrics bool

	// Recorder enables the ring-buffer event recorder at construction.
	// It can also be toggled at runtime with [Bus.EnableRecorder].
	Recorder bool

	// RecorderSize is the recorder's ring-buffer capacity. Defaults to
	// [DefaultRecorderSize].
	RecorderSize int

	// DisableDiagnostics stops [New] from registering the bus as the
	// process's event-bus diagnostic probe. By default every new bus
	// replaces the previously registered one, which is right for the bus an
	// application actually runs on and wrong for a private bus — a
	// library's internal bus, a per-tenant bus, a test double — that would
	// otherwise take over the report without anyone using it. Set it for
	// those. [RegisterDiagnostics] can still publish such a bus explicitly.
	DisableDiagnostics bool
}

// DefaultRecorderSize is the recorder ring-buffer capacity used when
// [Config.RecorderSize] is not set.
const DefaultRecorderSize = 256

// defaultQueueMultiplier derives the async queue depth from the worker
// count when [Config.QueueSize] is not set.
const defaultQueueMultiplier = 64

// normalize fills in defaults and resolves the Metrics/DisableMetrics
// pair into a single effective value.
func (c Config) normalize() Config {
	if c.Workers <= 0 {
		c.Workers = runtime.NumCPU()
	}
	if c.QueueSize <= 0 {
		c.QueueSize = c.Workers * defaultQueueMultiplier
	}
	if c.RecorderSize <= 0 {
		c.RecorderSize = DefaultRecorderSize
	}
	if c.OnPanic == nil {
		c.OnPanic = defaultPanicHandler
	}
	c.Metrics = !c.DisableMetrics
	return c
}

// defaultPanicHandler writes the panic and its stack to stderr. It mirrors
// the format used by the framework's worker pool so operators see one
// consistent shape for recovered panics.
func defaultPanicHandler(p *PanicError) {
	fmt.Fprintf(os.Stderr, "[Breeze][events][PANIC] %s\n%s\n", p.Error(), p.Stack)
}
