package breeze

// mcp_server.go — serving an application's own routes as MCP tools.
//
// Auto-MCP answers a narrow question: an agent needs to call this service, the
// service already has routes, so why should anyone write a second description
// of them? Everything here is derived from what the application already
// declared — the routing table for what exists, the Scalar registry for what
// each route accepts, and the route's own middleware chain for whether a
// particular caller is allowed in.
//
// # Why this lives in package breeze and not in internal/mcp
//
// internal/mcp is the development-time server: it generates projects, reads
// dashboards, runs the toolchain. It must not import this package, because a
// project's own MCP endpoint is a runtime feature of the running service and
// the two would form a cycle. So the protocol surface needed here — three
// methods — is served directly over the rpc package, which already implements
// JSON-RPC 2.0 framing, batching, and error objects. Nothing about JSON-RPC is
// reimplemented; nothing about schema inference is either.
//
// # Why a tool call runs the route's precomputed chain
//
// The chain is the route's authentication, its rate limiting, its validation.
// Rebuilding an equivalent path for MCP would mean two ways in, and the second
// one would be the one nobody audited. So a tool call builds a Context, fills
// it exactly as OnTraffic does, and runs route.chain — the same slice, holding
// the same middleware in the same order, rebuilt in place if Use is called
// later. A request that HTTP would reject with 401 is rejected here with 401,
// because it is the same code doing the rejecting.
//
// # Why a separate listener
//
// MCP speaks JSON-RPC and HTTP speaks HTTP. Multiplexing them on one port
// would mean sniffing the first bytes of every connection to decide which
// protocol it is, on the hot path, forever. A second listener costs one port
// and keeps both parsers honest — and it lets an operator bind MCP to
// localhost while the API faces the world, which is the deployment most people
// actually want.

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/url"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/nelthaarion/breeze/v2/rpc"
	"github.com/nelthaarion/breeze/v2/scalar"
)

// mcpProtocolVersion is the MCP revision this endpoint implements. It is the
// same revision internal/mcp speaks, so one client can talk to both.
const mcpProtocolVersion = "2024-11-05"

// mcpArgIn says where a tool argument belongs once the call becomes a request.
type mcpArgIn string

const (
	mcpInPath   mcpArgIn = "path"
	mcpInQuery  mcpArgIn = "query"
	mcpInHeader mcpArgIn = "header"
	mcpInBody   mcpArgIn = "body"
)

// mcpArg is one advertised argument and the place it is delivered to.
//
// The binding is decided once, at EnableMCP time, from the same declaration
// OpenAPI reads. Deciding it per call would risk the schema and the delivery
// disagreeing — the tool advertising a query parameter and then sending it in
// the body.
type mcpArg struct {
	name string
	in   mcpArgIn
}

// mcpTool is a resolved tool: its identity, its schema, its argument bindings,
// and the route it calls.
type mcpTool struct {
	name        string
	description string
	schema      json.RawMessage
	args        []mcpArg
	rt          *route
}

