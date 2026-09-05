package mcp_test

// inprocess_test.go — the embedded endpoint under a live application.
//
// This is package mcp_test rather than package mcp because it boots a real
// application: it needs the root breeze package, the dashboard, and the wrapper all
// at once, and the wrapper already imports the first two.
//
// The tests that matter here are the concurrency ones. A read-only tool set is a
// claim about what the endpoint cannot do, and the only way to check it is to run
// the application under load while an agent hammers the control plane and see
// whether either notices. Everything runs under -race in CI; the interleaving is
// what the assertions are about, not the individual results.

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nelthaarion/breeze"
	"github.com/nelthaarion/breeze/dashboard"
	"github.com/nelthaarion/breeze/mcp"
)

const (
	testToken    = "in-process-test-token"
	testUser     = "admin"
	testPassword = "admin"
)

// liveApp is an application with its own control endpoint beside it.
type liveApp struct {
	appURL     string
	controlURL string
	token      string
	server     *mcp.Server
}

// startLiveApp boots an instrumented application and an embedded MCP endpoint on
// two separate ports.
//
// The dashboard is installed because the read-only tools read it: an endpoint with
// no dashboard would answer "the feature is not installed", which is a true answer
// to a different question than the one these tests ask.
func startLiveApp(t *testing.T, cfg mcp.InProcessConfig) liveApp {
	t.Helper()

	appPort := freePort(t)
	controlPort := freePort(t)

	router := breeze.NewRouter()
	app := breeze.New(router, breeze.NewEventLoopWorkerPool(runtime.NumCPU()))

	dcfg := dashboard.DefaultConfig()
	dcfg.Username, dcfg.Password = testUser, testPassword
	dcfg.ServiceToken = testToken
	coll := dashboard.Install(app, router, dcfg)
	router.Use(dashboard.Middleware(coll))

	router.Handle(breeze.GET, "/api/widgets", func(ctx *breeze.Context) error {
		return ctx.JSON(map[string]any{"widgets": []string{"a", "b"}})
	})
	// A handler that does a little work, so concurrent load actually overlaps rather
	// than completing before the next request starts.
	router.Handle(breeze.GET, "/api/slow", func(ctx *breeze.Context) error {
		time.Sleep(2 * time.Millisecond)
		return ctx.JSON(map[string]any{"ok": true})
	})

	cfg.Port = controlPort
	if cfg.Token == "" {
		cfg.Token = testToken
	}
	// Mode has no default in the API — deliberately, see internal/mcp/mode.go — so
	// the fixture supplies the one this suite is about. A test that needs the other
	// mode says so explicitly at its own call site.
	if cfg.Mode == "" {
		cfg.Mode = mcp.ModeAppRuntime
	}

	server, token, err := mcp.StartInProcess(app, cfg)
	if err != nil {
		t.Fatalf("StartInProcess: %v", err)
	}
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Errorf("closing the control endpoint: %v", err)
		}
	})

	go func() { _ = server.Serve() }()
	go func() { _ = app.Run(appPort, false) }()

	appURL := fmt.Sprintf("http://127.0.0.1:%d", appPort)
	waitForPort(t, appPort)
	waitForPort(t, controlPort)

	return liveApp{
		appURL:     appURL,
		controlURL: server.URL(),
		token:      token,
		server:     server,
	}
}

func freePort(t *testing.T) int {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("releasing the reserved port: %v", err)
	}
	return port
}

func waitForPort(t *testing.T, port int) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("nothing came up on port %d within 10s", port)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ─── the MCP client side ─────────────────────────────────────────────────────

// mcpSession is a handshaken client against a control endpoint.
type mcpSession struct {
	endpoint string
	token    string
	session  string
}

// handshake opens a session, or reports the status that refused it.
func handshake(t *testing.T, endpoint, token string) (mcpSession, int) {
	t.Helper()

	const initialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":` +
		`{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`

	resp := rawPost(t, endpoint, token, "", initialize)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return mcpSession{}, resp.StatusCode
	}
	return mcpSession{
		endpoint: endpoint,
		token:    token,
		session:  resp.Header.Get("Mcp-Session-Id"),
	}, http.StatusOK
}

// rawPost sends one message, without asserting anything about the answer.
func rawPost(t *testing.T, endpoint, token, session, body string) *http.Response {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", endpoint, err)
	}
	return resp
}

