package main

// automcp_test.go — the four claims Auto-MCP makes, each asserted on its own.
//
// # Why these four and not one test
//
// Auto-MCP's value is a set of guarantees, and a monolithic test that walked a happy
// path would report "MCP works" or "MCP is broken" — which is not actionable. Each
// claim fails separately here, so a failure names which guarantee broke:
//
//  1. Schema parity. The tool's advertised schema says what the OpenAPI document says.
//     An agent reads the former, a human reads the latter, and if they disagree the
//     agent's calls are wrong in a way nobody reviewing the docs would notice.
//  2. A real handler ran. The tool result reflects logic that could not be echoed from
//     the input, so a stub could not pass.
//  3. An untagged route is absent. Not merely unlisted — unreachable by name.
//  4. Auth is enforced identically. The same middleware refuses the same call, proven
//     by running the request through the router's own dispatch path and comparing.
//
// # Why the tests drive rpc.Server.Handle rather than a socket
//
// The rpc package has no Stop, so a listener would own its port for the remainder of the
// process — and a package whose tests each want a fresh application would leak a port per
// test. Handle is the transport-independent entry point: same registry, same dispatch,
// same bytes back. What a socket would add is framing, which rpc's own tests cover.
//
// EnableMCP itself is exercised in TestEnableMCPValidatesEveryTagBeforeListening, which
// is the half that can be asserted without holding a port.

import (
	"encoding/json"
	"net"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/nelthaarion/breeze/v2"
	middleware "github.com/nelthaarion/breeze/v2/middlewares"
	"github.com/nelthaarion/breeze/v2/rpc"
	"github.com/nelthaarion/breeze/v2/scalar"
)

// testSecret is long enough to be a legitimate HMAC key. It is a test fixture, not a
// default: JWTAuthMiddleware panics on an empty secret, and main() generates a real one.
const testSecret = "automcp-example-test-secret-32-bytes"

// fixture builds the example's routes once and returns the pieces the tests need.
//
// Once, because scalar's registry is process-wide: registering the same route twice
// would leave duplicate entries and the OpenAPI document would describe the fixture
// twice. sync.Once is how mcp_auto_test.go in the root package solves the same problem.
var (
	fixtureApp    *breeze.Breeze
	fixtureRouter *breeze.Router
	fixtureSrv    *rpc.Server
	fixtureErr    error
	fixtureOnce   sync.Once
)

func fixture(t *testing.T) (*breeze.Breeze, *breeze.Router, *rpc.Server) {
	t.Helper()
	fixtureOnce.Do(func() {
		scalar.Enable()
		scalar.SetInfo("Auto-MCP Example API", "1.0.0", "test fixture")

		router := breeze.NewRouter()
		service := &app{store: newStore()}
		service.register(router, testSecret)

		application := breeze.New(router, breeze.NewEventLoopWorkerPool(2))
		fixtureApp, fixtureRouter = application, router
		fixtureSrv, fixtureErr = application.MCPServer()

	})
	if fixtureErr != nil {
		t.Fatalf("building the MCP server for the example's routes: %v", fixtureErr)
	}
	return fixtureApp, fixtureRouter, fixtureSrv
}

// token mints a valid access token for the JWT-protected route.
func token(t *testing.T, userID string) string {
	t.Helper()
	tok, err := middleware.GenerateJWT(testSecret, jwt.MapClaims{
		"user_id": userID,
		"role":    "operator",
	}, time.Hour, nil)
	if err != nil {
		t.Fatalf("minting a token: %v", err)
	}
	return tok
}

// ─── the MCP client side ─────────────────────────────────────────────────────

type rpcReply struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// rpcCall sends one JSON-RPC message and decodes the envelope.
func rpcCall(t *testing.T, srv *rpc.Server, method string, params any) rpcReply {
	t.Helper()

	req := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		req["params"] = params
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("encoding the %s request: %v", method, err)
	}
	out := srv.Handle(raw)
	if len(out) == 0 {
		t.Fatalf("%s produced no response", method)
	}
	var reply rpcReply
	if err := json.Unmarshal(out, &reply); err != nil {
		t.Fatalf("decoding the %s response %s: %v", method, out, err)
	}
	return reply
}

type listedTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// listTools performs tools/list, keyed by name.
func listTools(t *testing.T, srv *rpc.Server) map[string]listedTool {
	t.Helper()

	reply := rpcCall(t, srv, "tools/list", nil)
	if reply.Error != nil {
		t.Fatalf("tools/list failed: %d %s", reply.Error.Code, reply.Error.Message)
	}
	var body struct {
		Tools []listedTool `json:"tools"`
	}
	if err := json.Unmarshal(reply.Result, &body); err != nil {
		t.Fatalf("decoding the tool list: %v", err)
	}
	out := make(map[string]listedTool, len(body.Tools))
	for _, tool := range body.Tools {
		out[tool.Name] = tool
	}
	return out
}

// callResult is a tool result as a client receives it.
//
// StructuredContent is the part that matters here: it carries the HTTP status, body and
// headers the route produced, which is what lets a test compare an MCP call against an
// HTTP one field by field.
type callResult struct {
	StructuredContent struct {
		Tool     string         `json:"tool"`
		Method   string         `json:"method"`
		Route    string         `json:"route"`
		Path     string         `json:"path"`
		Status   int            `json:"status"`
		Body     string         `json:"body"`
		JSONBody map[string]any `json:"json_body"`
		Note     string         `json:"note"`
	} `json:"structuredContent"`
	IsError bool `json:"isError"`
}

// callTool performs tools/call. A protocol-level refusal is returned as the second
// value: "the call was malformed" and "the service said no" are different outcomes and a
// test should not have to guess which it got.
func callTool(t *testing.T, srv *rpc.Server, name string, args map[string]any) (callResult, *rpcReply) {
	t.Helper()

	params := map[string]any{"name": name}
	if args != nil {
		params["arguments"] = args
	}
	reply := rpcCall(t, srv, "tools/call", params)
	if reply.Error != nil {
		return callResult{}, &reply
	}
	var out callResult
	if err := json.Unmarshal(reply.Result, &out); err != nil {
		t.Fatalf("decoding the tool result: %v", err)
	}
	return out, nil
}

// runHTTP runs a request through the router's own dispatch path — the same lookup and the
// same chain OnTraffic uses — and reports what the chain wrote.
//
// This is the reference the MCP path is compared against in assertion 4. Anything less
// direct (a second fixture, a recorded expectation) would let the two drift while the
// test still passed.
func runHTTP(t *testing.T, router *breeze.Router, method breeze.Method, path string,
	headers map[string]string, body []byte) (int, string) {
	t.Helper()

	req := &breeze.HTTPRequest{Method: method, Path: path, Header: map[string]string{}, Body: body}
	for k, v := range headers {
		// Lowercased because that is what the HTTP parser produces and what middleware
		// reads; a mixed-case key here would be a header no middleware could find.
		req.Header[strings.ToLower(k)] = v
	}

	handler, globals, params := router.Find(req)
	if handler == nil {
		t.Fatalf("no route matched %s %s", method, path)
	}
	ctx := breeze.NewContext(method, path)
	ctx.Req = req
	if params != nil {
		ctx.SetParams(params)
	}
	ctx.SetMiddlewareChain(globals, handler)
	_ = ctx.Next()

	if ctx.Res == nil {
		return 0, ""
	}
	status := ctx.Res.Status
	if status == 0 {
		status = 200
	}
	return status, string(ctx.Res.Body)
}

// ─── 1. schema parity ────────────────────────────────────────────────────────

