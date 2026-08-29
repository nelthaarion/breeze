package mcp

// These tests exercise MCP through rpc.Server.Handle, not by calling handlers
// directly. That boundary matters: a handler can look correct while returning a
// shape the JSON-RPC encoder cannot represent, or while accidentally answering a
// notification. Handle is the seam the stdio transport uses in production.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

type wireResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *wireError      `json:"error"`
}

type wireError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func call(t *testing.T, srv *Server, id int, method string, params any) wireResponse {
	t.Helper()

	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
	}
	if params != nil {
		req["params"] = params
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	out := srv.RPCServer().Handle(b)
	if len(out) == 0 {
		t.Fatalf("%s returned no response", method)
	}

	var resp wireResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("%s returned invalid JSON: %q (%v)", method, out, err)
	}
	return resp
}

func callTool(t *testing.T, srv *Server, name string, args any) toolCallResult {
	t.Helper()

	resp := call(t, srv, 1, "tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	if resp.Error != nil {
		t.Fatalf("tools/call(%s): JSON-RPC error %d: %s", name, resp.Error.Code, resp.Error.Message)
	}

	var result toolCallResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("decode tool result: %v", err)
	}
	if len(result.Content) == 0 {
		t.Fatalf("tools/call(%s) returned no content", name)
	}
	return result
}

func TestInitialize(t *testing.T) {
	resp := call(t, NewServer("v-test"), 7, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test", "version": "1"},
	})
	if resp.Error != nil {
		t.Fatalf("initialize: %+v", resp.Error)
	}

	var got initializeResult
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatalf("decode initialize result: %v", err)
	}
	if got.ProtocolVersion != protocolVersion {
		t.Errorf("protocolVersion = %q, want %q", got.ProtocolVersion, protocolVersion)
	}
	if got.ServerInfo.Name != serverName || got.ServerInfo.Version != "v-test" {
		t.Errorf("serverInfo = %+v", got.ServerInfo)
	}

	// The tools key must be present. An omitted key means the server declared no
	// tool capability at all.
	var shape map[string]any
	if err := json.Unmarshal(resp.Result, &shape); err != nil {
		t.Fatal(err)
	}
	caps, ok := shape["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("capabilities is not an object: %v", shape["capabilities"])
	}
	if _, ok := caps["tools"]; !ok {
		t.Error("initialize omitted capabilities.tools")
	}
}

func TestInitializedNotificationWritesNothing(t *testing.T) {
	req := []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if out := NewServer("test").RPCServer().Handle(req); len(out) != 0 {
		t.Fatalf("initialized notification produced output: %q", out)
	}
}

func TestToolsListIsStableAndSchemasAreObjects(t *testing.T) {
	srv := NewServer("test")
	resp := call(t, srv, 1, "tools/list", map[string]any{})
	if resp.Error != nil {
		t.Fatalf("tools/list: %+v", resp.Error)
	}

	var got toolsListResult
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}

	want := []string{"breeze_add", "breeze_features", "breeze_generate", "breeze_new", "breeze_routes"}
	if len(got.Tools) != len(want) {
		t.Fatalf("got %d tools, want %d: %+v", len(got.Tools), len(want), got.Tools)
	}
	for i, tool := range got.Tools {
		if tool.Name != want[i] {
			t.Errorf("tool %d = %q, want %q (listing is not stable)", i, tool.Name, want[i])
		}
		if tool.Description == "" {
			t.Errorf("%s has no description", tool.Name)
		}
		var schema map[string]any
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Errorf("%s schema is invalid JSON: %v", tool.Name, err)
			continue
		}
		if schema["type"] != "object" {
			t.Errorf("%s schema type = %v, want object", tool.Name, schema["type"])
		}
	}
}

func TestToolsCallProtocolErrors(t *testing.T) {
	srv := NewServer("test")

	tests := []struct {
		name   string
		params any
		text   string
	}{
		{"params-not-object", []string{"breeze_features"}, "params must be an object"},
		{"missing-name", map[string]any{}, "requires a tool name"},
		{"unknown-name", map[string]any{"name": "breeze_guess"}, "unknown tool"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := call(t, srv, 1, "tools/call", tt.params)
			if resp.Error == nil {
				t.Fatalf("got result %s, want JSON-RPC error", resp.Result)
			}
			if resp.Error.Code != -32602 {
				t.Errorf("code = %d, want -32602", resp.Error.Code)
			}
			if !strings.Contains(resp.Error.Message, tt.text) {
				t.Errorf("message = %q, want it to contain %q", resp.Error.Message, tt.text)
			}
		})
	}
}