// MCPServer builds a JSON-RPC server exposing this application's tagged routes
// as MCP tools.
//
// It returns an error, rather than serving a partial tool list, when a tagged
// route cannot be described completely:
//
//   - two routes claim the same tool name, so a call would be ambiguous;
//   - a tagged route has no Scalar documentation, so its inputs are unknown;
//   - a route's inputs collide, so one argument would have two meanings.
//
// Each of these produces a tool an agent would call incorrectly, and a wrong
// call against a real service is worse than a missing capability. Reporting it
// at startup puts the failure where the author can still fix it.
//
// The returned server is not listening. Callers that want a listener use
// EnableMCP; callers that want to drive it directly — tests, or an embedding
// that owns its own transport — use rpc.Server.Handle.
func (s *Breeze) MCPServer() (*rpc.Server, error) {
	tools, err := s.buildMCPTools()
	if err != nil {
		return nil, err
	}

	byName := make(map[string]*mcpTool, len(tools))
	for i := range tools {
		byName[tools[i].name] = &tools[i]
	}

	srv := rpc.NewServer(rpc.NewRegistry())

	srv.Register("initialize", func(ctx *rpc.Context) {
		title, version, _ := scalar.APIInfo()
		if title == "" {
			title = "breeze"
		}
		if version == "" {
			version = "0.0.0"
		}
		ctx.Result(map[string]any{
			"protocolVersion": mcpProtocolVersion,
			// Only tools are advertised. This endpoint exposes routes, and a
			// route is a tool; claiming resources or prompts would invite
			// requests this server has nothing to answer with.
			"capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo":   map[string]any{"name": title, "version": version},
		})
	})

	srv.Register("notifications/initialized", func(ctx *rpc.Context) {
		// A notification carries no id, so the rpc layer sends nothing back.
		// It is registered so the call does not answer "method not found",
		// which some clients treat as a failed handshake.
	})

	srv.Register("tools/list", func(ctx *rpc.Context) {
		listed := make([]map[string]any, 0, len(tools))
		for i := range tools {
			listed = append(listed, map[string]any{
				"name":        tools[i].name,
				"description": tools[i].description,
				"inputSchema": tools[i].schema,
			})
		}
		ctx.Result(map[string]any{"tools": listed})
	})

	// A tool call runs a route's chain, which may read a database or a file.
	// Registering it as blocking keeps that work off the event loop, exactly
	// as HandleBlocking does for the HTTP side.
	srv.RegisterBlocking("tools/call", func(ctx *rpc.Context) {
		var params struct {
			Name      string                     `json:"name"`
			Arguments map[string]json.RawMessage `json:"arguments"`
		}
		if err := ctx.Bind(&params); err != nil {
			ctx.Errorf(rpc.CodeInvalidParams, "arguments must be an object: "+err.Error())
			return
		}
		tool := byName[params.Name]
		if tool == nil {
			// Method-not-found is about the JSON-RPC method, which was found.
			// The tool name is a parameter, so an unknown one is a parameter
			// error — and the message lists what does exist, because a model
			// that guessed a name can recover from that and cannot recover
			// from "invalid params".
			ctx.Errorf(
				rpc.CodeInvalidParams,
				"no such tool: "+params.Name+" (available: "+strings.Join(
					mcpToolNames(tools),
					", ",
				)+")",
			)
			return
		}
		result, err := s.callMCPTool(tool, params.Arguments)
		if err != nil {
			ctx.Errorf(rpc.CodeInvalidParams, err.Error())
			return
		}
		ctx.Result(result)
	})

	return srv, nil
}

// EnableMCP starts an MCP endpoint for this application's tagged routes on
// addr, in the background.
//
// # Auto-MCP is not the same thing as an in-process control plane
//
// This endpoint exposes the routes this application tagged with MCPTool — its
// own business capabilities, as tools an agent can invoke. It is a feature of
// the application's public surface.
//
// breeze/mcp.ServeInProcess is a different endpoint with a different purpose:
// read-only introspection of this instance's own runtime — its live route
// statistics, recent errors, logs, traces. It exposes none of the application's
// routes and answers none of its business calls.
//
// A project may run both, and some should. They must be on different ports, and
// ServeInProcess refuses to start on the port this one took, which is why the
// address is recorded below.
//
// Configuration errors are returned synchronously, so a misdescribed tool
// stops the process at startup rather than surfacing as a bad call later. A
// failure to bind happens after this returns and is logged: by then the HTTP
// server is the process's reason for existing, and taking it down because an
// auxiliary port was busy would be the wrong trade.
func (s *Breeze) EnableMCP(addr string) error {
	srv, err := s.MCPServer()
	if err != nil {
		return err
	}
	record, listen, err := mcpListenAddr(addr)
	if err != nil {
		return err
	}
	// Recorded before serving, so an in-process endpoint started afterwards can
	// refuse to collide with it. Two MCP servers on one port would answer each
	// other's requests with the wrong tool table, and the symptom — "the tool I
	// called does not exist" — would point at neither of them.
	s.mcpAddr.Store(&record)
	go func() {
		if err := srv.RunAddr(listen, false); err != nil {
			log.Printf("breeze: MCP endpoint on %s stopped: %v", record, err)
		}
	}()
	return nil
}

