package dashboard

// route_doc_test.go — the join that carries a route's own description into the
// dashboard's route list and API explorer.
//
// # What is under test
//
// Two facts live in two places and neither side can produce both: the collector
// knows a route's traffic and nothing about its purpose, the Scalar registry knows
// the sentence the developer wrote and nothing about traffic. describeRoute is the
// join, and these tests assert it against the registry's real normalisation rather
// than a restatement of it — `:id` becoming `{id}` and the method being uppercased
// are exactly where a join like this breaks.
//
// The registry is a package global, so these tests share it. Paths are prefixed to
// keep them from colliding with another test's registrations.

import (
	"strings"
	"testing"

	"github.com/nelthaarion/breeze/v2/scalar"
)

const (
	docTestList = "/dashtest/widgets"
	docTestByID = "/dashtest/widgets/:id"
	docTestBare = "/dashtest/undocumented"
)

type docTestParams struct {
	ID string `json:"id" description:"Which widget."`
}

type docTestBody struct {
	Name  string `json:"name" description:"Display name."`
	Count int    `json:"count,omitempty" description:"How many."`
}

// registerDocTestRoutes puts two documented routes in the registry.
//
// scalar.Enable is required: without it RegisterRoute returns immediately, which
// is the silent failure the scalar probe reports and would make every assertion
// below vacuous.
func registerDocTestRoutes(t *testing.T) {
	t.Helper()
	scalar.Enable()

	scalar.RegisterRoute("GET", docTestList, scalar.RouteDoc{
		Title:       "List widgets",
		Description: "Every widget the caller may see.",
		Tags:        []string{"Widgets"},
	})
	scalar.RegisterRoute("POST", docTestByID, scalar.RouteDoc{
		Title: "Replace a widget",
		Input: []scalar.InputGroup{
			{Type: scalar.InputParams, Fields: docTestParams{}},
			{Type: scalar.InputBody, Fields: docTestBody{}, Required: true,
				Description: "The replacement."},
		},
	})
}

// docMap builds the lookup describeRoute takes, the same way handleRoutes does.
func docMap() map[string]scalar.RouteDoc {
	docs := make(map[string]scalar.RouteDoc, 16)
	for _, r := range scalar.Routes() {
		docs[strings.ToUpper(r.Method)+" "+r.Path] = r.Doc
	}
	return docs
}

func TestDescribeRouteCarriesTitleDescriptionAndTags(t *testing.T) {
	registerDocTestRoutes(t)

	stat := RouteStat{Method: "GET", Pattern: docTestList}
	describeRoute(&stat, docMap())

	if !stat.Documented {
		t.Fatal("a documented route was not matched")
	}
	if stat.Summary != "List widgets" {
		t.Errorf("Summary = %q", stat.Summary)
	}
	if stat.Description != "Every widget the caller may see." {
		t.Errorf("Description = %q", stat.Description)
	}
	if len(stat.Tags) != 1 || stat.Tags[0] != "Widgets" {
		t.Errorf("Tags = %v", stat.Tags)
	}
}

// TestDescribeRouteMatchesAParameterisedPattern is the case the join exists to
// get right: the router says ":id" and the registry says "{id}".
func TestDescribeRouteMatchesAParameterisedPattern(t *testing.T) {
	registerDocTestRoutes(t)

	stat := RouteStat{Method: "POST", Pattern: docTestByID}
	describeRoute(&stat, docMap())

	if !stat.Documented {
		t.Fatalf("a :param route was not matched against its {param} registry entry")
	}
	if stat.Summary != "Replace a widget" {
		t.Errorf("Summary = %q", stat.Summary)
	}
}

func TestDescribeRouteLeavesAnUndocumentedRouteAlone(t *testing.T) {
	registerDocTestRoutes(t)

	stat := RouteStat{Method: "GET", Pattern: docTestBare}
	describeRoute(&stat, docMap())

	if stat.Documented {
		t.Error("an unregistered route was reported as documented")
	}
	if stat.Summary != "" || stat.Description != "" || stat.Tags != nil {
		t.Errorf("an unregistered route gained documentation: %+v", stat)
	}
}

// ─── the API explorer ────────────────────────────────────────────────────────

// TestExplorerInputsUseTheDocumentedSchema is the explorer's half of Part 5.
//
// Before this, the explorer could only ever derive path parameters from the
// pattern, typed as string, with no description. Everything else a route accepts —
// its body, its query fields, what any of them mean — was known to the registry
// and unavailable to the UI that exists to construct requests.
func TestExplorerInputsUseTheDocumentedSchema(t *testing.T) {
	registerDocTestRoutes(t)

	doc := docMap()["POST "+breezeToOpenAPIPath(docTestByID)]
	inputs := explorerInputs(doc)

	byType := map[string]APIExplorerInput{}
	for _, in := range inputs {
		byType[in.Type] = in
	}

	params, ok := byType["params"]
	if !ok {
		t.Fatalf("no params group in %+v", inputs)
	}
	id, ok := params.Fields["id"]
	if !ok {
		t.Fatalf("params group has no id: %+v", params)
	}
	if !id.Required {
		t.Error("a path parameter is not marked required; there is no URL without it")
	}
	if id.Description != "Which widget." {
		t.Errorf("id description = %q, want the struct tag's sentence", id.Description)
	}

	body, ok := byType["body"]
	if !ok {
		t.Fatalf("no body group in %+v", inputs)
	}
	if body.Description != "The replacement." {
		t.Errorf("body group description = %q", body.Description)
	}
	name, ok := body.Fields["name"]
	if !ok {
		t.Fatalf("body group has no name: %+v", body)
	}
	if name.Type != "string" || !name.Required {
		t.Errorf("name = %+v, want a required string", name)
	}
	if name.Description != "Display name." {
		t.Errorf("name description = %q", name.Description)
	}

	// omitempty is how the schema inference marks a field optional, so a field
	// declared with it must not come back required — an explorer that demanded
	// every field would make an optional one impossible to omit.
	count, ok := body.Fields["count"]
	if !ok {
		t.Fatalf("body group has no count: %+v", body)
	}
	if count.Required {
		t.Error("an omitempty field is marked required")
	}
	if count.Type != "integer" {
		t.Errorf("count type = %q, want integer", count.Type)
	}
}

// TestExplorerInputsSkipsAGroupWithNoFields keeps empty groups out of the UI.
//
// A group whose Fields is nil infers no schema, and an input group with no fields
// renders as a labelled empty section — worse than absent.
func TestExplorerInputsSkipsAGroupWithNoFields(t *testing.T) {
	inputs := explorerInputs(scalar.RouteDoc{
		Input: []scalar.InputGroup{
			{Type: scalar.InputQuery, Fields: nil},
			{Type: scalar.InputBody, Fields: struct{}{}},
		},
	})
	if len(inputs) != 0 {
		t.Errorf("got %d group(s) for a doc with no usable fields: %+v", len(inputs), inputs)
	}
}

func TestHasExplorerInputIgnoresAnEmptyGroup(t *testing.T) {
	if hasExplorerInput([]APIExplorerInput{{Type: "params"}}, "params") {
		t.Error("a group with no fields counted as present, which would suppress the fallback")
	}
	if !hasExplorerInput([]APIExplorerInput{
		{Type: "params", Fields: map[string]FieldSchema{"id": {Type: "string"}}},
	}, "params") {
		t.Error("a populated group was not detected")
	}
}
