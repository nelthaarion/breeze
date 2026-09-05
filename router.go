package breeze

import (
	"os"
	"path/filepath"
	"strings"
)

// HandlerFunc is a route handler or middleware.
//
// # Why it returns an error
//
// It used to return nothing, which made "this request failed" unrepresentable.
// Every handler had to decide for itself what a failure looked like on the wire,
// so a nil map lookup became a 200 with an empty body in one place and a 500 in
// another, and a middleware could not stop a chain without also inventing the
// response. Returning an error moves that decision to one place: the handler says
// what went wrong, and the framework's error path decides what the client sees.
//
// A non-nil error from any link in the chain — middleware or handler — stops the
// chain and is handed to Breeze.ErrorHandler. It always produces a real response;
// see errorHandler in error.go for why silence is not an option.
//
// Returning nil after writing a response is the normal case. Returning nil
// without writing anything is also legal and yields a 404-shaped empty response,
// exactly as before.
type HandlerFunc func(*Context) error

type route struct {
	method   Method
	pattern  string
	segments []string
	// paramIndex[i] is true when segments[i] is a :param (cached at registration)
	paramIndex []bool
	handler    HandlerFunc // finalHandler closure (for backward compat with public Find)
	// userHandler is the actual handler passed to Handle, without the
	// finalHandler wrapper. Used by findRoute to build the chain in one alloc.
	userHandler HandlerFunc
	// routeMWs is the per-route middleware slice (defensive copy made at
	// registration). Used by findRoute to build the full middleware chain
	// in a single allocation in OnTraffic, eliminating the double-build
	// that finalHandler caused.
	routeMWs     []HandlerFunc
	hasWildcard  bool
	wildcardName string
	// paramCount is the number of :param segments (pre-counted to pre-size the map)
	paramCount int
	// chain is the fully-resolved middleware chain for this route:
	//   [global_mw..., route_mw..., userHandler]
	// It is precomputed at registration (Handle) and rebuilt whenever a
	// global middleware is added (Use), so OnTraffic can assign it to the
	// Context with ZERO per-request allocation. The slice is read-only at
	// request time — Context.Next only mutates its own index, never the
	// backing array — so it is safe to share across concurrent requests.
	chain []HandlerFunc
	// blocking marks a route whose chain may block: file or network I/O, a
	// mutex held across a syscall, anything that is not pure CPU work on
	// in-memory data.
	//
	// Non-blocking routes run inline on the gnet event-loop goroutine, which
	// is where nearly all of the throughput win lives (see breeze.go). A
	// blocking route must not do that — one slow read would stall every other
	// connection pinned to the same reactor — so it is handed to the worker
	// pool instead. Set via HandleBlocking.
	blocking bool
}

// bucket indices for the per-method route index. Requests and routes are
// grouped by method so a lookup never scans routes that cannot match, and
// (for the six known methods) never compares the method string at all.
const (
	bucketGET = iota
	bucketPOST
	bucketPUT
	bucketPATCH
	bucketDELETE
	bucketOPTIONS
	// bucketOther holds every method outside the six above (HEAD, TRACE,
	// CONNECT, anything custom). Routes from different methods share it, so
	// lookups in this bucket still compare rt.method against req.Method.
	bucketOther
	nBuckets
)

// methodBucket maps a method to its bucket index.
//
// The switch dispatches on the first byte before comparing the whole string,
// so the common case is one byte load plus one short comparison. Because
// internMethod hands back the package-level Method constants, both sides of
// that comparison are usually the same string header and the runtime settles
// it on the data pointer without touching the bytes.
func methodBucket(m Method) int {
	if len(m) == 0 {
		return bucketOther
	}
	switch m[0] {
	case 'G':
		if m == GET {
			return bucketGET
		}
	case 'P':
		switch len(m) {
		case 3:
			if m == PUT {
				return bucketPUT
			}
		case 4:
			if m == POST {
				return bucketPOST
			}
		case 5:
			if m == PATCH {
				return bucketPATCH
			}
		}
	case 'D':
		if m == DELETE {
			return bucketDELETE
		}
	case 'O':
		if m == OPTIONS {
			return bucketOPTIONS
		}
	}
	return bucketOther
}

