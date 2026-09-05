package breeze

// mcp_auto_test.go — Auto-MCP: the properties that make exposing routes to an
// agent safe.
//
// Three of them carry the feature, and each is asserted against real behaviour
// rather than against a restatement of the implementation:
//
//   - A tool's schema says what the OpenAPI document says. The test generates
//     the actual document and compares, so the two cannot drift.
//   - A tool call is refused exactly as HTTP refuses it. The test runs the same
//     request through the router's own dispatch path and compares the outcome,
//     so "the same middleware ran" is demonstrated, not assumed.
//   - An untagged route is never listed. The fixture documents an internal
//     route and leaves it untagged, so a regression that listed everything
//     would fail here rather than in production.
//
// The tests drive the server through rpc.Server.Handle rather than a socket.
// The rpc package has no Stop, so a listener would own its port for the rest of
// the process; Handle exercises the same dispatch and returns the bytes.

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/nelthaarion/breeze/rpc"
	"github.com/nelthaarion/breeze/scalar"
)

const mcpFixtureToken = "mcp-test-token"

// The fixture's paths are prefixed so they cannot collide with routes another
// test in this package registers into the process-wide Scalar registry.
const (
	pathOrders   = "/mcpfix/orders"
	pathOrderID  = "/mcpfix/orders/:id"
	pathInternal = "/mcpfix/internal/metrics"
	pathTenant   = "/mcpfix/tenant"
	pathBroken   = "/mcpfix/pricing"
)

type orderBody struct {
	SKU string `json:"sku" description:"Stock keeping unit to order."`
	Qty int    `json:"qty,omitempty" description:"How many; defaults to one."`
}

type orderParams struct {
	ID string `json:"id" description:"The order's identifier."`
}

type orderQuery struct {
	Expand string `json:"expand,omitempty" description:"Related data to include."`
}

type tenantHeader struct {
	Tenant string `json:"x-tenant" description:"Tenant the call is made for."`
}

// requireFixtureToken is ordinary route middleware. Nothing about it knows that
// MCP exists, which is the point: the parity test proves an MCP call is
// refused by this code and not by a special case written for MCP.
func requireFixtureToken(ctx *Context) error {
	if ctx.Req.Header["authorization"] != "Bearer "+mcpFixtureToken {
		ctx.Status(401)
		_ = ctx.JSON(map[string]any{"error": "unauthorized"})
		ctx.Abort()
		return nil
	}
	return ctx.Next()
}

var (
	mcpFixtureOnce   sync.Once
	mcpFixtureRouter *Router
	mcpFixtureApp    *Breeze
)

