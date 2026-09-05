package mcp

// tools_knowledge_test.go — the tests that hold the knowledge tools to their
// promises.
//
// The promise that matters most is the one about drift. describe_schema exists so
// an agent can find out what a configuration accepts without sending one and
// reading the error, and that is only worth doing if the schema says the same
// thing the generator does. Two separate reflective walks over one struct is
// exactly the arrangement that rots quietly: a field is added, one walk sees it,
// the other does not, and nothing fails until an agent builds a configuration
// from a schema that was already wrong. So the parity test compares the two
// lists directly and names the difference in both directions.
//
// The llms tests are about the same kind of claim. A freshness checker that
// always answered "current" would pass any test that only asked whether it ran,
// so staleness is provoked by really changing the project — generating a model
// into it — and the checker has to notice on its own.
//
// The fixtures are real generated projects, built by newFixtureProject from
// tools_plan_test.go, and their models come from the real model generator. A
// hand-written models/ directory would only prove the parser can read what this
// file writes; what needs proving is that it can read what `breeze generate
// model` emits. The single exception is the relationship field in the models
// test, which the CLI cannot express at all — see addRelationshipField.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nelthaarion/breeze/internal/generator"
	"github.com/nelthaarion/breeze/scalar"
)

// ─── describe_schema ─────────────────────────────────────────────────────────

// TestSchemaFlagPathsMatchGeneratorFlagPaths is the parity assertion the whole
// schema tool rests on: every dotted path the schema advertises under x-flag is
// one the generator's setters accept, and every path they accept is advertised.
//
// Both directions are checked because they fail differently. A path in the
// schema and not in the generator is an agent being told about a field that does
// not exist; a path in the generator and not in the schema is a field an agent
// can never discover. The first produces a confusing rejection, the second
// produces silence, and the second is worse.
func TestSchemaFlagPathsMatchGeneratorFlagPaths(t *testing.T) {
	srv := NewServer("test")

	res := srv.tools["breeze_describe_schema"].run(mustJSON(t, map[string]any{}))
	if res.IsError {
		t.Fatalf("describe_schema failed: %s", res.Content[0].Text)
	}

	got, ok := res.StructuredContent.(schemaResult)
	if !ok {
		t.Fatalf("describe_schema returned %T, not a schemaResult", res.StructuredContent)
	}

	// The comparison is against the schema document itself rather than the
	// convenience list in the result, so that the thing being checked is what a
	// caller would parse out of the schema.
	fromSchema, err := generator.SchemaFlagPaths(got.Schema)
	if err != nil {
		t.Fatalf("reading flag paths back out of the schema: %v", err)
	}
	fromGenerator := generator.FlagPaths()

	inSchema := map[string]bool{}
	for _, p := range fromSchema {
		inSchema[p] = true
	}
	inGenerator := map[string]bool{}
	for _, p := range fromGenerator {
		inGenerator[p] = true
	}

	for _, p := range fromGenerator {
		if !inSchema[p] {
			t.Errorf("the generator accepts --%s but the schema does not mention it, so an agent reading the schema cannot discover the field", p)
		}
	}
	for _, p := range fromSchema {
		if !inGenerator[p] {
			t.Errorf("the schema advertises --%s but the generator does not accept it, so an agent following the schema would be rejected", p)
		}
	}

	if len(fromSchema) == 0 {
		t.Fatal("the schema carries no x-flag values at all, so this test would pass against an empty document")
	}

	// The list published alongside the schema is what most callers will read, so
	// it has to agree with the document it accompanies.
	if strings.Join(got.FlagPaths, "\n") != strings.Join(fromSchema, "\n") {
		t.Error("the flag_paths in the result differ from the x-flag values in the schema it returned")
	}

	t.Logf("%d settable paths, identical in both walks", len(fromSchema))
}

