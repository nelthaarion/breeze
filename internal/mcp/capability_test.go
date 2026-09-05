package mcp

// capability_test.go — Part 8: per-token scope, reported once and enforced twice.

import (
	"strings"
	"testing"
)

// TestEveryToolHasACapability asserts the classification and the registry agree in
// both directions.
//
// Both directions matter. A tool with no capability is unreachable by every scoped
// token — a silent loss of function. A capability entry naming a tool that no longer
// exists is a grant that quietly does nothing, which is worse: an operator believes
// they granted something.
func TestEveryToolHasACapability(t *testing.T) {
	server := NewServer("test")

	for name := range server.tools {
		if _, ok := capabilityOf(name); !ok {
			t.Errorf("tool %q has no capability; every scoped token would refuse it", name)
		}
	}
	for name := range toolCapabilities {
		if _, ok := server.tools[name]; !ok {
			t.Errorf("toolCapabilities names %q, which is not a registered tool", name)
		}
	}

	// The counts follow from the two loops above, but asserting them turns "one of
	// these lists is longer" into a single legible failure rather than a wall of
	// per-tool errors.
	if len(toolCapabilities) != len(server.tools) {
		t.Errorf("%d tools registered, %d classified", len(server.tools), len(toolCapabilities))
	}
	t.Logf("%d tools, all classified", len(server.tools))
}

// TestCapabilityCategoriesAreAllUsed — a category no tool belongs to is a grant that
// cannot do anything, and it would still appear in the handshake's known list as if it
// meant something.
func TestCapabilityCategoriesAreAllUsed(t *testing.T) {
	used := map[Capability]int{}
	for _, c := range toolCapabilities {
		used[c]++
	}

	for _, known := range KnownCapabilities() {
		if used[known] == 0 {
			t.Errorf("capability %q classifies no tool", known)
		}
	}
	t.Logf("capability distribution: %v", used)
}

// TestUnscopedTokenKeepsFullAccess pins the default. Scoping is opt-in hardening, and
// an existing deployment that never passes --scope must behave exactly as before.
func TestUnscopedTokenKeepsFullAccess(t *testing.T) {
	scope := UnscopedScope()

	if scope.IsScoped() {
		t.Error("the zero Scope reports itself as scoped")
	}
	for _, c := range KnownCapabilities() {
		if !scope.Allows(c) {
			t.Errorf("an unscoped token was refused %q", c)
		}
	}
	// Including a tool with no classification: an unscoped token already has
	// everything, so refusing one would be a regression rather than a safeguard.
	if !scope.AllowsTool("a_tool_that_does_not_exist_yet") {
		t.Error("an unscoped token refused an unclassified tool")
	}
	if len(scope.Granted()) != len(KnownCapabilities()) {
		t.Errorf("Granted() = %v, want every capability", scope.Granted())
	}
}

// TestScopedTokenAllowsOnlyItsCategories is the enforcement half.
func TestScopedTokenAllowsOnlyItsCategories(t *testing.T) {
	scope, err := NewScope(CapFleet, CapRuntime)
	if err != nil {
		t.Fatal(err)
	}

	if !scope.IsScoped() {
		t.Fatal("a scoped token reports itself unscoped")
	}
	if !scope.AllowsTool("breeze_get_trace") {
		t.Error("a fleet-scoped token was refused breeze_get_trace")
	}
	if !scope.AllowsTool("breeze_get_logs") {
		t.Error("a runtime-scoped token was refused breeze_get_logs")
	}
	if scope.AllowsTool("breeze_generate") {
		t.Error("a {fleet,runtime} token was allowed breeze_generate")
	}
	if scope.AllowsTool("provision_service") {
		t.Error("a {fleet,runtime} token was allowed provision_service")
	}
	// An unclassified tool is withheld from a scoped token: the safe direction.
	if scope.AllowsTool("a_tool_that_does_not_exist_yet") {
		t.Error("a scoped token was allowed an unclassified tool")
	}
}

// TestParseScope covers the flag's spelling, including the blanks a hand-written value
// carries and the empty value that means "unscoped".
func TestParseScope(t *testing.T) {
	unscoped, err := ParseScope("")
	if err != nil {
		t.Fatal(err)
	}
	if unscoped.IsScoped() {
		t.Error(
			`--scope="" produced a scoped token; not passing it and passing it empty are the same intent`,
		)
	}

	scoped, err := ParseScope("fleet, ,runtime")
	if err != nil {
		t.Fatal(err)
	}
	if !scoped.AllowsTool("breeze_get_trace") || !scoped.AllowsTool("breeze_get_logs") {
		t.Errorf("granted = %v, want fleet and runtime", scoped.Granted())
	}
	if scoped.AllowsTool("breeze_new") {
		t.Error("a {fleet,runtime} scope allowed breeze_new")
	}

	if _, err := ParseScope("fleet,nonsense"); err == nil {
		t.Error("an unknown capability was accepted")
	}
}

// TestEmptyScopeIsRefused — a credential that authenticates and then rejects every call
// is indistinguishable from a broken server, and it is what a caller gets when a
// dynamically built capability list came back empty. Both silent readings of that
// ("grant nothing", "grant everything") are worse than an error.
func TestEmptyScopeIsRefused(t *testing.T) {
	if _, err := NewScope(); err == nil {
		t.Fatal("NewScope() with no capabilities was accepted")
	}

	// The refusal has to point at the alternative, or a caller who genuinely wanted an
	// unrestricted token has no idea what to write instead.
	_, err := NewScope()
	if !strings.Contains(err.Error(), "omit the scope") {
		t.Errorf("the refusal does not name the unscoped alternative: %v", err)
	}
}

// TestScopeIsSafeToCopy — Scope is passed by value through NetworkConfig,
// InProcessConfig and Options, and a copy that shared mutable state with its original
// would make one server's scope changeable through another's.
//
// The map inside is written only in NewScope and never afterwards, so sharing the
// backing store is safe; this pins that no method mutates it.
func TestScopeIsSafeToCopy(t *testing.T) {
	original, err := NewScope(CapFleet)
	if err != nil {
		t.Fatal(err)
	}
	copied := original

	// Exercise every read path on the copy.
	_ = copied.Granted()
	_ = copied.Allows(CapProvisioning)
	_ = copied.AllowsTool("provision_service")

	if !original.AllowsTool("breeze_get_trace") {
		t.Error("reading through a copy changed the original's grants")
	}
	if original.AllowsTool("provision_service") {
		t.Error("the original gained a capability it was never granted")
	}
	if got := strings.Join(capabilityNames(original.Granted()), ","); got != "fleet" {
		t.Errorf("the original's granted set became %q", got)
	}
}