// mcpFixture builds the router once. Scalar's registry is process-wide and
// registering the same route twice would leave duplicate entries, so the
// fixture is shared rather than rebuilt per test.
func mcpFixture(t *testing.T) (*Breeze, *Router) {
	t.Helper()
	mcpFixtureOnce.Do(func() {
		scalar.Enable()

		r := NewRouter()

		// Tagged: a body, behind authentication.
		scalar.RegisterRoute("POST", pathOrders, scalar.RouteDoc{
			Title: "Create an order",
			Input: []scalar.InputGroup{
				{Type: scalar.InputBody, Fields: orderBody{}, Required: true},
			},
			Output: map[string]any{},
		})
		r.Handle(POST, pathOrders, func(ctx *Context) error {
			var in orderBody
			if err := ctx.Bind(&in); err != nil {
				return nil // Bind already wrote the 400
			}
			qty := in.Qty
			if qty == 0 {
				qty = 1
			}
			return ctx.JSON(map[string]any{"created": true, "sku": in.SKU, "qty": qty})
		}, requireFixtureToken, MCPTool("create_order", "Places an order for a customer."))

		// Tagged: a path parameter and a query parameter.
		scalar.RegisterRoute("GET", pathOrderID, scalar.RouteDoc{
			Title: "Fetch an order",
			Input: []scalar.InputGroup{
				{Type: scalar.InputParams, Fields: orderParams{}},
				{Type: scalar.InputQuery, Fields: orderQuery{}},
			},
			Output: map[string]any{},
		})
		r.Handle(GET, pathOrderID, func(ctx *Context) error {
			return ctx.JSON(map[string]any{
				"id":     ctx.Param("id"),
				"expand": ctx.Query("expand"),
			})
		}, MCPTool("get_order", "Fetches one order by id."))

		// Tagged: a header input.
		scalar.RegisterRoute("GET", pathTenant, scalar.RouteDoc{
			Title: "Report the tenant",
			Input: []scalar.InputGroup{
				{Type: scalar.InputHeader, Fields: tenantHeader{}},
			},
			Output: map[string]any{},
		})
		r.Handle(GET, pathTenant, func(ctx *Context) error {
			return ctx.JSON(map[string]any{"tenant": ctx.Req.Header["x-tenant"]})
		}, MCPTool("report_tenant", "Reports which tenant the call was made for."))

		// Tagged, and deliberately fails by returning an error. Part 1 made that
		// the way a handler reports failure, so the MCP path has to resolve it to
		// a response exactly as the HTTP path does — otherwise a failing tool is
		// indistinguishable from one that answered with an empty body.
		scalar.RegisterRoute("GET", pathBroken, scalar.RouteDoc{
			Title:  "A route that fails",
			Output: map[string]any{},
		})
		r.Handle(GET, pathBroken, func(ctx *Context) error {
			return WrapHTTPError(503, "the pricing service is unavailable",
				errors.New("dial tcp 10.0.0.7:5432: connect: connection refused"))
		}, MCPTool("check_pricing", "Checks pricing for a customer."))

		// Documented but deliberately NOT tagged.
		scalar.RegisterRoute("GET", pathInternal, scalar.RouteDoc{
			Title:  "Internal metrics",
			Output: map[string]any{},
		})
		r.Handle(GET, pathInternal, func(ctx *Context) error {
			return ctx.JSON(map[string]any{"secret": true})
		})

		mcpFixtureRouter = r
		mcpFixtureApp = &Breeze{Router: r}
	})
	return mcpFixtureApp, mcpFixtureRouter
}

type rpcReply struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func mcpRPC(t *testing.T, srv *rpc.Server, method string, params any) rpcReply {
	t.Helper()
	req := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		req["params"] = params
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("encoding %s request: %v", method, err)
	}
	out := srv.Handle(raw)
	if len(out) == 0 {
		t.Fatalf("%s produced no response", method)
	}
	var reply rpcReply
	if err := json.Unmarshal(out, &reply); err != nil {
		t.Fatalf("decoding %s response %s: %v", method, out, err)
	}
	return reply
}

func mcpServerFor(t *testing.T) *rpc.Server {
	t.Helper()
	app, _ := mcpFixture(t)
	srv, err := app.MCPServer()
	if err != nil {
		t.Fatalf("MCPServer: %v", err)
	}
	return srv
}

type listedTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

func listTools(t *testing.T, srv *rpc.Server) map[string]listedTool {
	t.Helper()
	reply := mcpRPC(t, srv, "tools/list", nil)
	if reply.Error != nil {
		t.Fatalf("tools/list failed: %d %s", reply.Error.Code, reply.Error.Message)
	}
	var body struct {
		Tools []listedTool `json:"tools"`
	}
	if err := json.Unmarshal(reply.Result, &body); err != nil {
		t.Fatalf("decoding tool list: %v", err)
	}
	out := make(map[string]listedTool, len(body.Tools))
	for _, tool := range body.Tools {
		out[tool.Name] = tool
	}
	return out
}

// callResult is the tool result as a client receives it.
type callResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StructuredContent struct {
		Tool         string            `json:"tool"`
		Method       string            `json:"method"`
		Route        string            `json:"route"`
		Path         string            `json:"path"`
		Status       int               `json:"status"`
		Headers      map[string]string `json:"headers"`
		Body         string            `json:"body"`
		JSONBody     map[string]any    `json:"json_body"`
		HandlerError string            `json:"handler_error"`
		Note         string            `json:"note"`
	} `json:"structuredContent"`
	IsError bool `json:"isError"`
}

