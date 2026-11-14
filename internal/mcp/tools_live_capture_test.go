package mcp_test

// What breeze_get_routes and breeze_get_recent_errors say once the dashboard counts every
// request. The services are real dashboards behind real servers, driven with the framework's own
// client: the claims these tools make about "zero" and "none" are claims about what the
// dashboard did, and a stub would only repeat the author's belief about it.

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nelthaarion/breeze/v2"
	"github.com/nelthaarion/breeze/v2/client"
	"github.com/nelthaarion/breeze/v2/dashboard"
)

type countingService struct {
	url  string
	coll *dashboard.Collector
	c    *client.Client
}

// startCountingService serves /api/ok (200), /api/missing (404), /api/boom (500) and
// /api/never (never called by these tests) behind a real dashboard. With instrument false the
// dashboard is installed and its middleware is not, which is the misconfiguration the tools must
// recognise.
func startCountingService(t *testing.T, instrument bool) *countingService {
	t.Helper()
	return startCountingServiceWith(t, instrument, nil)
}

// startCountingServiceWith is startCountingService with the dashboard's configuration adjusted.
func startCountingServiceWith(t *testing.T, instrument bool, tune func(*dashboard.Config)) *countingService {
	t.Helper()

	port := freeLivePort(t)
	router := breeze.NewRouter()
	app := breeze.New(router, breeze.NewEventLoopWorkerPool(runtime.NumCPU()))

	cfg := dashboard.DefaultConfig()
	cfg.DisableAuth = true
	if tune != nil {
		tune(&cfg)
	}
	coll := dashboard.Install(app, router, cfg)
	if instrument {
		router.Use(dashboard.Middleware(coll))
	}

	router.Handle(breeze.GET, "/api/ok", func(ctx *breeze.Context) error { return ctx.JSON(map[string]any{"ok": true}) })
	router.Handle(breeze.GET, "/api/missing", func(ctx *breeze.Context) error {
		ctx.Status(404)
		return ctx.JSON(map[string]any{"error": "no such record"})
	})
	router.Handle(breeze.GET, "/api/boom", func(ctx *breeze.Context) error {
		ctx.Status(500)
		return ctx.JSON(map[string]any{"error": "deliberate"})
	})
	router.Handle(breeze.GET, "/api/never", func(ctx *breeze.Context) error { return ctx.JSON(map[string]any{}) })

	go func() { _ = app.Run(port, false) }()
	waitForLivePort(t, port, 15*time.Second)

	c := client.New(client.Config{Timeout: 5 * time.Second})
	t.Cleanup(func() { _ = c.Close() })
	return &countingService{url: "http://127.0.0.1:" + strconv.Itoa(port), coll: coll, c: c}
}

func (s *countingService) hit(t *testing.T, n int, path string) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := s.c.Do(client.NewRequest("GET", s.url+path, nil)); err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
	}
	// The middleware records after the response is written.
	time.Sleep(150 * time.Millisecond)
}

func routeNamed(t *testing.T, got map[string]any, pattern string) map[string]any {
	t.Helper()
	for _, r := range asRows(t, got["routes"]) {
		if r["pattern"] == pattern {
			return r
		}
	}
	t.Fatalf("no route %s in %v", pattern, got["routes"])
	return nil
}

func firstLine(s string) string { return strings.SplitN(s, "\n", 2)[0] }

func notesOf(got map[string]any) string {
	var b strings.Builder
	if notes, ok := got["notes"].([]any); ok {
		for _, n := range notes {
			fmt.Fprintln(&b, n)
		}
	}
	return b.String()
}

// The failure this started from: a route that served thousands of fast requests read as never called.
func TestGetRoutesCountsEveryRequestSoZeroMeansNeverCalled(t *testing.T) {
	svc := startCountingService(t, true)
	svc.hit(t, 37, "/api/ok") // fast, successful, nobody watching: none of them is kept as a record

	if recs := svc.coll.Requests(100); len(recs) != 0 {
		t.Fatalf("the premise is wrong: %d full records were kept for fast successful requests", len(recs))
	}

	got, summary, isErr := callLiveTool(t, "breeze_get_routes", map[string]any{"service_url": svc.url})
	if isErr {
		t.Fatalf("breeze_get_routes: %s", summary)
	}
	if got["counting"] != "exact" {
		t.Fatalf("counting = %v, want exact", got["counting"])
	}
	if r := routeNamed(t, got, "/api/ok"); r["requests"] != float64(37) {
		t.Errorf("/api/ok requests = %v, want exactly 37", r["requests"])
	}
	if r := routeNamed(t, got, "/api/never"); r["requests"] != float64(0) {
		t.Errorf("/api/never requests = %v, want 0", r["requests"])
	}
	if line := firstLine(summary); !strings.Contains(line, "never called") || strings.Contains(line, "not observed") {
		t.Errorf("the summary %q does not say never called for the unused routes", line)
	}
	if strings.Contains(notesOf(got), "not measured") {
		t.Errorf("the notes tell the reader to distrust exact counts: %s", notesOf(got))
	}
}

