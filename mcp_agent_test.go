package breeze

// mcp_agent_test.go — one agent, start to finish, against a real service.
//
// The other Auto-MCP tests each pin a single property. This one walks the whole
// journey in the order an agent actually experiences it: discover the tools,
// read a schema, get the call wrong, get it wrong differently, hit the
// authentication wall, succeed, and then meet a route that is simply broken.
//
// It exists because the individual properties can all hold while the sequence
// is still unusable. A tool list that omits the schema, an error that does not
// say which argument was wrong, a 500 that looks like a 401 — none of those
// fail a narrow test, and all of them leave an agent stuck in a retry loop. The
// only way to catch that is to make the test take the same steps and require
// that each one produces enough information to reach the next.
//
// Every stage is also compared against the same request made over HTTP, so
// "the agent saw what a client would have seen" is checked rather than assumed.

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/nelthaarion/breeze/v2/scalar"
)

const (
	agentAPIKey      = "agent-sim-key"
	pathAgentReport  = "/agentsim/reports/:id"
	pathAgentBroken  = "/agentsim/breakable"
	pathAgentPrivate = "/agentsim/admin/secrets"
)

type reportParams struct {
	ID string `json:"id" description:"Identifier of the report to fetch."`
}

type reportAuth struct {
	Key string `json:"x-api-key" description:"API key authorising the read."`
}

// requireAgentKey is route middleware, deliberately unaware that MCP exists.
// Placing the check here rather than in the handler is what makes the auth
// stage of the journey a test of chain reuse.
func requireAgentKey(ctx *Context) error {
	if ctx.Req.Header["x-api-key"] != agentAPIKey {
		ctx.Status(401)
		_ = ctx.JSON(map[string]any{"error": "api key required"})
		ctx.Abort()
		return nil
	}
	return ctx.Next()
}

var (
	agentOnce   sync.Once
	agentApp    *Breeze
	agentRouter *Router
)

func agentFixture(t *testing.T) (*Breeze, *Router) {
	t.Helper()
	agentOnce.Do(func() {
		scalar.Enable()
		r := NewRouter()

		scalar.RegisterRoute("GET", pathAgentReport, scalar.RouteDoc{
			Title: "Fetch a report",
			Input: []scalar.InputGroup{
				{Type: scalar.InputParams, Fields: reportParams{}},
				{Type: scalar.InputHeader, Fields: reportAuth{}},
			},
			Output: map[string]any{},
		})
		r.Handle(GET, pathAgentReport, func(ctx *Context) error {
			id := ctx.Param("id")
			if id != "r-1" {
				ctx.Status(404)
				return ctx.JSON(map[string]any{"error": "no such report", "id": id})
			}
			return ctx.JSON(map[string]any{"id": id, "title": "Quarterly numbers"})
		}, requireAgentKey, MCPTool("fetch_report", "Fetches one report by its identifier."))

		// A route that fails on its own. An agent must be able to tell this
		// apart from a refusal, because one is worth reporting to a human and
		// the other is worth fixing in the call.
		scalar.RegisterRoute("GET", pathAgentBroken, scalar.RouteDoc{
			Title:  "A route that fails",
			Output: map[string]any{},
		})
		r.Handle(GET, pathAgentBroken, func(ctx *Context) error {
			ctx.Status(500)
			return ctx.JSON(map[string]any{"error": "downstream unavailable"})
		}, MCPTool("trigger_breakable", "Calls a dependency that is currently failing."))

		// Documented, reachable over HTTP, and never a tool.
		scalar.RegisterRoute("GET", pathAgentPrivate, scalar.RouteDoc{
			Title:  "Administrative secrets",
			Output: map[string]any{},
		})
		r.Handle(GET, pathAgentPrivate, func(ctx *Context) error {
			return ctx.JSON(map[string]any{"secret": "do not expose"})
		})

		agentRouter = r
		agentApp = &Breeze{Router: r}
	})
	return agentApp, agentRouter
}

