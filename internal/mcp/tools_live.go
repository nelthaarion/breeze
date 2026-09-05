package mcp

// tools_live.go — the tools that read a running service's dashboard.
//
// These answer questions no amount of source reading can: which routes have
// actually been hit, what the latency looks like now, what failed in the last
// minute, what the logs say for one trace. An agent debugging a live problem is
// otherwise reduced to asking a person to paste terminal output.
//
// Two decisions run through the whole file.
//
// First, the response types are declared here rather than imported from package
// dashboard. That is deliberate and it is structural, not stylistic: package
// dashboard imports the root breeze package, and Part 4 has the root breeze
// package importing this one to expose routes as MCP tools. Importing dashboard
// here would close that loop into an import cycle. Declaring the wire shapes
// locally also means a field added to the dashboard's internals does not
// silently change this tool's output contract — the coupling is to the JSON,
// which is the actual interface, and there is a test asserting the two agree.
//
// Second, every tool takes base_path. The dashboard's mount point is
// configurable, and a tool that hardcoded /dashboard would report "not
// installed" for a service that simply mounted it elsewhere — the single most
// misleading answer available.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/nelthaarion/breeze/v2/diag"
)

func registerLiveTools(s *Server) {
	s.addTool(getRoutesTool())
	s.addTool(getPerformanceTool())
	s.addTool(getRecentErrorsTool())
	s.addTool(getLogsTool())
	s.addTool(queryOpenAPITool())
	s.addTool(diagnoseServiceTool())
}

// defaultDashboardBase matches dashboard.DefaultConfig's BasePath.
//
// Duplicated rather than imported for the cycle reason above. A test asserts it
// still matches the dashboard's own default, so a change there fails here
// instead of silently sending every request to the wrong path.
const defaultDashboardBase = "/dashboard"

// liveArgs is the argument set every dashboard tool shares.
type liveArgs struct {
	ServiceURL string `json:"service_url"`
	BasePath   string `json:"base_path"`
	Token      string `json:"token"`
	Username   string `json:"username"`
	Password   string `json:"password"`
}

// request builds the transport call for one dashboard API path.
func (a liveArgs) request(apiPath, feature string) liveRequest {
	base := strings.TrimSpace(a.BasePath)
	if base == "" {
		base = defaultDashboardBase
	}
	base = "/" + strings.Trim(base, "/")

	return liveRequest{
		baseURL:  a.ServiceURL,
		path:     base + "/api" + apiPath,
		token:    a.Token,
		username: a.Username,
		password: a.Password,
		feature:  feature,
	}
}

// liveProps returns the shared schema properties.
//
// Built by a function so the five tools cannot drift apart: an argument renamed
// in one and not the others would be a silent trap for a caller that reasonably
// expects them to be uniform.
func liveProps(extra map[string]any) map[string]any {
	props := map[string]any{
		"service_url": stringProp("Base URL of the running service, e.g. http://127.0.0.1:8080. " +
			"A bare host:port is accepted."),
		"base_path": stringProp("Dashboard mount point. Defaults to " + defaultDashboardBase +
			"; pass the configured value if the service mounts it elsewhere."),
		"token": stringProp("Service token, sent as X-Fleet-Token. Accepted by the logs endpoint."),
		"username": stringProp("Dashboard username, sent as HTTP Basic. Needed when the " +
			"dashboard has auth enabled."),
		"password": stringProp("Dashboard password, sent as HTTP Basic."),
	}
	for k, v := range extra {
		props[k] = v
	}
	return props
}

// ─── get_routes ──────────────────────────────────────────────────────────────

// liveRoute mirrors dashboard.RouteStat's JSON.
type liveRoute struct {
	Method       string   `json:"method"`
	Pattern      string   `json:"pattern"`
	Controller   string   `json:"controller"`
	Middleware   []string `json:"middleware"`
	Requests     int64    `json:"requests"`
	AvgLatencyMS float64  `json:"avg_latency_ms"`
	MaxLatencyMS float64  `json:"max_latency_ms"`
	LastRequest  string   `json:"last_request"`
	Errors       int64    `json:"errors"`

	// Summary, Description and Tags are what the route's author wrote in
	// middleware.Doc. They are the reason this tool is worth calling rather than
	// reading the source: a method and a path do not say which endpoint to use,
	// and the sentence that does say so already exists.
	Summary     string   `json:"summary,omitempty"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`

	// Documented is false for a route with no Doc wrapper, which means it is
	// absent from the OpenAPI document — so an agent that consulted the document
	// first would conclude the endpoint does not exist.
	Documented bool `json:"documented"`
}

