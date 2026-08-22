package breeze

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
)

// WorkerPool dispatches tasks to a fixed pool of goroutines.
//
// # Backpressure
//
// When the task queue is full, the pool's behavior is controlled by the
// OverflowPolicy configured at construction time:
//
//   - OverflowBlock (default): Submit blocks until a worker drains a slot.
//     Provides backpressure. WARNING: do NOT use from a gnet event-loop
//     goroutine — blocking the event loop stalls ALL connections on that
//     reactor. Use NewEventLoopWorkerPool for event-loop callers.
//
//   - OverflowReject: Submit returns immediately without executing the
//     task. The caller detects this via SubmitErr's error return.
//     Submit (the no-error API) silently drops the task.
//
//   - OverflowSpawn: Submit spawns a new goroutine to run the task. This
//     is the legacy behavior, now EXPLICIT — callers must use
//     NewEventLoopWorkerPool or NewWorkerPoolWithConfig with
//     Overflow: OverflowSpawn. Under sustained overload it can create
//     unbounded goroutines; use only when the caller cannot tolerate
//     blocking (e.g., a network event loop).
//
// # Allocation model
//
// Tasks are submitted as func() values. The pool does NOT wrap each task
// in a per-Submit closure — panic recovery lives in the worker loop, so
// the only per-Submit allocation is the func() value itself (which the
// caller creates).
//
// # Shutdown state machine
//
// Shutdown uses a two-phase close to avoid TOCTOU races:
//
//  1. Close the `done` channel — wakes up any blocked Submits and causes
//     future Submits to return ErrPoolClosed.
//  2. Wait for all in-flight tasks to complete (wg.Wait).
//  3. Close the `tasks` channel — workers drain remaining queued tasks
//     and exit.
//
// The `done` channel is used in EVERY Submit path's select, so a closed
// pool cannot panic on "send on closed channel" — the select always has
// the `<-p.done` case to catch it.
//
// # Safety
//
//   - Submit is safe to call concurrently from multiple goroutines.
//   - Submit after Shutdown returns ErrPoolClosed (SubmitErr) or is a
//     silent no-op (Submit).
//   - A panicking task is logged and recovered — the worker continues.
//   - Workers exit cleanly when the tasks channel is closed.
//   - Shutdown is idempotent (safe to call multiple times).
const defaultChannelMultiplier = 16

// OverflowPolicy controls WorkerPool behavior when the task queue is full.
type OverflowPolicy int

const (
	// OverflowBlock makes Submit block until a worker drains a queue slot.
	// This is the DEFAULT policy for NewWorkerPoolWithConfig.
	// Do NOT use from a gnet event-loop goroutine.
	OverflowBlock OverflowPolicy = iota

	// OverflowReject makes Submit drop the task immediately when the queue
	// is full. Submit silently drops; SubmitErr returns ErrQueueFull.
	OverflowReject

	// OverflowSpawn makes Submit spawn a new goroutine to run the task when
	// the queue is full. This is the policy used by NewEventLoopWorkerPool
	// for gnet event-loop callers. Under sustained overload it can create
	// unbounded goroutines.
	OverflowSpawn
)

// WorkerPoolConfig configures a WorkerPool.
//
// Zero-value defaults:
//
//   - Workers: runtime.NumCPU() (if <= 0)
//   - QueueSize: Workers * 16 (if <= 0)
//   - Overflow: OverflowBlock (the recommended default for new code)
type WorkerPoolConfig struct {
	Workers   int
	QueueSize int
	Overflow  OverflowPolicy
}

// poolTask is the value type enqueued on the tasks channel.
// One word (8 B on 64-bit) — the func() pointer.
type poolTask struct {
	fn func()
}

// cacheLine is the coherency granule the CPU actually invalidates. 64 bytes on
// amd64 and arm64, which is what this runs on.
const cacheLine = 64

// paddedCounter is an atomic.Int64 that occupies a full cache line on its own.
//
// # Why this exists
//
// The five pool counters used to be declared as five adjacent atomic.Int64
// fields, which put all of them inside one 64-byte cache line. A cache line is
// the unit of coherency traffic, so an atomic increment of ANY of them stole
// the line from every core holding it — including cores about to bump a
// completely different counter. That is false sharing: the counters are
// logically independent but physically contend.
//
// It bites hardest exactly where it hurts most. submitted and queued are both
// incremented on every successful Submit, and Submit is called concurrently by
// every gnet event loop at once. So the common case was two guaranteed
// line-invalidating RMWs per submit, each one racing N-1 other cores for
// ownership of the same line. The atomic ops themselves are cheap; the
// cross-core line ping-pong around them is not.
//
// Giving each counter its own line makes the increments independent again. The
// cost is 5 × 64 B per pool, paid once at construction.
//
// atomic.Int64 is 8 bytes wide on every platform (its noCopy and align64
// members are zero-sized; align64 only constrains alignment), so 56 bytes of
// tail padding brings the struct to exactly one line.
type paddedCounter struct {
	v atomic.Int64
	_ [cacheLine - 8]byte
}

func (c *paddedCounter) Add(n int64) { c.v.Add(n) }

