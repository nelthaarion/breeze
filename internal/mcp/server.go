package mcp

// server.go — the MCP method vocabulary, registered on an rpc.Registry.
//
// There are only three methods, and two of them are pure data. The interesting
// one is tools/call, which is a second dispatch layer: JSON-RPC routes on
// "method", MCP routes on params.name. That is MCP's design, not a choice made
// here, and it is why tool lookup below reports -32602 rather than -32601 for an
// unknown tool — the method (tools/call) exists and was found; it is the argument
// that is wrong.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/nelthaarion/breeze/v2/rpc"
)

// Server holds the tool table and the JSON-RPC registry the tools are reached
// through.
type Server struct {
	reg   *rpc.Registry
	tools map[string]*tool
	// order is the tool names, sorted, so tools/list is stable. An unstable
	// listing is not wrong on the wire but it makes diffs and test fixtures
	// worthless.
	order []string
	// version is reported in the handshake.
	version string
	// mode is the server kind, reported in the handshake as breezeServerKind and
	// used by NewServerForMode to decide what gets registered at all. Set by that
	// constructor; NewServer leaves it unset because NewServer is the internal
	// "everything" builder that every mode filters down from.
	mode ServerMode

	// scope is the capability set of the token this server answers for.
	//
	// A field on the Server rather than a per-request value because a token is minted
	// with its scope and never changes it: one Server serves one credential, which is
	// what makes the initialize snapshot accurate for the whole session. A future
	// multi-token server would carry this per connection instead, and the handshake
	// would then have to be built per connection too — the comment in protocol.go on
	// BreezeCapabilities says why that is not needed today.
	scope Scope

	// logger receives one Event per dispatched call, or nil. See observe.go for why the
	// events carry no argument values.
	logger Logger
}

// tool is one callable.
type tool struct {
	name        string
	description string
	// schema is JSON Schema for arguments, pre-marshalled: it is constant per
	// tool, so building it per tools/list call would be waste.
	schema json.RawMessage
	// run executes the tool. It returns a result rather than writing to a
	// context so that a tool cannot accidentally emit a malformed response;
	// shaping the response is this file's job.
	run func(args json.RawMessage) toolCallResult
}

// NewServer returns a Server with the standard toolset registered.
func NewServer(version string) *Server {
	s := &Server{
		reg:     rpc.NewRegistry(),
		tools:   make(map[string]*tool),
		version: version,
	}

	s.registerMethods()
	registerGeneratorTools(s)
	registerIntrospectionTools(s)
	registerPlanningTools(s)
	registerChangeSetTools(s)
	registerKnowledgeTools(s)
	registerVerificationTools(s)
	registerSimulationTools(s)
	registerLiveTools(s)
	registerFleetTools(s)
	registerProvisioningTools(s)

	s.sortTools()

	return s
}

// Registry exposes the JSON-RPC registry, so a caller can build whichever
// transport it wants around it.
func (s *Server) Registry() *rpc.Registry { return s.reg }

// RPCServer returns a dispatcher over this server's methods.
func (s *Server) RPCServer() *rpc.Server { return rpc.NewServer(s.reg) }

// addTool registers a callable. It panics on a duplicate name because that can
// only be a programming error in this package, and a silently-shadowed tool
// would be found much later, by a caller wondering why its arguments are ignored.
func (s *Server) addTool(t *tool) {
	if _, dup := s.tools[t.name]; dup {
		panic("mcp: duplicate tool " + t.name)
	}
	s.tools[t.name] = t
}

func (s *Server) sortTools() {
	s.order = make([]string, 0, len(s.tools))
	for name := range s.tools {
		s.order = append(s.order, name)
	}
	sort.Strings(s.order)
}

// visibleNames is the tools this server's token may call, sorted.
//
// Separate from order because order is the registry and this is what a caller may
// see. Used in the unknown-tool message so a refusal never advertises a capability
// the caller does not have.
func (s *Server) visibleNames() []string {
	if !s.scope.IsScoped() {
		return s.order
	}
	out := make([]string, 0, len(s.order))
	for _, name := range s.order {
		if s.scope.AllowsTool(name) {
			out = append(out, name)
		}
	}
	return out
}

