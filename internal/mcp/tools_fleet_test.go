package mcp_test

// Category D against a real aggregator.
//
// The aggregator is installed with aggregator.InstallAggregator and run on a real
// port, and the spans are ingested over real HTTP with the framework's own client.
// Nothing is stubbed. The point of the exercise is that the mirror structs in
// tools_fleet.go decode what the aggregator actually serialises, and a stub built
// from the same reading of the source as the mirrors would agree with them while
// both were wrong.
//
// The span batches below are shaped to cross the aggregator's genuine incident
// thresholds â€” at least ten windowed calls and a windowed error rate over ten
// percent â€” so the blast radius reported by breeze_explain_incident is one the
// aggregator really computed, not a value this test arranged to be trivially
// non-empty.
//
// This file is package mcp_test rather than package mcp because it imports both
// fleet/aggregator and the mcp package. Those cannot be imported together from
// inside package mcp: the aggregator imports the root breeze package, and Part 4
// has the root breeze package importing mcp.

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nelthaarion/breeze"
	"github.com/nelthaarion/breeze/client"
	"github.com/nelthaarion/breeze/fleet"
	"github.com/nelthaarion/breeze/fleet/aggregator"
	"github.com/nelthaarion/breeze/fleet/contracts"
)

const (
	fleetIngestToken = "fleet-test-ingest"
	fleetUser        = "fleetviewer"
	fleetPassword    = "fleetsecret"
)

// The three trace ids the fixture ingests. Fixed rather than random so a failure
// names the same trace every run.
const (
	failingTraceID = "0af7651916cd43dd8448eb211c80319c"
	healthyTraceID = "1bf7651916cd43dd8448eb211c803100"
	soloTraceID    = "2cf7651916cd43dd8448eb211c803200"
)

type fleetFixture struct {
	base string
	agg  *aggregator.Aggregator
}

var (
	fleetOnce    sync.Once
	fleetShared  fleetFixture
	fleetStartUp error
)

// startFleetFixture boots one aggregator for the whole file.
//
// Breeze exposes Run but no Stop, so a booted app owns its port for the rest of
// the process; starting one per test would leak a listener each time. The same
// reasoning, and the same shape, as the aggregator package's own wsingest_test.
//
// Read auth is enabled. A fixture with auth off could not distinguish a tool that
// sends credentials correctly from one that sends none at all.
func startFleetFixture(t *testing.T) fleetFixture {
	t.Helper()

	fleetOnce.Do(func() {
		port := freeLivePort(t)

		router := breeze.NewRouter()
		app := breeze.New(router, breeze.NewEventLoopWorkerPool(runtime.NumCPU()))

		agg := aggregator.InstallAggregator(app, router, aggregator.Config{
			BasePath:    "/fleet",
			IngestToken: fleetIngestToken,
			Username:    fleetUser,
			Password:    fleetPassword,
		})

		go func() {
			// Blocks for the process lifetime. A bind failure shows up as the
			// wait below timing out rather than as a silent hang.
			_ = app.Run(port, false)
		}()

		waitForLivePort(t, port, 15*time.Second)

		fleetShared = fleetFixture{
			base: "http://127.0.0.1:" + strconv.Itoa(port),
			agg:  agg,
		}

		if err := ingestFixtureSpans(fleetShared.base); err != nil {
			fleetStartUp = err
			return
		}

		// Ingestion is synchronous through AcceptSpans, but topology
		// reconciliation and the windowed counters the incident calculation
		// reads are updated as the batch is accounted. Give the aggregator a
		// moment before the assertions run.
		time.Sleep(150 * time.Millisecond)
	})

	if fleetStartUp != nil {
		t.Fatalf("fixture ingestion failed: %v", fleetStartUp)
	}
	if fleetShared.agg == nil {
		t.Fatal("aggregator failed to start")
	}
	return fleetShared
}

// fleetCreds is the credential set the fixture requires.
func fleetCreds(base string) map[string]any {
	return map[string]any{
		"aggregator_url": base,
		"username":       fleetUser,
		"password":       fleetPassword,
	}
}

// withFleetCreds copies the credentials and adds tool-specific arguments.
func withFleetCreds(base string, extra map[string]any) map[string]any {
	args := fleetCreds(base)
	for k, v := range extra {
		args[k] = v
	}
	return args
}

