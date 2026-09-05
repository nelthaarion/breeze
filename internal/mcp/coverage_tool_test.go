package mcp

// coverage_tool_test.go — breeze_get_test_coverage as a tool in its own right.

import "testing"

// TestCoverageToolIsIndependentlyCallableAndAgreesWithVerifyProject.
//
// Two claims, and the second is the one worth a test.
//
// The first is that breeze_get_test_coverage is reachable on its own rather than
// only as a stage of something larger — it is registered in
// registerVerificationTools and callable through tools/call, and this asserts it
// end to end rather than by reading the registration.
//
// The second is that its per-package figures agree with what breeze_verify_project
// reports for the same project. They agree because both decode `go test` output
// through parseTestOutput, and that shared parser is the reason the answer is one
// answer rather than two. A second parser added on either side would show up here
// as a disagreement about a project that did not change between the calls.
func TestCoverageToolIsIndependentlyCallableAndAgreesWithVerifyProject(t *testing.T) {
	root := newToolchainFixture(t, goodModule)
	srv := NewServer("test")

	// Registered, and reachable by name: an unknown tool comes back as a
	// JSON-RPC error, which callTool would fail on before returning.
	if _, ok := srv.tools["breeze_get_test_coverage"]; !ok {
		t.Fatal("breeze_get_test_coverage is not in the tool table, so it cannot be called on its own")
	}

	coverage := callTool(t, srv, "breeze_get_test_coverage", map[string]any{"path": root})
	if coverage.IsError {
		t.Fatalf("the standalone coverage tool failed: %s", coverage.Content[0].Text)
	}
	verify := callTool(t, srv, "breeze_verify_project", map[string]any{"path": root})
	if verify.IsError {
		t.Fatalf("verify_project failed: %s", verify.Content[0].Text)
	}

	// Both report per-package status. The coverage tool adds a percentage, which
	// verify_project does not measure, so the comparison is over the fields both
	// claim to know: which packages there are and how each one fared.
	fromCoverage := packageStatuses(t, structOf(t, coverage))
	fromVerify := packageStatuses(t, testStepOf(t, structOf(t, verify)))

	if len(fromCoverage) == 0 {
		t.Fatal("the coverage tool reported no packages at all")
	}
	for pkg, status := range fromVerify {
		got, ok := fromCoverage[pkg]
		if !ok {
			t.Errorf("verify_project reports %s but the coverage tool does not mention it; the two "+
				"are reading the same `go test` output and should not disagree about which packages exist", pkg)
			continue
		}
		if got != status {
			t.Errorf("%s is %q according to the coverage tool and %q according to verify_project",
				pkg, got, status)
		}
	}

	// And the structured shape the tool promises, since a caller branches on it.
	report := structOf(t, coverage)
	for _, key := range []string{"path", "command", "packages", "tests_passed", "duration_ms"} {
		if _, ok := report[key]; !ok {
			t.Errorf("the standalone result has no %q field:\n%s", key, renderStructured("", report))
		}
	}
	if _, ok := report["total_percent"].(float64); !ok {
		t.Errorf("the standalone result carries no measured coverage, so it is not the report "+
			"verify_project's caller would get from it:\n%s", renderStructured("", report))
	}
}

// packageStatuses maps package path to reported status, for either report shape.
func packageStatuses(t *testing.T, node map[string]any) map[string]string {
	t.Helper()

	out := map[string]string{}
	for _, pkg := range listOf(t, node, "packages") {
		name, _ := pkg["package"].(string)
		status, _ := pkg["status"].(string)
		if name != "" {
			out[name] = status
		}
	}
	return out
}

// testStepOf returns verify_project's test stage, which is where its own
// per-package results live.
func testStepOf(t *testing.T, report map[string]any) map[string]any {
	t.Helper()

	for _, step := range listOf(t, report, "steps") {
		if name, _ := step["name"].(string); name == "test" {
			return step
		}
	}
	t.Fatalf("verify_project reported no test step:\n%s", renderStructured("", report))
	return nil
}