// methodIndex holds the routes registered for one method bucket.
type methodIndex struct {
	// routes are this bucket's routes in registration order. Matching walks
	// them in that order, so first-registered still wins.
	routes []*route

	// static maps a normalized path ("users/active", "" for "/") to the route
	// registered for it, letting an exact match skip both the scan and the
	// path split.
	//
	// A static route is eligible unless some route registered BEFORE it in this
	// bucket could also match its path. That is the exact condition the map has
	// to respect: matching is first-registered-wins, so answering from the map
	// is only correct when the ordered scan would have reached the same route.
	//
	// The earlier version approximated this by closing the map as soon as any
	// dynamic route joined the bucket. It was safe but far too blunt — one
	// ServeStatic call registers a wildcard, and from then on every static
	// route in that bucket fell back to the scan, path split and all. Since a
	// dynamic route can only shadow a *later* static one, the precise test is
	// to check the routes already in the bucket, which costs nothing at request
	// time and only O(routes²) once at startup.
	static map[string]*route

	// dynamic holds this bucket's :param and wildcard routes in registration
	// order — the only routes that can shadow a static path. Kept separate so
	// eligibility testing does not walk the whole bucket.
	dynamic []*route
}

type Router struct {
	routes        []*route
	middlewares   []HandlerFunc
	staticDir     string
	autoServeRoot bool
	// mcpTools holds the routes tagged with MCPTool, in registration order.
	// It is the only place Auto-MCP learns which routes are exposed, so an
	// untagged route cannot appear as a tool by any path.
	mcpTools []mcpRoute

	// byMethod is the per-method route index described above. r.routes stays
	// the authoritative registration-ordered list for Routes()/RoutesInfo().
	byMethod [nBuckets]methodIndex

	// autoIndexChain is the single-handler chain used to serve
	// <staticDir>/index.html for GET "/" when no route matched.
	//
	// It is built once, at construction. The previous version stat'd the file
	// and allocated both a closure and a one-element slice inside the lookup,
	// meaning every unmatched GET "/" paid a syscall on the event-loop
	// goroutine. Now the handler does its own I/O — off the event loop, since
	// the chain is dispatched as blocking work — and reports 404 when the file
	// is missing, which is what the lookup used to fall through to anyway.
	autoIndexChain []HandlerFunc

	// staticMounts records what ServeStatic was called with, for the "static"
	// diagnostic probe.
	//
	// The routes themselves are already in r.routes, but a wildcard route gives
	// no way to recover the directory behind it — and "which directory is this
	// serving from" is the whole question when a static mount returns 404 for a
	// file the developer can see on disk. Appended at registration only.
	staticMounts []staticMount
}

// staticMount is one ServeStatic call, kept for diagnostics.
type staticMount struct {
	prefix string
	root   string
}

func NewRouter() *Router {
	r := &Router{
		staticDir:     "./public",
		autoServeRoot: true,
	}
	r.autoIndexChain = []HandlerFunc{func(ctx *Context) error {
		// staticDir is read here rather than captured so SetStaticDir keeps
		// working after construction.
		data, err := os.ReadFile(filepath.Join(r.staticDir, "index.html"))
		if err != nil {
			ctx.Status(404)
			return ctx.WriteString("Not Found")
		}
		return ctx.HTML(data)
	}}
	return r
}

// RouteInfo exposes read-only information about a registered route so
// external packages (e.g. the dashboard) can inspect the routing table
// without depending on the unexported `route` struct.
type RouteInfo interface {
	Method() Method
	Pattern() string
	Segments() []string
	HasWildcard() bool
	WildcardName() string
	ParamCount() int
}

func (r *route) Method() Method       { return r.method }
func (r *route) Pattern() string      { return r.pattern }
func (r *route) Segments() []string   { return r.segments }
func (r *route) HasWildcard() bool    { return r.hasWildcard }
func (r *route) WildcardName() string { return r.wildcardName }
func (r *route) ParamCount() int      { return r.paramCount }

// RoutesInfo returns the routing table as a slice of RouteInfo so external
// packages can iterate without depending on the unexportd *route type.
func (r *Router) RoutesInfo() []RouteInfo {
	out := make([]RouteInfo, len(r.routes))
	for i, rt := range r.routes {
		out[i] = rt
	}
	return out
}

func (r *Router) Use(mw ...HandlerFunc) {
	r.middlewares = append(r.middlewares, mw...)
	// Global middlewares changed — rebuild every route's precomputed chain
	// so requests continue to see [global..., route..., handler]. This runs
	// only at setup time, never on the request path.
	//
	// The method index stores *route pointers, so the rebuilt chains are
	// visible through it without touching the index itself.
	for _, rt := range r.routes {
		rt.chain = r.buildChain(rt.routeMWs, rt.userHandler)
	}
}

