package mcp

// scope.go — which tools may run inside a live application's own process.
//
// # Why this exists as a table
//
// cmd/breeze-mcp/main.go refuses to serve stdio and network at once, and the
// reason is not the transports: the generation tools chdir and replace os.Stdout
// under one process-wide lock (capture.go), so two peers driving them would race
// over the same mutable workspace.
//
// Running MCP inside a running application raises the same question one level in.
// A tool called from an app's own goroutine shares that process with request
// handling, and a tool that chdirs changes the working directory *of the server
// currently serving traffic*. Any relative path the application resolves during
// that window — ServeStatic's root, a template directory, a log file, a SQLite
// path — resolves somewhere else. That is not a theoretical race; it is a certain
// one for the duration of every generator call.
//
// So each tool is classified once, here. A table rather than a field spread across
// eight files, because the classification is a security boundary and a reviewer
// should be able to read all of it at once. A test asserts the table and the
// registry agree in both directions, so a tool added later cannot quietly default
// into either group.
//
// # The two criteria
//
// A tool is in-process safe only if both hold:
//
//  1. It does not chdir, replace os.Stdout, or set process environment. Those are
//     process-global, and the application owns them while it is serving.
//  2. It does not need a source tree. A deployed binary was built by `go build`
//     from a module cache, not from a clone: its own source is not on disk, so a
//     tool that reads or writes a project directory has nothing to work on and
//     would report a missing project as a finding.
//
// The second criterion alone excludes generation, planning, verification and
// provisioning from a deployed app, independently of concurrency. Both criteria
// point the same way, which is why the default is not a compromise.

// toolScope says where a tool may run.
type toolScope int

const (
	// scopeWorkspace is a tool that operates on a source tree, and does so through
	// chdir, stdout capture, a sandbox copy, or the Go toolchain. Standalone
	// breeze-mcp only.
	scopeWorkspace toolScope = iota

	// scopeInProcess is a tool that reads live state over HTTP or answers from
	// constants compiled into the binary. It touches no process-global state and
	// needs no source tree, so it is safe to serve from inside a running app.
	scopeInProcess
)

