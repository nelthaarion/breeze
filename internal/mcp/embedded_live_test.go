package mcp_test

// Tests for an embedded endpoint reading the dashboard of its own process.
//
// Every tool here is called three ways, because each answers a different question:
//
//   - over HTTP, as a standalone server reads a service: the baseline;
//   - directly, on a server built by NewEmbeddedServer: the drift test, which holds the two
//     transports to one answer for the same collector;
//   - through NewInProcess, the constructor the running endpoint calls: the test that would
//     have caught the last embedded behaviour that passed in a unit test and failed in a
//     generated application.
//
// The service is real: a real dashboard installed in a real app, driven with the
// framework's own client, an event bus whose listener really fails, health checks that
// really report red. A stub would only prove the mirror types match themselves.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nelthaarion/breeze/v2"
	"github.com/nelthaarion/breeze/v2/client"
	"github.com/nelthaarion/breeze/v2/dashboard"
	"github.com/nelthaarion/breeze/v2/events"
	"github.com/nelthaarion/breeze/v2/internal/livesource"
	"github.com/nelthaarion/breeze/v2/internal/mcp"
	middleware "github.com/nelthaarion/breeze/v2/middlewares"
)

// A name no other test uses, so a line or a signal found in an answer was put there by this file.
const (
	liveMarker  = "embedded-live-marker-7f3a"
	liveTraceID = "trace-embedded-live-0001"
	liveSecret  = "pan-4242-must-not-leave-the-process"
)

type (
	liveOrderPlaced struct{ ID, Card string }
	liveUserJoined  struct{ Name string }
)

type localFixtureService struct {
	url  string
	coll *dashboard.Collector
}

var (
	localOnce sync.Once
	localFx   localFixtureService
)

// startLocalFixture boots one wired dashboard for this file and returns its collector, which
// the shared fixtures do not: a direct read needs to be pointed at a particular collector,
// because a test process installs several dashboards and the last one wins.
func startLocalFixture(t *testing.T) localFixtureService {
	t.Helper()

	localOnce.Do(func() {
		port := freeLivePort(t)
		router := breeze.NewRouter()
		app := breeze.New(router, breeze.NewEventLoopWorkerPool(runtime.NumCPU()))

		cfg := dashboard.DefaultConfig()
		cfg.DisableAuth = true
		coll := dashboard.Install(app, router, cfg)
		router.Use(dashboard.Middleware(coll))

		bus := events.New()
		// With payloads on, so the secret really is in the dashboard's data and the tool's refusal
		// to carry it is something this test can see.
		coll.AttachEventsWithPayload(bus)
		events.OnBus(bus, liveOrderPlaced{}, func(_ *events.Context, e liveOrderPlaced) error {
			return errors.New("charge declined for " + e.ID)
		})
		events.OnBus(bus, liveUserJoined{}, func(_ *events.Context, _ liveUserJoined) error { return nil })
		for i := 0; i < 3; i++ {
			_ = events.EmitBus(bus, liveOrderPlaced{ID: fmt.Sprintf("order-%d", i), Card: liveSecret})
		}
		for i := 0; i < 4; i++ {
			_ = events.EmitBus(bus, liveUserJoined{Name: fmt.Sprintf("user-%d", i)})
		}

		coll.RegisterHealthCheck("postgres", func() (string, string) { return "red", "connection refused" })
		coll.RegisterHealthCheck("redis", func() (string, string) { return "green", "PONG" })
		coll.RegisterConnection(dashboard.Connection{
			ID: "primary-db", Name: "PostgreSQL Primary", Type: dashboard.ConnDatabase,
			Driver: "postgres", Host: "10.0.0.5:5432", Status: dashboard.StatusConnected,
		})
		coll.UpdateConnectionStatus("primary-db", dashboard.StatusDisconnected, 0, "dial tcp 10.0.0.5:5432: i/o timeout")

		// Lines on three tabs, two of them one request's: what a trace_id read has to merge.
		now := time.Now()
		coll.RecordLog("app", dashboard.LogEntry{Time: now.Add(-3 * time.Second), Message: liveMarker + " charging card", Source: "orders", TraceID: liveTraceID})
		coll.RecordLog("error", dashboard.LogEntry{Time: now.Add(-2 * time.Second), Message: liveMarker + " charge failed", Source: "orders", TraceID: liveTraceID})
		coll.RecordLog("warning", dashboard.LogEntry{Time: now.Add(-1 * time.Second), Message: liveMarker + " unrelated warning", Source: "billing"})

		router.Handle(breeze.GET, "/api/orders", func(ctx *breeze.Context) error {
			return ctx.JSON(map[string]any{"orders": []string{"a", "b"}})
		})
		router.Handle(breeze.GET, "/api/boom", func(ctx *breeze.Context) error {
			ctx.Status(500)
			return ctx.JSON(map[string]any{"error": "deliberate failure"})
		})

		go func() { _ = app.Run(port, false) }()
		waitForLivePort(t, port, 15*time.Second)

		localFx = localFixtureService{url: "http://127.0.0.1:" + strconv.Itoa(port), coll: coll}

		c := client.New(client.Config{Timeout: 5 * time.Second})
		defer c.Close()
		for _, path := range []string{"/api/orders", "/api/orders", "/api/boom", "/api/boom"} {
			if _, err := c.Do(client.NewRequest("GET", localFx.url+path, nil)); err != nil {
				t.Fatalf("drive %s: %v", path, err)
			}
		}
		// The middleware records after the response is written.
		time.Sleep(200 * time.Millisecond)
	})

	if localFx.coll == nil {
		t.Fatal("the local fixture failed to start")
	}
	// Whatever another test installed since, this is the dashboard under test.
	localFx.coll.MakeCurrent()
	return localFx
}

