package mcp

// tools_plan_test.go — the tests that hold the planning tools to their promises.
//
// Two of those promises are the reason the tools are worth having, and both are
// about what does *not* happen: plan_project reports what a scaffold would
// produce without producing it, and a change set whose sequence fails leaves the
// project byte-for-byte as it was. Neither can be checked by reading a result —
// a tool that wrote files and then described them accurately would pass that
// test. So both are checked against the filesystem, by snapshotting the tree and
// comparing it afterwards.
//
// The fixtures are real generated projects. Building one is slower than writing
// a features_generated.go by hand, and it is the only version of this test worth
// running: the thing being verified is that explain_project can read what the
// generators actually emit, and a hand-written fixture would only prove it can
// read what this file emits.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nelthaarion/breeze/internal/generator"
)

// fixtureConfig exercises every value explain_project claims to recover: a
// non-default WebSocket path, the ws transport (whose block calls a different
// constructor), a fractional sample rate, a JSON-RPC port with one blocking and
// one non-blocking method, retitled docs on a non-default UI path, a CORS
// allow-list of more than one origin, and a rate limit.
const fixtureConfig = `module: example.com/scratch
websocket:
  enabled: true
  path: /live
fleet:
  enabled: true
  service_name: scratch-api
  transport: ws
  sample_rate: 0.25
  aggregator_url: http://localhost:9000/fleet
  aggregator_ws_url: ws://localhost:9000/fleet/ws
jsonrpc:
  enabled: true
  port: 9391
  methods: [ping, sum]
  blocking_methods: [sum]
docs:
  enabled: true
  title: Scratch API
  ui_path: /apidocs
  spec_path: /openapi.json
middleware:
  - name: cors
    origins: [https://a.example, https://b.example]
  - name: rate-limit
    rps: 42
`

// newFixtureProject scaffolds a real project from a configuration and returns
// its root.
//
// GOPROXY is switched off for the same reason the sandboxes switch it off: the
// scaffold ends with `go mod tidy`, which resolves breeze over the network,
// takes seconds, and has nothing to do with what is being tested. runNew already
// treats a tidy failure as a warning.
func newFixtureProject(t *testing.T, name, configYAML string) string {
	t.Helper()

	t.Setenv("GOPROXY", "off")
	t.Setenv("GOFLAGS", "-mod=mod")

	// The config file lives outside the parent directory, so it cannot be
	// mistaken for something the scaffold produced.
	configPath := filepath.Join(t.TempDir(), "breeze.yaml")
	if err := os.WriteFile(configPath, []byte(configYAML), 0o644); err != nil {
		t.Fatalf("writing the fixture configuration: %v", err)
	}

	parent := t.TempDir()
	argv := []string{name, "--template=api", "--config=" + configPath}

	// runInDir is used without captureStdout deliberately. captureStdout takes a
	// process-wide, non-reentrant lock, and everything under test here captures
	// internally; wrapping a capture around a capture waits for a lock the same
	// goroutine already holds, which hangs the run rather than failing it. The
	// generator's progress lines land in the test log instead, where they belong.
	if err := runInDir(parent, func() error { return generator.New(argv) }); err != nil {
		t.Fatalf("scaffolding the fixture project: %v", err)
	}
	return filepath.Join(parent, name)
}

// ─── plan_project ────────────────────────────────────────────────────────────

