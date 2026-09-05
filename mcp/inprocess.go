// Package mcp embeds a read-only MCP control endpoint in a running Breeze
// application.
//
// One process, two servers: the application serves its own traffic, and this
// serves an agent read-only introspection of what the application is doing.
//
//	app := breeze.New(router, pool)
//	go mcp.ServeInProcess(app, mcp.InProcessConfig{
//	    Port:  2000,
//	    Token: os.Getenv("BREEZE_MCP_TOKEN"),
//	})
//	app.Run(3000, true)
//
// # This is not Auto-MCP
//
// Breeze has two MCP endpoints and they answer different questions. Choosing
// between them is a design decision, and a project may legitimately want both:
//
//	app.EnableMCP(":2001")        // Auto-MCP: my tagged routes, as tools
//	mcp.ServeInProcess(app, cfg)  // this: read-only introspection of this instance
//
// Auto-MCP (breeze.Breeze.EnableMCP, breeze.MCPTool) exposes the application's own
// routes — "place an order", "look up a customer" — as tools an agent can invoke.
// The tool list is whatever the author tagged. It is part of the application's
// public surface and it performs business operations.
//
// ServeInProcess exposes none of that. It serves a fixed, framework-provided set
// of introspection tools — live route statistics, recent errors, logs, traces,
// performance — pointed at this instance. It is a window into a running service,
// not a way to call it.
//
// They must not share a port. ServeInProcess refuses to start on the address
// EnableMCP took, because two MCP servers on one port would each answer with the
// wrong tool table and the symptom — "no such tool" — would implicate neither.
//
// # Why the tool set is a subset
//
// The standalone breeze-mcp binary serves 39 tools, most of which generate or
// modify a project. Those are excluded here, for two independent reasons:
//
//   - They chdir and replace os.Stdout under a process-wide lock. Inside a live
//     application that changes the working directory of the server currently
//     serving requests, so every relative path the app resolves during the call —
//     a static root, a template directory, a log file — resolves elsewhere.
//   - They need a source tree. A deployed binary was built from a module cache,
//     not a clone; its own source is not on disk, so those tools would have
//     nothing to operate on.
//
// InProcessConfig.AllowWorkspaceTools restores them. Read its documentation before
// setting it; a deployed application should not.
//
// # Security
//
// Identical to the standalone network mode, by construction rather than by
// convention: a bearer token is required on every request including the handshake,
// the Origin header is validated, and the bind is loopback unless Host says
// otherwise. Embedding relaxes none of it.
package mcp

import (
	"fmt"
	"net"
	"strings"

	"github.com/nelthaarion/breeze"
	"github.com/nelthaarion/breeze/diag"
	internalmcp "github.com/nelthaarion/breeze/internal/mcp"
)

// Version is reported in the MCP handshake as this endpoint's server version.
//
// A variable rather than a constant so an application can set it to its own
// release identifier, which is what an operator reading a client's server list
// actually wants to see. The default is honest about being unset.
var Version = "(embedded)"

// ServerMode is the kind of MCP server: generator-level or app-runtime.
//
// Re-exported from internal/mcp so an application configuring an embedded endpoint
// names the mode without importing an internal package — which it cannot do from
// another module anyway.
type ServerMode = internalmcp.ServerMode

// The two valid modes. There is no third, and no zero value that means anything.
//
// ModeAppRuntime is what a deployed application wants: read-only introspection of
// itself, with the generating and provisioning tools not registered at all.
// ModeGenerator is the full toolchain, appropriate only where this process really
// does own a source tree it may rewrite.
const (
	ModeGenerator  = internalmcp.ModeGenerator
	ModeAppRuntime = internalmcp.ModeAppRuntime
)

// Endpoint is the URL path the MCP endpoint is served on. Fixed, so every client
// configuration can name the same URL.

// Scope is the set of capabilities a token may use.
//
// An alias rather than a wrapper type: a caller builds one with NewScope and hands it
// straight to InProcessConfig, and a distinct type here would mean converting between
// two identical things for no benefit.
type Scope = internalmcp.Scope

// Capability is one category of tool a scope may grant.
type Capability = internalmcp.Capability

// The capability categories, re-exported so an application can name them without
// importing an internal package.
//
// Only four of the eight are reachable on an app-runtime embed — the other four
// classify tools that mode does not register. They are re-exported anyway, because a
// development-container embed running in ModeGenerator can use them, and because a
// scope that names a capability the server has no tools for is harmless: it grants
// nothing and says so in the handshake.
const (
	CapGeneration    = internalmcp.CapGeneration
	CapIntrospection = internalmcp.CapIntrospection
	CapPlanning      = internalmcp.CapPlanning
	CapKnowledge     = internalmcp.CapKnowledge
	CapVerification  = internalmcp.CapVerification
	CapRuntime       = internalmcp.CapRuntime
	CapFleet         = internalmcp.CapFleet
	CapProvisioning  = internalmcp.CapProvisioning
)

