package mcp_test

// tools_live_test.go — the dashboard tools tested against a service that is
// actually running.
//
// A mocked HTTP server would prove almost nothing here. The whole risk in these
// tools is that this package's reading of the dashboard's JSON is wrong, and a
// handwritten stub is written from the same reading — so a misreading would be
// baked into both halves and the test would pass while the tool returned
// nonsense against a real service. These tests therefore boot a real Breeze app
// with the real dashboard installed, drive real requests through it with the
// framework's own client, and then call the tools across a real socket.
//
// Two structural notes.
//
// This is package mcp_test, not package mcp. It has to be: package dashboard
// imports the root breeze package, and Part 4 has the root breeze package import
// internal/mcp, so an in-package test importing dashboard would create an import
// cycle. The external test package can import both. The consequence is that
// unexported identifiers are unavailable, so these tests go through the exported
// tools/call surface — which is the surface a real client uses anyway.
//
// Breeze exposes Run but no Stop, so a booted app owns its port for the rest of
// the process. All of the happy-path assertions therefore share one fixture,
// started once, rather than leaking an event loop per test.

import (
	"encoding/json"
	"fmt"
	"net"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nelthaarion/breeze/v2"
	"github.com/nelthaarion/breeze/v2/client"
	"github.com/nelthaarion/breeze/v2/dashboard"
	"github.com/nelthaarion/breeze/v2/internal/mcp"
	"github.com/nelthaarion/breeze/v2/scalar"
)

const (
	fixtureToken    = "live-test-token"
	fixtureUser     = "liveadmin"
	fixturePassword = "livepassword"
)

// widgetQuery is the documented query shape for /api/widgets.
//
// Its description tag is what the Part 5 assertions look for: a field-level
// sentence that reaches an agent through the routes endpoint and the API
// explorer, not only through the OpenAPI document.
type widgetQuery struct {
	Limit int `json:"limit,omitempty" description:"How many widgets to return."`
}

type liveFixture struct {
	url  string
	port int
	coll *dashboard.Collector
}

var (
	fixtureOnce sync.Once
	fixture     liveFixture
)

// startFixture boots one instrumented service for the whole package.
//
// The dashboard is installed with auth ON. Running it with auth disabled would
// have been easier and would have tested the wrong thing: the tools have to send
// credentials correctly, and a fixture that accepted anonymous requests could not
// tell a working credential path from a missing one.
func startFixture(t *testing.T) liveFixture {
	t.Helper()

	fixtureOnce.Do(func() {
		port := freeLivePort(t)

		router := breeze.NewRouter()
		app := breeze.New(router, breeze.NewEventLoopWorkerPool(runtime.NumCPU()))

		cfg := dashboard.DefaultConfig()
		cfg.Username = fixtureUser
		cfg.Password = fixturePassword
		cfg.ServiceToken = fixtureToken

		coll := dashboard.Install(app, router, cfg)

		// Before any application route, so every request below is instrumented.
		// This ordering is itself one of the idioms check_idioms enforces.
		router.Use(dashboard.Middleware(coll))

		// Scalar collection is on for the whole fixture, so the documented route
		// below actually reaches the registry. Without Enable, RegisterRoute
		// returns immediately and the Part 5 assertions would be testing nothing —
		// which is the silent failure the scalar probe exists to report.
		scalar.Enable()

		// One documented route and several undocumented ones, so the tools can be
		// asserted on both: that a description survives into their output, and
		// that a route without one is reported as undocumented rather than as
		// absent.
		scalar.RegisterRoute("GET", "/api/widgets", scalar.RouteDoc{
			Title:       "List widgets",
			Description: "Returns every widget the caller may see.",
			Tags:        []string{"Widgets"},
			Input: []scalar.InputGroup{
				{Type: scalar.InputQuery, Fields: widgetQuery{}},
			},
		})

		router.Handle(breeze.GET, "/api/widgets", func(ctx *breeze.Context) error {
			return ctx.JSON(map[string]any{"widgets": []string{"a", "b"}})
		})
		router.Handle(breeze.GET, "/api/widgets/:id", func(ctx *breeze.Context) error {
			return ctx.JSON(map[string]any{"id": ctx.GetParam("id")})
		})
		// A route that fails on purpose, so get_recent_errors has something real
		// to find rather than being asserted against an empty list.
		router.Handle(breeze.GET, "/api/broken", func(ctx *breeze.Context) error {
			ctx.Status(500)
			return ctx.JSON(map[string]any{"error": "deliberate failure"})
		})
		router.Handle(breeze.GET, "/api/slow", func(ctx *breeze.Context) error {
			time.Sleep(15 * time.Millisecond)
			return ctx.JSON(map[string]any{"ok": true})
		})

		go func() {
			// Blocks for the process lifetime; a bind failure shows up as the
			// wait below timing out rather than as a silent hang.
			_ = app.Run(port, false)
		}()

		waitForLivePort(t, port, 10*time.Second)

		fixture = liveFixture{
			url:  "http://127.0.0.1:" + strconv.Itoa(port),
			port: port,
			coll: coll,
		}
	})

	if fixture.coll == nil {
		t.Fatal("the fixture service failed to start")
	}
	return fixture
}

func freeLivePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func waitForLivePort(t *testing.T, port int, timeout time.Duration) {
	t.Helper()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("nothing accepted on port %d within %s", port, timeout)
}

// driveTraffic sends real requests so the collector has real data.
//
// Uses the framework's own client, for the same reason the transport does: this
// is the client a Breeze service is talked to with.
func driveTraffic(t *testing.T, base string) {
	t.Helper()

	c := client.New(client.Config{Timeout: 5 * time.Second})
	defer c.Close()

	for _, path := range []string{
		"/api/widgets", "/api/widgets", "/api/widgets",
		"/api/widgets/7",
		"/api/slow",
		"/api/broken", "/api/broken",
		"/api/there-is-no-such-route",
	} {
		resp, err := c.Do(client.NewRequest("GET", base+path, nil))
		if err != nil {
			t.Fatalf("drive %s: %v", path, err)
		}
		_ = resp
	}

	// The middleware records after the response is written, so a moment's grace
	// keeps this from racing the collector on a loaded machine.
	time.Sleep(150 * time.Millisecond)
}

// callLiveTool invokes a tool over the real JSON-RPC surface and returns the
// decoded structured payload.
//
// The structured content is what an agent branches on, so that is what these
// tests assert against — not the prose summary, which is allowed to be reworded.
func callLiveTool(t *testing.T, name string, args map[string]any) (map[string]any, string, bool) {
	t.Helper()

	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]any{"name": name, "arguments": args},
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	out := mcp.NewServer("test").RPCServer().Handle(raw)
	if len(out) == 0 {
		t.Fatalf("%s produced no response", name)
	}

	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("%s returned invalid JSON: %s (%v)", name, out, err)
	}
	if resp.Error != nil {
		t.Fatalf("%s: JSON-RPC error %d: %s", name, resp.Error.Code, resp.Error.Message)
	}

	var result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		StructuredContent map[string]any `json:"structuredContent"`
		IsError           bool           `json:"isError"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("decode %s result: %v", name, err)
	}

	summary := ""
	if len(result.Content) > 0 {
		summary = result.Content[0].Text
	}
	return result.StructuredContent, summary, result.IsError
}

// creds is the credential set the fixture requires.
func creds(base string) map[string]any {
	return map[string]any{
		"service_url": base,
		"username":    fixtureUser,
		"password":    fixturePassword,
		"token":       fixtureToken,
	}
}

func TestGetRoutesReportsLiveTrafficFromARunningService(t *testing.T) {
	f := startFixture(t)
	driveTraffic(t, f.url)

	got, summary, isErr := callLiveTool(t, "breeze_get_routes", creds(f.url))
	if isErr {
		t.Fatalf("breeze_get_routes failed: %s", summary)
	}

	total, _ := got["total"].(float64)
	if total == 0 {
		t.Fatalf("no routes reported; structured result was %v", got)
	}
	if exercised, _ := got["exercised"].(float64); exercised == 0 {
		t.Errorf("traffic was driven but no route is marked exercised: %s", summary)
	}

	// The registered application routes must be present, with the live counts
	// attached to the right pattern.
	routes, ok := got["routes"].([]any)
	if !ok {
		t.Fatalf("routes is not an array: %T", got["routes"])
	}
	counts := map[string]float64{}
	for _, entry := range routes {
		row, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("a route entry is not an object: %T", entry)
		}
		for _, key := range []string{"method", "pattern", "requests", "avg_latency_ms", "errors"} {
			if _, present := row[key]; !present {
				t.Errorf("route entry is missing %q: %v", key, row)
			}
		}
		pattern, _ := row["pattern"].(string)
		requests, _ := row["requests"].(float64)
		counts[pattern] = requests
	}

	// The failing route is the one whose count is guaranteed. Per-route
	// statistics are only accumulated for requests the dashboard captures in
	// full, and to keep the hot path allocation-free it captures only when
	// someone has the dashboard open, when a request is slower than
	// SlowRequestMs, or when it failed. The fast 200s above are therefore
	// counted in the totals but deliberately not attributed to a route, so
	// asserting a count on /api/widgets would be asserting against the
	// framework's actual sampling design rather than against this tool.
	if counts["/api/broken"] < 2 {
		t.Errorf("/api/broken failed twice but reports %v requests; live counts are not reaching the tool",
			counts["/api/broken"])
	}
	if _, ok := counts["/api/widgets/:id"]; !ok {
		t.Errorf("the parameterised route is missing from %v", counts)
	}
	if _, ok := counts["/api/widgets"]; !ok {
		t.Errorf("a registered route is missing entirely from %v; the static route table should be "+
			"merged in even for routes with no captured traffic", counts)
	}

	// The dashboard's own endpoints must not be reported as application routes.
	for pattern := range counts {
		if strings.HasPrefix(pattern, "/dashboard") {
			t.Errorf("dashboard route %q leaked into the application route list", pattern)
		}
	}
}

func TestGetRecentErrorsFindsTheDeliberateFailureAndGroupsIt(t *testing.T) {
	f := startFixture(t)
	driveTraffic(t, f.url)

	got, summary, isErr := callLiveTool(t, "breeze_get_recent_errors", creds(f.url))
	if isErr {
		t.Fatalf("breeze_get_recent_errors failed: %s", summary)
	}

	if scanned, _ := got["requests_scanned"].(float64); scanned == 0 {
		t.Fatal("no requests were scanned, so the dashboard recorded nothing")
	}
	if server, _ := got["server_errors"].(float64); server < 2 {
		t.Errorf("/api/broken failed twice but server_errors = %v: %s", got["server_errors"], summary)
	}

	entries, ok := got["errors"].([]any)
	if !ok || len(entries) == 0 {
		t.Fatalf("no errors reported despite a route returning 500: %v", got)
	}

	foundBroken := false
	for _, entry := range entries {
		row, _ := entry.(map[string]any)
		path, _ := row["path"].(string)
		status, _ := row["status"].(float64)
		if path == "/api/broken" {
			foundBroken = true
			if status != 500 {
				t.Errorf("/api/broken reported status %v, want 500", status)
			}
		}
		// A successful request must never appear in this list.
		if status < 400 {
			if errText, _ := row["error"].(string); errText == "" {
				t.Errorf("a non-failing request was reported as an error: %v", row)
			}
		}
	}
	if !foundBroken {
		t.Errorf("/api/broken is missing from the reported errors: %v", entries)
	}

	// Grouping is the point of the tool: two identical failures are one problem.
	byRoute, ok := got["by_route"].(map[string]any)
	if !ok {
		t.Fatalf("by_route is not an object: %T", got["by_route"])
	}
	if count, _ := byRoute["GET /api/broken"].(float64); count < 2 {
		t.Errorf("by_route did not group the repeated failure: %v", byRoute)
	}
}

func TestGetPerformanceReadsRealRuntimeNumbers(t *testing.T) {
	f := startFixture(t)

	got, summary, isErr := callLiveTool(t, "breeze_get_performance", creds(f.url))
	if isErr {
		t.Fatalf("breeze_get_performance failed: %s", summary)
	}

	current, ok := got["current"].(map[string]any)
	if !ok {
		t.Fatalf("current is not an object: %T", got["current"])
	}

	// A running Go process always has goroutines and a non-zero heap. Zero here
	// means the envelope was decoded into the wrong shape — the exact failure
	// this test exists to catch, and one a stub server would have hidden.
	goroutines, _ := current["goroutines"].(float64)
	if goroutines <= 0 {
		t.Errorf("goroutines = %v, which cannot be true of a running service; "+
			"the {current,history} envelope is probably being decoded wrongly", current["goroutines"])
	}

	heap, ok := current["heap"].(map[string]any)
	if !ok {
		t.Fatalf("heap is not an object: %T", current["heap"])
	}
	if alloc, _ := heap["alloc"].(float64); alloc <= 0 {
		t.Errorf("heap.alloc = %v, want a positive byte count", heap["alloc"])
	}

	cpu, ok := current["cpu"].(map[string]any)
	if !ok {
		t.Fatalf("cpu is not an object: %T", current["cpu"])
	}
	if numCPU, _ := cpu["num_cpu"].(float64); int(numCPU) != runtime.NumCPU() {
		t.Errorf("cpu.num_cpu = %v but this machine has %d", cpu["num_cpu"], runtime.NumCPU())
	}

	if _, present := current["runtime_tuning"]; !present {
		t.Error("runtime_tuning is missing, so GOGC/GOMEMLIMIT cannot be reported")
	}
}

func TestGetLogsReadsWhatTheServiceLoggedAndFiltersByTraceID(t *testing.T) {
	f := startFixture(t)

	// Recorded through the collector's own API, which is what a service's
	// logging integration calls.
	const traceID = "trace-live-test-0001"
	f.coll.RecordLog("app", dashboard.LogEntry{
		Time:    time.Now(),
		Level:   "app",
		Message: "widget cache warmed",
		Source:  "widgets.go:41",
	})
	f.coll.RecordLog("app", dashboard.LogEntry{
		Time:    time.Now(),
		Level:   "app",
		Message: "charging card for order 55",
		Source:  "billing.go:12",
		TraceID: traceID,
	})
	f.coll.RecordLog("error", dashboard.LogEntry{
		Time:    time.Now(),
		Level:   "error",
		Message: "payment gateway refused the charge",
		Source:  "billing.go:31",
		TraceID: traceID,
	})

	// Unfiltered.
	got, summary, isErr := callLiveTool(t, "breeze_get_logs", creds(f.url))
	if isErr {
		t.Fatalf("breeze_get_logs failed: %s", summary)
	}
	if count, _ := got["count"].(float64); count < 2 {
		t.Errorf("count = %v, want at least the 2 app lines recorded: %s", got["count"], summary)
	}
	entries, _ := got["entries"].([]any)
	if len(entries) == 0 {
		t.Fatal("no log entries were returned")
	}
	first, _ := entries[0].(map[string]any)
	for _, key := range []string{"time", "level", "message"} {
		if _, present := first[key]; !present {
			t.Errorf("log entry is missing %q: %v", key, first)
		}
	}

	// Substring filter.
	args := creds(f.url)
	args["query"] = "cache warmed"
	got, summary, isErr = callLiveTool(t, "breeze_get_logs", args)
	if isErr {
		t.Fatalf("filtered breeze_get_logs failed: %s", summary)
	}
	entries, _ = got["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("the query matched %d lines, want exactly 1: %v", len(entries), entries)
	}

	// Trace filter — the reason this tool takes a trace id at all.
	args = creds(f.url)
	args["trace_id"] = traceID
	got, summary, isErr = callLiveTool(t, "breeze_get_logs", args)
	if isErr {
		t.Fatalf("trace-filtered breeze_get_logs failed: %s", summary)
	}
	entries, _ = got["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("the app-level trace filter matched %d lines, want 1: %v", len(entries), entries)
	}
	row, _ := entries[0].(map[string]any)
	if id, _ := row["trace_id"].(string); id != traceID {
		t.Errorf("trace_id = %q, want %q", id, traceID)
	}

	// The error level is a separate buffer, and the trace's failing line is in it.
	args = creds(f.url)
	args["trace_id"] = traceID
	args["level"] = "error"
	got, summary, isErr = callLiveTool(t, "breeze_get_logs", args)
	if isErr {
		t.Fatalf("error-level breeze_get_logs failed: %s", summary)
	}
	entries, _ = got["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("the error-level trace filter matched %d lines, want 1: %v", len(entries), entries)
	}
	row, _ = entries[0].(map[string]any)
	if msg, _ := row["message"].(string); !strings.Contains(msg, "refused the charge") {
		t.Errorf("message = %q, want the gateway failure", msg)
	}
}

// TestGetLogsExplainsAnEmptyResultRatherThanReturningBareNothing pins the
// behaviour that a caller cannot get from the raw endpoint: an empty array is
// ambiguous, and the tool has to say which kind of empty it is.
func TestGetLogsExplainsAnEmptyResultRatherThanReturningBareNothing(t *testing.T) {
	f := startFixture(t)

	args := creds(f.url)
	args["trace_id"] = "no-such-trace-id-exists-anywhere"
	got, summary, isErr := callLiveTool(t, "breeze_get_logs", args)
	if isErr {
		t.Fatalf("breeze_get_logs failed: %s", summary)
	}
	if count, _ := got["count"].(float64); count != 0 {
		t.Fatalf("count = %v, want 0 for an unknown trace", got["count"])
	}

	notes, ok := got["notes"].([]any)
	if !ok || len(notes) == 0 {
		t.Fatal("an empty result carried no explanation, so the caller cannot tell " +
			"'wrong trace id' from 'no logging configured'")
	}
	note, _ := notes[0].(string)
	if !strings.Contains(note, "trace id") {
		t.Errorf("the note does not explain the trace-id case: %q", note)
	}
}

// TestLiveToolsRejectMissingCredentialsAsUnauthorizedNotAsMissing is the
// distinction the transport exists to make. A protected endpoint answering 401
// must never be reported as "the dashboard is not installed": that would send an
// agent off to install a feature that is already there.
func TestLiveToolsRejectMissingCredentialsAsUnauthorizedNotAsMissing(t *testing.T) {
	f := startFixture(t)

	got, summary, isErr := callLiveTool(t, "breeze_get_routes", map[string]any{
		"service_url": f.url,
	})
	if !isErr {
		t.Fatalf("an uncredentialed call succeeded against an authenticated dashboard: %s", summary)
	}
	if kind, _ := got["error"].(string); kind != "unauthorized" {
		t.Errorf("error kind = %q, want unauthorized (got summary %q)", kind, summary)
	}
	detail, _ := got["detail"].(string)
	if strings.Contains(strings.ToLower(detail), "not installed") {
		t.Errorf("an auth failure was described as a missing feature: %q", detail)
	}
	if !strings.Contains(detail, "username") && !strings.Contains(detail, "password") {
		t.Errorf("the message does not say which credentials to pass: %q", detail)
	}
}

// TestLiveToolsReportAMissingFeatureAsMissing is the same distinction from the
// other side: a real service that simply has no dashboard at that base path.
func TestLiveToolsReportAMissingFeatureAsMissing(t *testing.T) {
	f := startFixture(t)

	args := creds(f.url)
	args["base_path"] = "/no-dashboard-here"
	got, summary, isErr := callLiveTool(t, "breeze_get_routes", args)
	if !isErr {
		t.Fatalf("reading a base path that does not exist succeeded: %s", summary)
	}
	if kind, _ := got["error"].(string); kind != "missing" {
		t.Errorf("error kind = %q, want missing (summary %q)", kind, summary)
	}
	detail, _ := got["detail"].(string)
	if !strings.Contains(detail, "base path") && !strings.Contains(detail, "not installed") {
		t.Errorf("the message does not suggest the two real causes: %q", detail)
	}
}

// TestLiveToolsReportAClosedPortAsUnreachable covers the failure an agent hits
// most often: it forgot to start the service.
func TestLiveToolsReportAClosedPortAsUnreachable(t *testing.T) {
	// Reserved and released, so nothing is listening.
	port := freeLivePort(t)

	got, summary, isErr := callLiveTool(t, "breeze_get_routes", map[string]any{
		"service_url": "http://127.0.0.1:" + strconv.Itoa(port),
	})
	if !isErr {
		t.Fatalf("reading a closed port succeeded: %s", summary)
	}
	if kind, _ := got["error"].(string); kind != "unreachable" {
		t.Errorf("error kind = %q, want unreachable (summary %q)", kind, summary)
	}
	detail, _ := got["detail"].(string)
	if !strings.Contains(detail, "Start the service") {
		t.Errorf("the message does not tell the caller to start the service: %q", detail)
	}
}

// TestLiveToolsReportANonBreezeListenerAsMalformed is the subtle one. Something
// answers, and it answers 200, but it is not this endpoint — the classic
// wrong-port mistake. Reporting "0 routes" here would be a false finding, so the
// tool has to name the real cause.
func TestLiveToolsReportANonBreezeListenerAsMalformed(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				_, _ = c.Read(buf)
				const body = "<html><body>some other server entirely</body></html>"
				_, _ = fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Type: text/html\r\n"+
					"Content-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
			}(conn)
		}
	}()

	port := ln.Addr().(*net.TCPAddr).Port
	got, summary, isErr := callLiveTool(t, "breeze_get_routes", map[string]any{
		"service_url": "http://127.0.0.1:" + strconv.Itoa(port),
	})
	if !isErr {
		t.Fatalf("an HTML response was accepted as a routes listing: %s", summary)
	}
	if kind, _ := got["error"].(string); kind != "malformed" {
		t.Errorf("error kind = %q, want malformed (summary %q)", kind, summary)
	}
	detail, _ := got["detail"].(string)
	if !strings.Contains(detail, "not the") {
		t.Errorf("the message does not suggest the wrong-port cause: %q", detail)
	}
}

func TestLiveToolsRejectAnUnusableServiceURL(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  string
		want string
	}{
		{"empty", "", "required"},
		{"scheme-only", "ftp://example.com", "only http and https"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, summary, isErr := callLiveTool(t, "breeze_get_routes", map[string]any{
				"service_url": tc.url,
			})
			if !isErr {
				t.Fatalf("%q was accepted: %s", tc.url, summary)
			}
			detail, _ := got["detail"].(string)
			if !strings.Contains(detail, tc.want) {
				t.Errorf("detail = %q, want it to mention %q", detail, tc.want)
			}
		})
	}
}

// TestDefaultDashboardBaseMatchesTheDashboardsOwnDefault guards the one piece of
// duplication these tools require.
//
// The base path is hardcoded in internal/mcp because importing package dashboard
// there would create an import cycle. That duplication is only safe if it is
// checked: if the dashboard's default ever moves, every live tool would silently
// send its requests to the wrong path and report a missing feature. This test is
// the thing that turns that into a build failure instead.
func TestDefaultDashboardBaseMatchesTheDashboardsOwnDefault(t *testing.T) {
	got, _, isErr := callLiveTool(t, "breeze_get_routes", map[string]any{
		"service_url": "http://127.0.0.1:1",
	})
	if !isErr {
		t.Fatal("expected the unreachable placeholder to fail")
	}

	// The failure names the URL it tried, which is where the default base path
	// becomes observable from outside the package.
	detail, _ := got["detail"].(string)
	want := strings.TrimSuffix(dashboard.DefaultConfig().BasePath, "/") + "/api/routes"
	if !strings.Contains(detail, want) {
		t.Errorf("the live tools do not use the dashboard's default base path %q.\n"+
			"The attempted URL was: %s", want, detail)
	}
}

// TestLiveResponseShapesMatchTheDashboardsOwnTypes is the other half of the
// decoupling argument.
//
// internal/mcp declares its own structs for these responses rather than
// importing dashboard's, so nothing would otherwise notice if a json tag on the
// dashboard side were renamed — the tool would just start reporting zeroes. This
// marshals the dashboard's real types and asserts the keys the tools read are
// actually present.
func TestLiveResponseShapesMatchTheDashboardsOwnTypes(t *testing.T) {
	assertKeys := func(what string, v any, keys ...string) {
		t.Helper()
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal %s: %v", what, err)
		}
		var shape map[string]any
		if err := json.Unmarshal(raw, &shape); err != nil {
			t.Fatalf("decode %s: %v", what, err)
		}
		for _, key := range keys {
			if _, ok := shape[key]; !ok {
				t.Errorf("%s no longer serialises %q; the matching field in "+
					"internal/mcp must be updated or the tool will report zeroes.\nGot: %v",
					what, key, shape)
			}
		}
	}

	assertKeys("dashboard.RouteStat", dashboard.RouteStat{},
		"method", "pattern", "controller", "middleware",
		"requests", "avg_latency_ms", "max_latency_ms", "last_request", "errors",
		// documented has no omitempty on purpose: false is a meaningful answer,
		// so the key must be present even on a zero value.
		"documented")

	// The three description fields are omitempty, so a zero RouteStat does not
	// carry them — which is why they are asserted against a populated one. These
	// are the Part 5 fields: the sentence the developer wrote reaching an agent.
	assertKeys("dashboard.RouteStat (documented)", dashboard.RouteStat{
		Summary: "Create an order", Description: "Places an order.", Tags: []string{"Orders"},
	}, "summary", "description", "tags")

	assertKeys("dashboard.RequestRecord", dashboard.RequestRecord{},
		"id", "time", "method", "path", "route", "status", "duration_ms", "ip", "user")

	assertKeys("dashboard.LogEntry", dashboard.LogEntry{
		Message: "x", Level: "app", TraceID: "t", Source: "s.go:1",
	}, "time", "level", "message", "source", "trace_id")

	assertKeys("dashboard.PerfMetrics", dashboard.PerfMetrics{},
		"goroutines", "heap", "stack", "gc", "memory", "cpu", "runtime_tuning")

	assertKeys("dashboard.HeapStats", dashboard.HeapStats{},
		"alloc", "total_alloc", "sys", "objects")

	assertKeys("dashboard.GCStats", dashboard.GCStats{},
		"num_gc", "pause_total_ns", "pause_ns", "cpu_fraction")

	assertKeys("dashboard.MemoryStats", dashboard.MemoryStats{},
		"sys", "heap_in_use", "usage_pct")

	assertKeys("dashboard.CPUStats", dashboard.CPUStats{},
		"num_cpu", "gomaxprocs", "usage_pct")

	assertKeys("dashboard.RuntimeTuning", dashboard.RuntimeTuning{},
		"gogc", "gomemlimit")
}