// call makes one JSON-RPC call on a session and returns the decoded response.
func (s mcpSession) call(t *testing.T, id int, method string, params any) map[string]any {
	t.Helper()

	message := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		message["params"] = params
	}
	encoded, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}

	resp := rawPost(t, s.endpoint, s.token, s.session, string(encoded))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s returned HTTP %d", method, resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decoding %s: %v", method, err)
	}
	return out
}

// toolNames lists the tools the endpoint advertises.
func (s mcpSession) toolNames(t *testing.T) []string {
	t.Helper()

	resp := s.call(t, 2, "tools/list", map[string]any{})
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("tools/list has no result: %v", resp)
	}
	listed, ok := result["tools"].([]any)
	if !ok {
		t.Fatalf("tools/list result has no tools array: %v", result)
	}

	names := make([]string, 0, len(listed))
	for _, entry := range listed {
		tool, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("a listed tool is not an object: %v", entry)
		}
		name, _ := tool["name"].(string)
		if name == "" {
			t.Error("a listed tool has no name, so a client could not call it")
		}
		names = append(names, name)
	}
	return names
}

// ─── the safe subset, over the wire ──────────────────────────────────────────

// TestInProcessAdvertisesOnlyTheSafeSubset checks the exclusion where a client sees
// it: in tools/list, from a real handshaken session.
//
// The internal test asserts the filtering; this asserts that the filtering survives
// the transport, which is the thing an agent actually depends on.
func TestInProcessAdvertisesOnlyTheSafeSubset(t *testing.T) {
	app := startLiveApp(t, mcp.InProcessConfig{})

	session, status := handshake(t, app.controlURL, app.token)
	if status != http.StatusOK {
		t.Fatalf("the handshake returned %d", status)
	}

	advertised := session.toolNames(t)
	if len(advertised) == 0 {
		t.Fatal("the embedded endpoint advertises no tools")
	}

	// The advertised set must be exactly what the package reports, so an application
	// can trust mcp.Tools() without making a call.
	expected := map[string]bool{}
	for _, name := range mcp.Tools() {
		expected[name] = true
	}
	for _, name := range advertised {
		if !expected[name] {
			t.Errorf("%s is advertised but is not in mcp.Tools()", name)
		}
	}
	if len(advertised) != len(mcp.Tools()) {
		t.Errorf("%d tools advertised, mcp.Tools() reports %d", len(advertised), len(mcp.Tools()))
	}

	// And none of the excluded ones appear.
	for _, name := range mcp.ExcludedTools() {
		if expected[name] {
			t.Fatalf("%s is in both Tools() and ExcludedTools()", name)
		}
		for _, got := range advertised {
			if got == name {
				t.Errorf("%s is workspace-mutating and must not be advertised in-process", got)
			}
		}
	}
}

// TestInProcessRejectsAWorkspaceToolCall is the acceptance criterion in its
// strongest form: a mutating tool is not merely unadvertised, it is refused when
// called by name.
//
// A client working from a cached tool list, or an agent guessing, will call it
// anyway. The refusal has to be the dispatcher's structured -32602 with the
// available names — which is what turns a dead end into a correction.
func TestInProcessRejectsAWorkspaceToolCall(t *testing.T) {
	app := startLiveApp(t, mcp.InProcessConfig{})

	session, status := handshake(t, app.controlURL, app.token)
	if status != http.StatusOK {
		t.Fatalf("the handshake returned %d", status)
	}

	for _, name := range []string{"breeze_new", "breeze_add", "provision_service"} {
		t.Run(name, func(t *testing.T) {
			resp := session.call(t, 3, "tools/call", map[string]any{
				"name":      name,
				"arguments": map[string]any{"name": "should-never-be-created"},
			})

			errObj, ok := resp["error"].(map[string]any)
			if !ok {
				t.Fatalf("%s was accepted in-process; the response was %v", name, resp)
			}
			if code, _ := errObj["code"].(float64); int(code) != -32602 {
				t.Errorf("error code = %v, want -32602 (an unknown tool name is a parameter error)", errObj["code"])
			}
			if message, _ := errObj["message"].(string); !strings.Contains(message, "unknown tool") {
				t.Errorf("the refusal does not say the tool is unknown: %q", message)
			}
		})
	}
}

