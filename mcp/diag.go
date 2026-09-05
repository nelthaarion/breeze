// diag.go — the embedded endpoint's own diagnostic probe.
//
// # Why this exists
//
// breeze_diagnose_service reads the diag registry and reports on every subsystem
// of a running service. Before this file it reported on every subsystem *except the
// one answering the call*: the MCP endpoint itself was the one thing invisible to
// the tool it serves.
//
// That gap is not cosmetic. The failures this probe reports are exactly the ones an
// agent cannot diagnose from the outside:
//
//   - **A scope withheld a tool.** The agent sees "no such tool" and cannot tell
//     whether the tool does not exist, the mode did not register it, or its own
//     token was narrowed. All three produce the same absence. This probe names
//     which layer, and it is the only place both layers are visible at once.
//   - **The mode is wrong for the deployment.** A ModeGenerator embed in a
//     container with no source tree registers 39 tools and every mutating one of
//     them fails at the first file operation. Nothing warns at startup, because
//     nothing at startup knows whether the source is there.
//   - **AllowWorkspaceTools is on in production.** That means this process will
//     chdir and rewrite its own tree while serving requests. It is a deliberate
//     setting for a dev container and a serious misconfiguration anywhere else, and
//     nothing else in the process reports it.
//
// # Why it registers here and not in internal/mcp
//
// internal/mcp is reachable from the standalone binary too, where "the embedded
// endpoint" is not a thing that exists — cmd/breeze-mcp *is* the server, and a probe
// claiming a subsystem of a process whose whole purpose is that subsystem is noise.
// This package is the embed, so this is where the fact is true.
//
// The probe is registered by StartInProcess rather than from an init, for the same
// reason the WebSocket hub's is: before the endpoint exists there is nothing to
// report, and a probe answering "off" for a feature the application never asked for
// is a row in every diagnostics read that no reader wants.

package mcp

import (
	"fmt"
	"sort"
	"strings"

	"github.com/nelthaarion/breeze/v2/diag"
	internalmcp "github.com/nelthaarion/breeze/v2/internal/mcp"
)

// diagName is the registry key.
//
// "mcp" rather than "mcp-embed" or "in-process-mcp": it is the name an agent will
// reach for, and the neighbouring key is already "auto-mcp", so the pair reads as
// the two endpoints the framework documents. `breeze add` has no mcp feature, so
// there is no feature name to match — see internal/mcp's diag_completeness_test.go,
// which lists this among the framework subsystems that exist without being added.
const diagName = "mcp"

// registerDiagnostics publishes the endpoint's probe.
//
// Called from StartInProcess with the server that was just bound. Registering
// replaces, so a process that starts a second endpoint reports the second one —
// which matches every other subsystem's rule and matches the wiring: the handle the
// application ends up using is the one it registered last.
func registerDiagnostics(s *Server, cfg InProcessConfig) {
	diag.Register(diagName, func() diag.Report { return probe(s, cfg) })
}

// probe reports the endpoint's address, mode, scope and tool count.
func probe(s *Server, cfg InProcessConfig) diag.Report {
	if s == nil || s.inner == nil {
		return diag.Off("no embedded MCP endpoint is running; start one with " +
			"mcp.StartInProcess(app, cfg)")
	}

	mode := s.inner.Kind()
	scope := s.inner.Scope()
	tools := internalmcp.ModeToolNames(mode)

	detail := map[string]any{
		"endpoint":              Endpoint,
		"mode":                  mode.String(),
		"version":               Version,
		"tools":                 len(tools),
		"scoped":                scope.IsScoped(),
		"granted_capabilities":  capabilityStrings(scope.Granted()),
		"workspace_tools":       cfg.AllowWorkspaceTools,
		"origin_check_disabled": originCheckDisabled(cfg.AllowedOrigins),
	}
	if addr := s.inner.Addr(); addr != nil {
		detail["address"] = addr.String()
	}

	// reachable is the count that answers "why can't I call X". Mode decides what is
	// registered; scope decides what the credential reaches. Reporting only the
	// first would make a scoped token's missing tools look like a build problem.
	reachable := 0
	var withheld []string
	for _, name := range tools {
		if scope.AllowsTool(name) {
			reachable++
			continue
		}
		withheld = append(withheld, name)
	}
	detail["reachable_tools"] = reachable
	if len(withheld) > 0 {
		sort.Strings(withheld)
		detail["withheld_by_scope"] = withheld
	}

	notes := endpointNotes(mode, scope, cfg, len(withheld))

	summary := fmt.Sprintf("MCP endpoint in %s mode, %d of %d tool(s) reachable",
		mode.String(), reachable, len(tools))
	if addr := s.inner.Addr(); addr != nil {
		summary = fmt.Sprintf("MCP endpoint on %s in %s mode, %d of %d tool(s) reachable",
			addr, mode.String(), reachable, len(tools))
	}

	// Degraded, not off: the endpoint is running and answering, and a caller whose
	// every tool is withheld has a working server it cannot use — which is a
	// configuration fault rather than an absent feature.
	if reachable == 0 {
		return diag.Degraded(summary+" — this token can call nothing", detail).
			WithNotes(notes...)
	}
	return diag.OK(summary, detail).WithNotes(notes...)
}