// ingestFixtureSpans posts three real traces over real HTTP.
//
// The shape is deliberate:
//
//   - gateway -> orders -> inventory, where inventory returns 500. This gives
//     assemble.go a genuine root cause to find: the deepest failing span, with
//     the two spans above it marked as derived errors rather than separate
//     faults. Nothing in this file computes that; the aggregator does.
//   - the same three-service path succeeding, so a healthy trace exists to
//     contrast against and so the error rate is not a flat 100%.
//   - a single-service trace, which exists purely to prove the min_services
//     default does not silently hide it.
//
// The failing path is repeated enough times to clear minWindowCalls, because
// below that floor the aggregator refuses to raise an incident at all â€” by
// design, since one failure against an idle service reads as a 100% error rate.
func ingestFixtureSpans(base string) error {
	var batch []fleet.Span

	// One failing journey, spelled out. Later repeats reuse this shape with
	// different ids.
	batch = append(batch, failingTrace(failingTraceID, 0)...)

	// Enough repeats of the same path that the windowed counters clear
	// minWindowCalls (10) for inventory and its callers.
	for i := 1; i <= 11; i++ {
		batch = append(batch, failingTrace(traceIDWithSuffix(failingTraceID, i), i)...)
	}

	// Two healthy journeys over the same path.
	batch = append(batch, healthyTrace(healthyTraceID)...)
	batch = append(batch, healthyTrace(traceIDWithSuffix(healthyTraceID, 1))...)

	// A single-service trace.
	batch = append(batch, fleet.Span{
		TraceID:      soloTraceID,
		SpanID:       "aaaa000000000001",
		Service:      "standalone",
		Route:        "/health",
		Method:       "GET",
		Status:       200,
		StartNanoUTC: time.Now().UnixNano(),
		DurationMs:   1.5,
	})

	if err := postSpans(base, batch); err != nil {
		return err
	}

	// A heartbeat, so the service registry has something too.
	return postHeartbeat(base, fleet.Heartbeat{
		Service:    "gateway",
		InstanceID: "gateway-1",
		Version:    "test",
		RPS:        12,
		ErrorRate:  0.25,
	})
}

// failingTrace builds gateway -> orders -> inventory where inventory fails.
func failingTrace(traceID string, seq int) []fleet.Span {
	start := time.Now().UnixNano()
	gatewaySpan := fmt.Sprintf("b0%014d", seq)
	ordersSpan := fmt.Sprintf("c0%014d", seq)
	inventorySpan := fmt.Sprintf("d0%014d", seq)

	return []fleet.Span{
		{
			TraceID:      traceID,
			SpanID:       gatewaySpan,
			Service:      "gateway",
			Route:        "/checkout",
			Method:       "POST",
			Status:       500,
			StartNanoUTC: start,
			DurationMs:   42,
			Error:        "downstream failure",
			Tags:         map[string]string{"tier": "edge"},
		},
		{
			TraceID:      traceID,
			SpanID:       ordersSpan,
			ParentSpanID: gatewaySpan,
			Service:      "orders",
			Route:        "/orders",
			Method:       "POST",
			Status:       500,
			StartNanoUTC: start + int64(2*time.Millisecond),
			DurationMs:   30,
			Error:        "inventory rejected the reservation",
		},
		{
			TraceID:      traceID,
			SpanID:       inventorySpan,
			ParentSpanID: ordersSpan,
			Service:      "inventory",
			Route:        "/reserve",
			Method:       "POST",
			Status:       500,
			StartNanoUTC: start + int64(5*time.Millisecond),
			DurationMs:   18,
			Error:        "stock ledger write failed",
		},
	}
}

// healthyTrace builds the same path succeeding.
func healthyTrace(traceID string) []fleet.Span {
	start := time.Now().UnixNano()
	return []fleet.Span{
		{
			TraceID:      traceID,
			SpanID:       "b1" + traceID[:14],
			Service:      "gateway",
			Route:        "/checkout",
			Method:       "POST",
			Status:       200,
			StartNanoUTC: start,
			DurationMs:   20,
		},
		{
			TraceID:      traceID,
			SpanID:       "c1" + traceID[:14],
			ParentSpanID: "b1" + traceID[:14],
			Service:      "orders",
			Route:        "/orders",
			Method:       "POST",
			Status:       200,
			StartNanoUTC: start + int64(time.Millisecond),
			DurationMs:   12,
		},
		{
			TraceID:      traceID,
			SpanID:       "d1" + traceID[:14],
			ParentSpanID: "c1" + traceID[:14],
			Service:      "inventory",
			Route:        "/reserve",
			Method:       "POST",
			Status:       200,
			StartNanoUTC: start + int64(2*time.Millisecond),
			DurationMs:   6,
		},
	}
}

