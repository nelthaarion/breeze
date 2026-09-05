package mcp

// mode.go — which kind of MCP server this is, declared rather than inferred.
//
// # Why there are two kinds
//
// Breeze serves MCP for two unrelated purposes, and conflating them is a security
// problem rather than a naming inconvenience:
//
//   - A generator-level server helps build a project. It scaffolds, edits, runs the
//     toolchain, and provisions containers. It belongs on a developer's machine or
//     in a build agent, pointed at a source tree.
//   - An app-runtime server helps understand a running instance. It reads live
//     routes, recent errors, logs, traces, performance. It belongs inside a
//     deployed process, and must not be able to rewrite that process's own source
//     or start siblings of itself.
//
// The tool sets are not merely configured differently. An app-runtime server has no
// mutating tool *registered at all* — see NewServerForMode — so no token scope, no
// configuration mistake and no argument-injection trick can reach one. Part 8's
// per-token scoping is a second layer on top of that, not the only one.
//
// # Why there is no default
//
// A default would have to be one of the two, and both choices are wrong:
//
//   - Defaulting to generator means a deployed application that forgot the flag
//     silently exposes project generation and Docker provisioning to whoever holds
//     its token. That is a privilege escalation nobody chose.
//   - Defaulting to app-runtime means a developer's server silently lacks the tools
//     they are trying to use, and the symptom is "unknown tool" for a tool the
//     documentation says exists — which reads as a broken build, not a
//     misconfiguration.
//
// The first is dangerous and the second is confusing, so construction fails and
// says which value to pass.

import (
	"fmt"
	"sort"
	"strings"
)

// ServerMode is the kind of MCP server: generator-level or app-runtime.
//
// A string rather than an int so it survives a JSON round trip into the handshake
// unchanged, and so a configuration file names it in the same words the
// documentation and the --mode flag do.
type ServerMode string

const (
	// ModeGenerator serves the full toolchain: generation, planning, verification,
	// provisioning, plus every read-only tool.
	ModeGenerator ServerMode = "generator"

	// ModeAppRuntime serves only the tools that read live state from a running
	// application. Nothing that writes a file, chdirs, or drives Docker is
	// registered.
	ModeAppRuntime ServerMode = "app-runtime"

	// ModeUnset is the zero value, and is never valid. Named so an error message
	// and a test can refer to it without spelling the empty string.
	ModeUnset ServerMode = ""
)

// KnownModes lists the valid values, sorted, for error messages and usage text.
func KnownModes() []string {
	return []string{string(ModeAppRuntime), string(ModeGenerator)}
}

// modeRequired is the one sentence every "you did not pick a mode" failure uses.
//
// Shared so a caller who set the struct field programmatically and a caller who
// forgot the flag read the same explanation, including which value does what.
func modeRequired(what string) error {
	return fmt.Errorf("mcp: %s is required and has no default; pass one of %s "+
		"(%q to build and change a project, %q to inspect a running instance)",
		what, strings.Join(KnownModes(), ", "), ModeGenerator, ModeAppRuntime)
}

// ParseMode converts a command-line or configuration value to a ServerMode.
//
// It refuses the empty string, because to a caller an empty and an unknown value
// are the same mistake: no mode was successfully specified.
func ParseMode(raw string) (ServerMode, error) {
	switch ServerMode(strings.TrimSpace(raw)) {
	case ModeGenerator:
		return ModeGenerator, nil
	case ModeAppRuntime:
		return ModeAppRuntime, nil
	case ModeUnset:
		return ModeUnset, modeRequired("--mode")
	default:
		return ModeUnset, fmt.Errorf("mcp: unknown mode %q; valid values are %s",
			raw, strings.Join(KnownModes(), ", "))
	}
}

// validate reports whether m is one of the two valid modes. Called by every
// construction path.
func (m ServerMode) validate() error {
	switch m {
	case ModeGenerator, ModeAppRuntime:
		return nil
	case ModeUnset:
		return modeRequired("Mode")
	default:
		return fmt.Errorf("mcp: unknown mode %q; valid values are %s",
			string(m), strings.Join(KnownModes(), ", "))
	}
}

// String makes ServerMode printable in a diagnostic without a conversion.
func (m ServerMode) String() string { return string(m) }

// mutating reports whether a tool changes something outside this process.
//
// Derived from scope.go rather than from a second list: a workspace-scoped tool is
// exactly one that writes files, chdirs, or drives the Go toolchain or Docker. One
// table, so a tool added later cannot be mutating in one place's opinion and
// read-only in another's.
func mutating(name string) bool {
	scope, classified := scopeOf(name)
	// An unclassified tool counts as mutating. That is the safe direction: the
	// consequence is a missing capability in app-runtime mode, which a test catches,
	// rather than an unreviewed tool with filesystem access inside a live process.
	return !classified || scope != scopeInProcess
}

// NewServerForMode builds a Server carrying only the tools its mode permits.
//
// This is the structural half of Part 9. app-runtime does not filter at call time
// or consult a scope before dispatch: the tools are absent from s.tools, so
// tools/list cannot advertise them and tools/call reports an unknown name. There is
// no code path from an app-runtime server to a generator.
func NewServerForMode(version string, mode ServerMode) (*Server, error) {
	if err := mode.validate(); err != nil {
		return nil, err
	}

	s := NewServer(version)
	s.mode = mode

	if mode == ModeAppRuntime {
		for name := range s.tools {
			if mutating(name) {
				delete(s.tools, name)
			}
		}
		s.sortTools()
	}
	return s, nil
}

// ModeToolNames reports the tools a mode serves, sorted.
//
// Exported so a startup diagnostic and a test can ask the question without building
// a server, and so the answer comes from the same filter the server uses.
func ModeToolNames(mode ServerMode) []string {
	all := NewServer("(inventory)")
	out := make([]string, 0, len(all.tools))
	for name := range all.tools {
		if mode == ModeAppRuntime && mutating(name) {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