// NewScope builds a scope granting exactly the given capabilities.
//
// It returns an error on an empty list rather than treating it as "everything": a
// caller who passes no capabilities has almost certainly built the list dynamically
// and got nothing, and silently granting full access there is the worst possible
// interpretation. Use the zero Scope, or UnscopedScope, to mean unscoped deliberately.
func NewScope(caps ...Capability) (Scope, error) { return internalmcp.NewScope(caps...) }

// UnscopedScope is a token with every capability, spelled as a decision.
func UnscopedScope() Scope { return internalmcp.UnscopedScope() }

// ParseScope builds a scope from a comma-separated string, as a flag or an
// environment variable would carry it. An empty value means unscoped.
func ParseScope(raw string) (Scope, error) { return internalmcp.ParseScope(raw) }

// Capabilities lists every capability category, sorted — the same set the handshake
// reports as `known`.
func Capabilities() []Capability { return internalmcp.KnownCapabilities() }

// InProcessConfig configures an embedded endpoint.
type InProcessConfig struct {
	// Mode is the server kind, and it is required with no default.
	//
	// Use ModeAppRuntime for a deployed application: that is what this feature is
	// for, and it is the value that makes the mutating tools structurally absent
	// rather than merely filtered. ModeGenerator is for a development container that
	// genuinely owns its own source tree.
	//
	// There is deliberately no default. Defaulting to app-runtime would silently
	// remove tools a development embed was relying on; defaulting to generator would
	// silently give a deployed process the ability to rewrite itself and provision
	// containers. Neither is a choice this package may make on a caller's behalf.
	Mode ServerMode

	// Port is the control port. Required, and never the port the application itself
	// listens on: this is a control address, the app's is an app address, and
	// conflating them is the mistake this whole feature is documented against.
	Port int

	// Host is the bind address. Empty means loopback only, exactly as in the
	// standalone binary. Widening it exposes an authenticated control plane to the
	// network, where the token is then the only guard.
	Host string

	// Token is the bearer token required on every request. Empty means one is
	// generated and returned, so a caller that did not supply one can still log it
	// once.
	//
	// Reading it from the environment is the usual choice, and it is what the
	// standalone binary and the provisioning orchestrator both do — so one
	// deployment can configure an app's embedded endpoint and a standalone instance
	// the same way.
	Token string

	// AllowedOrigins extends the Origin allowlist beyond loopback. A single "*"
	// disables the check, which is a deliberate act for a reverse-proxy deployment
	// and not something to set by default.
	AllowedOrigins []string

	// AllowWorkspaceTools restores the generation, planning, verification and
	// provisioning tools.
	//
	// Leave this false in anything deployed. Setting it means this application's own
	// process will chdir into and rewrite its own source tree at runtime, while it
	// is serving requests: for the duration of such a call, every relative path the
	// application resolves points somewhere else. It is intended for a development
	// container where the app runs from its own clone and serves no real traffic.
	//
	// A binary installed with `go get`, or built in a multi-stage image, has no
	// source tree at all — so enabling this there produces tools that fail rather
	// than tools that work.
	AllowWorkspaceTools bool

	// Scope narrows what this endpoint's token may do. The zero value grants every
	// capability, so an existing embed is unaffected.
	//
	// Worth setting even in ModeAppRuntime, where the mutating tools are already
	// absent. Mode is a property of the deployment; scope is a property of the
	// credential. An app-runtime embed with an unscoped token still hands one caller
	// live logs, traces, simulated requests and the OpenAPI document alike, and a CI
	// job that only reads traces has no use for the rest:
	//
	//	scope, err := mcp.NewScope(mcp.CapFleet)
	//	if err != nil { return err }
	//	cfg.Scope = scope
	//
	// The endpoint reports what it granted in the MCP handshake, and at
	// GET /mcp/features, so a caller never has to guess which of the two layers
	// withheld a tool.
	Scope Scope
}

// Server is a running embedded endpoint.
type Server struct {
	inner *internalmcp.NetworkServer

	// token is kept so Token() can report it to a caller that started the server
	// and then wants to log it.
	token string
}

const Endpoint = internalmcp.DefaultEndpointPath

// ServeInProcess starts an embedded MCP endpoint and serves it until Close.
//
// It is meant to be called in a goroutine beside app.Run. Binding happens before
// serving begins, so a port conflict surfaces as this function's return value
// rather than as an endpoint that silently never came up — but in the
// `go mcp.ServeInProcess(...)` form that return value is discarded. Use
// StartInProcess when the error matters, which in anything deployed it does.
func ServeInProcess(app *breeze.Breeze, cfg InProcessConfig) error {
	server, _, err := StartInProcess(app, cfg)
	if err != nil {
		return err
	}
	return server.Serve()
}

