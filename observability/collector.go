package observability

import (
	"sync"
	"sync/atomic"
)

// Config configures a Collector.
type Config struct {
	// Capacity is the number of Signals retained in the ring buffer.
	// Zero selects the default of 1000. The buffer is bounded by design:
	// a runaway subsystem can flood the dashboard, but it cannot grow the
	// process heap without limit.
	Capacity int

	// Metrics enables per-name statistics. This costs a small map update
	// per signal; disable it for maximum throughput when the dashboard is
	// the only consumer and it computes its own aggregates.
	Metrics bool

	// ErrSink receives failures produced by the observability layer
	// itself, so a bug in an observer can never take down the emitter.
	// When nil, internal errors are dropped.
	ErrSink func(error)
}

// Collector is the hub of the observability layer.
//
// Producers emit Signals through it; the dashboard reads them back
// through Snapshot, Stream, Metrics and Stats. It owns no goroutines —
// nothing is spawned by construction — and every method is safe for
// concurrent use.
type Collector struct {
	cfg Config

	seq atomic.Uint64

	ring *ringBuffer[Signal]

	// mu guards metrics and total. They are updated on the signal hot
	// path but the critical section is a few increments on one small map,
	// which keeps contention negligible.
	mu      sync.Mutex
	metrics map[string]*metric
	total   Stats

	// listener indexes Signals by (Source, Name) for graph traversal and
	// lookup, and tracks relationships so the Events page can answer
	// "what runs, and what runs inside it?".
	idxMu       sync.RWMutex
	index       map[string]*indexedNode
	childCounts map[uint64]int // ParentID -> observed children

	// stream wakes subscribers when a Signal is pushed.
	stream *signalStream

	closed atomic.Bool
}

// NewCollector builds a Collector with the given configuration, applying
// defaults for zero fields.
func NewCollector(cfg Config) *Collector {
	if cfg.Capacity <= 0 {
		cfg.Capacity = 1000
	}
	c := &Collector{
		cfg:         cfg,
		ring:        newRingBuffer[Signal](cfg.Capacity),
		metrics:     make(map[string]*metric),
		index:       make(map[string]*indexedNode),
		childCounts: make(map[uint64]int),
		stream:      newSignalStream(),
	}
	return c
}

// defaultCollector is the package-level collector used by the top-level
// functions. Creating it lazily keeps the zero import cost at zero.
var defaultCollector struct {
	once sync.Once
	c    *Collector
}

// Default returns the process-wide collector. Applications that want an
// isolated observability domain can construct their own.
func Default() *Collector {
	defaultCollector.once.Do(func() {
		defaultCollector.c = NewCollector(Config{})
	})
	return defaultCollector.c
}

// Publish records one completed signal. It is the only entry point
// producers need, and it never blocks: subscribers are handed a copy on
// their own goroutine.
func (c *Collector) Publish(s Signal) {
	if c.closed.Load() {
		return
	}
	s.ID = c.seq.Add(1)

	if c.cfg.Metrics {
		c.observe(s)
	}

	c.ring.Push(s)

	c.indexSignal(s)

	c.stream.publish(c.cloneForSubscriber(s))
}

// cloneForSubscriber copies the parts of s that could be mutated by a
// slow subscriber. The slice and map are copied shallowly — a subscriber
// that mutates element contents misbehaves only for itself.
func (c *Collector) cloneForSubscriber(s Signal) Signal {
	out := s
	if len(s.Spans) > 0 {
		out.Spans = append([]Span(nil), s.Spans...)
	}
	if len(s.Attrs) > 0 {
		out.Attrs = make(map[string]string, len(s.Attrs))
		for k, v := range s.Attrs {
			out.Attrs[k] = v
		}
	}
	return out
}

// observe folds a signal into the per-name statistics and the global
// totals.
//
// Metrics are keyed by (Source, Name), not by Name alone: two subsystems
// are free to publish the same name — a route and an event may both be
// called "user.created" — and their statistics must not merge.
func (c *Collector) observe(s Signal) {
	key := indexKey(s.Source, s.Name)

	c.mu.Lock()
	m := c.metrics[key]
	if m == nil {
		m = newMetric(s.Name, s.Source)
		c.metrics[key] = m
	}

	m.observe(s)
	c.total.Signals++
	if s.Failed {
		c.total.Failed++
	}
	if s.Cancelled {
		c.total.Cancelled++
	}
	if s.Async {
		c.total.Async++
	}
	c.total.Children += uint64(s.Children)
	c.total.Executed += uint64(s.Executed)
	c.mu.Unlock()
}

// indexSignal registers the signal's name and its parent/child link.
func (c *Collector) indexSignal(s Signal) {
	key := indexKey(s.Source, s.Name)

	c.idxMu.Lock()
	n := c.index[key]
	if n == nil {
		n = &indexedNode{Source: s.Source, Name: s.Name}
		c.index[key] = n
	}
	n.observe(s)
	if s.ParentID != 0 {
		c.childCounts[s.ParentID]++
	}
	c.idxMu.Unlock()
}

// Close stops the collector. Subscribers stop receiving signals, and
// further Publish calls are no-ops.
func (c *Collector) Close() {
	if !c.closed.CompareAndSwap(false, true) {
		return
	}
	c.stream.close()
}

// Config returns the effective configuration.
func (c *Collector) Config() Config { return c.cfg }

// ─── Reader API ──────────────────────────────────────────────────────────

// Snapshot returns a copy of the retained signals, oldest first.
func (c *Collector) Snapshot() []Signal { return c.ring.Snapshot() }

// Len returns the number of retained signals.
func (c *Collector) Len() int { return c.ring.Len() }

// Metrics returns a snapshot of the per-name statistics.
func (c *Collector) Metrics() []Metric { return c.metricsSnapshot() }

// MetricFor returns the statistics for one signal name, or nil.
//
// When several subsystems publish the same name, the first match wins and
// the result is ambiguous; use [Collector.MetricForSource] to disambiguate.
func (c *Collector) MetricFor(name string) *Metric {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.metrics {
		if m.Name == name {
			cp := *m
			return &cp
		}
	}
	return nil
}

// MetricForSource returns the statistics for one name within one
// subsystem, or nil. This is the unambiguous lookup: names are only
// guaranteed unique within a Source.
func (c *Collector) MetricForSource(src Source, name string) *Metric {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.metrics[indexKey(src, name)]
	if m == nil {
		return nil
	}
	cp := *m
	return &cp
}

// Stats returns the aggregate counters.
func (c *Collector) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}

// Stream returns a live stream of signals published after the call.
//
// The returned channel is closed when the collector is closed or when
// Unsubscribe is called.
func (c *Collector) Stream() (ch <-chan Signal, unsubscribe func()) {
	return c.stream.subscribe()
}

// ─── Internal helpers ────────────────────────────────────────────────────

func (c *Collector) metricsSnapshot() []Metric {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Metric, 0, len(c.metrics))
	for _, m := range c.metrics {
		cp := *m
		out = append(out, cp)
	}
	return out
}

// report delivers an internal error to the configured sink, if any.
func (c *Collector) report(err error) {
	if c.cfg.ErrSink != nil {
		c.cfg.ErrSink(err)
	}
}