// drop removes every key named in volatile, at any depth: the fields that are legitimately
// different between two reads of the same collector a moment apart.
func drop(v any, volatile map[string]bool) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if volatile[k] {
				continue
			}
			out[k] = drop(val, volatile)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = drop(val, volatile)
		}
		return out
	}
	return v
}

func canonical(t *testing.T, v any, volatile ...string) string {
	t.Helper()
	skip := map[string]bool{"service_url": true}
	for _, k := range volatile {
		skip[k] = true
	}
	b, err := json.Marshal(drop(v, skip))
	if err != nil {
		t.Fatalf("encoding a result: %v", err)
	}
	return string(b)
}

// liveToolCase is one tool with the arguments it is read with and what may differ.
type liveToolCase struct {
	name     string
	args     map[string]any
	volatile []string
	// substantial says what the answer must contain, so that two empty answers cannot agree.
	substantial func(t *testing.T, got map[string]any)
}

func liveToolCases() []liveToolCase {
	hasRows := func(key string) func(*testing.T, map[string]any) {
		return func(t *testing.T, got map[string]any) {
			t.Helper()
			if rows := asRows(t, got[key]); len(rows) == 0 {
				t.Errorf("%s is empty, so agreeing on it proves nothing: %v", key, got)
			}
		}
	}
	return []liveToolCase{
		{name: "breeze_get_routes", substantial: hasRows("routes")},
		{name: "breeze_get_recent_errors", substantial: hasRows("errors")},
		{
			name: "breeze_get_logs", args: map[string]any{"level": "all", "query": liveMarker},
			substantial: func(t *testing.T, got map[string]any) {
				t.Helper()
				if n, _ := got["count"].(float64); n != 3 {
					t.Errorf("count = %v, want the three marker lines on three tabs: %v", got["count"], got)
				}
			},
		},
		{
			name: "breeze_get_signals", args: map[string]any{"failed": false},
			// The rate over the last minute is computed when it is read.
			volatile: []string{"rate_per_sec"}, substantial: hasRows("recent"),
		},
		{
			name: "breeze_get_health",
			// Latency and the time of the check are measured when the check runs.
			volatile: []string{"latency_us", "checked", "last_check", "last_checked", "latency_ms"}, substantial: hasRows("checks"),
		},
		{
			name: "breeze_get_performance",
			// Runtime counters move between any two reads; the performance test below compares
			// their structure and what does not move.
			substantial: func(t *testing.T, got map[string]any) {
				t.Helper()
				if _, ok := got["current"].(map[string]any); !ok {
					t.Errorf("no current readings: %v", got)
				}
			},
		},
	}
}

