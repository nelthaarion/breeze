package mcp

// tools_fleet.go — reading a running Fleet aggregator.
//
// These five tools are the distributed counterpart to tools_live.go. That file
// asks one service what it is doing; this one asks the aggregator what the whole
// fleet is doing, which is the only place a cross-service question can be
// answered at all.
//
// # Nothing here computes an answer
//
// The aggregator already does the analysis. Assemble marks the root-cause span of
// a failing trace (§9B.1), Incidents computes blast radius from topology counters
// (§9B.2), and the contract checker groups violations. Re-deriving any of that
// here would produce a second implementation that disagrees with the first under
// exactly the conditions that matter — a partially reported trace, a service with
// too little traffic to judge — and the disagreement would surface as a
// contradiction between the dashboard and the agent.
//
// So breeze_explain_incident does no analysis. It reads the three answers the
// aggregator has already computed and joins them: the trace says what broke, the
// incident list says what that took down with it, and the violation list says
// whether a contract mismatch explains why. The joining is the value; the
// conclusions are the aggregator's.
//
// # Mirror structs, again
//
// The response types are declared locally rather than imported from
// fleet/aggregator, for the reason given in tools_live.go: package dashboard and
// package aggregator both import the root breeze package, and Part 4 requires
// breeze to import this one. Importing either here would close a cycle. A test in
// package mcp_test — which can import both — marshals the aggregator's real types
// and asserts every key these structs read still exists, so a rename upstream
// fails loudly here instead of silently decoding into zero values.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

func registerFleetTools(s *Server) {
	s.addTool(getTopologyTool())
	s.addTool(getTracesTool())
	s.addTool(getTraceTool())
	s.addTool(getContractViolationsTool())
	s.addTool(explainIncidentTool())
}

// defaultFleetBase matches aggregator.DefaultBasePath.
const defaultFleetBase = "/fleet"

// traceIDLength is the hex length the aggregator requires.
//
// Checked here rather than left to the server because the aggregator answers a
// malformed id with a bare 400, which fetchLiveJSON would report as a status
// failure — technically true and useless. The common causes are a truncated
// copy-paste and a span id used in place of a trace id, and both are worth naming.
const traceIDLength = 32