// mcpListenAddr splits an address into the form to record and the form to bind.
//
// They differ, and both callers are picky about which one they get:
//
//   - gnet.Run parses its address as a URL and rejects one without a scheme, so
//     "127.0.0.1:2001" — the form this function's own documentation shows, and the
//     obvious one to write — fails. It fails *inside the goroutine*, after
//     EnableMCP has already returned nil, so the endpoint simply never listens and
//     the only evidence is one log line.
//   - net.SplitHostPort, which is how mcp.StartInProcess detects a port collision
//     with this endpoint, rejects the scheme-qualified form for having too many
//     colons. Recording "tcp://127.0.0.1:2001" would make the collision check
//     silently answer "no conflict" for every address that actually works.
//
// So the plain host:port is what gets recorded and reported, and the scheme is
// added only for the bind. An address that already carries a scheme is passed
// through untouched: a caller naming a Unix socket means it, and a socket cannot
// collide with a TCP port anyway.
//
// A malformed address is returned as an error rather than logged, because it is a
// configuration mistake and EnableMCP already promises to report those
// synchronously — the whole point being that the process stops while someone can
// still fix it.
func mcpListenAddr(addr string) (record, listen string, err error) {
	trimmed := strings.TrimSpace(addr)
	if trimmed == "" {
		return "", "", fmt.Errorf("breeze: EnableMCP needs an address to listen on, " +
			`for example "127.0.0.1:2001" or ":2001"`)
	}
	if strings.Contains(trimmed, "://") {
		return trimmed, trimmed, nil
	}
	if _, _, err := net.SplitHostPort(trimmed); err != nil {
		return "", "", fmt.Errorf("breeze: EnableMCP address %q is not host:port: %w. "+
			`Write ":2001" to listen on every interface, or "127.0.0.1:2001" for loopback only`,
			addr, err)
	}
	return trimmed, "tcp://" + trimmed, nil
}

// mcpFields is the Auto-MCP state on Breeze.
//
// One atomic pointer, so AutoMCPAddr can be read from another goroutine — which
// is exactly what an in-process endpoint starting concurrently does — without a
// lock on a value written once at startup.
type mcpFields struct {
	mcpAddr atomic.Pointer[string]
}

// AutoMCPAddr reports the address EnableMCP is serving the tagged-route endpoint
// on, or "" when Auto-MCP is not enabled.
//
// It exists so an in-process introspection endpoint can refuse to bind the same
// port. The two are different features with different tool sets; sharing a port
// would mean whichever bound first silently decided what the other one served.
func (s *Breeze) AutoMCPAddr() string {
	if addr := s.mcpAddr.Load(); addr != nil {
		return *addr
	}
	return ""
}

func mcpToolNames(tools []mcpTool) []string {
	out := make([]string, 0, len(tools))
	for i := range tools {
		out = append(out, tools[i].name)
	}
	return out
}

// buildMCPTools resolves every tagged route into a tool.
func (s *Breeze) buildMCPTools() ([]mcpTool, error) {
	tagged := s.Router.mcpTools
	if len(tagged) == 0 {
		return nil, nil
	}

	// Index the Scalar registry once. Its paths are in OpenAPI form ({id}) and
	// its methods are lowercased, so route patterns are converted to match
	// rather than the other way round — the registry is the side that also
	// feeds the OpenAPI document, and rewriting it here would be inventing a
	// second normalisation.
	docs := make(map[string]scalar.RouteDoc, 16)
	for _, r := range scalar.Routes() {
		docs[strings.ToUpper(r.Method)+" "+r.Path] = r.Doc
	}

	seen := make(map[string]string, len(tagged))
	tools := make([]mcpTool, 0, len(tagged))

	for _, t := range tagged {
		name := t.spec.Name
		if name == "" {
			return nil, fmt.Errorf(
				"breeze: MCPTool on %s %s has an empty name",
				t.rt.method,
				t.rt.pattern,
			)
		}
		if prev, dup := seen[name]; dup {
			return nil, fmt.Errorf(
				"breeze: MCP tool %q is claimed by both %s and %s %s; tool names must be unique because a call carries only the name",
				name,
				prev,
				t.rt.method,
				t.rt.pattern,
			)
		}
		seen[name] = string(t.rt.method) + " " + t.rt.pattern

		key := strings.ToUpper(string(t.rt.method)) + " " + openAPIPattern(t.rt.pattern)
		doc, documented := docs[key]
		if !documented {
			return nil, fmt.Errorf(
				"breeze: MCP tool %q maps to %s %s, which has no Scalar documentation, so its arguments cannot be described; register it with scalar.RegisterRoute (middlewares.Doc) or remove the tag",
				name,
				t.rt.method,
				t.rt.pattern,
			)
		}

		tool, err := buildMCPTool(name, t.spec.Description, t.rt, doc)
		if err != nil {
			return nil, err
		}
		tools = append(tools, tool)
	}

	return tools, nil
}

