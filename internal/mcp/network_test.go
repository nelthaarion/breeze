package mcp

// network_test.go — the Streamable HTTP transport, checked against the parts of
// the specification that are checkable.
//
// These tests go through http.Handler rather than a bound port wherever they can,
// because the transport's job is entirely in the request/response headers and a
// real socket adds nothing to that. The two tests that do bind — the loopback
// default and the full handshake — bind because they are asserting something
// about the listener itself.
//
// The claim that matters most is the one in TestNetworkServesTheSameToolsAsStdio:
// there is no second tool table, no second dispatcher and no second schema. If
// that test can be made to fail by editing a tool, the transport has grown a copy
// of something it should have been reusing.

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestNetwork builds a network server over the real tool registry with a known
// token, and returns it with an httptest server in front.
func newTestNetwork(t *testing.T) (*NetworkServer, *httptest.Server, string) {
	t.Helper()

	const token = "test-token-not-a-secret"
	ns, got, err := NewNetworkServer(NewServer("test"), NetworkConfig{
		Mode:  ModeGenerator,
		Token: token,
	})
	if err != nil {
		t.Fatalf("NewNetworkServer: %v", err)
	}
	if got != token {
		t.Fatalf("configured token was replaced: %q", got)
	}

	srv := httptest.NewServer(ns.Handler())
	t.Cleanup(srv.Close)
	return ns, srv, token
}

// post sends one message the way a conforming MCP client would.
func post(t *testing.T, srv *httptest.Server, token, session string, body any) *http.Response {
	t.Helper()

	var payload []byte
	switch v := body.(type) {
	case string:
		payload = []byte(v)
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("encoding request: %v", err)
		}
		payload = encoded
	}

	req, err := http.NewRequest(
		http.MethodPost,
		srv.URL+DefaultEndpointPath,
		strings.NewReader(string(payload)),
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set(protocolHeader, protocolVersion)
	if token != "" {
		req.Header.Set("Authorization", bearerPrefix+token)
	}
	if session != "" {
		req.Header.Set(sessionHeader, session)
	}

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", DefaultEndpointPath, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// initializeRequest is the handshake message, spelled once.
func initializeRequest(id int) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "test", "version": "1"},
		},
	}
}

// handshake performs initialize and returns the session id the server minted.
func handshake(t *testing.T, srv *httptest.Server, token string) string {
	t.Helper()

	resp := post(t, srv, token, "", initializeRequest(1))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize status = %d, want 200", resp.StatusCode)
	}
	session := resp.Header.Get(sessionHeader)
	if session == "" {
		t.Fatalf(
			"initialize returned no %s header, so no subsequent request can be made",
			sessionHeader,
		)
	}
	return session
}

func decodeBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()

	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("response body is not JSON: %v", err)
	}
	return out
}

// readAll returns a response body verbatim, for the tests that compare bytes
// between transports rather than decoded values.
func readAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	return body
}

// ─── authentication ──────────────────────────────────────────────────────────

// TestNetworkRejectsUnauthenticated is the acceptance criterion "network mode
// requires auth by default", asserted at the only place it can be: the wire.
//
// Every case is a request that would have succeeded with the right token, so a
// failure here means the guard was skipped rather than that the message was
// wrong. initialize is included deliberately — a server that authenticated
// everything except the handshake would leak its version and capabilities to an
// anonymous caller, and would let one open sessions.
func TestNetworkRejectsUnauthenticated(t *testing.T) {
	_, srv, token := newTestNetwork(t)

	cases := []struct {
		name  string
		token string
	}{
		{"no-token", ""},
		{"wrong-token", "definitely-not-the-token"},
		{"right-length-wrong-value", strings.Repeat("a", len(token))},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := post(t, srv, tc.token, "", initializeRequest(1))
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
			if got := resp.Header.Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
				t.Errorf("WWW-Authenticate = %q, want it to name the Bearer scheme", got)
			}
			if resp.Header.Get(sessionHeader) != "" {
				t.Error("a rejected request was given a session id")
			}

			body := decodeBody(t, resp)
			errObj, ok := body["error"].(map[string]any)
			if !ok {
				t.Fatalf("rejection body has no JSON-RPC error member: %v", body)
			}
			// Fail closed *and* say why: the same discipline mcp_route.go applies
			// to a refused Auto-MCP call. A 401 with an empty body would leave a
			// client unable to tell a missing token from a wrong URL.
			if message, _ := errObj["message"].(string); !strings.Contains(
				message,
				"bearer token",
			) {
				t.Errorf("error message does not say what is missing: %q", message)
			}
			// And it must not say what the right answer is.
			if strings.Contains(fmt.Sprint(body), token) {
				t.Error("the rejection echoed the expected token")
			}
		})
	}
}