// SetScope narrows what this server's token may call.
//
// A setter rather than a NewServer parameter because scope is a property of the
// credential the transport authenticated, and the transport is constructed after the
// Server. NewNetworkServer calls this from its own configuration, so the two cannot
// disagree about which token they are serving.
func (s *Server) SetScope(scope Scope) { s.scope = scope }

// Scope reports the capability set in force, so a transport can answer a question
// about permissions without a second copy of the state.
func (s *Server) Scope() Scope { return s.scope }

// registerMethods wires the three MCP methods.
//
// All three are registered as blocking. The generators touch the filesystem and
// chdir, and capture.go serialises them anyway; handing them to a worker pool
// would add a hop without adding any concurrency.
func (s *Server) registerMethods() {
	s.reg.RegisterBlocking("initialize", s.handleInitialize)
	s.reg.RegisterBlocking("tools/list", s.handleToolsList)
	s.reg.RegisterBlocking("tools/call", s.handleToolsCall)

	// notifications/initialized is sent by the client after the handshake. It is
	// a notification, so it has no id and gets no response — but it must still be
	// registered, because an unregistered method is -32601, and a client that
	// receives an error for its own notification will often abandon the session.
	//
	// The handler is empty because there is genuinely nothing to do: the
	// handshake carries no state worth keeping.
	s.reg.RegisterBlocking("notifications/initialized", func(ctx *rpc.Context) {})
}

// handleInitialize answers the handshake.
func (s *Server) handleInitialize(ctx *rpc.Context) {
	s.emit(Event{Kind: EventHandshake})
	ctx.Result(initializeResult{
		ProtocolVersion: protocolVersion,
		Capabilities:    serverCapabilities{},
		ServerInfo: serverInfo{
			Name:    serverName,
			Version: s.version,
		},
		BreezeServerKind:   s.mode,
		BreezeCapabilities: s.capabilityReport(),
	})
}

// capabilityReport builds the capability half of the handshake.
//
// It reports the effective scope intersected with what this server actually has: an
// app-runtime server whose token grants "provisioning" does not claim provisioning,
// because there is no provisioning tool registered to reach. Reporting the raw token
// scope would tell an agent it may do something that will then fail as an unknown
// tool, which is the least useful order to learn those two facts in.
func (s *Server) capabilityReport() *capabilityReport {
	present := map[Capability]bool{}
	for name := range s.tools {
		if c, ok := capabilityOf(name); ok {
			present[c] = true
		}
	}

	granted := make([]Capability, 0, len(present))
	for _, c := range s.scope.Granted() {
		if present[c] {
			granted = append(granted, c)
		}
	}

	return &capabilityReport{
		Granted: capabilityNames(granted),
		Known:   capabilityNames(KnownCapabilities()),
		Scoped:  s.scope.IsScoped(),
	}
}

// handleToolsList enumerates the tools.
//
// Filtered by the connection's scope, which is what actually enforces it: the
// initialize payload tells an agent what to expect, this decides what it can see. A
// tool withheld here is simply not advertised, so a well-behaved agent never attempts
// it — and handleToolsCall refuses it anyway for one that does.
func (s *Server) handleToolsList(ctx *rpc.Context) {
	list := make([]toolDescriptor, 0, len(s.order))
	for _, name := range s.order {
		if !s.scope.AllowsTool(name) {
			continue
		}
		t := s.tools[name]
		list = append(list, toolDescriptor{
			Name:        t.name,
			Description: t.description,
			InputSchema: t.schema,
		})
	}
	ctx.Result(toolsListResult{Tools: list})
}

