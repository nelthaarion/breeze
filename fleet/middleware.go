package fleet

// Middleware is where a request becomes a span. It reads inbound trace context,
// decides sampling, exposes state for Tag and Inject to find, and on the way out
// emits one span describing this hop (§5.1).
//
// # Install order matters
//
//	router.Use(fleet.Middleware(tracer))   // first
//	router.Use(dashboard.Middleware(coll)) // second
//
// Fleet goes first so trace context exists before anything else runs, and so the
// span's duration covers the whole chain rather than the part after fleet.
//
// # Reusing the dashboard's timeline instead of instrumenting twice
//
// A sampled span carries the nested steps the dashboard's TimelineRecorder
// already captured. That recorder is attached by the dashboard middleware, which
// runs *after* this one, so the recorder cannot be captured on the way in — it
// does not exist yet. It is therefore read from the context on the way out, after
// the chain has returned. This is the only correct order, and getting it wrong is
// invisible: you get spans with empty timelines and no error anywhere.

import (
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/nelthaarion/breeze"
	"github.com/nelthaarion/breeze/dashboard"
)

// dashboardTimelineKey is where dashboard.Middleware stores its recorder.
//
// A literal, matching dashboard/middleware.go's own ctx.Set call, because the
// dashboard does not export it. If the dashboard ever renames this key, sampled
// spans quietly lose their nested timelines — which is why the integration test
// asserts a sampled span actually has steps, rather than trusting this string.
const dashboardTimelineKey = "breeze.dashboard.timeline"

// Teach the dashboard how to read a trace id off a request, so
// Collector.PushLogCtx can stamp log lines with it (§9C.2).
//
// Registered from init rather than from Middleware so it is set even in a
// process that logs before its first traced request, and so it does not depend
// on how many tracers a process happens to construct. TraceIDOf is a pure read
// of the request's own state, so there is nothing tracer-specific to bind.
func init() { dashboard.SetTraceIDResolver(TraceIDOf) }

// TraceIDOf returns the trace id of the request being handled, or "" if this
// request is not part of a trace.
//
// Public because a service using its own logger — rather than the dashboard's
// PushLog — still needs a way to put the trace id on its own log lines, which is
// the only thing that makes those lines findable from a trace later.
func TraceIDOf(ctx *breeze.Context) string {
	st, ok := stateFrom(ctx)
	if !ok {
		return ""
	}
	return st.tc.TraceIDHex()
}