// traceIDWithSuffix derives a distinct valid trace id from a base one.
func traceIDWithSuffix(base string, n int) string {
	suffix := fmt.Sprintf("%04x", n)
	return base[:len(base)-len(suffix)] + suffix
}

// postSpans ingests a batch the way a service does: POST with X-Fleet-Token.
func postSpans(base string, spans []fleet.Span) error {
	body, err := json.Marshal(spans)
	if err != nil {
		return err
	}

	c := client.New(client.Config{Timeout: 10 * time.Second})
	defer c.Close()

	req := client.NewRequest("POST", base+"/fleet/api/spans", body)
	req.SetHeader("Content-Type", "application/json")
	req.SetHeader("X-Fleet-Token", fleetIngestToken)

	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("post spans: %w", err)
	}
	if resp.Status != 202 {
		return fmt.Errorf("post spans returned %d: %s", resp.Status, resp.Body)
	}
	return nil
}

func postHeartbeat(base string, hb fleet.Heartbeat) error {
	body, err := json.Marshal(hb)
	if err != nil {
		return err
	}

	c := client.New(client.Config{Timeout: 10 * time.Second})
	defer c.Close()

	req := client.NewRequest("POST", base+"/fleet/api/heartbeat", body)
	req.SetHeader("Content-Type", "application/json")
	req.SetHeader("X-Fleet-Token", fleetIngestToken)

	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("post heartbeat: %w", err)
	}
	if resp.Status != 202 {
		return fmt.Errorf("post heartbeat returned %d: %s", resp.Status, resp.Body)
	}
	return nil
}

// â”€â”€â”€ tests â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

func TestGetTopologyReadsTheRealServiceGraph(t *testing.T) {
	f := startFleetFixture(t)

	got, summary, isErr := callLiveTool(t, "breeze_get_topology", fleetCreds(f.base))
	if isErr {
		t.Fatalf("breeze_get_topology failed: %s", summary)
	}

	if got["service_count"].(float64) < 4 {
		t.Errorf("service_count = %v, want at least the four services ingested: %s",
			got["service_count"], summary)
	}

	// The edges are the part a single service's dashboard could not report, and
	// they exist only because the spans carry parent ids.
	edges := map[string]bool{}
	for _, raw := range got["edges"].([]any) {
		e := raw.(map[string]any)
		edges[e["caller"].(string)+"->"+e["callee"].(string)] = true
	}
	for _, want := range []string{"gateway->orders", "orders->inventory"} {
		if !edges[want] {
			t.Errorf("edge %s was not observed; edges seen: %v", want, edges)
		}
	}

	// standalone reported one span with no parent and nothing calling it, so it
	// must be named as isolated. That distinction is the tool's, and it is worth
	// pinning: an isolated service usually means uninstrumented callers rather
	// than a genuinely standalone one.
	isolated := map[string]bool{}
	if raw, ok := got["isolated"].([]any); ok {
		for _, v := range raw {
			isolated[v.(string)] = true
		}
	}
	if !isolated["standalone"] {
		t.Errorf("standalone has no edges but was not reported as isolated: %v", got["isolated"])
	}
	if isolated["gateway"] {
		t.Error("gateway has edges but was reported as isolated")
	}

	if len(got["unhealthy"].([]any)) == 0 {
		t.Errorf("services returned 500 but none were reported unhealthy: %s", summary)
	}
}

func TestGetTracesListsRealTracesAndDoesNotHideSingleServiceOnes(t *testing.T) {
	f := startFleetFixture(t)

	got, summary, isErr := callLiveTool(t, "breeze_get_traces", fleetCreds(f.base))
	if isErr {
		t.Fatalf("breeze_get_traces failed: %s", summary)
	}

	ids := map[string]map[string]any{}
	for _, raw := range got["traces"].([]any) {
		tr := raw.(map[string]any)
		ids[tr["trace_id"].(string)] = tr
	}

	if _, ok := ids[failingTraceID]; !ok {
		t.Errorf("the failing trace is missing from the listing: %s", summary)
	}

	// The regression this guards: the endpoint's own min_services default is 2,
	// so a caller that did not override it would never see a single-service
	// trace and would be told there are none. Being told "no traces" when there
	// are is the worst failure this tool has.
	if _, ok := ids[soloTraceID]; !ok {
		t.Errorf("the single-service trace was hidden; the endpoint's min_services default "+
			"of 2 is not being overridden. traces seen: %d", len(ids))
	}

	if got["failing"].(float64) < 1 {
		t.Errorf("failing = %v, want at least one: %s", got["failing"], summary)
	}
}