// TestEmbeddedLiveToolsMatchWhatTheDashboardServes holds the in-memory read to the HTTP one,
// tool by tool, on one collector. A second route to the same data is only safe while something
// fails when the two stop agreeing.
func TestEmbeddedLiveToolsMatchWhatTheDashboardServes(t *testing.T) {
	f := startLocalFixture(t)

	for _, tc := range liveToolCases() {
		if tc.name == "breeze_get_performance" {
			continue // its own test: every number in it moves
		}
		t.Run(tc.name, func(t *testing.T) {
			f.coll.MakeCurrent()

			httpArgs := map[string]any{"service_url": f.url}
			for k, v := range tc.args {
				httpArgs[k] = v
			}
			viaHTTP, summary, isErr := callLiveTool(t, tc.name, httpArgs)
			if isErr {
				t.Fatalf("over HTTP: %s", summary)
			}
			direct, summary, isErr := callLiveToolOn(t, embeddedServer(), tc.name, tc.args)
			if isErr {
				t.Fatalf("in-process: %s", summary)
			}

			tc.substantial(t, viaHTTP)
			tc.substantial(t, direct)

			if direct["service_url"] != "(this process)" {
				t.Errorf("service_url = %v, want the answer labelled as this process", direct["service_url"])
			}
			if viaHTTP["service_url"] != f.url {
				t.Errorf("service_url over HTTP = %v, want %s", viaHTTP["service_url"], f.url)
			}
			if h, d := canonical(t, viaHTTP, tc.volatile...), canonical(t, direct, tc.volatile...); h != d {
				t.Errorf("the two transports disagree\n  HTTP:       %s\n  in-process: %s", h, d)
			}
		})
	}
}

// TestEmbeddedPerformanceMatchesInShapeAndInWhatDoesNotMove: the runtime's counters change
// between any two reads, so equality is the wrong test. The two have to carry the same
// fields, and the numbers that are fixed for the life of the process have to agree.
func TestEmbeddedPerformanceMatchesInShapeAndInWhatDoesNotMove(t *testing.T) {
	f := startLocalFixture(t)

	viaHTTP, summary, isErr := callLiveTool(t, "breeze_get_performance", map[string]any{"service_url": f.url})
	if isErr {
		t.Fatalf("over HTTP: %s", summary)
	}
	direct, summary, isErr := callLiveToolOn(t, embeddedServer(), "breeze_get_performance", map[string]any{})
	if isErr {
		t.Fatalf("in-process: %s", summary)
	}

	var shape func(v any) any
	shape = func(v any) any {
		switch x := v.(type) {
		case map[string]any:
			out := map[string]any{}
			for k, val := range x {
				out[k] = shape(val)
			}
			return out
		case []any:
			return "array"
		case float64:
			return "number"
		}
		return fmt.Sprintf("%T", v)
	}
	if h, d := canonical(t, shape(viaHTTP["current"])), canonical(t, shape(direct["current"])); h != d {
		t.Errorf("the readings carry different fields\n  HTTP:       %s\n  in-process: %s", h, d)
	}

	fixed := func(got map[string]any) string {
		cur, _ := got["current"].(map[string]any)
		cpu, _ := cur["cpu"].(map[string]any)
		tuning, _ := cur["runtime_tuning"].(map[string]any)
		return fmt.Sprint(cpu["num_cpu"], cpu["gomaxprocs"], tuning["gogc"], tuning["gomemlimit"])
	}
	if h, d := fixed(viaHTTP), fixed(direct); h != d || strings.TrimSpace(h) == "<nil> <nil> <nil> <nil>" {
		t.Errorf("settings that do not change disagree or are missing: HTTP %q, in-process %q", h, d)
	}
	if g, _ := direct["current"].(map[string]any)["goroutines"].(float64); g < 1 {
		t.Errorf("an in-process read reports %v goroutines", g)
	}
}

// TestEmbeddedSignalsStillLeaveEventPayloadsOut: the dashboard serves an event's payload to its
// own UI and, here, really holds one with a card number in it. The tool omits it by not declaring
// it, so reading from memory must not have opened a way around that.
func TestEmbeddedSignalsStillLeaveEventPayloadsOut(t *testing.T) {
	f := startLocalFixture(t)

	// The premise: the dashboard does hold the secret, or the assertion below is vacuous.
	raw, err := httpGetBody(f.url + "/dashboard/api/events?failed=1")
	if err != nil {
		t.Fatalf("reading the events endpoint: %v", err)
	}
	if !strings.Contains(raw, liveSecret) {
		t.Fatalf("the fixture's dashboard does not hold the payload, so this test proves nothing: %.300s", raw)
	}

	got, summary, isErr := callLiveToolOn(t, embeddedServer(), "breeze_get_signals", map[string]any{"failed": false})
	if isErr {
		t.Fatalf("breeze_get_signals in-process: %s", summary)
	}
	text, _ := jsonText(got)
	if strings.Contains(text, liveSecret) {
		t.Errorf("an event payload left the process through the tool: %.300s", text)
	}
	if !strings.Contains(text, "charge declined") {
		t.Errorf("the failing listener's error is missing from the answer: %.300s", text)
	}
}