// TestDescribeSchemaPerSectionIsValidAndScoped checks each section separately.
// A section schema that quietly returned the whole configuration would satisfy
// any test that only asked whether it parsed, so the flag paths are required to
// be prefixed with the section that was asked for.
func TestDescribeSchemaPerSectionIsValidAndScoped(t *testing.T) {
	srv := NewServer("test")

	sections := generator.ConfigSections()
	if len(sections) == 0 {
		t.Fatal("the generator reports no configuration sections")
	}

	for _, section := range sections {
		res := srv.tools["breeze_describe_schema"].run(mustJSON(t, map[string]any{
			"section": section,
		}))
		if res.IsError {
			t.Errorf("describe_schema(%q) failed: %s", section, res.Content[0].Text)
			continue
		}
		got := res.StructuredContent.(schemaResult)

		var doc map[string]any
		if err := json.Unmarshal(got.Schema, &doc); err != nil {
			t.Errorf("describe_schema(%q) returned unparseable JSON: %v", section, err)
			continue
		}
		if doc["$schema"] == nil {
			t.Errorf("describe_schema(%q) returned a document with no $schema dialect", section)
		}
		if doc["title"] != section {
			t.Errorf("describe_schema(%q) titled its document %v", section, doc["title"])
		}

		for _, path := range got.FlagPaths {
			if path != section && !strings.HasPrefix(path, section+".") {
				t.Errorf("describe_schema(%q) advertises --%s, which is outside the section that was asked for", section, path)
			}
		}
	}

	// An unknown section has to be refused, and the refusal has to name the
	// alternatives: a caller that guessed wrong needs the list more than it
	// needs the failure.
	res := srv.tools["breeze_describe_schema"].run(mustJSON(t, map[string]any{
		"section": "nonesuch",
	}))
	if !res.IsError {
		t.Fatal("describe_schema accepted an unknown section")
	}
	if !strings.Contains(res.Content[0].Text, sections[0]) {
		t.Errorf("the rejection does not list the sections that exist: %s", res.Content[0].Text)
	}
}

// ─── list_examples ───────────────────────────────────────────────────────────

// TestListExamplesAreAllValid runs the real validator over every shipped
// example. An example that no longer validates is worse than no example: it is a
// starting point that fails after the caller has committed to it.
func TestListExamplesAreAllValid(t *testing.T) {
	srv := NewServer("test")

	res := srv.tools["breeze_list_examples"].run(mustJSON(t, map[string]any{}))
	if res.IsError {
		t.Fatalf("list_examples failed: %s", res.Content[0].Text)
	}
	got := res.StructuredContent.(examplesResult)

	if got.Count == 0 {
		t.Fatal("list_examples returned nothing, so this test would pass against an empty list")
	}
	for _, ex := range got.Examples {
		if !ex.Valid {
			t.Errorf("example %q does not validate: %v", ex.Name, ex.Errors)
		}
		if len(ex.Sections) == 0 {
			t.Errorf("example %q reports no sections, so it cannot be filtered to", ex.Name)
		}
		if strings.TrimSpace(ex.YAML) == "" {
			t.Errorf("example %q carries no YAML", ex.Name)
		}
	}
	t.Logf("%d examples, all validated through the generator's own validator", got.Count)
}

// TestListExamplesFilterAndMiss checks that the filter selects rather than
// merely reorders, and that a filter matching nothing says so rather than
// returning an empty list that reads as "there are none".
func TestListExamplesFilterAndMiss(t *testing.T) {
	srv := NewServer("test")

	res := srv.tools["breeze_list_examples"].run(mustJSON(t, map[string]any{
		"section": "fleet",
	}))
	if res.IsError {
		t.Fatalf("list_examples(fleet) failed: %s", res.Content[0].Text)
	}
	got := res.StructuredContent.(examplesResult)

	if got.Count == 0 {
		t.Fatal("no example sets fleet, though several are meant to")
	}
	for _, ex := range got.Examples {
		if !containsString(ex.Sections, "fleet") {
			t.Errorf("example %q was returned for the fleet filter but does not set fleet", ex.Name)
		}
	}

	all := srv.tools["breeze_list_examples"].run(mustJSON(t, map[string]any{})).StructuredContent.(examplesResult)
	if got.Count >= all.Count {
		t.Errorf("the fleet filter returned %d of %d examples, so it is not filtering", got.Count, all.Count)
	}

	miss := srv.tools["breeze_list_examples"].run(mustJSON(t, map[string]any{
		"section": "not-a-section",
	})).StructuredContent.(examplesResult)
	if miss.Count != 0 {
		t.Errorf("a nonexistent section matched %d examples", miss.Count)
	}
	if miss.Note == "" {
		t.Error("an empty result carries no note, so a caller cannot tell it from 'there are none'")
	}
}

// ─── generate_llms_txt ───────────────────────────────────────────────────────

