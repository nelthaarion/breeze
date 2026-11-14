package dashboard

// routeindex.go — which route served a request, and what to remember about it.
//
// # Why this is on the request path
//
// The middleware used to attribute a request to a route only when it captured a full
// record, which it did only for a request that was slow, failed with a 5xx, or arrived
// while someone had the dashboard open. Everything else was counted in one total and
// attributed to nothing. The consequence reached the agent: breeze_get_routes reported a
// route that had served ten thousand fast requests as never called, and a 404 or a
// validation failure left no trace at all, so "no client errors" and "client errors that
// were not kept" read the same.
//
// The capture policy now is to count every request against its route and status class,
// which needs the route on every request, and to keep full records of failures under a
// bounded per-route sample. That puts a route lookup on the path all requests take, so it
// is written not to allocate, and BenchmarkZZRouted* measures what it costs.
//
// # Why not ask the router
//
// The router resolves the route for every request, but it does not keep the answer where
// the middleware can reach it, and adding a field to Context for this would put a store on
// the path of every application that never installs a dashboard. A lookup of its own costs
// something only where the dashboard is enabled.
//
// It has to agree with the router, and does by following the router's rule: of all the
// routes that match, the one registered first wins. That is the rule router_trie.go states
// and it is not the same as "static beats parameter", so a table built on the other rule
// would attribute requests to the wrong route exactly where routes overlap. The test that
// holds the two together walks a table of overlapping routes and asks both.

import (
	"strings"
	"sync/atomic"
	"time"

	"github.com/nelthaarion/breeze/v2"
)

// The capture policy for failed requests.
//
// Every request is counted. A failure is also kept in full, for the requests page and for
// breeze_get_recent_errors, but only up to a budget per route and per window, so a flood of
// one failure (a client enumerating ids that do not exist, one in a retry loop, a broken
// deployment answering 500 on everything) cannot evict the records of everything else from
// the ring buffer or make the middleware allocate per request. What the budget drops is
// counted, and reported beside the exact counts, so a reader is told how much was not kept.
//
// A server error gets the larger budget because it is the more likely to be the thing being
// looked for, and a route that fails a few times a minute is under it and loses nothing.
// Slow requests, and every request while someone has the dashboard open, are kept in full
// regardless, as before.
const (
	captureWindow     = 10 * time.Second
	clientErrorBudget = 20
	serverErrorBudget = 100
)

// sampler admits at most a budget of events per window, without allocating or locking.
//
// It is approximate on purpose: two requests racing at a window boundary may both count
// against either window. What it guarantees is the bound, which is the point.
type sampler struct {
	windowStart atomic.Int64 // unix nanos of the first admission in the window
	admitted    atomic.Int32 // admissions in this window, including those over budget
	dropped     atomic.Int64 // over budget, for the lifetime of the collector
}

// admit reports whether an event at now (unix nanos) is kept.
func (s *sampler) admit(now int64, budget int32) bool {
	if start := s.windowStart.Load(); now-start >= int64(captureWindow) {
		if s.windowStart.CompareAndSwap(start, now) {
			s.admitted.Store(1)
			return true
		}
	}
	if s.admitted.Add(1) <= budget {
		return true
	}
	s.dropped.Add(1)
	return false
}

// routeEntry is one route as the request path sees it: its identity, the accumulator the
// counts go into, and the samplers that bound what is kept of its failures.
//
// acc is the same accumulator RouteStats reads and the storage persists, not a copy, so
// counting here is counting there.
type routeEntry struct {
	method  string
	pattern string
	order   int
	acc     *routeStatAccumulator
	client  sampler
	server  sampler
}

// record counts one finished request against the route.
func (e *routeEntry) record(status int, durUS, when int64) {
	a := e.acc
	a.requests.Add(1)
	a.totalDurUS.Add(durUS)
	for {
		cur := a.maxDurUS.Load()
		if durUS <= cur || a.maxDurUS.CompareAndSwap(cur, durUS) {
			break
		}
	}
	a.lastRequest.Store(when)
	switch {
	case status >= 500:
		a.errors.Add(1)
	case status >= 400:
		a.clientErrors.Add(1)
	}
}

// admitFailure reports whether a failed request is kept in full, by status class.
func (e *routeEntry) admitFailure(status int, now int64) bool {
	switch {
	case status >= 500:
		return e.server.admit(now, serverErrorBudget)
	case status >= 400:
		return e.client.admit(now, clientErrorBudget)
	}
	return false
}

// notRecorded is how many failures were counted and not kept in full.
func (e *routeEntry) notRecorded() int64 { return e.client.dropped.Load() + e.server.dropped.Load() }