// TestEmbeddedLogsReadEveryTabForATraceID: the merged read for one request's lines (the
// behaviour breeze_get_logs added for trace_id) is done by the tool, above the transport, so it
// must still happen when the transport is memory.
func TestEmbeddedLogsReadEveryTabForATraceID(t *testing.T) {
	f := startLocalFixture(t)
	f.coll.MakeCurrent()

	got, summary, isErr := callLiveToolOn(t, embeddedServer(), "breeze_get_logs", map[string]any{"trace_id": liveTraceID})
	if isErr {
		t.Fatalf("breeze_get_logs in-process: %s", summary)
	}
	rows := asRows(t, got["entries"])
	if len(rows) != 2 {
		t.Fatalf("entries = %v, want the app line and the error line of this trace", got["entries"])
	}
	if got["level"] != "all" {
		t.Errorf("level = %v, want all: a trace id with no level reads every tab", got["level"])
	}
	if first, _ := rows[0]["message"].(string); !strings.Contains(first, "charging card") {
		t.Errorf("the lines are not in the order they were logged: %v", rows)
	}
}

// TestEmbeddedToolsNeedNoServiceURLAndSayWhenThereIsNoDashboard.
func TestEmbeddedToolsNeedNoServiceURLAndSayWhenThereIsNoDashboard(t *testing.T) {
	f := startLocalFixture(t)
	t.Cleanup(f.coll.MakeCurrent)

	livesource.Set(nil)
	for _, tc := range liveToolCases() {
		got, summary, isErr := callLiveToolOn(t, embeddedServer(), tc.name, tc.args)
		if !isErr {
			t.Errorf("%s answered with no dashboard in the process: %v", tc.name, got)
			continue
		}
		for _, want := range []string{"no dashboard is installed in this process", "service_url"} {
			if !strings.Contains(summary, want) {
				t.Errorf("%s: the refusal %q does not say %q", tc.name, summary, want)
			}
		}
		if got["error"] != "missing" {
			t.Errorf("%s: error kind = %v, want missing", tc.name, got["error"])
		}
	}
}

// TestOnlyAnEmbeddedServerReadsItsOwnDashboard: a standalone breeze-mcp has no dashboard of its
// own; in this test process there is one, which is how a standalone server that took the same
// shortcut would be caught doing it.
func TestOnlyAnEmbeddedServerReadsItsOwnDashboard(t *testing.T) {
	f := startLocalFixture(t)
	f.coll.MakeCurrent()

	for _, tc := range liveToolCases() {
		_, summary, isErr := callLiveTool(t, tc.name, tc.args)
		if !isErr || !strings.Contains(summary, "service URL is required") {
			t.Errorf("%s on a standalone server with no service_url: isErr=%v summary=%q", tc.name, isErr, summary)
		}
	}
}

// TestEmbeddedToolsStillReadAnotherServiceOverHTTP: given a URL the tool reads that service, and
// not this process, which here has a different dashboard.
func TestEmbeddedToolsStillReadAnotherServiceOverHTTP(t *testing.T) {
	f := startFixture(t)
	local := startLocalFixture(t)
	local.coll.MakeCurrent()

	got, summary, isErr := callLiveToolOn(t, embeddedServer(), "breeze_get_routes", creds(f.url))
	if isErr {
		t.Fatalf("an embedded server could not read another service: %s", summary)
	}
	if got["service_url"] != f.url {
		t.Errorf("service_url = %v, want %s", got["service_url"], f.url)
	}
	text, _ := jsonText(got)
	if !strings.Contains(text, "/api/widgets") || strings.Contains(text, "/api/orders") {
		t.Errorf("the routes are not the other service's: %.300s", text)
	}
}

