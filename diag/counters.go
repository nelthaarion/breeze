package diag

// counters.go — counted diagnostics, off until something asks for them.
//
// # The problem this solves
//
// Most subsystems can be diagnosed for free because they already hold the
// answer: the event bus already counts dispatches, the fleet tracer already
// counts export failures, the template engine already knows its cache size. A
// probe over those costs nothing because it only reads what exists.
//
// A few subsystems hold nothing. Compression does not know how many responses it
// compressed; the ETag middleware does not know its own hit rate; the rate
// limiter knows its client map but not how many requests it rejected. Those facts
// are exactly the ones an operator asks for, and none of them can be recovered
// after the fact — they have to be counted as they happen, on the response path.
//
// # Why there is a gate
//
// A counter on a response path is not free. One atomic increment is a handful of
// nanoseconds in isolation, but a *shared* counter incremented by every core
// moves a cache line between cores on every request, and under real concurrency
// that coherence traffic is the cost, not the instruction.
//
// So counting is off by default and the hot path reads a gate first:
//
//	if !counting.Load() { return }
//
// That load is a read of a global that is written approximately never. It sits in
// every core's cache in shared state, costs no coherence traffic at all, and the
// branch predicts perfectly. It is as close to zero as a runtime check gets — and
// unlike the increment it protects, it does not get worse as cores are added.
//
// # Who turns it on
//
// [EnableCounters] is called by the things whose presence already means the
// process has accepted observability cost: dashboard.Install and
// mcp.ServeInProcess both call it. A bare application that installed neither is
// not paying for counters it has nothing to read them with.
//
// Probes for counter-backed subsystems always report whether counting is on, so
// a zero is never ambiguous between "nothing happened" and "nothing was counted".

import (
	"sync"
	"sync/atomic"
	"time"
)

// counting is the process-wide gate. Read on hot paths, written at most a few
// times in a process's life.
var counting atomic.Bool

// countingSince records when counting was last enabled, so a snapshot can say
// what window its numbers cover. Guarded by countingMu rather than being an
// atomic, because it is only ever touched by Enable/Disable/Snapshot.
var (
	countingMu    sync.Mutex
	countingSince time.Time
)

// EnableCounters turns on counted diagnostics for the whole process.
//
// Idempotent, and safe to call from anywhere at any time — including from a
// running application, which is the point: an operator diagnosing a live problem
// can turn counting on without a restart, and the reports will then cover the
// window from that moment rather than pretending to cover the process's life.
//
// Enabling does not retroactively invent numbers. Every counter starts from where
// it was, which for a process that never enabled counting before is zero.
func EnableCounters() {
	countingMu.Lock()
	defer countingMu.Unlock()

	if counting.Load() {
		return
	}
	countingSince = time.Now()
	counting.Store(true)
}

// DisableCounters turns counting off again, restoring the untouched hot path.
//
// The accumulated values are kept rather than zeroed. Discarding them would mean
// a caller that disabled counting for a benchmark also destroyed the evidence it
// had collected, and re-enabling would silently restart from zero with no note
// saying so.
func DisableCounters() {
	countingMu.Lock()
	defer countingMu.Unlock()

	counting.Store(false)
}

// CountersEnabled reports whether counting is on.
//
// Probes use it to qualify their numbers; nothing on a hot path should call it,
// because the counter methods already do the check themselves and calling it
// first would just double the load.
func CountersEnabled() bool { return counting.Load() }

// CountersSince reports when counting was enabled, and whether it is on.
//
// A caller rendering a rate needs the window, and "since process start" is the
// one answer this must never give: counting can be enabled at any moment, and a
// rate computed against the wrong denominator is worse than no rate.
func CountersSince() (time.Time, bool) {
	countingMu.Lock()
	defer countingMu.Unlock()
	return countingSince, counting.Load()
}