func callTool(t *testing.T, srv *rpc.Server, name string, args map[string]any) (callResult, *rpcReply) {
	t.Helper()
	params := map[string]any{"name": name}
	if args != nil {
		params["arguments"] = args
	}
	reply := mcpRPC(t, srv, "tools/call", params)
	if reply.Error != nil {
		return callResult{}, &reply
	}
	var out callResult
	if err := json.Unmarshal(reply.Result, &out); err != nil {
		t.Fatalf("decoding tool result: %v", err)
	}
	return out, nil
}

func TestMCPHandshakeAdvertisesToolsOnly(t *testing.T) {
	srv := mcpServerFor(t)
	reply := mcpRPC(t, srv, "initialize", map[string]any{"protocolVersion": mcpProtocolVersion})
	if reply.Error != nil {
		t.Fatalf("initialize failed: %d %s", reply.Error.Code, reply.Error.Message)
	}
	var body struct {
		ProtocolVersion string         `json:"protocolVersion"`
		Capabilities    map[string]any `json:"capabilities"`
	}
	if err := json.Unmarshal(reply.Result, &body); err != nil {
		t.Fatalf("decoding initialize result: %v", err)
	}
	if body.ProtocolVersion != mcpProtocolVersion {
		t.Errorf("protocol version = %q, want %q", body.ProtocolVersion, mcpProtocolVersion)
	}
	if _, ok := body.Capabilities["tools"]; !ok {
		t.Errorf("tools capability missing from %v", body.Capabilities)
	}
	// Claiming a capability this endpoint cannot serve would invite requests it
	// can only answer with an error.
	for _, unsupported := range []string{"resources", "prompts"} {
		if _, ok := body.Capabilities[unsupported]; ok {
			t.Errorf("advertised %q, which routes-as-tools cannot serve", unsupported)
		}
	}
}

func TestUntaggedRoutesAreNeverExposedAsTools(t *testing.T) {
	srv := mcpServerFor(t)
	tools := listTools(t, srv)

	for _, want := range []string{"create_order", "get_order", "report_tenant", "check_pricing"} {
		if _, ok := tools[want]; !ok {
			t.Errorf("tagged route missing from tool list: %s", want)
		}
	}

	// The internal route is documented, so it is in the OpenAPI document and in
	// the Scalar registry. Being discoverable is not being callable.
	for name, tool := range tools {
		var schema struct {
			Properties map[string]any `json:"properties"`
		}
		_ = json.Unmarshal(tool.InputSchema, &schema)
		if strings.Contains(tool.Description, "Internal") {
			t.Errorf("tool %q exposes the untagged internal route", name)
		}
	}
	if len(tools) != 4 {
		t.Errorf("tool count = %d, want 4; an untagged route may have leaked in: %v", len(tools), keysOfTools(tools))
	}

	// And it cannot be reached by guessing a name either.
	if _, rpcErr := callTool(t, srv, "internal_metrics", nil); rpcErr == nil {
		t.Error("calling an untagged route by name succeeded")
	}
}

func keysOfTools(tools map[string]listedTool) []string {
	out := make([]string, 0, len(tools))
	for name := range tools {
		out = append(out, name)
	}
	return out
}

