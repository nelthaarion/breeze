package mcp

// These tests are about the two failure modes a static checker actually has.
//
// A checker that misses violations is useless, and a checker that invents them
// gets switched off — so both are tested, and the second gets as much attention
// as the first. Every positive case is paired with a lookalike that must stay
// silent: reflect outside a handler, a same-named package from somewhere else, a
// table name inside a string that is not a query.
//
// Expected lines are found by searching the fixture for a marker comment rather
// than written as literals. A literal line number is a second copy of the
// fixture's layout, and it goes stale the moment anyone adds an import.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeGo writes one fixture file and returns its source, so a caller can find
// marker lines in exactly the bytes that were parsed.
func writeGo(t *testing.T, root, rel, source string) string {
	t.Helper()

	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("creating the directory for %s: %v", rel, err)
	}
	if err := os.WriteFile(full, []byte(source), 0o644); err != nil {
		t.Fatalf("writing %s: %v", rel, err)
	}
	return source
}

// lineOf returns the 1-based line carrying marker.
//
// The marker is required to be unique: two matches mean the fixture no longer
// says what the assertion below thinks it says, and silently taking the first
// would turn that into a confusing mismatch instead of a clear failure.
func lineOf(t *testing.T, source, marker string) int {
	t.Helper()

	found := 0
	line := 0
	for i, text := range strings.Split(source, "\n") {
		if strings.Contains(text, marker) {
			found++
			line = i + 1
		}
	}
	switch found {
	case 1:
		return line
	case 0:
		t.Fatalf("the fixture does not contain the marker %q", marker)
	default:
		t.Fatalf("the marker %q appears %d times, so it cannot identify one line", marker, found)
	}
	return 0
}

// findingsFor returns the findings for one rule, so an assertion can be specific
// about the rule it cares about without being disturbed by unrelated ones.
func findingsFor(report idiomReport, rule string) []idiomFinding {
	var out []idiomFinding
	for _, f := range report.Findings {
		if f.Rule == rule {
			out = append(out, f)
		}
	}
	return out
}

func checkFixture(t *testing.T, root string) idiomReport {
	t.Helper()

	report, err := runIdiomCheck(root)
	if err != nil {
		t.Fatalf("checking %s: %v", root, err)
	}
	return report
}

// TestCheckedIdiomRulesMatchTheExplanations is the parity test that keeps
// explain_idiom honest.
//
// explain_idiom tells a caller whether a rule is machine-checked, and a caller
// uses that to decide whether a clean report means anything. If a rule claims to
// be checked and no check produces it, that promise is false and the caller has
// no way to find out. So the two lists must agree exactly, in both directions.
func TestCheckedIdiomRulesMatchTheExplanations(t *testing.T) {
	implemented := map[string]bool{}
	for _, rule := range checkedIdiomRules() {
		if implemented[rule] {
			t.Errorf("%s is listed twice by checkedIdiomRules", rule)
		}
		implemented[rule] = true

		if _, known := idiomsByRule[rule]; !known {
			t.Errorf("this file checks %s, but idioms.go has no such rule to explain it", rule)
		}
	}

	for _, i := range idiomList {
		switch {
		case i.Checked && !implemented[i.Rule]:
			t.Errorf("%s is advertised as statically checked, but no check produces it, so a clean report is misleading", i.Rule)
		case !i.Checked && implemented[i.Rule]:
			t.Errorf("%s is checked by this file but advertised as unchecked, so callers are told to verify it by hand for no reason", i.Rule)
		}
	}
}

// TestCheckIdiomsFindsReflectionInAHandler is the first case the task names.
func TestCheckIdiomsFindsReflectionInAHandler(t *testing.T) {
	root := t.TempDir()

	source := writeGo(t, root, "handlers/user.go", `package handlers

import (
	"reflect"

	"github.com/nelthaarion/breeze"
)

// ShowUser is a handler, so reflect here runs once per request.
func ShowUser(c *breeze.Context) error {
	kind := reflect.TypeOf(c).String() // want:reflect-in-handler
	return c.WriteString(kind)
}

// buildIndex runs at startup, where reflect is how half this framework works.
// A rule that flagged this would be turned off within a day.
func buildIndex(v any) string {
	return reflect.TypeOf(v).String()
}
`)

	report := checkFixture(t, root)

	found := findingsFor(report, "no-reflection-in-handlers")
	if len(found) != 1 {
		t.Fatalf("expected exactly one reflection finding, got %d: %+v", len(found), found)
	}

	got := found[0]
	if want := lineOf(t, source, "want:reflect-in-handler"); got.Line != want {
		t.Errorf("the finding is reported on line %d; the reflect call is on line %d", got.Line, want)
	}
	if got.File != "handlers/user.go" {
		t.Errorf("File = %q, want the path relative to the checked root", got.File)
	}
	if got.Severity != severityError {
		t.Errorf("Severity = %q, want %q, which is what idioms.go gives this rule", got.Severity, severityError)
	}
	if !strings.Contains(got.Message, "ShowUser") {
		t.Errorf("the message does not name the handler, so a reader cannot tell which one: %q", got.Message)
	}

	// The startup helper must not be reported. This is the assertion that keeps
	// the rule usable: reflect is legitimate everywhere except the request path.
	if report.Errors != 1 {
		t.Errorf("the report counts %d errors; reflect outside a handler must not be one of them: %+v",
			report.Errors, report.Findings)
	}
}