// Middleware returns the Breeze middleware that traces every request.
//
// With a disabled or nil tracer it returns a pass-through that calls ctx.Next()
// and nothing else, so leaving the middleware installed in a service with
// tracing turned off costs one function call per request.
func Middleware(t *Tracer) breeze.HandlerFunc {
	if !t.Enabled() {
		return func(ctx *breeze.Context) error { return ctx.Next() }
	}

	service := t.cfg.ServiceName
	resolve := t.cfg.RouteResolver

	return func(ctx *breeze.Context) error {

		// ── Inbound: adopt or start a trace ──────────────────────────
		//
		// Read straight off the request header map. breeze lowercases
		// header keys on parse, so these are exact lookups with no
		// canonicalization and no allocation.
		tc, baggage, inherited := defaultExtract(requestCarrier{ctx: ctx})

		// spanID identifies *this* hop, and where it comes from differs by
		// case. Inheriting a context means tc.ParentSpanID is the caller's
		// span, so this hop needs an id of its own. Starting a root trace
		// means NewTraceContext's span id already *is* this hop's (see
		// TraceContext.ParentSpanID) — reusing it avoids a second
		// crypto/rand draw on every request that arrives from outside the
		// fleet, which is every request at the edge.
		var spanID [8]byte
		if inherited {
			spanID = tc.NewChildSpanID()
		} else {
			// No usable inbound context: this service is the edge for
			// this request, so it mints the trace id and is the only
			// hop that makes a sampling decision.
			tc = NewTraceContext()
			spanID = tc.ParentSpanID
		}
		sampled := t.sampler.decide(tc, inherited)

		st := &requestState{
			tc:          tc,
			spanID:      spanID,
			start:       time.Now(),
			sampled:     sampled,
			baggage:     baggage,
			tr:          t,
			serviceName: service,
		}

		ctx.Set(ctxStateKey, st)

		// ── Run the rest of the chain ────────────────────────────────
		//
		// Held rather than returned: the span emitted below is the whole point of this
		// middleware, and a failing request is the one a trace is most needed for.
		// Returning early would make errors invisible to Fleet — the exact opposite of
		// what tracing is for.
		chainErr := ctx.Next()

		// ── Outbound: emit the span ──────────────────────────────────
		//
		// Everything below runs after the handler has returned, so
		// ctx.Conn may already be closed. Nothing here touches it.
		// time.Since uses the Time's monotonic reading, so a clock step
		// during the request cannot produce a negative duration.
		durationMs := float64(time.Since(st.start).Microseconds()) / 1000.0

		status := 0
		if ctx.Res != nil {
			status = ctx.Res.Status
		}

		var errText string
		if status >= 500 {
			// Deliberately not the response body: it may contain user
			// data, and the span is the wrong place for it. The status
			// is the fact; the body belongs in logs, which §9C.2
			// stitches back into this trace anyway.
			errText = "HTTP " + strconv.Itoa(status)

		}

		// §7's policy, in one call. Unsampled successes stop here, which
		// is the common case at a low sample rate — no string cloning, no
		// span, no export.
		kind := exportFor(st.sampled, status, errText)
		if kind == exportNone {
			return chainErr
		}

		// Strings from ctx.Req are views into the connection's read
		// buffer under breeze's zero-copy headers, and that buffer is
		// reused for the next request on this connection. The span
		// outlives the request — it sits in a ring buffer and is
		// marshalled later, on the export goroutine — so every string
		// kept must be cloned. Without this a stored span silently
		// rewrites itself into fragments of whatever request came next.
		// Same reasoning, and same fix, as dashboard/middleware.go's
		// capture block.
		s := Span{
			TraceID: tc.TraceIDHex(),
			SpanID:  hex.EncodeToString(st.spanID[:]),

			Service:      service,
			Route:        strings.Clone(routeOf(ctx, resolve)),
			Method:       string(ctx.Req.Method),
			Status:       status,
			StartNanoUTC: st.start.UnixNano(),
			DurationMs:   durationMs,
			Error:        errText,
			Tags:         st.tagsSnapshot(),
		}
		// Only an inherited context has a parent. A root trace's
		// tc.ParentSpanID holds this hop's own span id, so encoding it
		// here would claim a parent that cannot exist and send the
		// aggregator hunting for it; the field stays empty instead, which
		// is what keeps Span.IsRoot meaningful. ParseTraceparent has
		// already rejected zero ids, so an inherited parent is non-zero
		// by construction.
		if inherited {
			s.ParentSpanID = hex.EncodeToString(tc.ParentSpanID[:])
		}

		// The nested timeline is the expensive part, so it is attached
		// only for a full export. An unsampled error still gets
		// everything above — identity, timing, parent link — which is
		// what §7 promises: errors are always visible, in less detail.
		if kind == exportFull {
			s.Timeline = timelineStepsFrom(ctx)
			if ctx.Req != nil {
				s.RequestPayload = CaptureJSONPayload(ctx.Req.Body)
			}
			if ctx.Res != nil {
				s.ResponsePayload = CaptureJSONPayload(ctx.Res.Body)
			}
		}

		t.RecordSpan(s)

		return chainErr
	}
}

// requestCarrier adapts an inbound breeze request to Carrier.
//
// Read-only: Set is a no-op because injecting into a request this service is
// *receiving* would mean rewriting a client's headers, which is never what a
// caller wants. Outbound injection goes through the carriers in client.go.
type requestCarrier struct{ ctx *breeze.Context }

func (c requestCarrier) Set(string, string) {}

