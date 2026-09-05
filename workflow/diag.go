package workflow

// diag.go — the workflow engine's diagnostic probe.
//
// This is the subsystem the Part 3 audit found had no diagnostic surface at all:
// a workflow could run, retry, fail and roll back, and the only way to see any of
// it was to have subscribed to the event bus beforehand. The dashboard's Events
// page shows live executions when a bus is attached, but a workflow engine
// constructed without one — the default, since Bus defaults to events.Default and
// nothing attaches that to a dashboard automatically — was invisible.
//
// The probe answers from the engine's own registry and its counters. The counters
// are unconditional; see the field comments in engine.go for why an atomic add
// per execution and per step attempt is the right call here, when both already
// write to a Store and publish an event.
//
// What the probe does *not* do is query the Store. PendingWorkflows is exactly the
// number an operator wants, and it is a database round trip: a probe that made one
// would block the diagnostics endpoint on the health of the persistence layer,
// which is the one dependency most likely to be the reason someone is reading
// diagnostics. So the report says the store is configured, names its type, and
// says a pending count needs Resume or a direct query. That is honest and instant.

import (
	"fmt"
	"reflect"
	"time"

	"github.com/nelthaarion/breeze/v2/diag"
)

// diagName is the registry key, matching the `breeze add workflow` feature name.
const diagName = "workflow"

// RegisterDiagnostics publishes e as the process's workflow diagnostic.
//
// Called by [NewEngine]. An application that builds two engines reports the
// second, which is the same "last one wins" rule the registry uses everywhere.
func RegisterDiagnostics(e *Engine) {
	if e == nil {
		return
	}
	diag.Register(diagName, e.probe)
}

// probe reports the engine's state.
func (e *Engine) probe() diag.Report {
	if e == nil {
		return diag.Off("no workflow engine is registered")
	}

	defs := e.Definitions()
	started := e.started.Load()
	completed := e.completed.Load()
	failed := e.failed.Load()

	detail := map[string]any{
		"workflows_registered": len(defs),
		"workflows":            defs,
		"triggers":             e.triggerCounts(),
		"executions_started":   started,
		"completed":            completed,
		"failed":               failed,
		"compensated":          e.compensated.Load(),
		"step_attempts":        e.stepsRun.Load(),
		"step_retries":         e.stepRetries.Load(),
		"in_flight":            len(e.sem),
		"max_workers":          e.cfg.MaxWorkers,
		"store":                storeKind(e.store),
		"bus_attached":         e.bus != nil,
		"observability":        e.col != nil,
		"shutdown_timeout":     e.cfg.ShutdownTimeout.String(),
		"closed":               e.closed.Load(),
	}
	if nanos := e.lastRunNanos.Load(); nanos != 0 {
		detail["last_execution"] = time.Unix(0, nanos).UTC().Format(time.RFC3339Nano)
	}

	notes := []string{
		"Pending executions are not counted here: that is a Store query, and a diagnostic " +
			"probe must not block on the persistence layer. Call Engine.Resume, or query the " +
			"store directly, for a pending count.",
	}
	if e.col == nil {
		notes = append(
			notes,
			"Observability is disabled on this engine, so executions do not reach "+
				"the dashboard's Events page. The counters above are unaffected.",
		)
	}
	if _, memory := e.store.(*MemoryStore); memory {
		notes = append(
			notes,
			"The store is the in-memory default, so execution state does not survive "+
				"a restart and Engine.Resume will find nothing after one.",
		)
	}

	if e.closed.Load() {
		return diag.Off(fmt.Sprintf("the workflow engine is shut down; %d workflow(s) remain registered",
			len(defs))).
			WithNotes(notes...)
	}

	summary := fmt.Sprintf("%d workflow(s) registered, %d execution(s): %d completed, %d failed",
		len(defs), started, completed, failed)

	// A failure rate is a property of the workflows, not of the engine, so a
	// failed execution is not on its own a degraded engine. What is degraded is
	// an engine where *every* execution failed, which means a wiring problem
	// rather than a business one.
	if started > 0 && failed == started {
		return diag.Degraded(summary+" — every execution has failed", detail).WithNotes(notes...)
	}
	return diag.OK(summary, detail).WithNotes(notes...)
}

// triggerCounts summarises how the registered workflows are started.
//
// Worth reporting because "registered but never triggered" is the most common
// workflow bug: a definition with an event trigger whose event nothing emits
// looks identical to a healthy idle workflow from the outside.
func (e *Engine) triggerCounts() map[string]int {
	e.mu.RLock()
	defer e.mu.RUnlock()

	counts := map[string]int{}
	for _, d := range e.defs {
		kind := "manual"
		if d.trig != nil {
			kind = "event"
		}
		counts[kind]++
	}
	return counts
}

// storeKind names the Store implementation for a report.
//
// The type name rather than a stringer, because Store is an interface an
// application may implement and there is no method to ask. reflect is acceptable
// here: this runs once per diagnostic read, never on an execution path.
func storeKind(s Store) string {
	if s == nil {
		return "none"
	}
	t := reflect.TypeOf(s)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if pkg := t.PkgPath(); pkg != "" {
		return pkg + "." + t.Name()
	}
	return t.Name()
}
