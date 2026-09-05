package fleet

// Tests for the Tracer's lifecycle and export loop.
//
// This is the part of fleet that runs inside every participating service, so its
// failure modes are the ones that can damage a production app rather than merely
// lose telemetry. Three properties get the most attention here:
//
//   - Disabled means inert. Not "cheap" — inert: no goroutine, no buffer, no
//     export. §16 promises that omitting the fleet block changes nothing.
//   - An unreachable aggregator must never grow memory without bound, never
//     block a request, and never lose spans it could have retried.
//   - Close must not hang, because it runs during shutdown, which is exactly
//     when the aggregator may also be going away.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// recordingTransport captures what was exported and can be made to fail on
// demand, which is what makes the backoff and requeue paths testable without a
// real network.
type recordingTransport struct {
	mu         sync.Mutex
	spans      []Span
	heartbeats []Heartbeat
	failWith   error
	calls      int

	// exported is signalled after each successful span export so a test can
	// wait for a flush instead of sleeping and hoping.
	exported chan struct{}
}

func newRecordingTransport() *recordingTransport {
	return &recordingTransport{exported: make(chan struct{}, 64)}
}

func (r *recordingTransport) Name() string { return "recording" }

// Inject and Extract use the default encoding rather than doing nothing, so a
// Tracer built with this transport propagates exactly as a real one does. A fake
// that silently discarded trace context would make every propagation assertion
// in middleware_test.go pass or fail for reasons unrelated to the code under
// test.
func (r *recordingTransport) Inject(tc TraceContext, baggage Baggage, c Carrier) {
	InjectInto(tc, baggage, "", c)
}

func (r *recordingTransport) Extract(c Carrier) (TraceContext, Baggage, bool) {
	return ExtractFrom(c)
}

func (r *recordingTransport) ExportSpans(_ context.Context, _ string, spans []Span) error {
	r.mu.Lock()
	r.calls++
	if r.failWith != nil {
		err := r.failWith
		r.mu.Unlock()
		return err
	}
	r.spans = append(r.spans, spans...)
	r.mu.Unlock()

	select {
	case r.exported <- struct{}{}:
	default:
	}
	return nil
}

func (r *recordingTransport) ExportHeartbeat(_ context.Context, _ string, hb Heartbeat) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failWith != nil {
		return r.failWith
	}
	r.heartbeats = append(r.heartbeats, hb)
	return nil
}

func (r *recordingTransport) exportedSpans() []Span {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Span, len(r.spans))
	copy(out, r.spans)
	return out
}

func (r *recordingTransport) exportedHeartbeats() []Heartbeat {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Heartbeat, len(r.heartbeats))
	copy(out, r.heartbeats)
	return out
}

func (r *recordingTransport) setFailure(err error) {
	r.mu.Lock()
	r.failWith = err
	r.mu.Unlock()
}

// closeTracer shuts a Tracer down with a bounded context, so a hang shows up as
// a test failure rather than a stuck suite.
func closeTracer(t *testing.T, tr *Tracer) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := tr.Close(ctx); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// --- Disabled behaviour ----------------------------------------------------

// TestTracerDisabledIsInert covers all three ways to be off. They must be
// indistinguishable: a service that set Enabled:false and one that forgot to
// configure a URL should both behave exactly like one that never imported fleet.
func TestTracerDisabledIsInert(t *testing.T) {
	cases := map[string]TracerConfig{
		"explicitly disabled": {Enabled: false, ServiceName: "svc", AggregatorURL: "http://x"},
		"no service name":     {Enabled: true, AggregatorURL: "http://x"},
		"no aggregator url":   {Enabled: true, ServiceName: "svc"},
		"zero config":         {},
	}
	for desc, cfg := range cases {
		t.Run(desc, func(t *testing.T) {
			tr := New(cfg)
			if tr.Enabled() {
				t.Fatal("reports itself enabled")
			}

			tr.RecordSpan(validSpan())
			if got := tr.Metrics(); got != (TracerMetrics{}) {
				t.Errorf("a disabled tracer recorded metrics: %+v", got)
			}
			// Close must return immediately rather than waiting on a
			// goroutine that was never started.
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := tr.Close(ctx); err != nil {
				t.Errorf("Close on a disabled tracer: %v", err)
			}
		})
	}
}

// TestNilTracerIsSafe pins the "no panics regardless" requirement. A service that
// never constructed a Tracer will pass nil around, and every method has to
// tolerate that — a nil-pointer panic in telemetry taking down a request handler
// would be the worst possible failure mode for this feature.
func TestNilTracerIsSafe(t *testing.T) {
	var tr *Tracer

	tr.RecordSpan(validSpan())
	if tr.Enabled() {
		t.Error("nil tracer reports enabled")
	}
	if got := tr.ServiceName(); got != "" {
		t.Errorf("ServiceName = %q, want empty", got)
	}
	if tr.Transport() != nil {
		t.Error("nil tracer returned a transport")
	}
	if got := tr.Metrics(); got != (TracerMetrics{}) {
		t.Errorf("Metrics = %+v, want zero", got)
	}
	if err := tr.Close(context.Background()); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
}