// buildMCPTool derives one tool's schema and argument bindings from the same
// RouteDoc the OpenAPI generator reads.
//
// The schema is flat: path, query, header and body fields all appear as
// top-level properties. A nested shape would mirror OpenAPI's own layout more
// closely, but it asks the model to know that an id belongs under "path" and a
// name under "body" — a distinction about HTTP, not about the task. Flattening
// removes it, at the cost of requiring names to be unique across groups, which
// is checked here rather than discovered during a call.
func buildMCPTool(name, description string, rt *route, doc scalar.RouteDoc) (mcpTool, error) {
	props := make(map[string]any, 8)
	required := make([]string, 0, 4)
	args := make([]mcpArg, 0, 8)

	claim := func(field string, in mcpArgIn, schema *scalar.Schema, isRequired bool) error {
		if _, exists := props[field]; exists {
			return fmt.Errorf(
				"breeze: MCP tool %q has two inputs named %q on %s %s; flattened tool arguments must be unique",
				name,
				field,
				rt.method,
				rt.pattern,
			)
		}
		if schema == nil {
			schema = &scalar.Schema{Type: "string"}
		}
		props[field] = schema
		args = append(args, mcpArg{name: field, in: in})
		if isRequired {
			required = append(required, field)
		}
		return nil
	}

	for _, group := range doc.Input {
		schema := scalar.InferSchema(group.Fields)
		if schema == nil || schema.Properties == nil {
			continue
		}
		in := mcpInBody
		switch group.Type {
		case scalar.InputParams:
			in = mcpInPath
		case scalar.InputQuery:
			in = mcpInQuery
		case scalar.InputHeader:
			in = mcpInHeader
		case scalar.InputBody:
			in = mcpInBody
		}
		for field, fieldSchema := range schema.Properties {
			// A path parameter is always required: without it there is no URL
			// to request. Everything else follows what the struct declared.
			isRequired := in == mcpInPath || schemaRequires(schema, field) ||
				(in == mcpInBody && group.Required && schemaRequires(schema, field))
			if err := claim(field, in, fieldSchema, isRequired); err != nil {
				return mcpTool{}, err
			}
		}
	}

	// Every :param in the pattern must be an argument, whether or not the
	// documentation mentioned it. Omitting one would advertise a tool that can
	// only ever produce a request with a literal ":id" in its path.
	for i, seg := range rt.segments {
		if !rt.paramIndex[i] {
			continue
		}
		field := seg[1:]
		if _, exists := props[field]; exists {
			continue
		}
		if err := claim(field, mcpInPath, &scalar.Schema{Type: "string"}, true); err != nil {
			return mcpTool{}, err
		}
	}

	// Sorted so the advertised schema is a function of the route, not of Go's
	// map iteration order. An agent that caches tool definitions should not
	// see them churn between restarts.
	sort.Strings(required)
	sort.Slice(args, func(i, j int) bool { return args[i].name < args[j].name })

	no := false
	body := map[string]any{
		"type":       "object",
		"properties": props,
		// Unknown arguments are refused rather than dropped, so a model that
		// invents a field is told, instead of watching its value vanish.
		"additionalProperties": &no,
	}
	if len(required) > 0 {
		body["required"] = required
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return mcpTool{}, fmt.Errorf(
			"breeze: MCP tool %q schema could not be encoded: %w",
			name,
			err,
		)
	}

	if description == "" {
		description = doc.Title
	}
	if description == "" {
		description = string(rt.method) + " " + rt.pattern
	}

	return mcpTool{
		name:        name,
		description: description,
		schema:      raw,
		args:        args,
		rt:          rt,
	}, nil
}

func schemaRequires(schema *scalar.Schema, field string) bool {
	for _, r := range schema.Required {
		if r == field {
			return true
		}
	}
	return false
}

// openAPIPattern converts a Breeze route pattern to the form the Scalar
// registry stores, so the two can be matched.
//
// It mirrors scalar's own conversion. The parity test asserts that it agrees
// with what scalar.Routes actually reports, so a change on that side surfaces
// as a failure here rather than as tools that silently stop resolving.
func openAPIPattern(pattern string) string {
	if !strings.Contains(pattern, ":") {
		return pattern
	}
	parts := strings.Split(pattern, "/")
	for i, p := range parts {
		if strings.HasPrefix(p, ":") {
			parts[i] = "{" + p[1:] + "}"
		}
	}
	return strings.Join(parts, "/")
}

// mcpCallResult is the MCP tool result. It carries both a rendered text form,
// which every client can display, and the structured form an agent should act
// on.
type mcpCallResult struct {
	Content           []map[string]any `json:"content"`
	StructuredContent any              `json:"structuredContent,omitempty"`
	IsError           bool             `json:"isError,omitempty"`
}