// ── the index ────────────────────────────────────────────────────────────────

// maxIndexOrder is larger than any registration index.
const maxIndexOrder = int(^uint(0) >> 1)

// linearKeys is how many literal children a node scans before it builds a map too.
const linearKeys = 8

// ixNode is a node of one method's segment trie. See router_trie.go, which this follows:
// a literal segment descends into the child of that name, a :param into the single param
// child, and a trailing wildcard marks the node it is anchored on. min is the lowest
// registration index below the node, which lets the walk stop early once it holds a better
// match.
type ixNode struct {
	keys  []string
	kids  []*ixNode
	index map[string]*ixNode
	param *ixNode
	exact *routeEntry
	wild  *routeEntry
	min   int
}

func newIxNode() *ixNode { return &ixNode{min: maxIndexOrder} }

func (n *ixNode) child(seg string) *ixNode {
	if n.index != nil {
		return n.index[seg]
	}
	for i, k := range n.keys {
		if k == seg {
			return n.kids[i]
		}
	}
	return nil
}

func (n *ixNode) literalChild(seg string) *ixNode {
	if c := n.child(seg); c != nil {
		return c
	}
	c := newIxNode()
	n.keys = append(n.keys, seg)
	n.kids = append(n.kids, c)
	switch {
	case n.index != nil:
		n.index[seg] = c
	case len(n.keys) > linearKeys:
		n.index = make(map[string]*ixNode, 2*len(n.keys))
		for i, k := range n.keys {
			n.index[k] = n.kids[i]
		}
	}
	return c
}

func (n *ixNode) paramChild() *ixNode {
	if n.param == nil {
		n.param = newIxNode()
	}
	return n.param
}

// match walks segs[depth:] below n and returns the matching route with the lowest
// registration index, or best when nothing below n beats it.
func (n *ixNode) match(segs []string, depth int, best *routeEntry, bestOrder int) (*routeEntry, int) {
	if n.min >= bestOrder {
		return best, bestOrder
	}
	if w := n.wild; w != nil && w.order < bestOrder {
		best, bestOrder = w, w.order
	}
	if depth == len(segs) {
		if e := n.exact; e != nil && e.order < bestOrder {
			best, bestOrder = e, e.order
		}
		return best, bestOrder
	}
	if c := n.child(segs[depth]); c != nil {
		best, bestOrder = c.match(segs, depth+1, best, bestOrder)
	}
	if n.param != nil {
		best, bestOrder = n.param.match(segs, depth+1, best, bestOrder)
	}
	return best, bestOrder
}

// methodTable is the routes of one method.
type methodTable struct {
	method string
	// static answers a path that is exactly a route with no parameters in a single hash. It
	// holds only the routes the trie itself would pick for their own path, which is what
	// makes the shortcut safe: a static route that an earlier parameter route shadows is
	// left to the trie, which knows.
	static map[string]*routeEntry
	root   *ixNode
}

// routeIndex resolves (method, path) to the route that serves it.
type routeIndex struct {
	// count is how many routes the router had when this was built, so a reader on a cold
	// path can tell that it has grown.
	count   int
	methods []methodTable
}

func (ri *routeIndex) table(method string) *methodTable {
	for i := range ri.methods {
		if ri.methods[i].method == method {
			return &ri.methods[i]
		}
	}
	return nil
}

// find returns the route that serves the request, or nil when none does.
func (ri *routeIndex) find(method, path string) *routeEntry {
	mt := ri.table(method)
	if mt == nil || mt.root == nil {
		return nil
	}

	// The same normalisation the router applies: one leading and one trailing slash.
	if len(path) > 0 && path[0] == '/' {
		path = path[1:]
	}
	if len(path) > 0 && path[len(path)-1] == '/' {
		path = path[:len(path)-1]
	}
	if e := mt.static[path]; e != nil {
		return e
	}

	var buf [16]string
	segs := buf[:0]
	if path != "" {
		start := 0
		for i := 0; i <= len(path); i++ {
			if i == len(path) || path[i] == '/' {
				segs = append(segs, path[start:i])
				start = i + 1
			}
		}
	}
	e, _ := mt.root.match(segs, 0, nil, maxIndexOrder)
	return e
}