// TestGenerateLLMSDescribesTheProjectWithoutWriting checks both halves of the
// tool's contract: the content describes what the project really contains, and
// nothing is written unless asked.
func TestGenerateLLMSDescribesTheProjectWithoutWriting(t *testing.T) {
	srv := NewServer("test")
	project := newFixtureProject(t, "llmsproj", fixtureConfig)

	// A route is generated so the registry is not empty. Enabling docs does not
	// put anything in it: the docs feature block calls middlewares.Doc at the
	// project's own startup, and the spec and UI endpoints are registered then,
	// in that process. Only `generate handler` and `generate resource` write into
	// routes_generated.go, which is the file that can be read from here.
	generateResource(t, srv, project, "Widget", []string{"name:string"})

	before, err := snapshotTree(project)
	if err != nil {
		t.Fatalf("snapshotting the project: %v", err)
	}

	res := srv.tools["breeze_generate_llms_txt"].run(mustJSON(t, map[string]any{
		"path": project,
	}))
	if res.IsError {
		t.Fatalf("generate_llms_txt failed: %s", res.Content[0].Text)
	}
	got := res.StructuredContent.(llmsResult)

	after, err := snapshotTree(project)
	if err != nil {
		t.Fatalf("re-snapshotting the project: %v", err)
	}
	if changes := diffSnapshots(before, after); len(changes) != 0 {
		t.Fatalf("generate_llms_txt wrote to the project without being asked: %v", changedPaths(changes))
	}

	if len(got.Files) != 2 {
		t.Fatalf("expected llms.txt and llms-full.txt, got %d file(s)", len(got.Files))
	}
	byName := map[string]llmsFile{}
	for _, f := range got.Files {
		byName[f.Name] = f
		if f.Written {
			t.Errorf("%s reports written=true when write was not requested", f.Name)
		}
		if f.Digest == "" {
			t.Errorf("%s carries no digest, so its freshness can never be checked", f.Name)
		}
	}

	short := byName[llmsShortName].Content
	full := byName[llmsFullName].Content

	// The title comes from the configuration's docs.title, which the fixture
	// sets, so it is recoverable and must be used rather than the module name.
	if !strings.Contains(short, "# Scratch API") {
		t.Errorf("the index does not carry the configured docs title; it begins:\n%s", firstLines(short, 4))
	}
	if !strings.Contains(short, "> Go module: `example.com/scratch`") {
		t.Error("the index does not record the module path")
	}

	// The routes have to be the project's own, so they are compared against the
	// registry rather than against paths written here. Hardcoding "/widgets"
	// would test the generator's pluralisation, which is the generator's to
	// change; what this tool promises is that it reports what the registry says.
	var registry []generator.RouteEntry
	if err := runInDir(project, func() error {
		var err error
		registry, err = generator.ParseRoutes(generator.RegistryFileName)
		return err
	}); err != nil {
		t.Fatalf("reading the project's route registry: %v", err)
	}
	if len(registry) == 0 {
		t.Fatal("the fixture's registry has no routes, so the comparison below would be vacuous")
	}
	if got.Routes != len(registry) {
		t.Errorf("the document reports %d route(s) and the registry holds %d", got.Routes, len(registry))
	}
	for _, r := range registry {
		if !strings.Contains(full, r.Path) {
			t.Errorf("the full reference does not mention the %s route", r.Path)
		}
	}

	// The conventions section is what makes the document useful to a model that
	// has never seen the framework, and every statically checked rule should be
	// named in it.
	if !strings.Contains(full, "## Conventions") {
		t.Error("the full reference has no conventions section")
	}
	for _, rule := range idiomRules() {
		if !strings.Contains(full, rule) {
			t.Errorf("the conventions section does not name the %s rule", rule)
		}
	}

	// The stamp has to describe the document it is in, or a freshness check has
	// nothing to compare against.
	for name, f := range byName {
		recorded, ok := scalar.ReadStamp(f.Content)
		if !ok {
			t.Errorf("%s carries no stamp", name)
			continue
		}
		if recorded != f.Digest {
			t.Errorf("%s records digest %s but the result reports %s", name, recorded, f.Digest)
		}
	}
}