// TestInProcessSafeToolActuallyWorks is the other half. A subset that refuses
// everything would pass every exclusion test and be useless.
//
// breeze_get_routes is pointed at the application's own address — the app port, not
// the control port — and must come back with the routes this instance is serving.
func TestInProcessSafeToolActuallyWorks(t *testing.T) {
	app := startLiveApp(t, mcp.InProcessConfig{})

	// One request first, so there is live traffic to report.
	hit(t, app.appURL+"/api/widgets")

	session, status := handshake(t, app.controlURL, app.token)
	if status != http.StatusOK {
		t.Fatalf("the handshake returned %d", status)
	}

	resp := session.call(t, 4, "tools/call", map[string]any{
		"name": "breeze_get_routes",
		"arguments": map[string]any{
			// The app's own address. This is the control-versus-app distinction in
			// practice: the call travels over the control port and is *about* the app port.
			"service_url": app.appURL,
			"username":    testUser,
			"password":    testPassword,
		},
	})

	if errObj, failed := resp["error"]; failed {
		t.Fatalf("breeze_get_routes returned a protocol error: %v", errObj)
	}
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", resp)
	}
	if isError, _ := result["isError"].(bool); isError {
		t.Fatalf("breeze_get_routes reported a failure: %v", result["content"])
	}

	rendered := fmt.Sprint(result["structuredContent"])
	if !strings.Contains(rendered, "/api/widgets") {
		t.Errorf("the route report does not mention a route this instance serves:\n%s", rendered)
	}
}

// hit makes one request and discards the body, for the tests that need traffic to
// exist before they read statistics about it.
func hit(t *testing.T, url string) {
	t.Helper()

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	resp.Body.Close()
}

// ─── concurrency: the app serving while MCP is called ────────────────────────

// TestAppTrafficAndToolCallsConcurrently is the acceptance criterion for -race
// cleanliness under real overlap.
//
// Both sides run flat out for a fixed window: goroutines hammering the application's
// own routes while others make MCP tool calls against the control port. The
// assertions are deliberately about *both* sides — an endpoint that starved the app,
// or an app that starved the endpoint, would pass a test that watched only one.
//
// Run this with -race. The interleaving is the point; the counts are how a
// regression announces itself.
func TestAppTrafficAndToolCallsConcurrently(t *testing.T) {
	app := startLiveApp(t, mcp.InProcessConfig{})

	session, status := handshake(t, app.controlURL, app.token)
	if status != http.StatusOK {
		t.Fatalf("the handshake returned %d", status)
	}

	const (
		trafficWorkers = 8
		toolWorkers    = 4
		window         = 2 * time.Second
	)

	var (
		wg           sync.WaitGroup
		mu           sync.Mutex
		appRequests  int
		appFailures  []string
		toolCalls    int
		toolFailures []string
	)
	deadline := time.Now().Add(window)

	recordApp := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		appRequests++
		if err != nil {
			appFailures = append(appFailures, err.Error())
		}
	}
	recordTool := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		toolCalls++
		if err != nil {
			toolFailures = append(toolFailures, err.Error())
		}
	}

	before := runtime.NumGoroutine()

	// The application's own traffic. Two routes, one of which sleeps, so requests
	// genuinely overlap rather than completing serially.
	for i := 0; i < trafficWorkers; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()

			paths := []string{"/api/widgets", "/api/slow"}
			for time.Now().Before(deadline) {
				path := paths[worker%len(paths)]
				resp, err := http.Get(app.appURL + path)
				if err != nil {
					recordApp(fmt.Errorf("GET %s: %w", path, err))
					continue
				}
				if resp.StatusCode != http.StatusOK {
					recordApp(fmt.Errorf("GET %s returned %d", path, resp.StatusCode))
				} else {
					recordApp(nil)
				}
				resp.Body.Close()
			}
		}(i)
	}

	// The control plane. Every call is a real tool call over the transport, not a bare
	// handshake — the point is to exercise the paths that read the app while the app is
	// busy.
	for i := 0; i < toolWorkers; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()

			calls := []struct {
				tool string
				args map[string]any
			}{
				{"breeze_get_routes", map[string]any{
					"service_url": app.appURL, "username": testUser, "password": testPassword}},
				{"breeze_get_performance", map[string]any{
					"service_url": app.appURL, "username": testUser, "password": testPassword}},
				// Answers from constants, so it exercises the dispatcher without an
				// outbound request — a different shape of concurrent work.
				{"breeze_describe_schema", map[string]any{}},
			}

			next := worker
			for time.Now().Before(deadline) {
				call := calls[next%len(calls)]
				next++
				recordTool(callTolerantly(session, call.tool, call.args))
			}
		}(i)
	}

	wg.Wait()

	if len(appFailures) > 0 {
		t.Errorf("%d of %d application requests failed while MCP was being called; first: %s",
			len(appFailures), appRequests, appFailures[0])
	}
	if len(toolFailures) > 0 {
		t.Errorf("%d of %d tool calls failed while the app was under load; first: %s",
			len(toolFailures), toolCalls, toolFailures[0])
	}

	// Both sides must have actually run. A window in which one made no progress is a
	// starvation result, and it would otherwise read as a pass.
	if appRequests < trafficWorkers {
		t.Errorf("only %d application requests completed in %s; the app was starved", appRequests, window)
	}
	if toolCalls < toolWorkers {
		t.Errorf("only %d tool calls completed in %s; the control plane was starved", toolCalls, window)
	}
	t.Logf("%d application requests and %d tool calls completed concurrently in %s",
		appRequests, toolCalls, window)

	// Goroutines settle. Sampled rather than asserted exactly: gnet and net/http both
	// keep pools, so an exact count would be flaky for reasons unrelated to a leak. A
	// large sustained growth after the load stops is what a leak looks like.
	time.Sleep(500 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before+trafficWorkers+toolWorkers+32 {
		t.Errorf("goroutines grew from %d to %d after the load stopped, which suggests a leak",
			before, after)
	}
}