// buildChain assembles the full, flat middleware chain for a route:
//
//	[global_mw..., route_mw..., handler]
//
// The result is stored on route.chain at registration time (and rebuilt by
// Use), so OnTraffic can hand it to the Context without any per-request
// slice allocation.
func (r *Router) buildChain(routeMWs []HandlerFunc, handler HandlerFunc) []HandlerFunc {
	chain := make([]HandlerFunc, 0, len(r.middlewares)+len(routeMWs)+1)
	chain = append(chain, r.middlewares...)
	chain = append(chain, routeMWs...)
	chain = append(chain, handler)
	return chain
}

func (r *Router) Routes() []*route {
	return r.routes
}

func (r *Router) SetStaticDir(dir string) {
	r.staticDir = dir
}

func (r *Router) Handle(method Method, pattern string, handler HandlerFunc, middlewares ...HandlerFunc) {
	if pattern == "" || pattern[0] != '/' {
		panic("invalid route pattern: must start with '/'")
	}

	trimmed := strings.Trim(pattern, "/")
	var segments []string
	hasWildcard := false
	wildcardName := ""

	if trimmed == "" {
		segments = []string{}
	} else {
		segments = strings.Split(trimmed, "/")
		last := segments[len(segments)-1]

		if strings.HasPrefix(last, "*") {
			hasWildcard = true
			if len(last) > 1 {
				wildcardName = last[1:]
			} else {
				wildcardName = "wildcard"
			}
			segments = segments[:len(segments)-1]
		}
	}

	// Pre-compute which segments are params and how many there are.
	paramIdx := make([]bool, len(segments))
	paramCount := 0
	for i, s := range segments {
		if len(s) > 0 && s[0] == ':' {
			paramIdx[i] = true
			paramCount++
		}
	}

	// Pull out any MCP tool tag before the middleware slice is copied.
	//
	// A tag declares what the route is; it is not a step in the chain. Leaving
	// it in would put a no-op call in front of every request to this route,
	// and would mean the chain an MCP call runs differs from the chain an HTTP
	// call runs — the one property Auto-MCP most needs to hold.
	//
	// The filtered slice is built with a zero-capacity reslice so appending
	// cannot write into the caller's backing array.
	var mcpSpec *mcpToolSpec
	if len(middlewares) > 0 {
		kept := middlewares[:0:0]
		for _, mw := range middlewares {
			if spec := mcpSpecOf(mw); spec != nil {
				mcpSpec = spec
				continue
			}
			kept = append(kept, mw)
		}
		middlewares = kept
	}

	// Capture per-route middlewares for the final handler closure.
	// We copy the slice so the caller can't mutate it after registration.
	var routeMWs []HandlerFunc
	if len(middlewares) > 0 {
		routeMWs = make([]HandlerFunc, len(middlewares))
		copy(routeMWs, middlewares)
	}

	// finalHandler is kept for backward compatibility with the public
	// Find method. OnTraffic uses findRoute (which returns the actual
	// handler + routeMWs) to build the chain in one allocation, avoiding
	// the double-build that finalHandler causes.
	finalHandler := func(ctx *Context) error {
		ctx.middlewares = append(routeMWs, handler)
		ctx.index = -1
		return ctx.Next()
	}

	rt := &route{
		method:       method,
		pattern:      pattern,
		segments:     segments,
		paramIndex:   paramIdx,
		handler:      finalHandler,
		userHandler:  handler,
		routeMWs:     routeMWs,
		hasWildcard:  hasWildcard,
		wildcardName: wildcardName,
		paramCount:   paramCount,
		// Precompute the flat [global..., route..., handler] chain so the
		// request path (OnTraffic → findChain) never allocates a chain slice.
		chain: r.buildChain(routeMWs, handler),
	}

	r.routes = append(r.routes, rt)
	r.indexRoute(rt)

	// Recorded after the route is built so the tool holds the same *route the
	// index holds, and therefore the same chain — rebuilt in place by Use.
	if mcpSpec != nil {
		r.mcpTools = append(r.mcpTools, mcpRoute{spec: mcpSpec, rt: rt})
	}
}