// TestPlanProjectWritesNothing is the filesystem assertion, not a reading of the
// result. The working directory is snapshotted before and after, and any
// difference at all fails: a preview that leaves a stray temporary file in the
// project is not a preview.
func TestPlanProjectWritesNothing(t *testing.T) {
	srv := NewServer("test")

	workspace := t.TempDir()
	before, err := snapshotTree(workspace)
	if err != nil {
		t.Fatalf("snapshotting the workspace: %v", err)
	}

	// The tool captures stdout itself, so this must not capture around it: the
	// lock is not reentrant and the call would block forever on a lock this
	// goroutine already holds.
	var res toolCallResult
	if err := runInDir(workspace, func() error {
		res = srv.tools["breeze_plan_project"].run(mustJSON(t, map[string]any{
			"name":        "planned",
			"config_yaml": fixtureConfig,
		}))
		return nil
	}); err != nil {
		t.Fatalf("running plan_project: %v", err)
	}

	if res.IsError {
		t.Fatalf("plan_project failed: %s", res.Content[0].Text)
	}

	after, err := snapshotTree(workspace)
	if err != nil {
		t.Fatalf("re-snapshotting the workspace: %v", err)
	}
	if changes := diffSnapshots(before, after); len(changes) != 0 {
		t.Fatalf("plan_project wrote to the filesystem: %v", changedPaths(changes))
	}

	plan, ok := res.StructuredContent.(planResult)
	if !ok {
		t.Fatalf("plan_project returned %T, not a planResult", res.StructuredContent)
	}
	if !plan.Valid {
		t.Fatalf("the fixture configuration should validate; errors: %v", plan.Errors)
	}
	if plan.WroteToDisk {
		t.Error("plan_project reported wrote_to_disk=true")
	}

	// The plan has to name the files a scaffold really produces, or it is not
	// telling the caller anything it can act on.
	planned := map[string]bool{}
	for _, f := range plan.Files {
		planned[f.Path] = true
	}
	for _, want := range []string{"main.go", "go.mod", generator.FeaturesFileName, "rpc_methods.go"} {
		if !planned[want] {
			t.Errorf("the plan does not mention %s; it listed %v", want, changedPathsOf(plan.Files))
		}
	}

	// And the features it reports must be the ones the configuration enables,
	// in the order they would be applied.
	if len(plan.Features) == 0 {
		t.Error("the plan reports no features for a configuration that enables six")
	}
}

// TestPlanProjectRejectsInvalidConfigWithoutScaffolding checks that validation
// comes first: an invalid configuration should be reported as invalid, not
// half-generated and then reported.
func TestPlanProjectRejectsInvalidConfigWithoutScaffolding(t *testing.T) {
	srv := NewServer("test")

	// gnet is a transport the specification names and the fleet package does not
	// implement, so it validates as a known value and is refused as ungeneratable.
	res := srv.tools["breeze_plan_project"].run(mustJSON(t, map[string]any{
		"config_yaml": "fleet:\n  enabled: true\n  transport: gnet\n",
	}))

	if !res.IsError {
		t.Fatal("a configuration naming an unimplemented transport was accepted")
	}
	plan, ok := res.StructuredContent.(planResult)
	if !ok {
		t.Fatalf("plan_project returned %T, not a planResult", res.StructuredContent)
	}
	if plan.Valid {
		t.Error("the result says valid=true for a configuration that was refused")
	}
	if len(plan.Errors) == 0 {
		t.Error("the result carries no errors explaining the refusal")
	}
	if len(plan.Files) != 0 {
		t.Errorf("an invalid configuration still produced a file plan: %v", changedPathsOf(plan.Files))
	}
}

// ─── explain_project ─────────────────────────────────────────────────────────

// TestExplainProjectRecoversTheConfiguration scaffolds a project from a known
// configuration and reads it back, which is the only way to find out whether the
// patterns in tools_plan.go match what the generators emit after gofmt has
// rearranged it.
func TestExplainProjectRecoversTheConfiguration(t *testing.T) {
	project := newFixtureProject(t, "scratch", fixtureConfig)
	srv := NewServer("test")

	res := srv.tools["breeze_explain_project"].run(mustJSON(t, map[string]any{"path": project}))
	if res.IsError {
		t.Fatalf("explain_project failed: %s", res.Content[0].Text)
	}

	facts, ok := res.StructuredContent.(projectFacts)
	if !ok {
		t.Fatalf("explain_project returned %T, not projectFacts", res.StructuredContent)
	}
	if !facts.Generated {
		t.Fatal("a scaffolded project was not recognised as generated")
	}
	if facts.Module != "example.com/scratch" {
		t.Errorf("module = %q, want example.com/scratch", facts.Module)
	}

	// Every feature the configuration asked for should be installed and
	// pristine: nothing has edited them since they were written.
	installed := map[string]featureState{}
	for _, f := range facts.Features {
		installed[f.Name] = f
	}
	for _, want := range []string{"fleet", "cors", "ratelimit", "docs", "websocket", "jsonrpc"} {
		state, ok := installed[want]
		if !ok {
			t.Errorf("feature %s is missing from the report", want)
			continue
		}
		if !state.Pristine {
			t.Errorf("feature %s reads as edited in a freshly generated project", want)
		}
	}
	if len(facts.Edited) != 0 {
		t.Errorf("a freshly generated project reports edited blocks: %v", facts.Edited)
	}

	// The values, read back out of the emitted Go source.
	cfg := facts.Config
	assertConfig(t, cfg, "fleet.enabled", true)
	assertConfig(t, cfg, "fleet.service_name", "scratch-api")
	assertConfig(t, cfg, "fleet.transport", "ws")
	assertConfig(t, cfg, "fleet.sample_rate", 0.25)
	assertConfig(t, cfg, "fleet.aggregator_ws_url", "ws://localhost:9000/fleet/ws")
	assertConfig(t, cfg, "websocket.enabled", true)
	assertConfig(t, cfg, "websocket.path", "/live")
	assertConfig(t, cfg, "jsonrpc.enabled", true)
	assertConfig(t, cfg, "jsonrpc.port", 9391)
	assertConfig(t, cfg, "docs.title", "Scratch API")
	assertConfig(t, cfg, "docs.ui_path", "/apidocs")
}

