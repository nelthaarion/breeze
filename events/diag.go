package events

// diag.go — the event bus's diagnostic probe.
//
// Everything here is a read of state the bus already maintains: per-event
// metrics it was already accumulating (unless DisableMetrics), the registry's own
// counts, and the pool and recorder statistics the inspector already exposes.
// Nothing new is measured and nothing is added to the dispatch path, so a bus
// that is never asked for a report behaves exactly as it did before this file
// existed.
//
// The bus is registered when it is constructed. A second bus replaces the first
// in the registry, which is the right reading of "which bus is this process
// using": an application that built a bus after the default one is using the one
// it built.

import (
	"fmt"
	"time"

	"github.com/nelthaarion/breeze/v2/diag"
)

// diagName is the registry key. It matches the `breeze add events` feature name
// so an agent can ask about "events" without a translation table.
const diagName = "events"

// RegisterDiagnostics publishes bus as the process's event-bus diagnostic.
//
// Called by [New], so an application never needs it. It is exported for the case
// where a bus was constructed before diagnostics mattered — a library handing one
// back, say — and the application wants that bus to be the one reported.
func RegisterDiagnostics(bus *Bus) {
	if bus == nil {
		return
	}
	diag.Register(diagName, bus.probe)
}

// probe reports the bus's state.
//
// A closed bus reports StatusOff rather than degraded: closing is a deliberate
// act, usually a shutdown in progress, and calling it a fault would make every
// clean shutdown look like an incident.
func (b *Bus) probe() diag.Report {
	if b == nil {
		return diag.Off("no event bus is registered")
	}
	if b.Closed() {
		return diag.Off("the event bus is closed; no further dispatch will happen").
			WithDetail("events_registered", b.EventCount())
	}

	total := b.TotalMetrics()
	pool := b.PoolStats()
	rec := b.RecorderStats()

	detail := map[string]any{
		"events_registered": b.EventCount(),
		"listeners":         b.ListenerCount(),
		"dispatches":        total.Dispatches,
		"listener_calls":    total.Listeners,
		"failures":          total.Failures,
		"panics":            total.Panics,
		"stopped":           total.Stopped,
		"filtered":          total.Filtered,
		"avg_ms":            msOf(total.AvgDuration),
		"max_ms":            msOf(total.MaxDuration),
		"metrics_enabled":   b.cfg.Metrics,
		"observer_attached": b.ObserverEnabled(),
		"async_mode":        b.cfg.Async.String(),
		"recorder": map[string]any{
			"enabled":  rec.Enabled,
			"payloads": rec.Payloads,
			"size":     rec.Size,
			"capacity": rec.Capacity,
			"written":  rec.Total,
			"evicted":  rec.Total - uint64(rec.Size),
		},
	}
	if !total.LastDispatch.IsZero() {
		detail["last_dispatch"] = total.LastDispatch.UTC().Format(time.RFC3339Nano)
	}
	// The pool exists only under AsyncWorkerPool, and reporting a zero-worker
	// pool for a bus that uses goroutines would read as a misconfiguration.
	if pool.Workers > 0 {
		detail["pool"] = map[string]any{
			"workers":  pool.Workers,
			"queued":   pool.Queued,
			"spawned":  pool.Spawned,
			"dropped":  pool.Dropped,
			"pending":  pool.Pending,
			"capacity": pool.Capacity,
		}
	}

	summary := fmt.Sprintf("%d event type(s), %d listener(s), %d dispatch(es)",
		b.EventCount(), b.ListenerCount(), total.Dispatches)

	// Three conditions are worth flagging, and each is a real operational
	// problem rather than a threshold someone guessed at.
	var notes []string
	degraded := false
	if total.Panics > 0 {
		degraded = true
		notes = append(notes, fmt.Sprintf("%d listener panic(s) were recovered. A recovered panic "+
			"still means the listener did not finish its work.", total.Panics))
	}
	if pool.Dropped > 0 {
		degraded = true
		notes = append(notes, fmt.Sprintf("%d async dispatch(es) were dropped because the pool queue "+
			"was full. Raise QueueSize or Workers, or use OverflowSpawn.", pool.Dropped))
	}
	if !b.cfg.Metrics {
		notes = append(notes, "Per-event metrics are disabled for this bus, so the dispatch, "+
			"listener and duration numbers above stay at zero. They are not a report of an idle bus.")
	}
	if total.Failures > 0 {
		notes = append(notes, fmt.Sprintf("%d listener error(s) were returned. Whether a dispatch "+
			"continued past one depends on ContinueOnError, which is %t here.",
			total.Failures, b.cfg.ContinueOnError))
	}

	if degraded {
		return diag.Degraded(summary, detail).WithNotes(notes...)
	}
	return diag.OK(summary, detail).WithNotes(notes...)
}

// msOf renders a duration as fractional milliseconds.
//
// A thin wrapper over [diag.Milliseconds] rather than a call at each site: it is
// used twice in this file's detail map, and the short name keeps those lines
// readable. The rounding rule lives with the shared function.
func msOf(d time.Duration) float64 {
	return diag.Milliseconds(d)
}
