package mcp

// capability_wire_test.go — Part 8 over the wire: what a client actually sees.
//
// These go through the real dispatcher and the real HTTP handler rather than calling
// Scope methods, because the requirement is about what a client observes: a payload
// field, a filtered listing, a structured refusal, and an endpoint that agrees with all
// three.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// scopedServer builds a network server with a known token and scope, plus an httptest
// server in front of it.
func scopedServer(
	t *testing.T,
	mode ServerMode,
	scope Scope,
) (*NetworkServer, *httptest.Server, string) {
	t.Helper()

	const token = "scope-test-token"
	inner, err := NewServerForMode("test", mode)
	if err != nil {
		t.Fatal(err)
	}
	ns, issued, err := NewNetworkServer(inner, NetworkConfig{
		Mode:  mode,
		Token: token,
		Scope: scope,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(ns.Handler())
	t.Cleanup(srv.Close)
	return ns, srv, issued
}

// initializeFor performs the handshake and returns the parsed result.
func initializeFor(t *testing.T, srv *httptest.Server, token string) initializeResult {
	t.Helper()

	body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":` +
		`{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	req, err := http.NewRequest(http.MethodPost, srv.URL+DefaultEndpointPath, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var reply struct {
		Result initializeResult `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		t.Fatalf("initialize reply: %v", err)
	}
	return reply.Result
}

// TestInitializeReportsGrantedAndKnownCapabilities is Part 8's primary mechanism: an
// agent learns its permissions at handshake time, with no extra call.
func TestInitializeReportsGrantedAndKnownCapabilities(t *testing.T) {
	scope, err := NewScope(CapFleet, CapRuntime)
	if err != nil {
		t.Fatal(err)
	}
	_, srv, token := scopedServer(t, ModeGenerator, scope)

	result := initializeFor(t, srv, token)
	if result.BreezeCapabilities == nil {
		t.Fatal("initialize carried no breezeCapabilities; an agent cannot learn its own scope")
	}

	granted := strings.Join(result.BreezeCapabilities.Granted, ",")
	if granted != "fleet,runtime" {
		t.Errorf("granted = %q, want \"fleet,runtime\"", granted)
	}
	if !result.BreezeCapabilities.Scoped {
		t.Error("scoped = false for a token minted with two capabilities")
	}

	// The full set is reported alongside, so an agent can tell "withheld" from "does
	// not exist" — which decides whether to ask for a wider token or give up.
	if len(result.BreezeCapabilities.Known) != len(KnownCapabilities()) {
		t.Errorf("known = %v, want all %d capabilities",
			result.BreezeCapabilities.Known, len(KnownCapabilities()))
	}
	for _, want := range []string{"generation", "provisioning"} {
		found := false
		for _, k := range result.BreezeCapabilities.Known {
			if k == want {
				found = true
			}
		}
		if !found {
			t.Errorf("known does not list %q, so a client cannot tell it was withheld", want)
		}
	}
}

// scopedListTools performs a handshake and returns the session id and the advertised
// tool names, which is what a real client does before calling anything.
//
// Named apart from server_test.go's in-process helpers because these go over HTTP: the
// transport is the point here, and a shared name would hide that.
func scopedListTools(
	t *testing.T,
	srv *httptest.Server,
	token string,
) (session string, names []string) {
	t.Helper()

	body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":` +
		`{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+DefaultEndpointPath, body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	session = resp.Header.Get(sessionHeader)
	resp.Body.Close()

	listBody := strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	listReq, _ := http.NewRequest(http.MethodPost, srv.URL+DefaultEndpointPath, listBody)
	listReq.Header.Set("Authorization", "Bearer "+token)
	listReq.Header.Set("Content-Type", "application/json")
	listReq.Header.Set("Accept", "application/json, text/event-stream")
	if session != "" {
		listReq.Header.Set(sessionHeader, session)
	}
	listResp, err := srv.Client().Do(listReq)
	if err != nil {
		t.Fatal(err)
	}
	defer listResp.Body.Close()

	var reply struct {
		Result toolsListResult `json:"result"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&reply); err != nil {
		t.Fatalf("tools/list reply: %v", err)
	}
	for _, d := range reply.Result.Tools {
		names = append(names, d.Name)
	}
	return session, names
}