// TestToolSchemaMatchesTheRoutesOpenAPIShape is assertion 1.
//
// The comparison is against the *generated* document rather than a recorded copy of it,
// so the two cannot drift while this still passes. It runs in both directions: every
// declared parameter and body field must be an accepted argument of the same type, and
// every accepted argument must be something the document declares. A tool that invented
// an argument would be the same drift the other way round.
//
// Descriptions are compared too. A model reads the description to decide what to put in
// a field, so losing it is a silent downgrade in call quality that no type check sees.
func TestToolSchemaMatchesTheRoutesOpenAPIShape(t *testing.T) {
	_, _, srv := fixture(t)
	tools := listTools(t, srv)

	var doc scalar.OpenAPI
	if err := json.Unmarshal(scalar.Generate(), &doc); err != nil {
		t.Fatalf("decoding the generated OpenAPI document: %v", err)
	}

	cases := []struct {
		tool   string
		method string
		path   string
	}{
		{"create_order", "post", "/orders"},
		{"get_order", "get", "/orders/{id}"},
	}

	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			tool, listed := tools[tc.tool]
			if !listed {
				t.Fatalf("%s is not in the tool list: %v", tc.tool, toolNames(tools))
			}

			item, ok := doc.Paths[tc.path]
			if !ok {
				t.Fatalf("the OpenAPI document has no path %q", tc.path)
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
			if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
				t.Fatalf("decoding the tool's input schema: %v", err)
			}
			if schema.Type != "object" {
				t.Errorf("tool schema type = %q, want object", schema.Type)
			}

			// Forward: everything the document declares, the tool accepts.
			for _, param := range op.Parameters {
				prop, ok := schema.Properties[param.Name]
				if !ok {
					t.Errorf("OpenAPI declares the %s parameter %q, which the tool does not accept",
						param.In, param.Name)
					continue
				}
				if param.Schema != nil && prop.Type != param.Schema.Type {
					t.Errorf("parameter %q: tool type %q, OpenAPI type %q",
						param.Name, prop.Type, param.Schema.Type)
				}
				// A path parameter has to be required: without it there is no URL.
				if param.In == "path" && !contains(schema.Required, param.Name) {
					t.Errorf("path parameter %q is optional in the tool, but a URL cannot be built without it",
						param.Name)
				}
			}
			if op.RequestBody != nil {
				media, ok := op.RequestBody.Content["application/json"]
				if !ok || media.Schema == nil {
					t.Fatal("the OpenAPI request body has no application/json schema")
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
					if want.Description != "" && prop.Description != want.Description {
						t.Errorf("body field %q description: tool %q, OpenAPI %q",
							field, prop.Description, want.Description)
					}
				}
				for _, field := range media.Schema.Required {
					if !contains(schema.Required, field) {
						t.Errorf("body field %q is required by OpenAPI but optional in the tool", field)
					}
				}
			}

			// Reverse: the tool invents nothing.
			for field := range schema.Properties {
				if !declaredByOpenAPI(op, field) {
					t.Errorf("the tool accepts %q, which the OpenAPI document does not declare", field)
				}
			}
		})
	}
}