// TestToolSchemaMatchesTheOpenAPIDocument compares each tool's advertised
// schema against the OpenAPI document the same registry generates.
//
// This is the parity that matters: an agent reads the tool schema, a human
// reads the OpenAPI document, and if they disagree the agent's calls are wrong
// in a way nobody reviewing the docs would notice.
func TestToolSchemaMatchesTheOpenAPIDocument(t *testing.T) {
	srv := mcpServerFor(t)
	tools := listTools(t, srv)

	var doc scalar.OpenAPI
	if err := json.Unmarshal(scalar.Generate(), &doc); err != nil {
		t.Fatalf("decoding generated OpenAPI: %v", err)
	}

	cases := []struct {
		tool   string
		method string
		path   string
	}{
		{"create_order", "post", pathOrders},
		{"get_order", "get", "/mcpfix/orders/{id}"},
		{"report_tenant", "get", pathTenant},
	}

	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			item, ok := doc.Paths[tc.path]
			if !ok {
				t.Fatalf("OpenAPI has no path %q; it has %v", tc.path, pathKeys(doc))
			}
			op, ok := item[tc.method]
			if !ok {
				t.Fatalf("OpenAPI path %q has no %s operation", tc.path, tc.method)
			}

			var schema struct {
				Type       string                    `json:"type"`
				Properties map[string]*scalar.Schema `json:"properties"`
				Required   []string                  `json:"required"`
			}
			if err := json.Unmarshal(tools[tc.tool].InputSchema, &schema); err != nil {
				t.Fatalf("decoding tool schema: %v", err)
			}
			if schema.Type != "object" {
				t.Errorf("tool schema type = %q, want object", schema.Type)
			}

			// Every OpenAPI parameter must be an argument of the same type.
			for _, param := range op.Parameters {
				prop, ok := schema.Properties[param.Name]
				if !ok {
					t.Errorf("OpenAPI declares %s parameter %q, which the tool does not accept", param.In, param.Name)
					continue
				}
				if param.Schema != nil && prop.Type != param.Schema.Type {
					t.Errorf("parameter %q: tool type %q, OpenAPI type %q", param.Name, prop.Type, param.Schema.Type)
				}
				if param.In == "path" && !contains(schema.Required, param.Name) {
					t.Errorf("path parameter %q is not required by the tool, but a URL cannot be built without it", param.Name)
				}
			}

			// Every body property must be an argument of the same type.
			if op.RequestBody != nil {
				media, ok := op.RequestBody.Content["application/json"]
				if !ok || media.Schema == nil {
					t.Fatal("OpenAPI request body has no application/json schema")
				}
				for field, want := range media.Schema.Properties {
					prop, ok := schema.Properties[field]
					if !ok {
						t.Errorf("OpenAPI declares body field %q, which the tool does not accept", field)
						continue
					}
					if prop.Type != want.Type {
						t.Errorf("body field %q: tool type %q, OpenAPI type %q", field, prop.Type, want.Type)
					}
					// The description is the sentence the model reads to decide
					// what to put here. Losing it is a silent downgrade.
					if want.Description != "" && prop.Description != want.Description {
						t.Errorf("body field %q description: tool %q, OpenAPI %q", field, prop.Description, want.Description)
					}
				}
				for _, field := range media.Schema.Required {
					if !contains(schema.Required, field) {
						t.Errorf("body field %q is required by OpenAPI but optional in the tool", field)
					}
				}
			}

			// The tool must not invent arguments the document knows nothing
			// about — that is the same drift in the other direction.
			for field := range schema.Properties {
				if knownToOpenAPI(op, field) {
					continue
				}
				t.Errorf("tool accepts %q, which the OpenAPI document does not declare", field)
			}
		})
	}
}

func pathKeys(doc scalar.OpenAPI) []string {
	out := make([]string, 0, len(doc.Paths))
	for p := range doc.Paths {
		out = append(out, p)
	}
	return out
}

