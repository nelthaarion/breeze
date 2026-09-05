package aggregator

// Topology tests.
//
// Two classes of property here. First, the graph must describe real observed
// calls — no phantom edges, no self-loops, callers present as nodes. Second, the
// numbers on it must be honest: percentiles from a bucketed histogram, error rates
// per direction, and a rolling window that actually forgets.

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/nelthaarion/breeze/fleet"
)

// topoSpan builds a span with just the fields the graph reads.
func topoSpan(service, spanID, parentID string, status int, durMs float64) fleet.Span {
	return fleet.Span{
		TraceID:      tid("a"),
		SpanID:       sid(spanID),
		ParentSpanID: parentIDOrEmpty(parentID),
		Service:      service,
		Route:        "/x",
		Method:       "GET",
		Status:       status,
		DurationMs:   durMs,
	}
}

func parentIDOrEmpty(fragment string) string {
	if fragment == "" {
		return ""
	}
	return sid(fragment)
}

func findEdge(t *testing.T, top Topology, caller, callee string) Edge {
	t.Helper()
	for _, e := range top.Edges {
		if e.Caller == caller && e.Callee == callee {
			return e
		}
	}
	t.Fatalf("no %s→%s edge in %+v", caller, callee, top.Edges)
	return Edge{}
}

func findNode(t *testing.T, top Topology, service string) Node {
	t.Helper()
	for _, n := range top.Nodes {
		if n.Service == service {
			return n
		}
	}
	t.Fatalf("no %s node in %+v", service, top.Nodes)
	return Node{}
}

func TestTopologyBuildsEdgesFromObservedCalls(t *testing.T) {
	g := NewTopologyGraph(Config{})
	now := time.Now()

	g.Observe(topoSpan("gateway", "a", "", 200, 40), "", now)
	g.Observe(topoSpan("orders", "b", "a", 200, 20), "gateway", now)

	top := g.Snapshot()
	if len(top.Nodes) != 2 {
		t.Fatalf("nodes = %d, want 2", len(top.Nodes))
	}
	if len(top.Edges) != 1 {
		t.Fatalf("edges = %d, want 1", len(top.Edges))
	}
	e := findEdge(t, top, "gateway", "orders")
	if e.Calls != 1 {
		t.Errorf("calls = %d, want 1", e.Calls)
	}
}

// TestTopologyRootSpanCreatesNoEdge — a root span has no caller inside the fleet,
// and inventing an edge from nothing would put a phantom dependency on the map.
func TestTopologyRootSpanCreatesNoEdge(t *testing.T) {
	g := NewTopologyGraph(Config{})
	g.Observe(topoSpan("gateway", "a", "", 200, 40), "", time.Now())

	top := g.Snapshot()
	if len(top.Edges) != 0 {
		t.Errorf("edges = %d, want 0", len(top.Edges))
	}
	if !findNode(t, top, "gateway").Entry {
		t.Error("a service handling root spans must be marked Entry for the layered layout")
	}
}

// TestTopologySkipsSelfEdges — real but pure noise as a loop on the map.
func TestTopologySkipsSelfEdges(t *testing.T) {
	g := NewTopologyGraph(Config{})
	now := time.Now()

	g.Observe(topoSpan("orders", "b", "a", 200, 5), "orders", now)

	if len(g.Snapshot().Edges) != 0 {
		t.Error("a service calling itself was rendered as an edge")
	}
}

func TestTopologyIgnoresUnnamedSpans(t *testing.T) {
	g := NewTopologyGraph(Config{})
	g.Observe(fleet.Span{SpanID: sid("a")}, "gateway", time.Now())

	nodes, edges := g.Stats()
	if nodes != 0 || edges != 0 {
		t.Errorf("nodes/edges = %d/%d, want 0/0", nodes, edges)
	}
}

// TestTopologyCallerBecomesNode guards against an edge pointing at a node the
// graph does not contain, which the UI would silently drop.
func TestTopologyCallerBecomesNode(t *testing.T) {
	g := NewTopologyGraph(Config{})
	// Only the callee ever reports; the caller is known by name alone.
	g.Observe(topoSpan("orders", "b", "a", 200, 5), "gateway", time.Now())

	top := g.Snapshot()
	if len(top.Nodes) != 2 {
		t.Fatalf("nodes = %d, want 2 (callee plus the never-reporting caller)", len(top.Nodes))
	}
	findNode(t, top, "gateway")
}

