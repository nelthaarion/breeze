package aggregator

// Blast-radius tests (§14.12).
//
// The whole value of this feature is trust: a banner that names the wrong service,
// or that fires during normal operation, is worse than no banner at all — someone
// will chase the wrong process during an outage, or learn to ignore it. So the
// tests here are mostly about what must *not* be reported: services that degraded
// gracefully, healthy fleets, unreachable nodes, and error rates computed from two
// requests.

import (
	"strings"
	"testing"
	"time"
)

// failEdge records n calls from caller to callee, of which failures fail.
//
// Written as a helper because every test here is a statement about traffic shape,
// and the arithmetic of "20 calls, 18 failing" should not be retyped in each one.
func failEdge(g *TopologyGraph, caller, callee string, calls, failures int, now time.Time) {
	for i := 0; i < calls; i++ {
		status := 200
		if i < failures {
			status = 500
		}
		g.Observe(topoSpan(callee, "b", "a", status, 5), caller, now)
	}
}

// failEntry records traffic a service received with no in-fleet caller — a
// gateway's own inbound requests, which is what makes it fail as a node without
// any edge pointing at it.
func failEntry(g *TopologyGraph, service string, calls, failures int, now time.Time) {
	for i := 0; i < calls; i++ {
		status := 200
		if i < failures {
			status = 500
		}
		g.Observe(topoSpan(service, "a", "", status, 40), "", now)
	}
}

// incidentFor picks one service's blast radius out of the reported set.
func incidentFor(got []BlastRadius, service string) (BlastRadius, bool) {
	for _, br := range got {
		if br.Service == service {
			return br, true
		}
	}
	return BlastRadius{}, false
}

func TestIncidentsHealthyFleetIsQuiet(t *testing.T) {
	g := NewTopologyGraph(Config{})
	now := time.Now()

	failEdge(g, "gateway", "orders", 100, 0, now)
	failEdge(g, "orders", "payments", 100, 0, now)

	if got := Incidents(g, Config{}, now); len(got) != 0 {
		t.Errorf("Incidents = %+v, want none for a healthy fleet", got)
	}
}

// TestIncidentsIgnoresLowVolume is the false-positive guard that matters most in
// practice: one failed health check against an idle service is a 100% error rate,
// and must not raise a fleet-wide incident.
func TestIncidentsIgnoresLowVolume(t *testing.T) {
	g := NewTopologyGraph(Config{})
	now := time.Now()

	failEdge(g, "gateway", "orders", 3, 3, now)

	if got := Incidents(g, Config{}, now); len(got) != 0 {
		t.Errorf("Incidents = %+v, want none: 3 calls is not evidence of an outage", got)
	}
}

func TestIncidentsBelowThresholdIsQuiet(t *testing.T) {
	g := NewTopologyGraph(Config{})
	now := time.Now()

	// 5% errors, under the 10% default.
	failEdge(g, "gateway", "orders", 100, 5, now)

	if got := Incidents(g, Config{}, now); len(got) != 0 {
		t.Errorf("Incidents = %+v, want none below the threshold", got)
	}
}

func TestIncidentsFlagsDegradedService(t *testing.T) {
	g := NewTopologyGraph(Config{})
	now := time.Now()

	failEdge(g, "gateway", "orders", 100, 14, now)

	got := Incidents(g, Config{}, now)
	if len(got) != 1 {
		t.Fatalf("Incidents = %d entries, want 1", len(got))
	}
	if got[0].Service != "orders" {
		t.Errorf("service = %q, want orders", got[0].Service)
	}
	if got[0].ErrorRate != 0.14 {
		t.Errorf("error rate = %v, want 0.14", got[0].ErrorRate)
	}
	if got[0].Calls != 100 {
		t.Errorf("calls = %d, want 100", got[0].Calls)
	}
}

// TestBlastRadiusFindsAffectedCallers is the core §9B.2 case: orders fails, the
// gateway calling it fails too, and the gateway is the one being *impacted* — not
// the cause.
func TestBlastRadiusFindsAffectedCallers(t *testing.T) {
	g := NewTopologyGraph(Config{})
	now := time.Now()

	// orders is broken.
	failEdge(g, "gateway", "orders", 100, 100, now)
	// The gateway's own inbound traffic fails as a result.
	failEntry(g, "gateway", 100, 100, now)

	br, ok := incidentFor(Incidents(g, Config{}, now), "orders")
	if !ok {
		t.Fatal("orders was not reported as an incident")
	}
	if len(br.Affected) != 1 {
		t.Fatalf("affected = %+v, want just the gateway", br.Affected)
	}
	a := br.Affected[0]
	if a.Service != "gateway" {
		t.Errorf("affected service = %q, want gateway", a.Service)
	}
	if a.Hops != 1 {
		t.Errorf("hops = %d, want 1 for a direct caller", a.Hops)
	}
	if a.Via != "orders" {
		t.Errorf("via = %q, want orders", a.Via)
	}
	if a.AttributedShare <= 0 {
		t.Errorf("attributed share = %v, want a positive share", a.AttributedShare)
	}
}