// TestGenerateLLMSWritesWhenAsked is the other half: write=true really writes,
// and what lands on disk is what was returned.
func TestGenerateLLMSWritesWhenAsked(t *testing.T) {
	srv := NewServer("test")
	project := newFixtureProject(t, "llmswrite", fixtureConfig)

	res := srv.tools["breeze_generate_llms_txt"].run(mustJSON(t, map[string]any{
		"path":  project,
		"write": true,
	}))
	if res.IsError {
		t.Fatalf("generate_llms_txt failed: %s", res.Content[0].Text)
	}
	got := res.StructuredContent.(llmsResult)

	for _, f := range got.Files {
		if !f.Written {
			t.Errorf("%s reports written=false after write=true; notes: %v", f.Name, got.Notes)
			continue
		}
		raw, err := os.ReadFile(filepath.Join(project, f.Name))
		if err != nil {
			t.Errorf("%s was reported written but cannot be read: %v", f.Name, err)
			continue
		}
		if string(raw) != f.Content {
			t.Errorf("%s on disk differs from the content that was returned", f.Name)
		}
	}
}

// TestLLMSDocumentsModelsFromTheSourceTree is the regression test for the defect
// this file's models handling exists to fix.
//
// A model is a whole generated file rather than a marker block, so a
// configuration rebuilt from the blocks contains no models at all. Reading them
// from there would produce a document that reports zero models for every project
// ever generated, and no test that only counted routes would notice. So a model
// is generated into a real project and the document has to describe its fields,
// its table and its relationship.
func TestLLMSDocumentsModelsFromTheSourceTree(t *testing.T) {
	srv := NewServer("test")
	project := newFixtureProject(t, "llmsmodels", fixtureConfig)

	generateModel(t, srv, project, "Customer", []string{"email:string"})
	generateModel(t, srv, project, "Order", []string{"total:float64"})

	// The relationship is added by editing, because `breeze generate model`
	// cannot express one: parseFieldsNoRules accepts only the types in
	// supportedFieldTypes, so a `customer:Customer` argument is rejected before
	// any file is written. A field pointing at another model is therefore always
	// hand-added, and that is the state the parser has to cope with.
	addRelationshipField(t, project, "Order", "Customer *Customer `json:\"customer\" db:\"customer_id\"`")

	res := srv.tools["breeze_generate_llms_txt"].run(mustJSON(t, map[string]any{
		"path": project,
	}))
	if res.IsError {
		t.Fatalf("generate_llms_txt failed: %s", res.Content[0].Text)
	}
	got := res.StructuredContent.(llmsResult)

	if got.Models != 2 {
		t.Fatalf("the document reports %d model(s) for a project with two; a configuration rebuilt from feature blocks would report 0", got.Models)
	}

	full := contentOf(t, got, llmsFullName)

	for _, want := range []string{
		"### `Customer`",
		"Table: `customers`",
		"### `Order`",
		"Table: `orders`",
		"- `Total` float64",
	} {
		if !strings.Contains(full, want) {
			t.Errorf("the full reference does not contain %q", want)
		}
	}

	// The primary key is read from the db tag the generator emits, not assumed.
	// The column is named too, because the generator's `db:"id"` differs from the
	// field name and writeModel reports a column whenever the two differ.
	if !strings.Contains(full, "- `ID` int64, column `id`, primary key") {
		t.Errorf("the full reference does not mark ID as the primary key; the Customer section reads:\n%s", sectionOf(full, "### `Customer`"))
	}
	// And the relationship is the point of the exercise. It has to be recognised
	// through a pointer, and recognised by the generator's own ModelRefs rather
	// than by re-deriving "the type starts with a capital" here.
	if !strings.Contains(full, "references `Customer`") {
		t.Errorf("the full reference does not record that Order references Customer; its Order section reads:\n%s", sectionOf(full, "### `Order`"))
	}
	// The column comes from the db tag on the hand-added field, which proves the
	// tag is read rather than the name being reused.
	if !strings.Contains(full, "column `customer_id`") {
		t.Errorf("the relationship field's column was not read from its db tag; the Order section reads:\n%s", sectionOf(full, "### `Order`"))
	}
}

// ─── check_llms_txt_freshness ────────────────────────────────────────────────