func TestAnAgentCanGoFromDiscoveryToASuccessfulCall(t *testing.T) {
	app, router := agentFixture(t)
	srv, err := app.MCPServer()
	if err != nil {
		t.Fatalf("MCPServer: %v", err)
	}

	// ── 1. Handshake ────────────────────────────────────────────────────────
	if reply := mcpRPC(t, srv, "initialize", map[string]any{
		"protocolVersion": mcpProtocolVersion,
	}); reply.Error != nil {
		t.Fatalf("initialize: %d %s", reply.Error.Code, reply.Error.Message)
	}

	// ── 2. Discovery ────────────────────────────────────────────────────────
	tools := listTools(t, srv)
	if len(tools) != 2 {
		t.Fatalf("discovered %d tools, want 2: %v", len(tools), keysOfTools(tools))
	}
	report, ok := tools["fetch_report"]
	if !ok {
		t.Fatal("fetch_report was not discovered")
	}
	for name, tool := range tools {
		if strings.Contains(strings.ToLower(tool.Description), "administrative") {
			t.Errorf("tool %q exposes the untagged admin route", name)
		}
	}

	// ── 3. Reading the schema ───────────────────────────────────────────────
	// The agent has to learn the argument names from here; if the schema is
	// silent, every later stage is guesswork.
	var schema struct {
		Type       string                    `json:"type"`
		Properties map[string]*scalar.Schema `json:"properties"`
		Required   []string                  `json:"required"`
	}
	if err := json.Unmarshal(report.InputSchema, &schema); err != nil {
		t.Fatalf("decoding fetch_report schema: %v", err)
	}
	for _, want := range []string{"id", "x-api-key"} {
		if _, ok := schema.Properties[want]; !ok {
			t.Fatalf(
				"schema does not advertise %q; the agent could not know to send it: %v",
				want,
				schema.Properties,
			)
		}
	}
	if !contains(schema.Required, "id") {
		t.Error("id is not marked required, yet no URL exists without it")
	}
	if desc := schema.Properties["id"].Description; !strings.Contains(desc, "Identifier") {
		t.Errorf("id has no usable description: %q", desc)
	}

	// ── 4. A call with an invented argument ─────────────────────────────────
	// A model that guesses "report_id" must be told the real name, not handed
	// a silently-dropped field and a confusing 404.
	_, rpcErr := callTool(t, srv, "fetch_report", map[string]any{
		"report_id": "r-1", "x-api-key": agentAPIKey,
	})
	if rpcErr == nil {
		t.Fatal(
			"an invented argument was accepted; the agent would have seen a 404 and blamed the data",
		)
	}
	if !strings.Contains(rpcErr.Error.Message, "report_id") ||
		!strings.Contains(rpcErr.Error.Message, "id") {
		t.Errorf("error does not name the bad argument and the good ones: %q", rpcErr.Error.Message)
	}

	// ── 5. A call missing the required path argument ────────────────────────
	_, rpcErr = callTool(t, srv, "fetch_report", map[string]any{"x-api-key": agentAPIKey})
	if rpcErr == nil {
		t.Fatal("a call with no id was sent; its path would have contained a literal :id")
	}
	if !strings.Contains(rpcErr.Error.Message, "id") {
		t.Errorf("error does not name the missing argument: %q", rpcErr.Error.Message)
	}

	// ── 6. The authentication wall ──────────────────────────────────────────
	result, rpcErr := callTool(t, srv, "fetch_report", map[string]any{"id": "r-1"})
	if rpcErr != nil {
		t.Fatalf(
			"a refusal arrived as a protocol error: %d %s",
			rpcErr.Error.Code,
			rpcErr.Error.Message,
		)
	}
	if result.StructuredContent.Status != 401 {
		t.Fatalf("status = %d, want 401", result.StructuredContent.Status)
	}
	if !result.IsError {
		t.Error("a 401 was not flagged as an error result")
	}
	if !strings.Contains(result.StructuredContent.Note, "credentials") {
		t.Errorf(
			"note does not tell the agent the problem is credentials: %q",
			result.StructuredContent.Note,
		)
	}
	wantStatus, wantBody := runHTTP(t, router, GET, "/agentsim/reports/r-1", nil, nil)
	if result.StructuredContent.Status != wantStatus || result.StructuredContent.Body != wantBody {
		t.Errorf(
			"MCP %d %q, HTTP %d %q",
			result.StructuredContent.Status,
			result.StructuredContent.Body,
			wantStatus,
			wantBody,
		)
	}

	// ── 7. The successful call ──────────────────────────────────────────────
	result, rpcErr = callTool(t, srv, "fetch_report", map[string]any{
		"id": "r-1", "x-api-key": agentAPIKey,
	})
	if rpcErr != nil {
		t.Fatalf("the corrected call failed: %d %s", rpcErr.Error.Code, rpcErr.Error.Message)
	}
	if result.StructuredContent.Status != 200 {
		t.Fatalf(
			"status = %d, want 200 (body %q)",
			result.StructuredContent.Status,
			result.StructuredContent.Body,
		)
	}
	if result.IsError {
		t.Error("a 200 was flagged as an error result")
	}
	if got := result.StructuredContent.JSONBody["title"]; got != "Quarterly numbers" {
		t.Errorf("title = %v; the agent did not receive the payload", got)
	}
	if result.StructuredContent.Path != "/agentsim/reports/r-1" {
		t.Errorf("path = %q, want /agentsim/reports/r-1", result.StructuredContent.Path)
	}
	// The rendered text is what a client without structured support displays.
	if len(result.Content) == 0 || !strings.Contains(result.Content[0].Text, "Quarterly numbers") {
		t.Error("the text content does not carry the result")
	}
	wantStatus, wantBody = runHTTP(t, router, GET, "/agentsim/reports/r-1",
		map[string]string{"X-API-Key": agentAPIKey}, nil)
	if result.StructuredContent.Status != wantStatus || result.StructuredContent.Body != wantBody {
		t.Errorf(
			"MCP %d %q, HTTP %d %q",
			result.StructuredContent.Status,
			result.StructuredContent.Body,
			wantStatus,
			wantBody,
		)
	}

	// ── 8. A missing resource, which is not a refusal ───────────────────────
	result, rpcErr = callTool(t, srv, "fetch_report", map[string]any{
		"id": "r-nope", "x-api-key": agentAPIKey,
	})
	if rpcErr != nil {
		t.Fatalf("tools/call: %d %s", rpcErr.Error.Code, rpcErr.Error.Message)
	}
	if result.StructuredContent.Status != 404 {
		t.Fatalf("status = %d, want 404", result.StructuredContent.Status)
	}
	if !strings.Contains(result.StructuredContent.Note, "path arguments") {
		t.Errorf("a 404 note should point at the arguments, got %q", result.StructuredContent.Note)
	}

	// ── 9. A route that is simply broken ────────────────────────────────────
	// The distinction that matters: this is not something the agent can fix by
	// calling differently, and the note has to say so.
	result, rpcErr = callTool(t, srv, "trigger_breakable", nil)
	if rpcErr != nil {
		t.Fatalf("tools/call: %d %s", rpcErr.Error.Code, rpcErr.Error.Message)
	}
	if result.StructuredContent.Status != 500 {
		t.Fatalf("status = %d, want 500", result.StructuredContent.Status)
	}
	if !result.IsError {
		t.Error("a 500 was not flagged as an error result")
	}
	if !strings.Contains(result.StructuredContent.Note, "fault in the service") {
		t.Errorf(
			"a 500 note should say the fault is not in the call, got %q",
			result.StructuredContent.Note,
		)
	}
	wantStatus, wantBody = runHTTP(t, router, GET, pathAgentBroken, nil, nil)
	if result.StructuredContent.Status != wantStatus || result.StructuredContent.Body != wantBody {
		t.Errorf(
			"MCP %d %q, HTTP %d %q",
			result.StructuredContent.Status,
			result.StructuredContent.Body,
			wantStatus,
			wantBody,
		)
	}

	// ── 10. Every refusal was distinguishable ───────────────────────────────
	// Four outcomes, four different notes. If any two collided, an agent could
	// not choose between retrying, re-authenticating, and giving up.
	notes := map[string]bool{}
	for _, call := range []struct {
		tool string
		args map[string]any
	}{
		{"fetch_report", map[string]any{"id": "r-1"}},
		{"fetch_report", map[string]any{"id": "r-nope", "x-api-key": agentAPIKey}},
		{"trigger_breakable", nil},
	} {
		out, err := callTool(t, srv, call.tool, call.args)
		if err != nil {
			t.Fatalf("tools/call %s: %d %s", call.tool, err.Error.Code, err.Error.Message)
		}
		if notes[out.StructuredContent.Note] {
			t.Errorf("note %q is reused for a different outcome", out.StructuredContent.Note)
		}
		notes[out.StructuredContent.Note] = true
	}
}