// fleetArgs is the argument set every Fleet tool shares.
type fleetArgs struct {
	AggregatorURL string `json:"aggregator_url"`
	BasePath      string `json:"base_path"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	Token         string `json:"token"`
}

// request builds the transport call for one aggregator API path.
//
// # Why the base path is not simply appended
//
// Two conventions for naming an aggregator exist in this codebase, and both are correct
// in their own place. A tracer's AggregatorURL and dashboard's FleetAggregatorURL are
// full mount URLs — "http://fleet:9000/fleet" — because that is the endpoint they POST
// to. These tools take an origin plus a separate base_path, because base_path is
// configurable and a tool has to build several API paths under it.
//
// So a caller who pastes the URL from their tracer config, from a dashboard config, or
// from provision_fleet's own aggregator_url gets "/fleet/fleet/api/topology" and a 404
// that says the feature is not installed — when it is installed, at the address they
// gave. That is the least useful possible answer to the most likely mistake.
//
// Detecting the duplicate is unambiguous: a base URL whose path already ends with the
// base path is naming the mount, not an origin. Nothing legitimate is broken by the
// check, because an aggregator mounted at /fleet/fleet would be named by a base_path of
// "/fleet/fleet" and still resolve correctly.
func (a fleetArgs) request(apiPath, feature string) liveRequest {
	base := strings.TrimSpace(a.BasePath)
	if base == "" {
		base = defaultFleetBase
	}
	base = "/" + strings.Trim(base, "/")

	path := base + "/api" + apiPath
	if strings.HasSuffix(strings.TrimSuffix(strings.TrimSpace(a.AggregatorURL), "/"), base) {
		// The URL already carries the mount, so adding it again would look for the
		// aggregator underneath itself.
		path = "/api" + apiPath
	}

	return liveRequest{
		baseURL:  a.AggregatorURL,
		path:     path,
		username: a.Username,
		password: a.Password,
		token:    a.Token,
		feature:  feature,
	}
}

// fleetProps returns the shared schema properties, for the same
// anti-drift reason as liveProps.
func fleetProps(extra map[string]any) map[string]any {
	props := map[string]any{
		"aggregator_url": stringProp("Base URL of the running Fleet aggregator, e.g. " +
			"http://127.0.0.1:9000. A bare host:port is accepted. This is the aggregator's " +
			"own address, not the address of a service that reports to it."),
		"base_path": stringProp(
			"Where the aggregator is mounted. Defaults to " + defaultFleetBase + ".",
		),
		"username": stringProp("Aggregator read username, sent as HTTP Basic. Required when the " +
			"aggregator sets username and password."),
		"password": stringProp("Aggregator read password, sent as HTTP Basic."),
		"token": stringProp(
			"Ingest token, sent as X-Fleet-Token. Not needed for reading — the read " +
				"endpoints use Basic auth — and accepted only so one credential set can be passed to " +
				"every tool.",
		),
	}
	for k, v := range extra {
		props[k] = v
	}
	return props
}

// ─── mirrors of the aggregator's JSON ────────────────────────────────────────

// fleetNode mirrors aggregator.Node.
type fleetNode struct {
	Service      string  `json:"service"`
	Calls        uint64  `json:"calls"`
	Errors       uint64  `json:"errors"`
	ErrorRate    float64 `json:"error_rate"`
	P99Ms        float64 `json:"p99_ms"`
	Entry        bool    `json:"entry,omitempty"`
	LastSeenUnix int64   `json:"last_seen_unix"`
}

// fleetEdge mirrors aggregator.Edge.
type fleetEdge struct {
	Caller       string  `json:"caller"`
	Callee       string  `json:"callee"`
	Calls        uint64  `json:"calls"`
	Errors       uint64  `json:"errors"`
	ErrorRate    float64 `json:"error_rate"`
	P50Ms        float64 `json:"p50_ms"`
	P95Ms        float64 `json:"p95_ms"`
	P99Ms        float64 `json:"p99_ms"`
	AvgMs        float64 `json:"avg_ms"`
	LastSeenUnix int64   `json:"last_seen_unix"`
}

// fleetTopology mirrors aggregator.Topology.
type fleetTopology struct {
	Nodes []fleetNode `json:"nodes"`
	Edges []fleetEdge `json:"edges"`
}

// fleetTraceSummary mirrors aggregator.TraceSummary.
type fleetTraceSummary struct {
	TraceID      string   `json:"trace_id"`
	Services     []string `json:"services"`
	RootService  string   `json:"root_service"`
	Route        string   `json:"route"`
	Method       string   `json:"method"`
	Status       int      `json:"status"`
	StartNanoUTC int64    `json:"start_ns"`
	DurationMs   float64  `json:"duration_ms"`
	SpanCount    int      `json:"span_count"`
	HasError     bool     `json:"has_error"`
	SkewFlagged  bool     `json:"skew_flagged,omitempty"`
}

// fleetTracePage mirrors aggregator.TracePage.
type fleetTracePage struct {
	Traces     []fleetTraceSummary `json:"traces"`
	NextCursor string              `json:"next_cursor,omitempty"`
	HasMore    bool                `json:"has_more"`
}

// fleetSpanNode mirrors aggregator.SpanNode, which embeds fleet.Span.
//
// The payload fields fleet.Span carries are deliberately omitted: a request body
// captured on every hop of a deep trace is far larger than everything else here
// combined, and an agent asking why a request failed does not need it inline. The
// span ids are reported, which is what it would need to go and fetch one.
type fleetSpanNode struct {
	TraceID      string            `json:"trace_id"`
	SpanID       string            `json:"span_id"`
	ParentSpanID string            `json:"parent_span_id,omitempty"`
	Service      string            `json:"service"`
	Route        string            `json:"route"`
	Method       string            `json:"method"`
	Status       int               `json:"status"`
	StartNanoUTC int64             `json:"start_ns"`
	DurationMs   float64           `json:"duration_ms"`
	Error        string            `json:"error,omitempty"`
	Tags         map[string]string `json:"tags,omitempty"`

	Children     []fleetSpanNode `json:"children,omitempty"`
	Orphan       bool            `json:"orphan,omitempty"`
	Skewed       bool            `json:"skewed,omitempty"`
	RootCause    bool            `json:"root_cause,omitempty"`
	DerivedError bool            `json:"derived_error,omitempty"`
}

// fleetTrace mirrors aggregator.Trace.
type fleetTrace struct {
	TraceID           string          `json:"trace_id"`
	Roots             []fleetSpanNode `json:"roots"`
	Services          []string        `json:"services"`
	SpanCount         int             `json:"span_count"`
	DurationMs        float64         `json:"duration_ms"`
	StartNanoUTC      int64           `json:"start_ns"`
	HasError          bool            `json:"has_error"`
	Status            int             `json:"status"`
	SkewFlagged       bool            `json:"skew_flagged"`
	OrphanCount       int             `json:"orphan_count"`
	SpansDropped      int             `json:"spans_dropped,omitempty"`
	RootCauseSpanID   string          `json:"root_cause_span_id,omitempty"`
	Summary           string          `json:"summary,omitempty"`
	FirstSeenUnixNano int64           `json:"first_seen_ns,omitempty"`
}

// fleetViolation mirrors contracts.Group, whose embedded Violation is flat in JSON.
type fleetViolation struct {
	TraceID   string `json:"trace_id"`
	SpanID    string `json:"span_id"`
	Caller    string `json:"caller"`
	Callee    string `json:"callee"`
	Route     string `json:"route"`
	Direction string `json:"direction"`
	Path      string `json:"path"`
	Rule      string `json:"rule"`
	Expected  string `json:"expected"`
	Observed  string `json:"observed"`
	Severity  string `json:"severity"`
	Timestamp int64  `json:"timestamp"`
	Count     int    `json:"count"`
	FirstSeen int64  `json:"first_seen"`
	LastSeen  int64  `json:"last_seen"`
}

// fleetAffected mirrors aggregator.AffectedService.
type fleetAffected struct {
	Service             string  `json:"service"`
	Hops                int     `json:"hops"`
	DependencyErrorRate float64 `json:"dependency_error_rate"`
	AttributedShare     float64 `json:"attributed_share"`
	Via                 string  `json:"via,omitempty"`
}

// fleetBlastRadius mirrors aggregator.BlastRadius.
type fleetBlastRadius struct {
	Service      string          `json:"service"`
	ErrorRate    float64         `json:"error_rate"`
	Calls        uint64          `json:"calls"`
	Affected     []fleetAffected `json:"affected"`
	Banner       string          `json:"banner"`
	ComputedUnix int64           `json:"computed_unix"`
}

// ─── get_topology ────────────────────────────────────────────────────────────

type topologyReport struct {
	AggregatorURL string      `json:"aggregator_url"`
	ServiceCount  int         `json:"service_count"`
	EdgeCount     int         `json:"edge_count"`
	EntryPoints   []string    `json:"entry_points"`
	Isolated      []string    `json:"isolated,omitempty"`
	Unhealthy     []string    `json:"unhealthy,omitempty"`
	Nodes         []fleetNode `json:"nodes"`
	Edges         []fleetEdge `json:"edges"`
	Notes         []string    `json:"notes,omitempty"`
}

func getTopologyTool() *tool {
	return &tool{
		name: "breeze_get_topology",
		description: "Read the live service graph from a Fleet aggregator: which services are " +
			"reporting, which calls which, and the per-service and per-edge call, error, and " +
			"latency numbers. This is the only view that spans services — a single service's " +
			"dashboard cannot show it.",
		schema: objectSchema(fleetProps(nil), "aggregator_url"),
		run: func(raw json.RawMessage) toolCallResult {
			var a fleetArgs
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}
			return getTopology(a)
		},
	}
}

func getTopology(a fleetArgs) toolCallResult {
	var topo fleetTopology
	if err := fetchLiveJSON(a.request("/topology", "fleet aggregator"), &topo); err != nil {
		return liveResultError("reading topology", err)
	}

	report := topologyReport{
		AggregatorURL: a.AggregatorURL,
		ServiceCount:  len(topo.Nodes),
		EdgeCount:     len(topo.Edges),
		Nodes:         topo.Nodes,
		Edges:         topo.Edges,
	}

	// A service with no edge in either direction is worth calling out. It means
	// either nothing calls it and it calls nothing — a genuinely standalone
	// service — or, far more often, that its callers are not instrumented, in
	// which case the graph is lying by omission and any conclusion drawn from
	// its shape is wrong.
	connected := make(map[string]bool, len(topo.Edges)*2)
	for _, e := range topo.Edges {
		connected[e.Caller] = true
		connected[e.Callee] = true
	}

	for _, n := range topo.Nodes {
		if n.Entry {
			report.EntryPoints = append(report.EntryPoints, n.Service)
		}
		if !connected[n.Service] {
			report.Isolated = append(report.Isolated, n.Service)
		}
		if n.Errors > 0 {
			report.Unhealthy = append(
				report.Unhealthy,
				fmt.Sprintf(
					"%s (%s of %d calls failing)",
					n.Service,
					formatRate(n.ErrorRate),
					n.Calls,
				),
			)
		}
	}
	sort.Strings(report.EntryPoints)
	sort.Strings(report.Isolated)

	if report.ServiceCount == 0 {
		report.Notes = append(
			report.Notes,
			"The aggregator is running but no service has reported yet. "+
				"A service appears here once it exports its first span or heartbeat, so this usually means "+
				"the services are not started, are not configured with this aggregator's URL, or are "+
				"rejecting on the ingest token.",
		)
	} else if report.EdgeCount == 0 {
		report.Notes = append(
			report.Notes,
			"Services are reporting but no call between them has been "+
				"observed. An edge is recorded from a span's parent, so this is expected when each service "+
				"is only receiving direct traffic, and a sign of missing trace propagation when they are "+
				"in fact calling each other.",
		)
	}
	if len(report.Isolated) > 0 {
		report.Notes = append(
			report.Notes,
			"Isolated services have no observed caller or callee. If "+
				"something does call them, the caller is not propagating trace context and the graph is "+
				"incomplete rather than sparse.",
		)
	}

	// Busiest first: on a graph of any size this is the order someone reading it
	// cares about, and it is stable across calls.
	sort.SliceStable(
		report.Nodes,
		func(i, j int) bool { return report.Nodes[i].Calls > report.Nodes[j].Calls },
	)
	sort.SliceStable(
		report.Edges,
		func(i, j int) bool { return report.Edges[i].Calls > report.Edges[j].Calls },
	)

	summary := fmt.Sprintf("%d service(s), %d call edge(s)", report.ServiceCount, report.EdgeCount)
	if len(report.Unhealthy) > 0 {
		summary += fmt.Sprintf(", %d with errors", len(report.Unhealthy))
	}
	return structuredResult(summary, report)
}

// ─── get_traces ──────────────────────────────────────────────────────────────

type tracesReport struct {
	AggregatorURL string              `json:"aggregator_url"`
	Count         int                 `json:"count"`
	Failing       int                 `json:"failing"`
	HasMore       bool                `json:"has_more"`
	NextCursor    string              `json:"next_cursor,omitempty"`
	SlowestMs     float64             `json:"slowest_ms"`
	Traces        []fleetTraceSummary `json:"traces"`
	Filters       map[string]string   `json:"filters_applied,omitempty"`
	Notes         []string            `json:"notes,omitempty"`
}

type tracesArgs struct {
	fleetArgs
	Service       string  `json:"service"`
	Status        string  `json:"status"`
	MinDurationMs float64 `json:"min_duration_ms"`
	MinServices   int     `json:"min_services"`
	Tag           string  `json:"tag"`
	Limit         int     `json:"limit"`
	Cursor        string  `json:"cursor"`
}

func getTracesTool() *tool {
	return &tool{
		name: "breeze_get_traces",
		description: "List recent distributed traces from a Fleet aggregator, newest first, with " +
			"optional filters on service, status, duration, span tag, and number of services " +
			"involved. Returns one summary line per request journey; use breeze_get_trace for " +
			"the span tree of a single one.",
		schema: objectSchema(fleetProps(map[string]any{
			"service": stringProp("Only traces involving this service."),
			"status": stringProp("Either an exact HTTP status such as 500, or \"error\" to match " +
				"any failing trace."),
			"min_duration_ms": map[string]any{
				"type":        "number",
				"description": "Only traces at least this slow, in milliseconds.",
			},
			"min_services": map[string]any{
				"type": "integer",
				"description": "Only traces spanning at least this many services. Defaults to 1 here, " +
					"meaning no filtering. Pass 2 to see only genuinely cross-service traces.",
			},
			"tag": stringProp("Match a span tag, written key:value."),
			"limit": map[string]any{
				"type":        "integer",
				"description": "Maximum traces to return. The aggregator caps this at 500 and defaults to 100.",
			},
			"cursor": stringProp("Opaque continuation token from a previous call's next_cursor, " +
				"for reading the next page."),
		}), "aggregator_url"),
		run: func(raw json.RawMessage) toolCallResult {
			var a tracesArgs
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}
			return getTraces(a)
		},
	}
}

func getTraces(a tracesArgs) toolCallResult {
	query := url.Values{}
	applied := map[string]string{}

	if s := strings.TrimSpace(a.Service); s != "" {
		query.Set("service", s)
		applied["service"] = s
	}
	if s := strings.TrimSpace(a.Status); s != "" {
		query.Set("status", s)
		applied["status"] = s
	}
	if a.MinDurationMs > 0 {
		v := strconv.FormatFloat(a.MinDurationMs, 'f', -1, 64)
		query.Set("min_duration_ms", v)
		applied["min_duration_ms"] = v
	}
	if s := strings.TrimSpace(a.Tag); s != "" {
		query.Set("tag", s)
		applied["tag"] = s
	}
	if a.Limit > 0 {
		query.Set("limit", strconv.Itoa(a.Limit))
		applied["limit"] = strconv.Itoa(a.Limit)
	}
	if s := strings.TrimSpace(a.Cursor); s != "" {
		query.Set("cursor", s)
	}

	// Sent explicitly on every call, and defaulted to 1 rather than left unset.
	//
	// The endpoint's own default is 2, because its first consumer was a
	// cross-service trace view where single-service traces are noise. That
	// default is a trap for a caller that has not read the handler: ask for the
	// traces of a two-service system where one service is down, and every
	// remaining trace has one service and the answer is an empty list. Being
	// told "no traces" when there are thousands is the worst possible failure
	// here, so the filter is opt-in from this tool.
	minServices := a.MinServices
	if minServices <= 0 {
		minServices = 1
	}
	query.Set("min_services", strconv.Itoa(minServices))
	if a.MinServices > 0 {
		applied["min_services"] = strconv.Itoa(minServices)
	}

	req := a.request("/traces?"+query.Encode(), "fleet aggregator")

	// The endpoint returns a bare array normally and a {traces, next_cursor,
	// has_more} envelope when paginating, so the shape is decided by the request.
	// Decoding by looking at the first byte handles both without asking the
	// caller to know which they are getting.
	var raw json.RawMessage
	if err := fetchLiveJSON(req, &raw); err != nil {
		return liveResultError("reading traces", err)
	}

	page, decodeErr := decodeTracePage(raw)
	if decodeErr != nil {
		return structuredErrorResult("reading traces: "+decodeErr.Error(), map[string]any{
			"error":  "malformed",
			"detail": decodeErr.Error(),
		})
	}

	report := tracesReport{
		AggregatorURL: a.AggregatorURL,
		Count:         len(page.Traces),
		HasMore:       page.HasMore,
		NextCursor:    page.NextCursor,
		Traces:        page.Traces,
	}
	if len(applied) > 0 {
		report.Filters = applied
	}
	for _, t := range page.Traces {
		if t.HasError {
			report.Failing++
		}
		if t.DurationMs > report.SlowestMs {
			report.SlowestMs = t.DurationMs
		}
	}

	if report.Count == 0 {
		note := "No trace matched. "
		if len(applied) == 0 {
			note += "With no filters applied this means the aggregator has received no spans — check " +
				"that the services are running, are pointed at this aggregator, and share its ingest token."
		} else {
			note += "Filters were applied, so this may mean the filters excluded everything rather than " +
				"that there is no data. Retry with no filters to tell the two apart."
		}
		report.Notes = append(report.Notes, note)
	}
	if minServices > 1 {
		report.Notes = append(
			report.Notes,
			fmt.Sprintf("Only traces spanning at least %d services were "+
				"considered; single-service traces were excluded.", minServices),
		)
	}

	summary := fmt.Sprintf("%d trace(s), %d failing", report.Count, report.Failing)
	if report.HasMore {
		summary += " (more available)"
	}
	return structuredResult(summary, report)
}

// decodeTracePage accepts either response shape of GET /api/traces.
func decodeTracePage(raw json.RawMessage) (fleetTracePage, error) {
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "[") {
		var list []fleetTraceSummary
		if err := json.Unmarshal(raw, &list); err != nil {
			return fleetTracePage{}, fmt.Errorf("trace list is not the expected array: %w", err)
		}
		return fleetTracePage{Traces: list}, nil
	}
	var page fleetTracePage
	if err := json.Unmarshal(raw, &page); err != nil {
		return fleetTracePage{}, fmt.Errorf(
			"trace page is neither an array nor a page envelope: %w",
			err,
		)
	}
	return page, nil
}

// ─── get_trace ───────────────────────────────────────────────────────────────

// flatSpan is one span with its depth, for reading a tree as a list.
//
// The tree is kept in the raw trace, but a nested structure is awkward to reason
// about in a transcript: finding the deepest failing span means walking it. The
// flattened form is in traversal order with an explicit depth, so the shape is
// still readable and the ordering answers "what happened next".
type flatSpan struct {
	Depth        int     `json:"depth"`
	Service      string  `json:"service"`
	Method       string  `json:"method"`
	Route        string  `json:"route"`
	Status       int     `json:"status"`
	DurationMs   float64 `json:"duration_ms"`
	SpanID       string  `json:"span_id"`
	Error        string  `json:"error,omitempty"`
	RootCause    bool    `json:"root_cause,omitempty"`
	DerivedError bool    `json:"derived_error,omitempty"`
	Orphan       bool    `json:"orphan,omitempty"`
	Skewed       bool    `json:"skewed,omitempty"`
}

type traceReport struct {
	AggregatorURL   string     `json:"aggregator_url"`
	TraceID         string     `json:"trace_id"`
	Services        []string   `json:"services"`
	SpanCount       int        `json:"span_count"`
	DurationMs      float64    `json:"duration_ms"`
	Status          int        `json:"status"`
	HasError        bool       `json:"has_error"`
	RootCauseSpanID string     `json:"root_cause_span_id,omitempty"`
	RootCause       *flatSpan  `json:"root_cause,omitempty"`
	Summary         string     `json:"summary,omitempty"`
	Spans           []flatSpan `json:"spans"`
	SlowestSpan     *flatSpan  `json:"slowest_span,omitempty"`
	OrphanCount     int        `json:"orphan_count"`
	SpansDropped    int        `json:"spans_dropped,omitempty"`
	SkewFlagged     bool       `json:"skew_flagged"`
	Notes           []string   `json:"notes,omitempty"`
}

type traceArgs struct {
	fleetArgs
	TraceID string `json:"trace_id"`
}

func getTraceTool() *tool {
	return &tool{
		name: "breeze_get_trace",
		description: "Read one distributed trace in full from a Fleet aggregator: every span in " +
			"traversal order with its service, route, status, and duration, plus the root-cause " +
			"span the aggregator identified and its plain-language summary. Use this after " +
			"breeze_get_traces has narrowed down which request to look at.",
		schema: objectSchema(fleetProps(map[string]any{
			"trace_id": stringProp(
				"The 32-character hex trace id, as returned by breeze_get_traces.",
			),
		}), "aggregator_url", "trace_id"),
		run: func(raw json.RawMessage) toolCallResult {
			var a traceArgs
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}
			return getTrace(a)
		},
	}
}

func getTrace(a traceArgs) toolCallResult {
	id, idErr := validateTraceID(a.TraceID)
	if idErr != nil {
		return errorResult(idErr.Error())
	}

	trace, err := fetchTrace(a.fleetArgs, id)
	if err != nil {
		return liveResultError("reading trace", err)
	}

	report := buildTraceReport(a.AggregatorURL, trace)
	summary := fmt.Sprintf("trace %s: %d span(s) across %d service(s), %.1fms",
		report.TraceID, report.SpanCount, len(report.Services), report.DurationMs)
	if report.HasError {
		summary += ", failed"
	}
	return structuredResult(summary, report)
}

// validateTraceID rejects locally what the aggregator would reject with a bare 400.
func validateTraceID(raw string) (string, error) {
	id := strings.TrimSpace(raw)
	if id == "" {
		return "", fmt.Errorf("a trace_id is required; breeze_get_traces lists them")
	}
	if len(id) != traceIDLength {
		return "", fmt.Errorf("trace_id %q is %d characters; a trace id is %d hex characters. "+
			"A %d-character value is a span id, which identifies one hop rather than the whole journey",
			id, len(id), traceIDLength, traceIDLength/2)
	}
	for _, r := range id {
		isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
		if !isHex {
			return "", fmt.Errorf("trace_id %q contains %q, which is not a hex digit", id, r)
		}
	}
	return id, nil
}

// fetchTrace reads one assembled trace.
//
// Shared by breeze_get_trace and breeze_explain_incident so both agree on what a
// missing trace means; the 404 override is the whole point.
func fetchTrace(a fleetArgs, id string) (fleetTrace, *liveError) {
	req := a.request("/traces/"+id, "fleet aggregator")
	req.notFound = fmt.Sprintf("The aggregator has no trace %s. Traces are held in a bounded, "+
		"time-limited store, so the usual cause is that this one has aged out or been evicted by newer "+
		"traffic rather than that it never existed. This says nothing about whether Fleet is installed — "+
		"breeze_get_topology confirms that separately.", id)

	var trace fleetTrace
	if err := fetchLiveJSON(req, &trace); err != nil {
		return fleetTrace{}, err
	}
	return trace, nil
}

// buildTraceReport flattens an assembled trace into the report shape.
func buildTraceReport(aggregatorURL string, trace fleetTrace) traceReport {
	report := traceReport{
		AggregatorURL:   aggregatorURL,
		TraceID:         trace.TraceID,
		Services:        trace.Services,
		SpanCount:       trace.SpanCount,
		DurationMs:      trace.DurationMs,
		Status:          trace.Status,
		HasError:        trace.HasError,
		RootCauseSpanID: trace.RootCauseSpanID,
		Summary:         trace.Summary,
		OrphanCount:     trace.OrphanCount,
		SpansDropped:    trace.SpansDropped,
		SkewFlagged:     trace.SkewFlagged,
		Spans:           flattenSpans(trace.Roots, 0, nil),
	}

	for i := range report.Spans {
		span := report.Spans[i]
		if span.RootCause {
			report.RootCause = &report.Spans[i]
		}
		if report.SlowestSpan == nil || span.DurationMs > report.SlowestSpan.DurationMs {
			report.SlowestSpan = &report.Spans[i]
		}
	}

	if report.OrphanCount > 0 {
		report.Notes = append(
			report.Notes,
			fmt.Sprintf("%d span(s) could not be linked to a parent and "+
				"were re-rooted. The tree is incomplete: a service crashed before exporting, sampled "+
				"differently, or is not instrumented. Gaps in the shape below are missing data, not idle time.",
				report.OrphanCount),
		)
	}
	if report.SpansDropped > 0 {
		report.Notes = append(
			report.Notes,
			fmt.Sprintf("%d span(s) were dropped by the aggregator's "+
				"per-trace limit, so this trace is truncated.", report.SpansDropped),
		)
	}
	if report.SkewFlagged {
		report.Notes = append(
			report.Notes,
			"At least one span started before its parent, which is "+
				"impossible and means the reporting machines' clocks disagree. Read the durations, which are "+
				"measured locally, rather than comparing absolute start times across services.",
		)
	}
	if report.HasError && report.RootCauseSpanID == "" {
		report.Notes = append(report.Notes, "The trace failed but no single root-cause span was "+
			"identified, which happens when the failing span's own parent was never reported.")
	}
	if len(trace.Roots) > 1 {
		report.Notes = append(
			report.Notes,
			fmt.Sprintf("This trace has %d root spans. Either it had "+
				"several entry points, or — more often — some spans' parents were never reported.",
				len(trace.Roots)),
		)
	}

	return report
}

// flattenSpans walks the tree depth-first, preserving sibling order.
func flattenSpans(nodes []fleetSpanNode, depth int, out []flatSpan) []flatSpan {
	for _, n := range nodes {
		out = append(out, flatSpan{
			Depth:        depth,
			Service:      n.Service,
			Method:       n.Method,
			Route:        n.Route,
			Status:       n.Status,
			DurationMs:   n.DurationMs,
			SpanID:       n.SpanID,
			Error:        n.Error,
			RootCause:    n.RootCause,
			DerivedError: n.DerivedError,
			Orphan:       n.Orphan,
			Skewed:       n.Skewed,
		})
		out = flattenSpans(n.Children, depth+1, out)
	}
	return out
}

// ─── get_contract_violations ─────────────────────────────────────────────────

type violationsReport struct {
	AggregatorURL string           `json:"aggregator_url"`
	Count         int              `json:"count"`
	TotalOccur    int              `json:"total_occurrences"`
	BySeverity    map[string]int   `json:"by_severity,omitempty"`
	ByPair        map[string]int   `json:"by_service_pair,omitempty"`
	Violations    []fleetViolation `json:"violations"`
	Notes         []string         `json:"notes,omitempty"`
}

type violationsArgs struct {
	fleetArgs
	Service  string `json:"service"`
	Severity string `json:"severity"`
	Rule     string `json:"rule"`
	Limit    int    `json:"limit"`
}

func getContractViolationsTool() *tool {
	return &tool{
		name: "breeze_get_contract_violations",
		description: "List contract violations the Fleet aggregator has observed: places where one " +
			"service sent or returned a payload that did not match the schema the other side " +
			"published. Each entry is a group with an occurrence count, so a single mismatch " +
			"repeated a thousand times reads as one finding. Optionally filter by service, " +
			"severity, or rule.",
		schema: objectSchema(fleetProps(map[string]any{
			"service": stringProp(
				"Only violations where this service is the caller or the callee.",
			),
			"severity": stringProp("Only violations of this severity, as reported by the checker " +
				"(for example error or warning)."),
			"rule": stringProp("Only violations of rules whose name contains this text."),
			"limit": map[string]any{
				"type":        "integer",
				"description": "Maximum groups to return after filtering.",
			},
		}), "aggregator_url"),
		run: func(raw json.RawMessage) toolCallResult {
			var a violationsArgs
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}
			return getContractViolations(a)
		},
	}
}

func getContractViolations(a violationsArgs) toolCallResult {
	groups, err := fetchViolations(a.fleetArgs)
	if err != nil {
		return liveResultError("reading contract violations", err)
	}

	// Filtered here rather than in the request because the endpoint takes no
	// query parameters: it returns the whole ring. Doing it locally is also what
	// lets the report say how many were filtered out, which "no results" alone
	// could not distinguish from "nothing recorded".
	service := strings.TrimSpace(a.Service)
	severity := strings.TrimSpace(a.Severity)
	rule := strings.ToLower(strings.TrimSpace(a.Rule))

	report := violationsReport{
		AggregatorURL: a.AggregatorURL,
		BySeverity:    map[string]int{},
		ByPair:        map[string]int{},
		Violations:    []fleetViolation{},
	}

	matched := 0
	for _, g := range groups {
		if service != "" && g.Caller != service && g.Callee != service {
			continue
		}
		if severity != "" && !strings.EqualFold(g.Severity, severity) {
			continue
		}
		if rule != "" && !strings.Contains(strings.ToLower(g.Rule), rule) {
			continue
		}
		matched++
		if a.Limit > 0 && len(report.Violations) >= a.Limit {
			continue
		}
		report.Violations = append(report.Violations, g)
		report.TotalOccur += g.Count
		report.BySeverity[g.Severity]++
		report.ByPair[g.Caller+" -> "+g.Callee]++
	}
	report.Count = len(report.Violations)

	// Most frequent first: occurrence count is the closest thing available to
	// impact, and a mismatch seen once may be a single malformed request while
	// one seen ten thousand times is a broken contract.
	sort.SliceStable(report.Violations, func(i, j int) bool {
		return report.Violations[i].Count > report.Violations[j].Count
	})

	if len(groups) == 0 {
		report.Notes = append(
			report.Notes,
			"No violation has been recorded. Contract checking compares "+
				"observed payloads against the schemas services publish in their heartbeats, so an empty "+
				"list means either that everything matched or that payload capture and schema publishing "+
				"are not both enabled.",
		)
	} else if report.Count == 0 {
		report.Notes = append(
			report.Notes,
			fmt.Sprintf("%d violation group(s) are recorded but none "+
				"matched the filters given.", len(groups)),
		)
	}
	if matched > report.Count {
		report.Notes = append(
			report.Notes,
			fmt.Sprintf("%d group(s) matched; %d shown because of the "+
				"limit argument.", matched, report.Count),
		)
	}
	if len(report.BySeverity) == 0 {
		report.BySeverity = nil
	}
	if len(report.ByPair) == 0 {
		report.ByPair = nil
	}

	summary := fmt.Sprintf("%d violation group(s), %d occurrence(s) in total",
		report.Count, report.TotalOccur)
	return structuredResult(summary, report)
}

// fetchViolations reads the violation ring. Shared with breeze_explain_incident.
func fetchViolations(a fleetArgs) ([]fleetViolation, *liveError) {
	var groups []fleetViolation
	if err := fetchLiveJSON(a.request("/violations", "fleet aggregator"), &groups); err != nil {
		return nil, err
	}
	return groups, nil
}

// fetchIncidents reads the computed blast radii. Shared with breeze_explain_incident.
func fetchIncidents(a fleetArgs) ([]fleetBlastRadius, *liveError) {
	var incidents []fleetBlastRadius
	if err := fetchLiveJSON(a.request("/incidents", "fleet aggregator"), &incidents); err != nil {
		return nil, err
	}
	return incidents, nil
}

// ─── explain_incident ────────────────────────────────────────────────────────

type incidentReport struct {
	AggregatorURL string `json:"aggregator_url"`
	TraceID       string `json:"trace_id"`

	// Explanation is the assembled narrative: what failed, where, what it took
	// down, and whether a contract mismatch accounts for it.
	Explanation string `json:"explanation"`

	Trace traceReport `json:"trace"`

	// Incidents are the aggregator's own blast-radius computations for the
	// services this trace touched. Not recomputed here.
	Incidents []fleetBlastRadius `json:"incidents"`

	// RelatedViolations are contract violations on this exact trace; Nearby are
	// ones between the same services on other traces.
	RelatedViolations []fleetViolation `json:"related_violations"`
	NearbyViolations  []fleetViolation `json:"nearby_violations,omitempty"`

	AffectedServices []string `json:"affected_services,omitempty"`
	Notes            []string `json:"notes,omitempty"`
}

type incidentArgs struct {
	fleetArgs
	TraceID string `json:"trace_id"`
}

func explainIncidentTool() *tool {
	return &tool{
		name: "breeze_explain_incident",
		description: "Explain one failing distributed trace by joining everything the Fleet " +
			"aggregator already knows about it: the root-cause span it identified, the blast " +
			"radius of the services involved, and any contract violation that would account for " +
			"the failure. Returns a plain-language explanation plus the underlying evidence. " +
			"This performs no analysis of its own — every conclusion is the aggregator's, read " +
			"from the same endpoints the dashboard uses, so the two cannot disagree.",
		schema: objectSchema(fleetProps(map[string]any{
			"trace_id": stringProp("The 32-character hex id of the failing trace to explain."),
		}), "aggregator_url", "trace_id"),
		run: func(raw json.RawMessage) toolCallResult {
			var a incidentArgs
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}
			return explainIncident(a)
		},
	}
}

func explainIncident(a incidentArgs) toolCallResult {
	id, idErr := validateTraceID(a.TraceID)
	if idErr != nil {
		return errorResult(idErr.Error())
	}

	// The trace is required; the other two are supporting evidence. A failure to
	// read them is reported as a note rather than aborting the whole
	// explanation, because "the root cause was X but blast radius was
	// unavailable" is a useful answer and no answer at all is not.
	trace, traceErr := fetchTrace(a.fleetArgs, id)
	if traceErr != nil {
		return liveResultError("explaining incident", traceErr)
	}

	report := incidentReport{
		AggregatorURL:     a.AggregatorURL,
		TraceID:           trace.TraceID,
		Trace:             buildTraceReport(a.AggregatorURL, trace),
		Incidents:         []fleetBlastRadius{},
		RelatedViolations: []fleetViolation{},
	}

	inTrace := make(map[string]bool, len(trace.Services))
	for _, s := range trace.Services {
		inTrace[s] = true
	}

	affected := map[string]bool{}
	if incidents, err := fetchIncidents(a.fleetArgs); err != nil {
		report.Notes = append(report.Notes, "Blast radius could not be read ("+err.Message+
			"), so this explanation covers the trace only and may understate the impact.")
	} else {
		for _, inc := range incidents {
			relevant := inTrace[inc.Service]
			for _, af := range inc.Affected {
				if inTrace[af.Service] {
					relevant = true
				}
			}
			if !relevant {
				continue
			}
			report.Incidents = append(report.Incidents, inc)
			for _, af := range inc.Affected {
				affected[af.Service] = true
			}
		}
	}
	for s := range affected {
		report.AffectedServices = append(report.AffectedServices, s)
	}
	sort.Strings(report.AffectedServices)

	if groups, err := fetchViolations(a.fleetArgs); err != nil {
		report.Notes = append(report.Notes, "Contract violations could not be read ("+err.Message+
			"), so a schema mismatch cannot be ruled in or out as the cause.")
	} else {
		for _, g := range groups {
			switch {
			case g.TraceID == trace.TraceID:
				report.RelatedViolations = append(report.RelatedViolations, g)
			case inTrace[g.Caller] && inTrace[g.Callee]:
				// Between the same two services but recorded against another
				// trace. Kept separate from the direct matches: it is a lead,
				// not evidence, because violations are grouped and the stored
				// trace id is whichever occurrence was seen most recently.
				report.NearbyViolations = append(report.NearbyViolations, g)
			}
		}
	}

	report.Explanation = buildIncidentExplanation(report)

	summary := fmt.Sprintf("trace %s: %s", report.TraceID, firstSentence(report.Explanation))
	return structuredResult(summary, report)
}

// buildIncidentExplanation assembles the narrative from facts already gathered.
//
// Deterministic string building over values the aggregator computed — the same
// approach as the aggregator's own trace summary. No inference happens here; if a
// fact was not reported, the sentence that would have used it is not written.
func buildIncidentExplanation(r incidentReport) string {
	var b strings.Builder

	switch {
	case !r.Trace.HasError:
		fmt.Fprintf(&b, "This trace did not fail: it returned %d across %d service(s) in %.1fms. ",
			r.Trace.Status, len(r.Trace.Services), r.Trace.DurationMs)
		if r.Trace.SlowestSpan != nil {
			fmt.Fprintf(&b, "Its slowest hop was %s %s in %s at %.1fms. ",
				r.Trace.SlowestSpan.Method, r.Trace.SlowestSpan.Route,
				r.Trace.SlowestSpan.Service, r.Trace.SlowestSpan.DurationMs)
		}

	case r.Trace.RootCause != nil:
		cause := r.Trace.RootCause
		fmt.Fprintf(&b, "The failure originated in %s, at %s %s, which returned %d",
			cause.Service, cause.Method, cause.Route, cause.Status)
		if cause.Error != "" {
			fmt.Fprintf(&b, " with the error %q", cause.Error)
		}
		fmt.Fprintf(&b, " after %.1fms. ", cause.DurationMs)

		derived := 0
		for _, s := range r.Trace.Spans {
			if s.DerivedError {
				derived++
			}
		}
		if derived > 0 {
			fmt.Fprintf(&b, "%d other span(s) in this trace also report an error; the aggregator "+
				"identified them as consequences of that failure rather than separate faults. ", derived)
		}

	default:
		fmt.Fprintf(
			&b,
			"This trace failed with status %d, but the aggregator could not identify a "+
				"single originating span. ",
			r.Trace.Status,
		)
	}

	if r.Trace.Summary != "" {
		b.WriteString("The aggregator's own summary reads: ")
		b.WriteString(r.Trace.Summary)
		b.WriteString(" ")
	}

	switch len(r.Incidents) {
	case 0:
		if r.Trace.HasError {
			b.WriteString(
				"No fleet-level incident is open for the services involved, so this looks " +
					"like an isolated failure rather than an ongoing outage — a service must exceed its " +
					"error-rate threshold over a minimum number of calls before an incident is raised. ",
			)
		}
	default:
		for _, inc := range r.Incidents {
			fmt.Fprintf(&b, "Fleet-wide, %s is currently failing %s of %d call(s). ",
				inc.Service, formatRate(inc.ErrorRate), inc.Calls)
			if len(inc.Affected) > 0 {
				names := make([]string, 0, len(inc.Affected))
				for _, af := range inc.Affected {
					names = append(names, fmt.Sprintf("%s (%s of its traffic, %d hop(s) away)",
						af.Service, formatRate(af.AttributedShare), af.Hops))
				}
				fmt.Fprintf(&b, "The aggregator attributes failures in %s to it. ",
					strings.Join(names, ", "))
			}
		}
	}

	if n := len(r.RelatedViolations); n > 0 {
		v := r.RelatedViolations[0]
		fmt.Fprintf(
			&b,
			"A contract violation was recorded on this very trace: %s expected %s at %s "+
				"from %s but observed %s (rule %s). That mismatch is the most likely explanation, and it is "+
				"a code-level fix rather than an operational one. ",
			v.Caller,
			v.Expected,
			v.Path,
			v.Callee,
			v.Observed,
			v.Rule,
		)
		if n > 1 {
			fmt.Fprintf(&b, "%d further violation(s) were recorded on this trace. ", n-1)
		}
	} else if len(r.NearbyViolations) > 0 {
		v := r.NearbyViolations[0]
		fmt.Fprintf(
			&b,
			"No contract violation was recorded on this trace, but %s and %s have violated "+
				"rule %s at %s on other traces, which is worth ruling out. ",
			v.Caller,
			v.Callee,
			v.Rule,
			v.Path,
		)
	} else if r.Trace.HasError {
		b.WriteString(
			"No contract violation involves these services, so the payloads matched the " +
				"published schemas and the failure is in behaviour rather than shape. ",
		)
	}

	if r.Trace.OrphanCount > 0 {
		fmt.Fprintf(&b, "Read this with the caveat that %d span(s) had no reported parent, so the "+
			"trace is incomplete and the true origin may be in a service that never reported. ",
			r.Trace.OrphanCount)
	}

	return strings.TrimSpace(b.String())
}

// formatRate renders a 0..1 error rate as a percentage.
func formatRate(rate float64) string {
	return strconv.FormatFloat(rate*100, 'f', 1, 64) + "%"
}

// firstSentence shortens the explanation for a one-line summary.
func firstSentence(text string) string {
	if idx := strings.Index(text, ". "); idx >= 0 {
		return text[:idx+1]
	}
	return text
}
