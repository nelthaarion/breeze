package mcp_test

// Tests for breeze_get_services, breeze_get_trace_logs and what breeze_explain_incident
// does with them.
//
// Like the rest of the fleet tests these run against a real aggregator with spans and
// heartbeats ingested over HTTP: the tools mirror the aggregator's JSON, and a stub would
// only prove the mirror matches itself. The scenario is the one the tools exist for, built
// the way it happens: gateway calls auth-service, which calls orders-service, and
// orders-service dies. It stops heartbeating, so the registry marks it down; it exports
// no spans, so the failing trace stops at auth-service.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goccy/go-json"

	"github.com/nelthaarion/breeze/v2"
	"github.com/nelthaarion/breeze/v2/dashboard"
	"github.com/nelthaarion/breeze/v2/fleet"
	"github.com/nelthaarion/breeze/v2/fleet/aggregator"
)

const (
	svcLogToken  = "svc-test-log-token"
	svcFailingID = "3df7651916cd43dd8448eb211c803300"

	// svcBillingFailID is a trace that fails at billing, which calls nobody. orders-service
	// is down but nothing ties it to this trace.
	svcBillingFailID = "4ef7651916cd43dd8448eb211c803311"

	// svcTTL is how long a service may be silent before the registry calls it down. The
	// registry forgets it entirely after twenty times this, so the fixture has about a
	// minute in which orders-service is down and still listed: more than the tests need,
	// and short enough that the fixture does not take long to make orders-service so.
	svcTTL = 3 * time.Second
)

var (
	svcOnce    sync.Once
	svcBase    string
	svcStartUp error
)