// TestTheAgentJourneyNeverReachesAnUntaggedRoute closes the loop on the
// discovery stage: the admin route is documented and served, and no sequence of
// MCP calls reaches it.
func TestTheAgentJourneyNeverReachesAnUntaggedRoute(t *testing.T) {
	app, router := agentFixture(t)
	srv, err := app.MCPServer()
	if err != nil {
		t.Fatalf("MCPServer: %v", err)
	}

	// It is genuinely there over HTTP.
	if status, body := runHTTP(
		t,
		router,
		GET,
		pathAgentPrivate,
		nil,
		nil,
	); status != 200 ||
		!strings.Contains(body, "do not expose") {
		t.Fatalf("the admin route does not serve as expected: %d %q", status, body)
	}

	// And it is in the OpenAPI document, so discoverability is not the guard.
	var doc scalar.OpenAPI
	if err := json.Unmarshal(scalar.Generate(), &doc); err != nil {
		t.Fatalf("decoding OpenAPI: %v", err)
	}
	if _, ok := doc.Paths[pathAgentPrivate]; !ok {
		t.Fatalf("the admin route is missing from OpenAPI, which would make this test vacuous")
	}

	// Yet no name reaches it.
	for _, guess := range []string{
		"admin_secrets", "get_secrets", "administrative_secrets",
		pathAgentPrivate, "GET " + pathAgentPrivate,
	} {
		if _, rpcErr := callTool(t, srv, guess, nil); rpcErr == nil {
			t.Errorf("the untagged admin route was reachable as %q", guess)
		}
	}
}