// TestCheckIdiomsFindsMiddlewareRegisteredInTheWrongOrder is the second case the
// task names.
func TestCheckIdiomsFindsMiddlewareRegisteredInTheWrongOrder(t *testing.T) {
	root := t.TempDir()

	source := writeGo(t, root, "setup.go", `package main

import (
	"github.com/nelthaarion/breeze"
	"github.com/nelthaarion/breeze/dashboard"
	"github.com/nelthaarion/breeze/fleet"
)

func setupObservability(router *breeze.Router, c *dashboard.Collector, tr *fleet.Tracer) {
	router.Use(dashboard.Middleware(c))
	router.Use(fleet.Middleware(tr)) // want:fleet-after-dashboard
}
`)

	report := checkFixture(t, root)

	found := findingsFor(report, "fleet-before-dashboard")
	if len(found) != 1 {
		t.Fatalf("expected exactly one ordering finding, got %d: %+v", len(found), found)
	}

	got := found[0]
	if want := lineOf(t, source, "want:fleet-after-dashboard"); got.Line != want {
		t.Errorf("the finding is reported on line %d; the misplaced Use call is on line %d", got.Line, want)
	}
	if got.Severity != severityError {
		t.Errorf("Severity = %q, want %q", got.Severity, severityError)
	}
}

// TestCheckIdiomsAcceptsTheCorrectMiddlewareOrder is the same fixture with the
// two calls the right way round. Without this the ordering rule could be
// satisfied by something that simply reports every project using both.
func TestCheckIdiomsAcceptsTheCorrectMiddlewareOrder(t *testing.T) {
	root := t.TempDir()

	writeGo(t, root, "setup.go", `package main

import (
	"github.com/nelthaarion/breeze"
	"github.com/nelthaarion/breeze/dashboard"
	"github.com/nelthaarion/breeze/fleet"
)

func setupObservability(router *breeze.Router, c *dashboard.Collector, tr *fleet.Tracer) {
	router.Use(fleet.Middleware(tr))
	router.Use(dashboard.Middleware(c))
}
`)

	if found := findingsFor(checkFixture(t, root), "fleet-before-dashboard"); len(found) != 0 {
		t.Errorf("the correct order was reported as a violation: %+v", found)
	}
}

// TestCheckIdiomsResolvesPackagesByImportPathNotByName is the defence against the
// false positive that would matter most.
//
// The rules match on selectors like dashboard.Middleware. Any project is free to
// import something else called dashboard, and a checker that decided from the
// spelling alone would report a violation in code that has nothing to do with
// this framework.
func TestCheckIdiomsResolvesPackagesByImportPathNotByName(t *testing.T) {
	root := t.TempDir()

	writeGo(t, root, "lookalike.go", `package main

import (
	"example.com/vendorkit/dashboard"
	"example.com/vendorkit/fleet"
	"example.com/vendorkit/middlewares"
)

type Router struct{}

func (r *Router) Use(_ ...any) {}

func setup(router *Router) {
	router.Use(dashboard.Middleware(nil))
	router.Use(fleet.Middleware(nil))
	_ = middlewares.SwaggerMiddleware
}
`)

	report := checkFixture(t, root)
	if report.Count != 0 {
		t.Errorf("packages that merely share a name produced %d finding(s): %+v", report.Count, report.Findings)
	}
	if report.Files != 1 {
		t.Errorf("the checker looked at %d files, want 1", report.Files)
	}
}

// TestCheckIdiomsFindsTheDeprecatedSwaggerSpelling covers the rule whose whole
// point is that the code compiles and still does not do what it says.
func TestCheckIdiomsFindsTheDeprecatedSwaggerSpelling(t *testing.T) {
	root := t.TempDir()

	source := writeGo(t, root, "docs.go", `package main

import (
	"github.com/nelthaarion/breeze"
	"github.com/nelthaarion/breeze/middlewares"
)

func setupDocs(router *breeze.Router) {
	router.Use(middlewares.SwaggerMiddleware(router, middlewares.ScalarOptions{})) // want:swagger
}
`)

	found := findingsFor(checkFixture(t, root), "scalar-is-the-viewer")
	if len(found) != 1 {
		t.Fatalf("expected one deprecated-spelling finding, got %d: %+v", len(found), found)
	}
	if want := lineOf(t, source, "want:swagger"); found[0].Line != want {
		t.Errorf("the finding is on line %d, the call is on line %d", found[0].Line, want)
	}
	if found[0].Severity != severityWarning {
		t.Errorf("Severity = %q, want %q", found[0].Severity, severityWarning)
	}
	if !strings.Contains(found[0].Message, "ScalarMiddleware") {
		t.Errorf("the message does not name the replacement: %q", found[0].Message)
	}
}