// endpointNotes are the things a reader should act on.
//
// Each one describes a state that produces no error anywhere else in the process,
// which is the criterion for being worth a note rather than a detail field.
func endpointNotes(mode ServerMode, scope Scope, cfg InProcessConfig, withheld int) []string {
	var notes []string

	if cfg.AllowWorkspaceTools {
		notes = append(notes, "AllowWorkspaceTools is on, so the generating and provisioning "+
			"tools are registered. Those chdir into and rewrite this application's source "+
			"tree while it is serving requests — every relative path the app resolves during "+
			"such a call resolves elsewhere. Correct for a development container, a serious "+
			"misconfiguration in anything deployed.")
	}

	if mode == ModeGenerator {
		notes = append(notes, "This endpoint is in generator mode, which registers the full "+
			"toolchain including tools that rewrite a source tree. A deployed binary was "+
			"built from a module cache and has no source on disk, so those tools fail at "+
			"their first file operation rather than being absent. Use ModeAppRuntime unless "+
			"this process genuinely owns a clone.")
	}

	if withheld > 0 {
		notes = append(notes, fmt.Sprintf("%d tool(s) are registered but withheld by this "+
			"token's scope, listed under withheld_by_scope. A client calling one gets the same "+
			"'no such tool' as for a tool that does not exist, so this is the only place the "+
			"distinction is visible.", withheld))
	}

	if !scope.IsScoped() {
		notes = append(notes, "The token is unscoped, so one credential reaches live logs, "+
			"traces, simulated requests and the OpenAPI document alike. Narrow it with "+
			"mcp.NewScope(...) — mode is a property of the deployment, scope is a property of "+
			"the credential.")
	}

	if originCheckDisabled(cfg.AllowedOrigins) {
		notes = append(notes, "AllowedOrigins contains \"*\", which disables the Origin check "+
			"entirely. That leaves the bearer token as the only guard, and makes a browser on "+
			"any page able to reach this endpoint if it learns the token.")
	}

	if host := strings.TrimSpace(cfg.Host); host != "" && !isLoopbackHost(host) {
		notes = append(notes, fmt.Sprintf("Bound to %q rather than loopback, so this control "+
			"plane is reachable from the network and the token is the only thing standing in "+
			"front of it.", host))
	}

	return notes
}

// capabilityStrings renders capabilities for the JSON detail.
//
// diag.Report.Detail must hold JSON-encodable values, and Capability is a named
// string type that encodes correctly today — converting explicitly means a future
// change to its underlying type cannot silently alter this endpoint's output.
func capabilityStrings(caps []Capability) []string {
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		out = append(out, string(c))
	}
	return out
}

// originCheckDisabled reports whether the allowlist contains the wildcard.
func originCheckDisabled(origins []string) bool {
	for _, o := range origins {
		if strings.TrimSpace(o) == "*" {
			return true
		}
	}
	return false
}

// isLoopbackHost reports whether a bind host keeps the endpoint off the network.
//
// A string comparison rather than a net.IP check because this is the value the
// caller configured, not a resolved address, and the three spellings below are what
// a configuration actually contains. Anything else is treated as network-reachable,
// which is the safe direction for a note that warns.
func isLoopbackHost(host string) bool {
	switch host {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}
