package mcp

// tools_observability.go — what the dashboard knows about a service's insides that no tool
// read.
//
// The live tools cover a service's front door: its routes, the requests that failed, its
// logs. A request that fails because a database call failed, a job that never ran, a
// workflow stuck on a step, an event listener that threw: those are recorded, by the
// observability collector, and served by the dashboard's events page. The only thing an
// agent could learn about them was a count in the observability diagnostic ("0 signal(s)
// from 0 name(s)"), which says the collector exists and not what it saw.
//
// And the dashboard keeps two answers to "what is this service attached to, and is it up":
// the health checks the application registered, and the connections it declared. Both are
// where a dead database shows up first, and neither was readable.
//
// Both tools read endpoints the dashboard already serves to its own UI. They add nothing
// to what a person looking at the dashboard could see, except that payloads are left out.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// signalSources are the sources the observability collector records, mirrored from
// observability.Source. The dashboard answers a source it does not know with an empty
// list, which reads as "nothing failed there", so an unknown one is refused instead; a
// test pins this list to the constants.
var signalSources = []string{
	"events", "router", "http", "cache", "database", "scheduler", "websocket", "oauth2", "plugin", "workflow",
}

// ── breeze_get_signals ───────────────────────────────────────────────────────

// signalRow mirrors dashboard.eventRow, less its payload.
//
// The payload is the event's own data, which is application data: customer records, tokens
// in a job's arguments. A person looking at the dashboard chose to open that page; an agent
// reading on someone's behalf has not been asked to take it, and everything it reads goes
// into a model's context. It is omitted by not being declared here.
type signalRow struct {
	ID          uint64          `json:"id"`
	Name        string          `json:"name"`
	Source      string          `json:"source"`
	Time        string          `json:"time"`
	DurationMS  float64         `json:"duration_ms"`
	Failed      bool            `json:"failed"`
	Cancelled   bool            `json:"cancelled,omitempty"`
	Error       string          `json:"error,omitempty"`
	RequestID   string          `json:"request_id,omitempty"`
	ExecutionID string          `json:"execution_id,omitempty"`
	State       string          `json:"state,omitempty"`
	Trigger     string          `json:"trigger,omitempty"`
	Query       json.RawMessage `json:"query,omitempty"`
}

// signalMetric mirrors dashboard.eventMetric.
type signalMetric struct {
	Name        string  `json:"name"`
	Count       uint64  `json:"count"`
	Executed    uint64  `json:"executed"`
	Failed      uint64  `json:"failed"`
	AvgMS       float64 `json:"avg_ms"`
	MaxMS       float64 `json:"max_ms"`
	FailureRate float64 `json:"failure_rate"`
	Last        string  `json:"last,omitempty"`
}

// signalTotals mirrors dashboard.eventTotals.
type signalTotals struct {
	Signals        uint64  `json:"signals"`
	Failed         uint64  `json:"failed"`
	Listeners      uint64  `json:"listeners"`
	Dropped        uint64  `json:"dropped"`
	DistinctEvents int     `json:"distinct_events"`
	RatePerSec     float64 `json:"rate_per_sec"`
}

// signalsWire is the events endpoint's payload, less what is left out on purpose.
type signalsWire struct {
	Attached bool            `json:"attached"`
	Recent   []signalRow     `json:"recent"`
	Metrics  []signalMetric  `json:"metrics"`
	Live     json.RawMessage `json:"live,omitempty"`
	Totals   signalTotals    `json:"totals"`
}

type signalsReport struct {
	ServiceURL string          `json:"service_url"`
	Attached   bool            `json:"attached"`
	Source     string          `json:"source,omitempty"`
	Totals     signalTotals    `json:"totals"`
	Failing    []signalMetric  `json:"failing"`
	Count      int             `json:"count"`
	Recent     []signalRow     `json:"recent"`
	Live       json.RawMessage `json:"live,omitempty"`
	Notes      []string        `json:"notes,omitempty"`
}