// TestNetworkAcceptsAuthenticated is the other half: a correct token gets in, and
// gets a real answer rather than an empty success.
func TestNetworkAcceptsAuthenticated(t *testing.T) {
	_, srv, token := newTestNetwork(t)

	session := handshake(t, srv, token)

	resp := post(t, srv, token, session, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/list",
		"params":  map[string]any{},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tools/list status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	var out struct {
		Result toolsListResult `json:"result"`
		Error  *wireError      `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Error != nil {
		t.Fatalf("tools/list: %+v", out.Error)
	}
	if len(out.Result.Tools) == 0 {
		t.Fatal("tools/list came back empty, so the registry did not survive the transport")
	}
}

// TestGeneratedTokenIsUsableAndRandom covers the startup-generated case: a token
// nobody supplied still works, and two servers do not get the same one.
func TestGeneratedTokenIsUsableAndRandom(t *testing.T) {
	first, _, err := NewNetworkServer(NewServer("test"), NetworkConfig{Mode: ModeGenerator})
	if err != nil {
		t.Fatal(err)
	}
	second, secondToken, err := NewNetworkServer(
		NewServer("test"),
		NetworkConfig{Mode: ModeGenerator},
	)
	if err != nil {
		t.Fatal(err)
	}
	if first.token == "" || secondToken == "" {
		t.Fatal("a server was created with no token, so it would be unauthenticated")
	}
	if first.token == secondToken {
		t.Fatal("two servers generated the same token")
	}

	srv := httptest.NewServer(second.Handler())
	defer srv.Close()

	if resp := post(
		t,
		srv,
		secondToken,
		"",
		initializeRequest(1),
	); resp.StatusCode != http.StatusOK {
		t.Fatalf("the reported token was rejected by its own server: status %d", resp.StatusCode)
	}
	if resp := post(
		t,
		srv,
		first.token,
		"",
		initializeRequest(1),
	); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("another server's token was accepted: status %d", resp.StatusCode)
	}
}

// ─── the same tools, not a second copy of them ───────────────────────────────

// TestNetworkServesTheSameToolsAsStdio is the reuse claim, made checkable.
//
// Both transports are driven with the identical tools/list request and the two
// listings are compared field by field — names, descriptions and the raw
// inputSchema bytes. Comparing the schemas as bytes rather than as decoded values
// is the point: it fails if either side re-marshals, reorders or re-describes
// anything, which is exactly what a second implementation would do.
func TestNetworkServesTheSameToolsAsStdio(t *testing.T) {
	_, srv, token := newTestNetwork(t)
	session := handshake(t, srv, token)

	request := map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/list",
		"params":  map[string]any{},
	}

	// The stdio side, through the same seam rpc.NewStdioServer dispatches to.
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var viaStdio struct {
		Result toolsListResult `json:"result"`
	}
	if err := json.Unmarshal(NewServer("test").RPCServer().Handle(encoded), &viaStdio); err != nil {
		t.Fatal(err)
	}

	var viaNetwork struct {
		Result toolsListResult `json:"result"`
	}
	resp := post(t, srv, token, session, request)
	if err := json.NewDecoder(resp.Body).Decode(&viaNetwork); err != nil {
		t.Fatal(err)
	}

	if len(viaStdio.Result.Tools) == 0 {
		t.Fatal("the stdio listing is empty, so this comparison would prove nothing")
	}
	if len(viaNetwork.Result.Tools) != len(viaStdio.Result.Tools) {
		t.Fatalf("network listed %d tools, stdio listed %d",
			len(viaNetwork.Result.Tools), len(viaStdio.Result.Tools))
	}

	for i, want := range viaStdio.Result.Tools {
		got := viaNetwork.Result.Tools[i]
		if got.Name != want.Name {
			t.Errorf("tool %d: name = %q over the network, %q over stdio", i, got.Name, want.Name)
			continue
		}
		if got.Description != want.Description {
			t.Errorf("%s: description differs between transports", want.Name)
		}
		if string(got.InputSchema) != string(want.InputSchema) {
			t.Errorf("%s: inputSchema differs between transports\nnetwork: %s\nstdio:   %s",
				want.Name, got.InputSchema, want.InputSchema)
		}
	}
}

// TestNetworkToolCallBehavesIdentically runs a tool that answers from the
// registry alone, over both transports, and compares the result.
//
// breeze_features is chosen because it touches no filesystem and no clock, so a
// byte difference between the two answers can only come from the transport.
func TestNetworkToolCallBehavesIdentically(t *testing.T) {
	_, srv, token := newTestNetwork(t)
	session := handshake(t, srv, token)

	request := map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "breeze_features",
			"arguments": map[string]any{"feature": "dashboard"},
		},
	}

	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	stdio := NewServer("test").RPCServer().Handle(encoded)

	resp := post(t, srv, token, session, request)
	network := readAll(t, resp)

	if string(network) != string(stdio) {
		t.Errorf(
			"the same tool call produced different responses\nnetwork: %s\nstdio:   %s",
			network,
			stdio,
		)
	}
}

// ─── transport conformance ───────────────────────────────────────────────────

// TestNetworkMethodHandling covers the methods the specification names.
//
// POST is the transport. GET and DELETE are answered 405 with an Allow header,
// which the handshake-era revisions permit for a server offering neither the
// standalone SSE stream nor client session termination, and which the 2026-07-28
// revision prescribes for a server that no longer implements them. A 404 or a
// silent 200 here would both make a conforming client hang waiting for a stream.
func TestNetworkMethodHandling(t *testing.T) {
	_, srv, token := newTestNetwork(t)

	for _, method := range []string{http.MethodGet, http.MethodDelete, http.MethodPut} {
		t.Run(method, func(t *testing.T) {
			req, err := http.NewRequest(method, srv.URL+DefaultEndpointPath, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", bearerPrefix+token)

			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want 405", resp.StatusCode)
			}
			if got := resp.Header.Get("Allow"); got != http.MethodPost {
				t.Errorf("Allow = %q, want POST", got)
			}
		})
	}
}

// TestNetworkHeaderValidation covers Accept, Content-Type and
// MCP-Protocol-Version. The statuses are the ones the specification names, and
// the 400 for an unsupported protocol version is a MUST.
func TestNetworkHeaderValidation(t *testing.T) {
	_, srv, token := newTestNetwork(t)

	cases := []struct {
		name     string
		headers  map[string]string
		want     int
		contains string
	}{
		{
			name:    "accept-omitted-is-fine",
			headers: map[string]string{"Accept": ""},
			want:    http.StatusOK,
		},
		{
			name:    "accept-json-only-is-fine",
			headers: map[string]string{"Accept": "application/json"},
			want:    http.StatusOK,
		},
		{
			name:     "accept-excludes-json",
			headers:  map[string]string{"Accept": "text/html"},
			want:     http.StatusNotAcceptable,
			contains: "application/json",
		},
		{
			name:     "content-type-missing",
			headers:  map[string]string{"Content-Type": ""},
			want:     http.StatusUnsupportedMediaType,
			contains: "application/json",
		},
		{
			name:     "content-type-wrong",
			headers:  map[string]string{"Content-Type": "text/plain"},
			want:     http.StatusUnsupportedMediaType,
			contains: "text/plain",
		},
		{
			name:    "content-type-with-charset",
			headers: map[string]string{"Content-Type": "application/json; charset=utf-8"},
			want:    http.StatusOK,
		},
		{
			// Absent means "assume 2025-03-26" per the specification, not an error.
			name:    "protocol-version-omitted",
			headers: map[string]string{protocolHeader: ""},
			want:    http.StatusOK,
		},
		{
			name:    "protocol-version-2025-06-18",
			headers: map[string]string{protocolHeader: "2025-06-18"},
			want:    http.StatusOK,
		},
		{
			name:     "protocol-version-unsupported",
			headers:  map[string]string{protocolHeader: "1900-01-01"},
			want:     http.StatusBadRequest,
			contains: "not supported",
		},
		{
			// The stateless revision: refused rather than half-served, because this
			// server implements initialize and a session, which that revision removed.
			name:     "protocol-version-2026-07-28",
			headers:  map[string]string{protocolHeader: "2026-07-28"},
			want:     http.StatusBadRequest,
			contains: "handshake-based",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(initializeRequest(1))
			if err != nil {
				t.Fatal(err)
			}
			req, err := http.NewRequest(
				http.MethodPost,
				srv.URL+DefaultEndpointPath,
				strings.NewReader(string(body)),
			)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set(protocolHeader, protocolVersion)
			req.Header.Set("Authorization", bearerPrefix+token)
			for key, value := range tc.headers {
				if value == "" {
					req.Header.Del(key)
					continue
				}
				req.Header.Set(key, value)
			}

			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
			if tc.contains != "" {
				if got := fmt.Sprint(decodeBody(t, resp)); !strings.Contains(got, tc.contains) {
					t.Errorf("rejection does not mention %q: %s", tc.contains, got)
				}
			}
		})
	}
}

// TestNetworkUnknownToolIsStillAProtocolError checks that the transport does not
// flatten the distinction the vocabulary draws: an unknown tool name is a
// JSON-RPC -32602 inside a 200, not an HTTP error.
func TestNetworkUnknownToolIsStillAProtocolError(t *testing.T) {
	_, srv, token := newTestNetwork(t)
	session := handshake(t, srv, token)

	resp := post(t, srv, token, session, map[string]any{
		"jsonrpc": "2.0",
		"id":      4,
		"method":  "tools/call",
		"params":  map[string]any{"name": "breeze_guess"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf(
			"status = %d, want 200: a dispatched-and-refused call is not a transport failure",
			resp.StatusCode,
		)
	}

	var out struct {
		Error *wireError `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Error == nil || out.Error.Code != -32602 {
		t.Fatalf("error = %+v, want -32602", out.Error)
	}
}

// ─── sessions ────────────────────────────────────────────────────────────────

// TestNetworkSessionManagement covers §Session Management: an id is minted on
// initialize, required afterwards, and an unknown one is 404 rather than 401 or
// 400 — because 404 is the code that tells a client to re-initialize, and any
// other answer leaves it retrying a session that will never work.
func TestNetworkSessionManagement(t *testing.T) {
	_, srv, token := newTestNetwork(t)

	session := handshake(t, srv, token)
	toolsList := map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/list",
		"params":  map[string]any{},
	}

	t.Run("missing-session-is-400", func(t *testing.T) {
		resp := post(t, srv, token, "", toolsList)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
		if got := fmt.Sprint(decodeBody(t, resp)); !strings.Contains(got, sessionHeader) {
			t.Errorf("the rejection does not name the missing header: %s", got)
		}
	})

	t.Run("unknown-session-is-404", func(t *testing.T) {
		resp := post(t, srv, token, "0123456789abcdef0123456789abcdef", toolsList)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
		if got := fmt.Sprint(decodeBody(t, resp)); !strings.Contains(got, "initialize") {
			t.Errorf("the rejection does not say how to recover: %s", got)
		}
	})

	t.Run("minted-session-is-accepted-repeatedly", func(t *testing.T) {
		// Twice, because a session that worked once and then vanished would be
		// worse than one that never worked: the failure would look intermittent.
		for i := 0; i < 2; i++ {
			if resp := post(t, srv, token, session, toolsList); resp.StatusCode != http.StatusOK {
				t.Fatalf("call %d: status = %d, want 200", i+1, resp.StatusCode)
			}
		}
	})

	t.Run("each-initialize-mints-a-distinct-session", func(t *testing.T) {
		other := handshake(t, srv, token)
		if other == session {
			t.Fatal("two handshakes produced the same session id")
		}
		if len(other) < 16 {
			t.Errorf("session id %q is too short to be unguessable", other)
		}
		// Visible ASCII only, per the specification. Hex satisfies that, and this
		// asserts the generator has not been swapped for one that does not.
		for _, r := range other {
			if r < 0x21 || r > 0x7e {
				t.Fatalf("session id %q contains a character outside visible ASCII", other)
			}
		}
	})
}

// TestNetworkNotificationGets202 is the specification's rule for a POST whose
// body is a notification: 202 Accepted with no body.
//
// It is also the one place the two layers agree without talking: rpc reports
// "nothing to say" by returning no bytes, and this transport turns that into 202.
// A notification answered with 200 and an empty body would leave a client parsing
// "" as JSON.
func TestNetworkNotificationGets202(t *testing.T) {
	_, srv, token := newTestNetwork(t)
	session := handshake(t, srv, token)

	// Some clients send notifications/initialized without the session header,
	// immediately after initialize; both spellings must work.
	for _, withSession := range []string{session, ""} {
		resp := post(t, srv, token, withSession, map[string]any{
			"jsonrpc": "2.0",
			"method":  "notifications/initialized",
		})
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", resp.StatusCode)
		}
		if body := readAll(t, resp); len(body) != 0 {
			t.Errorf("202 carried a body: %q", body)
		}
	}
}