// routesReport is what the tool returns.
//
// The raw endpoint returns a bare array. A bare array is a poor tool result: it
// has nowhere to put the counts a caller needs in order to decide what to do
// next, and no way to say "the dashboard is installed but nothing has been
// called yet" — which is a different situation from an empty service.
type routesReport struct {
	ServiceURL string      `json:"service_url"`
	Total      int         `json:"total"`
	Exercised  int         `json:"exercised"`
	Unused     int         `json:"unused"`
	WithErrors int         `json:"with_errors"`
	Documented int         `json:"documented"`
	Routes     []liveRoute `json:"routes"`
	Notes      []string    `json:"notes,omitempty"`
}

func getRoutesTool() *tool {
	return &tool{
		name: "breeze_get_routes",
		description: "List the routes a running service is serving, with each route's own summary and " +
			"description as its author wrote them, plus live request counts, average and maximum " +
			"latency, and error counts. Reads the dashboard's routes endpoint, so it reflects what " +
			"the process is actually doing rather than what the source says. Reports which routes " +
			"carry no documentation — those are also absent from the OpenAPI document. Requires " +
			"the dashboard feature.",
		schema: objectSchema(liveProps(nil), "service_url"),
		run: func(raw json.RawMessage) toolCallResult {
			var a liveArgs
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}
			return getRoutes(a)
		},
	}
}

func getRoutes(a liveArgs) toolCallResult {
	var routes []liveRoute
	if err := fetchLiveJSON(a.request("/routes", "dashboard"), &routes); err != nil {
		return liveResultError("reading routes", err)
	}

	report := routesReport{ServiceURL: a.ServiceURL, Total: len(routes), Routes: routes}
	for _, r := range routes {
		if r.Requests > 0 {
			report.Exercised++
		} else {
			report.Unused++
		}
		if r.Errors > 0 {
			report.WithErrors++
		}
		if r.Documented {
			report.Documented++
		}
	}

	// Said explicitly, because a listing of zeroes has two causes and the more
	// surprising one is not "no traffic".
	//
	// The dashboard middleware only accumulates per-route statistics for
	// requests it captures in full, and to keep the hot path allocation-free it
	// captures only when someone has the dashboard open, when the request was
	// slower than SlowRequestMs, or when it failed. A service that has served
	// ten thousand fast, successful requests with nobody watching therefore
	// reports every route at zero. An agent that did not know this would report
	// a healthy service as dead code, so the tool says it rather than leaving
	// the caller to infer it.
	if report.Total > 0 && report.Exercised == 0 {
		report.Notes = append(
			report.Notes,
			"Every route reports zero requests. That may mean no traffic, "+
				"but per-route statistics are only accumulated for requests the dashboard captures in full — "+
				"those served while someone has the dashboard open, those slower than the configured "+
				"slow_request_ms, and those that failed. Fast successful requests served with nobody watching "+
				"are counted in the totals but not attributed to a route. Treat zero here as 'not measured', "+
				"not as 'never called'.",
		)
	}

	// Slowest first — the ordering someone asking about performance wants.
	sort.SliceStable(report.Routes, func(i, j int) bool {
		return report.Routes[i].AvgLatencyMS > report.Routes[j].AvgLatencyMS
	})

	// An undocumented route is not merely undescribed: it is absent from the
	// OpenAPI document, so an agent that read the document first was told the
	// endpoint does not exist. Worth saying, because the fix is a one-line
	// wrapper and nothing else in the framework reports it.
	if undocumented := report.Total - report.Documented; undocumented > 0 {
		report.Notes = append(report.Notes, fmt.Sprintf("%d of %d route(s) carry no "+
			"middleware.Doc wrapper, so they have no summary here and do not appear in this "+
			"service's OpenAPI document at all — breeze_query_openapi will not find them. Their "+
			"method and path are still accurate; only the description is missing.",
			undocumented, report.Total))
	}

	summary := fmt.Sprintf(
		"%d route(s): %d exercised, %d never called, %d with errors, %d documented",
		report.Total,
		report.Exercised,
		report.Unused,
		report.WithErrors,
		report.Documented,
	)
	return structuredResult(summary, report)
}

// ─── get_performance ─────────────────────────────────────────────────────────