// TestCheckIdiomsFindsAHandWrittenTableNameOnlyForKnownTables checks the rule
// that needs the project's own models to decide anything, together with the two
// ways it must stay quiet: a table this project does not declare, and a longer
// identifier that merely starts with a known table name.
func TestCheckIdiomsFindsAHandWrittenTableNameOnlyForKnownTables(t *testing.T) {
	root := t.TempDir()

	// The shape `breeze generate model` emits, including the constant the rule
	// exists to recommend.
	writeGo(t, root, "models/order.go", `package models

type Order struct {
	ID    int64   `+"`json:\"id\" db:\"id\"`"+`
	Total float64 `+"`json:\"total\" db:\"total\"`"+`
}

const OrderTable = "orders"
`)

	source := writeGo(t, root, "store.go", `package main

const (
	listOrders   = "SELECT id, total FROM orders WHERE id = ?" // want:literal-table
	listItems    = "SELECT id FROM order_items"
	listUnknown  = "SELECT id FROM customers"
	notAQuery    = "orders are processed nightly"
)
`)

	report := checkFixture(t, root)

	found := findingsFor(report, "generated-model-accessors")
	if len(found) != 1 {
		t.Fatalf("expected one table-name finding, got %d: %+v", len(found), found)
	}
	if want := lineOf(t, source, "want:literal-table"); found[0].Line != want {
		t.Errorf("the finding is on line %d, the query is on line %d", found[0].Line, want)
	}
	if !strings.Contains(found[0].Message, "OrderTable") {
		t.Errorf("the message does not name the constant to use instead: %q", found[0].Message)
	}
}

// TestCheckIdiomsExemptsGeneratedFiles keeps the rule off code its author cannot
// change by hand. The generator writes the table name into its own output, and
// reporting that would be advice nobody can act on.
func TestCheckIdiomsExemptsGeneratedFiles(t *testing.T) {
	root := t.TempDir()

	writeGo(t, root, "models/order.go", `package models

type Order struct{}

const OrderTable = "orders"
`)
	writeGo(t, root, "queries_generated.go", "// Code generated by breeze. DO NOT EDIT.\n\npackage main\n\nconst listOrders = \"SELECT id FROM orders\"\n")

	if found := findingsFor(checkFixture(t, root), "generated-model-accessors"); len(found) != 0 {
		t.Errorf("a generated file was reported: %+v", found)
	}
}

// TestCheckIdiomsReportsUnparseableFilesWithoutAbandoningTheRest is the property
// that makes this tool usable mid-edit, which is when an agent needs it most.
func TestCheckIdiomsReportsUnparseableFilesWithoutAbandoningTheRest(t *testing.T) {
	root := t.TempDir()

	writeGo(t, root, "broken.go", "package main\n\nfunc oops( {\n")
	source := writeGo(t, root, "handler.go", `package main

import (
	"reflect"

	"github.com/nelthaarion/breeze"
)

func Show(c *breeze.Context) error {
	_ = reflect.TypeOf(c) // want:still-found
	return nil
}
`)

	report := checkFixture(t, root)

	if len(report.Skipped) != 1 {
		t.Fatalf("expected exactly one skipped file, got %v", report.Skipped)
	}
	if !strings.Contains(report.Skipped[0], "broken.go") {
		t.Errorf("the skipped entry does not name the file: %q", report.Skipped[0])
	}
	if report.Files != 1 {
		t.Errorf("files_checked = %d, want 1: the unparseable file must not be counted as checked", report.Files)
	}

	found := findingsFor(report, "no-reflection-in-handlers")
	if len(found) != 1 {
		t.Fatalf("the parseable file was not checked: %+v", report.Findings)
	}
	if want := lineOf(t, source, "want:still-found"); found[0].Line != want {
		t.Errorf("the finding is on line %d, want %d", found[0].Line, want)
	}
}