func TestGetTracesFiltersAreActuallyAppliedByTheServer(t *testing.T) {
	f := startFleetFixture(t)

	// min_services=2 must now exclude the solo trace. This is the other half of
	// the test above: the override must be a default, not a hardcoding that
	// ignores the caller.
	got, summary, isErr := callLiveTool(t, "breeze_get_traces",
		withFleetCreds(f.base, map[string]any{"min_services": 2}))
	if isErr {
		t.Fatalf("filtered breeze_get_traces failed: %s", summary)
	}
	for _, raw := range got["traces"].([]any) {
		if raw.(map[string]any)["trace_id"] == soloTraceID {
			t.Error("min_services=2 was requested but a single-service trace came back")
		}
	}

	// And a service filter must narrow to traces involving it.
	got, summary, isErr = callLiveTool(t, "breeze_get_traces",
		withFleetCreds(f.base, map[string]any{"service": "standalone"}))
	if isErr {
		t.Fatalf("service-filtered breeze_get_traces failed: %s", summary)
	}
	traces := got["traces"].([]any)
	if len(traces) == 0 {
		t.Fatalf("filtering by service standalone returned nothing: %s", summary)
	}
	for _, raw := range traces {
		tr := raw.(map[string]any)
		found := false
		for _, s := range tr["services"].([]any) {
			if s.(string) == "standalone" {
				found = true
			}
		}
		if !found {
			t.Errorf("trace %v does not involve standalone but was returned", tr["trace_id"])
		}
	}

	// A filter that matches nothing must say so, and must say that filters were
	// the reason â€” otherwise an agent reads it as "the aggregator is empty".
	got, _, isErr = callLiveTool(t, "breeze_get_traces",
		withFleetCreds(f.base, map[string]any{"service": "no-such-service"}))
	if isErr {
		t.Fatal("a filter matching nothing was reported as a transport failure")
	}
	if got["count"].(float64) != 0 {
		t.Fatalf("count = %v for an unknown service", got["count"])
	}
	notes := notesText(got)
	if !strings.Contains(notes, "Filters were applied") {
		t.Errorf("an empty filtered result did not explain that filters caused it: %q", notes)
	}
}

// TestGetTraceReadsTheAggregatorsOwnRootCause is the core of Category D's
// no-reimplementation requirement.
//
// The root cause is not computed here and must not be: assemble.go picks the
// deepest failing span and marks the ones above it as derived. This test asserts
// the tool reports that choice, and specifically that it reports inventory â€”
// which is deepest â€” rather than gateway, which fails first and is what a naive
// "first error wins" reimplementation would pick.
func TestGetTraceReadsTheAggregatorsOwnRootCause(t *testing.T) {
	f := startFleetFixture(t)

	got, summary, isErr := callLiveTool(t, "breeze_get_trace",
		withFleetCreds(f.base, map[string]any{"trace_id": failingTraceID}))
	if isErr {
		t.Fatalf("breeze_get_trace failed: %s", summary)
	}

	if got["span_count"].(float64) != 3 {
		t.Errorf("span_count = %v, want 3", got["span_count"])
	}
	if got["has_error"] != true {
		t.Errorf("has_error = %v for a trace of three 500s", got["has_error"])
	}

	cause, ok := got["root_cause"].(map[string]any)
	if !ok {
		t.Fatalf("no root_cause was reported for a failing trace: %s", summary)
	}
	if cause["service"] != "inventory" {
		t.Errorf("root cause service = %v, want inventory (the deepest failing span, which is "+
			"what the aggregator marks; gateway failing first is a consequence, not the cause)",
			cause["service"])
	}
	if cause["error"] != "stock ledger write failed" {
		t.Errorf("root cause error = %v", cause["error"])
	}
	if got["root_cause_span_id"] != cause["span_id"] {
		t.Errorf("root_cause_span_id %v does not match the flagged span %v",
			got["root_cause_span_id"], cause["span_id"])
	}

	// The tree must be flattened in traversal order with real depths, so the
	// shape of the call path is still readable.
	spans := got["spans"].([]any)
	if len(spans) != 3 {
		t.Fatalf("flattened %d spans, want 3", len(spans))
	}
	wantOrder := []struct {
		service string
		depth   float64
	}{
		{"gateway", 0}, {"orders", 1}, {"inventory", 2},
	}
	for i, want := range wantOrder {
		s := spans[i].(map[string]any)
		if s["service"] != want.service || s["depth"].(float64) != want.depth {
			t.Errorf("span %d = %v at depth %v, want %s at depth %v",
				i, s["service"], s["depth"], want.service, want.depth)
		}
	}

	// The aggregator writes its own summary sentence; the tool must pass it
	// through rather than substituting one.
	if got["summary"] == nil || got["summary"] == "" {
		t.Error("the aggregator's own trace summary was dropped")
	}
}

