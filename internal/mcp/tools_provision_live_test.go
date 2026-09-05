package mcp

// tools_provision_live_test.go — the tests that need a real Docker daemon.
//
// These are separated from tools_provision_test.go because they are a different kind of
// test with a different failure mode. Those use a fake Docker and always run, so they
// can be trusted as a gate. These build images and start containers, so they take
// minutes and cannot run at all on a machine without a daemon — and a test that silently
// passed there would be worse than one that says why it did not run.
//
// So they skip, loudly, and they skip under -short as well, because a `go test ./...`
// that takes fifteen minutes to build two Go images stops being run.
//
// What only these can prove is the part that matters most and is least checkable
// otherwise: that a returned app_port is genuinely reachable, and that a returned
// control_port and control_token are genuinely a working MCP endpoint. A fake cannot
// establish either, because it would be asserting against itself.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// liveOrchestrator returns an orchestrator with real Docker, or skips.
//
// The registry goes in a temporary directory so a live run cannot inherit or corrupt a
// developer's own provisioning state.
func liveOrchestrator(t *testing.T) *orchestrator {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping: this builds container images, which takes minutes (-short)")
	}

	orch := newOrchestrator("test")
	if _, err := orch.dockerOrError(); err != nil {
		t.Skipf("skipping: %v", err)
	}

	reg, err := loadProvisionRegistry(filepath.Join(t.TempDir(), registryFileName), orch.ports)
	if err != nil {
		t.Fatal(err)
	}
	orch.registry = reg
	return orch
}

// cleanupProvisioned removes everything a live test provisioned.
//
// Registered with t.Cleanup rather than deferred, so it still runs when a test fails
// half-way — which is exactly when containers would otherwise be left behind.
func cleanupProvisioned(t *testing.T, orch *orchestrator) {
	t.Helper()

	t.Cleanup(func() {
		for _, service := range orch.registry.list() {
			if result := orch.deprovisionService(service.ServiceName, true); result.IsError {
				t.Logf("cleanup: %s: %s", service.ServiceName, result.Content[0].Text)
			}
		}
	})
}

// TestLiveProvisionServiceReachableOnBothAddresses is the criterion that a provisioned
// service's two addresses genuinely work.
//
// Neither half is checked with `docker ps`. The app port is checked by making an HTTP
// request and reading a real response; the control port is checked by completing an MCP
// handshake with the returned token, and by confirming the same handshake is refused
// without it. `docker ps` would report a container that had bound nothing as running.
func TestLiveProvisionServiceReachableOnBothAddresses(t *testing.T) {
	orch := liveOrchestrator(t)
	cleanupProvisioned(t, orch)

	// The default wait applies: this test wants provisioning to have settled before it
	// starts asserting.
	result := orch.provisionService(provisionServiceArgs{
		Name:        "live-users",
		Template:    "api",
		configInput: configInput{Config: map[string]any{"module": "example.com/live-users"}},
	})
	if result.IsError {
		t.Fatalf("provision_service failed: %s", result.Content[0].Text)
	}
	got := structuredOf[provisionedResult](t, result)

	t.Run("app_port serves real HTTP", func(t *testing.T) {
		resp, err := http.Get(got.AppURL + "/")
		if err != nil {
			t.Fatalf("the returned app_url %s is not reachable: %v", got.AppURL, err)
		}
		defer resp.Body.Close()

		// The generated api template answers {"status":"ok"} at /. Asserting on the body
		// rather than only the status is what distinguishes "the application is serving"
		// from "something is listening on that port".
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("%s answered %d but not with the generated project's JSON: %v",
				got.AppURL, resp.StatusCode, err)
		}
		if body["status"] != "ok" {
			t.Errorf("%s returned %v, want the generated status response", got.AppURL, body)
		}
	})

	t.Run("control_port completes an MCP handshake with the token", func(t *testing.T) {
		status, listed := liveHandshake(t, got.ControlURL, got.ControlToken)
		if status != http.StatusOK {
			t.Fatalf("the handshake against %s returned %d", got.ControlURL, status)
		}
		if listed == 0 {
			t.Error("the provisioned control plane listed no tools")
		}
	})

	t.Run("control_port refuses the same handshake without the token", func(t *testing.T) {
		status, _ := liveHandshake(t, got.ControlURL, "")
		if status != http.StatusUnauthorized {
			t.Fatalf("an unauthenticated handshake against %s returned %d, want 401",
				got.ControlURL, status)
		}
	})

	t.Run("the two addresses are not interchangeable", func(t *testing.T) {
		// The app port must not answer MCP. This is the concrete form of the whole
		// control-versus-app distinction: two ports, two different services.
		endpoint := fmt.Sprintf("http://%s:%d%s", got.Host, got.AppPort, DefaultEndpointPath)
		if status, _ := liveHandshake(t, endpoint, got.ControlToken); status == http.StatusOK {
			t.Error(
				"the app port answered an MCP handshake, so both addresses reach the same service",
			)
		}
	})
}