// livePerformance is the subset of the performance endpoint's "current" object
// this tool reports.
//
// Deliberately a subset. The endpoint returns every runtime counter Go exposes;
// forwarding all of it wholesale would bury the handful of numbers that answer
// "is this service healthy" under a hundred that do not.
type livePerformance struct {
	Goroutines int `json:"goroutines"`
	Heap       struct {
		Alloc      uint64 `json:"alloc"`
		TotalAlloc uint64 `json:"total_alloc"`
		Sys        uint64 `json:"sys"`
		Objects    uint64 `json:"objects"`
	} `json:"heap"`
	Stack struct {
		InUse uint64 `json:"in_use"`
	} `json:"stack"`
	GC struct {
		NumGC        uint32  `json:"num_gc"`
		PauseTotalNS uint64  `json:"pause_total_ns"`
		PauseNS      uint64  `json:"pause_ns"`
		CPUFraction  float64 `json:"cpu_fraction"`
	} `json:"gc"`
	Memory struct {
		Sys       uint64  `json:"sys"`
		HeapInUse uint64  `json:"heap_in_use"`
		UsagePct  float64 `json:"usage_pct"`
	} `json:"memory"`
	CPU struct {
		NumCPU     int     `json:"num_cpu"`
		GOMAXPROCS int     `json:"gomaxprocs"`
		UsagePct   float64 `json:"usage_pct"`
	} `json:"cpu"`
	RuntimeTuning struct {
		GOGC       int   `json:"gogc"`
		GOMEMLIMIT int64 `json:"gomemlimit"`
	} `json:"runtime_tuning"`
}

// performanceEnvelope is the endpoint's own {current, history} wrapper.
type performanceEnvelope struct {
	Current livePerformance `json:"current"`
	History []struct {
		Goroutines int    `json:"goroutines"`
		HeapAlloc  uint64 `json:"heap_alloc"`
		NumGC      uint32 `json:"num_gc"`
	} `json:"history"`
}

type performanceReport struct {
	ServiceURL    string          `json:"service_url"`
	Current       livePerformance `json:"current"`
	HistoryPoints int             `json:"history_points"`
	Observations  []string        `json:"observations,omitempty"`
}

func getPerformanceTool() *tool {
	return &tool{
		name: "breeze_get_performance",
		description: "Read a running service's current runtime performance: goroutine count, heap and " +
			"stack usage, GC pause and CPU fraction, memory from the OS, and the GOGC/GOMEMLIMIT " +
			"settings actually in effect. Flags goroutine growth and GC pressure. Requires the " +
			"dashboard feature.",
		schema: objectSchema(liveProps(nil), "service_url"),
		run: func(raw json.RawMessage) toolCallResult {
			var a liveArgs
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}
			return getPerformance(a)
		},
	}
}

func getPerformance(a liveArgs) toolCallResult {
	var env performanceEnvelope
	if err := fetchLiveJSON(a.request("/performance", "dashboard"), &env); err != nil {
		return liveResultError("reading performance", err)
	}

	report := performanceReport{
		ServiceURL:    a.ServiceURL,
		Current:       env.Current,
		HistoryPoints: len(env.History),
	}

	// A few readings a caller would otherwise have to know the thresholds for.
	// Each is phrased as an observation, not a verdict: none of these is
	// necessarily wrong, and a tool that cried "leak" at 12k goroutines in a
	// service designed for them would be actively harmful.
	cur := env.Current
	if cur.Goroutines > 10000 {
		report.Observations = append(report.Observations, fmt.Sprintf(
			"%d goroutines is high. If this climbs between calls, something is starting goroutines "+
				"per request without letting them finish — a blocking call in a handler is the usual cause.",
			cur.Goroutines,
		))
	}
	if cur.GC.CPUFraction > 0.10 {
		report.Observations = append(report.Observations, fmt.Sprintf(
			"GC is using %.1f%% of CPU. Allocation per request is the thing to look at; "+
				"GOGC is currently %d.", cur.GC.CPUFraction*100, cur.RuntimeTuning.GOGC))
	}
	if cur.Memory.UsagePct > 90 {
		report.Observations = append(report.Observations, fmt.Sprintf(
			"Live heap is %.1f%% of memory obtained from the OS.", cur.Memory.UsagePct))
	}
	if len(env.History) >= 2 {
		first := env.History[0].Goroutines
		last := env.History[len(env.History)-1].Goroutines
		if first > 0 && last > first*2 {
			report.Observations = append(report.Observations, fmt.Sprintf(
				"Goroutines grew from %d to %d across the sampled history, which is the shape of a leak "+
					"rather than of load.",
				first,
				last,
			))
		}
	}

	summary := fmt.Sprintf(
		"%d goroutines, heap %s, GC %d cycles (%.1f%% CPU), %d history point(s)",
		cur.Goroutines,
		diag.HumanBytes(cur.Heap.Alloc),
		cur.GC.NumGC,
		cur.GC.CPUFraction*100,
		len(env.History),
	)
	return structuredResult(summary, report)
}