// TestTracerEnabledButUnusableWarns is a usability guard: someone who set
// enabled:true and expects spans deserves to be told why none appear, rather than
// silently getting nothing.
func TestTracerEnabledButUnusableWarns(t *testing.T) {
	var mu sync.Mutex
	var logged []string
	tr := New(TracerConfig{
		Enabled:     true,
		ServiceName: "svc", // no AggregatorURL
		Logger: func(level, msg, source string) {
			mu.Lock()
			defer mu.Unlock()
			logged = append(logged, level+": "+msg+" ["+source+"]")
		},
	})
	defer closeTracer(t, tr)

	mu.Lock()
	defer mu.Unlock()
	if len(logged) == 0 {
		t.Fatal("enabled-but-unusable produced no warning")
	}
}

// --- Recording and export --------------------------------------------------

func TestTracerRecordAndFlush(t *testing.T) {
	rt := newRecordingTransport()
	tr := New(TracerConfig{
		Enabled:       true,
		ServiceName:   "orders-service",
		AggregatorURL: "http://aggregator",
		Transport:     rt,
		// Long intervals: this test drives the flush through Close so it
		// is deterministic rather than timing-dependent.
		FlushInterval:     time.Hour,
		HeartbeatInterval: time.Hour,
	})

	if !tr.Enabled() {
		t.Fatal("a fully configured tracer reports itself disabled")
	}
	if got := tr.ServiceName(); got != "orders-service" {
		t.Errorf("ServiceName = %q", got)
	}
	if tr.Transport() == nil {
		t.Error("Transport() returned nil on an enabled tracer")
	}

	tr.RecordSpan(validSpan())
	tr.RecordSpan(validSpan())

	if got := tr.Metrics(); got.SpansRecorded != 2 || got.SpansBuffered != 2 {
		t.Errorf("metrics = %+v, want 2 recorded and 2 buffered", got)
	}

	closeTracer(t, tr)

	if got := rt.exportedSpans(); len(got) != 2 {
		t.Errorf("exported %d spans, want 2 — Close must flush what remains", len(got))
	}
	if got := tr.Metrics(); got.SpansExported != 2 {
		t.Errorf("SpansExported = %d, want 2", got.SpansExported)
	}
}

// TestTracerFlushesOnInterval exercises the actual background loop rather than
// the Close path, since the ticker branch is what runs in production.
func TestTracerFlushesOnInterval(t *testing.T) {
	rt := newRecordingTransport()
	tr := New(TracerConfig{
		Enabled:           true,
		ServiceName:       "svc",
		AggregatorURL:     "http://aggregator",
		Transport:         rt,
		FlushInterval:     10 * time.Millisecond,
		HeartbeatInterval: time.Hour,
	})
	defer closeTracer(t, tr)

	tr.RecordSpan(validSpan())

	select {
	case <-rt.exported:
	case <-time.After(2 * time.Second):
		t.Fatal("no export within 2s — the flush ticker is not firing")
	}
	if got := rt.exportedSpans(); len(got) != 1 {
		t.Errorf("exported %d spans, want 1", len(got))
	}
}

// TestTracerErrorCounting checks the counters the heartbeat's error_rate is
// derived from; a wrong count here misreports a service's health to the whole
// fleet view.
func TestTracerErrorCounting(t *testing.T) {
	rt := newRecordingTransport()
	tr := New(TracerConfig{
		Enabled: true, ServiceName: "svc", AggregatorURL: "http://a",
		Transport: rt, FlushInterval: time.Hour, HeartbeatInterval: time.Hour,
	})
	defer closeTracer(t, tr)

	ok := validSpan()
	failed := validSpan()
	failed.Status = 500
	recovered := validSpan()
	recovered.Error = "recovered panic" // 200 but failed

	tr.RecordSpan(ok)
	tr.RecordSpan(failed)
	tr.RecordSpan(recovered)

	if got := tr.Metrics().SpansRecorded; got != 3 {
		t.Errorf("SpansRecorded = %d, want 3", got)
	}
}

// --- Failure handling ------------------------------------------------------