// TestAToolCallCannotForgeAHeaderTheRouteDidNotDeclare is the containment
// property behind the whole design.
//
// Arguments are delivered only where the schema said they go, and a header that
// was never declared is not an argument at all. Without this, any tool call
// could set any header — Authorization included — and the route's middleware
// would believe it.
func TestAToolCallCannotForgeAHeaderTheRouteDidNotDeclare(t *testing.T) {
	app, _ := agentFixture(t)
	srv, err := app.MCPServer()
	if err != nil {
		t.Fatalf("MCPServer: %v", err)
	}

	for _, forged := range []string{"authorization", "Authorization", "x-forwarded-for", "cookie"} {
		_, rpcErr := callTool(t, srv, "trigger_breakable", map[string]any{forged: "smuggled"})
		if rpcErr == nil {
			t.Errorf("header %q was accepted on a route that never declared it", forged)
			continue
		}
		if !strings.Contains(rpcErr.Error.Message, forged) {
			t.Errorf("rejection does not name %q: %q", forged, rpcErr.Error.Message)
		}
	}
}

// TestEnableMCPReportsConfigurationErrorsBeforeListening checks the half of
// EnableMCP that can be tested without owning a port for the rest of the
// process: a misdescribed tool must be refused synchronously, so a bad build
// fails at startup instead of on an agent's first call.
func TestEnableMCPReportsConfigurationErrorsBeforeListening(t *testing.T) {
	scalar.Enable()
	r := NewRouter()
	r.Handle(GET, "/enablemcp/undocumented", func(ctx *Context) error {
		return nil
	}, MCPTool("enable_undocumented", "x"))
	app := &Breeze{Router: r}

	// Port 0 would bind, but the configuration error must be found first.
	if err := app.EnableMCP("127.0.0.1:0"); err == nil {
		t.Fatal("EnableMCP started a listener for a tool whose arguments are unknown")
	} else if !strings.Contains(err.Error(), "Scalar documentation") {
		t.Errorf("error does not explain what is missing: %v", err)
	}
}
