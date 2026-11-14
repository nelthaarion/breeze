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
	"time"

	"github.com/nelthaarion/breeze/v2/diag"
	"github.com/nelthaarion/breeze/v2/internal/livesource"
)

func registerLiveTools(s *Server) {
	s.addTool(getRoutesTool())
	s.addTool(getPerformanceTool())
	s.addTool(getRecentErrorsTool())
	s.addTool(getLogsTool())
	s.addTool(getSignalsTool())
	s.addTool(getHealthTool())
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

	// local, when set, reads this process's own dashboard collector instead of calling a
	// service over HTTP. Unexported on purpose: a caller cannot set it through a tool's
	// arguments. Only an embedded server sets it (embedded_live.go), and only for a call
	// that named no service_url.
	local livesource.Reader
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
		apiPath:  apiPath,
		local:    a.local,
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
	ClientErrors int64    `json:"client_errors"`

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
	ServiceURL string `json:"service_url"`
	// Counting says what a request count of zero means: "exact" (every request that reaches the
	// dashboard's middleware is counted, so zero is "never called"), "not_measured" (nothing is
	// counting, so zero says nothing) or "partial" (an older dashboard that counts only the requests
	// it kept in full).
	Counting string `json:"counting"`
	Total    int    `json:"total"`
	// Exercised is the routes that served at least one request, Unused those that served none. Read
	// Unused with Counting: it means "never called" only when counting is exact.
	Exercised int `json:"exercised"`
	Unused    int `json:"unused"`
	// WithErrors counts routes with a 5xx, WithClientErrors those with a 4xx.
	WithErrors       int         `json:"with_errors"`
	WithClientErrors int         `json:"with_client_errors"`
	Documented       int         `json:"documented"`
	Routes           []liveRoute `json:"routes"`
	// Unmatched is the requests that ran the middleware chain and that no route served (a CORS
	// preflight, the auto-served index page). They belong to no route above. A request for a path
	// that is not routed at all never reaches the dashboard's middleware and is in no count.
	Unmatched *liveRouteTraffic `json:"unmatched,omitempty"`
	Notes     []string          `json:"notes,omitempty"`
}