// StartInProcess binds an embedded endpoint and returns it without serving,
// together with the token in force.
//
// This is the form to prefer at startup: the bind has already succeeded or failed
// by the time it returns, the token can be logged once, and Serve is then the
// thing that goes in a goroutine.
func StartInProcess(app *breeze.Breeze, cfg InProcessConfig) (*Server, string, error) {
	if app == nil {
		return nil, "", fmt.Errorf("breeze/mcp: an in-process endpoint needs the application it describes")
	}
	if err := checkPortConflict(app, cfg); err != nil {
		return nil, "", err
	}

	// Counted diagnostics come on with an embedded endpoint, for the same reason
	// they come on with the dashboard: a process that went to the trouble of
	// exposing an inspection endpoint has already accepted the cost of being
	// inspectable, and breeze_diagnose_service reporting counting:false on every
	// counter-backed subsystem would make the tool's most useful numbers
	// permanently zero. Idempotent, so an application that installed the
	// dashboard too pays nothing extra here.
	//
	// This is also the only path that turns them on for a deployment with no
	// dashboard, which is the normal shape for an app-runtime embed.
	diag.EnableCounters()

	inner, token, err := internalmcp.NewInProcess(Version, internalmcp.InProcessConfig{
		Mode:                cfg.Mode,
		Port:                cfg.Port,
		Host:                cfg.Host,
		Token:               cfg.Token,
		AllowedOrigins:      cfg.AllowedOrigins,
		AllowWorkspaceTools: cfg.AllowWorkspaceTools,
		Scope:               cfg.Scope,
	})
	if err != nil {
		return nil, "", err
	}
	server := &Server{inner: inner, token: token}

	// Registered here rather than from an init: before the endpoint is bound there
	// is nothing to report, and a probe answering "off" for a feature the
	// application never asked for is a row in every diagnostics read that no reader
	// wants. See diag.go for what it reports and why each note is there.
	registerDiagnostics(server, cfg)

	return server, token, nil
}

// checkPortConflict refuses a port Auto-MCP already took.
//
// Only Auto-MCP can be checked from here: it records its address on the
// application, so the collision is knowable. The application's own HTTP port is
// not — app.Run takes it later, and this may run before that — so the operating
// system reports that one, as a bind failure naming the port, which is a clear
// enough answer.
func checkPortConflict(app *breeze.Breeze, cfg InProcessConfig) error {
	autoAddr := strings.TrimSpace(app.AutoMCPAddr())
	if autoAddr == "" {
		return nil
	}

	_, autoPort, err := net.SplitHostPort(autoAddr)
	if err != nil {
		// An address this cannot parse is not evidence of a conflict, and refusing on
		// it would break a legitimate setup over a formatting difference.
		return nil
	}
	if autoPort != fmt.Sprint(cfg.Port) {
		return nil
	}

	return fmt.Errorf("breeze/mcp: port %d is already serving Auto-MCP (EnableMCP(%q)). These are "+
		"different endpoints with different tool sets — Auto-MCP exposes this application's "+
		"MCPTool-tagged routes, this exposes read-only introspection of the running instance — so "+
		"they need separate ports", cfg.Port, autoAddr)
}

// Serve serves until Close. A closed listener is a clean stop, not a failure.
func (s *Server) Serve() error { return s.inner.Serve() }

// Close stops serving.
func (s *Server) Close() error { return s.inner.Close() }

// Addr reports the bound address, which is how a caller that asked for port 0
// learns the real one.
func (s *Server) Addr() net.Addr { return s.inner.Addr() }

// Token reports the bearer token in force, generated or supplied.
func (s *Server) Token() string { return s.token }

// URL is the full control-plane URL a client configuration names.
func (s *Server) URL() string {
	if addr := s.inner.Addr(); addr != nil {
		return fmt.Sprintf("http://%s%s", addr, Endpoint)
	}
	return ""
}

// Tools reports the tools this endpoint serves, sorted.
//
// Exported because "which tools does my embedded endpoint actually have" is worth
// being able to answer from the application itself, at startup, rather than by
// making a tools/list call.
func Tools() []string { return internalmcp.InProcessToolNames() }

// ExcludedTools reports the tools in-process mode leaves out by default, sorted.
//
// The counterpart to Tools: an omission that can be enumerated is a decision, and
// one that cannot is a surprise.
func ExcludedTools() []string { return internalmcp.WorkspaceOnlyToolNames() }
