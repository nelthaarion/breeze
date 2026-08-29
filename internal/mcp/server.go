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

	"github.com/nelthaarion/breeze/rpc"
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
	ctx.Result(initializeResult{
		ProtocolVersion: protocolVersion,
		Capabilities:    serverCapabilities{},
		ServerInfo: serverInfo{
			Name:    serverName,
			Version: s.version,
		},
	})
}

// handleToolsList enumerates the tools.
func (s *Server) handleToolsList(ctx *rpc.Context) {
	list := make([]toolDescriptor, 0, len(s.order))
	for _, name := range s.order {
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
		// guessed, and a list turns a dead end into a correction.
		ctx.Errorf(rpc.CodeInvalidParams,
			fmt.Sprintf("unknown tool %q; available: %s", p.Name, strings.Join(s.order, ", ")))
		return
	}

	// A panic in a generator would otherwise take down the whole session, which
	// for a long-lived editor integration means the user's tools stop working
	// with no explanation. Converting it to a failed tool result keeps the
	// session alive and puts the reason in front of whoever can act on it.
	defer func() {
		if r := recover(); r != nil {
			ctx.Result(errorResult(fmt.Sprintf("%s panicked: %v", t.name, r)))
		}
	}()

	ctx.Result(t.run(p.Arguments))
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