// TestLLMSFreshnessDistinguishesMissingCurrentStaleAndUnstamped walks the whole
// state machine on one project, in the order the states really occur.
//
// Staleness is provoked by generating a model rather than by editing a file,
// because that is the case that matters: the document is untouched and correct
// for the project it was written against, and the project has moved. A checker
// that only compared a file against itself would call that current.
func TestLLMSFreshnessDistinguishesMissingCurrentStaleAndUnstamped(t *testing.T) {
	srv := NewServer("test")
	project := newFixtureProject(t, "freshproj", fixtureConfig)

	// ── missing ──
	first := freshness(t, srv, project)
	if first.Fresh {
		t.Error("a project with no llms.txt is reported as fresh")
	}
	for _, f := range first.Files {
		if f.Status != "missing" {
			t.Errorf("%s is %q before anything was generated, expected missing", f.Name, f.Status)
		}
	}

	// ── current ──
	if res := srv.tools["breeze_generate_llms_txt"].run(mustJSON(t, map[string]any{
		"path": project, "write": true,
	})); res.IsError {
		t.Fatalf("generate_llms_txt failed: %s", res.Content[0].Text)
	}

	second := freshness(t, srv, project)
	if !second.Fresh {
		for _, f := range second.Files {
			t.Errorf("%s is %q immediately after generation: %s", f.Name, f.Status, f.Reason)
		}
		t.Fatal("freshly generated files are not reported as current")
	}
	for _, f := range second.Files {
		if f.RecordedDigest != f.ExpectedDigest {
			t.Errorf("%s is current but its digests differ (%s vs %s)", f.Name, f.RecordedDigest, f.ExpectedDigest)
		}
	}

	// ── stale, because the project changed ──
	generateModel(t, srv, project, "Invoice", []string{"amount:float64"})

	third := freshness(t, srv, project)
	if third.Fresh {
		t.Fatal("a project that gained a model since generation is still reported as fresh")
	}
	for _, f := range third.Files {
		if f.Status != "stale" {
			t.Errorf("%s is %q after the project changed, expected stale", f.Name, f.Status)
		}
		if f.RecordedDigest == f.ExpectedDigest {
			t.Errorf("%s is stale but its recorded and expected digests are identical", f.Name)
		}
	}
	if third.Regen == "" {
		t.Error("a stale result does not say how to regenerate")
	}

	// ── unstamped, which is a different fault from stale ──
	if err := os.WriteFile(filepath.Join(project, llmsShortName),
		[]byte("# Written by hand\n\nNot generated by anything.\n"), 0o644); err != nil {
		t.Fatalf("writing an unstamped file: %v", err)
	}

	fourth := freshness(t, srv, project)
	for _, f := range fourth.Files {
		if f.Name != llmsShortName {
			continue
		}
		if f.Status != "unstamped" {
			t.Errorf("a hand-written file is reported %q rather than unstamped, so regenerating it would silently discard the work", f.Status)
		}
	}
}

// ─── search_llms_txt ─────────────────────────────────────────────────────────

// TestSearchLLMSFindsRoutesModelsAndRules checks that a search returns the
// heading a match sits under. An excerpt without its heading is not
// interpretable — "response 200 OK" means nothing without the endpoint above it.
func TestSearchLLMSFindsRoutesModelsAndRules(t *testing.T) {
	srv := NewServer("test")
	project := newFixtureProject(t, "searchproj", fixtureConfig)
	generateModel(t, srv, project, "Ticket", []string{"subject:string"})

	// Searched before the file exists, so the fallback path is exercised too.
	res := srv.tools["breeze_search_llms_txt"].run(mustJSON(t, map[string]any{
		"path":  project,
		"query": "Ticket",
	}))
	if res.IsError {
		t.Fatalf("search_llms_txt failed: %s", res.Content[0].Text)
	}
	got := res.StructuredContent.(searchResult)

	if got.Count == 0 {
		t.Fatal("searching for a model that exists found nothing")
	}
	if !strings.Contains(got.Source, "freshly built") {
		t.Errorf("with no llms-full.txt on disk the source should say so; it said %q", got.Source)
	}
	if len(got.Notes) == 0 {
		t.Error("the fallback path does not suggest generating the file")
	}

	foundHeading := false
	for _, m := range got.Matches {
		if m.Line <= 0 {
			t.Errorf("match %q reports line %d", m.Excerpt, m.Line)
		}
		if m.Heading != "" {
			foundHeading = true
		}
	}
	if !foundHeading {
		t.Error("no match carries a heading, so the excerpts cannot be placed")
	}

	// Once written, the search must read the file the repository ships rather
	// than rebuilding, because that is the document a reader would see.
	if res := srv.tools["breeze_generate_llms_txt"].run(mustJSON(t, map[string]any{
		"path": project, "write": true,
	})); res.IsError {
		t.Fatalf("generate_llms_txt failed: %s", res.Content[0].Text)
	}

	onDisk := srv.tools["breeze_search_llms_txt"].run(mustJSON(t, map[string]any{
		"path":  project,
		"query": "fleet-before-dashboard",
	}))
	if onDisk.IsError {
		t.Fatalf("search_llms_txt failed: %s", onDisk.Content[0].Text)
	}
	second := onDisk.StructuredContent.(searchResult)
	if second.Source != llmsFullName {
		t.Errorf("with the file present the source should be %s, not %q", llmsFullName, second.Source)
	}
	if second.Count == 0 {
		t.Error("searching for a convention rule found nothing")
	}

	// A limit has to be reported as a limit, or a caller concludes it has seen
	// everything.
	limited := srv.tools["breeze_search_llms_txt"].run(mustJSON(t, map[string]any{
		"path":  project,
		"query": "e",
		"limit": 1,
	})).StructuredContent.(searchResult)
	if len(limited.Matches) != 1 {
		t.Errorf("limit=1 returned %d matches", len(limited.Matches))
	}
	if !limited.Truncated {
		t.Error("a truncated search does not report truncated=true")
	}
	if limited.Count <= 1 {
		t.Error("the total count was capped by the limit, so a caller cannot tell how much was hidden")
	}

	// A query is required: searching for nothing is a mistake, not a wildcard.
	if empty := srv.tools["breeze_search_llms_txt"].run(mustJSON(t, map[string]any{
		"path": project,
	})); !empty.IsError {
		t.Error("search_llms_txt accepted an empty query")
	}
}