func TestGetTraceRejectsABadIDBeforeCallingTheServer(t *testing.T) {
	f := startFleetFixture(t)

	// A span id used where a trace id belongs is the likeliest mistake, and the
	// aggregator answers it with a bare 400 that explains nothing.
	_, summary, isErr := callLiveTool(t, "breeze_get_trace",
		withFleetCreds(f.base, map[string]any{"trace_id": "d000000000000001"}))
	if !isErr {
		t.Fatal("a 16-character span id was accepted as a trace id")
	}
	if !strings.Contains(summary, "span id") {
		t.Errorf("the error did not explain the span-id confusion: %q", summary)
	}

	_, summary, isErr = callLiveTool(t, "breeze_get_trace",
		withFleetCreds(f.base, map[string]any{"trace_id": strings.Repeat("z", 32)}))
	if !isErr {
		t.Fatal("a non-hex trace id was accepted")
	}
	if !strings.Contains(summary, "hex") {
		t.Errorf("the error did not mention hex: %q", summary)
	}
}

// TestGetTraceDistinguishesAnAgedOutTraceFromAMissingFeature is why liveRequest
// grew a notFound override.
//
// Both cases are a 404. Reporting an evicted trace as "Fleet is not installed"
// would send an agent to reinstall a feature that is working, so the two have to
// read differently.
func TestGetTraceDistinguishesAnAgedOutTraceFromAMissingFeature(t *testing.T) {
	f := startFleetFixture(t)

	absent := "ffffffffffffffffffffffffffffffff"
	_, summary, isErr := callLiveTool(t, "breeze_get_trace",
		withFleetCreds(f.base, map[string]any{"trace_id": absent}))
	if !isErr {
		t.Fatal("a trace that was never ingested was reported as found")
	}
	if strings.Contains(summary, "not installed") {
		t.Errorf("an unknown trace was reported as a missing feature: %q", summary)
	}
	if !strings.Contains(summary, "aged out") {
		t.Errorf("an unknown trace did not explain eviction as the likely cause: %q", summary)
	}

	// The contrast: a wrong base path is a genuinely missing feature, and must
	// still say so.
	_, summary, isErr = callLiveTool(t, "breeze_get_topology",
		withFleetCreds(f.base, map[string]any{"base_path": "/not-fleet"}))
	if !isErr {
		t.Fatal("a wrong base path returned a result")
	}
	if !strings.Contains(summary, "not installed") {
		t.Errorf("a wrong base path did not read as a missing feature: %q", summary)
	}
}

