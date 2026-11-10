package mcp

import (
	"encoding/json"
	"net"
	"strings"
	"testing"

	"github.com/nelthaarion/breeze/v2/diag"
	"github.com/nelthaarion/breeze/v2/rpc"
)

// TestNewInProcessServesTheEmbeddedServer is the one that would have caught it. The tests
// for an embedded server reading its own process's diagnostics were written against a
// constructor the running endpoint never calls, so they passed while a generated
// application answered "a service URL is required". This goes through NewInProcess itself,
// the function StartInProcess calls, and asks it the question.
func TestNewInProcessServesTheEmbeddedServer(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()

	ns, _, err := NewInProcess("test", InProcessConfig{Mode: ModeAppRuntime, Port: port})
	if err != nil {
		t.Fatalf("NewInProcess: %v", err)
	}
	t.Cleanup(func() { _ = ns.Close() })

	out := ns.rpc.Handle([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":` +
		`{"name":"breeze_diagnose_service","arguments":{}}}`))

	var resp struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("decoding %s: %v", out, err)
	}
	if resp.Result.IsError {
		t.Fatalf("the endpoint StartInProcess serves cannot diagnose its own process: %s", out)
	}
	if text := resp.Result.Content[0].Text; !strings.Contains(text, thisProcess) {
		t.Errorf("the answer is not labelled as this process: %.200s", text)
	}
}

// TestStartingTheEndpointDoesNotReplaceTheApplicationsJSONRPCDiagnostic: an application with
// `breeze add jsonrpc` and `breeze add mcp` has two JSON-RPC servers, and the diagnostic registry
// keeps the last one built. The endpoint's is an adapter. It used to take the entry, so the
// application's own server (its methods, its calls, whether its listener ever started) was
// replaced by a report of five MCP methods.
func TestStartingTheEndpointDoesNotReplaceTheApplicationsJSONRPCDiagnostic(t *testing.T) {
	app := rpc.NewRegistry()
	app.Register("orders.create", func(*rpc.Context) {})
	rpc.NewServer(app)

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()

	ns, _, err := NewInProcess("test", InProcessConfig{Mode: ModeAppRuntime, Port: port})
	if err != nil {
		t.Fatalf("NewInProcess: %v", err)
	}
	t.Cleanup(func() { _ = ns.Close() })
	// A request through the endpoint builds nothing new, but it is the path that did.
	_ = ns.rpc.Handle([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))

	report, ok := diag.Get("jsonrpc")
	if !ok {
		t.Fatal("no jsonrpc diagnostic is registered")
	}
	methods, _ := report.Detail["methods"].([]string)
	got := strings.Join(methods, ",")
	if !strings.Contains(got, "orders.create") {
		t.Errorf("the jsonrpc diagnostic no longer describes the application's server: %q", got)
	}
	if strings.Contains(got, "tools/list") {
		t.Errorf("the jsonrpc diagnostic describes the MCP endpoint's methods: %q", got)
	}
}
