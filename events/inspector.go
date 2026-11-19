package events

import "sort"

// ListenerInfo describes one registered listener for the inspector.
type ListenerInfo struct {
	// ID is the listener's unique identifier.
	ID uint64
	// Name is the display name, derived from the handler function.
	Name string
	// Priority is the execution priority.
	Priority int
	// Phase is "before", "normal", or "after".
	Phase string
	// Order is the zero-based position in the execution sequence.
	Order int
	// Once reports whether the listener is a once-listener.
	Once bool
	// Fired reports whether a once-listener has already run. Always
	// false for ordinary listeners.
	Fired bool
	// Filtered reports whether the listener has a filter installed.
	Filtered bool
	// Calls is the number of times the listener has been invoked,
	// excluding skipped invocations.
	Calls uint64
}

// EventInfo describes one event type for the inspector.
type EventInfo struct {
	// Name is the display name.
	Name string
	// ListenerCount is the number of registered listeners.
	ListenerCount int
	// Listeners is the sorted list of listeners in execution order.
	Listeners []ListenerInfo
	// Metrics is the accumulated dispatch statistics.
	Metrics Metrics
}

// Inspect returns detailed information about T's listeners and metrics.
func Inspect[T any](b *Bus) EventInfo {
	e := b.lookup(typeKey[T]())
	if e == nil {
		return EventInfo{
			Name:    typeKey[T]().String(),
			Metrics: Metrics{},
		}
	}

	return EventInfo{
		Name:          e.displayName(),
		ListenerCount: e.erased.len(),
		Listeners:     e.erased.describe(),
		Metrics:       e.stats.snapshot(),
	}
}

// InspectAll returns information for every registered event type, sorted
// by display name.
func (b *Bus) InspectAll() []EventInfo {
	var out []EventInfo
	b.entries.Range(func(_, v any) bool {
		e := v.(*entry)
		out = append(out, EventInfo{
			Name:          e.displayName(),
			ListenerCount: e.erased.len(),
			Listeners:     e.erased.describe(),
			Metrics:       e.stats.snapshot(),
		})
		return true
	})
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out
}

// MetricsFor returns the dispatch statistics for T.
func MetricsFor[T any](b *Bus) Metrics {
	if e := b.lookup(typeKey[T]()); e != nil {
		return e.stats.snapshot()
	}
	return Metrics{}
}

// TotalMetrics returns the aggregated statistics across all event types.
func (b *Bus) TotalMetrics() Metrics {
	var total Metrics
	b.entries.Range(func(_, v any) bool {
		total.add(v.(*entry).stats.snapshot())
		return true
	})
	return total
}

// EnableRecorder turns the event recorder on. It captures dispatch
// history for debugging and dashboard display. Payloads are not retained;
// use [Bus.EnableRecorderWithPayload] to capture them.
func (b *Bus) EnableRecorder() {
	b.recorder.enable(false)
}

// EnableRecorderWithPayload turns the recorder on and retains event
// values. Payload capture extends the lifetime of every recorded event by
// up to the recorder's capacity, so enable it only when you need to
// inspect the values.
func (b *Bus) EnableRecorderWithPayload() {
	b.recorder.enable(true)
}

// DisableRecorder turns the recorder off. Existing history is retained.
func (b *Bus) DisableRecorder() {
	b.recorder.disable()
}

// RecorderEnabled reports whether recording is on.
func (b *Bus) RecorderEnabled() bool {
	return b.recorder.isEnabled()
}

// RecorderHistory returns a copy of the recorded dispatches, oldest first.
func (b *Bus) RecorderHistory() []Record {
	return b.recorder.history()
}

// ClearRecorderHistory discards the recorded history.
func (b *Bus) ClearRecorderHistory() {
	b.recorder.clear()
}

// RecorderStats returns the recorder's occupancy.
func (b *Bus) RecorderStats() RecorderStats {
	return b.recorder.stats()
}

// PoolStats returns the async worker pool's statistics. It returns a
// zero value when the pool has not been started.
func (b *Bus) PoolStats() PoolStats {
	p := b.pool.Load()
	if p == nil {
		return PoolStats{}
	}
	return p.stats()
}