// HandleBlocking registers a route whose chain performs blocking work — file
// or network I/O, a database round trip, a lock held across a syscall.
//
// Such a route is dispatched to the worker pool instead of running inline on
// the event-loop goroutine. Use it for anything that is not pure CPU work on
// data already in memory; running a blocking handler inline stalls every
// connection pinned to the same reactor for as long as it takes.
func (r *Router) HandleBlocking(method Method, pattern string, handler HandlerFunc, middlewares ...HandlerFunc) {
	r.Handle(method, pattern, handler, middlewares...)
	// The index holds the same *route pointer, so setting the flag here is
	// visible to every lookup path.
	r.routes[len(r.routes)-1].blocking = true
}

// indexRoute files rt into its method bucket and, when eligible, the bucket's
// exact-path map. See methodIndex for what eligibility means.
func (r *Router) indexRoute(rt *route) {
	b := methodBucket(rt.method)
	idx := &r.byMethod[b]
	idx.routes = append(idx.routes, rt)

	if rt.hasWildcard || rt.paramCount > 0 {
		// A dynamic route can only shadow static routes registered after it,
		// so nothing already in the map is invalidated by adding it here.
		idx.dynamic = append(idx.dynamic, rt)
		return
	}
	// bucketOther mixes methods, so a path alone does not identify a route
	// there. Those buckets always take the ordered scan.
	if b == bucketOther {
		return
	}
	// Any earlier dynamic route that also matches this path would win the
	// ordered scan, so the map must not answer for it.
	for _, dyn := range idx.dynamic {
		if dyn.matchesSegments(rt.segments) {
			return
		}
	}
	key := strings.Join(rt.segments, "/")
	if idx.static == nil {
		idx.static = make(map[string]*route, 8)
	}
	if _, exists := idx.static[key]; !exists {
		idx.static[key] = rt
	}
}

// matchesSegments reports whether r would match a request path split into
// segs. It is the registration-time counterpart of the matching loop in lookup
// and must agree with it: a disagreement would either put a shadowed route in
// the exact-path map (wrong route served) or keep an unshadowed one out of it
// (slower, but correct).
//
// Params are treated as matching anything, which is what makes this a shadow
// test rather than an equality test.
func (r *route) matchesSegments(segs []string) bool {
	if r.hasWildcard {
		// A wildcard route matches any path at least as long as its prefix.
		if len(segs) < len(r.segments) {
			return false
		}
	} else if len(r.segments) != len(segs) {
		return false
	}
	for i, rseg := range r.segments {
		if !r.paramIndex[i] && rseg != segs[i] {
			return false
		}
	}
	return true
}

// lookup resolves req to a registered route.
//
// It is the single matching implementation behind Find and findChain. Keeping
// the path-splitting scratch array local to this function is deliberate: the
// segments slice points into it, so handing the array to a helper would force
// it onto the heap and put an allocation back on the request path.
//
// Returns the matched route, its captured params (nil when the route has
// none), and autoIndex — true when nothing matched but the request is a GET
// for "/" and root auto-serving is on.
func (r *Router) lookup(req *HTTPRequest) (matched *route, params map[string]string, autoIndex bool) {
	path := req.Path
	if len(path) > 0 && path[0] == '/' {
		path = path[1:]
	}
	if len(path) > 0 && path[len(path)-1] == '/' {
		path = path[:len(path)-1]
	}

	b := methodBucket(req.Method)
	idx := &r.byMethod[b]

	// Exact-path fast path: one map probe, and the path is never split.
	if idx.static != nil {
		if rt := idx.static[path]; rt != nil {
			return rt, nil, false
		}
	}

	// Split the request path into segments without allocating a []string.
	// We store them in a small stack-allocated array for the common case.
	var segBuf [16]string
	reqSegments := segBuf[:0]

	if path != "" {
		start := 0
		for i := 0; i <= len(path); i++ {
			if i == len(path) || path[i] == '/' {
				seg := path[start:i]
				if len(reqSegments) < len(segBuf) {
					reqSegments = reqSegments[:len(reqSegments)+1]
					reqSegments[len(reqSegments)-1] = seg
				} else {
					// Overflow: fall back to heap allocation (paths with >16 segments)
					reqSegments = append(reqSegments, seg)
				}
				start = i + 1
			}
		}
	}

	nReq := len(reqSegments)

	// Within buckets 0..5 every route shares the request's method, so the
	// method comparison is skipped entirely. bucketOther mixes methods and
	// still needs it.
	checkMethod := b == bucketOther

	for _, rt := range idx.routes {
		if checkMethod && rt.method != req.Method {
			continue
		}

		if rt.hasWildcard {
			if nReq < len(rt.segments) {
				continue
			}
			match := true
			for i, rseg := range rt.segments {
				if rt.paramIndex[i] {
					// param: always matches, captured below
				} else if rseg != reqSegments[i] {
					match = false
					break
				}
			}
			if !match {
				continue
			}
			if rt.paramCount > 0 || rt.wildcardName != "" {
				params = acquireParams()
				for i, rseg := range rt.segments {
					if rt.paramIndex[i] {
						params[rseg[1:]] = reqSegments[i]
					}
				}
				params[rt.wildcardName] = strings.Join(reqSegments[len(rt.segments):], "/")
			}
			return rt, params, false
		}

		// Normal route: segment count must match exactly.
		if len(rt.segments) != nReq {
			continue
		}

		match := true
		for i, rseg := range rt.segments {
			if !rt.paramIndex[i] && rseg != reqSegments[i] {
				match = false
				break
			}
		}
		if !match {
			continue
		}

		// Only allocate the params map when there are actual params.
		if rt.paramCount > 0 {
			params = acquireParams()
			for i, rseg := range rt.segments {
				if rt.paramIndex[i] {
					params[rseg[1:]] = reqSegments[i]
				}
			}
		}

		return rt, params, false
	}

	// Nothing matched. GET "/" may still be served from staticDir/index.html.
	if r.autoServeRoot && nReq == 0 && b == bucketGET {
		return nil, nil, true
	}

	return nil, nil, false
}

