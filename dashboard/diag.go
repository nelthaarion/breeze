package dashboard

// diag.go — the dashboard's own probe, and the endpoint that serves every
// subsystem's.
//
// # Two separate things
//
// The probe reports the dashboard: what it captured, whether persistence is
// working, whether anyone is watching. It is one entry in the registry like any
// other.
//
// The endpoint is the read side of the whole registry. GET
// /dashboard/api/diagnostics runs every registered probe and returns the reports,
// which is what makes "what is every subsystem of this process doing" a single
// HTTP call — and therefore a single MCP tool call, since internal/mcp reaches a
// running service through exactly these endpoints.
//
// It is registered under the dashboard's auth like every other API route. A
// diagnostics document names configuration, directory paths and traffic volumes;
// it is not secret, but it is not something to serve to the internet either.
//
// # Counters
//
// Install enables diag's counted diagnostics. A process that installed the
// dashboard has already accepted per-request instrumentation — that is what the
// dashboard is — so the middleware counters that are gated off by default are
// exactly the numbers its operator wants. A process without the dashboard pays
// nothing, and the reports say counting is off rather than showing zeroes.

import (
	"fmt"
	"strings"
	"time"

	"github.com/nelthaarion/breeze"
	"github.com/nelthaarion/breeze/diag"
)

// diagName is the registry key, matching the `breeze add dashboard` feature name.
const diagName = "dashboard"

// registerDiagnostics publishes c as the process's dashboard diagnostic.
func (c *Collector) registerDiagnostics() {
	diag.Register(diagName, c.probe)
}

// probe reports the dashboard's state.
func (c *Collector) probe() diag.Report {
	if c == nil {
		return diag.Off("no dashboard collector is registered; call dashboard.Install(app, router, cfg)")
	}
	if !c.cfg.Enabled {
		return diag.Off("the dashboard is installed but Config.Enabled is false, so nothing is "+
			"being captured and the pages render empty").
			WithDetail("base_path", c.cfg.BasePath)
	}

	watchers := 0
	if c.hub != nil {
		watchers = c.hub.clientCount()
	}

	m := c.Metrics()
	detail := map[string]any{
		"base_path":         strings.TrimSuffix(c.cfg.BasePath, "/"),
		"requests_captured": c.requestsTotal.Load(),
		"errors":            c.errorsTotal.Load(),
		"live_watchers":     watchers,
		"buffered_requests": len(c.Requests(0)),
		"tracked_routes":    len(c.RouteStats()),
		"timelines":         len(c.Timelines(0)),
		"events_attached":   c.EventsAttached(),
		"video_attached":    c.VideoAttached(),
		"db_inspector":      c.DBInspector() != nil,
		"storage_type":      c.cfg.StorageType,
		"auth_enabled":      c.cfg.Username != "",
		"service_token":     c.cfg.ServiceToken != "",
		"fleet_view":        c.cfg.FleetAggregatorURL != "",
		"timeline_enabled":  c.cfg.Timeline,
		"dev_mode":          c.cfg.DevMode,
		"health_checks":     len(c.RunHealthChecks()),
		"goroutines":        m.Goroutines,
	}
	if !m.Time.IsZero() {
		detail["last_sample"] = m.Time.UTC().Format(time.RFC3339)
	}

	summary := fmt.Sprintf("mounted at %s: %d request(s) captured, %d live watcher(s)",
		detail["base_path"], c.requestsTotal.Load(), watchers)

	var notes []string
	degraded := false
	if !m.Time.IsZero() && time.Since(m.Time) > 5*time.Second {
		degraded = true
		notes = append(notes, fmt.Sprintf("The metrics sampler last ran %s ago. The Performance "+
			"page is showing stale numbers.", time.Since(m.Time).Round(time.Second)))
	}
	if c.cfg.Username == "" {
		degraded = true
		notes = append(notes, "The dashboard has no username configured, so its pages and API are "+
			"unauthenticated. Anyone who can reach this service can read its request log, its "+
			"queries and — if a DB inspector is set — its data.")
	}
	if watchers == 0 {
		notes = append(notes, "Nobody has the dashboard open, so per-route statistics are only "+
			"accumulated for slow and failed requests. A route reporting zero requests means "+
			"'not measured', not 'never called'.")
	}
	if c.cfg.DevMode {
		notes = append(notes, "DevMode serves the readable, unminified dashboard assets. Correct "+
			"while working on the dashboard itself, a larger download otherwise.")
	}

	if degraded {
		return diag.Degraded(summary, detail).WithNotes(notes...)
	}
	return diag.OK(summary, detail).WithNotes(notes...)
}

