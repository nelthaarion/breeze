package events

import (
	"sync"
	"sync/atomic"
)

// workerPool is the bounded executor used by [AsyncWorkerPool].
//
// The package deliberately ships its own pool instead of importing the
// framework's: events must stay dependency-free so that any Breeze
// subsystem — including the framework core itself — can import it without
// creating a cycle. The pool is small because it only needs to run
// listener invocations, which the dispatcher has already wrapped in panic
// recovery.
type workerPool struct {
	tasks chan func()
	wg    sync.WaitGroup

	// closeMu serialises submits against the close of the tasks channel.
	// Submitters hold it for reading, close holds it for writing, which
	// guarantees `close(p.tasks)` cannot interleave with a send. A
	// done-channel alone cannot rule that out, because observing done in
	// a select and then sending are not one atomic step.
	closeMu sync.RWMutex
	closed  bool

	// done is closed on shutdown so that a submitter parked on a full
	// queue wakes up instead of waiting for a drain that will not come.
	done     chan struct{}
	doneOnce sync.Once

	overflow OverflowPolicy
	workers  int

	spawned atomic.Uint64
	dropped atomic.Uint64
	queued  atomic.Uint64
}

// newWorkerPool starts a pool with the given worker count and queue size.
func newWorkerPool(workers, queueSize int, overflow OverflowPolicy) *workerPool {
	if workers <= 0 {
		workers = 1
	}
	if queueSize <= 0 {
		queueSize = workers
	}
	p := &workerPool{
		tasks:    make(chan func(), queueSize),
		done:     make(chan struct{}),
		overflow: overflow,
		workers:  workers,
	}
	p.wg.Add(workers)
	for i := 0; i < workers; i++ {
		go p.run()
	}
	return p
}

// run drains the task channel until it is closed.
//
// Tasks are not wrapped in recovery here: the dispatcher has already
// wrapped every listener invocation, and a second recover would strip the
// event context out of the panic report.
func (p *workerPool) run() {
	defer p.wg.Done()
	for task := range p.tasks {
		task()
	}
}

// submit schedules task according to the pool's overflow policy.
// It reports whether the task will run.
func (p *workerPool) submit(task func()) bool {
	// The read lock is held across the send so close cannot shut the
	// channel underneath it. Submitters do not exclude each other; only
	// close is exclusive.
	p.closeMu.RLock()
	defer p.closeMu.RUnlock()

	if p.closed {
		return false
	}

	switch p.overflow {
	case OverflowBlock:
		select {
		case p.tasks <- task:
			p.queued.Add(1)
			return true
		case <-p.done:
			return false
		}

	case OverflowDrop:
		select {
		case p.tasks <- task:
			p.queued.Add(1)
			return true
		default:
			p.dropped.Add(1)
			return false
		}

	default: // OverflowSpawn (and the defensive fallback for an unknown value)
		select {
		case p.tasks <- task:
			p.queued.Add(1)
			return true
		default:
			p.spawned.Add(1)
			// The task is outside the worker goroutines, so the worker
			// WaitGroup must account for it explicitly. Without this Add,
			// Close could return while an overflow listener still held
			// application state or was about to emit another event.
			p.wg.Add(1)
			go func() {
				defer p.wg.Done()
				task()
			}()
			return true
		}
	}
}

// close stops the pool and waits for its workers to drain. Tasks already
// queued still run; later submits return false. close is idempotent.
func (p *workerPool) close() {
	// Signal first. A submitter parked on a full queue under
	// OverflowBlock holds the read lock and will not release it until its
	// select fires, so closing done gives that select a branch to take
	// and lets the write lock below be acquired.
	p.doneOnce.Do(func() { close(p.done) })

	p.closeMu.Lock()
	if !p.closed {
		p.closed = true
		// Safe here: the write lock excludes every submitter, so no send
		// can be in flight on p.tasks.
		close(p.tasks)
	}
	// The lock is released BEFORE waiting. A queued task may itself emit
	// an async event, and that nested submit needs the read lock; holding
	// the write lock across wg.Wait would deadlock against it. Once
	// closed is set, such a submit returns false rather than blocking.
	p.closeMu.Unlock()

	p.wg.Wait()
}

// stats returns the pool counters for the inspector.
func (p *workerPool) stats() PoolStats {
	return PoolStats{
		Workers:  p.workers,
		Queued:   p.queued.Load(),
		Spawned:  p.spawned.Load(),
		Dropped:  p.dropped.Load(),
		Pending:  len(p.tasks),
		Capacity: cap(p.tasks),
	}
}

// PoolStats is a point-in-time snapshot of the async worker pool.
type PoolStats struct {
	// Workers is the configured goroutine count.
	Workers int
	// Queued is the number of tasks that entered the queue.
	Queued uint64
	// Spawned is the number of tasks run on an overflow goroutine.
	Spawned uint64
	// Dropped is the number of tasks discarded under [OverflowDrop].
	Dropped uint64
	// Pending is the number of tasks waiting in the queue right now.
	Pending int
	// Capacity is the queue depth.
	Capacity int
}