// scopedCallTool sends one tools/call over HTTP and returns either the result or the
// JSON-RPC error, so a test can assert which of the two a refusal came back as.
func scopedCallTool(
	t *testing.T,
	srv *httptest.Server,
	token, session, name string,
) (toolCallResult, json.RawMessage) {
	t.Helper()

	payload := `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"` + name + `","arguments":{}}}`
	req, err := http.NewRequest(
		http.MethodPost,
		srv.URL+DefaultEndpointPath,
		strings.NewReader(payload),
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if session != "" {
		req.Header.Set(sessionHeader, session)
	}

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	var reply struct {
		Result toolCallResult   `json:"result"`
		Error  *json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		t.Fatalf("tools/call reply is not JSON: %v\n%s", err, raw)
	}
	if reply.Error != nil {
		return toolCallResult{}, *reply.Error
	}
	return reply.Result, nil
}

// TestToolsListIsFilteredByScope is the enforcement the initialize payload only
// describes: what an agent can actually see.
func TestToolsListIsFilteredByScope(t *testing.T) {
	scope, err := NewScope(CapFleet, CapRuntime)
	if err != nil {
		t.Fatal(err)
	}
	_, srv, token := scopedServer(t, ModeGenerator, scope)

	_, names := scopedListTools(t, srv, token)
	if len(names) == 0 {
		t.Fatal("a {fleet,runtime} token was advertised no tools at all")
	}

	for _, name := range names {
		capability, ok := capabilityOf(name)
		if !ok {
			t.Errorf("tools/list advertised unclassified tool %q", name)
			continue
		}
		if capability != CapFleet && capability != CapRuntime {
			t.Errorf("tools/list advertised %q (%s), outside the token's scope", name, capability)
		}
	}

	// And everything in scope is present — a filter that dropped the lot would satisfy
	// the loop above without satisfying anybody using the server.
	for _, want := range []string{"breeze_get_trace", "breeze_get_topology", "breeze_get_logs"} {
		found := false
		for _, name := range names {
			if name == want {
				found = true
			}
		}
		if !found {
			t.Errorf("tools/list omitted %q, which is inside the token's scope", want)
		}
	}
}

// TestOutOfScopeToolCallIsRefusedStructurally — the refusal has to be a tool result an
// agent can read, carrying what would be needed to succeed and saying plainly that a
// retry will not help. A -32602 would tell a model "you sent a malformed request",
// which is false and invites it to reformat and try again forever.
func TestOutOfScopeToolCallIsRefusedStructurally(t *testing.T) {
	scope, err := NewScope(CapFleet)
	if err != nil {
		t.Fatal(err)
	}
	_, srv, token := scopedServer(t, ModeGenerator, scope)

	session, _ := scopedListTools(t, srv, token)
	result, rpcErr := scopedCallTool(t, srv, token, session, "provision_service")

	if rpcErr != nil {
		t.Fatalf("the refusal came back as a JSON-RPC error, not a tool result: %s", rpcErr)
	}
	if !result.IsError {
		t.Fatal("an out-of-scope call was not refused")
	}

	detail, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("the refusal carried no structured detail: %#v", result.StructuredContent)
	}
	if detail["requires"] != string(CapProvisioning) {
		t.Errorf("requires = %v, want %q", detail["requires"], CapProvisioning)
	}
	if detail["retry_will_succeed"] != false {
		t.Errorf("retry_will_succeed = %v, want false", detail["retry_will_succeed"])
	}
	// The granted set is echoed so an agent can reason about what it does have instead
	// of probing one tool at a time.
	if _, present := detail["granted"]; !present {
		t.Error("the refusal does not report what the token was granted")
	}
}

// TestInScopeToolCallStillWorks guards the obvious regression: a scope check that
// refuses everything would pass every test above.
func TestInScopeToolCallStillWorks(t *testing.T) {
	scope, err := NewScope(CapKnowledge)
	if err != nil {
		t.Fatal(err)
	}
	_, srv, token := scopedServer(t, ModeGenerator, scope)

	session, _ := scopedListTools(t, srv, token)
	// breeze_list_examples answers from compiled-in constants, so it needs no project
	// on disk and cannot fail for reasons unrelated to scope.
	result, rpcErr := scopedCallTool(t, srv, token, session, "breeze_list_examples")
	if rpcErr != nil {
		t.Fatalf("an in-scope call returned a JSON-RPC error: %s", rpcErr)
	}
	if result.IsError {
		t.Errorf("an in-scope call was refused: %+v", result.Content)
	}
}

// TestUnscopedInitializeReportsEverything — the default must look like full access, and
// must stay distinguishable from a token deliberately minted with every capability.
func TestUnscopedInitializeReportsEverything(t *testing.T) {
	_, srv, token := scopedServer(t, ModeGenerator, UnscopedScope())

	result := initializeFor(t, srv, token)
	if result.BreezeCapabilities == nil {
		t.Fatal("no breezeCapabilities in the handshake")
	}
	if result.BreezeCapabilities.Scoped {
		t.Error("an unscoped token reported scoped = true")
	}
	if len(result.BreezeCapabilities.Granted) != len(KnownCapabilities()) {
		t.Errorf("granted = %v, want every capability", result.BreezeCapabilities.Granted)
	}
}
