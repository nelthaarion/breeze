package aggregator

// The service graph (§8.1.5 `/fleet/api/topology`): which services call which,
// how often, how fast, and how often it fails.
//
// # Derived, never declared
//
// Nobody writes this graph down. It is inferred entirely from spans that already
// arrived — a span whose parent belongs to a different service *is* an observed
// edge. That means the topology always describes what is actually happening,
// including the call nobody documented and the dependency someone thought they
// deleted.
//
// # Incremental by requirement, not by preference
//
// §12.7 requires that adding a span never triggers a rescan of stored traces. So
// every number here is maintained as spans arrive: counts and error counts are
// running totals, and latency percentiles come from fixed-size histograms rather
// than retained samples. The whole graph is therefore O(edges) to read and O(1)
// to update, and its memory is bounded by the number of distinct service pairs —
// which is a property of the fleet's shape, not of its traffic volume.
//
// # Why histograms instead of kept samples
//
// p50/p95/p99 from retained samples means storing samples, which means either an
// unbounded slice or a reservoir with its own sampling error. A bucketed
// histogram gives bounded memory, O(1) insert, and an error bound that is known
// in advance (the bucket width at that latency). For "is this edge slow", knowing
// p99 is between 250ms and 500ms is as actionable as knowing it is 340ms.

import (
	"math"
	"sort"
	"sync"
	"time"

	"github.com/nelthaarion/breeze/fleet"
)

// latencyBounds are the upper edges, in milliseconds, of the histogram buckets.
//
// Roughly logarithmic, dense where service-call latencies actually cluster
// (1–100ms) and coarse past a second, where the only question left is "how bad".
// Fixed rather than configurable: a shared bucket layout is what lets two edges'
// percentiles be compared at all.
var latencyBounds = [...]float64{
	1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 2000, 5000, math.Inf(1),
}

// latencyHistogram counts observations per bucket.
type latencyHistogram struct {
	counts [len(latencyBounds)]uint64
	total  uint64
	sum    float64
}

func (h *latencyHistogram) observe(ms float64) {
	// A negative duration is physically impossible but arrives anyway when a
	// service's clock steps backwards mid-request. Clamping to the first
	// bucket keeps it counted (the call did happen) without letting it
	// corrupt the sum used for the mean.
	if ms < 0 {
		ms = 0
	}
	for i, upper := range latencyBounds {
		if ms <= upper {
			h.counts[i]++
			break
		}
	}
	h.total++
	h.sum += ms
}

// quantile returns the upper bound of the bucket containing the qth percentile.
//
// Reports a bucket edge rather than interpolating within the bucket: the data to
// interpolate from was never kept, so interpolation would be inventing precision.
// A number that is honestly coarse beats one that looks exact and isn't.
func (h *latencyHistogram) quantile(q float64) float64 {
	if h.total == 0 {
		return 0
	}
	target := uint64(math.Ceil(q * float64(h.total)))
	if target == 0 {
		target = 1
	}
	var cumulative uint64
	for i, count := range h.counts {
		cumulative += count
		if cumulative >= target {
			if math.IsInf(latencyBounds[i], 1) {
				// Everything past the last finite bound: report that
				// bound rather than +Inf, which JSON cannot encode
				// and no UI can render.
				if i > 0 {
					return latencyBounds[i-1]
				}
				return 0
			}
			return latencyBounds[i]
		}
	}
	return latencyBounds[len(latencyBounds)-2]
}

// Edge is one observed caller→callee relationship.
type Edge struct {
	Caller string `json:"caller"`
	Callee string `json:"callee"`

	Calls  uint64 `json:"calls"`
	Errors uint64 `json:"errors"`

	ErrorRate float64 `json:"error_rate"`
	P50Ms     float64 `json:"p50_ms"`
	P95Ms     float64 `json:"p95_ms"`
	P99Ms     float64 `json:"p99_ms"`
	AvgMs     float64 `json:"avg_ms"`

	LastSeenUnix int64 `json:"last_seen_unix"`
}

// Node is one service in the graph.
type Node struct {
	Service string `json:"service"`

	// Calls/Errors are inbound: how much traffic this service *received*.
	// Inbound rather than outbound because the question the graph answers is
	// "is this service healthy", and a service's own error rate is a property
	// of the requests it served.
	Calls     uint64  `json:"calls"`
	Errors    uint64  `json:"errors"`
	ErrorRate float64 `json:"error_rate"`
	P99Ms     float64 `json:"p99_ms"`

	// Entry marks a service observed handling root spans — the fleet's edge,
	// where requests come in. Used by the UI's layered layout (§9.2) to put
	// gateways on the left instead of guessing from the graph shape.
	Entry bool `json:"entry,omitempty"`

	LastSeenUnix int64 `json:"last_seen_unix"`
}

