package mcp

// live_traffic.go — the exact request counts behind the dashboard's sampled records.
//
// The requests endpoint serves full records, and the dashboard keeps a failure in full only
// up to a per-route budget per window; the routes endpoint serves per-route totals. Neither
// says how much was counted and not kept. The traffic endpoint does, and the two tools that
// describe traffic (breeze_get_routes, breeze_get_recent_errors) read it so that what they
// say about "zero" and "none" is a statement about the service, not about what happened to
// be stored.

import (
	"fmt"
)

// liveRouteTraffic mirrors dashboard.RouteTraffic's JSON.
type liveRouteTraffic struct {
	Method       string `json:"method"`
	Pattern      string `json:"pattern"`
	Requests     int64  `json:"requests"`
	ClientErrors int64  `json:"client_errors"`
	ServerErrors int64  `json:"server_errors"`
	NotRecorded  int64  `json:"not_recorded"`
}

// liveCapturePolicy mirrors dashboard.CapturePolicy's JSON.
type liveCapturePolicy struct {
	Watching              bool `json:"watching"`
	WindowSeconds         int  `json:"window_seconds"`
	ClientErrorsPerWindow int  `json:"client_errors_per_window"`
	ServerErrorsPerWindow int  `json:"server_errors_per_window"`
	SlowRequestMS         int  `json:"slow_request_ms"`
	RingCapacity          int  `json:"ring_capacity"`
}

// liveTraffic mirrors dashboard.TrafficReport's JSON.
type liveTraffic struct {
	Instrumented bool               `json:"instrumented"`
	Enabled      bool               `json:"enabled"`
	Requests     int64              `json:"requests"`
	ClientErrors int64              `json:"client_errors"`
	ServerErrors int64              `json:"server_errors"`
	Unmatched    liveRouteTraffic   `json:"unmatched"`
	Routes       []liveRouteTraffic `json:"routes"`
	Capture      liveCapturePolicy  `json:"capture"`
}

// counting says what the dashboard's request counts mean.
type counting string

const (
	// countingExact: every request that reaches the dashboard's middleware is counted against
	// its route and status class, so zero means nothing came.
	countingExact counting = "exact"
	// countingNotMeasured: the dashboard is installed but nothing is counting (its middleware
	// was never created, or the dashboard is disabled), so every number is zero because nothing
	// was measured.
	countingNotMeasured counting = "not_measured"
	// countingPartial: an older dashboard that counts a request against its route only when it
	// captured the request in full. Zero there means "not measured".
	countingPartial counting = "partial"
)

// readTraffic reads the traffic endpoint. A dashboard that does not have it, or a read that
// fails, answers nil: the caller then describes the counts the way an older dashboard earned,
// instead of claiming an exactness it cannot confirm.
func readTraffic(a liveArgs) *liveTraffic {
	var t liveTraffic
	if err := fetchLiveJSON(a.request("/traffic", "dashboard"), &t); err != nil {
		return nil
	}
	return &t
}

// countingOf classifies a traffic read.
func countingOf(t *liveTraffic) counting {
	switch {
	case t == nil:
		return countingPartial
	case !t.Instrumented || !t.Enabled:
		return countingNotMeasured
	}
	return countingExact
}

// notCountingExplanation says why nothing is being counted.
func notCountingExplanation(t *liveTraffic) string {
	if !t.Enabled {
		return "The dashboard is installed but Config.Enabled is false, so it counts nothing and every figure " +
			"here is zero. That is 'not measured', not 'never called'."
	}
	return "The dashboard is installed but its middleware is not: dashboard.Middleware(coll) was never created, " +
		"so no request is counted and every figure here is zero. That is 'not measured', not 'never called'. " +
		"Add router.Use(dashboard.Middleware(coll))."
}

// captureSentence states the sampling policy in the dashboard's own numbers.
func captureSentence(p liveCapturePolicy) string {
	slow := "slow requests are not kept separately"
	if p.SlowRequestMS > 0 {
		slow = fmt.Sprintf("a request slower than %dms is always kept", p.SlowRequestMS)
	}
	return fmt.Sprintf("Every request is counted exactly. Full records are kept for a failure only up to %d client errors "+
		"(4xx) and %d server errors (5xx) per route per %ds window, so a flood of one failure is counted and only "+
		"sampled; %s; and every request is kept while someone has the dashboard open. The most recent %d records are retained.",
		p.ClientErrorsPerWindow, p.ServerErrorsPerWindow, p.WindowSeconds, slow, p.RingCapacity)
}
