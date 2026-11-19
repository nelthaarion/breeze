package events

import (
	"sync/atomic"
	"time"
)

// eventStats accumulates counters for one event type.
//
// Every field is atomic so the dispatch path can update statistics
// without taking a lock and the dashboard can read them without blocking
// a dispatch. The cost on the hot path is a handful of atomic adds, which
// is why [Config.DisableMetrics] exists for callers who want none of it.
type eventStats struct {
	dispatches atomic.Uint64
	listeners  atomic.Uint64
	failures   atomic.Uint64
	panics     atomic.Uint64
	stopped    atomic.Uint64
	filtered   atomic.Uint64

	// totalNanos is the summed dispatch duration. Combined with
	// dispatches it yields the mean without storing a history.
	totalNanos atomic.Uint64

	// minNanos and maxNanos bracket the observed durations. minNanos holds
	// the minimum plus one, so that zero can mean "no sample yet" without
	// also swallowing a genuine zero-length sample: on a coarse clock a fast
	// dispatch measures 0, and storing it as 0 made the next sample look like
	// the first.
	minNanos atomic.Uint64
	maxNanos atomic.Uint64

	// lastUnixNano is the start time of the most recent dispatch.
	lastUnixNano atomic.Int64
}

// observe records one completed dispatch.
func (s *eventStats) observe(d time.Duration, start time.Time) {
	if d < 0 {
		d = 0
	}
	n := uint64(d)
	s.dispatches.Add(1)
	s.totalNanos.Add(n)
	s.lastUnixNano.Store(start.UnixNano())

	// min/max are read-modify-write, so they need a CAS loop. It retries
	// only when a concurrent dispatch moved the bound first, which is
	// rare and always makes progress.
	for {
		cur := s.maxNanos.Load()
		if n <= cur || s.maxNanos.CompareAndSwap(cur, n) {
			break
		}
	}
	for {
		cur := s.minNanos.Load() // minimum+1; 0 means unset
		if cur != 0 && n+1 >= cur {
			break
		}
		if s.minNanos.CompareAndSwap(cur, n+1) {
			break
		}
	}
}

// snapshot converts the live counters into an immutable view.
func (s *eventStats) snapshot() Metrics {
	dispatches := s.dispatches.Load()
	total := time.Duration(s.totalNanos.Load())
	m := Metrics{
		Dispatches:    dispatches,
		Listeners:     s.listeners.Load(),
		Failures:      s.failures.Load(),
		Panics:        s.panics.Load(),
		Stopped:       s.stopped.Load(),
		Filtered:      s.filtered.Load(),
		TotalDuration: total,
		MaxDuration:   time.Duration(s.maxNanos.Load()),
	}
	if mn := s.minNanos.Load(); mn != 0 {
		m.MinDuration = time.Duration(mn - 1)
	}
	if dispatches > 0 {
		m.AvgDuration = total / time.Duration(dispatches)
	}
	if ns := s.lastUnixNano.Load(); ns > 0 {
		m.LastDispatch = time.Unix(0, ns)
	}
	return m
}

// Metrics is a point-in-time statistics snapshot for one event type, or
// for the whole bus when returned by [Bus.TotalMetrics].
type Metrics struct {
	// Dispatches is the number of completed Emit calls.
	Dispatches uint64
	// Listeners is the number of listener invocations, summed across
	// dispatches. Filtered and spent once-listeners are not counted.
	Listeners uint64
	// Failures is the number of listener errors, excluding [Stop].
	Failures uint64
	// Panics is the number of recovered panics.
	Panics uint64
	// Stopped is the number of dispatches halted early by [Stop],
	// [Context.Cancel] or, unless [Config.ContinueOnError] is set, a listener
	// error. For an async dispatch only a listener returning [Stop] is
	// counted, once per dispatch however many listeners return it: whether
	// the dispatch was cancelled is not settled when scheduling finishes.
	Stopped uint64
	// Filtered is the number of listener invocations skipped by a
	// [Filter].
	Filtered uint64
	// TotalDuration is the summed wall-clock time of all dispatches.
	TotalDuration time.Duration
	// AvgDuration is TotalDuration divided by Dispatches.
	AvgDuration time.Duration
	// MinDuration is the fastest observed dispatch.
	MinDuration time.Duration
	// MaxDuration is the slowest observed dispatch.
	MaxDuration time.Duration
	// LastDispatch is when the most recent dispatch started. It is the
	// zero Time if the event has never been dispatched.
	LastDispatch time.Time
}

// add merges other into m. It is used to total per-event metrics across
// the bus: counters sum and the duration bounds widen to cover both.
//
// An event that has never dispatched contributes nothing to the duration
// bounds, and a bound of zero is a real value, not "unset": whether a bound
// means anything is decided by Dispatches.
func (m *Metrics) add(other Metrics) {
	if other.Dispatches > 0 {
		if m.Dispatches == 0 {
			m.MinDuration, m.MaxDuration = other.MinDuration, other.MaxDuration
		} else {
			m.MinDuration = min(m.MinDuration, other.MinDuration)
			m.MaxDuration = max(m.MaxDuration, other.MaxDuration)
		}
	}
	m.Dispatches += other.Dispatches
	m.Listeners += other.Listeners
	m.Failures += other.Failures
	m.Panics += other.Panics
	m.Stopped += other.Stopped
	m.Filtered += other.Filtered
	m.TotalDuration += other.TotalDuration

	if other.LastDispatch.After(m.LastDispatch) {
		m.LastDispatch = other.LastDispatch
	}
	if m.Dispatches > 0 {
		m.AvgDuration = m.TotalDuration / time.Duration(m.Dispatches)
	}
}