// mcpResponse is what the route answered.
type mcpResponse struct {
	Tool    string            `json:"tool"`
	Method  string            `json:"method"`
	Route   string            `json:"route"`
	Path    string            `json:"path"`
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
	// JSONBody is the parsed body when the route sent JSON. It is separate
	// from Body so a client is never forced to parse a string that might not
	// be JSON at all.
	JSONBody any `json:"json_body,omitempty"`
	// HandlerError is the text of the error the chain returned, when it
	// returned one.
	//
	// It is reported alongside the response rather than in place of it: the
	// error handler has already turned the error into a status and a body, and
	// the agent needs those to know what the caller would have seen. This field
	// answers the further question of why, which a sanitised 500 body
	// deliberately does not say.
	//
	// This is not a hole in the sanitising the default error handler does. That
	// exists because an HTTP client is arbitrary and remote; an MCP tool caller
	// has already been admitted to the tool surface by EnableMCP, which is a
	// deliberate act by the operator, and is normally the operator's own agent.
	// Withholding the reason from it would make a failing tool undiagnosable
	// through the very interface built for diagnosis.
	HandlerError string `json:"handler_error,omitempty"`
	// Note explains a status an agent should not simply retry.
	Note string `json:"note,omitempty"`
}

// callMCPTool turns a tool call into a request, runs it through the route's own
// chain, and reports what came back.
//
// The returned error covers only calls that could not be made — a missing path
// parameter, an unknown argument. A call the service refused is not an error
// here: it is a result with IsError set and the refusal's status intact, which
// is the difference between "the tool is broken" and "you are not allowed to
// do that".
func (s *Breeze) callMCPTool(
	tool *mcpTool,
	arguments map[string]json.RawMessage,
) (mcpCallResult, error) {
	binding := make(map[string]mcpArgIn, len(tool.args))
	for _, a := range tool.args {
		binding[a.name] = a.in
	}

	params := make(map[string]string, 4)
	query := url.Values{}
	headers := make(map[string]string, 4)
	body := make(map[string]json.RawMessage, 8)

	for field, raw := range arguments {
		in, known := binding[field]
		if !known {
			return mcpCallResult{}, fmt.Errorf(
				"tool %q has no argument %q; it accepts: %s",
				tool.name,
				field,
				strings.Join(mcpArgNames(tool.args), ", "),
			)
		}
		switch in {
		case mcpInPath:
			params[field] = scalarText(raw)
		case mcpInQuery:
			query.Set(field, scalarText(raw))
		case mcpInHeader:
			// Header keys are stored lowercased by the HTTP parser, and
			// middleware reads them that way. Injecting the canonical form
			// would produce a header no middleware could find.
			headers[strings.ToLower(field)] = scalarText(raw)
		case mcpInBody:
			body[field] = raw
		}
	}

	// Build the concrete path. A missing parameter is caught here rather than
	// producing a request for a URL containing ":id".
	path, err := fillPattern(tool.rt, params)
	if err != nil {
		return mcpCallResult{}, err
	}

	ctx := NewContext(tool.rt.method, path)
	for k, v := range headers {
		ctx.Req.Header[k] = v
	}
	if len(query) > 0 {
		ctx.Req.Query = query
	}
	if len(body) > 0 {
		encoded, err := json.Marshal(body)
		if err != nil {
			return mcpCallResult{}, fmt.Errorf(
				"tool %q could not encode its body arguments: %w",
				tool.name,
				err,
			)
		}
		ctx.Req.Body = encoded
		// Set so a handler or middleware that inspects the content type sees
		// what it would see from an HTTP client sending the same body.
		if _, set := ctx.Req.Header["content-type"]; !set {
			ctx.Req.Header["content-type"] = "application/json"
		}
	}
	if len(params) > 0 {
		ctx.SetParams(params)
	}

	// The route's own chain: [global..., route..., handler]. Running this and
	// nothing else is what makes an MCP call and an HTTP call indistinguishable
	// from the middleware's point of view.
	//
	// The chain's error is carried into mcpResultFrom rather than dropped. Since
	// Part 1, a handler reports failure by returning — so discarding it here would
	// make a failing tool look like one that answered with an empty body, and the
	// agent's only clue would be a status of zero.
	ctx.middlewares = tool.rt.chain
	ctx.index = -1
	chainErr := ctx.Next()
	if chainErr != nil {
		// Resolved to a response first, exactly as the HTTP path does, so the tool
		// result carries the same status and body an HTTP caller would have seen.
		// Without this an MCP call and an HTTP call to the same failing route would
		// disagree, which is the one thing Auto-MCP promises they never do.
		s.handleChainError(ctx, chainErr)
	}

	return mcpResultFrom(tool, path, ctx, chainErr), nil
}

