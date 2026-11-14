package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nelthaarion/breeze/v2/internal/generator"
)

// A project generated before the workflow block was corrected hands the engine the observability block's
// collector, and the dashboard reads a different one, so every workflow run is published where nothing reads
// it. Nothing looks broken: the runs' bus events are recorded and the live executions list works. The check
// exists for the projects that already have the old block, and it is run against what the real generator writes,
// not against a fixture that imitates it.
func TestSuggestNextStepsFlagsAWorkflowBlockThePreviousGeneratorWrote(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/wf\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	for _, args := range [][]string{{"events"}, {"observability"}, {"dashboard", "--no-auth"}, {"workflow"}} {
		if err := generator.Add(args); err != nil {
			t.Fatalf("breeze add %s: %v", strings.Join(args, " "), err)
		}
	}
	srv := NewServer("test")

	// What the generator writes now is right, so nothing is flagged.
	if got := suggestions(t, srv, root); findSuggestion(got, "workflow-invisible-to-dashboard", "workflow") != nil {
		t.Fatalf("a freshly generated workflow block is flagged: %v", suggestionKinds(got))
	}

	// Put back the block the previous generator wrote. The premise: the text it replaces is really there.
	path := filepath.Join(root, "features_generated.go")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	current := "Collector:       obs,"
	if !strings.Contains(string(source), current) {
		t.Fatalf("the generated workflow block no longer reads %q, so this test cannot put the old one back:\n%s", current, source)
	}
	old := strings.Replace(string(source), current, "Collector:       ObsCollector,", 1)
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}

	got := suggestions(t, srv, root)
	found := findSuggestion(got, "workflow-invisible-to-dashboard", "workflow")
	if found == nil {
		t.Fatalf("the old workflow block was not flagged: %v", suggestionKinds(got))
	}
	if !strings.Contains(found.Action, "feature=workflow") || found.Severity != severityWarning {
		t.Errorf("suggestion = %+v, want a warning that says to regenerate the workflow block", found)
	}
	checked := false
	for _, rule := range got.Checked {
		checked = checked || rule == "workflow-invisible-to-dashboard"
	}
	if !checked {
		t.Errorf("the result does not list the rule among those it checked: %v", got.Checked)
	}
}

// Without a dashboard the observability block's collector is the right one, and without events the dashboard has none.
func TestSuggestNextStepsLeavesAnObservabilityCollectorAloneWhereItIsRight(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/wf2\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	for _, args := range [][]string{{"events"}, {"observability"}, {"workflow"}} { // no dashboard
		if err := generator.Add(args); err != nil {
			t.Fatalf("breeze add %s: %v", strings.Join(args, " "), err)
		}
	}
	got := suggestions(t, NewServer("test"), root)
	if findSuggestion(got, "workflow-invisible-to-dashboard", "workflow") != nil {
		t.Errorf("a project with no dashboard is told its workflow is invisible to one: %v", suggestionKinds(got))
	}
}