type signalsArgs struct {
	liveArgs
	Source string `json:"source"`
	Name   string `json:"name"`
	Query  string `json:"query"`
	Failed *bool  `json:"failed"`
	Limit  int    `json:"limit"`
}

func getSignalsTool() *tool {
	return &tool{
		name: "breeze_get_signals",
		description: "Read what the service's observability collector recorded: event bus dispatches, " +
			"database queries, scheduler jobs, workflow executions, WebSocket activity, OAuth2 flows, cache " +
			"operations and plugin calls. This is where a failure that is not an HTTP error shows up: a job " +
			"that never ran, a listener that threw, a query that failed, a workflow stuck on a step. The " +
			"result lists which names are failing and how often, then the recent signals. By default only " +
			"failures are returned; pass failed=false for everything. Event payloads are not included, " +
			"because they are application data. Requires the dashboard feature with an event bus attached.",
		schema: objectSchema(liveProps(map[string]any{
			"source": map[string]any{
				"type":        "string",
				"enum":        append([]string{"all"}, signalSources...),
				"description": "Restrict to one source: database for queries, scheduler for jobs, workflow, events, websocket and so on. Defaults to all.",
			},
			"name":   stringProp("Exact signal name, such as an event type, a job name or a workflow name."),
			"query":  stringProp("Substring of the signal name to match."),
			"failed": boolProp("Return only failed signals. Defaults to true: this tool is read to find what is wrong."),
			"limit":  map[string]any{"type": "integer", "minimum": 1, "maximum": 1000, "description": "Most recent N signals. Defaults to 200."},
		}), "service_url"),
		run: func(raw json.RawMessage) toolCallResult {
			var a signalsArgs
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}
			return getSignals(a)
		},
	}
}

func getSignals(a signalsArgs) toolCallResult {
	source := strings.ToLower(strings.TrimSpace(a.Source))
	if source == "all" {
		source = ""
	}
	if source != "" {
		known := false
		for _, s := range signalSources {
			if s == source {
				known = true
			}
		}
		if !known {
			return errorResult(fmt.Sprintf("unknown source %q: use all, %s", a.Source, strings.Join(signalSources, ", ")))
		}
	}

	failedOnly := a.Failed == nil || *a.Failed

	query := url.Values{}
	if failedOnly {
		query.Set("failed", "1")
	}
	if source != "" {
		query.Set("source", source)
	}
	if name := strings.TrimSpace(a.Name); name != "" {
		query.Set("name", name)
	}
	if q := strings.TrimSpace(a.Query); q != "" {
		query.Set("q", q)
	}
	if a.Limit > 0 {
		query.Set("limit", fmt.Sprint(a.Limit))
	}

	req := a.request("/events", "dashboard")
	if enc := query.Encode(); enc != "" {
		req.path += "?" + enc
	}

	var wire signalsWire
	if err := fetchLiveJSON(req, &wire); err != nil {
		return liveResultError("reading observability signals", err)
	}

	report := signalsReport{
		ServiceURL: a.ServiceURL,
		Attached:   wire.Attached,
		Source:     source,
		Totals:     wire.Totals,
		Failing:    failingSignals(wire.Metrics),
		Count:      len(wire.Recent),
		Recent:     wire.Recent,
		Live:       wire.Live,
	}
	if report.Recent == nil {
		report.Recent = []signalRow{}
	}

	if !wire.Attached {
		report.Notes = append(report.Notes, "No event bus is attached to this service's dashboard, so the observability "+
			"collector records nothing: events, queries, jobs, workflows and WebSocket activity are invisible here, "+
			"and an empty answer is not evidence that none of them failed. Attach one with coll.AttachEvents; in a "+
			"generated project, add the events feature and re-add the dashboard so its block attaches the bus.")
		return structuredResult("no observability collector is attached to this service", report)
	}

	if wire.Totals.Dropped > 0 {
		report.Notes = append(report.Notes, fmt.Sprintf("%d signal(s) were dropped because the collector's buffer was full, "+
			"so failures older than the recent window may be missing from the rows below. The failing list is built "+
			"from counters, which still include them.", wire.Totals.Dropped))
	}
	if report.Count == 0 && failedOnly {
		switch {
		case wire.Totals.Failed > 0:
			report.Notes = append(report.Notes, fmt.Sprintf("No failed signal matches in the recent window, but %d failure(s) "+
				"were counted since the service started: they have been evicted from the buffer. The failing list "+
				"names which signals they belong to.", wire.Totals.Failed))
		default:
			report.Notes = append(report.Notes, fmt.Sprintf("No failed signals are recorded, out of %d signal(s) seen. That covers what "+
				"is instrumented: a failure in code that emits no signal is not here, and HTTP failures are read with "+
				"breeze_get_recent_errors.", wire.Totals.Signals))
		}
	}

	summary := fmt.Sprintf("%d signal(s) returned; %d recorded in total, %d failed; %d name(s) failing",
		report.Count, wire.Totals.Signals, wire.Totals.Failed, len(report.Failing))
	return structuredResult(summary, report)
}