// Topology is the full graph snapshot returned by GET /fleet/api/topology.
type Topology struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

// edgeStats is the mutable per-edge accumulator.
type edgeStats struct {
	caller, callee string
	calls          uint64
	errors         uint64
	hist           latencyHistogram
	lastSeen       time.Time

	// window holds the same counters over the rolling blast-radius window,
	// rotated by rollWindow. Kept separate from the lifetime totals because
	// §9B.2 asks "is this service failing *now*", and a lifetime error rate
	// on a long-lived edge would take hours to react to an incident that
	// started a minute ago.
	window edgeWindow
}

// edgeWindow is a two-bucket rolling counter.
//
// Two buckets, not a ring of many: the question is only "roughly the last
// minute", and a current/previous pair answers it with two integers and no
// per-observation bookkeeping. The cost is that the effective window drifts
// between 1x and 2x BlastRadiusWindow, which is documented and acceptable for a
// threshold whose whole purpose is spotting a step change.
type edgeWindow struct {
	curStart time.Time
	cur      windowCounts
	prev     windowCounts
}

type windowCounts struct {
	calls  uint64
	errors uint64
}

func (w *edgeWindow) roll(now time.Time, size time.Duration) {
	if w.curStart.IsZero() {
		w.curStart = now
		return
	}
	elapsed := now.Sub(w.curStart)
	switch {
	case elapsed < size:
		return
	case elapsed < 2*size:
		w.prev = w.cur
		w.cur = windowCounts{}
		w.curStart = now
	default:
		// Idle longer than two windows: everything remembered is stale,
		// so an edge that stopped being called does not keep reporting the
		// error rate it had when it stopped.
		w.prev = windowCounts{}
		w.cur = windowCounts{}
		w.curStart = now
	}
}

// rate returns the windowed error rate and call count.
func (w *edgeWindow) rate() (float64, uint64) {
	calls := w.cur.calls + w.prev.calls
	if calls == 0 {
		return 0, 0
	}
	return float64(w.cur.errors+w.prev.errors) / float64(calls), calls
}

// nodeStats accumulates inbound traffic per service.
type nodeStats struct {
	calls    uint64
	errors   uint64
	hist     latencyHistogram
	entry    bool
	lastSeen time.Time
	window   edgeWindow
}

// TopologyGraph maintains the service graph incrementally.
type TopologyGraph struct {
	cfg Config

	mu       sync.RWMutex
	edges    map[edgeKey]*edgeStats
	nodes    map[string]*nodeStats
	edgeSeen map[string]time.Time
}

type edgeKey struct{ caller, callee string }

func NewTopologyGraph(cfg Config) *TopologyGraph {
	return &TopologyGraph{
		cfg:      cfg.withDefaults(),
		edges:    make(map[edgeKey]*edgeStats),
		nodes:    make(map[string]*nodeStats),
		edgeSeen: make(map[string]time.Time),
	}
}

// Observe records one span's contribution to the graph.
//
// Takes the span plus its parent's service, which the caller resolves — the graph
// deliberately does not hold traces or look parents up itself. Keeping it
// dependency-free is what makes it O(1) per span and testable without a store.
//
// An empty parentService means the parent was not (yet) known: either a genuine
// root span, or a parent that has not arrived. Both are recorded as node traffic
// with no edge, since inventing an edge from an unknown caller would put a
// phantom dependency on the map.
func (g *TopologyGraph) Observe(span fleet.Span, parentService string, now time.Time) {
	if span.Service == "" {
		return
	}
	failed := span.Failed()
	window := g.cfg.BlastRadiusWindow

	g.mu.Lock()
	defer g.mu.Unlock()

	n, ok := g.nodes[span.Service]
	if !ok {
		n = &nodeStats{}
		g.nodes[span.Service] = n
	}
	n.calls++
	if failed {
		n.errors++
	}
	n.hist.observe(span.DurationMs)
	n.lastSeen = now
	if span.IsRoot() {
		n.entry = true
	}
	n.window.roll(now, window)
	n.window.cur.calls++
	if failed {
		n.window.cur.errors++
	}

	// Self-edges are dropped. A service calling itself (a retry, an internal
	// sub-request) is real, but rendering it as a loop on the topology map
	// adds noise without answering anything the per-node stats don't.
	if parentService == "" || parentService == span.Service {
		return
	}

	key := edgeKey{caller: parentService, callee: span.Service}
	e, ok := g.edges[key]
	if !ok {
		e = &edgeStats{caller: parentService, callee: span.Service}
		g.edges[key] = e
	}
	e.calls++
	if failed {
		e.errors++
	}
	e.hist.observe(span.DurationMs)
	e.lastSeen = now
	e.window.roll(now, window)
	e.window.cur.calls++
	if failed {
		e.window.cur.errors++
	}

	// The caller must exist as a node even if it never reported a span of its
	// own — otherwise an edge would point at a node the graph doesn't
	// contain, and the UI would drop it.
	if _, ok := g.nodes[parentService]; !ok {
		g.nodes[parentService] = &nodeStats{lastSeen: now}
	}
}