func (c requestCarrier) Get(key string) (string, bool) {
	if c.ctx == nil || c.ctx.Req == nil || c.ctx.Req.Header == nil {
		return "", false
	}
	v, ok := c.ctx.Req.Header[key]
	return v, ok
}

// routeOf returns the route pattern for this request, falling back to its path.
//
// The pattern ("/orders/:id") is what spans should carry, not the concrete path
// ("/orders/42"): topology edges and per-route latency have to group requests
// that differ only by a parameter, and raw paths would make every order id its
// own node in the graph.
//
// breeze does not record the matched pattern on the request, so obtaining one
// requires the router — hence the resolver argument. When none is configured the
// concrete path is used, which keeps spans useful (a route attributed to
// /orders/42 still names the right service and endpoint) while making the
// higher-cardinality consequence the caller's explicit choice.
func routeOf(ctx *breeze.Context, resolve RouteResolver) string {
	if ctx == nil || ctx.Req == nil {
		return ""
	}
	if resolve != nil {
		if p := resolve(ctx); p != "" {
			return p
		}
	}
	return ctx.Req.Path
}

// RouteResolver maps a request to its registered route pattern.
//
// A function rather than a *breeze.Router field so a Tracer can be constructed
// without one (tests, and services that genuinely prefer path-level spans), and
// so an application that already knows its pattern by another means can supply
// it directly instead of paying for a lookup.
type RouteResolver func(*breeze.Context) string

// RouterResolver returns a RouteResolver backed by a Breeze router.
//
//	tracer := fleet.New(fleet.TracerConfig{
//	    ServiceName:   "orders-service",
//	    RouteResolver: fleet.RouterResolver(router),
//	})
//
// This mirrors how dashboard/middleware.go's matchRoute resolves patterns — a
// linear scan of the router's routes, comparing segments and treating ":param"
// as a wildcard. The cost is the same the dashboard already accepts on captured
// requests, and it is paid only on spans that are actually exported (§7), not on
// every request.
func RouterResolver(router *breeze.Router) RouteResolver {
	if router == nil {
		return nil
	}
	return func(ctx *breeze.Context) string {
		if ctx == nil || ctx.Req == nil {
			return ""
		}
		reqSegments := splitRoutePath(ctx.Req.Path)
		for _, rt := range router.RoutesInfo() {
			if rt.Method() != ctx.Req.Method {
				continue
			}
			if routeSegmentsMatch(rt, reqSegments) {
				return rt.Pattern()
			}
		}
		return ""
	}
}

// splitRoutePath splits a request path into segments, ignoring leading and
// trailing slashes so "/orders/" and "/orders" match the same route.
func splitRoutePath(p string) []string {
	if len(p) > 0 && p[0] == '/' {
		p = p[1:]
	}
	if len(p) > 0 && p[len(p)-1] == '/' {
		p = p[:len(p)-1]
	}
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// routeSegmentsMatch reports whether a registered route matches the request's
// segments, with ":param" matching any single segment and a wildcard route
// matching any longer path.
func routeSegmentsMatch(rt breeze.RouteInfo, req []string) bool {
	segs := rt.Segments()
	if rt.HasWildcard() {
		if len(req) < len(segs) {
			return false
		}
	} else if len(segs) != len(req) {
		return false
	}
	for i, s := range segs {
		if len(s) > 0 && s[0] == ':' {
			continue
		}
		if i >= len(req) || s != req[i] {
			return false
		}
	}
	return true
}

// timelineStepsFrom returns the dashboard's captured timeline for this request.
//
// Read from the context rather than held as a pointer because the dashboard
// middleware attaches its recorder after this middleware has already run, and
// only when someone is watching — so a sampled span legitimately has no timeline
// when nobody has the dashboard open. That is not a failure, and no counter is
// incremented for it.
func timelineStepsFrom(ctx *breeze.Context) []dashboard.TimelineStep {
	v, ok := ctx.Get(dashboardTimelineKey)
	if !ok {
		return nil
	}
	rec, ok := v.(*dashboard.TimelineRecorder)
	if !ok || rec == nil {
		return nil
	}
	return rec.Build()
}