// TestEmbeddedSchemasNoLongerRequireAServiceURL: the schema is what a client shows an agent, and
// an agent told the URL is required will go looking for one.
func TestEmbeddedSchemasNoLongerRequireAServiceURL(t *testing.T) {
	list := func(srv *mcp.Server) map[string][]string {
		out := srv.RPCServer().Handle([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		var resp struct {
			Result struct {
				Tools []struct {
					Name        string `json:"name"`
					Description string `json:"description"`
					InputSchema struct {
						Required []string `json:"required"`
					} `json:"inputSchema"`
				} `json:"tools"`
			} `json:"result"`
		}
		if err := json.Unmarshal(out, &resp); err != nil {
			t.Fatalf("tools/list: %v", err)
		}
		got := map[string][]string{}
		for _, tl := range resp.Result.Tools {
			got[tl.Name] = append([]string{tl.Description}, tl.InputSchema.Required...)
		}
		return got
	}

	standalone, embedded := list(mcp.NewServer("test")), list(embeddedServer())
	for _, tc := range liveToolCases() {
		s, e := standalone[tc.name], embedded[tc.name]
		if len(s) < 2 || s[1] != "service_url" {
			t.Errorf("%s: a standalone server's required arguments = %v, want service_url", tc.name, s[1:])
		}
		if len(e) != 1 {
			t.Errorf("%s: an embedded server still requires %v", tc.name, e[1:])
		}
		if len(e) > 0 && !strings.Contains(e[0], "omit service_url") {
			t.Errorf("%s: the embedded description does not say service_url can be omitted", tc.name)
		}
		if len(s) > 0 && strings.Contains(s[0], "omit service_url") {
			t.Errorf("%s: a standalone description claims a shortcut it does not have", tc.name)
		}
	}
	// breeze_query_openapi reads a document, not the dashboard, and has no local form.
	if e := embedded["breeze_query_openapi"]; len(e) < 2 || e[1] != "service_url" {
		t.Errorf("breeze_query_openapi lost its required service_url on an embedded server: %v", e)
	}
}

// ── through NewInProcess ─────────────────────────────────────────────────────

// callThroughEndpoint makes a real MCP call to a server NewInProcess built: initialize, then
// tools/call, with the bearer token, over the endpoint's own HTTP handler.
func callThroughEndpoint(t *testing.T, srv *httptest.Server, token, tool string, args map[string]any) (map[string]any, string, bool) {
	t.Helper()

	post := func(session string, body any) *http.Response {
		raw, _ := json.Marshal(body)
		req, err := http.NewRequest(http.MethodPost, srv.URL+mcp.DefaultEndpointPath, strings.NewReader(string(raw)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer "+token)
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}

	init := post("", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-03-26", "capabilities": map[string]any{},
			"clientInfo": map[string]any{"name": "test", "version": "1"},
		},
	})
	session := init.Header.Get("Mcp-Session-Id")
	if init.StatusCode != http.StatusOK || session == "" {
		body, _ := io.ReadAll(init.Body)
		t.Fatalf("initialize: status %d, session %q: %s", init.StatusCode, session, body)
	}

	resp := post(session, map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	})
	body, _ := io.ReadAll(resp.Body)
	var envelope struct {
		Result struct {
			Content           []struct{ Text string } `json:"content"`
			StructuredContent map[string]any          `json:"structuredContent"`
			IsError           bool                    `json:"isError"`
		} `json:"result"`
		Error *struct{ Message string } `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decoding %s: %v", body, err)
	}
	if envelope.Error != nil {
		t.Fatalf("%s: JSON-RPC error: %s", tool, envelope.Error.Message)
	}
	summary := ""
	if len(envelope.Result.Content) > 0 {
		summary = envelope.Result.Content[0].Text
	}
	return envelope.Result.StructuredContent, summary, envelope.Result.IsError
}

// TestNewInProcessReadsItsOwnDashboardForEveryLiveTool goes through NewInProcess, the function
// StartInProcess calls. The behaviour for breeze_diagnose_service was first written into a
// constructor the endpoint never calls, and passed its tests while a generated application
// refused the call. This is the same question asked of the real one, for each tool.
func TestNewInProcessReadsItsOwnDashboardForEveryLiveTool(t *testing.T) {
	f := startLocalFixture(t)
	f.coll.MakeCurrent()

	port := freeLivePort(t)
	ns, token, err := mcp.NewInProcess("test", mcp.InProcessConfig{Mode: mcp.ModeAppRuntime, Port: port})
	if err != nil {
		t.Fatalf("NewInProcess: %v", err)
	}
	t.Cleanup(func() { _ = ns.Close() })
	srv := httptest.NewServer(ns.Handler())
	t.Cleanup(srv.Close)

	for _, tc := range liveToolCases() {
		t.Run(tc.name, func(t *testing.T) {
			got, summary, isErr := callThroughEndpoint(t, srv, token, tc.name, tc.args)
			if isErr {
				t.Fatalf("the endpoint StartInProcess serves cannot read its own process: %s", summary)
			}
			if got["service_url"] != "(this process)" {
				t.Errorf("service_url = %v, want this process", got["service_url"])
			}
			tc.substantial(t, got)
		})
	}
}

// ── the rate limiter ─────────────────────────────────────────────────────────

var (
	limitedOnce sync.Once
	limitedFx   localFixtureService
)

// startLimitedService is a service whose limiter has been used up: five requests a minute, a
// dashboard behind it exactly as in a generated application, and a burst already sent.
func startLimitedService(t *testing.T) localFixtureService {
	t.Helper()

	limitedOnce.Do(func() {
		port := freeLivePort(t)
		router := breeze.NewRouter()
		app := breeze.New(router, breeze.NewEventLoopWorkerPool(runtime.NumCPU()))

		cfg := dashboard.DefaultConfig()
		cfg.DisableAuth = true
		coll := dashboard.Install(app, router, cfg)

		// The order a generated application uses: the limiter, then the dashboard's middleware.
		router.Use(middleware.NewRateLimiter(middleware.RateLimiterOptions{Requests: 5, Per: time.Minute}))
		router.Use(dashboard.Middleware(coll))
		router.Handle(breeze.GET, "/api/ping", func(ctx *breeze.Context) error {
			return ctx.JSON(map[string]any{"ok": true})
		})
		coll.PushLog("info", liveMarker+" the service is serving", "app")

		go func() { _ = app.Run(port, false) }()
		waitForLivePort(t, port, 15*time.Second)
		limitedFx = localFixtureService{url: "http://127.0.0.1:" + strconv.Itoa(port), coll: coll}

		c := client.New(client.Config{Timeout: 5 * time.Second})
		defer c.Close()
		var statuses []int
		for i := 0; i < 12; i++ {
			resp, err := c.Do(client.NewRequest("GET", limitedFx.url+"/api/ping", nil))
			if err != nil {
				t.Fatalf("burst request %d: %v", i, err)
			}
			statuses = append(statuses, resp.Status)
		}
		// The premise of everything this service is for: the limiter is rejecting.
		if statuses[len(statuses)-1] != 429 {
			t.Fatalf("the burst did not exhaust the limiter, so the fixture is not the situation under test: %v", statuses)
		}
	})
	return limitedFx
}

// TestEmbeddedReadsSurviveTheRateLimiterThatRefusesHTTPOnes reproduces what was measured: after a
// burst past the limit, the dashboard's HTTP API answered 429 and the logs tool failed, while a read
// from memory went on working.
func TestEmbeddedReadsSurviveTheRateLimiterThatRefusesHTTPOnes(t *testing.T) {
	f := startLimitedService(t)
	f.coll.MakeCurrent()

	_, summary, isErr := callLiveTool(t, "breeze_get_logs", map[string]any{"service_url": f.url, "level": "all"})
	if !isErr || !strings.Contains(summary, "rate") {
		t.Fatalf("over HTTP the call was not refused by the limiter (isErr=%v): %s", isErr, summary)
	}
	if got, _, _ := callLiveTool(t, "breeze_get_logs", map[string]any{"service_url": f.url, "level": "all"}); got["error"] != "rate_limited" {
		t.Errorf("error kind over HTTP = %v, want rate_limited", got["error"])
	}

	for _, tc := range []liveToolCase{
		{name: "breeze_get_logs", args: map[string]any{"level": "all", "query": liveMarker}},
		{name: "breeze_get_recent_errors"},
		{name: "breeze_get_routes"},
		{name: "breeze_get_performance"},
		{name: "breeze_get_health"},
		{name: "breeze_get_signals"},
	} {
		got, summary, isErr := callLiveToolOn(t, embeddedServer(), tc.name, tc.args)
		if isErr {
			t.Errorf("%s was refused in-process while the limiter was rejecting: %s", tc.name, summary)
			continue
		}
		if tc.name == "breeze_get_logs" && got["count"] != float64(1) {
			t.Errorf("the service's log line was not read: %v", got)
		}
	}
}

// httpGetBody reads a URL with the standard library: the premise checks here ask what the dashboard
// itself serves, independently of the code under test.
func httpGetBody(url string) (string, error) {
	resp, err := http.Get(url) //nolint:gosec // loopback fixture
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}
