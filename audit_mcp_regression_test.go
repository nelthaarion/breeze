package breeze

// Regression tests for the audit findings in the MCP and router categories:
//
//   - a tool argument that was an array or object for a path/query/header slot was
//     silently turned into "", so the parameter vanished;
//   - JSON bodies were decoded into `any`, so integers above 2^53 were rounded;
//   - every JSON-RPC response was decoded and re-encoded even when nothing changed;
//   - a caller-supplied token was logged whenever BREEZE_MCP_TOKEN was unset;
//   - a CORS preflight to a path with no OPTIONS route answered 404 before any
//     middleware ran.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestStructuredPathArgumentIsRefusedNotDropped(t *testing.T) {
	srv := mcpServerFor(t)
	for name, args := range map[string]map[string]any{
		"array in a path slot":  {"id": []string{"1", "2"}},
		"object in a path slot": {"id": map[string]any{"a": 1}},
	} {
		_, rpcErr := callTool(t, srv, "get_order", args)
		if rpcErr == nil {
			t.Errorf("%s: call went through with the argument silently emptied", name)
			continue
		}
		if !strings.Contains(rpcErr.Error.Message, `"id"`) {
			t.Errorf("%s: error does not name the argument: %q", name, rpcErr.Error.Message)
		}
	}
}

func TestQueryArgumentArrayAndObject(t *testing.T) {
	srv := mcpServerFor(t)

	res, rpcErr := callTool(t, srv, "get_order", map[string]any{"id": "7", "expand": []string{"items", "customer"}})
	if rpcErr != nil {
		t.Fatalf("an array of scalars is a valid repeated query parameter: %s", rpcErr.Error.Message)
	}
	if got := res.StructuredContent.JSONBody["expand"]; got != "items" {
		t.Errorf("expand = %v, want the first repeated value %q (it used to vanish)", got, "items")
	}

	if _, rpcErr := callTool(t, srv, "get_order", map[string]any{"id": "7", "expand": map[string]any{"a": 1}}); rpcErr == nil {
		t.Error("an object in a query slot must be refused")
	}
	if _, rpcErr := callTool(t, srv, "get_order", map[string]any{"id": "7", "expand": [][]string{{"a"}}}); rpcErr == nil {
		t.Error("nested arrays in a query slot must be refused")
	}
}

func TestMCPResultKeepsLargeIntegersExact(t *testing.T) {
	const id = "9007199254740993" // 2^53 + 1: not representable as float64
	tool := &mcpTool{name: "t", rt: &route{method: GET, pattern: "/x"}}
	ctx := NewContext(GET, "/x")
	ctx.Res = &HTTPResponse{Status: 200, Body: []byte(`{"id":` + id + `}`)}

	res := mcpResultFrom(tool, "/x", ctx, nil)
	wire, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(wire, []byte(`"json_body":{"id":`+id+`}`)) {
		t.Fatalf("integer precision lost in json_body: %s", wire)
	}
}

func TestMCPTextBlockDoesNotRepeatTheParsedBody(t *testing.T) {
	tool := &mcpTool{name: "t", rt: &route{method: GET, pattern: "/x"}}
	ctx := NewContext(GET, "/x")
	ctx.Res = &HTTPResponse{Status: 200, Body: []byte(`{"k":"unique-marker-123"}`)}
	res := mcpResultFrom(tool, "/x", ctx, nil)
	text := res.Content[0]["text"].(string)
	if n := strings.Count(text, "unique-marker-123"); n != 1 {
		t.Fatalf("the body appears %d times in the text block, want 1", n)
	}
}

func TestDecorateLeavesUnrelatedResponsesUntouched(t *testing.T) {
	big := []byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[],"structuredContent":{"json_body":{"id":9007199254740993}}}}`)
	call := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x"}}`)
	for _, v := range []string{autoMCPProtocolLegacy, autoMCPProtocolModern} {
		if got := decorateAutoMCPResponse(big, call, v); !bytes.Equal(got, big) {
			t.Fatalf("version %s: tools/call response was rewritten:\n%s", v, got)
		}
	}
}