func TestTopologyAggregatesRepeatedCalls(t *testing.T) {
	g := NewTopologyGraph(Config{})
	now := time.Now()

	for i := 0; i < 100; i++ {
		status := 200
		if i < 10 {
			status = 500
		}
		g.Observe(topoSpan("orders", "b", "a", status, 10), "gateway", now)
	}

	e := findEdge(t, g.Snapshot(), "gateway", "orders")
	if e.Calls != 100 {
		t.Errorf("calls = %d, want 100", e.Calls)
	}
	if e.Errors != 10 {
		t.Errorf("errors = %d, want 10", e.Errors)
	}
	if e.ErrorRate != 0.1 {
		t.Errorf("error rate = %v, want 0.1", e.ErrorRate)
	}
	if e.AvgMs != 10 {
		t.Errorf("avg = %v, want 10", e.AvgMs)
	}
}

// TestTopologyCountsErrorFieldNotJustStatus — a span carrying an error string with
// a 200 status is still a failure, matching Span.Failed.
func TestTopologyCountsErrorFieldNotJustStatus(t *testing.T) {
	g := NewTopologyGraph(Config{})
	sp := topoSpan("orders", "b", "a", 200, 5)
	sp.Error = "context deadline exceeded"
	g.Observe(sp, "gateway", time.Now())

	if e := findEdge(t, g.Snapshot(), "gateway", "orders"); e.Errors != 1 {
		t.Errorf("errors = %d, want 1", e.Errors)
	}
}

// TestTopologyPercentilesReportBucketBounds pins the histogram's contract: a
// bucket edge, not an interpolated value, since the samples to interpolate from
// were never kept.
func TestTopologyPercentilesReportBucketBounds(t *testing.T) {
	g := NewTopologyGraph(Config{})
	now := time.Now()

	// 99 fast calls and one slow one: p50 lands in a low bucket, p99 in the
	// bucket containing 800ms.
	for i := 0; i < 99; i++ {
		g.Observe(topoSpan("orders", "b", "a", 200, 3), "gateway", now)
	}
	g.Observe(topoSpan("orders", "b", "a", 200, 800), "gateway", now)

	e := findEdge(t, g.Snapshot(), "gateway", "orders")
	if e.P50Ms != 5 {
		t.Errorf("p50 = %v, want the 5ms bucket bound", e.P50Ms)
	}
	if e.P99Ms != 5 {
		t.Errorf("p99 = %v, want 5ms: the 99th of 100 values is still a fast call", e.P99Ms)
	}
	// The outlier must be visible somewhere — that is the point of tracking
	// percentiles at all.
	if e.AvgMs < 10 {
		t.Errorf("avg = %v, want the 800ms outlier reflected", e.AvgMs)
	}
}

func TestTopologyPercentilesEmpty(t *testing.T) {
	var h latencyHistogram
	if got := h.quantile(0.99); got != 0 {
		t.Errorf("quantile of nothing = %v, want 0", got)
	}
}

// TestTopologyHistogramClampsNegativeDurations — a clock that steps backwards
// mid-request produces one, and it must be counted without corrupting the mean.
func TestTopologyHistogramClampsNegativeDurations(t *testing.T) {
	var h latencyHistogram
	h.observe(-50)

	if h.total != 1 {
		t.Errorf("total = %d, want the call counted", h.total)
	}
	if got := h.quantile(0.5); got != 1 {
		t.Errorf("quantile = %v, want the first bucket bound", got)
	}
}

// TestTopologyHistogramHandlesOverflowBucket — the last bound is +Inf, which JSON
// cannot encode and no UI can render, so the finite bound below it is reported.
func TestTopologyHistogramHandlesOverflowBucket(t *testing.T) {
	var h latencyHistogram
	h.observe(30 * 1000) // well past the last finite bound

	got := h.quantile(0.99)
	if got != 5000 {
		t.Errorf("quantile = %v, want the 5000ms bound rather than +Inf", got)
	}
}

func TestTopologyNodeErrorRateIsInbound(t *testing.T) {
	g := NewTopologyGraph(Config{})
	now := time.Now()

	// orders serves 10 requests, 5 failing.
	for i := 0; i < 10; i++ {
		status := 200
		if i < 5 {
			status = 503
		}
		g.Observe(topoSpan("orders", "b", "a", status, 5), "gateway", now)
	}

	n := findNode(t, g.Snapshot(), "orders")
	if n.ErrorRate != 0.5 {
		t.Errorf("error rate = %v, want 0.5", n.ErrorRate)
	}
	// The caller's own node should show no inbound failures of its own.
	if got := findNode(t, g.Snapshot(), "gateway"); got.Errors != 0 {
		t.Errorf("gateway errors = %d, want 0: node stats are inbound", got.Errors)
	}
}