// ─── get_recent_errors ───────────────────────────────────────────────────────

// liveRequestRecord mirrors dashboard.RequestRecord's JSON.
type liveRequestRecord struct {
	ID         string  `json:"id"`
	Time       string  `json:"time"`
	Method     string  `json:"method"`
	Path       string  `json:"path"`
	Route      string  `json:"route"`
	Status     int     `json:"status"`
	DurationMS float64 `json:"duration_ms"`
	IP         string  `json:"ip"`
	User       string  `json:"user"`
	Error      string  `json:"error,omitempty"`
	TimelineID string  `json:"timeline_id,omitempty"`
}

type errorsReport struct {
	ServiceURL string              `json:"service_url"`
	Scanned    int                 `json:"requests_scanned"`
	Count      int                 `json:"count"`
	ServerSide int                 `json:"server_errors"`
	ClientSide int                 `json:"client_errors"`
	ByRoute    map[string]int      `json:"by_route,omitempty"`
	Errors     []liveRequestRecord `json:"errors"`
	Notes      []string            `json:"notes,omitempty"`
}

type recentErrorsArgs struct {
	liveArgs
	Limit int `json:"limit"`
}

func getRecentErrorsTool() *tool {
	return &tool{
		name: "breeze_get_recent_errors",
		description: "List the recent failed requests of a running service — status, path, matched " +
			"route, duration, and timeline id — grouped by route so a repeated failure is " +
			"visible as one problem. Separates 5xx from 4xx. Requires the dashboard feature.",
		schema: objectSchema(liveProps(map[string]any{
			"limit": map[string]any{
				"type": "integer",
				"description": "How many recent requests to scan for failures. Defaults to 200, " +
					"the dashboard's own default.",
			},
		}), "service_url"),
		run: func(raw json.RawMessage) toolCallResult {
			var a recentErrorsArgs
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}
			return getRecentErrors(a)
		},
	}
}

func getRecentErrors(a recentErrorsArgs) toolCallResult {
	// The endpoint has a status filter, but it takes one value at a time and
	// cannot express "anything that failed". Scanning the request log and
	// filtering here is one call instead of several, and the scanned count is
	// reported so the caller knows the denominator.
	req := a.request("/requests", "dashboard")
	if a.Limit > 0 {
		req.path += "?limit=" + fmt.Sprint(a.Limit)
	}

	var records []liveRequestRecord
	if err := fetchLiveJSON(req, &records); err != nil {
		return liveResultError("reading recent errors", err)
	}

	report := errorsReport{
		ServiceURL: a.ServiceURL,
		Scanned:    len(records),
		Errors:     make([]liveRequestRecord, 0),
		ByRoute:    map[string]int{},
	}
	for _, r := range records {
		if r.Status < 400 && r.Error == "" {
			continue
		}
		report.Errors = append(report.Errors, r)
		switch {
		case r.Status >= 500:
			report.ServerSide++
		case r.Status >= 400:
			report.ClientSide++
		}
		key := r.Route
		if key == "" {
			// An unmatched path is itself the finding: it means no route
			// matched, so grouping by the raw path is the useful grouping.
			key = r.Method + " " + r.Path + " (no route matched)"
		} else {
			key = r.Method + " " + key
		}
		report.ByRoute[key]++
	}
	report.Count = len(report.Errors)

	if report.Scanned == 0 {
		report.Notes = append(
			report.Notes,
			"The service has recorded no requests at all, so this is not "+
				"evidence that it is error-free. Check that the dashboard middleware is installed and that "+
				"traffic has reached the service.",
		)
	}

	summary := fmt.Sprintf("%d failed request(s) out of %d scanned: %d server-side, %d client-side",
		report.Count, report.Scanned, report.ServerSide, report.ClientSide)
	return structuredResult(summary, report)
}

// ─── get_logs ────────────────────────────────────────────────────────────────

// liveLogEntry mirrors dashboard.LogEntry's JSON.
type liveLogEntry struct {
	Time    string `json:"time"`
	Level   string `json:"level"`
	Message string `json:"message"`
	Source  string `json:"source,omitempty"`
	TraceID string `json:"trace_id,omitempty"`
}