// ─── suggest_next_steps ──────────────────────────────────────────────────────

// TestSuggestNextStepsFindsAModelWithNoRoutes is the second regression test for
// the models defect. This rule reads models from the source tree; had it read
// them from the reconstructed configuration it would never fire, and the tool
// would report "nothing to suggest" for a project with an unreachable model.
func TestSuggestNextStepsFindsAModelWithNoRoutes(t *testing.T) {
	srv := NewServer("test")
	project := newFixtureProject(t, "advisemodel", fixtureConfig)

	// A model on its own: `generate model` writes the struct and the migration
	// and registers no routes, so nothing can reach it.
	generateModel(t, srv, project, "Warehouse", []string{"name:string"})

	got := suggestions(t, srv, project)

	if !got.Generated {
		t.Fatal("a scaffolded project is not recognised as generated")
	}
	if len(got.Checked) == 0 {
		t.Error("the result does not say which rules ran, so an empty list is unreadable")
	}

	found := findSuggestion(got, "model-without-routes", "Warehouse")
	if found == nil {
		t.Fatalf("no model-without-routes finding for Warehouse; findings were %v", suggestionKinds(got))
	}
	if found.Rationale == "" {
		t.Error("the finding carries no rationale, so it is an instruction rather than advice")
	}
	if !strings.Contains(found.Action, "Warehouse") {
		t.Errorf("the action does not name the model: %q", found.Action)
	}

	// The heuristic has to stop firing once a route serves the model, or it is
	// noise that trains a reader to ignore it.
	generateResource(t, srv, project, "Warehouse", []string{"name:string"})

	after := suggestions(t, srv, project)
	if still := findSuggestion(after, "model-without-routes", "Warehouse"); still != nil {
		t.Error("Warehouse is still reported as having no routes after a resource was generated for it")
	}
}

// TestSuggestNextStepsFindsWebSocketWithoutAuth uses the fixture configuration,
// which enables WebSocket and no JWT.
func TestSuggestNextStepsFindsWebSocketWithoutAuth(t *testing.T) {
	srv := NewServer("test")
	project := newFixtureProject(t, "advisews", fixtureConfig)

	got := suggestions(t, srv, project)

	if findSuggestion(got, "websocket-without-auth", "websocket") == nil {
		t.Errorf("no websocket-without-auth finding for a project with WebSocket and no JWT; findings were %v", suggestionKinds(got))
	}

	// llms.txt has not been generated for this project, so that gap should be
	// reported too — it is the finding that makes the others discoverable.
	if findSuggestion(got, "llms-missing", llmsShortName) == nil {
		t.Errorf("no llms-missing finding for a project without one; findings were %v", suggestionKinds(got))
	}

	after := srv.tools["breeze_add"].run(mustJSON(t, map[string]any{
		"feature": "jwt",
		"dir":     project,
	}))
	if after.IsError {
		t.Fatalf("adding jwt failed: %s", after.Content[0].Text)
	}

	if still := findSuggestion(suggestions(t, srv, project), "websocket-without-auth", "websocket"); still != nil {
		t.Error("the WebSocket finding survives adding jwt")
	}
}

