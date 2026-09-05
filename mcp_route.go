package breeze

// mcp_route.go — tagging a route as an MCP tool.
//
// Auto-MCP turns routes an application already serves into tools an agent can
// call. The tag is the whole opt-in surface:
//
//	router.Handle(breeze.POST, "/orders", createOrder,
//		auth.Require(),
//		breeze.MCPTool("create_order", "Places an order for a customer."))
//
// # Why the tag is a HandlerFunc
//
// Handle already accepts a variadic of per-route middleware, so a tag shaped
// like a middleware needs no new registration function, no parallel table
// keyed by method+pattern that could drift from the routing table, and no
// second call the author can forget. The tag arrives at the same instant as
// the route it describes, which is the only moment the two are guaranteed to
// agree.
//
// It is not, however, a step in the chain. Handle strips it at registration,
// so nothing about the request path changes for a tagged route: the same
// middlewares run in the same order, and the tag costs nothing per request.
//
// # Why identity is settled by probing, not by pointer comparison
//
// Recovering "which tool does this tag describe" from a func value is the
// awkward part. Go closures are not comparable, and reflect.Value.Pointer on
// a closure yields the address of the shared code, not of the captured data —
// so every tag ever created reports the same pointer. Pointer equality can
// therefore answer "is this a tag" exactly, and can never answer "which one".
//
// So the two questions are answered separately. The code pointer identifies a
// tag with certainty, because only MCPTool builds closures from that literal.
// Having established that, calling it is safe — it is known not to be user
// middleware — and the call reveals the spec it captured. The alternative,
// calling every middleware to see which ones respond, would execute real
// authentication middleware at registration time.
//
// The cost is one reflect call and one indirect call per registered
// middleware, at startup only. Nothing here runs on the request path.

import (
	"reflect"
	"sync"
	"sync/atomic"
)

// mcpToolSpec is what a tag carries: the name an agent calls the route by and
// the sentence telling it when to.
type mcpToolSpec struct {
	// Name is the tool name exposed over MCP. It is chosen by the author
	// rather than derived from method and path because it is what the model
	// reads first, and "create_order" earns a correct call more often than
	// "post_orders".
	Name string

	// Description is the tool's one-line purpose, shown to the model beside
	// the name. An empty description is allowed but unhelpful.
	Description string
}

// mcpRoute binds a tag to the route it was registered with.
//
// The route is held by pointer, the same pointer the method index holds, so a
// tool always dispatches through the chain that route is actually serving —
// including global middleware added by a later Use, which rebuilds chains in
// place.
type mcpRoute struct {
	spec *mcpToolSpec
	rt   *route
}

var (
	// mcpProbeSlot is where a tag deposits its spec when called. It is written
	// only under mcpProbeMu (by mcpSpecOf) or by a stray call that cannot
	// happen once Handle has stripped the tag; it is atomic so that even the
	// impossible case is not a data race.
	mcpProbeSlot atomic.Pointer[mcpToolSpec]

	// mcpProbeMu serialises probes so two concurrent registrations cannot read
	// each other's deposit. Route registration is a startup activity, so the
	// contention this creates is not on any path that matters.
	mcpProbeMu sync.Mutex

	// mcpSentinelPC is the code pointer shared by every closure MCPTool
	// returns. It stays zero until the first tag is created, which is what
	// makes mcpSpecOf free for applications that never use Auto-MCP.
	mcpSentinelPC   uintptr
	mcpSentinelOnce sync.Once
)

// MCPTool tags the route it is registered with as an MCP tool.
//
// Pass it to Handle in the middleware position. It performs no work during a
// request — Handle removes it from the chain — and it changes nothing about
// how the route is served over HTTP. A route without this tag is never
// exposed as a tool, which is what keeps an internal endpoint internal.
func MCPTool(name, description string) HandlerFunc {
	spec := &mcpToolSpec{Name: name, Description: description}

	// The body deposits the spec unconditionally rather than checking a
	// "probing" flag. A flag would have to be read here and written by the
	// prober, and the prober holds the lock while calling this, so the read
	// could not take it. Depositing always is race-free and, for a value only
	// ever read immediately after a deliberate call, just as precise.
	fn := func(ctx *Context) error {
		mcpProbeSlot.Store(spec)
		return nil
	}

	mcpSentinelOnce.Do(func() { mcpSentinelPC = reflect.ValueOf(fn).Pointer() })
	return fn
}

// mcpSpecOf returns the tool spec fn carries, or nil when fn is ordinary
// middleware.
//
// The code-pointer test comes first and is what makes the call below safe:
// only closures built by MCPTool share that pointer, so nothing else is ever
// invoked here.
func mcpSpecOf(fn HandlerFunc) *mcpToolSpec {
	if fn == nil || mcpSentinelPC == 0 {
		return nil
	}
	if reflect.ValueOf(fn).Pointer() != mcpSentinelPC {
		return nil
	}

	mcpProbeMu.Lock()
	defer mcpProbeMu.Unlock()

	mcpProbeSlot.Store(nil)
	fn(nil) // safe: established above that this is a tag, not user middleware
	return mcpProbeSlot.Load()
}

// MCPRoutes reports the tagged routes in registration order.
//
// It exists so the MCP server can build its tool list from the routing table
// itself. Untagged routes are absent by construction: nothing adds a route
// here except a tag on that route.
func (r *Router) MCPRoutes() []RouteInfo {
	out := make([]RouteInfo, 0, len(r.mcpTools))
	for _, t := range r.mcpTools {
		out = append(out, t.rt)
	}
	return out
}
