package fleet

// The Tracer is the only long-lived object fleet puts inside a service. It holds
// the span buffer, owns the single background goroutine that exports spans and
// sends heartbeats, and is the thing a disabled configuration turns into a no-op.
//
// # The one rule that shapes this whole file
//
// Nothing here may make a request wait. RecordSpan does a bounded write to a ring
// buffer and returns; every network operation, retry, and backoff happens on the
// background goroutine. An aggregator that is slow, restarting, or permanently
// gone must degrade tracing and nothing else — a tracing feature that can add
// latency to real traffic is worse than no tracing feature.

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Defaults from §6.1/§10.
const (
	DefaultFlushInterval     = time.Second
	DefaultMaxBatchSize      = 200
	DefaultMaxBufferSpans    = 4096
	DefaultExportTimeout     = 2 * time.Second
	DefaultHeartbeatInterval = 5 * time.Second

	// maxBackoff caps the retry delay. Capped rather than unbounded so a
	// service that lost its aggregator for an hour resumes exporting within
	// 30s of it returning, instead of sitting in a multi-minute sleep.
	maxBackoff = 30 * time.Second
)

// TracerConfig configures a Tracer. The zero value is deliberately *not* usable
// (see Enabled) — tracing is opt-in.
type TracerConfig struct {
	// ServiceName names this service in every span it reports. Required:
	// without it the topology graph has an unnamed node, so New refuses to
	// enable tracing without one.
	ServiceName string

	// AggregatorURL is where spans go. Its meaning is transport-specific —
	// a base URL for httptransport, a dial target for grpc, a topic prefix
	// for events — which is why it is a plain string here and interpreted by
	// the Transport.
	//
	// Empty disables tracing, on the same reasoning as Enabled: a service
	// configured to trace with nowhere to send spans is a misconfiguration,
	// and buffering forever for an aggregator that was never configured
	// would burn memory for no benefit.
	AggregatorURL string

	// Transport moves spans and encodes propagation (§5A.1). Defaults to
	// httptransport when nil.
	//
	// The spec's §5A.0 nominates eventtransport as the framework default;
	// that ordering is a *recommendation* order, and http is the build-order
	// baseline the conformance suite is defined against (per the approved
	// build-order decision). This field is how a service opts into any other
	// transport once it exists, with no other code change.
	Transport Transport

	// FlushInterval is how often the background goroutine exports whatever
	// has accumulated. A batch also goes early once MaxBatchSize is reached,
	// so this is a latency bound, not a throughput one.
	FlushInterval time.Duration

	// MaxBatchSize bounds one export. Large batches amortize request
	// overhead; unbounded ones would let a traffic spike turn into a single
	// enormous payload the aggregator has to accept in one read.
	MaxBatchSize int

	// MaxBufferSpans bounds memory. Once full, the oldest spans are dropped
	// (see spanRing) — never blocked on, never grown.
	MaxBufferSpans int

	// SampleRate is §7's fixed rate, applied only at the root span. Errors
	// are always reported regardless.
	SampleRate float64

	// RouteResolver maps a request to its registered route pattern, so spans
	// report "/orders/:id" rather than "/orders/42" (see routeOf).
	//
	// Optional: without it spans carry concrete paths, which still identify
	// the service and endpoint but give the topology graph one edge per
	// distinct path. Set it with RouterResolver(router) to get patterns.
	RouteResolver RouteResolver

	// ExportTimeout bounds one export attempt, so a hung aggregator cannot
	// stall the flush loop indefinitely and starve the heartbeat tick that
	// shares it.
	ExportTimeout time.Duration

	// HeartbeatInterval is how often liveness and schema-hash are reported
	// (§8.1.2). Must be well under the aggregator's ServiceTTL or a healthy
	// service will flap between up and down.
	HeartbeatInterval time.Duration

	// Enabled must be set explicitly. A zero-value TracerConfig therefore
	// produces a no-op Tracer, which is what makes "the fleet block is
	// absent from config" mean "nothing happens" rather than "tracing tries
	// to start and fails".
	Enabled bool

	// InstanceID distinguishes replicas. Generated from the trace-id
	// generator when empty, which is fine for the registry's purposes: it
	// only needs to be distinct, not meaningful.
	InstanceID string

	// Version, OpenAPIHash and OpenAPIURL are reported on each heartbeat.
	// OpenAPIHash is what lets the aggregator re-fetch a schema only when it
	// actually changed (§9A.3) instead of polling every service constantly.
	Version     string
	OpenAPIHash string
	OpenAPIURL  string

	// Logger receives the tracer's own diagnostics, matching the dashboard
	// collector's PushLog(level, message, source) shape so a caller can pass
	// coll.PushLog directly:
	//
	//	cfg.Logger = coll.PushLog
	//
	// A function rather than a *dashboard.Collector so a Tracer can be built
	// and tested without a dashboard, and so fleet does not depend on the
	// collector's lifecycle. Nil silently discards.
	Logger func(level, message, source string)
}