func (g *TopologyGraph) MarkEdgeSeen(span fleet.Span, parentService string, now time.Time) {
	if span.TraceID == "" || span.SpanID == "" || parentService == "" || parentService == span.Service {
		return
	}
	g.mu.Lock()
	g.edgeSeen[span.TraceID+"\x00"+span.SpanID] = now
	g.mu.Unlock()
}

// ReconcileTrace adds only edges whose parent arrived after the child. Node
// traffic was already counted on the child's original ingestion.
func (g *TopologyGraph) ReconcileTrace(tr Trace, now time.Time) {
	for _, root := range tr.Roots {
		walk(root, func(parent, child *SpanNode) {
			if parent == nil || parent.Service == child.Service {
				return
			}
			identity := child.TraceID + "\x00" + child.SpanID
			g.mu.Lock()
			if _, ok := g.edgeSeen[identity]; ok {
				g.mu.Unlock()
				return
			}
			key := edgeKey{caller: parent.Service, callee: child.Service}
			e := g.edges[key]
			if e == nil {
				e = &edgeStats{caller: parent.Service, callee: child.Service}
				g.edges[key] = e
			}
			e.calls++
			failed := child.Failed()
			if failed {
				e.errors++
			}
			e.hist.observe(child.DurationMs)
			e.lastSeen = now
			e.window.roll(now, g.cfg.BlastRadiusWindow)
			e.window.cur.calls++
			if failed {
				e.window.cur.errors++
			}
			g.edgeSeen[identity] = now
			g.mu.Unlock()
		})
	}
}

// ObserveTrace records every edge in an assembled trace.
//
// The convenience path for callers that have a tree rather than loose spans: it
// resolves each span's parent service from the tree, which is the one piece of
// context Observe cannot determine on its own.
func (g *TopologyGraph) ObserveTrace(tr Trace, now time.Time) {
	for _, root := range tr.Roots {
		walk(root, func(parent, child *SpanNode) {
			var parentService string
			if parent != nil {
				parentService = parent.Service
			}
			g.Observe(child.Span, parentService, now)
		})
	}
}

// Snapshot returns the graph, sorted deterministically.
//
// Sorted for the same reason the registry sorts: a graph that reorders between
// polls makes a force-directed layout jump around on screen, and a table of edges
// that shuffles is unreadable.
func (g *TopologyGraph) Snapshot() Topology {
	g.mu.RLock()
	defer g.mu.RUnlock()

	top := Topology{
		Nodes: make([]Node, 0, len(g.nodes)),
		Edges: make([]Edge, 0, len(g.edges)),
	}
	for name, n := range g.nodes {
		node := Node{
			Service: name,
			Calls:   n.calls,
			Errors:  n.errors,
			Entry:   n.entry,
			P99Ms:   n.hist.quantile(0.99),
		}
		if n.calls > 0 {
			node.ErrorRate = float64(n.errors) / float64(n.calls)
		}
		if !n.lastSeen.IsZero() {
			node.LastSeenUnix = n.lastSeen.Unix()
		}
		top.Nodes = append(top.Nodes, node)
	}
	for _, e := range g.edges {
		edge := Edge{
			Caller: e.caller,
			Callee: e.callee,
			Calls:  e.calls,
			Errors: e.errors,
			P50Ms:  e.hist.quantile(0.50),
			P95Ms:  e.hist.quantile(0.95),
			P99Ms:  e.hist.quantile(0.99),
		}
		if e.calls > 0 {
			edge.ErrorRate = float64(e.errors) / float64(e.calls)
		}
		if e.hist.total > 0 {
			edge.AvgMs = e.hist.sum / float64(e.hist.total)
		}
		if !e.lastSeen.IsZero() {
			edge.LastSeenUnix = e.lastSeen.Unix()
		}
		top.Edges = append(top.Edges, edge)
	}

	sort.Slice(top.Nodes, func(i, j int) bool { return top.Nodes[i].Service < top.Nodes[j].Service })
	sort.Slice(top.Edges, func(i, j int) bool {
		if top.Edges[i].Caller != top.Edges[j].Caller {
			return top.Edges[i].Caller < top.Edges[j].Caller
		}
		return top.Edges[i].Callee < top.Edges[j].Callee
	})
	return top
}