type logsReport struct {
	ServiceURL string         `json:"service_url"`
	Level      string         `json:"level"`
	Query      string         `json:"query,omitempty"`
	TraceID    string         `json:"trace_id,omitempty"`
	Count      int            `json:"count"`
	Entries    []liveLogEntry `json:"entries"`
	Notes      []string       `json:"notes,omitempty"`
}

type logsArgs struct {
	liveArgs
	Level   string `json:"level"`
	Query   string `json:"query"`
	TraceID string `json:"trace_id"`
	Limit   int    `json:"limit"`
}

func getLogsTool() *tool {
	return &tool{
		name: "breeze_get_logs",
		description: "Read a running service's logs, optionally filtered by a substring or by trace id. " +
			"Filtering by trace_id is the way to see every line one request produced, including " +
			"across a Fleet trace. Levels are app, http, error, panic, and warning. Requires the " +
			"dashboard feature; this endpoint also accepts the service token.",
		schema: objectSchema(liveProps(map[string]any{
			"level": stringProp(
				"Log level to read: app (default), http, error, panic, or warning.",
			),
			"query": stringProp("Case-insensitive substring to match against the message."),
			"trace_id": stringProp(
				"Return only lines carrying this trace id — the lines belonging " +
					"to one request.",
			),
			"limit": map[string]any{
				"type":        "integer",
				"description": "Maximum lines to read. Defaults to 500, the dashboard's own default.",
			},
		}), "service_url"),
		run: func(raw json.RawMessage) toolCallResult {
			var a logsArgs
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}
			return getLogs(a)
		},
	}
}

func getLogs(a logsArgs) toolCallResult {
	level := strings.TrimSpace(a.Level)
	if level == "" {
		level = "app"
	}

	// Filtering is pushed to the service rather than done here: the ring buffer
	// holds a bounded number of lines, so filtering locally would first discard
	// everything past the limit and then search the remainder, which silently
	// misses matches the service could have found.
	query := url.Values{}
	query.Set("level", level)
	if a.Limit > 0 {
		query.Set("limit", fmt.Sprint(a.Limit))
	}
	if q := strings.TrimSpace(a.Query); q != "" {
		query.Set("q", q)
	}
	if id := strings.TrimSpace(a.TraceID); id != "" {
		query.Set("trace_id", id)
	}

	req := a.request("/logs", "dashboard")
	req.path += "?" + query.Encode()

	var entries []liveLogEntry
	if err := fetchLiveJSON(req, &entries); err != nil {
		return liveResultError("reading logs", err)
	}

	report := logsReport{
		ServiceURL: a.ServiceURL,
		Level:      level,
		Query:      strings.TrimSpace(a.Query),
		TraceID:    strings.TrimSpace(a.TraceID),
		Count:      len(entries),
		Entries:    entries,
	}

	// An empty result has two very different causes and the caller cannot tell
	// them apart from an empty array.
	if len(entries) == 0 {
		switch {
		case report.TraceID != "":
			report.Notes = append(report.Notes, "No lines carry this trace id at level "+level+
				". The id may belong to another service in the trace, or the work may have logged "+
				"at a different level — try level=error, or read the trace with breeze_get_trace.")
		case report.Query != "":
			report.Notes = append(report.Notes, "Nothing at level "+level+" matched this query. "+
				"The buffer holds only recent lines, so an older match will have been evicted.")
		default:
			report.Notes = append(report.Notes, "No lines at level "+level+
				". Levels are separate buffers: application logs are under app, request logs under http, "+
				"and failures under error or panic.")
		}
	}

	summary := fmt.Sprintf("%d log line(s) at level %s", report.Count, level)
	return structuredResult(summary, report)
}

// ─── query_openapi ───────────────────────────────────────────────────────────

type openAPIReport struct {
	ServiceURL  string             `json:"service_url"`
	SpecPath    string             `json:"spec_path"`
	OpenAPI     string             `json:"openapi"`
	Title       string             `json:"title"`
	Version     string             `json:"version"`
	PathCount   int                `json:"path_count"`
	Operations  []openAPIOperation `json:"operations"`
	SchemaNames []string           `json:"schema_names,omitempty"`
	Notes       []string           `json:"notes,omitempty"`
}

type openAPIOperation struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	OperationID string `json:"operation_id,omitempty"`
	Summary     string `json:"summary,omitempty"`
	Tag         string `json:"tag,omitempty"`
}