// TestSuggestNextStepsOnANonProject checks the case that would otherwise produce
// confident nonsense: an empty directory is not a project with no problems.
func TestSuggestNextStepsOnANonProject(t *testing.T) {
	srv := NewServer("test")

	got := suggestions(t, srv, t.TempDir())

	if got.Generated {
		t.Error("an empty directory is reported as a generated project")
	}
	if got.Count != 0 {
		t.Errorf("an empty directory produced %d suggestion(s)", got.Count)
	}
	if len(got.Notes) == 0 {
		t.Error("nothing explains why there is no advice")
	}
}

// ─── explain_idiom ───────────────────────────────────────────────────────────

// TestExplainIdiomListsAndResolves checks the list, the lookups, and the field
// that decides whether a clean check report is evidence.
func TestExplainIdiomListsAndResolves(t *testing.T) {
	srv := NewServer("test")

	res := srv.tools["breeze_explain_idiom"].run(mustJSON(t, map[string]any{}))
	if res.IsError {
		t.Fatalf("explain_idiom failed: %s", res.Content[0].Text)
	}
	list := res.StructuredContent.(idiomListResult)

	if list.Count == 0 {
		t.Fatal("no conventions are documented")
	}
	if list.Count != len(list.Rules) {
		t.Errorf("the count says %d and the list has %d", list.Count, len(list.Rules))
	}

	checked := 0
	for _, r := range list.Rules {
		if r.Rule == "" || r.Topic == "" {
			t.Errorf("a convention is missing its rule name or topic: %+v", r)
		}
		// Every one of these fields is load-bearing. Without Why a rule is an
		// order; without Do a reader knows only what not to write.
		if r.Why == "" {
			t.Errorf("%s does not say why it exists", r.Rule)
		}
		if r.Do == "" {
			t.Errorf("%s does not say what to do instead", r.Rule)
		}
		if r.Dont == "" {
			t.Errorf("%s does not say what it forbids", r.Rule)
		}
		if r.Severity != severityError && r.Severity != severityWarning {
			t.Errorf("%s has severity %q, which is neither %q nor %q", r.Rule, r.Severity, severityError, severityWarning)
		}
		if r.StaticallyChecked {
			checked++
		}
	}
	if checked == 0 {
		t.Error("no convention is marked as statically checked, so check_idioms could never be evidence for any of them")
	}

	// Lookup by exact rule name.
	one := srv.tools["breeze_explain_idiom"].run(mustJSON(t, map[string]any{
		"topic": "fleet-before-dashboard",
	}))
	if one.IsError {
		t.Fatalf("explain_idiom(fleet-before-dashboard) failed: %s", one.Content[0].Text)
	}
	if got := one.StructuredContent.(idiomExplanation); got.Rule != "fleet-before-dashboard" {
		t.Errorf("looking up a rule by name returned %q", got.Rule)
	}

	// And by a phrase, which is how an agent that has not seen the rule names
	// will actually ask.
	phrase := srv.tools["breeze_explain_idiom"].run(mustJSON(t, map[string]any{
		"topic": "reflection",
	}))
	if phrase.IsError {
		t.Errorf("a phrase lookup failed: %s", phrase.Content[0].Text)
	}

	// An unknown topic is refused, with the alternatives.
	miss := srv.tools["breeze_explain_idiom"].run(mustJSON(t, map[string]any{
		"topic": "nonesuch-convention",
	}))
	if !miss.IsError {
		t.Fatal("explain_idiom accepted an unknown topic")
	}
	if !strings.Contains(miss.Content[0].Text, list.Rules[0].Rule) {
		t.Errorf("the rejection does not list the rules that exist: %s", miss.Content[0].Text)
	}
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// generateModel runs the real model generator into a project.
//
// The generator is used rather than a hand-written file because the parser under
// test is meant to read what `breeze generate model` emits — struct tags, table
// constant and all. A fixture written here would only prove it can read this
// file.
func generateModel(t *testing.T, srv *Server, project, name string, fields []string) {
	t.Helper()

	res := srv.tools["breeze_generate"].run(mustJSON(t, map[string]any{
		"kind":   "model",
		"name":   name,
		"fields": fields,
		"dir":    project,
	}))
	if res.IsError {
		t.Fatalf("generating model %s: %s", name, res.Content[0].Text)
	}
}

// generateResource runs the real resource generator, which is what puts routes
// into routes_generated.go.
//
// Fields are required rather than optional: `generate resource` refuses a bare
// name, because a resource with no fields would produce handlers that bind an
// empty struct.
func generateResource(t *testing.T, srv *Server, project, name string, fields []string) {
	t.Helper()

	res := srv.tools["breeze_generate"].run(mustJSON(t, map[string]any{
		"kind":   "resource",
		"name":   name,
		"fields": fields,
		"dir":    project,
	}))
	if res.IsError {
		t.Fatalf("generating resource %s: %s", name, res.Content[0].Text)
	}
}

// addRelationshipField adds a field pointing at another model to an already
// generated model file.
//
// Editing is the only option: the CLI cannot express a relationship, so a
// generated project that has one got it by hand. Only the one field is written
// here — the struct, its tags, its table constant and its migration are all the
// generator's own output, so what the parser is being asked to read is still
// generated code.
//
// The file is located by the declaration it contains rather than by reproducing
// the generator's file-slugging rule, which is the generator's own to change.
func addRelationshipField(t *testing.T, project, model, field string) {
	t.Helper()

	dir := filepath.Join(project, "models")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading the models directory: %v", err)
	}

	opening := "type " + model + " struct {"
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}

		path := filepath.Join(dir, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", entry.Name(), err)
		}

		source := string(raw)
		idx := strings.Index(source, opening)
		if idx < 0 {
			continue
		}

		cut := idx + len(opening)
		if err := os.WriteFile(path, []byte(source[:cut]+"\n\t"+field+source[cut:]), 0o644); err != nil {
			t.Fatalf("writing %s: %v", entry.Name(), err)
		}
		return
	}

	t.Fatalf("no file under models/ declares %s, so the fixture could not be built", model)
}