// failingSignals picks the names with failures out of the collector's per-name counters,
// worst first. The counters are what survive eviction, so this is the answer to "what
// has been failing" even when the rows that failed are gone.
func failingSignals(metrics []signalMetric) []signalMetric {
	failing := []signalMetric{}
	for _, m := range metrics {
		if m.Failed > 0 {
			failing = append(failing, m)
		}
	}
	sort.SliceStable(failing, func(i, j int) bool {
		if failing[i].FailureRate != failing[j].FailureRate {
			return failing[i].FailureRate > failing[j].FailureRate
		}
		return failing[i].Failed > failing[j].Failed
	})
	if len(failing) > 10 {
		failing = failing[:10]
	}
	return failing
}

// ── breeze_get_health ────────────────────────────────────────────────────────

// dashboardSelfCheck is the one health check the dashboard registers for itself, named in
// dashboard.Install. It says the dashboard is running and nothing about the application.
const dashboardSelfCheck = "dashboard"

// healthCheck mirrors dashboard.HealthStatus.
type healthCheck struct {
	Name      string `json:"name"`
	Status    string `json:"status"`
	LatencyUS int64  `json:"latency_us"`
	Message   string `json:"message,omitempty"`
	Checked   string `json:"checked,omitempty"`
}

// healthWire is the architecture endpoint's payload. Connections are kept as the
// dashboard serves them: the fields are the dashboard's to add to, and this tool only
// needs to order them and read their status.
type healthWire struct {
	Connections  []map[string]any `json:"connections"`
	Total        int              `json:"total"`
	Connected    int              `json:"connected"`
	Degraded     int              `json:"degraded"`
	Disconnected int              `json:"disconnected"`
	Unknown      int              `json:"unknown"`
}

type healthReport struct {
	ServiceURL   string           `json:"service_url"`
	Red          int              `json:"red"`
	Yellow       int              `json:"yellow"`
	Green        int              `json:"green"`
	Checks       []healthCheck    `json:"checks"`
	Total        int              `json:"connections_total"`
	Connected    int              `json:"connected"`
	Degraded     int              `json:"degraded"`
	Disconnected int              `json:"disconnected"`
	Unknown      int              `json:"unknown"`
	Connections  []map[string]any `json:"connections"`
	Notes        []string         `json:"notes,omitempty"`
}

func getHealthTool() *tool {
	return &tool{
		name: "breeze_get_health",
		description: "Read whether the service's dependencies are up: the health checks the application " +
			"registered (a database ping, a cache check, an upstream) and the connections it declared " +
			"(databases, caches, queues, external services) with their current status. A dead database shows up " +
			"here first. Failing checks and connections are listed first. Running the checks is what reading " +
			"them does, so a slow dependency makes this call slow. Requires the dashboard feature; what appears " +
			"is what the application registered, so an empty list says nothing about its dependencies.",
		schema: objectSchema(liveProps(nil), "service_url"),
		run: func(raw json.RawMessage) toolCallResult {
			var a liveArgs
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}
			return getHealth(a)
		},
	}
}