// handleToolsCall runs one tool.
func (s *Server) handleToolsCall(ctx *rpc.Context) {
	var p toolCallParams
	if err := ctx.Bind(&p); err != nil {
		ctx.Errorf(rpc.CodeInvalidParams, "tools/call params must be an object with a name")
		return
	}

	if p.Name == "" {
		ctx.Errorf(rpc.CodeInvalidParams, "tools/call requires a tool name")
		return
	}

	t, ok := s.tools[p.Name]
	if !ok {
		// -32602, not -32601: tools/call was found and dispatched correctly. The
		// name inside its params is what does not resolve.
		//
		// The known names are listed because the caller is often a model that
		// guessed, and a list turns a dead end into a correction. Only the visible
		// ones: listing a tool the caller cannot call would be an invitation to
		// retry something that will be refused identically.
		s.emit(Event{Kind: EventToolUnknown, Tool: p.Name})
		ctx.Errorf(rpc.CodeInvalidParams,
			fmt.Sprintf("unknown tool %q; available: %s", p.Name, strings.Join(s.visibleNames(), ", ")))
		return
	}

	// Out of scope is refused as a structured tool result rather than a JSON-RPC
	// error, because the request was well-formed and the tool does exist: the answer
	// is "no", which is an outcome a model can read and act on. A -32602 would say
	// the call was malformed, which is a different and misleading thing.
	//
	// Checked here as well as in tools/list because a client may have cached a
	// listing, or may be calling a name it read in documentation.
	if !s.scope.AllowsTool(p.Name) {
		capability, classified := capabilityOf(p.Name)
		detail := map[string]any{
			"tool":               p.Name,
			"refused":            true,
			"reason":             "outside this token's granted capabilities",
			"granted":            capabilityNames(s.scope.Granted()),
			"known":              capabilityNames(KnownCapabilities()),
			"retry_will_succeed": false,
		}
		if classified {
			detail["requires"] = string(capability)
		}
		s.emit(Event{
			Kind:   EventToolRefused,
			Tool:   p.Name,
			Reason: string(capability),
		})
		ctx.Result(structuredErrorResult(
			fmt.Sprintf("%s is outside this token's scope; it requires the %q capability, which this "+
				"token was not granted. A different token is needed — retrying will not help.",
				p.Name, capability),
			detail))
		return
	}

	// A panic in a generator would otherwise take down the whole session, which
	// for a long-lived editor integration means the user's tools stop working
	// with no explanation. Converting it to a failed tool result keeps the
	// session alive and puts the reason in front of whoever can act on it.
	started := time.Now()
	defer func() {
		if r := recover(); r != nil {
			// The panic value is not logged. It is composed at the panic site and can
			// hold anything the panicking code had in hand — a path, a captured output,
			// an argument. It goes to the caller, who asked for this tool; it does not
			// go to a log file that outlives the process. Event.Tool says which tool,
			// which is what a log is for.
			s.emit(Event{
				Kind:     EventToolPanic,
				Tool:     t.name,
				Outcome:  OutcomeError,
				ArgNames: argumentNames(p.Arguments),
				Duration: time.Since(started),
			})
			ctx.Result(errorResult(fmt.Sprintf("%s panicked: %v", t.name, r)))
		}
	}()

	result := t.run(p.Arguments)

	outcome := OutcomeOK
	if result.IsError {
		outcome = OutcomeError
	}
	s.emit(Event{
		Kind:     EventToolCall,
		Tool:     t.name,
		Outcome:  outcome,
		ArgNames: argumentNames(p.Arguments),
		Duration: time.Since(started),
	})

	ctx.Result(result)
}

// decodeArgs unmarshals a tool's arguments.
//
// Absent arguments are treated as an empty object rather than an error: a tool
// whose fields are all optional is legitimately callable with none, and MCP
// clients differ on whether they send "arguments":{} or omit it.
func decodeArgs(raw json.RawMessage, dst any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return json.Unmarshal(raw, dst)
}

// mustSchema marshals a schema at construction time.
//
// It panics on failure because the schemas are literals in this package: a
// failure means this package does not compile correctly, which is not a runtime
// condition a caller could do anything about.
func mustSchema(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic("mcp: bad tool schema: " + err.Error())
	}
	return b
}

// schema is a small helper for writing JSON Schema without a dependency.
//
// It is a map rather than a struct because JSON Schema is open-ended and a struct
// would need a field per keyword; the schemas here are short enough that the
// literal reads as well as a builder would.
type schema map[string]any

// objectSchema builds an object schema with the given properties and required
// names.
func objectSchema(props map[string]any, required ...string) json.RawMessage {
	s := schema{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		s["required"] = required
	}
	return mustSchema(s)
}

// stringProp describes one string argument.
func stringProp(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

// boolProp describes one boolean argument.
func boolProp(description string) map[string]any {
	return map[string]any{"type": "boolean", "description": description}
}

// stringsProp describes a repeated string argument.
func stringsProp(description string) map[string]any {
	return map[string]any{
		"type":        "array",
		"items":       map[string]any{"type": "string"},
		"description": description,
	}
}