// TestExplainProjectReportsAnEditedBlock covers the fact the tool exists to
// report: a block someone has changed by hand cannot be regenerated, and the
// checksum in its marker is how that is known.
func TestExplainProjectReportsAnEditedBlock(t *testing.T) {
	project := newFixtureProject(t, "scratch", "docs:\n  enabled: true\n  title: Original\n")
	srv := NewServer("test")

	featuresPath := filepath.Join(project, generator.FeaturesFileName)
	source, err := os.ReadFile(featuresPath)
	if err != nil {
		t.Fatalf("reading the features file: %v", err)
	}

	// An edit inside the docs block, leaving its recorded checksum in place —
	// which is exactly what a developer changing a generated value looks like.
	//
	// Only the quoted value is matched, never the `Title:` key and its padding:
	// upsertBlock runs the whole file through format.Source, so gofmt re-aligns
	// the struct literal's columns to its longest key. Matching the emitted
	// spacing would tie this test to that alignment and break the moment a
	// longer field joins the literal.
	edited := replaceFirst(t, string(source), `"Original"`, `"Edited By Hand"`)

	if err := os.WriteFile(featuresPath, []byte(edited), 0o644); err != nil {
		t.Fatalf("writing the edited features file: %v", err)
	}

	res := srv.tools["breeze_explain_project"].run(mustJSON(t, map[string]any{"path": project}))
	if res.IsError {
		t.Fatalf("explain_project failed: %s", res.Content[0].Text)
	}
	facts := res.StructuredContent.(projectFacts)

	if len(facts.Edited) == 0 {
		t.Fatal("an edited block was not reported as edited")
	}
	if facts.Edited[0] != "docs" {
		t.Errorf("edited = %v, want [docs]", facts.Edited)
	}
	// The edited value is what the project now says, so that is what must be
	// reported: reading back the original would be reporting the config file
	// rather than the project.
	assertConfig(t, facts.Config, "docs.title", "Edited By Hand")
}

// ─── change sets ─────────────────────────────────────────────────────────────