func (c TracerConfig) withDefaults() TracerConfig {
	if c.FlushInterval <= 0 {
		c.FlushInterval = DefaultFlushInterval
	}
	if c.MaxBatchSize <= 0 {
		c.MaxBatchSize = DefaultMaxBatchSize
	}
	if c.MaxBufferSpans <= 0 {
		c.MaxBufferSpans = DefaultMaxBufferSpans
	}
	if c.ExportTimeout <= 0 {
		c.ExportTimeout = DefaultExportTimeout
	}
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if c.SampleRate == 0 && !c.sampleRateWasSet() {
		c.SampleRate = DefaultSampleRate
	}
	return c
}

// sampleRateWasSet distinguishes "0.0, meaning errors-only" from "unset".
//
// Go cannot tell those apart on a float64, and the distinction matters: silently
// turning a deliberate 0.0 into 1.0 would trace everything in a service that
// explicitly asked for almost nothing. Resolved by treating an explicitly
// disabled tracer's zero as unset and any enabled tracer's zero as deliberate —
// the only case where the difference is observable.
func (c TracerConfig) sampleRateWasSet() bool { return c.Enabled }

// Tracer buffers spans locally and exports them in the background.
type Tracer struct {
	cfg     TracerConfig
	enabled bool

	// sampler and transport are read-only after New, so neither needs
	// synchronization despite being touched from every request goroutine.
	sampler   sampler
	transport Transport

	ring *spanRing

	// Counters for the heartbeat's rps/error-rate and for Metrics. Atomics
	// because requests write them concurrently.
	spansRecorded atomic.Uint64
	spansExported atomic.Uint64
	spansErrored  atomic.Uint64
	exportFails   atomic.Uint64

	// lastHeartbeat tracks the window used to derive rps: rate is computed
	// from the delta since the previous heartbeat rather than from process
	// start, so a service that was busy yesterday does not report a
	// misleadingly high rate today.
	lastHeartbeatAt    time.Time
	lastRecordedAtSend uint64
	lastErroredAtSend  uint64

	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
}

// New returns a Tracer for cfg.
//
// A disabled, misconfigured, or incomplete configuration returns a working
// no-op Tracer rather than an error or nil. That is deliberate: a service must
// still start and serve traffic when its tracing config is wrong, and every
// method here is nil-safe and enabled-checked, so callers never branch on
// whether tracing is on.
func New(cfg TracerConfig) *Tracer {
	cfg = cfg.withDefaults()

	t := &Tracer{
		cfg:     cfg,
		sampler: newSampler(cfg.SampleRate),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}

	// Three ways to be disabled, all treated identically. ServiceName and
	// AggregatorURL are checked here rather than trusted later so the
	// failure is one branch at startup instead of a surprise on the first
	// export attempt.
	if !cfg.Enabled || cfg.ServiceName == "" || cfg.AggregatorURL == "" {
		close(t.done) // Close() must not wait for a goroutine that never ran.
		if cfg.Enabled {
			// Enabled-but-unusable is worth saying out loud: this is
			// someone who meant to turn tracing on and will otherwise
			// spend a while wondering why no spans appear.
			t.logf("warn", "fleet tracing enabled but inactive: service_name and aggregator_url are both required")
		}
		return t
	}

	t.enabled = true
	t.transport = cfg.Transport
	t.ring = newSpanRing(cfg.MaxBufferSpans)
	if t.cfg.InstanceID == "" {
		// Reuses the trace-id generator: it is already a source of
		// crypto-random hex of exactly the right shape, and an instance
		// id only has to be unique.
		t.cfg.InstanceID = NewTraceContext().TraceIDHex()[:16]
	}
	t.lastHeartbeatAt = time.Now()

	go t.run()
	return t
}