type openAPIArgs struct {
	liveArgs
	SpecPath string `json:"spec_path"`
}

func queryOpenAPITool() *tool {
	return &tool{
		name: "breeze_query_openapi",
		description: "Fetch and summarise a running service's OpenAPI document: title, version, every " +
			"operation with its method, path, operation id, and tag, plus the component schema " +
			"names. Use this to learn another service's contract before calling it. Requires the " +
			"docs feature.",
		schema: objectSchema(liveProps(map[string]any{
			"spec_path": stringProp("Path to the OpenAPI document. Defaults to /openapi.json, " +
				"the generator's default spec_path."),
		}), "service_url"),
		run: func(raw json.RawMessage) toolCallResult {
			var a openAPIArgs
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}
			return queryOpenAPI(a)
		},
	}
}

func queryOpenAPI(a openAPIArgs) toolCallResult {
	specPath := strings.TrimSpace(a.SpecPath)
	if specPath == "" {
		specPath = "/openapi.json"
	}
	if !strings.HasPrefix(specPath, "/") {
		specPath = "/" + specPath
	}

	// The spec is served at the project's own spec_path, not under the
	// dashboard's base, so this one call does not go through liveArgs.request.
	req := liveRequest{
		baseURL:  a.ServiceURL,
		path:     specPath,
		token:    a.Token,
		username: a.Username,
		password: a.Password,
		feature:  "docs (OpenAPI)",
	}

	// Decoded loosely on purpose: the point is to summarise whatever a service
	// serves, including one not built with this framework, and a strict struct
	// would reject a valid document that merely used a field this tool does not
	// care about.
	var doc struct {
		OpenAPI string `json:"openapi"`
		Info    struct {
			Title   string `json:"title"`
			Version string `json:"version"`
		} `json:"info"`
		Paths      map[string]map[string]json.RawMessage `json:"paths"`
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := fetchLiveJSON(req, &doc); err != nil {
		return liveResultError("reading the OpenAPI document", err)
	}

	report := openAPIReport{
		ServiceURL: a.ServiceURL,
		SpecPath:   specPath,
		OpenAPI:    doc.OpenAPI,
		Title:      doc.Info.Title,
		Version:    doc.Info.Version,
		PathCount:  len(doc.Paths),
		Operations: make([]openAPIOperation, 0, len(doc.Paths)),
	}

	// Sorted so two calls against the same service produce identical output;
	// Go's map order would otherwise make this look like it changed.
	paths := make([]string, 0, len(doc.Paths))
	for p := range doc.Paths {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, p := range paths {
		methods := make([]string, 0, len(doc.Paths[p]))
		for m := range doc.Paths[p] {
			methods = append(methods, m)
		}
		sort.Strings(methods)

		for _, m := range methods {
			// Keys that are not operations live alongside them at this level.
			switch strings.ToLower(m) {
			case "parameters", "servers", "summary", "description", "$ref":
				continue
			}
			var op struct {
				OperationID string   `json:"operationId"`
				Summary     string   `json:"summary"`
				Tags        []string `json:"tags"`
			}
			_ = json.Unmarshal(doc.Paths[p][m], &op)

			entry := openAPIOperation{
				Method:      strings.ToUpper(m),
				Path:        p,
				OperationID: op.OperationID,
				Summary:     op.Summary,
			}
			if len(op.Tags) > 0 {
				entry.Tag = op.Tags[0]
			}
			report.Operations = append(report.Operations, entry)
		}
	}

	for name := range doc.Components.Schemas {
		report.SchemaNames = append(report.SchemaNames, name)
	}
	sort.Strings(report.SchemaNames)

	if report.OpenAPI == "" {
		report.Notes = append(
			report.Notes,
			"The document has no openapi version field, so it may not be "+
				"an OpenAPI document at all. Check that spec_path points at the spec and not at the docs UI.",
		)
	}
	if len(report.Operations) == 0 {
		report.Notes = append(
			report.Notes,
			"The document declares no operations. Routes appear in the spec "+
				"once they are registered with the docs middleware; a route added by hand without it "+
				"will be served but undocumented.",
		)
	}

	summary := fmt.Sprintf(
		"%s %s: %d operation(s) across %d path(s), %d schema(s)",
		report.Title,
		report.Version,
		len(report.Operations),
		report.PathCount,
		len(report.SchemaNames),
	)
	return structuredResult(summary, report)
}