func getHealth(a liveArgs) toolCallResult {
	var checks []healthCheck
	checksErr := fetchLiveJSON(a.request("/health", "dashboard"), &checks)

	var arch healthWire
	archErr := fetchLiveJSON(a.request("/architecture", "dashboard"), &arch)

	// Either half is worth having without the other, so one failing is a note. Both
	// failing means nothing was read, and that is an error.
	if checksErr != nil && archErr != nil {
		return liveResultError("reading health", checksErr)
	}

	report := healthReport{
		ServiceURL:   a.ServiceURL,
		Checks:       checks,
		Total:        arch.Total,
		Connected:    arch.Connected,
		Degraded:     arch.Degraded,
		Disconnected: arch.Disconnected,
		Unknown:      arch.Unknown,
		Connections:  arch.Connections,
	}
	if report.Checks == nil {
		report.Checks = []healthCheck{}
	}
	if report.Connections == nil {
		report.Connections = []map[string]any{}
	}

	if checksErr != nil {
		report.Notes = append(report.Notes, "The health checks could not be read ("+checksErr.Message+"); only the declared connections are shown.")
	}
	if archErr != nil {
		report.Notes = append(report.Notes, "The declared connections could not be read ("+archErr.Message+"); only the health checks are shown.")
	}

	for _, c := range report.Checks {
		switch c.Status {
		case "red":
			report.Red++
		case "yellow":
			report.Yellow++
		case "green":
			report.Green++
		}
	}

	// Worst first. This is read to find what is wrong.
	// Ties are broken by name: the dashboard runs its checks out of a map, so the order it
	// answers in is different every time, and a reader comparing two calls would see a
	// change that is not one.
	checkRank := map[string]int{"red": 0, "yellow": 1, "green": 2}
	sort.SliceStable(report.Checks, func(i, j int) bool {
		ri, rj := checkRank[report.Checks[i].Status], checkRank[report.Checks[j].Status]
		if ri != rj {
			return ri < rj
		}
		return report.Checks[i].Name < report.Checks[j].Name
	})
	connRank := map[string]int{"disconnected": 0, "degraded": 1, "unknown": 2, "connected": 3}
	sort.SliceStable(report.Connections, func(i, j int) bool {
		si, _ := report.Connections[i]["status"].(string)
		sj, _ := report.Connections[j]["status"].(string)
		return connRank[si] < connRank[sj]
	})

	// The dashboard registers one check of its own, which reports that the dashboard is
	// running. A service that registered nothing else would otherwise show "1 green", which
	// reads as a healthy service and says only that the page you are looking at loaded.
	application := 0
	for _, c := range report.Checks {
		if c.Name != dashboardSelfCheck {
			application++
		}
	}
	if checksErr == nil && application == 0 {
		report.Notes = append(report.Notes, "No health checks are registered by the application "+
			"(coll.RegisterHealthCheck); the only one is the dashboard's own, which reports that the dashboard is "+
			"running. So this says nothing about whether this service's dependencies are up.")
	}
	if archErr == nil && report.Total == 0 {
		report.Notes = append(report.Notes, "No connections are declared (coll.RegisterConnection): the dashboard "+
			"does not know what this service depends on, so an empty list is not evidence that it depends on nothing.")
	}
	if report.Unknown > 0 {
		report.Notes = append(report.Notes, fmt.Sprintf("%d connection(s) have status unknown: they have not been checked "+
			"yet, which is not the same as being up.", report.Unknown))
	}

	summary := fmt.Sprintf("%d health check(s): %d red, %d yellow, %d green; %d connection(s): %d disconnected, %d degraded, %d unknown",
		len(report.Checks), report.Red, report.Yellow, report.Green, report.Total, report.Disconnected, report.Degraded, report.Unknown)
	return structuredResult(summary, report)
}