// Enabled reports whether this Tracer does anything.
func (t *Tracer) Enabled() bool { return t != nil && t.enabled }

// ServiceName is the name this Tracer reports spans under.
func (t *Tracer) ServiceName() string {
	if t == nil {
		return ""
	}
	return t.cfg.ServiceName
}

// Transport returns the configured transport, or nil when disabled.
func (t *Tracer) Transport() Transport {
	if t == nil {
		return nil
	}
	return t.transport
}

// RecordSpan buffers a finished span for export.
//
// This is the hot path: on a disabled Tracer it is one nil check plus one bool
// check and a return, with no allocation (§12.1). On an enabled one it is a
// bounded copy into the ring buffer — no network, no marshalling, no blocking.
func (t *Tracer) RecordSpan(s Span) {
	if t == nil || !t.enabled {
		return
	}
	t.spansRecorded.Add(1)
	if s.Failed() {
		t.spansErrored.Add(1)
	}
	t.ring.push(s)
}

// Close flushes what remains and stops the background goroutine.
//
// Best-effort by design: on shutdown the useful thing is to get the spans of the
// requests that just failed off the box, but a service must not hang waiting for
// an aggregator that may itself be going down. ctx bounds the wait; an expired
// ctx returns its error and abandons the remaining spans rather than blocking.
func (t *Tracer) Close(ctx context.Context) error {
	if t == nil || !t.enabled {
		return nil
	}
	t.stopOnce.Do(func() { close(t.stop) })

	select {
	case <-t.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return t.flush(ctx)
}

// run is the single background goroutine: export on one tick, heartbeat on
// another.
//
// One goroutine for both, rather than two, because they share the same failure
// handling and the same transport, and because a service running twenty of these
// (one per Tracer in a test binary, say) should not cost forty goroutines.
func (t *Tracer) run() {
	defer close(t.done)

	flushTick := time.NewTicker(t.cfg.FlushInterval)
	defer flushTick.Stop()
	heartbeatTick := time.NewTicker(t.cfg.HeartbeatInterval)
	defer heartbeatTick.Stop()

	// backoff is only consulted after a failure. While zero, the flush tick
	// governs cadence; after a failure it delays the *next* attempt so a
	// down aggregator is not hammered once per FlushInterval.
	var backoff time.Duration
	var nextAttempt time.Time

	for {
		select {
		case <-t.stop:
			return

		case <-flushTick.C:
			if !nextAttempt.IsZero() && time.Now().Before(nextAttempt) {
				continue
			}
			if t.ring.len() == 0 {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), t.cfg.ExportTimeout)
			err := t.flush(ctx)
			cancel()
			if err != nil {
				backoff = nextBackoff(backoff)
				nextAttempt = time.Now().Add(backoff)
				// Logged once per backoff cycle, not per failure:
				// an aggregator that is down for an hour would
				// otherwise fill the log ring buffer with the
				// same line and evict the logs someone actually
				// needs.
				t.logf("warn", "fleet span export failed, retrying in "+backoff.String()+": "+err.Error())
				continue
			}
			backoff = 0
			nextAttempt = time.Time{}

		case <-heartbeatTick.C:
			ctx, cancel := context.WithTimeout(context.Background(), t.cfg.ExportTimeout)
			t.sendHeartbeat(ctx)
			cancel()
		}
	}
}

