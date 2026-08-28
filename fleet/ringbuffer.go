package fleet

// spanRing is the bounded buffer spans wait in between being recorded on a
// request and being picked up by the export goroutine.
//
// # Why a copy of dashboard's ringBuffer rather than the original
//
// dashboard.ringBuffer[T] is the same idea and is already generic, but it is
// unexported. Exporting it would make an internal storage detail of the
// dashboard part of its public API permanently, to save these few lines — and
// this buffer needs one operation the dashboard's does not have: Drain, which
// removes what it returns. Snapshot-then-clear is not a substitute, because
// spans recorded between the two calls would be lost.
//
// # The overflow rule
//
// Full means drop the oldest, never block and never grow. A request must not
// wait on tracing, and a service must not run out of memory because its
// aggregator went away. Losing the oldest spans is the correct sacrifice: the
// newest are the ones an operator is currently looking at.

import "sync"

type spanRing struct {
	// A mutex, not a lock-free queue. Contention is a short append against
	// one drain per flush interval, so the lock is uncontended in practice,
	// and the drop-oldest policy needs an atomic read-modify-write of two
	// indices that a channel or lock-free ring cannot express as cheaply.
	mu       sync.Mutex
	entries  []Span
	head     int // index of the oldest entry
	count    int
	capacity int

	// dropped counts spans lost to overflow. Guarded by mu rather than
	// atomic because it is only ever touched while the lock is already held.
	dropped uint64
}

func newSpanRing(capacity int) *spanRing {
	if capacity < 1 {
		capacity = 1
	}
	return &spanRing{
		entries:  make([]Span, capacity),
		capacity: capacity,
	}
}

// push appends s, evicting the oldest span if the buffer is full.
//
// This is the only ring operation on the request path, so it does exactly one
// lock, one copy, and two integer updates — no allocation.
func (r *spanRing) push(s Span) {
	r.mu.Lock()
	if r.count < r.capacity {
		r.entries[(r.head+r.count)%r.capacity] = s
		r.count++
	} else {
		r.entries[r.head] = s
		r.head = (r.head + 1) % r.capacity
		r.dropped++
	}
	r.mu.Unlock()
}

// drain removes and returns up to max spans, oldest first.
//
// Returns nil when empty so the caller can skip an export round without an
// allocation. Cleared slots are zeroed: a Span holds maps and slices, and
// leaving stale references in the backing array would keep whole timelines and
// payloads alive long after they were exported.
func (r *spanRing) drain(max int) []Span {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.count == 0 {
		return nil
	}
	n := r.count
	if max > 0 && max < n {
		n = max
	}
	out := make([]Span, n)
	for i := 0; i < n; i++ {
		idx := (r.head + i) % r.capacity
		out[i] = r.entries[idx]
		r.entries[idx] = Span{}
	}
	r.head = (r.head + n) % r.capacity
	r.count -= n
	return out
}

// requeue puts spans back at the front after a failed export.
//
// Ordering is preserved (these are older than anything already buffered), and
// anything that no longer fits is dropped rather than displacing newer spans —
// the same drop-oldest rule as push, applied to a batch. Without this, a single
// aggregator restart would lose every span already in flight.
func (r *spanRing) requeue(spans []Span) {
	if len(spans) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	room := r.capacity - r.count
	if room <= 0 {
		r.dropped += uint64(len(spans))
		return
	}
	if len(spans) > room {
		// Keep the newest of the failed batch: they are the closest to
		// what an operator is looking at now.
		r.dropped += uint64(len(spans) - room)
		spans = spans[len(spans)-room:]
	}
	// Walk backwards so the earliest span ends up at the new head.
	for i := len(spans) - 1; i >= 0; i-- {
		r.head = (r.head - 1 + r.capacity) % r.capacity
		r.entries[r.head] = spans[i]
		r.count++
	}
}

func (r *spanRing) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count
}

func (r *spanRing) droppedCount() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dropped
}