// TestBlastRadiusExcludesGracefulDegradation is the trust property. A service that
// calls a broken dependency and recovers — cache, fallback, retry — is working
// correctly, and naming it as collateral damage would be wrong.
func TestBlastRadiusExcludesGracefulDegradation(t *testing.T) {
	g := NewTopologyGraph(Config{})
	now := time.Now()

	// orders fails every call it receives from the gateway...
	failEdge(g, "gateway", "orders", 100, 100, now)
	// ...but the gateway serves all of its own traffic successfully.
	failEntry(g, "gateway", 100, 0, now)

	br, ok := incidentFor(Incidents(g, Config{}, now), "orders")
	if !ok {
		t.Fatal("orders was not reported as an incident")
	}
	if len(br.Affected) != 0 {
		t.Errorf("affected = %+v, want none: the gateway degraded gracefully", br.Affected)
	}
	if !strings.Contains(br.Banner, "no impact") {
		t.Errorf("banner = %q, want it to state no impact was detected", br.Banner)
	}
}

// TestBlastRadiusStopsAtDisconnectedNodes — §14.12 explicitly: a service with no
// observed edge to the unhealthy one must never appear, no matter how unhealthy it
// is on its own.
func TestBlastRadiusStopsAtDisconnectedNodes(t *testing.T) {
	g := NewTopologyGraph(Config{})
	now := time.Now()

	// One broken dependency chain.
	failEdge(g, "gateway", "orders", 100, 100, now)
	failEntry(g, "gateway", 100, 100, now)

	// An entirely separate, also-broken pipeline that shares no edge.
	failEdge(g, "cron", "reports", 100, 100, now)
	failEntry(g, "cron", 100, 100, now)

	br, ok := incidentFor(Incidents(g, Config{}, now), "orders")
	if !ok {
		t.Fatal("orders was not reported as an incident")
	}
	for _, a := range br.Affected {
		if a.Service == "cron" || a.Service == "reports" {
			t.Errorf("blast radius of orders included %q, which has no edge to it", a.Service)
		}
	}
}

// TestBlastRadiusMultiHop checks BFS reaches a caller's caller and records the
// shortest distance.
func TestBlastRadiusMultiHop(t *testing.T) {
	g := NewTopologyGraph(Config{})
	now := time.Now()

	// gateway → orders → payments, with payments broken and failing upward.
	failEdge(g, "orders", "payments", 100, 100, now)
	failEdge(g, "gateway", "orders", 100, 100, now)
	failEntry(g, "gateway", 100, 100, now)

	br, ok := incidentFor(Incidents(g, Config{}, now), "payments")
	if !ok {
		t.Fatal("payments was not reported as an incident")
	}
	hops := map[string]int{}
	for _, a := range br.Affected {
		hops[a.Service] = a.Hops
	}
	if hops["orders"] != 1 {
		t.Errorf("orders hops = %d, want 1", hops["orders"])
	}
	if hops["gateway"] != 2 {
		t.Errorf("gateway hops = %d, want 2", hops["gateway"])
	}
}