func declaredByOpenAPI(op scalar.Operation, field string) bool {
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

func toolNames(tools map[string]listedTool) []string {
	out := make([]string, 0, len(tools))
	for name := range tools {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ─── 2. the real handler ran ─────────────────────────────────────────────────

// TestCallingCreateOrderRunsTheRealHandler is assertion 2.
//
// A schema that matches the route proves nothing if the handler behind it is a stub, so
// this asserts on three things the handler computes and could not echo:
//
//   - id, which the store generates;
//   - unit_cents, looked up in the catalogue and never sent by the caller;
//   - total_cents, which must equal unit_cents × quantity.
//
// The 404 subtest is the other half of "real": the handler refuses an unknown SKU, and
// that refusal reaches the agent as a 404 with the same body an HTTP caller would get,
// not as a broken tool.
func TestCallingCreateOrderRunsTheRealHandler(t *testing.T) {
	_, _, srv := fixture(t)

	result, rpcErr := callTool(t, srv, "create_order", map[string]any{
		"sku":      "BRZ-100",
		"quantity": 3,
		"customer": "ada@example.com",
	})
	if rpcErr != nil {
		t.Fatalf("tools/call failed: %d %s", rpcErr.Error.Code, rpcErr.Error.Message)
	}
	if result.StructuredContent.Status != 201 {
		t.Fatalf("status = %d, want 201 (body %q)",
			result.StructuredContent.Status, result.StructuredContent.Body)
	}

	body := result.StructuredContent.JSONBody
	if id, _ := body["id"].(string); !strings.HasPrefix(id, "ord-") {
		t.Errorf("id = %v, want a generated ord-N; an echo handler could not produce one", body["id"])
	}
	// 1250 is BRZ-100's catalogue price. The caller never sent it.
	if unit, _ := body["unit_cents"].(float64); unit != 1250 {
		t.Errorf("unit_cents = %v, want 1250 from the catalogue", body["unit_cents"])
	}
	if total, _ := body["total_cents"].(float64); total != 3750 {
		t.Errorf("total_cents = %v, want 3750 (1250 × 3); the handler did not do the arithmetic",
			body["total_cents"])
	}
	if status, _ := body["status"].(string); status != "confirmed" {
		t.Errorf("status = %v, want confirmed", body["status"])
	}

	t.Run("an unknown sku is refused as a 404, not a broken tool", func(t *testing.T) {
		result, rpcErr := callTool(t, srv, "create_order", map[string]any{
			"sku":      "NOPE-1",
			"quantity": 1,
			"customer": "ada@example.com",
		})
		if rpcErr != nil {
			t.Fatalf("a refusal arrived as a protocol error (%d %s); a refused call is a result",
				rpcErr.Error.Code, rpcErr.Error.Message)
		}
		if result.StructuredContent.Status != 404 {
			t.Errorf("status = %d, want 404", result.StructuredContent.Status)
		}
		if !result.IsError {
			t.Error("a 404 was not reported as an error result")
		}
	})

	t.Run("validation still applies", func(t *testing.T) {
		// quantity 0 fails validate:"required,min=1". The tool advertises the field, so
		// the call is well-formed; it is the *value* the route rejects — which is a 422
		// from ctx.Bind, exactly as over HTTP.
		result, rpcErr := callTool(t, srv, "create_order", map[string]any{
			"sku":      "BRZ-100",
			"quantity": 0,
			"customer": "ada@example.com",
		})
		if rpcErr != nil {
			t.Fatalf("validation failure arrived as a protocol error: %s", rpcErr.Error.Message)
		}
		if result.StructuredContent.Status != 422 {
			t.Errorf("status = %d, want 422 from the binding layer (body %q)",
				result.StructuredContent.Status, result.StructuredContent.Body)
		}
	})
}

// ─── 3. the untagged route is unreachable ────────────────────────────────────

// TestTheUntaggedRouteIsNeitherListedNorCallable is assertion 3.
//
// Two separate claims, and the second is the one worth having. Absent from tools/list is
// what a well-behaved agent sees. Unreachable by name is what a badly-behaved one gets:
// there is no dispatch path from a tool name to an untagged route, so guessing does not
// work either.
//
// The route is deliberately documented, so it *is* in the OpenAPI document. That is the
// point of the pairing — discoverability and callability are different properties, and
// only the tag decides the second.
func TestTheUntaggedRouteIsNeitherListedNorCallable(t *testing.T) {
	_, router, srv := fixture(t)
	tools := listTools(t, srv)

	// Exactly the two tagged routes, and nothing else.
	want := []string{"create_order", "get_order"}
	if got := toolNames(tools); len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("tool list = %v, want %v; an untagged route may have leaked in", got, want)
	}
	for name, tool := range tools {
		if strings.Contains(strings.ToLower(tool.Description), "internal") {
			t.Errorf("tool %q describes the internal route", name)
		}
	}

	// Not reachable by guessing, under any plausible name.
	for _, guess := range []string{"internal_metrics", "get_internal_metrics", "metrics"} {
		result, rpcErr := callTool(t, srv, guess, nil)
		if rpcErr == nil {
			t.Errorf("calling %q succeeded; the untagged route is reachable (status %d)",
				guess, result.StructuredContent.Status)
			continue
		}
		// The refusal lists what does exist, so a model that guessed can correct itself.
		// Only that list is checked, not the whole message: the message echoes the name
		// the caller asked for, so "internal_metrics" appears in it because the caller
		// typed it — which says nothing about what the server exposes.
		if offered := availableFrom(rpcErr.Error.Message); strings.Contains(offered, "internal") ||
			strings.Contains(offered, "metrics") {
			t.Errorf("the refusal for %q offers the untagged route: %q", guess, offered)
		}
	}

	// And the route genuinely works over HTTP — otherwise this test would pass for the
	// uninteresting reason that the route is broken.
	status, body := runHTTP(t, router, breeze.GET, "/internal/metrics", nil, nil)
	if status != 200 {
		t.Errorf("GET /internal/metrics = %d over HTTP; the exclusion must be the tag, not a broken route", status)
	}
	if !strings.Contains(body, `"internal":true`) {
		t.Errorf("unexpected metrics body: %s", body)
	}

	// It is in the OpenAPI document, which is the half that is *supposed* to be visible.
	var doc scalar.OpenAPI
	if err := json.Unmarshal(scalar.Generate(), &doc); err != nil {
		t.Fatalf("decoding the OpenAPI document: %v", err)
	}
	if _, documented := doc.Paths["/internal/metrics"]; !documented {
		t.Error("the untagged route is missing from the OpenAPI document, so this test no longer " +
			"distinguishes documented-but-untagged from simply absent")
	}
}

// availableFrom extracts the "(available: ...)" list from an unknown-tool refusal.
//
// Needed because the refusal necessarily echoes the name the caller asked for, so a
// substring search over the whole message would report the caller's own guess as a leak.
// What matters is what the server offers back. An unrecognised message shape yields "",
// which fails no assertion — the tests around this one already prove the tool is absent.
func availableFrom(message string) string {
	_, rest, found := strings.Cut(message, "(available:")
	if !found {
		return ""
	}
	list, _, _ := strings.Cut(rest, ")")
	return strings.TrimSpace(list)
}

// ─── 4. auth enforcement ─────────────────────────────────────────────────────

// TestGetOrderIsRefusedOverMCPExactlyAsOverHTTP is assertion 4, and the security one.
//
// The route is behind JWTAuthMiddleware. If MCP dispatch bypassed the chain — or rebuilt
// an equivalent one that dropped a step — this is where it would show.
//
// The assertion is a *comparison*, not a hardcoded 401. Running the same request through
// the router's own dispatch path and requiring both status and body to match is what
// demonstrates the same middleware ran; asserting 401 alone would also pass if MCP had
// its own separate refusal that happened to use the same code.
func TestGetOrderIsRefusedOverMCPExactlyAsOverHTTP(t *testing.T) {
	_, router, srv := fixture(t)

	// Seed an order so a successful read has something to find. Placed through the tool,
	// which also means the two subtests below are reading a real record.
	seed, rpcErr := callTool(t, srv, "create_order", map[string]any{
		"sku":      "BRZ-200",
		"quantity": 1,
		"customer": "grace@example.com",
	})
	if rpcErr != nil {
		t.Fatalf("seeding an order: %s", rpcErr.Error.Message)
	}
	orderID, _ := seed.StructuredContent.JSONBody["id"].(string)
	if orderID == "" {
		t.Fatalf("the seeded order has no id: %+v", seed.StructuredContent.JSONBody)
	}

	t.Run("without credentials", func(t *testing.T) {
		result, rpcErr := callTool(t, srv, "get_order", map[string]any{"id": orderID})
		if rpcErr != nil {
			t.Fatalf("the refusal arrived as a protocol error (%d %s); a refused call is a "+
				"result, not a broken tool", rpcErr.Error.Code, rpcErr.Error.Message)
		}
		if result.StructuredContent.Status != 401 {
			t.Errorf("status = %d, want 401 (body %q)",
				result.StructuredContent.Status, result.StructuredContent.Body)
		}
		if !result.IsError {
			t.Error("a 401 was not reported as an error result")
		}

		// The comparison that proves it was the same middleware, not a parallel one.
		wantStatus, wantBody := runHTTP(t, router, breeze.GET, "/orders/"+orderID, nil, nil)
		if result.StructuredContent.Status != wantStatus {
			t.Errorf("MCP status %d, HTTP status %d — the two paths do not agree",
				result.StructuredContent.Status, wantStatus)
		}
		if result.StructuredContent.Body != wantBody {
			t.Errorf("MCP body %q, HTTP body %q", result.StructuredContent.Body, wantBody)
		}
	})

	t.Run("with a forged token", func(t *testing.T) {
		// Signed with the wrong key. This is the case an empty-secret bug would let
		// through, so it is asserted rather than assumed.
		forged, err := middleware.GenerateJWT("not-the-real-signing-secret-at-all",
			jwt.MapClaims{"user_id": "attacker", "role": "admin"}, time.Hour, nil)
		if err != nil {
			t.Fatal(err)
		}
		result, rpcErr := callTool(t, srv, "get_order", map[string]any{
			"id":            orderID,
			"authorization": "Bearer " + forged,
		})
		if rpcErr != nil {
			t.Fatalf("tools/call failed: %s", rpcErr.Error.Message)
		}
		if result.StructuredContent.Status != 401 {
			t.Errorf("a token signed with the wrong key was accepted: status %d, body %q",
				result.StructuredContent.Status, result.StructuredContent.Body)
		}
	})

	t.Run("with valid credentials", func(t *testing.T) {
		valid := token(t, "grace")
		result, rpcErr := callTool(t, srv, "get_order", map[string]any{
			"id":            orderID,
			"authorization": "Bearer " + valid,
		})
		if rpcErr != nil {
			t.Fatalf("tools/call failed: %s", rpcErr.Error.Message)
		}
		if result.StructuredContent.Status != 200 {
			t.Fatalf("status = %d, want 200 (body %q)",
				result.StructuredContent.Status, result.StructuredContent.Body)
		}
		// read_by comes from the claims the middleware stored, so its presence is proof
		// the chain ran to completion rather than being skipped.
		if got, _ := result.StructuredContent.JSONBody["read_by"].(string); got != "grace" {
			t.Errorf("read_by = %q, want grace; the JWT claims did not reach the handler", got)
		}

		wantStatus, wantBody := runHTTP(t, router, breeze.GET, "/orders/"+orderID,
			map[string]string{"Authorization": "Bearer " + valid}, nil)
		if result.StructuredContent.Status != wantStatus || result.StructuredContent.Body != wantBody {
			t.Errorf("MCP %d %q, HTTP %d %q — the two paths do not agree on a successful call",
				result.StructuredContent.Status, result.StructuredContent.Body, wantStatus, wantBody)
		}
	})

	t.Run("an undeclared header cannot be smuggled in", func(t *testing.T) {
		// create_order declares no header inputs, so none of these are advertised
		// arguments. Each must be refused by name rather than quietly dropped.
		for _, forged := range []string{"authorization", "cookie", "x-forwarded-for"} {
			_, rpcErr := callTool(t, srv, "create_order", map[string]any{
				"sku":      "BRZ-100",
				"quantity": 1,
				"customer": "ada@example.com",
				forged:     "smuggled",
			})
			if rpcErr == nil {
				t.Errorf("header %q was accepted on a route that never declared it", forged)
				continue
			}
			if !strings.Contains(rpcErr.Error.Message, forged) {
				t.Errorf("the rejection does not name %q: %q", forged, rpcErr.Error.Message)
			}
		}
	})
}

// ─── EnableMCP itself ────────────────────────────────────────────────────────

// TestEnableMCPValidatesEveryTagBeforeListening covers the half of EnableMCP that can be
// asserted without holding a port for the rest of the process.
//
// A tag on an undocumented route produces a tool whose arguments are unknown, which an
// agent would call incorrectly. EnableMCP returns that as an error instead of serving a
// partial tool list — so main() stops at startup, where the author can still fix it,
// rather than on an agent's first call in production.
func TestEnableMCPValidatesEveryTagBeforeListening(t *testing.T) {
	router := breeze.NewRouter()
	router.Handle(breeze.GET, "/automcp-test/undocumented", func(ctx *breeze.Context) error {
		return ctx.JSON(map[string]any{"ok": true})
	}, breeze.MCPTool("undocumented_tool", "has no RouteDoc"))

	application := breeze.New(router, breeze.NewEventLoopWorkerPool(1))

	// Port 0 would bind successfully; the configuration error has to be found first.
	err := application.EnableMCP("127.0.0.1:0")
	if err == nil {
		t.Fatal("EnableMCP started a listener for a tool whose arguments are unknown")
	}
	if !strings.Contains(err.Error(), "Scalar documentation") {
		t.Errorf("the error does not explain what is missing: %v", err)
	}
}

// TestEnableMCPRecordsAnAddressTheConflictCheckCanRead is the seam between Auto-MCP and
// the embedded introspection endpoint.
//
// mcp.StartInProcess refuses the port this endpoint took, and it detects that by parsing
// the recorded address with net.SplitHostPort. So the address has to be recorded in the
// form that parses — plain host:port. Recording the gnet form ("tcp://host:port") would
// make the collision check answer "no conflict" for every address, and two MCP servers
// would end up sharing a port with each answering some of the other's requests.
func TestEnableMCPRecordsAnAddressTheConflictCheckCanRead(t *testing.T) {
	router := breeze.NewRouter()
	application := breeze.New(router, breeze.NewEventLoopWorkerPool(1))

	// No tagged routes, so this builds an empty tool table and serves nothing useful —
	// enough here, because what is under test is the recorded address.
	if err := application.EnableMCP("127.0.0.1:0"); err != nil {
		t.Fatalf("EnableMCP: %v", err)
	}

	addr := application.AutoMCPAddr()
	if addr == "" {
		t.Fatal("EnableMCP recorded no address, so a port conflict could not be detected")
	}
	if strings.Contains(addr, "://") {
		t.Errorf("recorded address %q carries a scheme; net.SplitHostPort cannot parse it, "+
			"so mcp.StartInProcess would miss every port conflict", addr)
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		t.Errorf("recorded address %q does not parse as host:port: %v", addr, err)
	}
}
