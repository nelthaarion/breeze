package mcp

// capability_map.go — which category each tool belongs to.
//
// Separate from capability.go so the table is readable on its own: it is a security
// boundary, and a reviewer should be able to see all of it at once without the
// surrounding rationale in the way.

// toolCapabilities classifies every registered tool into exactly one category.
//
// One category per tool, not a set: a tool that belonged to two would mean granting
// either one grants it, which makes the narrower grant a lie. Where a tool could
// plausibly sit in two categories the more privileged one wins — breeze_generate_llms_txt
// writes files, so it is generation-adjacent knowledge and is classified by what it
// *does* rather than by what it is about.
//
// A test asserts this map and the registry agree in both directions, so a tool added
// later cannot default into a category or go unclassified.
var toolCapabilities = map[string]Capability{
	// ── generation: writes project files ─────────────────────────────────────
	"breeze_new":      CapGeneration,
	"breeze_generate": CapGeneration,
	"breeze_add":      CapGeneration,

	// ── introspection: what exists, read-only ────────────────────────────────
	"breeze_features": CapIntrospection,
	"breeze_routes":   CapIntrospection,

	// ── planning: previews and change sets ───────────────────────────────────
	//
	// A change set writes only to its own temporary copy until commit, and commit is
	// the one operation that touches the project — which is why the whole group is
	// planning rather than generation, and why a token granted planning but not
	// generation can still commit a change set it staged. That is deliberate: the
	// staged calls were themselves subject to this same check when they ran.
	"breeze_plan_project":       CapPlanning,
	"breeze_explain_project":    CapPlanning,
	"breeze_diff_config":        CapPlanning,
	"breeze_begin_change_set":   CapPlanning,
	"breeze_stage_call":         CapPlanning,
	"breeze_commit_change_set":  CapPlanning,
	"breeze_discard_change_set": CapPlanning,
	"breeze_get_change_history": CapPlanning,

	// ── knowledge: llms.txt, examples, idioms ────────────────────────────────
	"breeze_describe_schema":          CapKnowledge,
	"breeze_list_examples":            CapKnowledge,
	"breeze_explain_idiom":            CapKnowledge,
	"breeze_generate_llms_txt":        CapKnowledge,
	"breeze_check_llms_txt_freshness": CapKnowledge,
	"breeze_search_llms_txt":          CapKnowledge,
	"breeze_suggest_next_steps":       CapKnowledge,

	// ── verification: runs the Go toolchain, and therefore project code ──────
	"breeze_check_idioms":      CapVerification,
	"breeze_verify_project":    CapVerification,
	"breeze_run_benchmarks":    CapVerification,
	"breeze_get_test_coverage": CapVerification,

	// ── runtime: reads a running service ─────────────────────────────────────
	"breeze_get_routes":        CapRuntime,
	"breeze_get_performance":   CapRuntime,
	"breeze_get_recent_errors": CapRuntime,
	"breeze_get_logs":          CapRuntime,
	"breeze_query_openapi":     CapRuntime,
	"breeze_simulate_request":  CapRuntime,
	"breeze_diagnose_service":  CapRuntime,

	// ── fleet: reads an aggregator ───────────────────────────────────────────
	"breeze_get_topology":            CapFleet,
	"breeze_get_traces":              CapFleet,
	"breeze_get_trace":               CapFleet,
	"breeze_get_contract_violations": CapFleet,
	"breeze_explain_incident":        CapFleet,

	// ── provisioning: drives Docker ──────────────────────────────────────────
	"provision_service":         CapProvisioning,
	"list_provisioned_services": CapProvisioning,
	"deprovision_service":       CapProvisioning,
	"provision_fleet":           CapProvisioning,
}

// capabilityOf reports a tool's category, and whether it has one.
//
// An unclassified tool is not silently granted to everyone: it is withheld from every
// scoped token and a test fails on it. Withholding is the safe direction — the
// consequence is a missing capability, which is visible, rather than an unreviewed
// tool reachable by a credential that was supposed to be narrow.
func capabilityOf(name string) (Capability, bool) {
	c, ok := toolCapabilities[name]
	return c, ok
}