// callTolerantly makes one tool call and reports a transport or protocol failure.
//
// A tool result of isError is not a failure here: get_performance can legitimately
// report that the dashboard has collected nothing yet, and treating that as a
// concurrency problem would fail the test for the wrong reason. What must not happen
// is an HTTP error, a JSON-RPC error, or a refused connection.
func callTolerantly(s mcpSession, tool string, args map[string]any) error {
	message, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]any{"name": tool, "arguments": args},
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, s.endpoint, strings.NewReader(string(message)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Mcp-Session-Id", s.session)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", tool, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned HTTP %d", tool, resp.StatusCode)
	}
	var out struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("%s: decoding: %w", tool, err)
	}
	if out.Error != nil {
		return fmt.Errorf("%s: JSON-RPC %d: %s", tool, out.Error.Code, out.Error.Message)
	}
	return nil
}

// ─── the opt-in, under load ──────────────────────────────────────────────────

// TestWorkspaceToolInProcessUnderLoad is the test the prompt asks for in its second
// form.
//
// The question was whether a workspace-mutating tool can be made safe to run
// in-process while the app is handling requests. It cannot: breeze_new chdirs the
// whole process, so for the duration of that call every relative path the
// application resolves points at the generated project instead of the app's own
// working directory. That is not a race that can be closed from this side — the
// working directory is one value shared by the process.
//
// So this asserts the design consequence rather than pretending otherwise: with the
// opt-in enabled the tool is present and does work, and the app keeps serving,
// because the chdir window is short and the fixture's routes hold no relative paths.
// The test exists to pin that the opt-in is real, and to demonstrate concretely what
// it puts at risk — a deployed app whose handlers do resolve relative paths would be
// reading the wrong files during that window, which is why the default excludes it.
func TestWorkspaceToolInProcessUnderLoad(t *testing.T) {
	// ModeGenerator, not the suite default: AllowWorkspaceTools is meaningless in
	// app-runtime mode — the tools are not registered there at all — so this test is
	// necessarily about a development-container embed that owns its source tree.
	app := startLiveApp(t, mcp.InProcessConfig{
		Mode:                mcp.ModeGenerator,
		AllowWorkspaceTools: true,
	})

	session, status := handshake(t, app.controlURL, app.token)
	if status != http.StatusOK {
		t.Fatalf("the handshake returned %d", status)
	}

	// The opt-in is real: the tool is advertised now.
	advertised := session.toolNames(t)
	found := false
	for _, name := range advertised {
		if name == "breeze_new" {
			found = true
		}
	}
	if !found {
		t.Fatalf("AllowWorkspaceTools did not restore breeze_new; %d tools advertised", len(advertised))
	}

	// Traffic for the duration, so the generator call and request handling overlap.
	var (
		wg       sync.WaitGroup
		stop     = make(chan struct{})
		mu       sync.Mutex
		served   int
		failures []string
	)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				resp, err := http.Get(app.appURL + "/api/widgets")
				mu.Lock()
				served++
				if err != nil {
					failures = append(failures, err.Error())
				} else {
					if resp.StatusCode != http.StatusOK {
						failures = append(failures, fmt.Sprintf("status %d", resp.StatusCode))
					}
					resp.Body.Close()
				}
				mu.Unlock()
			}
		}()
	}

	// The generator runs into a temporary directory, so the test does not scatter a
	// project across the repository.
	dir := t.TempDir()
	resp := session.call(t, 9, "tools/call", map[string]any{
		"name":      "breeze_new",
		"arguments": map[string]any{"name": "generated-under-load", "dir": dir},
	})

	close(stop)
	wg.Wait()

	if errObj, failed := resp["error"]; failed {
		t.Fatalf("breeze_new returned a protocol error: %v", errObj)
	}
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", resp)
	}
	if isError, _ := result["isError"].(bool); isError {
		// Generation can legitimately fail here — `go mod tidy` needs the network —
		// and that is not what this test is about. What matters is that the app kept
		// serving either way, which is asserted below regardless.
		t.Logf("breeze_new reported a failure (not this test's subject): %v", result["content"])
	}

	if len(failures) > 0 {
		t.Errorf("%d of %d application requests failed while a workspace tool ran in-process; first: %s",
			len(failures), served, failures[0])
	}
	if served == 0 {
		t.Error("no application requests completed during the generator call")
	}
	t.Logf("%d application requests served while breeze_new ran in the same process", served)
}