// ─── Origin ──────────────────────────────────────────────────────────────────

// postWithOrigin sends an initialize carrying an Origin header, and optionally a
// token. It is separate from post because Origin is the one header a legitimate
// client usually does not send at all.
func postWithOrigin(t *testing.T, srv *httptest.Server, token, origin string) *http.Response {
	t.Helper()

	body, err := json.Marshal(initializeRequest(1))
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(
		http.MethodPost,
		srv.URL+DefaultEndpointPath,
		strings.NewReader(string(body)),
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if token != "" {
		req.Header.Set("Authorization", bearerPrefix+token)
	}

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// TestNetworkOriginValidation is the specification's first security requirement,
// which is a MUST: an invalid Origin is 403.
//
// The absent case matters as much as the rejected one. A non-browser MCP client
// sends no Origin at all, and refusing those would reject every real client while
// protecting against nothing — the header exists only to stop a page in a browser
// from reaching a loopback server.
func TestNetworkOriginValidation(t *testing.T) {
	_, srv, token := newTestNetwork(t)

	cases := []struct {
		name   string
		origin string
		want   int
	}{
		{"absent", "", http.StatusOK},
		{"loopback-ip", "http://127.0.0.1", http.StatusOK},
		{"loopback-name", "http://localhost", http.StatusOK},
		{"hostile-page", "https://evil.example.com", http.StatusForbidden},
		// A suffix match would let this through, which is why the check is exact.
		{"lookalike", "http://localhost.evil.example.com", http.StatusForbidden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := postWithOrigin(t, srv, token, tc.origin)
			if resp.StatusCode != tc.want {
				t.Fatalf("Origin %q: status = %d, want %d", tc.origin, resp.StatusCode, tc.want)
			}
		})
	}
}

// TestNetworkOriginCheckPrecedesAuth records an ordering decision that is easy to
// reverse by accident.
//
// A hostile page's request is refused for its Origin whether or not it also
// carries a valid token. Checking auth first would let a page distinguish "wrong
// token" from "wrong origin" and use that to confirm a stolen token against a
// server it is otherwise not allowed to reach.
func TestNetworkOriginCheckPrecedesAuth(t *testing.T) {
	_, srv, token := newTestNetwork(t)

	for _, tc := range []struct{ name, token string }{
		{"with-valid-token", token},
		{"with-no-token", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := postWithOrigin(t, srv, tc.token, "https://evil.example.com")
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 regardless of the token", resp.StatusCode)
			}
		})
	}
}