func (c *paddedCounter) Load() int64 { return c.v.Load() }

// Errors returned by SubmitErr.
var (
	ErrPoolClosed = errors.New("breeze: worker pool is closed")
	ErrQueueFull  = errors.New("breeze: worker pool queue is full")
)

// WorkerPool dispatches tasks to a fixed pool of goroutines.
type WorkerPool struct {
	tasks    chan poolTask
	wg       sync.WaitGroup
	count    int
	overflow OverflowPolicy

	// done is closed by Shutdown to signal that no new tasks should be
	// accepted. Every Submit path selects on done, so a closed pool's
	// Submit never panics on "send on closed channel" — it returns
	// ErrPoolClosed instead.
	done chan struct{}

	// doneOnce ensures close(p.done) happens exactly once.
	doneOnce sync.Once

	// tasksCloseOnce ensures close(p.tasks) happens exactly once, even
	// if Shutdown is called multiple times with different timeouts.
	tasksCloseOnce sync.Once

	// Metrics for observability. All atomic for lock-free reads.
	//
	// Each counter sits on its own cache line — see paddedCounter for why.
	// They are declared last so the padding does not push the hot fields
	// (tasks, overflow) apart.
	submitted paddedCounter
	queued    paddedCounter
	spawned   paddedCounter
	rejected  paddedCounter
	panicked  paddedCounter
}

// NewWorkerPoolWithConfig is the main constructor. It creates a pool with
// explicit configuration. Zero-value fields use the defaults documented on
// WorkerPoolConfig. The Overflow field defaults to OverflowBlock.
//
// This is the recommended constructor for all new code. For code that
// calls Submit from a gnet event-loop goroutine (e.g., inside OnTraffic),
// use NewEventLoopWorkerPool instead — OverflowBlock would stall the
// event loop.
func NewWorkerPoolWithConfig(cfg WorkerPoolConfig) *WorkerPool {
	if cfg.Workers <= 0 {
		cfg.Workers = runtime.NumCPU()
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = cfg.Workers * defaultChannelMultiplier
	}
	p := &WorkerPool{
		tasks:    make(chan poolTask, cfg.QueueSize),
		count:    cfg.Workers,
		overflow: cfg.Overflow,
		done:     make(chan struct{}),
	}
	for i := 0; i < cfg.Workers; i++ {
		go p.worker()
	}
	return p
}

// NewEventLoopWorkerPool creates a pool with OverflowSpawn policy.
//
// Use this constructor when Submit will be called from a gnet event-loop
// goroutine (e.g., inside OnTraffic or OnTraffic-derived callbacks). The
// Spawn policy ensures Submit never blocks — if the queue is full, a
// goroutine is spawned to run the task. This prevents stalling the event
// loop, which would block ALL connections on that reactor.
//
// Trade-off: under sustained overload, Spawn can create unbounded
// goroutines. This is acceptable for event-loop callers because the
// alternative (blocking) is worse — a stalled event loop serves no one.
//
// For non-event-loop callers (worker-to-worker pipelines, background
// producers), use NewWorkerPoolWithConfig with OverflowBlock instead.
func NewEventLoopWorkerPool(concurrency int) *WorkerPool {
	if concurrency <= 0 {
		concurrency = runtime.NumCPU()
	}
	return NewWorkerPoolWithConfig(WorkerPoolConfig{
		Workers:  concurrency,
		Overflow: OverflowSpawn,
	})
}

// NewWorkerPool creates a pool with `concurrency` goroutines.
//
// Deprecated: This constructor uses OverflowSpawn for backward
// compatibility. New code should use either:
//
//   - NewEventLoopWorkerPool(n) — if Submit is called from a gnet
//     event-loop goroutine (equivalent to this constructor, but explicit)
//   - NewWorkerPoolWithConfig(cfg) — for all other callers (defaults to
//     OverflowBlock, the recommended policy)
//
// This function is retained so existing user code keeps compiling and
// behaving identically. It will not be removed.
func NewWorkerPool(concurrency int) *WorkerPool {
	return NewEventLoopWorkerPool(concurrency)
}

// worker is the goroutine that drains the tasks channel.
//
// Panic recovery lives HERE (not in a per-Submit wrapper) so that Submit
// does not allocate a closure on every call. The worker exits when the
// channel is closed and drained.
func (p *WorkerPool) worker() {
	for task := range p.tasks {
		p.runTask(task.fn)
	}
}

// runTask executes a single task with panic recovery.
//
// The WaitGroup Done is deferred FIRST so it always runs, even if the
// recover re-panics. Shared between the worker loop (queued tasks) and
// the spawn path (OverflowSpawn fallback).
func (p *WorkerPool) runTask(fn func()) {
	defer p.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			p.panicked.Add(1)
			fmt.Printf("[Breeze][WorkerPool][PANIC] %v\n%s\n", r, debug.Stack())
		}
	}()
	fn()
}