// TestBlastRadiusTerminatesOnCycles — a retry or callback makes A call B and B call
// A, and a traversal without a visited set would never return.
func TestBlastRadiusTerminatesOnCycles(t *testing.T) {
	g := NewTopologyGraph(Config{})
	now := time.Now()

	failEdge(g, "a", "b", 100, 100, now)
	failEdge(g, "b", "a", 100, 100, now)

	done := make(chan []BlastRadius, 1)
	go func() { done <- Incidents(g, Config{}, now) }()

	select {
	case got := <-done:
		for _, br := range got {
			for _, a := range br.Affected {
				if a.Service == br.Service {
					t.Errorf("%q appears in its own blast radius", a.Service)
				}
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Incidents did not terminate on a cyclic graph")
	}
}

// TestBlastRadiusAttributedShareIsCapped — a dependency cannot be responsible for
// more failures than its caller actually had.
func TestBlastRadiusAttributedShareIsCapped(t *testing.T) {
	g := NewTopologyGraph(Config{})
	now := time.Now()

	// Every gateway→orders call fails, but the gateway only fails a fifth of
	// its own requests (it retries or falls back for the rest).
	failEdge(g, "gateway", "orders", 100, 100, now)
	failEntry(g, "gateway", 100, 20, now)

	br, ok := incidentFor(Incidents(g, Config{}, now), "orders")
	if !ok {
		t.Fatal("orders was not reported as an incident")
	}
	callerRate, _ := g.WindowedErrorRate("gateway", now)
	for _, a := range br.Affected {
		if a.Service != "gateway" {
			continue
		}
		if a.AttributedShare > callerRate {
			t.Errorf("attributed share %v exceeds the gateway's own error rate %v",
				a.AttributedShare, callerRate)
		}
	}
}

func TestBlastRadiusBannerNamesOriginAndImpact(t *testing.T) {
	g := NewTopologyGraph(Config{})
	now := time.Now()

	failEdge(g, "gateway", "orders", 100, 100, now)
	failEntry(g, "gateway", 100, 100, now)

	br, ok := incidentFor(Incidents(g, Config{}, now), "orders")
	if !ok {
		t.Fatal("orders was not reported as an incident")
	}
	for _, want := range []string{"orders", "degraded", "gateway"} {
		if !strings.Contains(br.Banner, want) {
			t.Errorf("banner %q missing %q", br.Banner, want)
		}
	}
}

func TestBlastRadiusBannerSingularForOneService(t *testing.T) {
	g := NewTopologyGraph(Config{})
	now := time.Now()

	failEdge(g, "gateway", "orders", 100, 100, now)
	failEntry(g, "gateway", 100, 100, now)

	br, _ := incidentFor(Incidents(g, Config{}, now), "orders")
	if strings.Contains(br.Banner, "1 downstream services") {
		t.Errorf("banner %q should read 'service' for a single affected service", br.Banner)
	}
}

// TestIncidentsSortedByImpact — the UI shows one banner, so the largest incident
// must come first.
func TestIncidentsSortedByImpact(t *testing.T) {
	g := NewTopologyGraph(Config{})
	now := time.Now()

	// A broken service with two affected callers.
	failEdge(g, "gateway", "orders", 100, 100, now)
	failEdge(g, "admin", "orders", 100, 100, now)
	failEntry(g, "gateway", 100, 100, now)
	failEntry(g, "admin", 100, 100, now)

	// A separately broken leaf with nothing calling it.
	failEntry(g, "lonely", 100, 100, now)

	got := Incidents(g, Config{}, now)
	if len(got) < 2 {
		t.Fatalf("Incidents = %d, want several", len(got))
	}
	if len(got[0].Affected) < len(got[len(got)-1].Affected) {
		t.Errorf("incidents are not sorted by impact: %d affected first, %d last",
			len(got[0].Affected), len(got[len(got)-1].Affected))
	}
}

func TestIncidentsNilGraph(t *testing.T) {
	if got := Incidents(nil, Config{}, time.Now()); got != nil {
		t.Errorf("Incidents(nil) = %+v, want nil", got)
	}
}

// TestIncidentsRespectsCustomThreshold verifies the config knob is actually read
// rather than the default being hardcoded.
func TestIncidentsRespectsCustomThreshold(t *testing.T) {
	g := NewTopologyGraph(Config{})
	now := time.Now()

	failEdge(g, "gateway", "orders", 100, 6, now)

	if got := Incidents(g, Config{}, now); len(got) != 0 {
		t.Fatalf("Incidents at the default threshold = %+v, want none", got)
	}
	strict := Config{BlastRadiusErrorRateThreshold: 0.05}
	if got := Incidents(g, strict, now); len(got) != 1 {
		t.Errorf("Incidents at a 5%% threshold = %d, want 1", len(got))
	}
}

// TestBlastRadiusIgnoresStaleEdges — an incident must be computed over current
// dependencies, not ones a service dropped long ago.
func TestBlastRadiusIgnoresStaleEdges(t *testing.T) {
	cfg := Config{TraceTTL: time.Minute, BlastRadiusWindow: time.Minute}
	g := NewTopologyGraph(cfg)
	start := time.Now()

	failEdge(g, "gateway", "orders", 100, 100, start)
	failEntry(g, "gateway", 100, 100, start)

	// Recompute long after every edge fell outside TraceTTL. The TTL that makes
	// them stale is the graph's own (passed to NewTopologyGraph above), not a
	// parameter of the walk — which is why ComputeBlastRadius takes no Config.
	later := start.Add(time.Hour)
	br := ComputeBlastRadius(g, "orders", 1.0, 100, later)
	if len(br.Affected) != 0 {
		t.Errorf("affected = %+v, want none: every edge is stale", br.Affected)
	}
}