// TestChangeSetInvalidSequenceWritesZeroFiles is the atomicity assertion. A
// sequence whose second call is refused must leave the project untouched — not
// carrying the first call's files, which is what would happen without staging.
func TestChangeSetInvalidSequenceWritesZeroFiles(t *testing.T) {
	project := newFixtureProject(t, "scratch", "docs:\n  enabled: true\n")
	srv := NewServer("test")

	before, err := snapshotTree(project)
	if err != nil {
		t.Fatalf("snapshotting the project: %v", err)
	}

	open := srv.tools["breeze_begin_change_set"].run(mustJSON(t, map[string]any{"project_path": project}))
	if open.IsError {
		t.Fatalf("begin_change_set failed: %s", open.Content[0].Text)
	}
	id := open.StructuredContent.(changeSetView).ID

	// A call that works. Its files land in the sandbox, not the project.
	staged := srv.tools["breeze_stage_call"].run(mustJSON(t, map[string]any{
		"change_set": id,
		"tool":       "breeze_generate",
		"arguments":  map[string]any{"kind": "model", "name": "User", "fields": []string{"name:string"}},
	}))
	if staged.IsError {
		t.Fatalf("staging a valid call failed: %s", staged.Content[0].Text)
	}
	if pending := staged.StructuredContent.(stageResult).Pending; len(pending) == 0 {
		t.Error("a staged model generated no pending files")
	}

	// A call that cannot work. The change set stays open and keeps the first
	// call, but nothing has reached the project.
	refused := srv.tools["breeze_stage_call"].run(mustJSON(t, map[string]any{
		"change_set": id,
		"tool":       "breeze_add",
		"arguments":  map[string]any{"feature": "not-a-real-feature"},
	}))
	if !refused.IsError {
		t.Fatal("staging an unknown feature was accepted")
	}

	during, err := snapshotTree(project)
	if err != nil {
		t.Fatalf("re-snapshotting the project: %v", err)
	}
	if changes := diffSnapshots(before, during); len(changes) != 0 {
		t.Fatalf("staged calls reached the project before commit: %v", changedPaths(changes))
	}

	// Discarding must also write nothing, and must say what it dropped.
	discarded := srv.tools["breeze_discard_change_set"].run(mustJSON(t, map[string]any{"change_set": id}))
	if discarded.IsError {
		t.Fatalf("discard_change_set failed: %s", discarded.Content[0].Text)
	}
	if result := discarded.StructuredContent.(discardResult); len(result.Discarded) == 0 {
		t.Error("discard reported nothing dropped, though a model was staged")
	}

	after, err := snapshotTree(project)
	if err != nil {
		t.Fatalf("snapshotting the project after discard: %v", err)
	}
	if changes := diffSnapshots(before, after); len(changes) != 0 {
		t.Fatalf("a discarded change set modified the project: %v", changedPaths(changes))
	}

	// And the id is gone: a discarded set cannot then be committed.
	if again := srv.tools["breeze_commit_change_set"].run(mustJSON(t, map[string]any{"change_set": id})); !again.IsError {
		t.Error("a discarded change set was committable")
	}
}