func TestDecorateStillEditsInitializeAndModernListing(t *testing.T) {
	init := []byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"x","big":9007199254740993}}`)
	req := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	got := string(decorateAutoMCPResponse(init, req, autoMCPProtocolLegacy))
	if !strings.Contains(got, `"protocolVersion":"`+autoMCPProtocolLegacy+`"`) {
		t.Errorf("initialize must report the negotiated version: %s", got)
	}
	if !strings.Contains(got, `9007199254740993`) {
		t.Errorf("fields that are not edited must survive byte for byte: %s", got)
	}

	list := []byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[]}}`)
	lreq := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	got = string(decorateAutoMCPResponse(list, lreq, autoMCPProtocolModern))
	if !strings.Contains(got, `"ttlMs":0`) || !strings.Contains(got, `"cacheScope":"private"`) {
		t.Errorf("modern tools/list must carry caching hints: %s", got)
	}
}

func TestSuppliedMCPTokenIsNeverLogged(t *testing.T) {
	const secret = "s3cret-token-from-the-secret-store"
	t.Setenv("BREEZE_MCP_TOKEN", "") // the case that used to leak

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	mcpFixture(t)
	app := &Breeze{Router: mcpFixtureRouter}
	if err := app.EnableMCPWithToken("127.0.0.1:0", secret); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if h := app.mcpHTTP.Load(); h != nil {
			_ = h.Shutdown(ctx)
		}
	}()
	if strings.Contains(buf.String(), secret) {
		t.Fatalf("a caller-supplied token was written to the log:\n%s", buf.String())
	}
}

func TestPreflightToAnUnroutedPathReachesGlobalMiddleware(t *testing.T) {
	router := NewRouter()
	app := New(router, NewEventLoopWorkerPool(runtime.NumCPU()))
	router.Use(func(ctx *Context) error {
		if ctx.Req.Method == OPTIONS {
			ctx.SetHeader("Access-Control-Allow-Origin", "https://app.example")
			ctx.Status(204)
			ctx.Abort()
			return nil
		}
		return ctx.Next()
	})
	router.Handle(POST, "/api/data", func(c *Context) error { return c.WriteString("posted") })

	port := startAuditServer(t, app)
	c := dial(t, port)
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = c.Write([]byte("OPTIONS /api/data HTTP/1.1\r\nHost: x\r\nOrigin: https://app.example\r\n" +
		"Access-Control-Request-Method: POST\r\n\r\n"))
	head, _ := readResponse(t, bufio.NewReader(c))
	if !strings.HasPrefix(head, "HTTP/1.1 204") {
		t.Fatalf("preflight must be answered by the middleware, got %q", strings.SplitN(head, "\r\n", 2)[0])
	}
	if !strings.Contains(strings.ToLower(head), "access-control-allow-origin: https://app.example") {
		t.Fatalf("preflight response lacks the CORS header: %q", head)
	}
}

func TestPreflightWithoutAnAnsweringMiddlewareStill404s(t *testing.T) {
	router := NewRouter()
	app := New(router, NewEventLoopWorkerPool(runtime.NumCPU()))
	router.Use(func(ctx *Context) error { return ctx.Next() })
	router.Handle(GET, "/only-get", func(c *Context) error { return c.WriteString("x") })

	port := startAuditServer(t, app)
	c := dial(t, port)
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = c.Write([]byte("OPTIONS /only-get HTTP/1.1\r\nHost: x\r\n\r\n"))
	head, _ := readResponse(t, bufio.NewReader(c))
	if !strings.HasPrefix(head, "HTTP/1.1 404") {
		t.Fatalf("with nothing answering OPTIONS the behaviour must stay a 404, got %q", strings.SplitN(head, "\r\n", 2)[0])
	}
}

var _ = net.IPv4len