func mcpArgNames(args []mcpArg) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		out = append(out, a.name)
	}
	return out
}

// mcpResultFrom reads the response the chain produced.
//
// chainErr is the error the chain returned, or nil. It is reported as a separate field
// rather than folded into the status, because "the handler failed" and "the handler
// answered 500" are different facts an agent may want to distinguish — and because the
// error's own text names the failure in a way a status code cannot.
func mcpResultFrom(tool *mcpTool, path string, ctx *Context, chainErr error) mcpCallResult {
	out := mcpResponse{
		Tool:   tool.name,
		Method: string(tool.rt.method),
		Route:  tool.rt.pattern,
		Path:   path,
	}
	if chainErr != nil {
		out.HandlerError = chainErr.Error()
	}

	if ctx.Res == nil {
		// The chain returned without writing anything. Over HTTP this is a
		// bug that shows up as an empty reply; naming it is more useful than
		// reporting a status of zero.
		out.Status = 0
		out.Note = "the route produced no response; a middleware may have stopped the chain without writing one"
		return mcpCallResult{
			Content:           []map[string]any{{"type": "text", "text": mcpRender(out)}},
			StructuredContent: out,
			IsError:           true,
		}
	}

	out.Status = ctx.Res.Status
	if out.Status == 0 {
		// Status is only stamped when a body method runs; a handler that set
		// headers alone leaves it zero, and the wire format defaults to 200.
		out.Status = 200
	}
	if len(ctx.Res.Headers) > 0 {
		// Copied because the map may be one of the shared header maps the
		// response fast paths point at. Handing that out would let a caller
		// mutate every future response.
		out.Headers = make(map[string]string, len(ctx.Res.Headers))
		for k, v := range ctx.Res.Headers {
			out.Headers[k] = v
		}
	}
	if len(ctx.Res.Body) > 0 {
		out.Body = string(ctx.Res.Body)
		var parsed any
		if json.Unmarshal(ctx.Res.Body, &parsed) == nil {
			out.JSONBody = parsed
		}
	}

	isError := out.Status >= 400
	if isError {
		out.Note = mcpStatusNote(out.Status)
	}

	return mcpCallResult{
		Content:           []map[string]any{{"type": "text", "text": mcpRender(out)}},
		StructuredContent: out,
		IsError:           isError,
	}
}

// mcpStatusNote says what a refusal means, so an agent does not retry a call
// that will be refused identically every time.
func mcpStatusNote(status int) string {
	switch {
	case status == 401:
		return "the route's own middleware refused these credentials; retrying without changing them will fail the same way"
	case status == 403:
		return "authenticated, but not permitted to do this"
	case status == 404:
		return "the route matched no resource; check the path arguments"
	case status == 422 || status == 400:
		return "the route rejected the arguments; check them against the tool's schema"
	case status == 429:
		return "rate limited by the route's own middleware"
	case status >= 500:
		return "the route failed internally; this is a fault in the service, not in the call"
	default:
		return "the route refused the call"
	}
}

func mcpRender(out mcpResponse) string {
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Sprintf("%s %s -> %d", out.Method, out.Path, out.Status)
	}
	return string(b)
}

// fillPattern substitutes params into a route pattern.
func fillPattern(rt *route, params map[string]string) (string, error) {
	if rt.paramCount == 0 && !rt.hasWildcard {
		return rt.pattern, nil
	}
	parts := make([]string, 0, len(rt.segments)+1)
	for i, seg := range rt.segments {
		if !rt.paramIndex[i] {
			parts = append(parts, seg)
			continue
		}
		name := seg[1:]
		value, ok := params[name]
		if !ok || value == "" {
			return "", fmt.Errorf(
				"argument %q is required: %s has no path without it",
				name,
				rt.pattern,
			)
		}
		parts = append(parts, url.PathEscape(value))
	}
	if rt.hasWildcard {
		if value := params[rt.wildcardName]; value != "" {
			parts = append(parts, value)
		}
	}
	return "/" + strings.Join(parts, "/"), nil
}

// scalarText renders a JSON value as the text a path segment or query value
// carries.
//
// A JSON string becomes its contents, so "abc" does not travel as "\"abc\"".
// Everything else keeps its JSON spelling, which is what a number, a boolean
// and null all look like in a query string anyway.
func scalarText(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return ""
	}
	if trimmed[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
	}
	return trimmed
}