// buildRouteIndex builds the index over the router's current routes. The dashboard's own
// routes are left out: the middleware does not instrument them.
//
// Each entry's accumulator is the one already in the collector's table when there is one,
// so counts survive a rebuild and so do the counts restored from storage.
func (c *Collector) buildRouteIndex() *routeIndex {
	infos := c.router.RoutesInfo()
	ri := &routeIndex{count: len(infos)}

	base := strings.TrimSuffix(c.cfg.BasePath, "/")
	if base == "" {
		base = "/dashboard"
	}

	for order, info := range infos {
		pattern := info.Pattern()
		if pattern == base || strings.HasPrefix(pattern, base+"/") {
			continue
		}
		method := string(info.Method())

		entry := &routeEntry{method: method, pattern: pattern, order: order, acc: c.accumulatorFor(method, pattern)}

		mt := ri.table(method)
		if mt == nil {
			ri.methods = append(ri.methods, methodTable{method: method, root: newIxNode()})
			mt = &ri.methods[len(ri.methods)-1]
		}

		cur := mt.root
		if order < cur.min {
			cur.min = order
		}
		for _, seg := range info.Segments() {
			if len(seg) > 0 && seg[0] == ':' {
				cur = cur.paramChild()
			} else {
				cur = cur.literalChild(seg)
			}
			if order < cur.min {
				cur.min = order
			}
		}
		// A later route of the same shape ends on the same node and matches exactly what the
		// earlier one does, so it can never win. The router keeps the first.
		if info.HasWildcard() {
			if cur.wild == nil {
				cur.wild = entry
			}
		} else if cur.exact == nil {
			cur.exact = entry
		}
	}

	// The static shortcut, for the routes the trie would pick for their own path.
	for order, info := range infos {
		if info.HasWildcard() || info.ParamCount() > 0 {
			continue
		}
		pattern := info.Pattern()
		if pattern == base || strings.HasPrefix(pattern, base+"/") {
			continue
		}
		mt := ri.table(string(info.Method()))
		if mt == nil {
			continue
		}
		segs := info.Segments()
		if winner, _ := mt.root.match(segs, 0, nil, maxIndexOrder); winner != nil && winner.order == order {
			if mt.static == nil {
				mt.static = make(map[string]*routeEntry)
			}
			mt.static[strings.Join(segs, "/")] = winner
		}
	}
	return ri
}

// accumulatorFor returns the accumulator for a route, creating it when it is new.
func (c *Collector) accumulatorFor(method, pattern string) *routeStatAccumulator {
	key := method + " " + pattern
	c.routeStatsMu.Lock()
	defer c.routeStatsMu.Unlock()
	acc, ok := c.routeStats[key]
	if !ok {
		acc = &routeStatAccumulator{}
		c.routeStats[key] = acc
	}
	// An accumulator restored from storage has counts and no name.
	if acc.pattern == "" {
		acc.method, acc.pattern = method, pattern
	}
	return acc
}

// routeIndexNow returns the index, building it on first use.
//
// It is built at the first request rather than when the middleware is created, because the
// middleware is installed before the application's routes (that ordering is one of the
// idioms) and an index built then would be empty.
func (c *Collector) routeIndexNow() *routeIndex {
	if ri := c.routeIdx.Load(); ri != nil {
		return ri
	}
	return c.refreshRouteIndex(false)
}

// refreshRouteIndex rebuilds the index when there is none, or, with force or when the router
// has grown since, to pick up routes registered later. It is for cold paths: it allocates.
func (c *Collector) refreshRouteIndex(force bool) *routeIndex {
	c.routeIdxMu.Lock()
	defer c.routeIdxMu.Unlock()

	if c.router == nil {
		ri := &routeIndex{}
		c.routeIdx.Store(ri)
		return ri
	}
	if cur := c.routeIdx.Load(); cur != nil && !force && cur.count == len(c.router.RoutesInfo()) {
		return cur
	}
	ri := c.buildRouteIndex()
	c.routeIdx.Store(ri)
	return ri
}

// resolveRoute returns the entry a request is counted against: its route, or the shared
// unmatched entry when no route serves it.
//
// What reaches this is only what reaches a middleware. A request for a path no route serves is
// answered by the framework before any global middleware runs (router.findDispatch returns no
// chain for it), so a 404 for an unrouted path is not seen here, nor by the logger, nor by the
// rate limiter. What lands in the unmatched entry is the requests that do run the chain and match
// no route: a CORS preflight answered by the CORS middleware, the auto-served index page, and a
// route registered after the index was built until something refreshes it.
func (c *Collector) resolveRoute(method, path string) *routeEntry {
	if e := c.routeIndexNow().find(method, path); e != nil {
		return e
	}
	return c.unmatched
}

// newUnmatchedEntry is where requests that no route served are counted.
func newUnmatchedEntry() *routeEntry {
	return &routeEntry{acc: &routeStatAccumulator{}}
}

var _ = breeze.GET // the package is imported for RouteInfo's method type