func TestGetRoutesReportsClientAndServerErrorsPerRoute(t *testing.T) {
	svc := startCountingService(t, true)
	svc.hit(t, 12, "/api/missing")
	svc.hit(t, 4, "/api/boom")

	got, summary, isErr := callLiveTool(t, "breeze_get_routes", map[string]any{"service_url": svc.url})
	if isErr {
		t.Fatalf("breeze_get_routes: %s", summary)
	}
	missing, boom := routeNamed(t, got, "/api/missing"), routeNamed(t, got, "/api/boom")
	if missing["client_errors"] != float64(12) || missing["errors"] != float64(0) {
		t.Errorf("/api/missing = %v, want 12 client errors and no server errors", missing)
	}
	if boom["errors"] != float64(4) || boom["client_errors"] != float64(0) {
		t.Errorf("/api/boom = %v, want 4 server errors and no client errors", boom)
	}
	if got["with_errors"] != float64(1) || got["with_client_errors"] != float64(1) {
		t.Errorf("with_errors = %v, with_client_errors = %v, want 1 and 1", got["with_errors"], got["with_client_errors"])
	}
}

// A dashboard whose middleware was never created counts nothing, and that must not read as a
// service nobody called.
func TestGetRoutesSaysNotMeasuredWhenTheDashboardIsNotCounting(t *testing.T) {
	svc := startCountingService(t, false)
	svc.hit(t, 25, "/api/ok")

	got, summary, isErr := callLiveTool(t, "breeze_get_routes", map[string]any{"service_url": svc.url})
	if isErr {
		t.Fatalf("breeze_get_routes: %s", summary)
	}
	if got["counting"] != "not_measured" {
		t.Fatalf("counting = %v, want not_measured", got["counting"])
	}
	if r := routeNamed(t, got, "/api/ok"); r["requests"] != float64(0) {
		t.Fatalf("the premise is wrong: nothing counts and /api/ok reports %v", r["requests"])
	}
	for _, want := range []string{"middleware is not", "dashboard.Middleware(coll)", "not measured"} {
		if !strings.Contains(notesOf(got), want) {
			t.Errorf("the notes do not say %q: %s", want, notesOf(got))
		}
	}
	// The first line is the tool's own summary; what follows it is the report, whose notes quote the phrase.
	if line := firstLine(summary); strings.Contains(line, "never called") {
		t.Errorf("the summary %q claims routes were never called when nothing was counting", line)
	}
	if line := firstLine(summary); !strings.Contains(line, "not observed") {
		t.Errorf("the summary %q does not say the routes are not observed", line)
	}
}

// A dashboard that is switched off counts nothing either, and the tool says that, not the other thing.
func TestGetRoutesSaysNotMeasuredWhenTheDashboardIsSwitchedOff(t *testing.T) {
	svc := startCountingServiceWith(t, true, func(c *dashboard.Config) { c.Enabled = false })
	svc.hit(t, 25, "/api/ok")

	got, summary, isErr := callLiveTool(t, "breeze_get_routes", map[string]any{"service_url": svc.url})
	if isErr {
		t.Fatalf("breeze_get_routes: %s", summary)
	}
	if got["counting"] != "not_measured" {
		t.Fatalf("counting = %v, want not_measured", got["counting"])
	}
	if !strings.Contains(notesOf(got), "Config.Enabled is false") {
		t.Errorf("the notes do not say the dashboard is switched off: %s", notesOf(got))
	}
}

// The framework answers an unrouted path before any middleware runs. The tools say so, so the
// claim is held here to what the framework does.
func TestRequestsToUnroutedPathsAreInvisibleToTheDashboardAndTheToolsSaySo(t *testing.T) {
	svc := startCountingService(t, true)
	svc.hit(t, 3, "/api/ok")

	before := svc.coll.Metrics().RequestsTotal
	resp, err := svc.c.Do(client.NewRequest("GET", svc.url+"/definitely/not/routed", nil))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if resp.Status != 404 {
		t.Fatalf("an unrouted path answered %d, want 404", resp.Status)
	}
	if after := svc.coll.Metrics().RequestsTotal; after != before {
		t.Fatalf("the dashboard counted an unrouted request (%d -> %d): the note that says it cannot is now wrong", before, after)
	}

	got, summary, isErr := callLiveTool(t, "breeze_get_recent_errors", map[string]any{"service_url": svc.url})
	if isErr {
		t.Fatalf("breeze_get_recent_errors: %s", summary)
	}
	if !strings.Contains(notesOf(got), "answered by the framework before any middleware runs") &&
		!strings.Contains(notesOf(got), "none failed") {
		t.Errorf("no note about requests the dashboard cannot see: %s", notesOf(got))
	}
	routes, _, _ := callLiveTool(t, "breeze_get_routes", map[string]any{"service_url": svc.url})
	if !strings.Contains(notesOf(routes), "answered by the framework before any middleware runs") {
		t.Errorf("breeze_get_routes does not say unrouted requests are not counted: %s", notesOf(routes))
	}
}