func getRoutesTool() *tool {
	return &tool{
		name: "breeze_get_routes",
		description: "List the routes a running service is serving, with each route's own summary and " +
			"description as its author wrote them, plus live request counts, average and maximum " +
			"latency, and server-error and client-error counts. The counts are exact: the dashboard " +
			"counts every request against its route, so a route with zero requests was never called " +
			"(the report's counting field says when that does not hold). Reads the dashboard's routes " +
			"endpoint, so it reflects what the process is actually doing rather than what the source " +
			"says. Reports which routes carry no documentation — those are also absent from the " +
			"OpenAPI document. Requires the dashboard feature.",
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

	// What a zero means depends on whether anything is counting, so that is read first.
	traffic := readTraffic(a)
	mode := countingOf(traffic)

	report := routesReport{ServiceURL: a.ServiceURL, Counting: string(mode), Total: len(routes), Routes: routes}
	if traffic != nil && traffic.Unmatched.Requests > 0 {
		report.Unmatched = &traffic.Unmatched
	}
	for _, r := range routes {
		if r.Requests > 0 {
			report.Exercised++
		} else {
			report.Unused++
		}
		if r.Errors > 0 {
			report.WithErrors++
		}
		if r.ClientErrors > 0 {
			report.WithClientErrors++
		}
		if r.Documented {
			report.Documented++
		}
	}

	// Said explicitly, because a listing of zeroes has more than one cause and an agent that
	// guessed would call a healthy service dead code, or a dead counter a quiet service.
	//
	// The dashboard now counts every request against its route, so on a dashboard that is
	// counting a zero is a fact: the route was not called. Where nothing is counting (the
	// middleware was never created, or the dashboard is off) or the dashboard is an older build
	// that attributed only the requests it kept in full, a zero says nothing about traffic, and
	// the tool says which of those it is looking at.
	switch mode {
	case countingNotMeasured:
		report.Notes = append(report.Notes, notCountingExplanation(traffic))
	case countingPartial:
		if report.Total > 0 && report.Exercised == 0 {
			report.Notes = append(report.Notes, "Every route reports zero requests. This dashboard does not report "+
				"exact counts (it has no traffic endpoint, or it could not be read), and an older dashboard "+
				"attributes a request to its route only when it captured the request in full: while someone "+
				"has the dashboard open, when it was slower than slow_request_ms, or when it failed. Fast "+
				"successful requests served with nobody watching are counted in the totals but not attributed "+
				"to a route. Treat zero here as 'not measured', not as 'never called'.")
		}
	case countingExact:
		if report.Total > 0 && report.Exercised == 0 && traffic.Unmatched.Requests == 0 {
			report.Notes = append(report.Notes, "No route has served a request since the dashboard started counting. "+
				"The counts are exact, so this is 'no traffic', not 'not measured'. Requests refused before the "+
				"dashboard's middleware runs (by a rate limiter or other middleware ordered ahead of it) are not "+
				"counted.")
		}
		if traffic.Unmatched.Requests > 0 {
			report.Notes = append(report.Notes, fmt.Sprintf("%d request(s) ran the middleware and matched no route (%d of them "+
				"client errors): a CORS preflight, the auto-served index page. See the unmatched field; they belong to "+
				"no route above.", traffic.Unmatched.Requests, traffic.Unmatched.ClientErrors))
		}
		// The framework answers a request for a path that no route serves before any middleware runs, so
		// it is in no count and in no log. An agent chasing "clients get 404" has to be told, or it will
		// read a zero as the absence of the problem.
		report.Notes = append(report.Notes, "A request for a path that no route serves is answered by the framework before "+
			"any middleware runs, so it is not counted here and does not appear in the logs either: these counts "+
			"cover requests that matched a route (or a preflight).")
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

	// "Never called" is a claim about traffic, so it is made only where the counts can support it.
	idle := "never called"
	if mode != countingExact {
		idle = "not observed"
	}
	summary := fmt.Sprintf("%d route(s): %d exercised, %d %s, %d with server errors, %d with client errors, %d documented",
		report.Total, report.Exercised, report.Unused, idle, report.WithErrors, report.WithClientErrors, report.Documented)
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
			cur.Goroutines))
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
					"rather than of load.", first, last))
		}
	}

	summary := fmt.Sprintf("%d goroutines, heap %s, GC %d cycles (%.1f%% CPU), %d history point(s)",
		cur.Goroutines, diag.HumanBytes(cur.Heap.Alloc), cur.GC.NumGC, cur.GC.CPUFraction*100, len(env.History))
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
	ServiceURL string `json:"service_url"`
	// Counting says how far to trust the counts: "exact" (the dashboard counts every request, so
	// the exact block below is the whole picture and the records are a sample of it), "not_measured"
	// (nothing is counting) or "partial" (an older dashboard that keeps only some failures).
	Counting   string              `json:"counting"`
	Scanned    int                 `json:"requests_scanned"`
	Count      int                 `json:"count"`
	ServerSide int                 `json:"server_errors"`
	ClientSide int                 `json:"client_errors"`
	ByRoute    map[string]int      `json:"by_route,omitempty"`
	Errors     []liveRequestRecord `json:"errors"`
	// Exact is what the dashboard counted, of which the records above are a sample. The counts in
	// ServerSide and ClientSide are of the records scanned; these are of every request.
	Exact *exactFailures `json:"exact,omitempty"`
	Notes []string       `json:"notes,omitempty"`
}