// TestExplainIncidentComposesTheAggregatorsOwnAnalysis is the acceptance test for
// the "compose, do not reimplement" requirement.
func TestExplainIncidentComposesTheAggregatorsOwnAnalysis(t *testing.T) {
	f := startFleetFixture(t)

	got, summary, isErr := callLiveTool(t, "breeze_explain_incident",
		withFleetCreds(f.base, map[string]any{"trace_id": failingTraceID}))
	if isErr {
		t.Fatalf("breeze_explain_incident failed: %s", summary)
	}

	explanation, _ := got["explanation"].(string)
	if explanation == "" {
		t.Fatal("no explanation was produced")
	}

	// It must name the aggregator's root cause, not the first failure.
	if !strings.Contains(explanation, "inventory") {
		t.Errorf("the explanation does not name the root-cause service:\n%s", explanation)
	}
	if !strings.Contains(explanation, "stock ledger write failed") {
		t.Errorf("the explanation does not quote the root-cause error:\n%s", explanation)
	}

	// The two spans above the cause must be described as consequences. That
	// classification is assemble.go's, and it is the difference between one
	// incident and three.
	if !strings.Contains(explanation, "consequences") {
		t.Errorf("derived errors were not distinguished from the cause:\n%s", explanation)
	}

	// The trace evidence must be carried, not just narrated.
	trace, ok := got["trace"].(map[string]any)
	if !ok {
		t.Fatal("the explanation carries no underlying trace evidence")
	}
	if trace["trace_id"] != failingTraceID {
		t.Errorf("evidence is for trace %v, want %s", trace["trace_id"], failingTraceID)
	}

	// The blast radius must be the aggregator's own computation. Enough failing
	// calls were ingested to clear its minimum-calls floor and error-rate
	// threshold, so an empty list here means the incidents endpoint was not
	// consulted at all.
	incidents, _ := got["incidents"].([]any)
	if len(incidents) == 0 {
		t.Errorf("no blast radius was reported despite %d failing calls being ingested; "+
			"the incidents endpoint is not being composed. explanation:\n%s", 12, explanation)
	} else {
		named := false
		for _, raw := range incidents {
			inc := raw.(map[string]any)
			if _, ok := inc["banner"]; !ok {
				t.Error("an incident came back without the aggregator's own banner sentence")
			}
			if inc["service"] == "inventory" || inc["service"] == "orders" || inc["service"] == "gateway" {
				named = true
			}
		}
		if !named {
			t.Errorf("incidents were returned but none concern the services in this trace: %v", incidents)
		}
		if !strings.Contains(explanation, "Fleet-wide") {
			t.Errorf("blast radius was fetched but not narrated:\n%s", explanation)
		}
	}

	// No contract violation was ingested, so the explanation must say the
	// payloads matched rather than staying silent â€” the absence of a violation is
	// itself a finding, because it rules out a shape mismatch.
	if !strings.Contains(explanation, "No contract violation") {
		t.Errorf("the explanation does not rule contract violations in or out:\n%s", explanation)
	}
}

// TestExplainIncidentOnAHealthyTraceSaysSoRatherThanInventingAFault guards the
// failure mode where a tool built to explain incidents finds one regardless.
func TestExplainIncidentOnAHealthyTraceSaysSoRatherThanInventingAFault(t *testing.T) {
	f := startFleetFixture(t)

	got, summary, isErr := callLiveTool(t, "breeze_explain_incident",
		withFleetCreds(f.base, map[string]any{"trace_id": healthyTraceID}))
	if isErr {
		t.Fatalf("breeze_explain_incident failed on a healthy trace: %s", summary)
	}

	explanation := got["explanation"].(string)
	if !strings.Contains(explanation, "did not fail") {
		t.Errorf("a successful trace was not described as successful:\n%s", explanation)
	}
	if strings.Contains(explanation, "originated in") {
		t.Errorf("a root cause was described for a trace that did not fail:\n%s", explanation)
	}

	trace := got["trace"].(map[string]any)
	if trace["has_error"] != false {
		t.Errorf("has_error = %v for the healthy trace", trace["has_error"])
	}
	if _, ok := trace["root_cause"]; ok {
		t.Error("a root cause span was reported for a healthy trace")
	}
}

func TestGetContractViolationsReportsAnEmptyRingHonestly(t *testing.T) {
	f := startFleetFixture(t)

	got, summary, isErr := callLiveTool(t, "breeze_get_contract_violations", fleetCreds(f.base))
	if isErr {
		t.Fatalf("breeze_get_contract_violations failed: %s", summary)
	}

	// Contract validation is off in this fixture, so the honest answer is zero
	// with the reason attached. A bare empty array would read as "the contracts
	// are all satisfied", which is a much stronger claim than the data supports.
	if got["count"].(float64) != 0 {
		t.Fatalf("count = %v, want 0 with validation disabled", got["count"])
	}
	if _, ok := got["violations"].([]any); !ok {
		t.Errorf("violations should be an empty array, not null: %v", got["violations"])
	}
	notes := notesText(got)
	if !strings.Contains(notes, "schemas") {
		t.Errorf("an empty violation list did not explain why it might be empty: %q", notes)
	}
}

