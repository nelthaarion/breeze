package mcp_test

// tools_routes_doc_test.go — Part 5: a route's description surviving into the
// tools an agent reads.
//
// # The gap this closes
//
// A developer writes `middleware.Doc(scalar.RouteDoc{Title: "Create an order"})`
// once. Before this, that sentence reached exactly one consumer: the OpenAPI
// document. Everything else an agent could ask — the routes endpoint, the API
// explorer, breeze_get_routes — returned a method, a path and a latency number,
// which is enough to render a table and not enough to answer "which endpoint
// should I call".
//
// So these tests assert the join, end to end, against a real service: the
// description the fixture registered comes back through the tool, and a route
// without one is reported as undocumented rather than silently blank.
//
// The distinction matters more than it looks. An undocumented route is absent
// from the OpenAPI document entirely, so an agent that consulted the document
// first was told the endpoint does not exist. "documented: false" is the only
// place that is reported.

import (
	"strings"
	"testing"
)

func TestGetRoutesCarriesTheDescriptionTheDeveloperWrote(t *testing.T) {
	fx := startFixture(t)
	driveTraffic(t, fx.url)

	got, summary, isErr := callLiveTool(t, "breeze_get_routes", creds(fx.url))
	if isErr {
		t.Fatalf("breeze_get_routes reported an error: %s", summary)
	}

	route := findRoute(t, got, "GET", "/api/widgets")

	// The three fields the fixture registered, each read back from the tool
	// rather than from the registry — so a break anywhere in the chain (registry
	// key normalisation, the dashboard's join, the tool's struct tags) fails here.
	if route["summary"] != "List widgets" {
		t.Errorf("summary = %v, want %q", route["summary"], "List widgets")
	}
	if route["description"] != "Returns every widget the caller may see." {
		t.Errorf("description = %v", route["description"])
	}
	if route["documented"] != true {
		t.Errorf("documented = %v for a route with a Doc wrapper", route["documented"])
	}

	tags, ok := route["tags"].([]any)
	if !ok || len(tags) != 1 || tags[0] != "Widgets" {
		t.Errorf("tags = %v, want [Widgets]", route["tags"])
	}
}

// TestGetRoutesReportsAnUndocumentedRouteAsUndocumented is the half that makes
// the other half meaningful.
//
// Without it, a join that returned blank summaries for everything would still
// pass the test above as long as one route happened to match.
func TestGetRoutesReportsAnUndocumentedRouteAsUndocumented(t *testing.T) {
	fx := startFixture(t)

	got, summary, isErr := callLiveTool(t, "breeze_get_routes", creds(fx.url))
	if isErr {
		t.Fatalf("breeze_get_routes reported an error: %s", summary)
	}

	// /api/broken has no Doc wrapper, so it is absent from the OpenAPI document.
	route := findRoute(t, got, "GET", "/api/broken")
	if route["documented"] != false {
		t.Errorf("documented = %v for a route with no Doc wrapper", route["documented"])
	}
	if _, present := route["summary"]; present {
		t.Errorf("an undocumented route carries a summary: %v", route["summary"])
	}

	// The counts and the note, which are what let a caller act on this rather
	// than inspect every entry.
	documented := numberField(t, got, "documented")
	total := numberField(t, got, "total")
	if documented < 1 {
		t.Errorf("documented = %v; the fixture documents at least one route", documented)
	}
	if documented >= total {
		t.Errorf("documented = %v of %v; the fixture leaves several routes undocumented",
			documented, total)
	}

	notes, _ := got["notes"].([]any)
	found := false
	for _, raw := range notes {
		if note, _ := raw.(string); strings.Contains(note, "middleware.Doc") {
			found = true
		}
	}
	if !found {
		t.Errorf("no note names the missing Doc wrapper: %v", notes)
	}

	// The note must say what the real consequence is — absence from the OpenAPI
	// document — rather than only that a description is missing.
	joined := ""
	for _, raw := range notes {
		if note, _ := raw.(string); note != "" {
			joined += note + " "
		}
	}
	if !strings.Contains(joined, "OpenAPI") {
		t.Errorf("the note does not mention the OpenAPI document: %v", notes)
	}
}

// TestTheSummaryLineReportsTheDocumentedCount keeps the prose result useful to a
// client that shows only the first line.
func TestTheSummaryLineReportsTheDocumentedCount(t *testing.T) {
	fx := startFixture(t)

	_, summary, isErr := callLiveTool(t, "breeze_get_routes", creds(fx.url))
	if isErr {
		t.Fatalf("breeze_get_routes reported an error: %s", summary)
	}
	if !strings.Contains(summary, "documented") {
		t.Errorf("the summary line does not mention documentation: %s", summary)
	}
}

// findRoute returns one route entry from a breeze_get_routes result.
func findRoute(t *testing.T, result map[string]any, method, pattern string) map[string]any {
	t.Helper()

	routes, ok := result["routes"].([]any)
	if !ok {
		t.Fatalf("routes is %T, want a list", result["routes"])
	}
	for _, raw := range routes {
		entry, ok := raw.(map[string]any)
		if ok && entry["method"] == method && entry["pattern"] == pattern {
			return entry
		}
	}
	t.Fatalf("no %s %s in the route list", method, pattern)
	return nil
}
