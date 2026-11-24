package dashboard

import "sync"

// ringBuffer is a fixed-capacity, thread-safe ring buffer for ordered events.
//
// All collectors (requests, queries, logs, timelines) share the same shape:
// append-only, with old entries evicted once capacity is reached, and a
// snapshot operation that returns a copy of the contents in insertion order.
//
// The implementation uses a mutex rather than a lock-free scheme because
// collectors are typically read on the WebSocket broadcast tick (a few times
// per second) and written on the request hot path — the contention is low
// and a mutex keeps the code trivially correct.
type ringBuffer[T any] struct {
	mu       sync.Mutex
	entries  []T
	head     int // index of the oldest entry
	count    int // number of valid entries
	capacity int
}

func newRingBuffer[T any](capacity int) *ringBuffer[T] {
	if capacity < 1 {
		capacity = 1
	}
	return &ringBuffer[T]{
		entries:  make([]T, capacity),
		capacity: capacity,
	}
}

// Push appends v, evicting the oldest entry if the buffer is full.
func (r *ringBuffer[T]) Push(v T) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.count < r.capacity {
		idx := (r.head + r.count) % r.capacity
		r.entries[idx] = v
		r.count++
	} else {
		r.entries[r.head] = v
		r.head = (r.head + 1) % r.capacity
	}
}

// Snapshot returns a copy of all entries in insertion order (oldest first).
func (r *ringBuffer[T]) Snapshot() []T {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]T, r.count)
	for i := 0; i < r.count; i++ {
		out[i] = r.entries[(r.head+i)%r.capacity]
	}
	return out
}

// Last returns a copy of the newest n entries in insertion order (all of them when n is
// not positive or exceeds what is held). Taking the tail of a Snapshot copied the whole
// buffer to keep a few entries.
func (r *ringBuffer[T]) Last(n int) []T {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n <= 0 || n > r.count {
		n = r.count
	}
	out := make([]T, n)
	for i := 0; i < n; i++ {
		out[i] = r.entries[(r.head+r.count-n+i)%r.capacity]
	}
	return out
}

// Find returns the newest entry for which match is true, without copying the buffer.
func (r *ringBuffer[T]) Find(match func(*T) bool) (T, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := r.count - 1; i >= 0; i-- {
		e := &r.entries[(r.head+i)%r.capacity]
		if match(e) {
			return *e, true
		}
	}
	var zero T
	return zero, false
}

// Len returns the current number of entries.
func (r *ringBuffer[T]) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count
}

// Clear removes all entries.
func (r *ringBuffer[T]) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Zeroed, not just forgotten: a record holds maps and slices, and the buffer would
	// keep all of them alive until each slot happened to be overwritten.
	var zero T
	for i := range r.entries {
		r.entries[i] = zero
	}
	r.head = 0
	r.count = 0
}