// stubDashboard serves one service's log tabs the way the real dashboard does: one tab per
// request, defaulting to app, filtered by the trace id it is asked for.
//
// It is never closed. Like the aggregator, it lives for the process, because the fixture
// is built once for the file and a Cleanup registered by whichever test got here first
// would close it under the rest.
func stubDashboard(lines []dashboard.LogEntry) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/api/logs") {
			http.NotFound(w, r)
			return
		}
		tab := r.URL.Query().Get("level")
		if tab == "" {
			tab = "app"
		}
		trace := r.URL.Query().Get("trace_id")

		out := []dashboard.LogEntry{}
		for _, l := range lines {
			if l.Level == tab && (trace == "" || l.TraceID == trace) {
				out = append(out, l)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
}

// startServicesFixture boots one aggregator for the file, configured for the scenario:
// a short service TTL so a silent service is down within seconds, and log stitching on,
// with the stub dashboards' loopback address allowed.
func startServicesFixture(t *testing.T) string {
	t.Helper()

	svcOnce.Do(func() {
		base := time.Now()
		gatewayLogs := stubDashboard([]dashboard.LogEntry{
			{Time: base, Level: "app", Message: "gateway: proxying order 123", TraceID: svcFailingID},
		})
		authLogs := stubDashboard([]dashboard.LogEntry{
			{Time: base.Add(time.Millisecond), Level: "app", Message: "auth-service authorized order 123", TraceID: svcFailingID},
			{Time: base.Add(2 * time.Millisecond), Level: "error", TraceID: svcFailingID,
				Message: "auth-service could not reach orders-service: dial tcp 127.0.0.1:3002: connect: connection refused"},
		})

		port := freeLivePort(t)
		router := breeze.NewRouter()
		app := breeze.New(router, breeze.NewEventLoopWorkerPool(runtime.NumCPU()))
		aggregator.InstallAggregator(app, router, aggregator.Config{
			BasePath:          "/fleet",
			IngestToken:       fleetIngestToken, // the token postSpans and postHeartbeat already send
			Username:          fleetUser,
			Password:          fleetPassword,
			ServiceTTL:        svcTTL,
			ServiceToken:      svcLogToken,
			FetchAllowedHosts: []string{"127.0.0.1"},
		})
		go func() { _ = app.Run(port, false) }()
		waitForLivePort(t, port, 15*time.Second)
		svcBase = "http://127.0.0.1:" + strconv.Itoa(port)

		beat := func(service string, errorRate float64, openAPI string) error {
			return postHeartbeat(svcBase, fleet.Heartbeat{
				Service: service, InstanceID: service + "-1", Version: "test",
				RPS: 5, ErrorRate: errorRate, OpenAPIURL: openAPI,
			})
		}
		alive := func() error {
			for _, s := range []struct {
				name    string
				errRate float64
				openAPI string
			}{
				{"gateway", 0.01, gatewayLogs.URL + "/openapi.json"},
				{"auth-service", 0.4, authLogs.URL + "/openapi.json"}, // failing, and alive
				// billing has no address, so its logs cannot be read: the case that makes
				// a source unavailable for a reason other than being down.
				{"billing", 0.9, ""},
			} {
				if err := beat(s.name, s.errRate, s.openAPI); err != nil {
					return err
				}
			}
			return nil
		}

		// orders-service heartbeats once and never again.
		if err := beat("orders-service", 0.0, ""); err != nil {
			svcStartUp = err
			return
		}
		if err := alive(); err != nil {
			svcStartUp = err
			return
		}

		// The others keep heartbeating for the life of the process.
		go func() {
			for range time.Tick(svcTTL / 6) {
				_ = alive()
			}
		}()

		// Healthy journeys first, so the topology has the edges the failing one lacks:
		// gateway -> auth-service -> orders-service.
		var spans []fleet.Span
		for i := 0; i < 3; i++ {
			spans = append(spans, journey(traceIDWithSuffix(svcFailingID, i+1), false)...)
		}
		// The failing journey: orders-service never answers, so it has no span.
		spans = append(spans, journey(svcFailingID, true)...)
		spans = append(spans, billingFailure(svcBillingFailID)...)
		if err := postSpans(svcBase, spans); err != nil {
			svcStartUp = err
			return
		}

		// Let orders-service go quiet for longer than the TTL.
		time.Sleep(svcTTL + 700*time.Millisecond)
	})

	if svcStartUp != nil {
		t.Fatalf("starting the services fixture: %v", svcStartUp)
	}
	return svcBase
}

// journey is gateway -> auth-service -> orders-service, or, when failing, the same
// journey cut short where the dead service would have answered.
func journey(traceID string, failing bool) []fleet.Span {
	start := time.Now().UnixNano()

	// A span id is 16 hex characters: two for the span's place in the journey, six from
	// the trace so journeys do not collide, and eight of padding.
	suffix := traceID[len(traceID)-6:]

	gateway := fleet.Span{
		TraceID: traceID, SpanID: "ba" + suffix + "00000000", Service: "gateway",
		Route: "/api/orders/:id", Method: "GET", Status: 200, StartNanoUTC: start, DurationMs: 40,
	}
	auth := fleet.Span{
		TraceID: traceID, SpanID: "ca" + suffix + "00000000", ParentSpanID: gateway.SpanID,
		Service: "auth-service", Route: "/internal/auth/:id", Method: "GET", Status: 200,
		StartNanoUTC: start + int64(time.Millisecond), DurationMs: 30,
	}
	if failing {
		gateway.Status, gateway.Error = 502, "HTTP 502"
		auth.Status, auth.Error = 502, "HTTP 502"

		// billing took part and succeeded. It matters to the log tests: it is in the
		// trace, and it has no address, so its logs are the ones that cannot be read.
		billing := fleet.Span{
			TraceID: traceID, SpanID: "ea" + suffix + "00000000", ParentSpanID: gateway.SpanID,
			Service: "billing", Route: "/internal/billing/:id", Method: "POST", Status: 200,
			StartNanoUTC: start + 3*int64(time.Millisecond), DurationMs: 10,
		}
		return []fleet.Span{gateway, auth, billing}
	}

	orders := fleet.Span{
		TraceID: traceID, SpanID: "da" + suffix + "00000000", ParentSpanID: auth.SpanID,
		Service: "orders-service", Route: "/internal/orders/:id", Method: "POST", Status: 200,
		StartNanoUTC: start + 2*int64(time.Millisecond), DurationMs: 20,
	}
	return []fleet.Span{gateway, auth, orders}
}

// billingFailure is gateway -> billing, where billing fails on its own account.
func billingFailure(traceID string) []fleet.Span {
	start := time.Now().UnixNano()
	suffix := traceID[len(traceID)-6:]

	gateway := fleet.Span{
		TraceID: traceID, SpanID: "bb" + suffix + "00000000", Service: "gateway",
		Route: "/api/invoices/:id", Method: "GET", Status: 502, Error: "HTTP 502",
		StartNanoUTC: start, DurationMs: 25,
	}
	billing := fleet.Span{
		TraceID: traceID, SpanID: "cb" + suffix + "00000000", ParentSpanID: gateway.SpanID,
		Service: "billing", Route: "/internal/billing/:id", Method: "GET", Status: 500, Error: "HTTP 500",
		StartNanoUTC: start + int64(time.Millisecond), DurationMs: 20,
	}
	return []fleet.Span{gateway, billing}
}

func asRows(t *testing.T, v any) []map[string]any {
	t.Helper()
	list, _ := v.([]any)
	rows := make([]map[string]any, len(list))
	for i, item := range list {
		rows[i], _ = item.(map[string]any)
	}
	return rows
}

func joinNotes(got map[string]any) string {
	var all []string
	notes, _ := got["notes"].([]any)
	for _, n := range notes {
		all = append(all, fmt.Sprint(n))
	}
	return strings.Join(all, "\n")
}

// ── breeze_get_services ──────────────────────────────────────────────────────

// TestGetServicesShowsADeadServiceFirst is the fact nothing exposed: orders-service is
// gone, and the registry knows it.
func TestGetServicesShowsADeadServiceFirst(t *testing.T) {
	base := startServicesFixture(t)

	got, summary, isErr := callLiveTool(t, "breeze_get_services", fleetCreds(base))
	if isErr {
		t.Fatalf("breeze_get_services failed: %s", summary)
	}
	if !strings.Contains(summary, "down: orders-service") {
		t.Errorf("the summary does not name the dead service: %q", summary)
	}
	if got["down"] != float64(1) {
		t.Errorf("down = %v, want 1", got["down"])
	}

	rows := asRows(t, got["services"])
	if len(rows) != 4 {
		t.Fatalf("got %d services, want gateway, auth-service, billing and orders-service: %v", len(rows), rows)
	}

	// Worst first: what is wrong goes on top.
	var order []string
	for _, r := range rows {
		order = append(order, fmt.Sprintf("%s:%s", r["name"], r["status"]))
	}
	want := []string{"orders-service:down", "auth-service:degraded", "billing:degraded", "gateway:up"}
	if strings.Join(order, " ") != strings.Join(want, " ") {
		t.Errorf("order = %v, want %v", order, want)
	}

	if silent, _ := rows[0]["silent_seconds"].(float64); silent < 3 {
		t.Errorf("orders-service silent_seconds = %v, want at least the TTL", rows[0]["silent_seconds"])
	}

	notes := joinNotes(got)
	for _, want := range []string{"down is inferred from silence", "exports no spans", "degraded means"} {
		if !strings.Contains(notes, want) {
			t.Errorf("the notes do not say %q:\n%s", want, notes)
		}
	}
}

func TestGetServicesExplainsAnEmptyRegistry(t *testing.T) {
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fleet/api/services" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	t.Cleanup(empty.Close)

	got, summary, isErr := callLiveTool(t, "breeze_get_services", fleetCreds(empty.URL))
	if isErr {
		t.Fatalf("breeze_get_services failed: %s", summary)
	}
	notes := joinNotes(got)
	for _, want := range []string{"registry is empty", "never appear here", "breeze_get_topology"} {
		if !strings.Contains(notes, want) {
			t.Errorf("an empty registry is not explained (%q missing):\n%s", want, notes)
		}
	}
}

// ── breeze_explain_incident ──────────────────────────────────────────────────

// TestExplainIncidentNamesADeadDependency: the trace stops at auth-service, so the
// aggregator's own account of it blames auth-service. The topology says auth-service calls
// orders-service and the registry says orders-service is down, and neither is new: the
// explanation just had no way to say them together.
func TestExplainIncidentNamesADeadDependency(t *testing.T) {
	base := startServicesFixture(t)

	got, summary, isErr := callLiveTool(t, "breeze_explain_incident",
		withFleetCreds(base, map[string]any{"trace_id": svcFailingID}))
	if isErr {
		t.Fatalf("breeze_explain_incident failed: %s", summary)
	}

	suspects := asRows(t, got["suspects"])
	if len(suspects) != 1 {
		t.Fatalf("suspects = %v, want orders-service alone", got["suspects"])
	}
	s := suspects[0]
	if s["service"] != "orders-service" || s["status"] != "down" || s["called_by"] != "auth-service" {
		t.Errorf("suspect = %v, want orders-service, down, called by auth-service", s)
	}
	if got["other_down_services"] != nil {
		t.Errorf("a down service the topology does tie to the failure was also listed as a mere lead: %v", got["other_down_services"])
	}

	explanation, _ := got["explanation"].(string)
	for _, want := range []string{
		"auth-service calls orders-service",
		"reports orders-service down",
		"exports no spans",
		"before auth-service's own code",
	} {
		if !strings.Contains(explanation, want) {
			t.Errorf("the explanation does not say %q:\n%s", want, explanation)
		}
	}

	steps, _ := got["next_steps"].([]any)
	if len(steps) != 2 {
		t.Fatalf("next_steps = %v, want the trace logs and the dead service", steps)
	}
	if first := fmt.Sprint(steps[0]); !strings.Contains(first, "breeze_get_trace_logs") || !strings.Contains(first, svcFailingID) {
		t.Errorf("the first step does not name the call that reads the trace's logs: %s", first)
	}
	if second := fmt.Sprint(steps[1]); !strings.Contains(second, "Check orders-service directly") ||
		!strings.Contains(second, "breeze_get_services") {
		t.Errorf("the second step does not send the reader to the dead service: %s", second)
	}
}

// TestExplainIncidentOffersADownServiceAsALeadWhenTheTopologyDoesNotShowTheCall is the
// shape the first live run took: orders-service died before it exported a single span, so
// no edge records that anything calls it, and the registry alone knows it is gone. That is
// worth saying, and worth saying as what it is: a lead, not a finding. A fleet usually has
// something unrelated down, and blaming it on the strength of timing would be a guess.
func TestExplainIncidentOffersADownServiceAsALeadWhenTheTopologyDoesNotShowTheCall(t *testing.T) {
	base := startServicesFixture(t)

	got, summary, isErr := callLiveTool(t, "breeze_explain_incident",
		withFleetCreds(base, map[string]any{"trace_id": svcBillingFailID}))
	if isErr {
		t.Fatalf("breeze_explain_incident failed: %s", summary)
	}

	if got["suspects"] != nil {
		t.Errorf("a suspect was named without a topology edge to it: %v", got["suspects"])
	}
	leads := asRows(t, got["other_down_services"])
	if len(leads) != 1 || leads[0]["service"] != "orders-service" || leads[0]["status"] != "down" {
		t.Fatalf("other_down_services = %v, want orders-service alone (auth-service is alive, only failing)", got["other_down_services"])
	}

	explanation, _ := got["explanation"].(string)
	for _, want := range []string{
		"also reports orders-service down",
		"a lead and not a finding",
		"billing's own log",
	} {
		if !strings.Contains(explanation, want) {
			t.Errorf("the explanation does not say %q:\n%s", want, explanation)
		}
	}
	if strings.Contains(explanation, "consistent with orders-service being the real cause") {
		t.Errorf("a lead was worded as a finding:\n%s", explanation)
	}

	steps, _ := got["next_steps"].([]any)
	if len(steps) != 2 || !strings.Contains(fmt.Sprint(steps[1]), "Check orders-service: the registry reports it down, "+
		"though the topology does not show billing calling it") {
		t.Errorf("next_steps = %v, want the log step and the lead", steps)
	}
}

// TestExplainIncidentOffersTheLogStepWithoutInventingASuspect: a failure whose callees
// are all fine must not grow a suspect, and the one step that always applies is still
// given. A trace of the shared fixture fails at a service that calls nothing.
func TestExplainIncidentOffersTheLogStepWithoutInventingASuspect(t *testing.T) {
	fx := startFleetFixture(t)

	got, summary, isErr := callLiveTool(t, "breeze_explain_incident",
		withFleetCreds(fx.base, map[string]any{"trace_id": failingTraceID}))
	if isErr {
		t.Fatalf("breeze_explain_incident failed: %s", summary)
	}
	if got["suspects"] != nil {
		t.Errorf("a failing service with no unhealthy callee grew suspects: %v", got["suspects"])
	}
	steps, _ := got["next_steps"].([]any)
	if len(steps) != 1 || !strings.Contains(fmt.Sprint(steps[0]), "breeze_get_trace_logs") {
		t.Errorf("next_steps = %v, want only the trace-logs step", steps)
	}
}

// ── breeze_get_trace_logs ────────────────────────────────────────────────────

// TestGetTraceLogsMergesEveryServiceAndEveryLevel: the failure is under error and the work
// that led to it under app, in two different services, and one call returns the story in
// the order it happened.
func TestGetTraceLogsMergesEveryServiceAndEveryLevel(t *testing.T) {
	base := startServicesFixture(t)

	got, summary, isErr := callLiveTool(t, "breeze_get_trace_logs",
		withFleetCreds(base, map[string]any{"trace_id": svcFailingID}))
	if isErr {
		t.Fatalf("breeze_get_trace_logs failed: %s", summary)
	}

	if !strings.Contains(summary, "3 log line(s) from 2 of 3 service(s)") {
		t.Errorf("summary = %q, want 3 lines from the 2 readable of 3 services", summary)
	}

	var story []string
	for _, l := range asRows(t, got["logs"]) {
		story = append(story, fmt.Sprintf("%s/%s", l["service"], l["level"]))
	}
	if want := "gateway/app auth-service/app auth-service/error"; strings.Join(story, " ") != want {
		t.Errorf("story = %v, want %s: the work, then the failure, across services", story, want)
	}
	last := asRows(t, got["logs"])[2]
	if msg, _ := last["message"].(string); !strings.Contains(msg, "connection refused") {
		t.Errorf("the failure line does not carry the cause: %v", last)
	}

	// billing is in the trace and has no address: its logs are missing, which is not the
	// same as empty, and the note has to say so.
	notes := joinNotes(got)
	for _, want := range []string{"billing", "missing, not empty", "breeze_get_services"} {
		if !strings.Contains(notes, want) {
			t.Errorf("the notes do not say %q:\n%s", want, notes)
		}
	}
}

// TestGetTraceLogsSaysWhenLogStitchingIsOff: the shared fixture's aggregator has no
// service token, so it reads nobody's logs. That has to come back as a finding with the
// way around it, not as an empty list that reads as a trace that logged nothing.
func TestGetTraceLogsSaysWhenLogStitchingIsOff(t *testing.T) {
	fx := startFleetFixture(t)

	got, summary, isErr := callLiveTool(t, "breeze_get_trace_logs",
		withFleetCreds(fx.base, map[string]any{"trace_id": failingTraceID}))
	if isErr {
		t.Fatalf("breeze_get_trace_logs failed: %s", summary)
	}
	if !strings.Contains(summary, "disabled") {
		t.Errorf("summary = %q, want it to say log stitching is disabled", summary)
	}
	if reason, _ := got["disabled"].(string); reason == "" {
		t.Errorf("no reason is given for the aggregator being unable to read logs: %v", got)
	}
	if notes := joinNotes(got); !strings.Contains(notes, "breeze_get_logs") {
		t.Errorf("the way around it is not given:\n%s", notes)
	}
}

func TestGetTraceLogsRejectsAMalformedTraceID(t *testing.T) {
	fx := startFleetFixture(t)

	_, summary, isErr := callLiveTool(t, "breeze_get_trace_logs",
		withFleetCreds(fx.base, map[string]any{"trace_id": "../../etc/passwd"}))
	if !isErr {
		t.Fatalf("a trace id that is a path was accepted: %s", summary)
	}
}

// askedQuery holds the raw query the stub aggregator saw last. The handler runs on the HTTP server's
// goroutines and the test reads it from its own, and a loopback round trip gives the race detector no
// ordering between the two, so both sides go through the mutex.
type askedQuery struct {
	mu sync.Mutex
	q  string
}

func (a *askedQuery) set(q string) {
	a.mu.Lock()
	a.q = q
	a.mu.Unlock()
}

func (a *askedQuery) get() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.q
}