func TestGetRecentErrorsReportsExactCountsBesideTheSample(t *testing.T) {
	svc := startCountingService(t, true)
	svc.hit(t, 100, "/api/missing") // far over the budget of 20 per window
	svc.hit(t, 3, "/api/boom")      // under its budget: all kept
	svc.hit(t, 40, "/api/ok")

	got, summary, isErr := callLiveTool(t, "breeze_get_recent_errors", map[string]any{"service_url": svc.url})
	if isErr {
		t.Fatalf("breeze_get_recent_errors: %s", summary)
	}
	if got["counting"] != "exact" {
		t.Fatalf("counting = %v, want exact", got["counting"])
	}

	// The records are a sample: 20 of the 100 404s and all 3 of the 500s.
	if got["count"] != float64(23) {
		t.Errorf("count = %v, want the 23 records that were kept (20 + 3)", got["count"])
	}
	exact, ok := got["exact"].(map[string]any)
	if !ok {
		t.Fatalf("no exact block: %v", got)
	}
	if exact["client_errors"] != float64(100) || exact["server_errors"] != float64(3) {
		t.Errorf("exact = %v, want 100 client errors and 3 server errors", exact)
	}
	if exact["not_recorded"] != float64(80) {
		t.Errorf("not_recorded = %v, want the 80 404s over the budget", exact["not_recorded"])
	}
	if exact["requests"] != float64(143) {
		t.Errorf("requests = %v, want 143", exact["requests"])
	}

	routes := asRows(t, exact["routes"])
	if len(routes) != 2 || routes[0]["pattern"] != "/api/missing" || routes[0]["client_errors"] != float64(100) ||
		routes[0]["not_recorded"] != float64(80) {
		t.Errorf("exact.routes = %v, want /api/missing first with 100 client errors of which 80 were not kept", routes)
	}

	policy, _ := exact["capture"].(map[string]any)
	if policy["client_errors_per_window"] != float64(20) || policy["server_errors_per_window"] != float64(100) {
		t.Errorf("capture = %v, want the dashboard's own budgets", policy)
	}
	notes := notesOf(got)
	for _, want := range []string{"20 client errors (4xx)", "per route per 10s window", "80 of the 103 failure(s)"} {
		if !strings.Contains(notes, want) {
			t.Errorf("the notes do not say %q: %s", want, notes)
		}
	}
}

func TestGetRecentErrorsSaysNoFailureWasCountedOnlyWhenNoneWas(t *testing.T) {
	svc := startCountingService(t, true)
	svc.hit(t, 30, "/api/ok")

	got, summary, isErr := callLiveTool(t, "breeze_get_recent_errors", map[string]any{"service_url": svc.url})
	if isErr {
		t.Fatalf("breeze_get_recent_errors: %s", summary)
	}
	notes := notesOf(got)
	for _, want := range []string{"counted 30 request(s) and none failed", "evidence", "refused before the dashboard's middleware"} {
		if !strings.Contains(notes, want) {
			t.Errorf("the notes do not say %q: %s", want, notes)
		}
	}
	if strings.Contains(notes, "not evidence") {
		t.Errorf("the notes distrust a count that is exact: %s", notes)
	}
}

func TestGetRecentErrorsRefusesToCallAnUncountedServiceHealthy(t *testing.T) {
	svc := startCountingService(t, false)
	svc.hit(t, 10, "/api/boom")

	got, summary, isErr := callLiveTool(t, "breeze_get_recent_errors", map[string]any{"service_url": svc.url})
	if isErr {
		t.Fatalf("breeze_get_recent_errors: %s", summary)
	}
	if got["counting"] != "not_measured" || got["exact"] != nil {
		t.Errorf("counting = %v, exact = %v; want not_measured and no exact block", got["counting"], got["exact"])
	}
	if !strings.Contains(notesOf(got), "dashboard.Middleware(coll)") {
		t.Errorf("the notes do not name the missing middleware: %s", notesOf(got))
	}
}

// An older dashboard has no traffic endpoint. The tools must not claim an exactness it never had.
func TestRoutesAndErrorsOnAnOlderDashboardKeepTheOldCaveats(t *testing.T) {
	url := requestLogService(t, nil) // serves /dashboard/api/requests only: no /traffic

	errs, _, isErr := callLiveTool(t, "breeze_get_recent_errors", creds(url))
	if isErr {
		t.Fatal("breeze_get_recent_errors failed against an older dashboard")
	}
	if errs["counting"] != "partial" || errs["exact"] != nil {
		t.Errorf("counting = %v, exact = %v; want partial and no exact block", errs["counting"], errs["exact"])
	}
	if !strings.Contains(notesOf(errs), "has no exact counts") {
		t.Errorf("an older dashboard's answer does not say it has no exact counts: %s", notesOf(errs))
	}
}