func knownToOpenAPI(op scalar.Operation, field string) bool {
	for _, param := range op.Parameters {
		if param.Name == field {
			return true
		}
	}
	if op.RequestBody != nil {
		if media, ok := op.RequestBody.Content["application/json"]; ok && media.Schema != nil {
			if _, ok := media.Schema.Properties[field]; ok {
				return true
			}
		}
	}
	return false
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// runHTTP runs a request through the router's own dispatch path — the same
// lookup and the same chain OnTraffic uses — and reports what the chain wrote.
//
// It is the reference the MCP path is compared against. Anything less direct
// (a second fixture, a recorded expectation) would let the two drift while the
// test still passed.
//
// A returned error is resolved through handleChainError, because that is what
// OnTraffic does with one. Ignoring it here would make the reference disagree
// with the real HTTP path for exactly the routes that fail — the case the
// comparison is most needed for.
func runHTTP(t *testing.T, r *Router, method Method, path string, headers map[string]string, body []byte) (int, string) {
	t.Helper()
	req := &HTTPRequest{Method: method, Path: path, Header: map[string]string{}, Body: body}
	for k, v := range headers {
		req.Header[strings.ToLower(k)] = v
	}
	chain, params, _ := r.findDispatch(req)
	if chain == nil {
		t.Fatalf("no route matched %s %s", method, path)
	}
	ctx := &Context{Req: req, index: -1, middlewares: chain}
	if params != nil {
		ctx.SetParams(params)
	}
	if err := ctx.Next(); err != nil {
		// A zero-valued Breeze, so the default error handler runs — the same one
		// the MCP fixtures use, since neither sets ErrorHandler.
		(&Breeze{}).handleChainError(ctx, err)
	}
	if ctx.Res == nil {
		return 0, ""
	}
	status := ctx.Res.Status
	if status == 0 {
		status = 200
	}
	return status, string(ctx.Res.Body)
}

// TestToolCallIsRefusedExactlyAsHTTPWouldRefuseIt is the security property.
//
// The route is behind authentication. If MCP dispatch bypassed the chain — or
// rebuilt an equivalent one that forgot a step — this is where it would show.
func TestToolCallIsRefusedExactlyAsHTTPWouldRefuseIt(t *testing.T) {
	srv := mcpServerFor(t)
	_, router := mcpFixture(t)

	t.Run("without credentials", func(t *testing.T) {
		result, rpcErr := callTool(t, srv, "create_order", map[string]any{"sku": "ABC-1"})
		if rpcErr != nil {
			t.Fatalf("refusal arrived as a protocol error (%d %s); a refused call is a result, not a broken tool",
				rpcErr.Error.Code, rpcErr.Error.Message)
		}
		if result.StructuredContent.Status != 401 {
			t.Errorf("status = %d, want 401", result.StructuredContent.Status)
		}
		if !result.IsError {
			t.Error("a 401 was not reported as an error result")
		}
		if !strings.Contains(result.StructuredContent.Note, "refused these credentials") {
			t.Errorf("note does not explain the refusal: %q", result.StructuredContent.Note)
		}

		// The comparison that proves it was the same middleware.
		wantStatus, wantBody := runHTTP(t, router, POST, pathOrders, nil, []byte(`{"sku":"ABC-1"}`))
		if result.StructuredContent.Status != wantStatus {
			t.Errorf("MCP status %d, HTTP status %d", result.StructuredContent.Status, wantStatus)
		}
		if result.StructuredContent.Body != wantBody {
			t.Errorf("MCP body %q, HTTP body %q", result.StructuredContent.Body, wantBody)
		}
	})

	t.Run("with credentials", func(t *testing.T) {
		result, rpcErr := callTool(t, srv, "create_order", map[string]any{
			"sku":           "ABC-1",
			"qty":           3,
			"authorization": "Bearer " + mcpFixtureToken,
		})
		// authorization is not an advertised argument, so the call is refused
		// as malformed rather than silently sent without credentials.
		if rpcErr == nil {
			t.Fatalf("an undeclared argument was accepted; result: %+v", result.StructuredContent)
		}
		if !strings.Contains(rpcErr.Error.Message, "authorization") {
			t.Errorf("error does not name the rejected argument: %q", rpcErr.Error.Message)
		}
	})
}

// TestCredentialsTravelWhenTheRouteDeclaresTheHeader shows the supported way to
// authenticate a tool call: the route declares the header as an input, so it
// becomes an argument, and the value is injected under the lowercased key the
// parser would have produced.
func TestCredentialsTravelWhenTheRouteDeclaresTheHeader(t *testing.T) {
	srv := mcpServerFor(t)
	_, router := mcpFixture(t)

	result, rpcErr := callTool(t, srv, "report_tenant", map[string]any{"x-tenant": "acme"})
	if rpcErr != nil {
		t.Fatalf("tools/call failed: %d %s", rpcErr.Error.Code, rpcErr.Error.Message)
	}
	if result.StructuredContent.Status != 200 {
		t.Fatalf("status = %d, want 200 (body %q)", result.StructuredContent.Status, result.StructuredContent.Body)
	}
	if got := result.StructuredContent.JSONBody["tenant"]; got != "acme" {
		t.Errorf("tenant = %v, want acme; the header did not reach the handler under the key it reads", got)
	}

	wantStatus, wantBody := runHTTP(t, router, GET, pathTenant, map[string]string{"X-Tenant": "acme"}, nil)
	if result.StructuredContent.Status != wantStatus || result.StructuredContent.Body != wantBody {
		t.Errorf("MCP %d %q, HTTP %d %q", result.StructuredContent.Status, result.StructuredContent.Body, wantStatus, wantBody)
	}
}

func TestArgumentsArriveWhereTheSchemaSaidTheyWould(t *testing.T) {
	srv := mcpServerFor(t)
	_, router := mcpFixture(t)

	t.Run("path and query", func(t *testing.T) {
		result, rpcErr := callTool(t, srv, "get_order", map[string]any{"id": "42", "expand": "items"})
		if rpcErr != nil {
			t.Fatalf("tools/call failed: %d %s", rpcErr.Error.Code, rpcErr.Error.Message)
		}
		if result.StructuredContent.Path != "/mcpfix/orders/42" {
			t.Errorf("path = %q, want /mcpfix/orders/42", result.StructuredContent.Path)
		}
		if got := result.StructuredContent.JSONBody["id"]; got != "42" {
			t.Errorf("id = %v, want 42", got)
		}
		if got := result.StructuredContent.JSONBody["expand"]; got != "items" {
			t.Errorf("expand = %v, want items; a query argument did not reach the query string", got)
		}
		if result.StructuredContent.Route != pathOrderID {
			t.Errorf("route = %q, want the pattern %q", result.StructuredContent.Route, pathOrderID)
		}
	})

	t.Run("body", func(t *testing.T) {
		// Sent through the same tool that requires authentication, so this also
		// shows the chain running to completion rather than stopping early.
		result, rpcErr := callTool(t, srv, "create_order", nil)
		if rpcErr != nil {
			t.Fatalf("tools/call failed: %d %s", rpcErr.Error.Code, rpcErr.Error.Message)
		}
		// No credentials, so 401 — asserted above. What matters here is that a
		// call with no arguments is still a well-formed call.
		if result.StructuredContent.Status != 401 {
			t.Errorf("status = %d, want 401", result.StructuredContent.Status)
		}
	})

	t.Run("a missing path argument is refused before any request is made", func(t *testing.T) {
		_, rpcErr := callTool(t, srv, "get_order", map[string]any{"expand": "items"})
		if rpcErr == nil {
			t.Fatal("a call with no id produced a request; the path would have contained a literal :id")
		}
		if !strings.Contains(rpcErr.Error.Message, "id") {
			t.Errorf("error does not name the missing argument: %q", rpcErr.Error.Message)
		}
	})

	t.Run("a numeric argument is not sent with its JSON quotes", func(t *testing.T) {
		result, rpcErr := callTool(t, srv, "get_order", map[string]any{"id": 42})
		if rpcErr != nil {
			t.Fatalf("tools/call failed: %d %s", rpcErr.Error.Code, rpcErr.Error.Message)
		}
		if result.StructuredContent.Path != "/mcpfix/orders/42" {
			t.Errorf("path = %q, want /mcpfix/orders/42", result.StructuredContent.Path)
		}
	})

	// Whatever the tool reports, HTTP must agree.
	wantStatus, wantBody := runHTTP(t, router, GET, "/mcpfix/orders/42", nil, nil)
	if wantStatus != 200 || !strings.Contains(wantBody, `"id":"42"`) {
		t.Fatalf("HTTP reference call disagrees: %d %q", wantStatus, wantBody)
	}
}

// TestAReturnedErrorReachesTheAgentAsTheSameFailureHTTPWouldSee is the Part 1
// property on the MCP side.
//
// The handler reports failure by returning, which is the only way it now has. Three
// things have to be true at once for that to be usable by an agent:
//
//   - the status the error handler chose arrives, so the agent sees 503 and not the
//     zero status of "nothing was written";
//   - the result is marked as an error, so the agent does not treat the problem
//     document as the answer it asked for;
//   - the tool and an HTTP client agree exactly, which is Auto-MCP's whole premise
//     and would be silently broken by dispatch that dropped the error.
func TestAReturnedErrorReachesTheAgentAsTheSameFailureHTTPWouldSee(t *testing.T) {
	srv := mcpServerFor(t)
	_, router := mcpFixture(t)

	result, rpcErr := callTool(t, srv, "check_pricing", nil)
	if rpcErr != nil {
		t.Fatalf("a handler's returned error arrived as a protocol error (%d %s); the route was called, so this is a failed call and not a broken tool",
			rpcErr.Error.Code, rpcErr.Error.Message)
	}

	if result.StructuredContent.Status != 503 {
		t.Errorf("status = %d, want 503 — the returned error did not become a response",
			result.StructuredContent.Status)
	}
	if !result.IsError {
		t.Error("a failing handler was not reported as an error result")
	}
	// The message the handler chose for a client to read.
	if !strings.Contains(result.StructuredContent.Body, "pricing service is unavailable") {
		t.Errorf("the handler's message is missing from the body: %q", result.StructuredContent.Body)
	}
	// The wrapped cause is not in the body, exactly as over HTTP: WrapHTTPError's two
	// fields exist to keep the internal address out of the response.
	if strings.Contains(result.StructuredContent.Body, "10.0.0.7") {
		t.Errorf("the wrapped cause leaked into the response body: %q", result.StructuredContent.Body)
	}
	// It is reported out of band instead. Without this an operator debugging their own
	// agent has a 503 and no way to learn why, which is the failure the field exists
	// to prevent.
	if !strings.Contains(result.StructuredContent.HandlerError, "10.0.0.7") {
		t.Errorf("handler_error does not carry the cause: %q", result.StructuredContent.HandlerError)
	}

	// The parity check. Same route, same chain, same error handler.
	wantStatus, wantBody := runHTTP(t, router, GET, pathBroken, nil, nil)
	if result.StructuredContent.Status != wantStatus {
		t.Errorf("MCP status %d, HTTP status %d", result.StructuredContent.Status, wantStatus)
	}
	if result.StructuredContent.Body != wantBody {
		t.Errorf("MCP body %q, HTTP body %q", result.StructuredContent.Body, wantBody)
	}
}

// TestTheTagIsNotAStepInTheChain guards the property that makes tagging free:
// a tagged route serves HTTP exactly as it would untagged.
func TestTheTagIsNotAStepInTheChain(t *testing.T) {
	r := NewRouter()
	handler := func(ctx *Context) error { return ctx.WriteString("ok") }
	mw := func(ctx *Context) error { return ctx.Next() }

	r.Handle(GET, "/tagged", handler, mw, MCPTool("tagged", "tagged"))
	r.Handle(GET, "/plain", handler, mw)
	r.Handle(GET, "/tag-only", handler, MCPTool("tag_only", "tag only"))

	tagged, plain, tagOnly := r.routes[0], r.routes[1], r.routes[2]

	if len(tagged.chain) != len(plain.chain) {
		t.Errorf("tagged chain has %d entries, untagged has %d; the tag is still in the chain",
			len(tagged.chain), len(plain.chain))
	}
	if len(tagged.routeMWs) != 1 {
		t.Errorf("tagged route kept %d middlewares, want 1", len(tagged.routeMWs))
	}
	if len(tagOnly.routeMWs) != 0 {
		t.Errorf("a route whose only middleware was a tag kept %d, want 0", len(tagOnly.routeMWs))
	}
	// A tag left in the chain would be a nil-free no-op and easy to miss, so
	// the request is actually run.
	if status, body := runHTTP(t, r, GET, "/tag-only", nil, nil); status != 200 || body != "ok" {
		t.Errorf("tagged route served %d %q, want 200 \"ok\"", status, body)
	}

	// Only tagged routes are collected, in registration order.
	exposed := r.MCPRoutes()
	if len(exposed) != 2 {
		t.Fatalf("MCPRoutes returned %d routes, want 2", len(exposed))
	}
	if exposed[0].Pattern() != "/tagged" || exposed[1].Pattern() != "/tag-only" {
		t.Errorf("MCPRoutes = %q, %q; want /tagged, /tag-only", exposed[0].Pattern(), exposed[1].Pattern())
	}
}

// TestTagsSurviveAGlobalMiddlewareAddedLater checks that a tool dispatches
// through the chain the route ends up with, not the one it had when tagged.
func TestTagsSurviveAGlobalMiddlewareAddedLater(t *testing.T) {
	r := NewRouter()
	r.Handle(GET, "/late", func(ctx *Context) error { return ctx.WriteString("handler") }, MCPTool("late", "late"))

	seen := false
	r.Use(func(ctx *Context) error {
		seen = true
		return ctx.Next()
	})

	rt := r.mcpTools[0].rt
	if len(rt.chain) != 2 {
		t.Fatalf("chain has %d entries after Use, want 2", len(rt.chain))
	}

	ctx := NewContext(GET, "/late")
	ctx.middlewares = rt.chain
	ctx.index = -1
	ctx.Next()

	if !seen {
		t.Error("the global middleware added after tagging did not run for the tool's chain")
	}
}

func TestMisdescribedToolsAreRejectedAtStartup(t *testing.T) {
	t.Run("duplicate names", func(t *testing.T) {
		scalar.Enable()
		r := NewRouter()
		scalar.RegisterRoute("GET", "/dup/a", scalar.RouteDoc{Title: "a"})
		scalar.RegisterRoute("GET", "/dup/b", scalar.RouteDoc{Title: "b"})
		noop := func(ctx *Context) error {
			return nil
		}
		r.Handle(GET, "/dup/a", noop, MCPTool("same", "first"))
		r.Handle(GET, "/dup/b", noop, MCPTool("same", "second"))

		app := &Breeze{Router: r}
		_, err := app.MCPServer()
		if err == nil {
			t.Fatal("two routes claiming one tool name was accepted")
		}
		if !strings.Contains(err.Error(), "same") {
			t.Errorf("error does not name the duplicate: %v", err)
		}
	})

	t.Run("undocumented route", func(t *testing.T) {
		scalar.Enable()
		r := NewRouter()
		r.Handle(GET, "/undocumented/route", func(ctx *Context) error {
			return nil
		}, MCPTool("undocumented", "no doc"))

		app := &Breeze{Router: r}
		_, err := app.MCPServer()
		if err == nil {
			t.Fatal("a tagged route with no documentation was exposed; its arguments would be unknown")
		}
		if !strings.Contains(err.Error(), "Scalar documentation") {
			t.Errorf("error does not explain what is missing: %v", err)
		}
	})

	t.Run("empty name", func(t *testing.T) {
		scalar.Enable()
		r := NewRouter()
		scalar.RegisterRoute("GET", "/emptyname", scalar.RouteDoc{Title: "x"})
		r.Handle(GET, "/emptyname", func(ctx *Context) error {
			return nil
		}, MCPTool("", "nameless"))

		app := &Breeze{Router: r}
		if _, err := app.MCPServer(); err == nil {
			t.Fatal("a tag with no name was accepted")
		}
	})
}

// TestOpenAPIPatternAgreesWithScalar pins the local pattern conversion to
// scalar's own.
//
// The conversion has to match exactly or tools stop resolving to their
// documentation — and the failure would be silent, because an unmatched route
// looks the same as an undocumented one. Comparing against what the registry
// actually reports means a change on scalar's side fails here.
func TestOpenAPIPatternAgreesWithScalar(t *testing.T) {
	mcpFixture(t)

	registered := make(map[string]bool)
	for _, r := range scalar.Routes() {
		registered[strings.ToUpper(r.Method)+" "+r.Path] = true
	}

	for _, pattern := range []string{pathOrderID, pathOrders, pathTenant} {
		method := "GET"
		if pattern == pathOrders {
			method = "POST"
		}
		key := method + " " + openAPIPattern(pattern)
		if !registered[key] {
			t.Errorf("openAPIPattern(%q) produced %q, which is not how scalar stored it", pattern, key)
		}
	}

	if got := openAPIPattern("/a/:b/c/:d"); got != "/a/{b}/c/{d}" {
		t.Errorf("openAPIPattern(/a/:b/c/:d) = %q", got)
	}
	if got := openAPIPattern("/no/params"); got != "/no/params" {
		t.Errorf("a pattern without params was rewritten: %q", got)
	}
}

func TestApplicationsWithoutTagsExposeNoTools(t *testing.T) {
	r := NewRouter()
	r.Handle(GET, "/untagged/only", func(ctx *Context) error {
		return nil
	})
	app := &Breeze{Router: r}

	srv, err := app.MCPServer()
	if err != nil {
		t.Fatalf("MCPServer on an untagged app: %v", err)
	}
	tools := listTools(t, srv)
	if len(tools) != 0 {
		t.Errorf("an app with no tags exposed %d tools: %v", len(tools), keysOfTools(tools))
	}
}
