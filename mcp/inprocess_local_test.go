package mcp_test

// An embedded endpoint reading the dashboard of the application it lives in, through
// StartInProcess: the function a generated application calls. The dashboard here has
// credentials the endpoint is not told, which is the point: it reads the collector in
// memory, not the dashboard's HTTP API.

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/nelthaarion/breeze/v2/mcp"
)

func TestInProcessReadsItsOwnProcessWithoutCredentials(t *testing.T) {
	app := startLiveApp(t, mcp.InProcessConfig{})
	hit(t, app.appURL+"/api/widgets")

	session, status := handshake(t, app.controlURL, app.token)
	if status != http.StatusOK {
		t.Fatalf("the handshake returned %d", status)
	}

	callTool := func(id int, name string, args map[string]any) (structured map[string]any, isError bool) {
		resp := session.call(t, id, "tools/call", map[string]any{"name": name, "arguments": args})
		if errObj, failed := resp["error"]; failed {
			t.Fatalf("%s: protocol error: %v", name, errObj)
		}
		result, _ := resp["result"].(map[string]any)
		structured, _ = result["structuredContent"].(map[string]any)
		isError, _ = result["isError"].(bool)
		return structured, isError
	}

	// The premise: over HTTP, without the dashboard's credentials, the same read is refused.
	// Without this the test could pass because the dashboard is open to anyone.
	if got, isError := callTool(10, "breeze_get_routes", map[string]any{"service_url": app.appURL}); !isError {
		t.Fatalf("the dashboard answered an anonymous read over HTTP, so this proves nothing: %v", got)
	}

	for i, tool := range []string{"breeze_get_routes", "breeze_get_recent_errors", "breeze_get_logs", "breeze_get_health", "breeze_get_performance", "breeze_get_signals"} {
		got, isError := callTool(20+i, tool, map[string]any{})
		if isError {
			t.Errorf("%s with no service_url failed on the endpoint StartInProcess serves: %v", tool, got)
			continue
		}
		if got["service_url"] != "(this process)" {
			t.Errorf("%s: service_url = %v, want this process", tool, got["service_url"])
		}
		if tool == "breeze_get_routes" && !strings.Contains(fmt.Sprint(got), "/api/widgets") {
			t.Errorf("the routes read from memory do not include one this application serves: %v", got)
		}
	}
}
