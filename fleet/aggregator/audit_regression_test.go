package aggregator

// Regression tests for the audit findings in the aggregator category:
//
//   - every route ran on the gnet event loop, including a 32 MiB gunzip+decode and
//     a log fan-out that waits seconds on other services;
//   - heartbeat-supplied URLs (openapi_url) were dereferenced without any guard,
//     and the log fan-out sent ServiceToken to whatever address a heartbeat named;
//   - contractEngine.hub was assigned after its goroutine started (data race);
//   - the ingest wrapper discarded the wrapped handler's error.

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nelthaarion/breeze/v2"
	"github.com/nelthaarion/breeze/v2/fleet"
	"github.com/nelthaarion/breeze/v2/fleet/contracts"
)

func TestEveryAggregatorRouteRunsOffTheEventLoop(t *testing.T) {
	router := breeze.NewRouter()
	cfg := DefaultConfig()
	cfg.BasePath = "/fleet"
	_ = installWithStore(router, cfg, nil)

	var n int
	for _, ri := range router.RoutesInfo() {
		if !strings.HasPrefix(ri.Pattern(), "/fleet/api/") {
			continue
		}
		n++
		if !ri.Blocking() {
			t.Errorf("%s %s runs inline on the event loop; ingest decode and log fan-out must use the worker pool",
				ri.Method(), ri.Pattern())
		}
	}
	if n < 9 {
		t.Fatalf("expected the aggregator's API routes to be registered, found %d", n)
	}
}

func TestCheckFetchURLPolicy(t *testing.T) {
	orig := lookupIPs
	defer func() { lookupIPs = orig }()
	lookupIPs = func(_ context.Context, host string) ([]net.IP, error) {
		switch host {
		case "rebind.example":
			return []net.IP{net.ParseIP("169.254.169.254")}, nil
		case "orders.internal":
			return []net.IP{net.ParseIP("10.1.2.3")}, nil
		}
		return nil, errors.New("no such host")
	}

	refuse := []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://[fe80::1]/x",
		"http://0.0.0.0:8080/x",
		"http://224.0.0.1/x",
		"http://metadata.google.internal/computeMetadata/v1/",
		"http://100.100.100.200/latest/meta-data/",
		"http://2852039166/",      // decimal form of 169.254.169.254
		"http://0xA9FEA9FE/",      // hex form
		"http://rebind.example/x", // name that resolves to the metadata address
		"file:///etc/passwd",
		"gopher://10.0.0.1/",
		"http://user:pass@10.0.0.1/", // embedded credentials
		"http:///nohost",
	}
	for _, u := range refuse {
		if err := checkFetchURL(u, nil); err == nil {
			t.Errorf("checkFetchURL(%q) = nil, want a refusal", u)
		}
	}

	allow := []string{
		"http://10.1.2.3:8080/openapi.json",
		"http://127.0.0.1:9000/openapi.json",
		"https://orders.internal/openapi.json",
		"http://svc.cluster.local:8080/x", // unresolvable here; the fetch itself will fail
	}
	for _, u := range allow {
		if err := checkFetchURL(u, nil); err != nil {
			t.Errorf("checkFetchURL(%q) = %v, want nil (private and loopback services are normal)", u, err)
		}
	}
}

func TestCheckFetchURLAllowlist(t *testing.T) {
	hosts := []string{"orders.internal", "10.1.2.3:8080"}
	if err := checkFetchURL("http://10.1.2.3:8080/x", hosts); err != nil {
		t.Errorf("listed host:port refused: %v", err)
	}
	if err := checkFetchURL("http://10.1.2.3:9999/x", hosts); err == nil {
		t.Error("a different port of a listed IP must not pass a host:port entry")
	}
	if err := checkFetchURL("http://10.9.9.9/x", hosts); err == nil {
		t.Error("unlisted host must be refused when an allowlist is set")
	}
}

func heartbeatWithURL(service, openapi string) fleet.Heartbeat {
	return fleet.Heartbeat{Service: service, OpenAPIHash: "h1", OpenAPIURL: openapi}
}

// A heartbeat can name any openapi_url. Without IngestToken, log fan-out would
// send ServiceToken to it; it must stay off instead.
func TestLogFanoutIsOffWhenHeartbeatsAreUnauthenticated(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ServiceToken = "shared-secret"
	cfg.IngestToken = ""
	a := installWithStore(breeze.NewRouter(), cfg, nil)
	defer a.Close(context.Background())

	if err := a.AcceptHeartbeat(heartbeatWithURL("evil", "http://203.0.113.50/openapi.json")); err != nil {
		t.Fatal(err)
	}
	if eps := a.logEndpoints([]string{"evil"}); len(eps) != 0 {
		t.Fatalf("log fan-out would send ServiceToken to %v on the strength of an unauthenticated heartbeat", eps)
	}
}

func TestLogFanoutSkipsForbiddenTargetsEvenWhenAuthenticated(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ServiceToken = "shared-secret"
	cfg.IngestToken = "ingest"
	a := installWithStore(breeze.NewRouter(), cfg, nil)
	defer a.Close(context.Background())

	_ = a.AcceptHeartbeat(heartbeatWithURL("meta", "http://169.254.169.254/openapi.json"))
	_ = a.AcceptHeartbeat(heartbeatWithURL("orders", "http://10.1.2.3:8080/openapi.json"))

	eps := a.logEndpoints([]string{"meta", "orders"})
	if _, bad := eps["meta"]; bad {
		t.Errorf("metadata address accepted as a log endpoint: %v", eps)
	}
	if eps["orders"] != "http://10.1.2.3:8080/dashboard" {
		t.Errorf("legitimate private service lost: %v", eps)
	}
}

func TestLogFanoutRespectsAllowlistWithoutIngestToken(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ServiceToken = "shared-secret"
	cfg.FetchAllowedHosts = []string{"10.1.2.3:8080"}
	a := installWithStore(breeze.NewRouter(), cfg, nil)
	defer a.Close(context.Background())

	_ = a.AcceptHeartbeat(heartbeatWithURL("orders", "http://10.1.2.3:8080/openapi.json"))
	_ = a.AcceptHeartbeat(heartbeatWithURL("other", "http://10.9.9.9:8080/openapi.json"))
	eps := a.logEndpoints([]string{"orders", "other"})
	if len(eps) != 1 || eps["orders"] == "" {
		t.Fatalf("only the allowlisted host may be used: %v", eps)
	}
}

func TestSchemaRegistryRefusesGuardedURLsWithoutFetching(t *testing.T) {
	reg := contracts.NewSchemaRegistry(nil)
	reg.SetURLGuard(func(u string) error { return checkFetchURL(u, nil) })
	_, err := reg.Refresh(context.Background(), "svc", "h", "http://169.254.169.254/latest/meta-data/")
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("Refresh must refuse a metadata URL before fetching, got %v", err)
	}
}

// Run with -race: setHub races with the engine goroutine's read otherwise.
func TestContractHubCanBeInstalledWhileEngineRuns(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ContractValidation = true
	e := newContractEngine(cfg) // starts its own goroutine
	defer func() { close(e.stop); <-e.done }()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			e.enqueueSpan(fleet.Span{Service: "s", Route: "/r", Method: "GET", RequestPayload: []byte(`{}`)}, "c")
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			e.setHub(func(contracts.Group) {})
			time.Sleep(50 * time.Microsecond)
		}
	}()
	wg.Wait()
}