// toolScopes classifies every registered tool.
//
// The grouping follows the reason rather than the file, because the reason is what
// a reader needs: two tools in one file can differ, and two in different files can
// be excluded for the same cause.
var toolScopes = map[string]toolScope{
	// ── Safe in-process: live runtime reads ──────────────────────────────────
	//
	// These fetch a running service's dashboard or Fleet aggregator over HTTP and
	// decode JSON. No chdir, no stdout capture, no filesystem. Pointed at the host
	// application's own address they answer "what is this instance doing right
	// now", which is the entire purpose of in-process mode.
	"breeze_get_routes":              scopeInProcess,
	"breeze_get_performance":         scopeInProcess,
	"breeze_get_recent_errors":       scopeInProcess,
	"breeze_get_logs":                scopeInProcess,
	"breeze_query_openapi":           scopeInProcess,
	"breeze_diagnose_service":        scopeInProcess,
	"breeze_get_topology":            scopeInProcess,
	"breeze_get_traces":              scopeInProcess,
	"breeze_get_trace":               scopeInProcess,
	"breeze_get_contract_violations": scopeInProcess,
	"breeze_explain_incident":        scopeInProcess,

	// breeze_simulate_request sends one real HTTP request with the framework's own
	// client. In-process that means the app calling itself, which is a supported
	// thing to do: it is an ordinary client connection, indistinguishable from an
	// external caller, and it exercises the route's real chain. It writes nothing.
	"breeze_simulate_request": scopeInProcess,

	// ── Safe in-process: answers from constants ──────────────────────────────
	//
	// These read no disk at all. describe_schema reflects over ProjectConfig,
	// list_examples validates literals declared in this package, explain_idiom and
	// features return data from tables. They are useful in-process for the same
	// reason they are useful anywhere: an agent that guesses a field name gets it
	// wrong, and a wrong field is silently ignored.
	"breeze_describe_schema": scopeInProcess,
	"breeze_list_examples":   scopeInProcess,
	"breeze_explain_idiom":   scopeInProcess,
	"breeze_features":        scopeInProcess,

	// ── Workspace-only: generation ───────────────────────────────────────────
	//
	// runGenerator → captureStdout → runInDir. Replaces os.Stdout and chdirs the
	// whole process while the application is serving requests on it.
	"breeze_new":      scopeWorkspace,
	"breeze_generate": scopeWorkspace,
	"breeze_add":      scopeWorkspace,

	// breeze_routes is read-only in intent and still workspace-only in mechanism:
	// it goes through runGenerator, so it takes the capture lock and chdirs. It also
	// reads routes_generated.go, which a deployed binary does not carry. The live
	// equivalent is breeze_get_routes, which is in the safe set — and is the better
	// answer in-process anyway, because it reports what is actually being served
	// rather than what the source declared.
	"breeze_routes": scopeWorkspace,

	// ── Workspace-only: planning and change sets ─────────────────────────────
	//
	// plan_project and diff_config scaffold into sandbox copies; explain_project
	// reads a tree through inProject, which is captureStdout plus chdir. A change
	// set holds a temporary copy of a project across several calls and commits it
	// back — stateful, filesystem-bound, and meaningless without a source tree.
	"breeze_plan_project":       scopeWorkspace,
	"breeze_explain_project":    scopeWorkspace,
	"breeze_diff_config":        scopeWorkspace,
	"breeze_begin_change_set":   scopeWorkspace,
	"breeze_stage_call":         scopeWorkspace,
	"breeze_commit_change_set":  scopeWorkspace,
	"breeze_discard_change_set": scopeWorkspace,
	"breeze_get_change_history": scopeWorkspace,

	// ── Workspace-only: knowledge tools that read or write a tree ────────────
	//
	// generate_llms_txt writes files into a project. The freshness check, the search
	// and the suggestions all build their answer from a project directory, and
	// gatherProjectFacts goes through inProject — so they chdir too.
	"breeze_generate_llms_txt":        scopeWorkspace,
	"breeze_check_llms_txt_freshness": scopeWorkspace,
	"breeze_search_llms_txt":          scopeWorkspace,
	"breeze_suggest_next_steps":       scopeWorkspace,

	// ── Workspace-only: verification ─────────────────────────────────────────
	//
	// These run the Go toolchain against a module root. They use cmd.Dir rather than
	// chdir, so they are not a concurrency hazard — but they need go.mod and a full
	// source tree, which a deployed binary does not have, and `go test` inside a
	// production container is not something to leave one tool call away.
	"breeze_check_idioms":      scopeWorkspace,
	"breeze_verify_project":    scopeWorkspace,
	"breeze_run_benchmarks":    scopeWorkspace,
	"breeze_get_test_coverage": scopeWorkspace,

	// ── Workspace-only: provisioning ─────────────────────────────────────────
	//
	// Category H generates projects (chdir, stdout capture) and drives Docker. An
	// application able to provision its own siblings from inside its own process is
	// a privilege escalation with no upside: the orchestrator exists precisely so
	// that exactly one instance holds that capability.
	"provision_service":         scopeWorkspace,
	"list_provisioned_services": scopeWorkspace,
	"deprovision_service":       scopeWorkspace,
	"provision_fleet":           scopeWorkspace,
}

// scopeOf reports a tool's classification, and whether it has one.
//
// An unclassified tool is not silently allowed anywhere: NewInProcessServer drops
// it and a test fails on it. Defaulting to workspace-only would be the safe
// direction but would let a new read-only tool go missing from in-process mode
// without anyone noticing.
func scopeOf(name string) (toolScope, bool) {
	scope, ok := toolScopes[name]
	return scope, ok
}