// Submit enqueues a task for execution by a worker goroutine.
//
// Behavior when the queue is full depends on the pool's OverflowPolicy:
//
//   - OverflowBlock:  Submit blocks until a slot is available.
//   - OverflowReject: Submit returns immediately; the task is dropped.
//   - OverflowSpawn:  Submit spawns a goroutine to run the task.
//
// After Shutdown has been called, Submit is a silent no-op.
//
// Submit does NOT return an error. Callers that need to detect rejected
// tasks should use SubmitErr.
func (p *WorkerPool) Submit(f func()) {
	_ = p.submit(f)
}

// SubmitErr enqueues a task and returns an error if the task was rejected
// or if the pool is shut down.
//
// Returns nil if the task was queued or spawned.
// Returns ErrPoolClosed if the pool is shut down.
// Returns ErrQueueFull if the queue is full and Overflow == OverflowReject.
func (p *WorkerPool) SubmitErr(f func()) error {
	return p.submit(f)
}

// submit is the shared implementation for Submit and SubmitErr.
//
// The `done` channel is checked on EVERY path via select, so a closed
// pool's Submit never panics on "send on closed channel". This eliminates
// the TOCTOU race that existed in the pre-audit version (where closed was
// an atomic.Bool checked before the send, leaving a window between the
// check and the send).
func (p *WorkerPool) submit(f func()) error {
	p.submitted.Add(1)

	// Fast path: if done is already closed, reject immediately without
	// touching the WaitGroup or the channel.
	select {
	case <-p.done:
		return ErrPoolClosed
	default:
	}

	// The WaitGroup Add MUST happen before the task is dispatched so that
	// Shutdown's wg.Wait sees all in-flight tasks. If the pool shuts down
	// between the done-check above and the send below, the select's
	// <-p.done case will fire and we decrement the Add before returning.
	p.wg.Add(1)

	task := poolTask{fn: f}

	switch p.overflow {
	case OverflowBlock:
		select {
		case p.tasks <- task:
			p.queued.Add(1)
		case <-p.done:
			p.wg.Done()
			return ErrPoolClosed
		}

	case OverflowReject:
		select {
		case p.tasks <- task:
			p.queued.Add(1)
		default:
			p.wg.Done()
			p.rejected.Add(1)
			return ErrQueueFull
		}

	case OverflowSpawn:
		select {
		case p.tasks <- task:
			p.queued.Add(1)
		default:
			p.spawned.Add(1)
			go p.runTask(f)
		}

	default:
		// Unknown policy — fall back to Block (safest).
		select {
		case p.tasks <- task:
			p.queued.Add(1)
		case <-p.done:
			p.wg.Done()
			return ErrPoolClosed
		}
	}

	return nil
}

// Shutdown waits for all in-flight tasks to complete or for ctx to expire.
//
// Shutdown is idempotent — calling it multiple times is safe.
//
// After Shutdown returns:
//   - The `done` channel is closed (future Submits return ErrPoolClosed).
//   - All worker goroutines have exited (if ctx did not expire first).
//   - The tasks channel is closed (if ctx did not expire first).
//
// If ctx expires before all in-flight tasks complete, the pool is left
// in a "draining" state: `done` is closed (no new tasks accepted), but
// workers stay alive to process in-flight tasks. The pool's resources
// are not freed until all tasks finish and the channel is eventually
// closed (by a subsequent Shutdown call with a longer timeout, or by
// process exit).
func (p *WorkerPool) Shutdown(ctx context.Context) {
	// Phase 1: close `done` to reject new Submits. Idempotent via Once.
	p.doneOnce.Do(func() {
		close(p.done)
	})

	// Phase 2: wait for all in-flight tasks to complete.
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// All in-flight tasks completed. Safe to close the tasks channel.
		// Workers will drain any remaining queued tasks and exit.
		// Protected by tasksCloseOnce — safe across multiple Shutdown calls.
		p.tasksCloseOnce.Do(func() {
			close(p.tasks)
		})
	case <-ctx.Done():
		// Timeout — leave workers running to drain in-flight tasks.
		// A subsequent Shutdown call with a longer timeout can finish
		// the close.
	}
}

// WorkerPoolMetrics is a point-in-time snapshot of pool counters.
type WorkerPoolMetrics struct {
	Submitted int64 // total Submit calls (including rejected/spawned)
	Queued    int64 // tasks successfully enqueued to the channel
	Spawned   int64 // tasks run via `go` fallback (OverflowSpawn only)
	Rejected  int64 // tasks dropped (OverflowReject only)
	Panicked  int64 // tasks that panicked (recovered by worker)
	Workers   int   // configured worker count
	InFlight  int64 // tasks submitted but not yet completed
}

// Metrics returns a point-in-time snapshot of pool counters.
func (p *WorkerPool) Metrics() WorkerPoolMetrics {
	submitted := p.submitted.Load()
	queued := p.queued.Load()
	spawned := p.spawned.Load()
	rejected := p.rejected.Load()
	panicked := p.panicked.Load()
	return WorkerPoolMetrics{
		Submitted: submitted,
		Queued:    queued,
		Spawned:   spawned,
		Rejected:  rejected,
		Panicked:  panicked,
		Workers:   p.count,
		InFlight:  submitted - queued - spawned - rejected,
	}
}