func TestFeatureIntrospection(t *testing.T) {
	srv := NewServer("test")

	all := callTool(t, srv, "breeze_features", map[string]any{})
	if all.IsError {
		t.Fatalf("feature list failed: %s", all.Content[0].Text)
	}
	if !strings.Contains(all.Content[0].Text, "features available") ||
		!strings.Contains(all.Content[0].Text, "jsonrpc") {
		t.Errorf("feature list is missing expected facts:\n%s", all.Content[0].Text)
	}

	one := callTool(t, srv, "breeze_features", map[string]any{"feature": "dashboard"})
	if one.IsError {
		t.Fatalf("dashboard description failed: %s", one.Content[0].Text)
	}
	if !strings.Contains(one.Content[0].Text, "--allow-writes") {
		t.Errorf("dashboard flags omitted allow-writes:\n%s", one.Content[0].Text)
	}

	bad := callTool(t, srv, "breeze_features", map[string]any{"feature": "definitely-not-real"})
	if !bad.IsError || !strings.Contains(bad.Content[0].Text, "unknown feature") {
		t.Errorf("unknown feature result = %+v", bad)
	}
}

func TestGeneratorArgumentFailureIsToolErrorNotProtocolError(t *testing.T) {
	resp := callTool(t, NewServer("test"), "breeze_generate", map[string]any{})
	if !resp.IsError {
		t.Fatalf("missing kind was reported as success: %+v", resp)
	}
	if !strings.Contains(resp.Content[0].Text, "kind is required") {
		t.Errorf("message = %q", resp.Content[0].Text)
	}
}

func TestBreezeNewCreatesProjectAndCapturesGeneratorStdout(t *testing.T) {
	parent := t.TempDir()
	srv := NewServer("test")

	result := callTool(t, srv, "breeze_new", map[string]any{
		"name":   "mcpfixture",
		"module": "example.com/mcpfixture",
		"dir":    parent,
	})
	if result.IsError {
		t.Fatalf("breeze_new failed: %s", result.Content[0].Text)
	}
	if result.Content[0].Text == "" {
		t.Fatal("generator stdout was discarded instead of becoming tool content")
	}

	for _, name := range []string{"go.mod", "main.go"} {
		if _, err := os.Stat(filepath.Join(parent, "mcpfixture", name)); err != nil {
			t.Errorf("generated %s: %v", name, err)
		}
	}

	mod, err := os.ReadFile(filepath.Join(parent, "mcpfixture", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mod), "module example.com/mcpfixture") {
		t.Errorf("go.mod does not contain requested module:\n%s", mod)
	}
}

func TestCaptureStdoutSerialisesConcurrentCallsAndRestoresStdout(t *testing.T) {
	original := os.Stdout
	const n = 8

	outputs := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out, err := captureStdout(func() error {
				_, _ = os.Stdout.WriteString(strings.Repeat(string(rune('a'+i)), 4096))
				return nil
			})
			if err != nil {
				t.Errorf("capture %d: %v", i, err)
				return
			}
			outputs[i] = out
		}(i)
	}
	wg.Wait()

	if os.Stdout != original {
		t.Fatal("captureStdout did not restore os.Stdout")
	}

	// Each output must contain exactly one writer's byte. Interleaving means the
	// process-global replacement was not actually serialised.
	for i, out := range outputs {
		seen := make(map[rune]bool)
		for _, r := range out {
			seen[r] = true
		}
		if len(seen) != 1 || !seen[rune('a'+i)] {
			keys := make([]string, 0, len(seen))
			for r := range seen {
				keys = append(keys, string(r))
			}
			sort.Strings(keys)
			t.Errorf("capture %d contains writers %v", i, keys)
		}
	}
}

func TestCaptureStdoutRestoresStdoutAfterPanic(t *testing.T) {
	original := os.Stdout
	const marker = "capture panic marker"

	func() {
		defer func() {
			if got := recover(); got != marker {
				t.Fatalf("recovered %v, want %q", got, marker)
			}
		}()
		_, _ = captureStdout(func() error {
			panic(marker)
		})
	}()

	if os.Stdout != original {
		t.Fatal("captureStdout did not restore os.Stdout after panic")
	}
}

func TestFlagsToArgv(t *testing.T) {
	got := flagsToArgv(map[string]any{
		"allow-writes": true,
		"disabled":     false,
		"driver":       "postgres",
		"unset":        nil,
	})
	sort.Strings(got)
	want := []string{"--allow-writes", "--driver=postgres"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("flagsToArgv = %v, want %v", got, want)
	}
}

func TestGenerateToolPassesKindSpecificFlags(t *testing.T) {
	dir := t.TempDir()
	// Generation commands intentionally refuse to write outside a Go module.
	// A one-line module is enough for this fixture; the generator supplies the
	// Breeze import when it writes the handler and route registry.
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/mcptest\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatalf("write fixture go.mod: %v", err)
	}
	result := callTool(t, NewServer("test"), "breeze_generate", map[string]any{
		"kind": "handler",
		"name": "Health",
		"flags": map[string]any{
			"path": "/healthz",
		},
		"dir": dir,
	})
	if result.IsError {
		t.Fatalf("breeze_generate failed: %s", result.Content[0].Text)
	}

	registry, err := os.ReadFile(filepath.Join(dir, "routes_generated.go"))
	if err != nil {
		t.Fatalf("read route registry: %v", err)
	}
	text := string(registry)
	if !strings.Contains(text, `"/healthz"`) {
		t.Errorf("kind-specific flags did not reach the generator:\n%s", text)
	}
}