// TestFleetToolsRejectMissingCredentialsAsUnauthorized pins the same distinction
// the dashboard tools make, against the aggregator's own Basic auth.
func TestFleetToolsRejectMissingCredentialsAsUnauthorized(t *testing.T) {
	f := startFleetFixture(t)

	for _, name := range []string{
		"breeze_get_topology", "breeze_get_traces", "breeze_get_contract_violations",
	} {
		got, summary, isErr := callLiveTool(t, name, map[string]any{"aggregator_url": f.base})
		if !isErr {
			t.Errorf("%s succeeded without credentials", name)
			continue
		}
		if got["error"] != "unauthorized" {
			t.Errorf("%s reported %v, want unauthorized: %s", name, got["error"], summary)
		}
		// The message must not send an agent off to install a feature that is
		// present and merely protected.
		if strings.Contains(summary, "not installed") {
			t.Errorf("%s described a 401 as a missing feature: %s", name, summary)
		}
	}
}

// TestFleetToolsSendBasicNotTheTokenForReads pins the credential mapping.
//
// The aggregator's read endpoints check HTTP Basic; X-Fleet-Token authorises
// ingestion only. Passing the token alone must therefore still be rejected, and
// this is worth a test because the two are easy to conflate and the failure would
// be a confusing 401 in the field.
func TestFleetToolsSendBasicNotTheTokenForReads(t *testing.T) {
	f := startFleetFixture(t)

	got, summary, isErr := callLiveTool(t, "breeze_get_topology", map[string]any{
		"aggregator_url": f.base,
		"token":          fleetIngestToken,
	})
	if !isErr {
		t.Fatal("the ingest token alone authorised a read; the aggregator requires Basic auth")
	}
	if got["error"] != "unauthorized" {
		t.Errorf("error = %v, want unauthorized: %s", got["error"], summary)
	}

	// And the message must point at the right credential.
	if !strings.Contains(summary, "username and password") {
		t.Errorf("the 401 message does not name the credential that would work: %q", summary)
	}
}

func TestFleetToolsReportAClosedPortAsUnreachable(t *testing.T) {
	port := freeLivePort(t)
	base := "http://127.0.0.1:" + strconv.Itoa(port)

	got, summary, isErr := callLiveTool(t, "breeze_get_topology", map[string]any{
		"aggregator_url": base,
	})
	if !isErr {
		t.Fatal("a closed port returned a topology")
	}
	if got["error"] != "unreachable" {
		t.Errorf("error = %v, want unreachable: %s", got["error"], summary)
	}
}

// TestDefaultFleetBaseMatchesTheAggregatorsOwnDefault is what makes the
// duplicated constant safe.
func TestDefaultFleetBaseMatchesTheAggregatorsOwnDefault(t *testing.T) {
	if aggregator.DefaultBasePath != "/fleet" {
		t.Fatalf("the aggregator's default base path is now %q; the mcp package's "+
			"defaultFleetBase must be updated to match", aggregator.DefaultBasePath)
	}

	// Proven end to end as well: the fixture is mounted at the aggregator's
	// default, and the tools reach it without being told where it is.
	f := startFleetFixture(t)
	if _, summary, isErr := callLiveTool(t, "breeze_get_topology", fleetCreds(f.base)); isErr {
		t.Fatalf("the default base path did not reach the aggregator: %s", summary)
	}
}