// ─── the diagnostics endpoint ────────────────────────────────────────────────

// diagnosticsPayload is the endpoint's response.
//
// Deliberately not a bare array. A caller needs to know whether counted
// diagnostics were on when this was read — without it every zero is ambiguous —
// and needs the summary counts to decide whether to look further. A bare array
// has nowhere to put either.
type diagnosticsPayload struct {
	// Subsystems is every registered probe's report, sorted by name.
	Subsystems []diag.Report `json:"subsystems"`

	// Counts summarise the statuses, so a caller can decide "is anything wrong"
	// without walking the list.
	Total     int `json:"total"`
	OK        int `json:"ok"`
	Degraded  int `json:"degraded"`
	Off       int `json:"off"`
	Unknown   int `json:"unknown"`
	WithNotes int `json:"with_notes"`

	// Counting reports whether diag's counted diagnostics were enabled, and
	// since when. Install enables them, so this is true for any process serving
	// this endpoint — but it can be turned off at runtime, and a reader looking
	// at zeroes needs to know which.
	Counting      bool   `json:"counting"`
	CountingSince string `json:"counting_since,omitempty"`

	// Notes are about the document itself, not about any one subsystem.
	Notes []string `json:"notes,omitempty"`
}

// handleDiagnostics serves every subsystem's report.
//
// One query parameter: subsystem=<name> runs a single probe, for a caller that
// knows what it wants and does not need thirteen reports to get it.
func (c *Collector) handleDiagnostics(ctx *breeze.Context) error {
	if name := strings.TrimSpace(ctx.Query("subsystem")); name != "" {
		report, found := diag.Get(name)
		if !found {
			ctx.Status(404)
			return ctx.JSON(map[string]any{
				"error":      fmt.Sprintf("no subsystem named %q is registered", name),
				"registered": diag.Registered(),
			})
		}
		return ctx.JSON(report)
	}

	reports := diag.Snapshot()
	payload := diagnosticsPayload{
		Subsystems: reports,
		Total:      len(reports),
	}
	for _, r := range reports {
		switch r.Status {
		case diag.StatusOK:
			payload.OK++
		case diag.StatusDegraded:
			payload.Degraded++
		case diag.StatusOff:
			payload.Off++
		default:
			payload.Unknown++
		}
		if len(r.Notes) > 0 {
			payload.WithNotes++
		}
	}

	since, counting := diag.CountersSince()
	payload.Counting = counting
	if counting && !since.IsZero() {
		payload.CountingSince = since.UTC().Format(time.RFC3339)
	}
	if !counting {
		payload.Notes = append(payload.Notes, "Counted diagnostics are off, so any hit/miss "+
			"numbers below are not measurements of an idle subsystem — nothing was counted. "+
			"Call diag.EnableCounters() to turn them on without a restart.")
	}
	if payload.Off > 0 {
		payload.Notes = append(payload.Notes, fmt.Sprintf("%d subsystem(s) report status \"off\". "+
			"That is not a fault: it means not installed, or installed and deliberately disabled. "+
			"Each one's summary names what would turn it on.", payload.Off))
	}

	return ctx.JSON(payload)
}
