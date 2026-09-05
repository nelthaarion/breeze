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
	"time"
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

	// Stability is the property under test, not any particular length: a client
	// caches this listing, so the order must not depend on map iteration.
	names := make([]string, len(got.Tools))
	seen := make(map[string]bool, len(got.Tools))
	for i, tool := range got.Tools {
		names[i] = tool.Name
		if seen[tool.Name] {
			t.Errorf("%s is listed twice", tool.Name)
		}
		seen[tool.Name] = true

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
	if !sort.StringsAreSorted(names) {
		t.Errorf("the listing is not in a stable order: %v", names)
	}

	// The generator tools this server started life as must never disappear.
	for _, want := range []string{"breeze_add", "breeze_features", "breeze_generate", "breeze_new", "breeze_routes"} {
		if !seen[want] {
			t.Errorf("%s is no longer registered", want)
		}
	}

	// A second server must list exactly the same tools. Registration happens in
	// NewServer, so a tool registered from state that outlives one server (a
	// package-level map, say) would show up here as a difference.
	var second toolsListResult
	resp2 := call(t, NewServer("test"), 2, "tools/list", map[string]any{})
	if err := json.Unmarshal(resp2.Result, &second); err != nil {
		t.Fatalf("decode second tools/list: %v", err)
	}
	if len(second.Tools) != len(got.Tools) {
		t.Fatalf("two servers listed %d and %d tools", len(got.Tools), len(second.Tools))
	}
	for i := range second.Tools {
		if second.Tools[i].Name != names[i] {
			t.Errorf("tool %d differs between servers: %q vs %q", i, names[i], second.Tools[i].Name)
		}
	}
}

// baselineToolCount is what this server exposed before the planning, knowledge
// and live-inspection tools were added: breeze_new, breeze_generate, breeze_add,
// breeze_features and breeze_routes. It is recorded as a number rather than
// inferred so that the growth reported below stays meaningful after those five
// are themselves changed.
const baselineToolCount = 5

// TestToolRegistryCovers is the record of what this server exposes. It exists so
// that adding a tool is a deliberate act with a name chosen once, and so the
// count is checkable: the server began with the five generator tools listed
// above and every tool added since is named here.
func TestToolRegistryCovers(t *testing.T) {
	srv := NewServer("test")

	want := []string{
		// generation
		"breeze_add", "breeze_generate", "breeze_new",
		// introspection
		"breeze_features", "breeze_routes",
		// planning and state
		"breeze_plan_project", "breeze_explain_project", "breeze_diff_config",
		"breeze_begin_change_set", "breeze_stage_call", "breeze_commit_change_set",
		"breeze_discard_change_set", "breeze_get_change_history",
		// knowledge
		"breeze_describe_schema", "breeze_list_examples", "breeze_generate_llms_txt",
		"breeze_check_llms_txt_freshness", "breeze_search_llms_txt",
		"breeze_suggest_next_steps", "breeze_explain_idiom",
		"breeze_check_idioms",
		"breeze_get_routes", "breeze_get_performance",
		"breeze_get_recent_errors", "breeze_get_logs",
		"breeze_query_openapi", "breeze_diagnose_service",

		// Category D — live Fleet aggregator.
		"breeze_get_topology", "breeze_get_traces", "breeze_get_trace",
		"breeze_get_contract_violations", "breeze_explain_incident",

		// Category G — verification against the toolchain and a live service.
		"breeze_verify_project", "breeze_run_benchmarks", "breeze_get_test_coverage",
		"breeze_simulate_request",

		// Category H — Docker-aware fleet provisioning. These four are the only
		// tools without the breeze_ prefix, because a tool name is API and these are
		// the names the orchestration design gives them.
		"provision_service", "list_provisioned_services", "deprovision_service",
		"provision_fleet",
	}

	named := map[string]bool{}
	for _, name := range want {
		named[name] = true
		if _, ok := srv.tools[name]; !ok {
			t.Errorf("%s is not registered", name)
		}
	}

	// The reverse direction is the half that keeps this list honest: a tool
	// registered but not named here would otherwise pass unnoticed, and the
	// count below would stop being a record of anything.
	for name := range srv.tools {
		if !named[name] {
			t.Errorf("%s is registered but not named in this test", name)
		}
	}

	if len(srv.tools) != len(srv.order) {
		t.Errorf("%d tools but %d ordered names", len(srv.tools), len(srv.order))
	}
	if len(srv.tools) != len(want) {
		t.Errorf("the server registers %d tools, expected %d", len(srv.tools), len(want))
	}
	t.Logf(
		"tool count: %d (the server began with %d generator tools)",
		len(srv.tools),
		baselineToolCount,
	)
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

// TestCaptureStdoutRefusesToNestInsteadOfDeadlocking pins the behaviour that
// makes this class of mistake survivable.
//
// captureMu is process-wide and not reentrant, so a capture inside a capture
// waits on a lock the same goroutine holds. sync.Mutex has no owner, so the
// runtime cannot report it: the call just never returns, and the whole package
// eventually fails on the test binary's timeout with a stack that points at the
// lock rather than at the nesting. This test would hang forever if the guard were
// removed, so it is run with a watchdog — a failure here is a report, not a hang.
func TestCaptureStdoutRefusesToNestInsteadOfDeadlocking(t *testing.T) {
	original := os.Stdout

	type outcome struct {
		err error
	}
	result := make(chan outcome, 1)

	go func() {
		_, err := captureStdout(func() error {
			_, inner := captureStdout(func() error { return nil })
			return inner
		})
		result <- outcome{err: err}
	}()

	select {
	case got := <-result:
		if got.err == nil {
			t.Fatal("a nested capture was allowed; it must be refused")
		}
		if !strings.Contains(got.err.Error(), "already running") {
			t.Errorf("error = %v, want it to explain the nesting", got.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a nested capture deadlocked instead of being refused")
	}

	// The refusal must not leave the redirection half-applied.
	if os.Stdout != original {
		t.Error("os.Stdout was not restored after a refused nested capture")
	}

	// And the guard must not have poisoned the lock for later callers.
	if _, err := captureStdout(func() error { return nil }); err != nil {
		t.Errorf("a later capture failed after a refusal: %v", err)
	}
}

// TestCaptureStdoutAllowsConcurrentCapturesFromOtherGoroutines is the other side
// of the guard: it must key on goroutine identity, not on "a capture is running".
// Refusing a second goroutine would turn correct serialisation into an error.
func TestCaptureStdoutAllowsConcurrentCapturesFromOtherGoroutines(t *testing.T) {
	release := make(chan struct{})
	holding := make(chan struct{})

	go func() {
		_, _ = captureStdout(func() error {
			close(holding)
			<-release
			return nil
		})
	}()
	<-holding

	second := make(chan error, 1)
	go func() {
		_, err := captureStdout(func() error { return nil })
		second <- err
	}()

	// The second goroutine must be waiting, not refused.
	select {
	case err := <-second:
		close(release)
		t.Fatalf(
			"a capture on another goroutine returned early (err=%v); it should have waited",
			err,
		)
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-second:
		if err != nil {
			t.Errorf("a queued capture failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a queued capture never ran after the holder released")
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
	if err := os.WriteFile(
		filepath.Join(dir, "go.mod"),
		[]byte("module example.com/mcptest\n\ngo 1.24\n"),
		0o644,
	); err != nil {
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