// Find matches the incoming request to a registered route.
//
// Performance decisions:
//   - Routes are indexed by method, and an exact-path map answers static
//     routes without splitting the path at all (see methodIndex).
//   - Path splitting uses a manual scanner instead of strings.Split so we can
//     bail out early (wrong segment count) with zero allocations on a miss.
//   - The params map is only allocated when the route actually has :param
//     segments, and is pre-sized to paramCount.
//   - paramIndex[] is a pre-computed bool slice so we avoid strings.HasPrefix
//     inside the hot matching loop.
func (r *Router) Find(req *HTTPRequest) (HandlerFunc, []HandlerFunc, map[string]string) {
	rt, params, autoIndex := r.lookup(req)
	if rt != nil {
		return rt.handler, r.middlewares, params
	}
	if autoIndex {
		// Find is a public API whose callers treat a nil handler as "no such
		// route", so it keeps stat'ing the file and reports nil when it is
		// absent. findDispatch — the request path — skips the syscall and lets
		// the handler answer 404 instead.
		if _, err := os.Stat(filepath.Join(r.staticDir, "index.html")); err == nil {
			return r.autoIndexChain[0], r.middlewares, nil
		}
	}
	return nil, nil, nil
}

// findChain is the internal version of Find used by OnTraffic. It returns
// the route's PRECOMPUTED middleware chain:
//
//	chain = [global_mw..., route_mw..., handler]
//
// The chain is built once at registration time (Handle) and rebuilt only
// when global middlewares change (Use), so the request path performs ZERO
// chain allocations — OnTraffic assigns chain straight onto the Context.
// The returned slice is read-only at request time; Context.Next mutates
// only its own index, never the backing array, so a single shared chain is
// safe across concurrent requests.
//
// A nil chain means no route matched (404).
func (r *Router) findChain(req *HTTPRequest) (chain []HandlerFunc, params map[string]string) {
	chain, params, _ = r.findDispatch(req)
	return chain, params
}

// findDispatch is findChain plus the route's blocking flag, which OnTraffic
// needs to decide between running the chain inline on the event loop and
// handing it to the worker pool.
//
// The auto-index chain always reports blocking: it reads a file.
func (r *Router) findDispatch(req *HTTPRequest) (chain []HandlerFunc, params map[string]string, blocking bool) {
	rt, params, autoIndex := r.lookup(req)
	if rt != nil {
		return rt.chain, params, rt.blocking
	}
	if autoIndex {
		return r.autoIndexChain, nil, true
	}
	return nil, nil, false
}

// Middlewares returns the router's global middleware slice. This is used
// by OnTraffic to build the full middleware chain in a single allocation.
// The returned slice is NOT a copy — callers must not mutate it.
func (r *Router) Middlewares() []HandlerFunc {
	return r.middlewares
}
