package mcp_test

// diag_test.go — the embedded endpoint reports on itself.
//
// # What this is checking
//
// breeze_diagnose_service reads the diag registry, so before this probe existed the
// tool reported on every subsystem except the one answering the call. The tests here
// are the ones that would catch the three states an agent cannot diagnose from
// outside: a scope that withheld a tool, a generator-mode embed in a container with
// no source tree, and AllowWorkspaceTools left on in production.
//
// Each assertion is about the probe's contract rather than its wording, except where
// the wording is the point — a note exists to be read, so a note that says nothing
// specific is worse than no note.

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/nelthaarion/breeze/diag"
	"github.com/nelthaarion/breeze/mcp"
)

// TestEmbeddedEndpointRegistersItsOwnProbe is the gap this closed.
func TestEmbeddedEndpointRegistersItsOwnProbe(t *testing.T) {
	app := startLiveApp(t, mcp.InProcessConfig{})

	report, found := diag.Get("mcp")
	if !found {
		t.Fatalf("no probe is registered under %q; a running endpoint is invisible to "+
			"breeze_diagnose_service. Registered: %v", "mcp", diag.Registered())
	}

	if report.Status != diag.StatusOK {
		t.Errorf("status = %q, want ok for a bound and serving endpoint: %+v",
			report.Status, report)
	}

	// The address is what a caller configures a client with, and the only field that
	// answers "which endpoint am I looking at" on a process running two.
	addr, _ := report.Detail["address"].(string)
	if addr == "" {
		t.Error("the report has no address")
	}
	if !strings.Contains(app.controlURL, addr) {
		t.Errorf("report address %q is not the endpoint's URL %q", addr, app.controlURL)
	}

	if mode, _ := report.Detail["mode"].(string); mode != string(mcp.ModeAppRuntime) {
		t.Errorf("mode = %q, want %q", mode, mcp.ModeAppRuntime)
	}
	if ep, _ := report.Detail["endpoint"].(string); ep != mcp.Endpoint {
		t.Errorf("endpoint = %q, want %q", ep, mcp.Endpoint)
	}

	// reachable_tools is the number that answers "why can't I call X". It must be
	// positive on an unscoped app-runtime embed, and no larger than the registered
	// count.
	total, okT := report.Detail["tools"].(int)
	reachable, okR := report.Detail["reachable_tools"].(int)
	if !okT || !okR {
		t.Fatalf("tools/reachable_tools are %T/%T, want ints: %+v",
			report.Detail["tools"], report.Detail["reachable_tools"], report.Detail)
	}
	if total <= 0 {
		t.Errorf("tools = %d, want the app-runtime tool set", total)
	}
	if reachable != total {
		t.Errorf("reachable_tools = %d but tools = %d on an unscoped token; nothing "+
			"should be withheld", reachable, total)
	}
	if _, present := report.Detail["withheld_by_scope"]; present {
		t.Errorf("withheld_by_scope is present on an unscoped token: %v",
			report.Detail["withheld_by_scope"])
	}
}

// TestProbeNamesTheLayerThatWithheldATool is the reason the probe carries two
// counts instead of one.
//
// A client calling a scoped-out tool gets the same "no such tool" as for a tool that
// does not exist. Mode decides what is registered; scope decides what the credential
// reaches. Neither is visible to the caller, and this is the only place both are.
func TestProbeNamesTheLayerThatWithheldATool(t *testing.T) {
	scope, err := mcp.NewScope(mcp.CapRuntime)
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	startLiveApp(t, mcp.InProcessConfig{Scope: scope})

	report, found := diag.Get("mcp")
	if !found {
		t.Fatal("no probe is registered")
	}

	if scoped, _ := report.Detail["scoped"].(bool); !scoped {
		t.Error("scoped = false on a token built with NewScope")
	}

	total, _ := report.Detail["tools"].(int)
	reachable, _ := report.Detail["reachable_tools"].(int)
	if reachable >= total {
		t.Errorf("reachable_tools = %d of %d; a CapRuntime-only token should not reach "+
			"the whole app-runtime tool set", reachable, total)
	}

	withheld, ok := report.Detail["withheld_by_scope"].([]string)
	if !ok || len(withheld) == 0 {
		t.Fatalf("withheld_by_scope is %v; the tools the scope removed are the whole "+
			"point of this field", report.Detail["withheld_by_scope"])
	}
	if len(withheld) != total-reachable {
		t.Errorf("withheld_by_scope lists %d tool(s) but %d are unreachable",
			len(withheld), total-reachable)
	}

	// The granted list must report what the token can actually do, and a
	// CapRuntime-only scope must not claim more.
	granted, _ := report.Detail["granted_capabilities"].([]string)
	if len(granted) != 1 || granted[0] != string(mcp.CapRuntime) {
		t.Errorf("granted_capabilities = %v, want exactly [%s]", granted, mcp.CapRuntime)
	}

	// And a note must say so in words, because a caller reading the report is
	// looking for an explanation, not a subtraction.
	if !hasNoteContaining(report.Notes, "withheld_by_scope") {
		t.Errorf("no note explains the withheld tools: %v", report.Notes)
	}
}