// TestFleetResponseShapesMatchTheAggregatorsOwnTypes is what makes the mirror
// structs safe.
//
// tools_fleet.go cannot import fleet/aggregator without closing an import cycle,
// so it redeclares the JSON shapes. That duplication is only sound if a rename
// upstream breaks something: without this test, a changed json tag would silently
// decode into a zero value and the tools would report an empty fleet as fact.
//
// This marshals the aggregator's real types and asserts every key the tools read
// is present.
func TestFleetResponseShapesMatchTheAggregatorsOwnTypes(t *testing.T) {
	assertKeys := func(what string, v any, keys ...string) {
		t.Helper()
		body, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal %s: %v", what, err)
		}
		var shape map[string]any
		if err := json.Unmarshal(body, &shape); err != nil {
			t.Fatalf("%s is not a JSON object: %v", what, err)
		}
		for _, key := range keys {
			if _, ok := shape[key]; !ok {
				t.Errorf("%s no longer has the key %q that tools_fleet.go reads; "+
					"update the mirror struct. Actual keys: %v", what, key, keysOf(shape))
			}
		}
	}

	assertKeys("aggregator.Node", aggregator.Node{},
		"service", "calls", "errors", "error_rate", "p99_ms", "last_seen_unix")
	assertKeys("aggregator.Edge", aggregator.Edge{},
		"caller", "callee", "calls", "errors", "error_rate", "p50_ms", "p95_ms", "p99_ms",
		"avg_ms", "last_seen_unix")
	assertKeys("aggregator.Topology", aggregator.Topology{}, "nodes", "edges")
	assertKeys("aggregator.TraceSummary", aggregator.TraceSummary{},
		"trace_id", "services", "root_service", "route", "method", "status", "start_ns",
		"duration_ms", "span_count", "has_error")
	assertKeys("aggregator.TracePage", aggregator.TracePage{}, "traces", "has_more")
	assertKeys("aggregator.Trace", aggregator.Trace{},
		"trace_id", "roots", "services", "span_count", "duration_ms", "start_ns",
		"has_error", "status", "skew_flagged", "orphan_count")
	assertKeys("aggregator.BlastRadius", aggregator.BlastRadius{},
		"service", "error_rate", "calls", "affected", "banner", "computed_unix")
	assertKeys("aggregator.AffectedService", aggregator.AffectedService{},
		"service", "hops", "dependency_error_rate", "attributed_share")

	// The Entry flag is omitempty, so it is only present when true â€” assert it
	// on a value that has it set.
	assertKeys("aggregator.Node{Entry:true}", aggregator.Node{Entry: true}, "entry")

	// contracts.Group embeds Violation, which must stay flat in JSON: the mirror
	// is a single flat struct, and a change to a nested shape would break it
	// without changing any single field name.
	assertKeys("contracts.Group", contracts.Group{
		Violation: contracts.Violation{Rule: "r", Severity: "error"},
		Count:     1,
	},
		"trace_id", "span_id", "caller", "callee", "route", "direction", "path", "rule",
		"expected", "observed", "severity", "timestamp", "count", "first_seen", "last_seen")

	// A span node's own fields, including the root-cause markers the whole of
	// explain_incident depends on.
	assertKeys("aggregator.SpanNode", aggregator.SpanNode{
		Span:         fleet.Span{TraceID: "t", SpanID: "s"},
		RootCause:    true,
		DerivedError: true,
		Orphan:       true,
		Skewed:       true,
	},
		"trace_id", "span_id", "service", "route", "method", "status", "start_ns",
		"duration_ms", "root_cause", "derived_error", "orphan", "skewed")
}

// notesText joins a report's notes for substring assertions.
func notesText(report map[string]any) string {
	raw, ok := report["notes"].([]any)
	if !ok {
		return ""
	}
	parts := make([]string, 0, len(raw))
	for _, n := range raw {
		parts = append(parts, fmt.Sprint(n))
	}
	return strings.Join(parts, " ")
}

func keysOf(shape map[string]any) []string {
	keys := make([]string, 0, len(shape))
	for k := range shape {
		keys = append(keys, k)
	}
	return keys
}

// â”€â”€â”€ the aggregator_url a caller actually pastes â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

// TestFleetToolsAcceptAMountURLAsWellAsAnOrigin is the fix for the most likely
// aggregator_url mistake, and the reason it was worth fixing rather than documenting.
//
// Every other place in this codebase that names an aggregator names its *mount*:
// fleet.TracerConfig.AggregatorURL, dashboard.Config.FleetAggregatorURL, and
// provision_fleet's own returned aggregator_url are all "http://host:9000/fleet". These
// tools take an origin plus a separate base_path. So the value a caller has to hand
// produces /fleet/fleet/api/... â€” and the 404 that follows reports "the fleet aggregator
// feature is not installed", which is false and points at the wrong thing entirely.
func TestFleetToolsAcceptAMountURLAsWellAsAnOrigin(t *testing.T) {
	fixture := startFleetFixture(t)

	// The two spellings of the same aggregator: a bare origin, and the mount URL that
	// every other Fleet configuration field takes.
	for name, aggregatorURL := range map[string]string{
		"origin":           fixture.base,
		"mount URL":        fixture.base + "/fleet",
		"mount with slash": fixture.base + "/fleet/",
	} {
		t.Run(name, func(t *testing.T) {
			args := fleetCreds(aggregatorURL)
			got, summary, isErr := callLiveTool(t, "breeze_get_topology", args)
			if isErr {
				t.Fatalf("aggregator_url %q was refused: %s", aggregatorURL, summary)
			}

			// A reachable aggregator carrying the fixture's spans reports services. An
			// empty topology here would mean the request went somewhere else and
			// happened not to 404.
			if count, _ := got["service_count"].(float64); count == 0 {
				t.Errorf("aggregator_url %q reached something, but it reported no services: %s",
					aggregatorURL, summary)
			}
		})
	}
}