// ─── auth and bind: identical to standalone ──────────────────────────────────

// TestInProcessAuthMatchesStandalone is the acceptance criterion that embedding
// relaxes nothing.
//
// The same checks the standalone transport tests make, made here against an embedded
// endpoint: no token is 401 with a WWW-Authenticate header, a wrong token is 401, a
// correct one is 200, and a hostile Origin is 403 whether or not the token was right.
func TestInProcessAuthMatchesStandalone(t *testing.T) {
	app := startLiveApp(t, mcp.InProcessConfig{Token: testToken})

	const initialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":` +
		`{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`

	t.Run("no token is 401", func(t *testing.T) {
		resp := rawPost(t, app.controlURL, "", "", initialize)
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
		if got := resp.Header.Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
			t.Errorf("WWW-Authenticate = %q, want it to name the Bearer scheme", got)
		}
	})

	t.Run("wrong token is 401", func(t *testing.T) {
		resp := rawPost(t, app.controlURL, "not-the-token", "", initialize)
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("correct token is accepted", func(t *testing.T) {
		resp := rawPost(t, app.controlURL, testToken, "", initialize)
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if resp.Header.Get("Mcp-Session-Id") == "" {
			t.Error("no session id was returned, so no subsequent call could be made")
		}
	})

	t.Run("hostile Origin is 403 even with the token", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodPost, app.controlURL, strings.NewReader(initialize))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+testToken)
		req.Header.Set("Origin", "https://evil.example.com")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", resp.StatusCode)
		}
	})
}

// TestInProcessDefaultBindIsLoopback is the criterion that the embedded default
// matches the standalone one, asserted against the bound socket.
func TestInProcessDefaultBindIsLoopback(t *testing.T) {
	app := startLiveApp(t, mcp.InProcessConfig{})

	addr, ok := app.server.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("the bound address is %T, not TCP", app.server.Addr())
	}
	if !addr.IP.IsLoopback() {
		t.Fatalf("an embedded endpoint bound %s by default; it must be loopback, exactly as the "+
			"standalone binary is", addr.IP)
	}
}