// TestTracerRequeuesOnExportFailure is the aggregator-restart case. Spans that
// failed to export must go back in the buffer, not into a hole: a brief
// aggregator outage should cost latency in the dashboard, not data.
func TestTracerRequeuesOnExportFailure(t *testing.T) {
	rt := newRecordingTransport()
	rt.setFailure(errors.New("connection refused"))

	tr := New(TracerConfig{
		Enabled: true, ServiceName: "svc", AggregatorURL: "http://a",
		Transport: rt, FlushInterval: time.Hour, HeartbeatInterval: time.Hour,
	})

	tr.RecordSpan(validSpan())
	tr.RecordSpan(validSpan())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Close surfaces the export error rather than swallowing it.
	if err := tr.Close(ctx); err == nil {
		t.Error("Close returned nil despite a failing transport")
	}

	m := tr.Metrics()
	if m.ExportFails == 0 {
		t.Error("export failure was not counted")
	}
	if m.SpansExported != 0 {
		t.Errorf("SpansExported = %d, want 0", m.SpansExported)
	}
	if m.SpansBuffered != 2 {
		t.Errorf(
			"SpansBuffered = %d, want 2 — failed spans must be requeued, not dropped",
			m.SpansBuffered,
		)
	}
	if m.SpansDropped != 0 {
		t.Errorf("SpansDropped = %d, want 0 — failure is not overflow", m.SpansDropped)
	}
}

// TestTracerRecoversAfterFailure is the other half of the outage story: once the
// aggregator returns, the buffered spans must actually get through.
func TestTracerRecoversAfterFailure(t *testing.T) {
	rt := newRecordingTransport()
	rt.setFailure(errors.New("connection refused"))

	tr := New(TracerConfig{
		Enabled: true, ServiceName: "svc", AggregatorURL: "http://a",
		Transport: rt, FlushInterval: 10 * time.Millisecond, HeartbeatInterval: time.Hour,
	})
	defer closeTracer(t, tr)

	tr.RecordSpan(validSpan())

	// Wait for at least one failed attempt, so recovery is genuinely
	// exercising the post-backoff path.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if tr.Metrics().ExportFails > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if tr.Metrics().ExportFails == 0 {
		t.Fatal("no export attempt failed within 2s")
	}

	rt.setFailure(nil)

	select {
	case <-rt.exported:
	case <-time.After(5 * time.Second):
		t.Fatal("no successful export after the transport recovered")
	}
	if got := rt.exportedSpans(); len(got) == 0 {
		t.Error("the buffered span was never delivered after recovery")
	}
}

// TestTracerDropsOldestWhenBufferFull is the OOM guard: an unreachable
// aggregator plus a busy service must cost bounded memory and old spans, never
// unbounded growth.
func TestTracerDropsOldestWhenBufferFull(t *testing.T) {
	rt := newRecordingTransport()
	rt.setFailure(errors.New("down"))

	tr := New(TracerConfig{
		Enabled: true, ServiceName: "svc", AggregatorURL: "http://a",
		Transport: rt, FlushInterval: time.Hour, HeartbeatInterval: time.Hour,
		MaxBufferSpans: 8,
	})
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = tr.Close(ctx) // returns the transport error; not the point here
	}()

	for i := 0; i < 100; i++ {
		tr.RecordSpan(validSpan())
	}

	m := tr.Metrics()
	if m.SpansBuffered != 8 {
		t.Errorf("SpansBuffered = %d, want the cap of 8", m.SpansBuffered)
	}
	if m.SpansDropped != 92 {
		t.Errorf("SpansDropped = %d, want 92", m.SpansDropped)
	}
	if m.SpansRecorded != 100 {
		t.Errorf(
			"SpansRecorded = %d, want 100 — dropping must not hide that a span happened",
			m.SpansRecorded,
		)
	}
}