// stubAggregator serves a fixed list at /fleet/api/services and records what it was asked.
func stubAggregator(t *testing.T, list string, asked *askedQuery) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fleet/api/services" {
			http.NotFound(w, r)
			return
		}
		if asked != nil {
			asked.set(r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(list))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestGetServicesListsForgottenServicesAfterTheLiveOnes: a service the registry swept is absent from
// the live list, which reads the same as a service that never existed. The tool asks for the tombstones
// and puts them after everything alive, counted apart.
func TestGetServicesListsForgottenServicesAfterTheLiveOnes(t *testing.T) {
	now := time.Now()
	var asked askedQuery
	base := stubAggregator(t, fmt.Sprintf(`[
		{"name":"orders","instances":0,"rps":0,"error_rate":0,"status":"forgotten","last_heartbeat_unix":%d,"forgotten_at_unix":%d,"dashboard_url":"http://orders:3000/dashboard"},
		{"name":"gateway","instances":1,"rps":5,"error_rate":0,"status":"up","last_heartbeat_unix":%d,"dashboard_url":"http://gateway:3000/dashboard"}
	]`, now.Add(-30*time.Minute).Unix(), now.Add(-25*time.Minute).Unix(), now.Unix()), &asked)

	got, summary, isErr := callLiveTool(t, "breeze_get_services", fleetCreds(base))
	if isErr {
		t.Fatalf("breeze_get_services: %s", summary)
	}
	if q := asked.get(); !strings.Contains(q, "forgotten=1") {
		t.Errorf("the tool asked the aggregator %q, so it never gets the tombstones", q)
	}
	rows := asRows(t, got["services"])
	if len(rows) != 2 || rows[0]["name"] != "gateway" || rows[1]["name"] != "orders" || rows[1]["status"] != "forgotten" {
		t.Fatalf("services = %v, want the live gateway and then the forgotten orders", rows)
	}
	if got["total"] != float64(1) || got["forgotten"] != float64(1) {
		t.Errorf("total/forgotten = %v/%v, want 1/1: a tombstone is not a service the registry holds", got["total"], got["forgotten"])
	}
	if silent, _ := rows[1]["silent_seconds"].(float64); silent < 29*60 {
		t.Errorf("the forgotten service was last heard from 30 minutes ago and silent_seconds says %v", rows[1]["silent_seconds"])
	}
	if !strings.Contains(summary, "1 forgotten") || !strings.Contains(joinNotes(got), "forgotten service(s) are listed after the live ones") {
		t.Errorf("the answer does not say a service was forgotten: %q / %s", summary, joinNotes(got))
	}
}

// TestGetServicesNamesTheServicesItCannotReadLogsFrom: a service that advertises no address has its
// lines missing from every stitched trace, and nothing said so.
func TestGetServicesNamesTheServicesItCannotReadLogsFrom(t *testing.T) {
	now := time.Now().Unix()
	base := stubAggregator(t, fmt.Sprintf(`[
		{"name":"has-dashboard","instances":1,"rps":1,"error_rate":0,"status":"up","last_heartbeat_unix":%[1]d,"dashboard_url":"http://a:3000/dashboard"},
		{"name":"has-openapi","instances":1,"rps":1,"error_rate":0,"status":"up","last_heartbeat_unix":%[1]d,"openapi_url":"http://b:3000/openapi.json"},
		{"name":"has-neither","instances":1,"rps":1,"error_rate":0,"status":"up","last_heartbeat_unix":%[1]d},
		{"name":"gone","instances":0,"rps":0,"error_rate":0,"status":"forgotten","last_heartbeat_unix":%[1]d}
	]`, now), nil)

	got, summary, isErr := callLiveTool(t, "breeze_get_services", fleetCreds(base))
	if isErr {
		t.Fatalf("breeze_get_services: %s", summary)
	}
	notes := joinNotes(got)
	if !strings.Contains(notes, "cannot read logs from has-neither:") || !strings.Contains(notes, "FLEET_DASHBOARD_URL") {
		t.Errorf("the notes do not name the service that cannot be log-stitched, or how to fix it:\n%s", notes)
	}
	for _, not := range []string{"has-dashboard", "has-openapi", "gone"} {
		if strings.Contains(notes, "cannot read logs from") && strings.Contains(strings.SplitN(notes, "cannot read logs from", 2)[1], not+",") {
			t.Errorf("%s is named as unreadable, and it is not: %s", not, notes)
		}
	}
}

// TestGetServicesSeesATombstoneFromARealAggregator is the same over the real wire: an aggregator whose
// registry swept a service, asked through HTTP by the tool.
func TestGetServicesSeesATombstoneFromARealAggregator(t *testing.T) {
	port := freeLivePort(t)
	router := breeze.NewRouter()
	app := breeze.New(router, breeze.NewEventLoopWorkerPool(1))
	agg := aggregator.InstallAggregator(app, router, aggregator.Config{
		BasePath: "/fleet", IngestToken: fleetIngestToken, Username: fleetUser, Password: fleetPassword,
	})
	t.Cleanup(func() { _ = agg.Close(context.Background()) })
	go func() { _ = app.Run(port, false) }()
	waitForLivePort(t, port, 15*time.Second)

	reg := agg.Registry()
	reg.Observe(fleet.Heartbeat{Service: "retired-orders", InstanceID: "o1", DashboardURL: "http://orders:3000/dashboard"}, time.Now().Add(-time.Hour))
	reg.Observe(fleet.Heartbeat{Service: "gateway", InstanceID: "g1"}, time.Now())
	reg.Sweep(time.Now())

	got, summary, isErr := callLiveTool(t, "breeze_get_services", fleetCreds("http://127.0.0.1:"+strconv.Itoa(port)))
	if isErr {
		t.Fatalf("breeze_get_services: %s", summary)
	}
	rows := asRows(t, got["services"])
	if len(rows) != 2 || rows[1]["name"] != "retired-orders" || rows[1]["status"] != "forgotten" || rows[1]["dashboard_url"] != "http://orders:3000/dashboard" {
		t.Fatalf("services = %v, want gateway and then the tombstone with what it advertised", rows)
	}
}