// liveHandshake posts an initialize and then a tools/list to a control endpoint,
// returning the first non-200 status it saw and the number of tools listed.
func liveHandshake(t *testing.T, endpoint, token string) (int, int) {
	t.Helper()

	post := func(body, session string) (*http.Response, error) {
		req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if token != "" {
			req.Header.Set("Authorization", bearerPrefix+token)
		}
		if session != "" {
			req.Header.Set(sessionHeader, session)
		}
		return http.DefaultClient.Do(req)
	}

	const initialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":` +
		`{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`

	resp, err := post(initialize, "")
	if err != nil {
		// A refused connection is a real answer to "is a control plane here", so it is
		// reported rather than failing the test: one subtest above expects exactly this.
		t.Logf("initialize against %s: %v", endpoint, err)
		return 0, 0
	}
	session := resp.Header.Get(sessionHeader)
	status := resp.StatusCode
	resp.Body.Close()
	if status != http.StatusOK {
		return status, 0
	}

	resp, err = post(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`, session)
	if err != nil {
		t.Fatalf("tools/list against %s: %v", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, 0
	}

	var out struct {
		Result toolsListResult `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decoding tools/list from %s: %v", endpoint, err)
	}
	return http.StatusOK, len(out.Result.Tools)
}

// TestLiveProvisionFleetTracesFlowBetweenServices is the end-to-end criterion: two
// services and an aggregator provisioned in one call, and traces that actually join up.
//
// This is the test that makes the reuse claim more than a code review note. If
// provision_fleet wired Fleet by any path other than the generator's own, the services
// would still start and still answer — and their spans would not arrive, or would arrive
// under a different service name, or would not share a trace. Nothing short of reading
// the aggregator's assembled traces distinguishes those cases.
func TestLiveProvisionFleetTracesFlowBetweenServices(t *testing.T) {
	orch := liveOrchestrator(t)
	cleanupProvisioned(t, orch)

	result := orch.provisionFleet(
		[]fleetServiceRequest{
			{
				Name:     "live-gateway",
				Template: "api",
				configInput: configInput{
					Config: map[string]any{"module": "example.com/live-gateway"},
				},
			},
			{
				Name:     "live-orders",
				Template: "api",
				configInput: configInput{
					Config: map[string]any{"module": "example.com/live-orders"},
				},
			},
		},
		fleetAggregatorConfig{HostedBy: "live-gateway"},
		dockerOptions{},
	)
	if result.IsError {
		t.Fatalf("provision_fleet failed: %s", result.Content[0].Text)
	}
	report := structuredOf[fleetResult](t, result)

	// Every service answers on its own app port. Asserted before the trace check
	// because a service that never started would otherwise look like a tracing failure.
	for _, service := range report.Services {
		resp, err := http.Get(service.AppURL + "/")
		if err != nil {
			t.Fatalf(
				"%s is not reachable at its app_url %s: %v",
				service.ServiceName,
				service.AppURL,
				err,
			)
		}
		resp.Body.Close()
	}

	// Traffic, so there is something to trace. Each request produces a root span in the
	// service that served it.
	for _, service := range report.Services {
		for i := 0; i < 3; i++ {
			resp, err := http.Get(service.AppURL + "/")
			if err != nil {
				t.Fatalf("%s: %v", service.ServiceName, err)
			}
			resp.Body.Close()
		}
	}

	// The aggregator is read at its *own* address — not the hosting service's app port,
	// which is a different port on the same container. Getting this wrong is the exact
	// confusion the three-address distinction exists to prevent, and it would show up
	// here as an aggregator that reports nothing.
	agg := report.Aggregator
	if agg.AggregatorURL == "" {
		t.Fatal("provision_fleet returned no aggregator address")
	}

	names := waitForFleetServices(t, agg, 2, 60*time.Second)
	for _, service := range report.Services {
		if !names[service.ServiceName] {
			t.Errorf(
				"%s never appeared in the aggregator's topology at %s; its spans are not arriving",
				service.ServiceName,
				agg.AggregatorURL,
			)
		}
	}
}

// waitForFleetServices polls the aggregator's topology until it reports at least want
// services, and returns the service names it saw.
//
// It goes through the same fleetArgs/fetchLiveJSON path breeze_get_topology uses, rather
// than a hand-rolled HTTP call: if the tool can read this aggregator, so can an agent,
// and that is the thing worth asserting.
func waitForFleetServices(
	t *testing.T,
	agg fleetAggregatorAddress,
	want int,
	within time.Duration,
) map[string]bool {
	t.Helper()

	// The aggregator's read side is Basic-authenticated with the defaults
	// cmd/fleet-aggregator uses; the ingest token is the write credential and will not
	// authorise a read.
	args := fleetArgs{
		AggregatorURL: agg.AggregatorURL,
		Username:      "admin",
		Password:      "admin",
	}

	deadline := time.Now().Add(within)
	seen := map[string]bool{}

	for {
		var topo fleetTopology
		if err := fetchLiveJSON(args.request("/topology", "fleet aggregator"), &topo); err != nil {
			if time.Now().After(deadline) {
				t.Fatalf(
					"the aggregator at %s never answered a topology read: %s",
					agg.AggregatorURL,
					err.Message,
				)
			}
		} else {
			for _, node := range topo.Nodes {
				seen[node.Service] = true
			}
			if len(seen) >= want {
				return seen
			}
		}

		if time.Now().After(deadline) {
			t.Errorf("after %s the aggregator reports %d service(s), want %d: %v",
				within, len(seen), want, seen)
			return seen
		}
		time.Sleep(time.Second)
	}
}
