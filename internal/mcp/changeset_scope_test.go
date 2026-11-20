package mcp

// changeset_scope_test.go — a change set must not be a way around a token's scope.
//
// breeze_stage_call runs a generator tool by name from inside another tool, so the call
// never passes through tools/call and its scope check. It is classified as planning. If it
// did not ask the scope about the tool it stages, a token granted planning but not
// generation could stage breeze_generate / breeze_add / breeze_new and commit them: project
// files written with a credential minted so that it could not write any.

import (
	"strings"
	"testing"
)

func planningOnlyServer(t *testing.T) (*Server, *recorder) {
	t.Helper()
	scope, err := NewScope(CapPlanning)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer("test")
	srv.SetScope(scope)
	rec := &recorder{}
	srv.SetLogger(rec)
	return srv, rec
}

func TestPlanningOnlyTokenCannotStageAGenerationTool(t *testing.T) {
	project := newFixtureProject(t, "scratch", "docs:\n  enabled: true\n")
	srv, rec := planningOnlyServer(t)

	before, err := snapshotTree(project)
	if err != nil {
		t.Fatal(err)
	}

	open := srv.tools["breeze_begin_change_set"].run(mustJSON(t, map[string]any{"project_path": project}))
	if open.IsError {
		t.Fatalf("a planning token could not open a change set, which is what planning is for: %s", open.Content[0].Text)
	}
	id := open.StructuredContent.(changeSetView).ID

	for _, tool := range stageableTools {
		staged := srv.tools["breeze_stage_call"].run(mustJSON(t, map[string]any{
			"change_set": id,
			"tool":       tool,
			"arguments":  map[string]any{"kind": "model", "name": "User", "fields": []string{"name:string"}},
		}))
		if !staged.IsError {
			t.Fatalf("a planning-only token staged %s: scope was bypassed through a change set", tool)
		}
		text := staged.Content[0].Text
		if !strings.Contains(text, "outside this token's scope") || !strings.Contains(text, "generation") {
			t.Errorf("%s was refused, but not as a scope refusal that names the missing capability: %s", tool, text)
		}
		detail, ok := staged.StructuredContent.(map[string]any)
		if !ok || detail["requires"] != "generation" || detail["refused"] != true {
			t.Errorf("%s: structured refusal = %#v, want requires=generation refused=true", tool, staged.StructuredContent)
		}
	}

	// Nothing was staged, so committing writes nothing.
	commit := srv.tools["breeze_commit_change_set"].run(mustJSON(t, map[string]any{"change_set": id}))
	if !commit.IsError {
		if view, ok := commit.StructuredContent.(commitResult); ok && len(view.Applied) > 0 {
			t.Fatalf("committing a change set that only held refused calls wrote %v", changedPaths(view.Applied))
		}
	}
	after, err := snapshotTree(project)
	if err != nil {
		t.Fatal(err)
	}
	if changes := diffSnapshots(before, after); len(changes) > 0 {
		t.Fatalf("the project changed although every staged call was refused: %v", changedPaths(changes))
	}

	// Every refusal is on the record, so an attempt is visible to whoever reads the log.
	var refused int
	for _, ev := range rec.all() {
		if ev.Kind == EventToolRefused {
			refused++
		}
	}
	if refused != len(stageableTools) {
		t.Errorf("%d refusals were logged, want %d (one per refused stage)", refused, len(stageableTools))
	}
}

func TestPlanningPlusGenerationTokenCanStage(t *testing.T) {
	// The other half: the check must refuse on the missing capability, not on staging
	// as such, or a token meant to generate through change sets would be locked out.
	project := newFixtureProject(t, "scratch", "docs:\n  enabled: true\n")
	scope, err := NewScope(CapPlanning, CapGeneration)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer("test")
	srv.SetScope(scope)

	open := srv.tools["breeze_begin_change_set"].run(mustJSON(t, map[string]any{"project_path": project}))
	if open.IsError {
		t.Fatal(open.Content[0].Text)
	}
	id := open.StructuredContent.(changeSetView).ID

	staged := srv.tools["breeze_stage_call"].run(mustJSON(t, map[string]any{
		"change_set": id,
		"tool":       "breeze_generate",
		"arguments":  map[string]any{"kind": "model", "name": "User", "fields": []string{"name:string"}},
	}))
	if staged.IsError {
		t.Fatalf("a token granted planning and generation could not stage a generator call: %s", staged.Content[0].Text)
	}
}

func TestStagingAnUnknownToolStillReportsTheStageableList(t *testing.T) {
	// An unknown name must not be reported as a scope problem: the useful answer is the
	// list of tools a change set will run.
	project := newFixtureProject(t, "scratch", "docs:\n  enabled: true\n")
	srv, _ := planningOnlyServer(t)

	open := srv.tools["breeze_begin_change_set"].run(mustJSON(t, map[string]any{"project_path": project}))
	id := open.StructuredContent.(changeSetView).ID

	staged := srv.tools["breeze_stage_call"].run(mustJSON(t, map[string]any{
		"change_set": id, "tool": "breeze_does_not_exist", "arguments": map[string]any{},
	}))
	if !staged.IsError {
		t.Fatal("staging a tool that does not exist was accepted")
	}
	if text := staged.Content[0].Text; strings.Contains(text, "outside this token's scope") {
		t.Errorf("an unknown tool was reported as a scope refusal: %s", text)
	}
}

func TestEveryStageableToolIsClassifiedAsGeneration(t *testing.T) {
	// The scope check in stageCallTool is only as good as the classification it consults.
	// If a stageable tool were ever reclassified lower — or left unclassified, which a
	// scoped token cannot call at all — the guarantee that a planning token cannot
	// generate would depend on something nobody is looking at. Pinned here.
	for _, tool := range stageableTools {
		capability, classified := capabilityOf(tool)
		if !classified {
			t.Errorf("%s is stageable but unclassified", tool)
			continue
		}
		if capability != CapGeneration {
			t.Errorf("%s is stageable and classified %q; it writes project files, which is generation",
				tool, capability)
		}
	}
}