// TestNetworkAllowedOriginsExtendTheList covers --allow-origin, including the
// explicit "*" escape hatch. The escape hatch is tested because it exists for
// operators behind a reverse proxy, and an option that silently does nothing is
// worse than no option.
func TestNetworkAllowedOriginsExtendTheList(t *testing.T) {
	named, _, err := NewNetworkServer(NewServer("test"), NetworkConfig{
		Mode:           ModeGenerator,
		Token:          "t",
		AllowedOrigins: []string{"https://console.example.com"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !named.originAllowed("https://console.example.com") {
		t.Error("an explicitly allowed origin was refused")
	}
	if named.originAllowed("https://other.example.com") {
		t.Error("allowing one origin allowed another")
	}

	wildcard, _, err := NewNetworkServer(NewServer("test"), NetworkConfig{
		Mode:           ModeGenerator,
		Token:          "t",
		AllowedOrigins: []string{"*"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !wildcard.originAllowed("https://anything.example.com") {
		t.Error(`--allow-origin "*" did not disable the check`)
	}
}

// ─── binding ─────────────────────────────────────────────────────────────────

// TestDefaultBindIsLoopbackOnly is the acceptance criterion "loopback-only bind
// by default", asserted against the socket rather than the config struct.
//
// The assertion is on the bound address, because that is what an operator can be
// wrong about: a default of "" would have bound every interface and looked
// identical in every unit test that only inspected NetworkConfig.
func TestDefaultBindIsLoopbackOnly(t *testing.T) {
	ns, _, err := NewNetworkServer(NewServer("test"), NetworkConfig{Mode: ModeGenerator})
	if err != nil {
		t.Fatal(err)
	}
	// Port 0 rather than a fixed one: this test must not fail because something
	// else on the machine happens to hold a port.
	if err := ns.Listen(NetworkConfig{}); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ns.Close()

	addr, ok := ns.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("bound address is %T, not TCP", ns.Addr())
	}
	if !addr.IP.IsLoopback() {
		t.Fatalf("the default bind is %s, which is not loopback: an unauthenticated-by-accident "+
			"code generator would be reachable from the network", addr.IP)
	}
}

// TestExplicitHostOverridesTheDefault is the other half: --host is what widens
// the bind, and nothing else does.
//
// It binds 0.0.0.0 because that is the case the default exists to prevent, and
// asserting it works is what makes the default a choice rather than a limitation.
func TestExplicitHostOverridesTheDefault(t *testing.T) {
	ns, _, err := NewNetworkServer(NewServer("test"), NetworkConfig{
		Mode: ModeGenerator,
		Host: "0.0.0.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ns.Listen(NetworkConfig{Host: "0.0.0.0"}); err != nil {
		t.Skipf("cannot bind 0.0.0.0 in this environment: %v", err)
	}
	defer ns.Close()

	addr, ok := ns.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("bound address is %T, not TCP", ns.Addr())
	}
	if !addr.IP.IsUnspecified() {
		t.Errorf("--host 0.0.0.0 bound %s instead of every interface", addr.IP)
	}
}

// TestServeOverARealSocket is the end-to-end check: a bound listener, a real HTTP
// client, the full handshake, and a tool call that comes back with real data.
//
// Everything above this drives Handler directly, which is the right level for
// header behaviour but proves nothing about Listen and Serve actually being
// wired to each other.
func TestServeOverARealSocket(t *testing.T) {
	cfg := NetworkConfig{Mode: ModeGenerator, Token: "socket-test-token"}

	ns, token, err := NewNetworkServer(NewServer("test"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := ns.Listen(cfg); err != nil {
		t.Fatal(err)
	}

	served := make(chan error, 1)
	go func() { served <- ns.Serve() }()
	t.Cleanup(func() {
		if err := ns.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
		if err := <-served; err != nil {
			t.Errorf("Serve returned %v; a closed listener is a clean stop", err)
		}
	})

	endpoint := fmt.Sprintf("http://%s%s", ns.Addr(), ns.Endpoint())

	// The handshake, by hand, so this test does not depend on the helpers above
	// and therefore covers the socket path independently.
	session := ""
	for _, step := range []struct {
		name string
		body map[string]any
		want int
	}{
		{"initialize", initializeRequest(1), http.StatusOK},
		{"tools/list", map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": map[string]any{}}, http.StatusOK},
	} {
		payload, err := json.Marshal(step.body)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(string(payload)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", bearerPrefix+token)
		if session != "" {
			req.Header.Set(sessionHeader, session)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		body := readAll(t, resp)
		resp.Body.Close()

		if resp.StatusCode != step.want {
			t.Fatalf("%s: status = %d, want %d: %s", step.name, resp.StatusCode, step.want, body)
		}
		if id := resp.Header.Get(sessionHeader); id != "" {
			session = id
		}
		if step.name == "tools/list" {
			var out struct {
				Result toolsListResult `json:"result"`
			}
			if err := json.Unmarshal(body, &out); err != nil {
				t.Fatalf("tools/list body: %v", err)
			}
			if len(out.Result.Tools) == 0 {
				t.Fatal("tools/list over a real socket came back empty")
			}
		}
	}
	if session == "" {
		t.Fatal("no session id was ever returned over the socket path")
	}
}

// TestWrongPathIsAJSONRPCError checks the one thing a misconfigured client is
// most likely to hit: the right host and port, the wrong path. net/http's default
// 404 is HTML, which an MCP client cannot read; this answers in JSON-RPC and names
// the endpoint.
func TestWrongPathIsAJSONRPCError(t *testing.T) {
	_, srv, token := newTestNetwork(t)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/rpc", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", bearerPrefix+token)

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if got := fmt.Sprint(decodeBody(t, resp)); !strings.Contains(got, DefaultEndpointPath) {
		t.Errorf("the 404 does not name the real endpoint: %s", got)
	}
}