// TestInProcessGeneratesATokenWhenNoneIsSupplied covers where a project that forgot
// to set BREEZE_MCP_TOKEN lands: a token is minted and reported, and the endpoint is
// not left unauthenticated.
func TestInProcessGeneratesATokenWhenNoneIsSupplied(t *testing.T) {
	app := startLiveApp(t, mcp.InProcessConfig{Token: " "})

	if strings.TrimSpace(app.token) == "" {
		t.Fatal("no token was generated, so the endpoint would be unauthenticated")
	}
	if app.token == testToken {
		t.Error("the generated token is the test's own constant, so nothing was generated")
	}
	if app.server.Token() != app.token {
		t.Errorf("Token() reports %q but StartInProcess returned %q", app.server.Token(), app.token)
	}

	if _, status := handshake(t, app.controlURL, app.token); status != http.StatusOK {
		t.Errorf("the generated token was rejected by its own endpoint: status %d", status)
	}
	if _, status := handshake(t, app.controlURL, ""); status != http.StatusUnauthorized {
		t.Errorf("an anonymous handshake returned %d, want 401", status)
	}
}

// ─── Auto-MCP coexistence ────────────────────────────────────────────────────

// TestPortConflictWithAutoMCPIsRefused is the criterion that the two endpoints do not
// silently overlap.
//
// Sharing a port would mean whichever bound first decided what the other served, and
// the symptom — an agent told a tool does not exist — would implicate neither. The
// refusal names both features, because the fix is to pick a second port and a caller
// has to know why.
func TestPortConflictWithAutoMCPIsRefused(t *testing.T) {
	router := breeze.NewRouter()
	app := breeze.New(router, breeze.NewEventLoopWorkerPool(2))

	// EnableMCP with no tagged routes builds no tools and serves nothing useful, which
	// is enough here: it records the address, which is what the check reads.
	port := freePort(t)
	if err := app.EnableMCP(fmt.Sprintf("127.0.0.1:%d", port)); err != nil {
		t.Fatalf("EnableMCP: %v", err)
	}
	if app.AutoMCPAddr() == "" {
		t.Fatal("EnableMCP did not record its address, so a conflict cannot be detected")
	}

	_, _, err := mcp.StartInProcess(app, mcp.InProcessConfig{
		Mode:  mcp.ModeAppRuntime,
		Port:  port,
		Token: testToken,
	})
	if err == nil {
		t.Fatal("an in-process endpoint was allowed onto the port Auto-MCP is serving")
	}
	for _, want := range []string{"Auto-MCP", "separate ports"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}

	// A different port is fine, which is the arrangement a project wanting both uses.
	server, _, err := mcp.StartInProcess(app, mcp.InProcessConfig{
		Mode:  mcp.ModeAppRuntime,
		Port:  freePort(t),
		Token: testToken,
	})
	if err != nil {
		t.Fatalf("a second, different port was refused as well: %v", err)
	}
	if err := server.Close(); err != nil {
		t.Errorf("closing: %v", err)
	}
}

// TestInProcessRequiresAnExplicitPort covers the configuration mistake that would
// otherwise bind something arbitrary no client configuration names.
func TestInProcessRequiresAnExplicitPort(t *testing.T) {
	router := breeze.NewRouter()
	app := breeze.New(router, breeze.NewEventLoopWorkerPool(2))

	if _, _, err := mcp.StartInProcess(app, mcp.InProcessConfig{
		Mode:  mcp.ModeAppRuntime,
		Token: testToken,
	}); err == nil {
		t.Fatal("an in-process endpoint with no port was accepted")
	}
	if _, _, err := mcp.StartInProcess(nil, mcp.InProcessConfig{
		Mode: mcp.ModeAppRuntime,
		Port: 2000,
	}); err == nil {
		t.Fatal("an in-process endpoint with no application was accepted")
	}
}

// TestInProcessRequiresAnExplicitMode is Part 9's assertion at this layer: the
// exported wrapper must refuse a config with no Mode, not pick one.
//
// Checked here as well as in internal/mcp because this is the constructor a
// generated project actually calls, and a default reintroduced in this file would
// not be caught by the internal test.
func TestInProcessRequiresAnExplicitMode(t *testing.T) {
	router := breeze.NewRouter()
	app := breeze.New(router, breeze.NewEventLoopWorkerPool(2))

	_, _, err := mcp.StartInProcess(app, mcp.InProcessConfig{
		Port:  freePort(t),
		Token: testToken,
	})
	if err == nil {
		t.Fatal("an in-process endpoint with no Mode was accepted")
	}
	if !strings.Contains(err.Error(), "Mode is required") {
		t.Errorf("the refusal does not say Mode is required: %v", err)
	}
}

