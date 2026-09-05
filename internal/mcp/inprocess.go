package mcp

// inprocess.go — the safe-subset server a live application embeds.
//
// This is the internal half: it builds a Server carrying only the tools scope.go
// classifies as in-process safe, and hands it to the same NetworkServer the
// standalone binary uses. The exported wrapper a generated project actually calls
// lives in the root-level mcp package, because internal/ cannot be imported from
// another module and because that package is the only one allowed to know about
// *breeze.Breeze — see the note in mcp_server.go about why this package must not
// import the root one.
//
// # What is dropped, and how
//
// The full registry is built first and then filtered, rather than registering a
// different set. That ordering is deliberate: a tool must exist before it can be
// classified, so building everything means a tool added without a scope entry is
// caught by a test comparing the registry against the table, instead of quietly
// never appearing. Filtering after the fact costs one map walk at startup.
//
// # The opt-in
//
// AllowWorkspaceTools restores the excluded set. It exists because a development
// container — where the app really is running from its own clone, with a source
// tree present and no production traffic — is a legitimate case, and refusing it
// outright would push people to run a second process against the same directory,
// which is the race this is trying to avoid rather than a fix for it.
//
// It is off by default and it is not a convenience. Turning it on means the
// application's own process will chdir and rewrite its own source tree while
// serving requests, and every relative path the app resolves during that window
// resolves somewhere else.

import (
	"fmt"
	"sort"
)

// NewInProcessServer returns a Server carrying only the tools that are safe to
// serve from inside a running application.
//
// allowWorkspace restores the workspace-mutating tools. Callers should leave it
// false; see the file comment and the documentation on the exported wrapper.
func NewInProcessServer(version string, allowWorkspace bool) *Server {
	s := NewServer(version)
	if allowWorkspace {
		return s
	}

	for name := range s.tools {
		scope, classified := scopeOf(name)
		// An unclassified tool is dropped rather than admitted. It is a bug either
		// way — a test fails on it — but the failure mode of dropping is a missing
		// capability, and of admitting is an unreviewed tool with filesystem access
		// inside a production process.
		if !classified || scope != scopeInProcess {
			delete(s.tools, name)
		}
	}
	s.sortTools()
	return s
}

// InProcessToolNames returns the tools an in-process server serves by default,
// sorted.
//
// Exported so the wrapper package can report the inventory without re-deriving
// the classification, and so a test elsewhere can assert on it.
func InProcessToolNames() []string { return namesWithScope(scopeInProcess) }

// WorkspaceOnlyToolNames returns the tools excluded from in-process mode by
// default, sorted.
//
// Exported for the same reason, and because a refusal that names what it refused
// is worth more than one that does not: the wrapper uses this to explain the
// omission when a caller asks for a tool that is not there.
func WorkspaceOnlyToolNames() []string { return namesWithScope(scopeWorkspace) }

func namesWithScope(want toolScope) []string {
	out := make([]string, 0, len(toolScopes))
	for name, scope := range toolScopes {
		if scope == want {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// InProcessConfig is how an embedded MCP endpoint is configured.
//
// It is a distinct type from NetworkConfig rather than an alias, because the two
// have different defaults to explain and because a field added to one should not
// silently appear in the other. The security posture is identical, and that is
// enforced by construction below rather than by repeating it.
type InProcessConfig struct {
	// Mode is the server kind. Required, with no default.
	//
	// An embedded endpoint is almost always ModeAppRuntime — that is the case it
	// exists for — but it is not defaulted, because a development container running
	// from its own clone is a legitimate ModeGenerator embed, and silently choosing
	// for the caller would mean one of those two setups is wrong without saying so.
	Mode ServerMode

	// Port is the control port. Required: there is no sensible default, and a
	// zero here would bind something arbitrary that no client configuration names.
	Port int

	// Host is the bind address. Empty means loopback, exactly as in standalone
	// network mode.
	Host string

	// Token is the bearer token. Empty means one is generated and returned, so a
	// process that did not supply one can still print it once.
	Token string

	// AllowedOrigins extends the Origin allowlist. Same meaning, same "*" escape
	// hatch, same risk as standalone.
	AllowedOrigins []string

	// AllowWorkspaceTools restores the workspace-mutating tools. See the file
	// comment. Almost every deployed application should leave this false.
	AllowWorkspaceTools bool

	// Scope is what the endpoint's token may do. The zero value is unscoped, so an
	// existing embed keeps behaving exactly as it did.
	//
	// Worth setting even here, where mode has already removed the mutating tools:
	// mode decides what a server offers, scope decides what a credential reaches, and
	// an app-runtime embed handing out one token that reads logs, traces and simulated
	// requests alike is still broader than most callers need.
	Scope Scope
}

// network converts an in-process configuration to the transport's own.
//
// One function, so the two modes cannot drift apart on defaults. If loopback ever
// stops being the default for one of them, it stops for both.
func (c InProcessConfig) network() NetworkConfig {
	return NetworkConfig{
		Mode:           c.Mode,
		Host:           c.Host,
		Port:           c.Port,
		Token:          c.Token,
		Scope:          c.Scope,
		AllowedOrigins: c.AllowedOrigins,
	}
}

// NewInProcess builds a listening-ready in-process endpoint and reports the token
// in force.
//
// It returns before serving so the caller can print the token and learn the bound
// address, and so a bind failure is a synchronous error rather than something a
// goroutine logs after the application has already started answering traffic.
func NewInProcess(version string, cfg InProcessConfig) (*NetworkServer, string, error) {
	if err := cfg.Mode.validate(); err != nil {
		return nil, "", err
	}
	if cfg.Port <= 0 {
		return nil, "", fmt.Errorf("mcp: an in-process endpoint needs an explicit port; " +
			"it is the control address a client configuration names")
	}

	// Built through NewServerForMode rather than NewInProcessServer so that an
	// app-runtime embed gets the structural guarantee: the mutating tools are never
	// registered, so no scope check stands between an agent and a generator because
	// there is no generator to reach.
	inner, err := NewServerForMode(version, cfg.Mode)
	if err != nil {
		return nil, "", err
	}
	// AllowWorkspaceTools only means anything for a generator-mode embed. In
	// app-runtime it is not merely ignored — honouring it would defeat the whole
	// point of the mode — so it is refused loudly rather than silently dropped.
	if cfg.AllowWorkspaceTools && cfg.Mode == ModeAppRuntime {
		return nil, "", fmt.Errorf("mcp: AllowWorkspaceTools cannot be set on a %q server; "+
			"the workspace tools are not registered in this mode at all. Use Mode: %q if this "+
			"process really does own a source tree", ModeAppRuntime, ModeGenerator)
	}

	server, token, err := NewNetworkServer(inner, cfg.network())
	if err != nil {
		return nil, "", err
	}
	if err := server.Listen(cfg.network()); err != nil {
		return nil, "", err
	}
	return server, token, nil
}