// TestChangeSetCommitAppliesAndRecordsHistory is the other half: a sequence that
// works lands in one operation and is recorded where a later question can find
// it.
func TestChangeSetCommitAppliesAndRecordsHistory(t *testing.T) {
	project := newFixtureProject(t, "scratch", "docs:\n  enabled: true\n")
	srv := NewServer("test")

	open := srv.tools["breeze_begin_change_set"].run(mustJSON(t, map[string]any{"project_path": project}))
	if open.IsError {
		t.Fatalf("begin_change_set failed: %s", open.Content[0].Text)
	}
	id := open.StructuredContent.(changeSetView).ID

	staged := srv.tools["breeze_stage_call"].run(mustJSON(t, map[string]any{
		"change_set": id,
		"tool":       "breeze_generate",
		"arguments":  map[string]any{"kind": "model", "name": "Invoice", "fields": []string{"total:int"}},
	}))
	if staged.IsError {
		t.Fatalf("staging failed: %s", staged.Content[0].Text)
	}

	committed := srv.tools["breeze_commit_change_set"].run(mustJSON(t, map[string]any{"change_set": id}))
	if committed.IsError {
		t.Fatalf("commit_change_set failed: %s", committed.Content[0].Text)
	}
	result := committed.StructuredContent.(commitResult)
	if len(result.Applied) == 0 {
		t.Fatal("commit applied no files")
	}

	// The files are really there now.
	for _, change := range result.Applied {
		if _, err := os.Stat(filepath.Join(project, filepath.FromSlash(change.Path))); err != nil {
			t.Errorf("commit reported %s but it is not on disk: %v", change.Path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(project, "models", "invoice.go")); err != nil {
		t.Errorf("the generated model is missing: %v", err)
	}

	// And the history records what produced them.
	history := srv.tools["breeze_get_change_history"].run(mustJSON(t, map[string]any{"project_path": project}))
	if history.IsError {
		t.Fatalf("get_change_history failed: %s", history.Content[0].Text)
	}
	entries := history.StructuredContent.(historyResult).Entries
	if len(entries) != 1 {
		t.Fatalf("history has %d entries, want 1", len(entries))
	}
	if entries[0].Tool != "breeze_generate" {
		t.Errorf("history records tool %q, want breeze_generate", entries[0].Tool)
	}
	if entries[0].ChangeSet != id {
		t.Errorf("history records change set %q, want %q", entries[0].ChangeSet, id)
	}
	if len(entries[0].Changes) == 0 {
		t.Error("the history entry records no file changes")
	}
	// The arguments are kept verbatim, so a later reader can see what was asked
	// for rather than this package's reading of it.
	if len(entries[0].Arguments) == 0 {
		t.Error("the history entry records no arguments")
	}
}

// TestStageCallRefusesUnstageableTools checks that a read-only tool cannot be
// staged: it would contribute nothing to a commit, and accepting it would
// suggest otherwise.
func TestStageCallRefusesUnstageableTools(t *testing.T) {
	project := newFixtureProject(t, "scratch", "docs:\n  enabled: true\n")
	srv := NewServer("test")

	open := srv.tools["breeze_begin_change_set"].run(mustJSON(t, map[string]any{"project_path": project}))
	id := open.StructuredContent.(changeSetView).ID
	defer srv.tools["breeze_discard_change_set"].run(mustJSON(t, map[string]any{"change_set": id}))

	for _, name := range []string{"breeze_routes", "breeze_new"} {
		res := srv.tools["breeze_stage_call"].run(mustJSON(t, map[string]any{
			"change_set": id,
			"tool":       name,
			"arguments":  map[string]any{"name": "whatever"},
		}))
		if !res.IsError {
			t.Errorf("staging %s was accepted", name)
		}
	}
}

// ─── diff_config ─────────────────────────────────────────────────────────────

// TestDiffConfigReportsChangedFieldsAndBlockedTouches covers both halves of the
// tool: which settings differ, and which of the resulting edits the generator
// would refuse.
func TestDiffConfigReportsChangedFieldsAndBlockedTouches(t *testing.T) {
	project := newFixtureProject(t, "scratch", "docs:\n  enabled: true\n  title: Original\n")
	srv := NewServer("test")

	before, err := snapshotTree(project)
	if err != nil {
		t.Fatalf("snapshotting the project: %v", err)
	}

	res := srv.tools["breeze_diff_config"].run(mustJSON(t, map[string]any{
		"existing_path": project,
		"config_yaml":   "docs:\n  enabled: true\n  title: Renamed\n",
	}))
	if res.IsError {
		t.Fatalf("diff_config failed: %s", res.Content[0].Text)
	}
	diff := res.StructuredContent.(diffResult)

	if !diff.Valid {
		t.Fatalf("the proposal should validate; errors: %v", diff.Errors)
	}
	if !hasChange(diff.Changes, "docs.title", "Original", "Renamed") {
		t.Errorf("docs.title change not reported; changes were %+v", diff.Changes)
	}
	if len(diff.Touches) == 0 {
		t.Error("changing a generated value reported no file touches")
	}

	// diff_config must not write to the project it is comparing against.
	after, err := snapshotTree(project)
	if err != nil {
		t.Fatalf("re-snapshotting the project: %v", err)
	}
	if changes := diffSnapshots(before, after); len(changes) != 0 {
		t.Fatalf("diff_config modified the project: %v", changedPaths(changes))
	}

	// Now edit the block by hand and ask again: the same proposal must come back
	// blocked, because regenerating it would discard the edit.
	featuresPath := filepath.Join(project, generator.FeaturesFileName)
	source, err := os.ReadFile(featuresPath)
	if err != nil {
		t.Fatalf("reading the features file: %v", err)
	}
	// Again only the value, for the alignment reason above.
	edited := replaceFirst(t, string(source), `"Original"`, `"Hand Edited"`)

	if err := os.WriteFile(featuresPath, []byte(edited), 0o644); err != nil {
		t.Fatalf("writing the edited features file: %v", err)
	}

	blocked := srv.tools["breeze_diff_config"].run(mustJSON(t, map[string]any{
		"existing_path": project,
		"config_yaml":   "docs:\n  enabled: true\n  title: Renamed\n",
	}))
	if blocked.IsError {
		t.Fatalf("diff_config failed against an edited project: %s", blocked.Content[0].Text)
	}
	blockedDiff := blocked.StructuredContent.(diffResult)

	if len(blockedDiff.Blocked) == 0 {
		t.Fatal("an edited block was not reported as blocked")
	}
	if blockedDiff.Blocked[0].Feature != "docs" {
		t.Errorf("blocked feature = %q, want docs", blockedDiff.Blocked[0].Feature)
	}
	if blockedDiff.Blocked[0].Reason == "" {
		t.Error("a blocked touch carries no reason")
	}
}
