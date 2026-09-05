package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nelthaarion/breeze"
	"github.com/nelthaarion/breeze/events"
	"github.com/nelthaarion/breeze/observability"
	"github.com/nelthaarion/breeze/workflow"
)

// These tests cover the path a workflow execution takes to reach the
// Events page. That path broke once already: the engine published to a
// different collector than the dashboard read from, so executions were
// recorded correctly and displayed nowhere. The tests below assert the
// wiring end to end rather than trusting it.

// invokeAPI drives a request through the real router, so route matching
// and middleware are exercised rather than the handler alone.
//
// The router matches on Path alone and the query lives in its own field,
// exactly as the HTTP parser produces it, so a "path?query" string is
// split here rather than handed to Find intact.
func invokeAPI(t *testing.T, router *breeze.Router, target string) *breeze.Context {
	t.Helper()
	path, rawQuery, _ := strings.Cut(target, "?")
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		t.Fatalf("bad query %q: %v", rawQuery, err)
	}
	req := &breeze.HTTPRequest{
		Method: breeze.GET,
		Path:   path,
		Query:  query,
		Header: map[string]string{},
	}
	handler, middlewares, params := router.Find(req)
	if handler == nil {
		t.Fatalf("router.Find(GET %s) did not resolve to a handler", path)
	}
	ctx := breeze.NewContext(req.Method, req.Path)
	ctx.Req = req
	ctx.SetParams(params)
	ctx.SetMiddlewareChain(middlewares, handler)
	ctx.Next()
	return ctx
}

// decodeEvents reads the Events API response.
func decodeEvents(t *testing.T, ctx *breeze.Context) eventsPayload {
	t.Helper()
	if ctx.Res.Status != 0 && ctx.Res.Status != 200 {
		t.Fatalf("status = %d, want 200; body=%s", ctx.Res.Status, ctx.Res.Body)
	}
	var got eventsPayload
	if err := json.Unmarshal(ctx.Res.Body, &got); err != nil {
		t.Fatalf("unmarshal events payload: %v; body=%s", err, ctx.Res.Body)
	}
	return got
}

// newWorkflowDashboard wires a dashboard and an engine the way an
// application is expected to: the engine publishes into the collector
// the dashboard reads from.
func newWorkflowDashboard(t *testing.T) (*breeze.Router, *workflow.Engine, func()) {
	t.Helper()

	router := breeze.NewRouter()
	cfg := DefaultConfig()
	cfg.DisableAuth = true
	coll := Install(nil, router, cfg)

	// A private bus keeps these tests independent of events.Default,
	// which other tests in the package also use.
	bus := events.New()
	detach := coll.AttachEvents(bus)

	col := coll.Observability()
	if col == nil {
		t.Fatal("Observability() = nil after AttachEvents; the engine would have nowhere to publish")
	}

	engine := workflow.NewEngine(workflow.Config{Bus: bus, Collector: col})
	return router, engine, func() { detach() }
}

// TestWorkflowExecutionAppearsInEventsAPI is the regression test for the
// collector wiring bug.
func TestWorkflowExecutionAppearsInEventsAPI(t *testing.T) {
	router, engine, cleanup := newWorkflowDashboard(t)
	defer cleanup()

	def := workflow.New("billing").
		Step("validate", func(*workflow.Context) error { return nil }).
		Step("charge", func(*workflow.Context) error { return nil },
			workflow.WithDependsOn("validate"))
	if err := engine.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}

	res, err := engine.Run(context.Background(), "billing", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := decodeEvents(t, invokeAPI(t, router, "/dashboard/api/events?source=workflow"))
	if !got.Attached {
		t.Fatal("Attached = false, want true")
	}
	if len(got.Recent) == 0 {
		t.Fatal("no workflow rows in the Events API; the engine's signals are not reaching the dashboard")
	}

	row := got.Recent[0]
	if row.Source != string(observability.SourceWorkflow) {
		t.Errorf("Source = %q, want %q", row.Source, observability.SourceWorkflow)
	}
	if row.Name != "billing" {
		t.Errorf("Name = %q, want \"billing\"", row.Name)
	}
	if row.ExecutionID != res.ExecutionID {
		t.Errorf("ExecutionID = %q, want %q", row.ExecutionID, res.ExecutionID)
	}
	if row.State != "completed" {
		t.Errorf("State = %q, want \"completed\"", row.State)
	}
	if row.Failed {
		t.Error("Failed = true, want false")
	}
	if len(row.Spans) != 2 {
		t.Fatalf("len(Spans) = %d, want 2 (one per step)", len(row.Spans))
	}
}