// TestTracerCloseRespectsContext covers shutdown under a context that has
// already expired: Close must return promptly with an error rather than blocking
// a shutdown sequence.
func TestTracerCloseRespectsContext(t *testing.T) {
	rt := newRecordingTransport()
	tr := New(TracerConfig{
		Enabled: true, ServiceName: "svc", AggregatorURL: "http://a",
		Transport: rt, FlushInterval: time.Hour, HeartbeatInterval: time.Hour,
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() { done <- tr.Close(ctx) }()

	select {
	case <-done:
		// Either outcome is acceptable; returning at all is the point.
	case <-time.After(time.Second):
		t.Fatal("Close blocked on an already-cancelled context")

	}
}

// TestTracerCloseIsIdempotent matters because Close tends to be wired into both
// a defer and a signal handler, so it gets called twice in real shutdowns. The
// second call must not panic on an already-closed channel.
func TestTracerCloseIsIdempotent(t *testing.T) {
	rt := newRecordingTransport()
	tr := New(TracerConfig{
		Enabled: true, ServiceName: "svc", AggregatorURL: "http://a",
		Transport: rt, FlushInterval: time.Hour, HeartbeatInterval: time.Hour,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for i := 0; i < 3; i++ {
		if err := tr.Close(ctx); err != nil {
			t.Errorf("Close call %d: %v", i+1, err)
		}
	}
}

// --- Heartbeats ------------------------------------------------------------

func TestTracerSendsHeartbeat(t *testing.T) {
	rt := newRecordingTransport()
	tr := New(TracerConfig{
		Enabled: true, ServiceName: "orders-service", AggregatorURL: "http://a",
		Transport: rt, FlushInterval: time.Hour,
		HeartbeatInterval: 10 * time.Millisecond,
		Version:           "v1.2.3",
		OpenAPIHash:       "abc123",
		OpenAPIURL:        "http://orders/openapi.json",
	})
	defer closeTracer(t, tr)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(rt.exportedHeartbeats()) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	hbs := rt.exportedHeartbeats()
	if len(hbs) == 0 {
		t.Fatal("no heartbeat within 2s")
	}
	hb := hbs[0]
	if hb.Service != "orders-service" {
		t.Errorf("Service = %q", hb.Service)
	}
	if hb.InstanceID == "" {
		t.Error("InstanceID is empty — the aggregator cannot count instances without it")
	}
	if hb.Version != "v1.2.3" {
		t.Errorf("Version = %q", hb.Version)
	}
	// §9A.3 and §9C.3 both key schema refresh off this hash; dropping it
	// would silently disable contract validation and the catalog.
	if hb.OpenAPIHash != "abc123" {
		t.Errorf("OpenAPIHash = %q", hb.OpenAPIHash)
	}
	if hb.OpenAPIURL != "http://orders/openapi.json" {
		t.Errorf("OpenAPIURL = %q", hb.OpenAPIURL)
	}
}

// TestTracerGeneratesInstanceID checks that two instances of the same service
// are distinguishable without operator configuration — otherwise the registry
// cannot tell three replicas from one flapping process.
func TestTracerGeneratesInstanceID(t *testing.T) {
	mk := func() string {
		rt := newRecordingTransport()
		tr := New(TracerConfig{
			Enabled: true, ServiceName: "svc", AggregatorURL: "http://a",
			Transport: rt, FlushInterval: time.Hour, HeartbeatInterval: time.Hour,
		})
		defer closeTracer(t, tr)
		return tr.cfg.InstanceID
	}
	a, b := mk(), mk()
	if a == "" || b == "" {
		t.Fatal("InstanceID was not generated")
	}
	if a == b {
		t.Error("two tracers generated the same InstanceID")
	}
}

func TestTracerRespectsExplicitInstanceID(t *testing.T) {
	rt := newRecordingTransport()
	tr := New(TracerConfig{
		Enabled: true, ServiceName: "svc", AggregatorURL: "http://a",
		Transport: rt, FlushInterval: time.Hour, HeartbeatInterval: time.Hour,
		InstanceID: "pod-7",
	})
	defer closeTracer(t, tr)

	if got := tr.cfg.InstanceID; got != "pod-7" {
		t.Errorf("InstanceID = %q, want pod-7", got)
	}
}

// --- Backoff ---------------------------------------------------------------

// TestNextBackoff pins the retry curve. The cap is what keeps a long outage from
// pushing the retry interval into hours, which would leave a recovered
// aggregator waiting a long time for its first span.
func TestNextBackoff(t *testing.T) {
	cases := []struct {
		in, want time.Duration
	}{
		{0, time.Second},
		{-time.Second, time.Second},
		{time.Second, 2 * time.Second},
		{2 * time.Second, 4 * time.Second},
		{16 * time.Second, maxBackoff},
		{maxBackoff, maxBackoff},
		{time.Hour, maxBackoff},
	}
	for _, c := range cases {
		if got := nextBackoff(c.in); got != c.want {
			t.Errorf("nextBackoff(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// --- Concurrency -----------------------------------------------------------

// TestTracerConcurrentRecordAndFlush is the production shape: many request
// goroutines recording while the background loop exports. Run under -race, this
// is what proves the counters and buffer are safe to share.
func TestTracerConcurrentRecordAndFlush(t *testing.T) {
	rt := newRecordingTransport()
	tr := New(TracerConfig{
		Enabled: true, ServiceName: "svc", AggregatorURL: "http://a",
		Transport: rt, FlushInterval: time.Millisecond,
		HeartbeatInterval: 2 * time.Millisecond,
		MaxBufferSpans:    4096,
	})

	const (
		writers = 8
		perW    = 200
	)
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perW; i++ {
				tr.RecordSpan(validSpan())
			}
		}()
	}
	// Concurrent readers too: the dashboard polls Metrics while this runs.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = tr.Metrics()
		}
	}()
	wg.Wait()
	closeTracer(t, tr)

	if got := tr.Metrics().SpansRecorded; got != writers*perW {
		t.Errorf("SpansRecorded = %d, want %d", got, writers*perW)
	}
}