func TestTopologyObserveTraceWalksTree(t *testing.T) {
	g := NewTopologyGraph(Config{})
	now := time.Now()

	tr := Assemble(tid("a"), []fleet.Span{
		span("a", "", "gateway", 0, 40, 200),
		span("b", "a", "orders", 5, 20, 200),
		span("c", "b", "payments", 10, 8, 500),
	})
	g.ObserveTrace(tr, now)

	top := g.Snapshot()
	if len(top.Edges) != 2 {
		t.Fatalf("edges = %d, want 2", len(top.Edges))
	}
	findEdge(t, top, "gateway", "orders")
	e := findEdge(t, top, "orders", "payments")
	if e.Errors != 1 {
		t.Errorf("payments edge errors = %d, want 1", e.Errors)
	}
}

// --- Traversal primitives --------------------------------------------------

func TestTopologyCalleesAndCallers(t *testing.T) {
	g := NewTopologyGraph(Config{})
	now := time.Now()

	g.Observe(topoSpan("orders", "b", "a", 200, 5), "gateway", now)
	g.Observe(topoSpan("payments", "c", "b", 200, 5), "orders", now)
	g.Observe(topoSpan("orders", "d", "e", 200, 5), "admin", now)

	if got := g.Callees("gateway", now); len(got) != 1 || got[0] != "orders" {
		t.Errorf("Callees(gateway) = %v, want [orders]", got)
	}
	callers := g.Callers("orders", now)
	if len(callers) != 2 || callers[0] != "admin" || callers[1] != "gateway" {
		t.Errorf("Callers(orders) = %v, want sorted [admin gateway]", callers)
	}
	if got := g.Callees("payments", now); len(got) != 0 {
		t.Errorf("Callees(payments) = %v, want none for a leaf", got)
	}
}

// TestTopologyTraversalIgnoresStaleEdges — a dependency dropped an hour ago must
// not appear in a blast-radius walk as if it were current.
func TestTopologyTraversalIgnoresStaleEdges(t *testing.T) {
	g := NewTopologyGraph(Config{TraceTTL: time.Minute})
	old := time.Now()

	g.Observe(topoSpan("orders", "b", "a", 200, 5), "gateway", old)

	future := old.Add(time.Hour)
	if got := g.Callees("gateway", future); len(got) != 0 {
		t.Errorf("Callees = %v, want none: the edge is far outside TraceTTL", got)
	}
	if got := g.Callers("orders", future); len(got) != 0 {
		t.Errorf("Callers = %v, want none", got)
	}
}

// --- Rolling window --------------------------------------------------------

func TestTopologyWindowedErrorRate(t *testing.T) {
	g := NewTopologyGraph(Config{BlastRadiusWindow: time.Minute})
	now := time.Now()

	for i := 0; i < 20; i++ {
		status := 200
		if i < 4 {
			status = 500
		}
		g.Observe(topoSpan("orders", "b", "a", status, 5), "gateway", now)
	}

	rate, calls := g.WindowedErrorRate("orders", now)
	if calls != 20 {
		t.Errorf("calls = %d, want 20", calls)
	}
	if rate != 0.2 {
		t.Errorf("rate = %v, want 0.2", rate)
	}
}

// TestTopologyWindowForgets is what makes the incident threshold react to *now*
// rather than to a lifetime average.
func TestTopologyWindowForgets(t *testing.T) {
	g := NewTopologyGraph(Config{BlastRadiusWindow: time.Minute})
	start := time.Now()

	for i := 0; i < 20; i++ {
		g.Observe(topoSpan("orders", "b", "a", 500, 5), "gateway", start)
	}
	if rate, _ := g.WindowedErrorRate("orders", start); rate != 1 {
		t.Fatalf("rate = %v, want 1", rate)
	}

	// Long after the window has rolled twice, nothing is remembered.
	quiet := start.Add(10 * time.Minute)
	rate, calls := g.WindowedErrorRate("orders", quiet)
	if calls != 0 || rate != 0 {
		t.Errorf("rate/calls = %v/%d, want 0/0 once the window has passed", rate, calls)
	}
}

func TestTopologyWindowedErrorRateUnknownService(t *testing.T) {
	g := NewTopologyGraph(Config{})
	if rate, calls := g.WindowedErrorRate("nope", time.Now()); rate != 0 || calls != 0 {
		t.Errorf("rate/calls = %v/%d, want 0/0", rate, calls)
	}
	if rate, calls := g.EdgeWindowedErrorRate("nope", "nada", time.Now()); rate != 0 || calls != 0 {
		t.Errorf("edge rate/calls = %v/%d, want 0/0", rate, calls)
	}
}

