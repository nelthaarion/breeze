package fleet

// diag.go — the tracer's diagnostic probe.
//
// The tracer already keeps every number this reports: spans recorded, exported
// and dropped, export failures, and the ring's current depth. What it did not
// have was a way for anything other than the dashboard's Performance page to ask
// for them, and the Performance page only exists if the dashboard is installed.
//
// Nothing here is added to RecordSpan or to the export loop. The probe reads the
// same atomics Metrics reads, plus configuration decided once at New.
//
// # Why a disabled tracer is StatusOff rather than absent
//
// New returns a working no-op Tracer for a disabled, misconfigured or incomplete
// configuration, deliberately: a service must start and serve traffic when its
// tracing config is wrong. The cost of that decision is that "tracing is silently
// off" is a state a service can be in for months. The probe reports exactly why —
// disabled, no service name, no aggregator URL — which is the one thing the
// no-op design cannot tell anyone by itself.

import (
	"fmt"
	"time"

	"github.com/nelthaarion/breeze/v2/diag"
)

// diagName is the registry key, matching the `breeze add fleet` feature name.
const diagName = "fleet"

// RegisterDiagnostics publishes t as the process's fleet-tracing diagnostic.
//
// Called by [New], including for the no-op tracer a disabled configuration
// produces — that tracer's report is the answer to "why are there no traces".
func RegisterDiagnostics(t *Tracer) {
	if t == nil {
		return
	}
	diag.Register(diagName, t.probe)
}

// probe reports the tracer's state.
func (t *Tracer) probe() diag.Report {
	if t == nil {
		return diag.Off("no fleet tracer is registered; call fleet.New(cfg) and install " +
			"fleet.Middleware(tracer)")
	}

	if !t.enabled {
		// Naming the specific reason rather than "disabled": a config with
		// Enabled true and no ServiceName lands here too, and those want
		// different fixes.
		reason := "fleet tracing is disabled"
		switch {
		case !t.cfg.Enabled:
			reason = "fleet tracing is disabled: TracerConfig.Enabled is false"
		case t.cfg.ServiceName == "":
			reason = "fleet tracing is off because TracerConfig.ServiceName is empty — without a " +
				"name the topology graph would have an unnamed node, so New refuses to enable it"
		case t.cfg.AggregatorURL == "":
			reason = "fleet tracing is off because TracerConfig.AggregatorURL is empty, so there " +
				"is nowhere to export spans to"
		}
		return diag.Off(reason).
			WithDetail("service_name", t.cfg.ServiceName).
			WithDetail("aggregator_url", t.cfg.AggregatorURL).
			WithNotes("A misconfigured tracer is a working no-op rather than a startup error, so " +
				"the service runs normally and records nothing. This report is the only place that " +
				"is stated.")
	}

	m := t.Metrics()
	detail := map[string]any{
		"service_name":    t.cfg.ServiceName,
		"aggregator_url":  t.cfg.AggregatorURL,
		"sample_rate":     t.cfg.SampleRate,
		"spans_recorded":  m.SpansRecorded,
		"spans_exported":  m.SpansExported,
		"spans_dropped":   m.SpansDropped,
		"spans_buffered":  m.SpansBuffered,
		"export_failures": m.ExportFails,
		"spans_errored":   t.spansErrored.Load(),
		"max_buffer":      t.cfg.MaxBufferSpans,
		"flush_interval":  t.cfg.FlushInterval.String(),
		"transport":       transportKind(t.transport),
	}
	if !t.lastHeartbeatAt.IsZero() {
		detail["last_heartbeat"] = t.lastHeartbeatAt.UTC().Format(time.RFC3339)
	}

	summary := fmt.Sprintf("%s → %s: %d span(s) recorded, %d exported",
		t.cfg.ServiceName, t.cfg.AggregatorURL, m.SpansRecorded, m.SpansExported)

	var notes []string
	degraded := false
	if m.SpansDropped > 0 {
		degraded = true
		notes = append(notes, fmt.Sprintf("%d span(s) were dropped rather than exported. That is "+
			"either an unreachable aggregator or MaxBufferSpans (%d) being too small for this "+
			"service's rate. Export is best-effort by design, so nothing else reports this.",
			m.SpansDropped, t.cfg.MaxBufferSpans))
	}
	if m.ExportFails > 0 {
		degraded = true
		notes = append(notes, fmt.Sprintf("%d export attempt(s) failed. Check that %s is reachable "+
			"from inside this process — in a container, the host's loopback is not the container's.",
			m.ExportFails, t.cfg.AggregatorURL))
	}
	if m.SpansRecorded > 0 && m.SpansExported == 0 {
		degraded = true
		notes = append(notes, "Spans are being recorded but none has been exported. If this "+
			"persists past one flush interval, the transport is not reaching the aggregator.")
	}
	if t.cfg.SampleRate > 0 && t.cfg.SampleRate < 1 {
		notes = append(notes, fmt.Sprintf("Sampling is at %.2f, so roughly %.0f%% of requests are "+
			"not traced at all. A trace that appears incomplete may simply have unsampled hops.",
			t.cfg.SampleRate, (1-t.cfg.SampleRate)*100))
	}

	if degraded {
		return diag.Degraded(summary, detail).WithNotes(notes...)
	}
	return diag.OK(summary, detail).WithNotes(notes...)
}

// transportKind names the Transport implementation for a report.
//
// A string switch on the concrete type would need this package to import every
// transport, which is exactly the coupling the Transport interface exists to
// avoid. %T is the honest answer and costs nothing at read time.
func transportKind(tr Transport) string {
	if tr == nil {
		return "none"
	}
	return fmt.Sprintf("%T", tr)
}