// TestWorkflowParallelStepsShareAPhase is what makes parallelism visible
// in the UI: the frontend groups spans by phase, so steps that ran
// concurrently must report the same one.
func TestWorkflowParallelStepsShareAPhase(t *testing.T) {
	router, engine, cleanup := newWorkflowDashboard(t)
	defer cleanup()

	def := workflow.New("fanout").
		Step("root", func(*workflow.Context) error { return nil }).
		Step("a", func(*workflow.Context) error { return nil }, workflow.WithDependsOn("root")).
		Step("b", func(*workflow.Context) error { return nil }, workflow.WithDependsOn("root")).
		Step("c", func(*workflow.Context) error { return nil }, workflow.WithDependsOn("root")).
		Step("join", func(*workflow.Context) error { return nil },
			workflow.WithDependsOn("a", "b", "c"))
	if err := engine.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := engine.Run(context.Background(), "fanout", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := decodeEvents(t, invokeAPI(t, router, "/dashboard/api/events?source=workflow"))
	if len(got.Recent) == 0 {
		t.Fatal("no workflow rows returned")
	}

	byPhase := map[string][]string{}
	for _, sp := range got.Recent[0].Spans {
		byPhase[sp.Phase] = append(byPhase[sp.Phase], sp.Name)
	}
	if len(byPhase) != 3 {
		t.Fatalf("got %d distinct phases %v, want 3 (root | a,b,c | join)", len(byPhase), byPhase)
	}

	// Exactly one phase must hold the three concurrent steps.
	var parallel []string
	for _, names := range byPhase {
		if len(names) == 3 {
			parallel = names
		}
	}
	if parallel == nil {
		t.Fatalf("no phase contains 3 steps; parallel steps are not grouped: %v", byPhase)
	}
	for _, want := range []string{"a", "b", "c"} {
		found := false
		for _, name := range parallel {
			if name == want {
				found = true
			}
		}
		if !found {
			t.Errorf("step %q missing from the parallel phase %v", want, parallel)
		}
	}
}

// TestWorkflowFailureIsVisible asserts the failure and its rollback
// reach the page, since a silent failure is the worst possible outcome
// for an observability surface.
func TestWorkflowFailureIsVisible(t *testing.T) {
	router, engine, cleanup := newWorkflowDashboard(t)
	defer cleanup()

	sentinel := errors.New("card declined")
	def := workflow.New("checkout").
		Step("reserve", func(*workflow.Context) error { return nil },
			workflow.WithCompensation(func(*workflow.Context) error { return nil })).
		Step("pay", func(*workflow.Context) error {
			return workflow.NonRetryable(sentinel)
		}, workflow.WithDependsOn("reserve"))
	if err := engine.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := engine.Run(context.Background(), "checkout", nil); err == nil {
		t.Fatal("Run returned nil error for a failing workflow")
	}

	got := decodeEvents(t, invokeAPI(t, router, "/dashboard/api/events?source=workflow"))
	if len(got.Recent) == 0 {
		t.Fatal("no workflow rows returned")
	}

	row := got.Recent[0]
	if !row.Failed {
		t.Error("Failed = false, want true")
	}
	if row.State != "compensated" {
		t.Errorf("State = %q, want \"compensated\"", row.State)
	}
	if row.Error == "" {
		t.Error("Error is empty; the failure reason is not shown")
	}

	var failed *eventSpan
	for i := range row.Spans {
		if row.Spans[i].Failed {
			failed = &row.Spans[i]
		}
	}
	if failed == nil {
		t.Fatal("no span marked failed; the failing step is not identifiable in the UI")
	}
	if failed.Name != "pay" {
		t.Errorf("failed span = %q, want \"pay\"", failed.Name)
	}
}

// TestEventsSourceFilter checks the filter both restricts and, when
// absent, does not: a filter that quietly hides everything would be
// worse than none.
func TestEventsSourceFilter(t *testing.T) {
	router, engine, cleanup := newWorkflowDashboard(t)
	defer cleanup()

	def := workflow.New("filtered").
		Step("only", func(*workflow.Context) error { return nil })
	if err := engine.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := engine.Run(context.Background(), "filtered", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Workflow events also travel the bus as plain events, so an
	// unfiltered query must return strictly more than the workflow one.
	all := decodeEvents(t, invokeAPI(t, router, "/dashboard/api/events"))
	wf := decodeEvents(t, invokeAPI(t, router, "/dashboard/api/events?source=workflow"))
	if len(wf.Recent) == 0 {
		t.Fatal("source=workflow returned nothing")
	}
	if len(all.Recent) <= len(wf.Recent) {
		t.Errorf("unfiltered returned %d rows, filtered %d; the filter is not narrowing anything",
			len(all.Recent), len(wf.Recent))
	}
	for _, r := range wf.Recent {
		if r.Source != "workflow" {
			t.Errorf("source=workflow returned a %q row", r.Source)
		}
	}

	// "all" is the UI's word for no restriction and must behave like an
	// empty filter, not like a source named "all".
	explicit := decodeEvents(t, invokeAPI(t, router, "/dashboard/api/events?source=all"))
	if len(explicit.Recent) != len(all.Recent) {
		t.Errorf("source=all returned %d rows, unfiltered %d; they must agree",
			len(explicit.Recent), len(all.Recent))
	}

	// An unknown source matches nothing rather than erroring.
	none := decodeEvents(t, invokeAPI(t, router, "/dashboard/api/events?source=nope"))
	if len(none.Recent) != 0 {
		t.Errorf("unknown source returned %d rows, want 0", len(none.Recent))
	}
}

// TestObservabilityNilBeforeAttach documents the contract the engine
// depends on: before AttachEvents there is nothing to publish into, and
// the accessor says so instead of handing back a collector that goes
// nowhere.
func TestObservabilityNilBeforeAttach(t *testing.T) {
	c := newCollector(DefaultConfig(), nil)
	if c.Observability() != nil {
		t.Error("Observability() != nil before AttachEvents")
	}
	if c.EventsAttached() {
		t.Error("EventsAttached() = true before AttachEvents")
	}

	bus := events.New()
	detach := c.AttachEvents(bus)
	if c.Observability() == nil {
		t.Error("Observability() = nil after AttachEvents")
	}

	detach()
	if c.Observability() != nil {
		t.Error("Observability() != nil after detach")
	}
}

// TestWorkflowRowSurvivesRoundTrip guards the JSON contract the frontend
// reads. The workflow fields are omitempty, so a rename would silently
// drop them from the page rather than fail a build.
func TestWorkflowRowSurvivesRoundTrip(t *testing.T) {
	row := eventRowFrom(observability.Signal{
		ID:     7,
		Source: observability.SourceWorkflow,
		Name:   "orders",
		Time:   time.Now(),
		Attrs: map[string]string{
			"execution_id": "exec-1",
			"state":        "compensated",
			"trigger":      "OrderCreated",
		},
		Spans: []observability.Span{{Name: "step", Phase: "L1"}},
	})

	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// These key names are what dashboard.js reads.
	for key, want := range map[string]string{
		"execution_id": "exec-1",
		"state":        "compensated",
		"trigger":      "OrderCreated",
	} {
		if got, _ := wire[key].(string); got != want {
			t.Errorf("wire[%q] = %v, want %q", key, wire[key], want)
		}
	}

	spans, ok := wire["spans"].([]any)
	if !ok || len(spans) != 1 {
		t.Fatalf("spans missing from the wire format: %v", wire["spans"])
	}
	span, _ := spans[0].(map[string]any)
	if got, _ := span["phase"].(string); got != "L1" {
		t.Errorf("span phase = %v, want \"L1\"; the frontend groups parallel steps on this", span["phase"])
	}
}