func TestTopologyEdgeWindowedErrorRate(t *testing.T) {
	g := NewTopologyGraph(Config{BlastRadiusWindow: time.Minute})
	now := time.Now()

	// gateway→orders fails; gateway→auth is fine. Per-edge rates must not be
	// pooled, or attribution in §9B.2 would blame the wrong dependency.
	for i := 0; i < 10; i++ {
		g.Observe(topoSpan("orders", "b", "a", 500, 5), "gateway", now)
		g.Observe(topoSpan("auth", "c", "a", 200, 2), "gateway", now)
	}

	if rate, _ := g.EdgeWindowedErrorRate("gateway", "orders", now); rate != 1 {
		t.Errorf("gateway→orders rate = %v, want 1", rate)
	}
	if rate, _ := g.EdgeWindowedErrorRate("gateway", "auth", now); rate != 0 {
		t.Errorf("gateway→auth rate = %v, want 0", rate)
	}
}

// TestTopologyWindowRollsForward covers the middle branch of roll: one window
// elapsed keeps the previous bucket, so the rate does not drop to zero the instant
// a window boundary is crossed.
func TestTopologyWindowRollsForward(t *testing.T) {
	g := NewTopologyGraph(Config{BlastRadiusWindow: time.Minute})
	start := time.Now()

	for i := 0; i < 10; i++ {
		g.Observe(topoSpan("orders", "b", "a", 500, 5), "gateway", start)
	}

	// Just past one window: the previous bucket still counts.
	rate, calls := g.WindowedErrorRate("orders", start.Add(70*time.Second))
	if calls != 10 || rate != 1 {
		t.Errorf("rate/calls = %v/%d, want 1/10 immediately after one roll", rate, calls)
	}
}

// --- Sweep -----------------------------------------------------------------

func TestTopologySweepEvictsStaleGraph(t *testing.T) {
	g := NewTopologyGraph(Config{TraceTTL: time.Minute})
	start := time.Now()

	g.Observe(topoSpan("orders", "b", "a", 200, 5), "gateway", start)

	if removed := g.Sweep(start.Add(time.Second)); removed != 0 {
		t.Errorf("Sweep removed %d fresh entries", removed)
	}
	// 2× TraceTTL past last observation.
	if removed := g.Sweep(start.Add(10 * time.Minute)); removed != 3 {
		t.Errorf("Sweep removed %d, want 3 (one edge, two nodes)", removed)
	}
	nodes, edges := g.Stats()
	if nodes != 0 || edges != 0 {
		t.Errorf("nodes/edges = %d/%d after sweep, want 0/0", nodes, edges)
	}
}

// TestTopologyStaysBoundedAcrossRedeploys is the storage-bounds property: a fleet
// redeployed with new service names forever must not accumulate them forever.
func TestTopologyStaysBoundedAcrossRedeploys(t *testing.T) {
	g := NewTopologyGraph(Config{TraceTTL: time.Minute})
	now := time.Now()

	for gen := 0; gen < 50; gen++ {
		caller := fmt.Sprintf("gateway-v%d", gen)
		callee := fmt.Sprintf("orders-v%d", gen)
		g.Observe(topoSpan(callee, "b", "a", 200, 5), caller, now)
		now = now.Add(5 * time.Minute)
		g.Sweep(now)
	}

	nodes, edges := g.Stats()
	// Only the newest generation should survive each sweep.
	if nodes > 4 || edges > 2 {
		t.Errorf("nodes/edges = %d/%d, want the graph bounded to recent generations", nodes, edges)
	}
}

// --- Concurrency -----------------------------------------------------------

// TestTopologyConcurrentObserveAndSnapshot is the real access pattern: ingestion
// writing while the dashboard reads. Meaningful under -race (§14.6).
func TestTopologyConcurrentObserveAndSnapshot(t *testing.T) {
	g := NewTopologyGraph(Config{})
	now := time.Now()

	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			callee := fmt.Sprintf("svc-%d", w%3)
			for i := 0; i < 200; i++ {
				g.Observe(topoSpan(callee, "b", "a", 200, float64(i%50)), "gateway", now)
			}
		}(w)
	}
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				_ = g.Snapshot()
				_ = g.Callees("gateway", now)
				_ = g.Callers("svc-0", now)
				_, _ = g.WindowedErrorRate("svc-0", now)
				_, _ = g.EdgeWindowedErrorRate("gateway", "svc-0", now)
			}
		}()
	}
	wg.Wait()

	nodes, edges := g.Stats()
	if nodes != 4 { // gateway + three callees
		t.Errorf("nodes = %d, want 4", nodes)
	}
	if edges != 3 {
		t.Errorf("edges = %d, want 3", edges)
	}
	total := uint64(0)
	for _, e := range g.Snapshot().Edges {
		total += e.Calls
	}
	if total != 8*200 {
		t.Errorf("total calls = %d, want %d — a lost update means a dropped lock", total, 8*200)
	}
}