// TestProbeWarnsAboutWorkspaceToolsInAnEmbed covers the setting with the worst
// consequence and no other warning anywhere.
//
// AllowWorkspaceTools means this process will chdir into and rewrite its own source
// tree while serving requests. Correct in a dev container, a serious
// misconfiguration in anything deployed — and nothing else in the process reports it.
//
// It requires ModeGenerator: StartInProcess refuses the combination with
// ModeAppRuntime, because in that mode the workspace tools are not registered at all
// and accepting the flag would silently do nothing. So both notes fire together, and
// both are asserted — they say different things, and a reader who set generator mode
// deliberately still needs to be told about the chdir.
func TestProbeWarnsAboutWorkspaceToolsInAnEmbed(t *testing.T) {
	startLiveApp(t, mcp.InProcessConfig{
		Mode:                mcp.ModeGenerator,
		AllowWorkspaceTools: true,
	})

	report, found := diag.Get("mcp")
	if !found {
		t.Fatal("no probe is registered")
	}

	if on, _ := report.Detail["workspace_tools"].(bool); !on {
		t.Error("workspace_tools = false after enabling AllowWorkspaceTools")
	}
	if mode, _ := report.Detail["mode"].(string); mode != string(mcp.ModeGenerator) {
		t.Errorf("mode = %q, want %q", mode, mcp.ModeGenerator)
	}

	if !hasNoteContaining(report.Notes, "AllowWorkspaceTools") {
		t.Errorf("no note mentions AllowWorkspaceTools: %v", report.Notes)
	}
	// The note has to say what the risk is, not merely that the flag is set.
	if !hasNoteContaining(report.Notes, "source") {
		t.Errorf("the workspace-tools note does not explain the consequence: %v", report.Notes)
	}
	if !hasNoteContaining(report.Notes, "generator mode") {
		t.Errorf("no note flags generator mode, which is the other half of this "+
			"configuration: %v", report.Notes)
	}

	// Generator mode registers the full toolchain, so the count must exceed the
	// app-runtime subset the other tests see.
	if total, _ := report.Detail["tools"].(int); total < 20 {
		t.Errorf("tools = %d in generator mode with workspace tools allowed; want the "+
			"full toolchain", total)
	}
}

// TestProbeWarnsAboutAnUnscopedToken is the note that fires on the default
// configuration, which is the one most deployments will have.
func TestProbeWarnsAboutAnUnscopedToken(t *testing.T) {
	startLiveApp(t, mcp.InProcessConfig{})

	report, found := diag.Get("mcp")
	if !found {
		t.Fatal("no probe is registered")
	}
	if !hasNoteContaining(report.Notes, "unscoped") {
		t.Errorf("no note mentions the unscoped token: %v", report.Notes)
	}
}

// hasNoteContaining reports whether any note mentions sub.
func hasNoteContaining(notes []string, sub string) bool {
	for _, n := range notes {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}

// TestDiagnoseServiceToolReadsTheEndpointsOwnProbe closes the loop.
//
// The three tests above read diag.Get directly, which proves the probe registers and
// says the right things. This one goes the way an agent does: over the control port,
// through a handshaken MCP session, calling breeze_diagnose_service against the
// application port, and finds the endpoint's own report in the result.
//
// It is the assertion that would catch the integration failing at any of the four
// joints between the probe and the agent — registration, the dashboard's
// /diagnostics endpoint, the tool's envelope decoding, and the JSON-RPC transport —
// none of which the direct-read tests exercise.
func TestDiagnoseServiceToolReadsTheEndpointsOwnProbe(t *testing.T) {
	app := startLiveApp(t, mcp.InProcessConfig{})

	session, status := handshake(t, app.controlURL, app.token)
	if status != http.StatusOK {
		t.Fatalf("the handshake returned %d", status)
	}

	resp := session.call(t, 9, "tools/call", map[string]any{
		"name": "breeze_diagnose_service",
		"arguments": map[string]any{
			// The app's port, not the control port: the call travels over the
			// control plane and is about the application.
			"service_url": app.appURL,
			"username":    testUser,
			"password":    testPassword,
		},
	})

	if errObj, failed := resp["error"]; failed {
		t.Fatalf("breeze_diagnose_service returned a protocol error: %v", errObj)
	}
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", resp)
	}
	if isError, _ := result["isError"].(bool); isError {
		t.Fatalf("breeze_diagnose_service reported a failure: %v", result["content"])
	}

	rendered := fmt.Sprint(result["structuredContent"])

	// The endpoint answering this very call must appear in its own report. Before
	// the probe existed, it was the one subsystem missing from the document.
	if !strings.Contains(rendered, "mcp") {
		t.Errorf("the diagnostics document does not mention the mcp subsystem:\n%s", rendered)
	}
	// And the two fields that make the report actionable have to survive the trip.
	for _, want := range []string{"reachable_tools", "app-runtime"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the mcp report reached the agent without %q:\n%s", want, rendered)
		}
	}
	// The dashboard's own probe too — a document with one subsystem would pass the
	// checks above and mean the registry was not really read.
	if !strings.Contains(rendered, "dashboard") {
		t.Errorf("the document does not mention the dashboard serving it:\n%s", rendered)
	}
}