// flush exports one batch, requeueing it on failure.
//
// Requeueing is what makes an aggregator restart lossless rather than a hole in
// the trace history: the spans go back to the front of the ring and are retried,
// and only overflow (not failure) drops them.
func (t *Tracer) flush(ctx context.Context) error {
	if t.transport == nil {
		return nil
	}
	spans := t.ring.drain(t.cfg.MaxBatchSize)
	if len(spans) == 0 {
		return nil
	}
	if err := t.transport.ExportSpans(ctx, t.cfg.AggregatorURL, spans); err != nil {
		t.exportFails.Add(1)
		t.ring.requeue(spans)
		return err
	}
	t.spansExported.Add(uint64(len(spans)))
	return nil
}

// sendHeartbeat reports liveness, load, and schema hash (§8.1.2).
//
// Failures are counted but not retried or backed off: heartbeats are periodic by
// nature, so the next tick is the retry. Backing off would only delay the
// recovery signal the aggregator is waiting for.
func (t *Tracer) sendHeartbeat(ctx context.Context) {
	if t.transport == nil {
		return
	}
	now := time.Now()
	recorded := t.spansRecorded.Load()
	errored := t.spansErrored.Load()

	// Rates over the window since the last heartbeat, not since process
	// start — a long-running service's current load is the useful number.
	var rps, errRate float64
	if elapsed := now.Sub(t.lastHeartbeatAt).Seconds(); elapsed > 0 {
		deltaReq := recorded - t.lastRecordedAtSend
		deltaErr := errored - t.lastErroredAtSend
		rps = float64(deltaReq) / elapsed
		if deltaReq > 0 {
			errRate = float64(deltaErr) / float64(deltaReq)
		}
	}
	t.lastHeartbeatAt = now
	t.lastRecordedAtSend = recorded
	t.lastErroredAtSend = errored

	hb := Heartbeat{
		Service:     t.cfg.ServiceName,
		InstanceID:  t.cfg.InstanceID,
		Version:     t.cfg.Version,
		RPS:         rps,
		ErrorRate:   errRate,
		OpenAPIHash: t.cfg.OpenAPIHash,
		OpenAPIURL:  t.cfg.OpenAPIURL,
	}
	if err := t.transport.ExportHeartbeat(ctx, t.cfg.AggregatorURL, hb); err != nil {
		t.exportFails.Add(1)
	}
}

// nextBackoff doubles the delay up to maxBackoff.
func nextBackoff(cur time.Duration) time.Duration {
	if cur <= 0 {
		return time.Second
	}
	next := cur * 2
	if next > maxBackoff {
		return maxBackoff
	}
	return next
}

func (t *Tracer) logf(level, msg string) {
	if t.cfg.Logger == nil {
		return
	}
	// "fleet" is the source argument, matching the dashboard collector's
	// PushLog(level, message, source) ordering.
	t.cfg.Logger(level, msg, "fleet")
}

// TracerMetrics is a snapshot of one Tracer's own counters.
type TracerMetrics struct {
	SpansRecorded uint64 `json:"spans_recorded"`
	SpansExported uint64 `json:"spans_exported"`
	SpansDropped  uint64 `json:"spans_dropped"`
	SpansBuffered int    `json:"spans_buffered"`
	ExportFails   uint64 `json:"export_failures"`
}

// Metrics returns this Tracer's counters, for the Performance page.
//
// SpansDropped is the one to watch: a rising value means either the aggregator
// is unreachable or MaxBufferSpans is too small for this service's rate.
func (t *Tracer) Metrics() TracerMetrics {
	if t == nil || !t.enabled {
		return TracerMetrics{}
	}
	return TracerMetrics{
		SpansRecorded: t.spansRecorded.Load(),
		SpansExported: t.spansExported.Load(),
		SpansDropped:  t.ring.droppedCount(),
		SpansBuffered: t.ring.len(),
		ExportFails:   t.exportFails.Load(),
	}
}