func freshness(t *testing.T, srv *Server, project string) freshnessResult {
	t.Helper()

	res := srv.tools["breeze_check_llms_txt_freshness"].run(mustJSON(t, map[string]any{
		"path": project,
	}))
	if res.IsError {
		t.Fatalf("check_llms_txt_freshness failed: %s", res.Content[0].Text)
	}
	got, ok := res.StructuredContent.(freshnessResult)
	if !ok {
		t.Fatalf("check_llms_txt_freshness returned %T", res.StructuredContent)
	}
	if len(got.Files) != 2 {
		t.Fatalf("the checker reported on %d file(s), expected 2", len(got.Files))
	}
	return got
}

func suggestions(t *testing.T, srv *Server, project string) suggestionsResult {
	t.Helper()

	res := srv.tools["breeze_suggest_next_steps"].run(mustJSON(t, map[string]any{
		"path": project,
	}))
	if res.IsError {
		t.Fatalf("suggest_next_steps failed: %s", res.Content[0].Text)
	}
	got, ok := res.StructuredContent.(suggestionsResult)
	if !ok {
		t.Fatalf("suggest_next_steps returned %T", res.StructuredContent)
	}
	return got
}

// findSuggestion returns the finding of a kind about a subject, or nil.
func findSuggestion(result suggestionsResult, kind, subject string) *suggestion {
	for i := range result.Suggestions {
		if result.Suggestions[i].Kind == kind && result.Suggestions[i].Subject == subject {
			return &result.Suggestions[i]
		}
	}
	return nil
}

// suggestionKinds is for failure messages: a test that says "the finding is
// missing" is much easier to act on when it also says what was found.
func suggestionKinds(result suggestionsResult) []string {
	out := make([]string, 0, len(result.Suggestions))
	for _, s := range result.Suggestions {
		out = append(out, s.Kind+"/"+s.Subject)
	}
	return out
}

func contentOf(t *testing.T, result llmsResult, name string) string {
	t.Helper()

	for _, f := range result.Files {
		if f.Name == name {
			return f.Content
		}
	}
	t.Fatalf("the result contains no %s", name)
	return ""
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// sectionOf returns the text from a heading to the next one, for failure
// messages that need to show where something should have been.
func sectionOf(document, heading string) string {
	idx := strings.Index(document, heading)
	if idx < 0 {
		return "(the heading is not present at all)"
	}
	rest := document[idx+len(heading):]
	if next := strings.Index(rest, "\n### "); next >= 0 {
		rest = rest[:next]
	}
	return heading + rest
}