// Callees returns the services a given service was observed calling.
//
// The primitive blast-radius BFS walks (§9B.2). Returns only edges seen within
// TraceTTL, so the traversal follows current dependencies rather than one a
// service dropped an hour ago.
func (g *TopologyGraph) Callees(service string, now time.Time) []string {
	cutoff := now.Add(-g.cfg.TraceTTL)

	g.mu.RLock()
	defer g.mu.RUnlock()

	var out []string
	for key, e := range g.edges {
		if key.caller != service {
			continue
		}
		if e.lastSeen.Before(cutoff) {
			continue
		}
		out = append(out, key.callee)
	}
	sort.Strings(out)
	return out
}

// Callers is Callees' inverse: who calls this service.
func (g *TopologyGraph) Callers(service string, now time.Time) []string {
	cutoff := now.Add(-g.cfg.TraceTTL)

	g.mu.RLock()
	defer g.mu.RUnlock()

	var out []string
	for key, e := range g.edges {
		if key.callee != service {
			continue
		}
		if e.lastSeen.Before(cutoff) {
			continue
		}
		out = append(out, key.caller)
	}
	sort.Strings(out)
	return out
}

// WindowedErrorRate returns a service's error rate over the rolling window, plus
// the call count it was computed from.
//
// The call count is returned alongside deliberately: a 100% error rate over two
// calls is noise, and §9B.2 needs to be able to tell that apart from a 12% rate
// over ten thousand. A threshold applied without the denominator would declare an
// incident every time an idle service saw one failed health check.
func (g *TopologyGraph) WindowedErrorRate(service string, now time.Time) (float64, uint64) {
	g.mu.Lock() // roll mutates, so this cannot be a read lock
	defer g.mu.Unlock()

	n, ok := g.nodes[service]
	if !ok {
		return 0, 0
	}
	n.window.roll(now, g.cfg.BlastRadiusWindow)
	return n.window.rate()
}

// EdgeWindowedErrorRate is WindowedErrorRate for one caller→callee pair.
func (g *TopologyGraph) EdgeWindowedErrorRate(caller, callee string, now time.Time) (float64, uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()

	e, ok := g.edges[edgeKey{caller: caller, callee: callee}]
	if !ok {
		return 0, 0
	}
	e.window.roll(now, g.cfg.BlastRadiusWindow)
	return e.window.rate()
}

// Sweep forgets edges and nodes not observed within TraceTTL.
//
// Bounded storage requires this: a fleet that is redeployed daily accumulates a
// new set of service names every time, and without eviction the graph would grow
// forever with names nothing calls anymore.
func (g *TopologyGraph) Sweep(now time.Time) int {
	// Generous relative to TraceTTL: an edge is dropped only once it is well
	// past being traversable, since deleting it removes a dependency from the
	// map that a reader may still be looking at.
	cutoff := now.Add(-2 * g.cfg.TraceTTL)

	g.mu.Lock()
	defer g.mu.Unlock()

	removed := 0
	for key, e := range g.edges {
		if e.lastSeen.Before(cutoff) {
			delete(g.edges, key)
			removed++
		}
	}
	for name, n := range g.nodes {
		if n.lastSeen.Before(cutoff) {
			delete(g.nodes, name)
			removed++
		}
	}
	seenCutoff := now.Add(-g.cfg.TraceTTL)
	for identity, seenAt := range g.edgeSeen {
		if seenAt.Before(seenCutoff) {
			delete(g.edgeSeen, identity)
		}
	}
	return removed
}

// Stats reports graph size, for the storage-bounds story.
func (g *TopologyGraph) Stats() (nodes, edges int) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.nodes), len(g.edges)
}
