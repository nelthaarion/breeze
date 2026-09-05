package mcp

// scope_test.go — the classification, held to the registry.
//
// The table in scope.go is a security boundary: it decides which tools may run
// inside a live application's process. A table that drifts from the registry fails
// in one of two ways, and both are silent. A tool added without an entry would
// either vanish from in-process mode (a missing capability nobody notices) or, if
// the default went the other way, appear there unreviewed. So both directions are
// asserted.

import (
	"sort"
	"strings"
	"testing"
)

// TestEveryToolIsClassified is the both-directions check.
//
// It is why a new tool cannot be added without a deliberate decision about where it
// may run: whichever side is missing, this fails and names it.
func TestEveryToolIsClassified(t *testing.T) {
	registry := NewServer("test")

	for name := range registry.tools {
		if _, ok := scopeOf(name); !ok {
			t.Errorf(
				"%s is registered but not classified in toolScopes. Decide whether it is safe to "+
					"run inside a live application's process — see scope.go for the two criteria — and add it.",
				name,
			)
		}
	}

	for name := range toolScopes {
		if _, ok := registry.tools[name]; !ok {
			t.Errorf(
				"toolScopes classifies %s, which is not a registered tool; the entry is stale",
				name,
			)
		}
	}

	if len(toolScopes) != len(registry.tools) {
		t.Errorf(
			"%d tools are registered but %d are classified",
			len(registry.tools),
			len(toolScopes),
		)
	}
}

// TestInProcessServerServesOnlyTheSafeSubset is the acceptance criterion that
// in-process mode defaults to the safe set.
//
// It asserts against the actual server's tool map rather than against the table, so
// a filtering bug in NewInProcessServer cannot pass by agreeing with the
// classification it failed to apply.
func TestInProcessServerServesOnlyTheSafeSubset(t *testing.T) {
	server := NewInProcessServer("test", false)

	if len(server.tools) == 0 {
		t.Fatal(
			"the in-process server has no tools at all, which makes it useless rather than safe",
		)
	}

	for name := range server.tools {
		scope, classified := scopeOf(name)
		if !classified {
			t.Errorf("%s is served in-process but has no classification", name)
			continue
		}
		if scope != scopeInProcess {
			t.Errorf("%s is workspace-only but is served in-process", name)
		}
	}

	// The count matches the table, so a tool cannot go missing from both without this
	// noticing.
	if len(server.tools) != len(InProcessToolNames()) {
		t.Errorf("the in-process server serves %d tools, the table lists %d as safe",
			len(server.tools), len(InProcessToolNames()))
	}

	// And the ordering is rebuilt after filtering. A stale order would leave
	// tools/list naming tools the server no longer has, which is worse than either
	// serving or not serving them.
	if len(server.order) != len(server.tools) {
		t.Errorf("%d tools but %d ordered names; sortTools was not re-run after filtering",
			len(server.tools), len(server.order))
	}
	for _, name := range server.order {
		if _, ok := server.tools[name]; !ok {
			t.Errorf("tools/list would name %s, which the in-process server does not have", name)
		}
	}
	if !sort.StringsAreSorted(server.order) {
		t.Error("the in-process tool order is not sorted, so tools/list is unstable")
	}
}

// TestWorkspaceToolsAreAbsentInProcess names the specific exclusions, so removing
// one from the table is a visible change rather than a quiet widening.
//
// The generation three chdir and capture os.Stdout while the application is serving
// on that working directory; the provisioning ones do both and then talk to Docker.
func TestWorkspaceToolsAreAbsentInProcess(t *testing.T) {
	server := NewInProcessServer("test", false)

	for _, name := range []string{
		"breeze_new", "breeze_generate", "breeze_add",
		"provision_service", "provision_fleet", "deprovision_service",
		"breeze_verify_project", "breeze_plan_project", "breeze_begin_change_set",
	} {
		if _, present := server.tools[name]; present {
			t.Errorf(
				"%s is served in-process by default; it mutates a workspace and must be opt-in",
				name,
			)
		}
	}

	// The counterpart: the tools an in-process endpoint exists for are present.
	for _, name := range []string{
		"breeze_get_routes", "breeze_get_performance", "breeze_get_logs",
		"breeze_get_recent_errors", "breeze_query_openapi", "breeze_simulate_request",
	} {
		if _, present := server.tools[name]; !present {
			t.Errorf(
				"%s is absent from in-process mode, which is the read-only introspection it exists for",
				name,
			)
		}
	}
}

// TestAllowWorkspaceToolsRestoresEverything covers the opt-in.
//
// An option that silently does nothing is worse than no option, and this one has a
// documented risk attached — so it has to actually do the thing the documentation
// warns about.
func TestAllowWorkspaceToolsRestoresEverything(t *testing.T) {
	full := NewServer("test")
	opted := NewInProcessServer("test", true)

	if len(opted.tools) != len(full.tools) {
		t.Fatalf("with AllowWorkspaceTools the server has %d tools, the full registry has %d",
			len(opted.tools), len(full.tools))
	}
	for name := range full.tools {
		if _, present := opted.tools[name]; !present {
			t.Errorf("%s is missing even with AllowWorkspaceTools set", name)
		}
	}
}

// TestClassificationCountsAreReportedAccurately checks the two exported inventories
// against each other and the registry.
//
// They are what the wrapper package reports to an application at startup, and what
// the PR's classification table is derived from, so a wrong count is a wrong claim
// in two places at once. The log lines are the table itself, so a reviewer can read
// the classification out of a test run.
func TestClassificationCountsAreReportedAccurately(t *testing.T) {
	safe := InProcessToolNames()
	workspace := WorkspaceOnlyToolNames()

	if len(safe)+len(workspace) != len(toolScopes) {
		t.Errorf("%d safe + %d workspace = %d, but %d tools are classified",
			len(safe), len(workspace), len(safe)+len(workspace), len(toolScopes))
	}
	if !sort.StringsAreSorted(safe) || !sort.StringsAreSorted(workspace) {
		t.Error("the inventories are not sorted, so they are unstable between calls")
	}

	// No overlap: a tool in both lists would mean scopeOf returned two answers.
	inSafe := map[string]bool{}
	for _, name := range safe {
		inSafe[name] = true
	}
	for _, name := range workspace {
		if inSafe[name] {
			t.Errorf("%s appears in both inventories", name)
		}
	}

	t.Logf("in-process safe (%d): %s", len(safe), strings.Join(safe, ", "))
	t.Logf("workspace-only (%d): %s", len(workspace), strings.Join(workspace, ", "))
}