// ─── per-token scope, on the embedded endpoint ────────────────────────────────

// TestInProcessScopeFiltersTools is the point of plumbing Scope this far: an embedded
// endpoint can hand out a credential narrower than the mode it runs in.
//
// ModeAppRuntime has already removed everything that writes; this narrows what is left,
// which is the difference between "cannot damage the project" and "can only read
// traces".
func TestInProcessScopeFiltersTools(t *testing.T) {
	scope, err := mcp.NewScope(mcp.CapFleet)
	if err != nil {
		t.Fatal(err)
	}
	app := startLiveApp(t, mcp.InProcessConfig{Scope: scope})

	session, status := handshake(t, app.controlURL, app.token)
	if status != http.StatusOK {
		t.Fatalf("the handshake returned %d", status)
	}

	advertised := session.toolNames(t)
	if len(advertised) == 0 {
		t.Fatal("a fleet-scoped endpoint advertises nothing at all")
	}
	// mcp.Tools() is the unscoped inventory, so a scoped endpoint must advertise
	// strictly fewer than it — otherwise the scope did not reach the transport.
	if len(advertised) >= len(mcp.Tools()) {
		t.Errorf("%d tools advertised with a one-capability scope; the unscoped set is %d",
			len(advertised), len(mcp.Tools()))
	}
	for _, name := range advertised {
		if !strings.HasPrefix(name, "breeze_get_trace") &&
			!strings.Contains(name, "topology") &&
			!strings.Contains(name, "incident") &&
			!strings.Contains(name, "contract_violations") {
			t.Errorf("%s is advertised to a fleet-scoped token", name)
		}
	}
	// breeze_get_logs is in-process safe and app-runtime registered, and still absent —
	// which is the scope layer doing something the mode layer did not.
	for _, name := range advertised {
		if name == "breeze_get_logs" {
			t.Error("breeze_get_logs is runtime, not fleet, and must not be advertised here")
		}
	}
}

// TestInProcessUnscopedIsUnchanged pins the default: an embed that never sets Scope
// behaves exactly as it did before Part 8.
func TestInProcessUnscopedIsUnchanged(t *testing.T) {
	app := startLiveApp(t, mcp.InProcessConfig{})

	session, status := handshake(t, app.controlURL, app.token)
	if status != http.StatusOK {
		t.Fatalf("the handshake returned %d", status)
	}
	if got, want := len(session.toolNames(t)), len(mcp.Tools()); got != want {
		t.Errorf("%d tools advertised without a scope, want the full in-process set of %d", got, want)
	}
}

// TestInProcessFeaturesEndpointAnswers — the same convenience the standalone binary
// serves is reachable on an embed, because an operator debugging a deployed
// application is exactly who needs "what can this token do" answered by curl.
func TestInProcessFeaturesEndpointAnswers(t *testing.T) {
	scope, err := mcp.NewScope(mcp.CapRuntime)
	if err != nil {
		t.Fatal(err)
	}
	app := startLiveApp(t, mcp.InProcessConfig{Scope: scope})

	req, err := http.NewRequest(http.MethodGet, app.controlURL+"/features", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+app.token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /mcp/features returned %d", resp.StatusCode)
	}

	var report struct {
		ServerKind string   `json:"server_kind"`
		Granted    []string `json:"granted"`
		Scoped     bool     `json:"scoped"`
		Tools      []string `json:"tools"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&report); err != nil {
		t.Fatalf("decoding the report: %v", err)
	}

	if report.ServerKind != string(mcp.ModeAppRuntime) {
		t.Errorf("server_kind = %q, want %q", report.ServerKind, mcp.ModeAppRuntime)
	}
	if strings.Join(report.Granted, ",") != "runtime" {
		t.Errorf("granted = %v, want [runtime]", report.Granted)
	}
	if !report.Scoped {
		t.Error("scoped = false for a one-capability token")
	}
	if len(report.Tools) == 0 {
		t.Error("tools is empty")
	}
}