// exactFailures is the dashboard's own count of failures, and how many it did not keep in full.
type exactFailures struct {
	Requests     int64 `json:"requests"`
	ServerErrors int64 `json:"server_errors"`
	ClientErrors int64 `json:"client_errors"`
	// NotRecorded is the failures counted and not stored as full records, the difference between
	// this block and the records above.
	NotRecorded int64 `json:"not_recorded"`
	// Unmatched is the failures that no route served.
	Unmatched liveRouteTraffic `json:"unmatched"`
	// Routes are the routes with the most failures, worst first, at most ten.
	Routes  []liveRouteTraffic `json:"routes"`
	Capture liveCapturePolicy  `json:"capture"`
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
			"visible as one problem. Separates 5xx from 4xx. The records are a sample (the dashboard " +
			"keeps a failure in full up to a budget per route per window); the exact block beside them " +
			"is the dashboard's own count of every failure, by route, with how many were not kept. " +
			"Requires the dashboard feature.",
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

	traffic := readTraffic(a.liveArgs)
	mode := countingOf(traffic)

	report := errorsReport{
		ServiceURL: a.ServiceURL,
		Counting:   string(mode),
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

	// What the dashboard keeps decides what this tool can see, and the caller cannot tell an
	// empty answer from an unobserved one, so the notes say which this is.
	//
	// A dashboard that counts exactly gives the tool two views of failures: the records, which are a
	// sample (a failure is kept in full up to a budget per route per window, so that a flood of one
	// cannot evict everything else), and the counts, which are complete. An agent told "ten 404s"
	// could not otherwise know whether that was ten or ten thousand, and one told "no 4xx" could not
	// tell a quiet service from a policy that kept none.
	switch mode {
	case countingNotMeasured:
		report.Notes = append(report.Notes, notCountingExplanation(traffic))

	case countingPartial:
		// An older dashboard keeps a 5xx always, a 4xx only while watched or slow, and has no counts.
		switch {
		case report.Scanned == 0:
			report.Notes = append(report.Notes, "The dashboard holds no request records, so this is not evidence that the "+
				"service is error-free. This dashboard has no exact counts (it has no traffic endpoint, or it could not "+
				"be read) and records a request in full only while someone has the dashboard open, when the request "+
				"was slower than slow_request_ms, or when it failed with a 5xx: a fast success or a 4xx served with "+
				"nobody watching is counted in the totals and not kept. If traffic should have produced records, "+
				"check that the dashboard middleware is installed and that the traffic reached this service.")
		case report.ClientSide == 0:
			report.Notes = append(report.Notes, "No client-side (4xx) failures were found, which does not rule them out: this "+
				"dashboard keeps a 4xx only while someone has the dashboard open or when it was slow, and counts the "+
				"rest without recording them. 5xx failures are always kept.")
		}

	case countingExact:
		exact := &exactFailures{
			Requests:     traffic.Requests,
			ServerErrors: traffic.ServerErrors,
			ClientErrors: traffic.ClientErrors,
			Unmatched:    traffic.Unmatched,
			NotRecorded:  traffic.Unmatched.NotRecorded,
			Routes:       traffic.Routes,
			Capture:      traffic.Capture,
		}
		for _, r := range traffic.Routes {
			exact.NotRecorded += r.NotRecorded
		}
		if len(exact.Routes) > 10 {
			exact.Routes = exact.Routes[:10]
		}
		if exact.Routes == nil {
			exact.Routes = []liveRouteTraffic{}
		}
		report.Exact = exact

		failures := traffic.ClientErrors + traffic.ServerErrors
		switch {
		case traffic.Requests == 0:
			report.Notes = append(report.Notes, "The dashboard has counted no requests yet, so there is nothing to report: this is "+
				"not evidence that the service is error-free, only that nothing has reached the dashboard's middleware.")
		case failures == 0:
			report.Notes = append(report.Notes, fmt.Sprintf("The dashboard has counted %d request(s) and none failed: no 4xx and no 5xx. "+
				"The counts are exact, so this is evidence that the service has not failed a request it served, with one "+
				"limit: a request refused before the dashboard's middleware runs (by a rate limiter, CORS or security "+
				"middleware ordered ahead of it) is not counted.", traffic.Requests))
		default:
			report.Notes = append(report.Notes, captureSentence(traffic.Capture))
			if exact.NotRecorded > 0 {
				report.Notes = append(report.Notes, fmt.Sprintf("%d of the %d failure(s) counted were not kept in full: a route's budget "+
					"for the window was spent. The records above are a sample; exact.routes lists each route's exact counts.",
					exact.NotRecorded, failures))
			}
			if report.Scanned == 0 {
				report.Notes = append(report.Notes, "The dashboard counted failures but holds no records of them (the ring buffer "+
					"was overwritten, or they were sampled out); the exact block is the evidence here.")
			}
		}
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
	ServiceURL string `json:"service_url"`
	Level      string `json:"level"`
	// Tabs lists the log tabs that were read when Level is "all", so a reader can see
	// that an empty result covers every one of them and not just the default.
	Tabs    []string       `json:"tabs,omitempty"`
	Query   string         `json:"query,omitempty"`
	TraceID string         `json:"trace_id,omitempty"`
	Count   int            `json:"count"`
	Entries []liveLogEntry `json:"entries"`
	Notes   []string       `json:"notes,omitempty"`
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
			"across a Fleet trace, so with a trace_id and no level every level is read: a failure is " +
			"logged under error, beside the app lines of the same request. Logs are per service: to " +
			"find why a Fleet trace failed, read the service the trace names as the failing hop. " +
			"Levels are app, http, error, panic, warning and all. Requires the dashboard feature; " +
			"this endpoint also accepts the service token.",
		schema: objectSchema(liveProps(map[string]any{
			"level": stringProp("Log level to read: app, http, error, panic, warning, or all. Defaults to " +
				"app, or to all when trace_id is given. info and debug mean app; warn means warning."),
			"query": stringProp("Case-insensitive substring to match against the message."),
			"trace_id": stringProp("Return only lines carrying this trace id — the lines belonging " +
				"to one request."),
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

// logLevelAll reads every tab. It is the default when a trace id is given.
const logLevelAll = "all"

// logTabs are the dashboard's log tabs. They are separate buffers and a line is stored
// on exactly one of them, so reading everything one request logged means reading all
// of them.
var logTabs = []string{"app", "http", "error", "panic", "warning"}

// canonicalLogLevel maps the names callers use onto the tab that holds them.
//
// The dashboard accepts these aliases itself now, but the service being read may be an
// older build than this server, and an older dashboard answers a name it does not know
// with an empty tab: the same answer as "nothing was logged". Mapping here means the
// request that goes out only ever names a tab.
func canonicalLogLevel(level string) (string, bool) {
	switch level {
	case "app", "info", "debug":
		return "app", true
	case "http", "error", "panic", "warning":
		return level, true
	case "warn":
		return "warning", true
	}
	return "", false
}

func getLogs(a logsArgs) toolCallResult {
	traceID := strings.TrimSpace(a.TraceID)
	requested := strings.ToLower(strings.TrimSpace(a.Level))

	// A trace id is how one request's lines are found, and the thing a caller holding
	// one does not know is which tab they were written to: the failure is under error
	// and the work that led to it under app. Defaulting to app answered "no lines"
	// for exactly the request whose lines were wanted.
	var tabs []string
	switch requested {
	case "":
		if traceID != "" {
			requested, tabs = logLevelAll, logTabs
		} else {
			requested, tabs = "app", []string{"app"}
		}
	case logLevelAll:
		tabs = logTabs
	default:
		tab, ok := canonicalLogLevel(requested)
		if !ok {
			// Refused rather than passed on. The service answers an unknown level with an
			// empty list, and an empty list is what "nothing was logged" looks like.
			return errorResult(fmt.Sprintf("unknown level %q: use app, http, error, panic, warning or all",
				strings.TrimSpace(a.Level)))
		}
		requested, tabs = tab, []string{tab}
	}

	limit := a.Limit
	if limit <= 0 {
		limit = 500 // the dashboard's own default
	}

	// Filtering is pushed to the service rather than done here: the ring buffer
	// holds a bounded number of lines, so filtering locally would first discard
	// everything past the limit and then search the remainder, which silently
	// misses matches the service could have found.
	var entries []liveLogEntry
	for _, tab := range tabs {
		query := url.Values{}
		query.Set("level", tab)
		query.Set("limit", fmt.Sprint(limit))
		if q := strings.TrimSpace(a.Query); q != "" {
			query.Set("q", q)
		}
		if traceID != "" {
			query.Set("trace_id", traceID)
		}

		req := a.request("/logs", "dashboard")
		req.path += "?" + query.Encode()

		var tabEntries []liveLogEntry
		if err := fetchLiveJSON(req, &tabEntries); err != nil {
			return liveResultError("reading logs", err)
		}
		entries = append(entries, tabEntries...)
	}

	if len(tabs) > 1 {
		entries = newestLogEntries(entries, limit)
	}

	report := logsReport{
		ServiceURL: a.ServiceURL,
		Level:      requested,
		Query:      strings.TrimSpace(a.Query),
		TraceID:    traceID,
		Count:      len(entries),
		Entries:    entries,
	}
	if len(tabs) > 1 {
		report.Tabs = tabs
	}

	// An empty result has two very different causes and the caller cannot tell
	// them apart from an empty array.
	if len(entries) == 0 {
		switch {
		case report.TraceID != "" && len(tabs) > 1:
			report.Notes = append(report.Notes, "No line at any level carries this trace id. Logs are per service, "+
				"so the id may belong to another service in the trace: read the one the trace names as failing, "+
				"or read the trace with breeze_get_trace. Older lines may also have been evicted.")
		case report.TraceID != "":
			report.Notes = append(report.Notes, "No lines carry this trace id at level "+requested+
				". The id may belong to another service in the trace, or the work may have logged "+
				"at a different level  try level=all, or read the trace with breeze_get_trace.")
		case report.Query != "":
			report.Notes = append(report.Notes, "Nothing at level "+requested+" matched this query. "+
				"The buffer holds only recent lines, so an older match will have been evicted.")
		default:
			report.Notes = append(report.Notes, "No lines at level "+requested+
				". Levels are separate buffers: application logs are under app, request logs under http, "+
				"and failures under error or panic. level=all reads every one.")
		}
	}

	summary := fmt.Sprintf("%d log line(s) at level %s", report.Count, requested)
	return structuredResult(summary, report)
}

// newestLogEntries orders lines by when they were logged and keeps the newest limit of
// them. Lines from different tabs have no shared order until they are sorted: each
// tab arrives oldest first on its own.
//
// Times are compared as times. They are RFC 3339 strings with trailing zeros trimmed, so
// 12:00:01.98Z sorts after 12:00:01.981Z as text and before it as time, which is the
// difference between a failure appearing after the work that caused it and before.
func newestLogEntries(entries []liveLogEntry, limit int) []liveLogEntry {
	times := make([]time.Time, len(entries))
	for i, e := range entries {
		times[i], _ = time.Parse(time.RFC3339Nano, e.Time) // an unparseable time sorts first
	}

	order := make([]int, len(entries))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return times[order[a]].Before(times[order[b]]) })

	sorted := make([]liveLogEntry, len(entries))
	for i, idx := range order {
		sorted[i] = entries[idx]
	}
	if limit > 0 && len(sorted) > limit {
		sorted = sorted[len(sorted)-limit:]
	}
	return sorted
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
		report.Notes = append(report.Notes, "The document has no openapi version field, so it may not be "+
			"an OpenAPI document at all. Check that spec_path points at the spec and not at the docs UI.")
	}
	if len(report.Operations) == 0 {
		report.Notes = append(report.Notes, "The document declares no operations. Routes appear in the spec "+
			"once they are registered with the docs middleware; a route added by hand without it "+
			"will be served but undocumented.")
	}

	summary := fmt.Sprintf("%s %s: %d operation(s) across %d path(s), %d schema(s)",
		report.Title, report.Version, len(report.Operations), report.PathCount, len(report.SchemaNames))
	return structuredResult(summary, report)
}