// TestCheckIdiomsOnAnEmptyTreeSaysSoRatherThanReportingClean is the distinction a
// caller cannot make from an empty findings list, and the reason Notes exists.
func TestCheckIdiomsOnAnEmptyTreeSaysSoRatherThanReportingClean(t *testing.T) {
	report := checkFixture(t, t.TempDir())

	if report.Count != 0 || report.Files != 0 {
		t.Fatalf("an empty tree produced count=%d files=%d", report.Count, report.Files)
	}

	joined := strings.Join(report.Notes, " ")
	if !strings.Contains(joined, "No Go files") {
		t.Errorf("a clean report over zero files did not say so; notes = %v", report.Notes)
	}
}

// TestCheckIdiomsFindsNothingInAGeneratedProject is the case the task asks for
// directly: the framework's own generator must produce code that satisfies the
// framework's own conventions.
//
// This runs the real generator, so it is also the test that would catch a rule
// written to match the generator's output by accident.
func TestCheckIdiomsFindsNothingInAGeneratedProject(t *testing.T) {
	project := newFixtureProject(t, "idiomclean", fixtureConfig)

	report := checkFixture(t, project)

	if report.Files == 0 {
		t.Fatal("no Go files were checked, so this proves nothing about the generated project")
	}
	if report.Count != 0 {
		t.Errorf("a freshly generated project violates %d of its own conventions: %+v",
			report.Count, report.Findings)
	}
	if len(report.Rules) != len(checkedIdiomRules()) {
		t.Errorf("the report says %d rules were applied, want %d", len(report.Rules), len(checkedIdiomRules()))
	}
}

// TestCheckIdiomsToolReturnsStructuredFindings goes through tools/call, because
// that is how an agent reaches this and the structured half is what it branches
// on. A tool that returned only prose would pass every test above.
func TestCheckIdiomsToolReturnsStructuredFindings(t *testing.T) {
	root := t.TempDir()
	writeGo(t, root, "handler.go", `package main

import (
	"reflect"

	"github.com/nelthaarion/breeze"
)

func Show(c *breeze.Context) error {
	_ = reflect.TypeOf(c)
	return nil
}
`)

	srv := NewServer("test")

	// Called directly first, because this is the only way to see the value the
	// tool actually produced. Going through tools/call marshals and re-decodes
	// it, so every field arrives as a map entry and a wrong type would pass.
	direct := srv.tools["breeze_check_idioms"].run(mustJSON(t, map[string]any{"path": root}))

	// A violation is a successful check, not a failed call. An agent that saw
	// IsError here would retry the call instead of fixing the code.
	if direct.IsError {
		t.Fatalf("a project with a violation was reported as a tool failure: %s", direct.Content[0].Text)
	}

	got, ok := direct.StructuredContent.(idiomReport)
	if !ok {
		t.Fatalf("check_idioms returned %T, not an idiomReport", direct.StructuredContent)
	}
	if got.Count != 1 || len(got.Findings) != 1 {
		t.Fatalf("count = %d with %d findings, want one of each", got.Count, len(got.Findings))
	}

	f := got.Findings[0]
	if f.File == "" || f.Line == 0 || f.Rule == "" || f.Message == "" || f.Severity == "" {
		t.Errorf("a finding is missing part of the required shape: %+v", f)
	}

	// The summary must carry the counts, so a client that only shows text still
	// tells its user something true.
	if text := direct.Content[0].Text; !strings.Contains(text, "1 violation(s)") {
		t.Errorf("the summary does not report the count: %q", text)
	}

	// Then over the wire, because the structured half is worthless if it does not
	// survive being serialised: an agent reads these five keys by name, and a
	// renamed json tag would not show up in the typed assertions above.
	wire := callTool(t, srv, "breeze_check_idioms", map[string]any{"path": root})
	report, ok := wire.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structuredContent came over the wire as %T, want a JSON object", wire.StructuredContent)
	}

	list, ok := report["findings"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("findings came over the wire as %#v", report["findings"])
	}
	finding, ok := list[0].(map[string]any)
	if !ok {
		t.Fatalf("a finding came over the wire as %T", list[0])
	}
	for _, key := range []string{"file", "line", "rule", "message", "severity"} {
		if value, present := finding[key]; !present || value == nil || value == "" {
			t.Errorf("the serialised finding has no usable %q: %#v", key, finding)
		}
	}
}

// TestCheckIdiomsRejectsAPathThatIsNotADirectory covers the argument error, which
// is a genuine tool failure and must be reported as one.
func TestCheckIdiomsRejectsAPathThatIsNotADirectory(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "go.mod")
	if err := os.WriteFile(file, []byte("module example.com/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := callTool(t, NewServer("test"), "breeze_check_idioms", map[string]any{"path": file})
	if !res.IsError {
		t.Fatalf("checking a file instead of a directory succeeded: %+v", res)
	}
	if !strings.Contains(res.Content[0].Text, "not a directory") {
		t.Errorf("the message does not explain the problem: %q", res.Content[0].Text)
	}
}
