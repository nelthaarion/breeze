package fleet

// Tests for §5.1 — the middleware that turns a request into a span.
//
// Requests are driven through a real middleware chain rather than by calling the
// handler directly, because two of the properties under test are entirely about
// ordering: the span's duration has to cover the whole chain, and the dashboard's
// timeline recorder is attached *after* this middleware runs, so it can only be
// read on the way out. Both would pass trivially against a hand-rolled call and
// fail in production.
//
// Spans are collected by closing the tracer, whose documented Close flushes
// whatever is buffered. That makes every assertion below deterministic — no
// sleeping, no polling a background goroutine and hoping it woke up.

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/nelthaarion/breeze"
	"github.com/nelthaarion/breeze/dashboard"
)

// newRequest builds a parsed request as breeze's HTTP parser would hand one over.
//
// Header keys are written lowercase because that is what the parser produces, and
// the middleware reads the map directly to avoid canonicalization on the hot path.
func newRequest(method breeze.Method, path string, headers map[string]string) *breeze.HTTPRequest {
	h := make(map[string]string, len(headers))
	for k, v := range headers {
		h[strings.ToLower(k)] = v
	}
	return &breeze.HTTPRequest{Method: method, Path: path, Header: h}
}

// traced runs one request through the fleet middleware and returns the spans that
// actually reached the transport, plus the context the chain ran on.
//
// The tracer is closed before returning, so the spans are the complete set for
// this request rather than whatever a background flush happened to have sent.
func traced(t *testing.T, cfg TracerConfig, req *breeze.HTTPRequest, handler breeze.HandlerFunc) ([]Span, *breeze.Context) {
	t.Helper()

	rt := newRecordingTransport()
	cfg.Transport = rt
	if cfg.ServiceName == "" {
		cfg.ServiceName = "orders-service"
	}
	if cfg.AggregatorURL == "" {
		cfg.AggregatorURL = "http://aggregator"
	}
	tr := New(cfg)

	ctx := breeze.NewContext(req.Method, req.Path)
	ctx.Req = req
	ctx.SetMiddlewareChain([]breeze.HandlerFunc{Middleware(tr)}, handler)
	ctx.Next()

	closeTracer(t, tr)
	return rt.exportedSpans(), ctx
}

// sampledConfig is the common case for these tests: every request sampled, so
// span content is asserted without the sampler deciding otherwise.
func sampledConfig() TracerConfig {
	return TracerConfig{Enabled: true, SampleRate: 1.0}
}

// ok is a handler that returns 200.
func ok(ctx *breeze.Context) error {
	ctx.Status(200)
	return nil
}

// exactlyOne fails the test unless a single span was exported.
func exactlyOne(t *testing.T, spans []Span) Span {
	t.Helper()
	if len(spans) != 1 {
		t.Fatalf("exported %d spans, want exactly 1", len(spans))
	}
	return spans[0]
}

// --- Disabled ---------------------------------------------------------------

// TestMiddlewareDisabledIsPassThrough — §16 requires an app with tracing off to
// be indistinguishable from one that never imported fleet. The middleware must
// therefore leave no state behind for Tag or Inject to find, and record nothing.
func TestMiddlewareDisabledIsPassThrough(t *testing.T) {
	req := newRequest(breeze.GET, "/orders", nil)
	reached := false

	spans, ctx := traced(t, TracerConfig{Enabled: false}, req, func(c *breeze.Context) error {
		reached = true
		if _, ok := stateFrom(c); ok {
			t.Error("a disabled tracer still attached request state")
		}
		c.Status(200)

		return nil
	})

	if !reached {
		t.Fatal("the handler never ran, so the middleware swallowed the request")
	}
	if len(spans) != 0 {
		t.Errorf("exported %d spans with tracing disabled", len(spans))
	}
	if _, ok := stateFrom(ctx); ok {
		t.Error("state outlived the request on a disabled tracer")
	}
}

// TestMiddlewareNilTracerIsPassThrough — Middleware(nil) is what a service ends
// up with when its setup returned no tracer. It must serve traffic normally.
func TestMiddlewareNilTracerIsPassThrough(t *testing.T) {
	req := newRequest(breeze.GET, "/orders", nil)
	ctx := breeze.NewContext(req.Method, req.Path)
	ctx.Req = req

	reached := false
	ctx.SetMiddlewareChain([]breeze.HandlerFunc{Middleware(nil)}, func(c *breeze.Context) error {
		reached = true
		c.Status(200)

		return nil
	})
	ctx.Next()

	if !reached {
		t.Error("Middleware(nil) did not call the next handler")
	}
}

// --- Root traces ------------------------------------------------------------

// TestMiddlewareStartsARootTrace covers the edge service: a request arrives from
// outside the fleet with no traceparent, so this hop mints the trace.
func TestMiddlewareStartsARootTrace(t *testing.T) {
	req := newRequest(breeze.POST, "/orders", nil)

	spans, _ := traced(t, sampledConfig(), req, ok)

	s := exactlyOne(t, spans)
	if s.ParentSpanID != "" {
		t.Errorf("ParentSpanID = %q, want empty — a root span with a parent sends the aggregator hunting for a span that cannot exist", s.ParentSpanID)
	}
	if !s.IsRoot() {
		t.Error("IsRoot() = false for a request that arrived without a traceparent")
	}
	if len(s.TraceID) != 32 {
		t.Errorf("TraceID = %q, want 32 hex chars", s.TraceID)
	}
	if len(s.SpanID) != 16 {
		t.Errorf("SpanID = %q, want 16 hex chars", s.SpanID)
	}
}

// TestMiddlewareRecordsTheRequestFacts pins the span's descriptive fields, which
// are what every view in the Fleet UI groups and filters on.
func TestMiddlewareRecordsTheRequestFacts(t *testing.T) {
	req := newRequest(breeze.POST, "/orders/42/charge", nil)
	before := time.Now().UnixNano()

	spans, _ := traced(t, TracerConfig{Enabled: true, SampleRate: 1.0, ServiceName: "orders-service"}, req,
		func(c *breeze.Context) error {
			time.Sleep(2 * time.Millisecond) // give the duration something to measure
			c.Status(201)

			return nil
		})

	s := exactlyOne(t, spans)
	if s.Service != "orders-service" {
		t.Errorf("Service = %q, want orders-service", s.Service)
	}
	if s.Method != "POST" {
		t.Errorf("Method = %q, want POST", s.Method)
	}
	if s.Route != "/orders/42/charge" {
		t.Errorf("Route = %q, want the request path when no resolver is configured", s.Route)
	}
	if s.Status != 201 {
		t.Errorf("Status = %d, want 201", s.Status)
	}
	if s.DurationMs < 1 {
		t.Errorf("DurationMs = %v, want at least the 2ms the handler slept", s.DurationMs)
	}
	if s.StartNanoUTC < before {
		t.Error("StartNanoUTC predates the request")
	}
	if !s.Valid() {
		t.Error("the middleware produced a span the aggregator would reject as malformed")
	}
}

// TestMiddlewareSpanCoversTheWholeChain is why install order is documented. The
// span's duration must include the handlers that run after this middleware, or
// every reported latency understates the request it describes.
func TestMiddlewareSpanCoversTheWholeChain(t *testing.T) {
	req := newRequest(breeze.GET, "/orders", nil)
	rt := newRecordingTransport()
	tr := New(TracerConfig{
		Enabled: true, SampleRate: 1.0,
		ServiceName: "svc", AggregatorURL: "http://aggregator", Transport: rt,
	})

	slow := func(c *breeze.Context) error {
		time.Sleep(5 * time.Millisecond)
		return c.Next()
	}

	ctx := breeze.NewContext(req.Method, req.Path)
	ctx.Req = req
	ctx.SetMiddlewareChain([]breeze.HandlerFunc{Middleware(tr), slow}, ok)
	ctx.Next()
	closeTracer(t, tr)

	s := exactlyOne(t, rt.exportedSpans())
	if s.DurationMs < 4 {
		t.Errorf("DurationMs = %v, want ≥ the 5ms spent downstream — the span is not timing the full chain", s.DurationMs)
	}
}

// --- Adopting an inbound trace ---------------------------------------------

// TestMiddlewareAdoptsInboundTrace is the property that makes a trace a tree
// rather than a pile: this hop joins the caller's trace and records the caller's
// span as its parent.
func TestMiddlewareAdoptsInboundTrace(t *testing.T) {
	upstream := NewTraceContext()
	upstream.Sampled = true
	req := newRequest(breeze.GET, "/auth/verify", map[string]string{
		HeaderTraceparent: upstream.String(),
		HeaderService:     "gateway",
	})

	spans, _ := traced(t, sampledConfig(), req, ok)

	s := exactlyOne(t, spans)
	if s.TraceID != upstream.TraceIDHex() {
		t.Errorf("TraceID = %q, want the caller's %q — the two hops would appear in separate traces", s.TraceID, upstream.TraceIDHex())
	}
	if s.ParentSpanID == "" {
		t.Fatal("ParentSpanID is empty on an inbound-traced request, so this hop looks like a second root")
	}
	if s.SpanID == s.ParentSpanID {
		t.Error("the span is its own parent")
	}
	if s.IsRoot() {
		t.Error("IsRoot() = true for a hop that was called by someone else")
	}
}

// TestMiddlewareInheritsTheSamplingDecision — §7: the root decides for the whole
// trace. A downstream service must not re-roll, even with a rate that says no,
// because a hole in the middle of a trace is indistinguishable from a service
// that failed to report.
func TestMiddlewareInheritsTheSamplingDecision(t *testing.T) {
	upstream := NewTraceContext()
	upstream.Sampled = true
	req := newRequest(breeze.GET, "/auth/verify", map[string]string{
		HeaderTraceparent: upstream.String(),
	})

	// SampleRate 0 would decline this request if it were the root.
	spans, _ := traced(t, TracerConfig{Enabled: true, SampleRate: 0}, req, ok)

	if len(spans) != 1 {
		t.Fatalf("exported %d spans, want 1 — the local rate overrode the caller's decision", len(spans))
	}
}

// TestMiddlewareHonoursAnUnsampledParent is the same rule in the other
// direction: a local rate of 1.0 must not promote a trace the root declined.
func TestMiddlewareHonoursAnUnsampledParent(t *testing.T) {
	upstream := NewTraceContext()
	upstream.Sampled = false
	req := newRequest(breeze.GET, "/auth/verify", map[string]string{
		HeaderTraceparent: upstream.String(),
	})

	spans, _ := traced(t, sampledConfig(), req, ok)

	if len(spans) != 0 {
		t.Errorf("exported %d spans, want 0 — this hop overruled an unsampled root", len(spans))
	}
}

// TestMiddlewareMalformedHeaderStartsAFreshTrace — §4.1: a broken header from a
// misbehaving upstream must not propagate a broken trace, and must be counted so
// the culprit is findable.
func TestMiddlewareMalformedHeaderStartsAFreshTrace(t *testing.T) {
	before := ReadMetrics().MalformedHeaderTotal
	req := newRequest(breeze.GET, "/orders", map[string]string{
		HeaderTraceparent: "00-not-a-real-traceparent-xx",
	})

	spans, _ := traced(t, sampledConfig(), req, ok)

	s := exactlyOne(t, spans)
	if s.ParentSpanID != "" {
		t.Error("a malformed header produced a parent link, so the span points at a parent that was never parsed")
	}
	if len(s.TraceID) != 32 {
		t.Errorf("TraceID = %q, want a freshly minted 32-hex id", s.TraceID)
	}
	if ReadMetrics().MalformedHeaderTotal <= before {
		t.Error("fleet_malformed_header_total did not move, so a broken upstream would be invisible")
	}
}

// --- Sampling and error policy ---------------------------------------------

// TestMiddlewareDropsUnsampledSuccess is the common case at a low sample rate,
// and the reason tracing is affordable: nothing is recorded at all.
func TestMiddlewareDropsUnsampledSuccess(t *testing.T) {
	req := newRequest(breeze.GET, "/orders", nil)

	spans, _ := traced(t, TracerConfig{Enabled: true, SampleRate: 0}, req, ok)

	if len(spans) != 0 {
		t.Errorf("exported %d spans for an unsampled success, want 0", len(spans))
	}
}

// TestMiddlewareAlwaysReportsErrors — §7's always-sample-errors rule. An
// unsampled failure still produces a correctly parent-linked span, because a
// failure nobody recorded is the one case where the sample budget is the wrong
// thing to respect.
func TestMiddlewareAlwaysReportsErrors(t *testing.T) {
	req := newRequest(breeze.GET, "/orders", nil)

	spans, _ := traced(t, TracerConfig{Enabled: true, SampleRate: 0}, req, func(c *breeze.Context) error {
		c.Status(503)

		return nil
	})

	s := exactlyOne(t, spans)
	if s.Status != 503 {
		t.Errorf("Status = %d, want 503", s.Status)
	}
	if s.Error == "" {
		t.Error("Error is empty on a 5xx span, so the Fleet UI cannot tell it failed")
	}
	if !s.Failed() {
		t.Error("Failed() = false for a 503")
	}
	if s.Timeline != nil {
		t.Error("an unsampled error carried a timeline — the expensive capture was never authorised for this request")
	}
}

// TestMiddlewareErrorTextExcludesTheResponseBody — the body may hold user data,
// and a span is the wrong place for it. §11.3 treats this as a security property,
// not a formatting preference.
func TestMiddlewareErrorTextExcludesTheResponseBody(t *testing.T) {
	req := newRequest(breeze.GET, "/orders", nil)
	secret := "card number 4111111111111111"

	spans, _ := traced(t, sampledConfig(), req, func(c *breeze.Context) error {
		c.Status(500)
		return c.WriteString(secret)
	})

	s := exactlyOne(t, spans)
	if strings.Contains(s.Error, "4111") {
		t.Errorf("Error = %q, which carries response-body content off the service", s.Error)
	}
	if !strings.Contains(s.Error, "500") {
		t.Errorf("Error = %q, want it to name the status", s.Error)
	}
}

// TestMiddlewareClientErrorsAreNotFailures — a 404 is the caller's problem, not
// this service's. Treating 4xx as an error would make root-cause analysis (§9B)
// point at whichever service was handed a bad request.
func TestMiddlewareClientErrorsAreNotFailures(t *testing.T) {
	req := newRequest(breeze.GET, "/orders/nope", nil)

	spans, _ := traced(t, sampledConfig(), req, func(c *breeze.Context) error {
		c.Status(404)

		return nil
	})

	s := exactlyOne(t, spans)
	if s.Error != "" {
		t.Errorf("Error = %q, want empty for a 404", s.Error)
	}
	if s.Failed() {
		t.Error("Failed() = true for a 404")
	}
}

// TestMiddlewareUnsampledClientErrorIsDropped — the corollary: a 4xx gets no
// special treatment, so at a low sample rate it costs nothing.
func TestMiddlewareUnsampledClientErrorIsDropped(t *testing.T) {
	req := newRequest(breeze.GET, "/orders/nope", nil)

	spans, _ := traced(t, TracerConfig{Enabled: true, SampleRate: 0}, req, func(c *breeze.Context) error {
		c.Status(404)

		return nil
	})

	if len(spans) != 0 {
		t.Errorf("exported %d spans for an unsampled 404, want 0", len(spans))
	}
}

// TestMiddlewareHandlesAMissingResponse — a handler that writes nothing leaves
// ctx.Res nil. The span must still be emitted rather than panicking on the way
// out, since this is exactly what an aborted or hijacked request looks like.
func TestMiddlewareHandlesAMissingResponse(t *testing.T) {
	req := newRequest(breeze.GET, "/orders", nil)

	spans, _ := traced(t, sampledConfig(), req, func(c *breeze.Context) error {
		return nil
	})

	s := exactlyOne(t, spans)
	if s.Status != 0 {
		t.Errorf("Status = %d, want 0 when the handler wrote no response", s.Status)
	}
	if s.Error != "" {
		t.Errorf("Error = %q, want empty — no response is not a 5xx", s.Error)
	}
}

// --- Timeline reuse --------------------------------------------------------

// TestMiddlewareAttachesTheDashboardTimeline is §5.1's no-double-instrumentation
// requirement, and the ordering trap the file comment describes: the recorder is
// attached by the dashboard middleware *after* fleet's, so it can only be read
// after the chain returns. Reading it on the way in silently yields empty
// timelines with no error anywhere.
func TestMiddlewareAttachesTheDashboardTimeline(t *testing.T) {
	req := newRequest(breeze.GET, "/orders", nil)

	spans, _ := traced(t, sampledConfig(), req, func(c *breeze.Context) error {
		rec := dashboard.NewTimelineRecorder(dashboard.Config{MaxTimelineEntries: 32})
		c.Set(dashboardTimelineKey, rec)
		rec.Step("ORM Query")(map[string]any{"rows": 42})
		c.Status(200)

		return nil
	})

	s := exactlyOne(t, spans)
	if len(s.Timeline) == 0 {
		t.Fatal("a sampled span carries no timeline steps, so the merged waterfall would show this hop as a bare bar")
	}
	if s.Timeline[0].Name != "ORM Query" {
		t.Errorf("Timeline[0].Name = %q, want ORM Query", s.Timeline[0].Name)
	}
}

// TestMiddlewareToleratesNoTimeline — the dashboard attaches its recorder only
// when someone is watching, so a sampled span legitimately has no timeline. That
// is not a failure and must not be treated as one.
func TestMiddlewareToleratesNoTimeline(t *testing.T) {
	req := newRequest(breeze.GET, "/orders", nil)

	spans, _ := traced(t, sampledConfig(), req, ok)

	s := exactlyOne(t, spans)
	if s.Timeline != nil {
		t.Errorf("Timeline = %v, want nil when no recorder was attached", s.Timeline)
	}
}

// TestTimelineStepsFromIgnoresAForeignValue — the context store is shared with
// application code that has never heard of fleet, so the key could hold
// anything. A wrong type must yield no timeline instead of panicking.
func TestTimelineStepsFromIgnoresAForeignValue(t *testing.T) {
	ctx := breeze.NewContext(breeze.GET, "/")

	if got := timelineStepsFrom(ctx); got != nil {
		t.Errorf("timelineStepsFrom with nothing set = %v, want nil", got)
	}

	ctx.Set(dashboardTimelineKey, "not a recorder")
	if got := timelineStepsFrom(ctx); got != nil {
		t.Errorf("timelineStepsFrom with a foreign value = %v, want nil", got)
	}

	var nilRec *dashboard.TimelineRecorder
	ctx.Set(dashboardTimelineKey, nilRec)
	if got := timelineStepsFrom(ctx); got != nil {
		t.Errorf("timelineStepsFrom with a nil recorder = %v, want nil", got)
	}
}

// --- Tags and baggage ------------------------------------------------------

// TestMiddlewareCarriesTagsOntoTheSpan — §9C.1: a tag set in the handler is what
// makes "find the trace for order 123" possible, so it has to survive onto the
// exported span.
func TestMiddlewareCarriesTagsOntoTheSpan(t *testing.T) {
	req := newRequest(breeze.GET, "/orders/123", nil)

	spans, _ := traced(t, sampledConfig(), req, func(c *breeze.Context) error {
		Tag(c, "order_id", "123")
		Tag(c, "payment_provider", "stripe")
		c.Status(200)

		return nil
	})

	s := exactlyOne(t, spans)
	if s.Tags["order_id"] != "123" {
		t.Errorf("Tags[order_id] = %q, want 123", s.Tags["order_id"])
	}
	if s.Tags["payment_provider"] != "stripe" {
		t.Errorf("Tags[payment_provider] = %q, want stripe", s.Tags["payment_provider"])
	}
}

// TestMiddlewareInheritsBaggageAsTags — §9C.1's propagation half. A tag set by an
// upstream service must appear on this hop's span too, otherwise a tag search
// finds only the service that set it rather than the whole request's journey.
func TestMiddlewareInheritsBaggageAsTags(t *testing.T) {
	upstream := NewTraceContext()
	upstream.Sampled = true
	req := newRequest(breeze.GET, "/auth/verify", map[string]string{
		HeaderTraceparent: upstream.String(),
		HeaderBaggage:     Baggage{"order_id": "123"}.String(),
	})

	spans, ctx := traced(t, sampledConfig(), req, func(c *breeze.Context) error {
		// The inherited value must be visible to a handler that tags on
		// top of it, not merely present on the wire.
		Tag(c, "local", "yes")
		c.Status(200)

		return nil
	})

	st, found := stateFrom(ctx)
	if !found {
		t.Fatal("no request state")
	}
	if st.baggage["order_id"] != "123" {
		t.Errorf("inherited baggage = %v, want order_id=123", st.baggage)
	}

	// Onward propagation must carry both the inherited key and the local one,
	// so a fourth hop sees the whole chain's context.
	carrier := MapCarrier{}
	Inject(ctx, carrier)
	raw, _ := carrier.Get(HeaderBaggage)
	bag, _ := ParseBaggage(raw)
	if bag["order_id"] != "123" {
		t.Errorf("onward baggage lost the inherited key: %v", bag)
	}
	if bag["local"] != "yes" {
		t.Errorf("onward baggage lost the local tag: %v", bag)
	}

	_ = exactlyOne(t, spans)
}

// TestMiddlewareExposesStateForPropagation is the seam every PropagateFromX call
// depends on: by the time the handler runs, Inject must find a usable trace.
func TestMiddlewareExposesStateForPropagation(t *testing.T) {
	req := newRequest(breeze.GET, "/orders", nil)
	var injected MapCarrier

	spans, _ := traced(t, sampledConfig(), req, func(c *breeze.Context) error {
		injected = MapCarrier{}
		Inject(c, injected)
		c.Status(200)

		return nil
	})

	s := exactlyOne(t, spans)
	raw, ok := injected.Get(HeaderTraceparent)
	if !ok {
		t.Fatal("Inject inside the handler wrote nothing, so no downstream call from this service would be traced")
	}
	tc, valid := ParseTraceparent(raw)
	if !valid {
		t.Fatalf("unparseable traceparent %q", raw)
	}
	if tc.TraceIDHex() != s.TraceID {
		t.Error("the outgoing call was attributed to a different trace than this hop's own span")
	}
	// The downstream hop's parent must be this hop's span, which is what
	// links the two rows in the assembled trace.
	if got := hex.EncodeToString(tc.ParentSpanID[:]); got != s.SpanID {
		t.Errorf("outgoing parent = %s, want this span %s", got, s.SpanID)
	}
	if got, _ := injected.Get(HeaderService); got != s.Service {
		t.Errorf("%s = %q, want %q", HeaderService, got, s.Service)
	}
}

// --- Route resolution ------------------------------------------------------

// TestMiddlewareUsesTheRoutePattern — spans must carry "/orders/:id", not
// "/orders/42", or the topology graph grows one node per order id.
func TestMiddlewareUsesTheRoutePattern(t *testing.T) {
	router := breeze.NewRouter()
	router.Handle(breeze.GET, "/orders/:id", ok)
	req := newRequest(breeze.GET, "/orders/42", nil)

	cfg := sampledConfig()
	cfg.RouteResolver = RouterResolver(router)
	spans, _ := traced(t, cfg, req, ok)

	s := exactlyOne(t, spans)
	if s.Route != "/orders/:id" {
		t.Errorf("Route = %q, want the pattern /orders/:id — raw paths give every id its own topology node", s.Route)
	}
}

// TestMiddlewareFallsBackToThePath — an unmatched path still has to identify the
// endpoint, since a span with an empty route is useless in every view.
func TestMiddlewareFallsBackToThePath(t *testing.T) {
	router := breeze.NewRouter()
	router.Handle(breeze.GET, "/orders/:id", ok)
	req := newRequest(breeze.GET, "/unregistered/path", nil)

	cfg := sampledConfig()
	cfg.RouteResolver = RouterResolver(router)
	spans, _ := traced(t, cfg, req, ok)

	s := exactlyOne(t, spans)
	if s.Route != "/unregistered/path" {
		t.Errorf("Route = %q, want the concrete path as a fallback", s.Route)
	}
}

func TestRouterResolver(t *testing.T) {
	router := breeze.NewRouter()
	router.Handle(breeze.GET, "/orders", ok)
	router.Handle(breeze.GET, "/orders/:id", ok)
	router.Handle(breeze.GET, "/orders/:id/items/:item", ok)
	router.Handle(breeze.POST, "/orders", ok)
	router.Handle(breeze.GET, "/static/*filepath", ok)

	resolve := RouterResolver(router)

	cases := []struct {
		desc   string
		method breeze.Method
		path   string
		want   string
	}{
		{"exact", breeze.GET, "/orders", "/orders"},
		{"one param", breeze.GET, "/orders/42", "/orders/:id"},
		{"two params", breeze.GET, "/orders/42/items/7", "/orders/:id/items/:item"},
		{"method distinguishes same path", breeze.POST, "/orders", "/orders"},
		// A trailing slash is the same resource, so it must not fall back to
		// the raw path and split one endpoint into two rows in the catalog.
		{"trailing slash", breeze.GET, "/orders/", "/orders"},
		{"wildcard", breeze.GET, "/static/css/app.css", "/static/*filepath"},
		{"wildcard at its own root", breeze.GET, "/static", "/static/*filepath"},
		// Unmatched cases return "" so routeOf can apply the path fallback.
		{"too many segments", breeze.GET, "/orders/42/items/7/extra", ""},
		{"unknown prefix", breeze.GET, "/nope", ""},
		{"unregistered method", breeze.DELETE, "/orders", ""},
		{"root", breeze.GET, "/", ""},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			ctx := breeze.NewContext(c.method, c.path)
			ctx.Req = newRequest(c.method, c.path, nil)
			if got := resolve(ctx); got != c.want {
				t.Errorf("resolve(%s %s) = %q, want %q", c.method, c.path, got, c.want)
			}
		})
	}
}

// TestRouterResolverNilRouter — a nil router yields no resolver rather than one
// that panics on first use, so RouterResolver is safe to call unconditionally
// during config assembly.
func TestRouterResolverNilRouter(t *testing.T) {
	if RouterResolver(nil) != nil {
		t.Error("RouterResolver(nil) returned a resolver")
	}
}

// TestRouterResolverToleratesNoRequest — resolvers run on the way out, when a
// context may be in an unexpected state.
func TestRouterResolverToleratesNoRequest(t *testing.T) {
	router := breeze.NewRouter()
	router.Handle(breeze.GET, "/orders", ok)
	resolve := RouterResolver(router)

	if got := resolve(nil); got != "" {
		t.Errorf("resolve(nil) = %q, want empty", got)
	}
	if got := resolve(contextWithoutRequest()); got != "" {
		t.Errorf("resolve with no Req = %q, want empty", got)
	}
}

func TestRouteOf(t *testing.T) {
	req := newRequest(breeze.GET, "/orders/42", nil)
	ctx := breeze.NewContext(breeze.GET, "/orders/42")
	ctx.Req = req

	if got := routeOf(nil, nil); got != "" {
		t.Errorf("routeOf(nil, nil) = %q, want empty", got)
	}
	if got := routeOf(contextWithoutRequest(), nil); got != "" {
		t.Errorf("routeOf with no Req = %q, want empty", got)
	}
	if got := routeOf(ctx, nil); got != "/orders/42" {
		t.Errorf("routeOf without a resolver = %q, want the path", got)
	}
	// A resolver that finds nothing must not blank out the route.
	empty := func(*breeze.Context) string { return "" }
	if got := routeOf(ctx, empty); got != "/orders/42" {
		t.Errorf("routeOf with an unmatched resolver = %q, want the path fallback", got)
	}
	pattern := func(*breeze.Context) string { return "/orders/:id" }
	if got := routeOf(ctx, pattern); got != "/orders/:id" {
		t.Errorf("routeOf with a resolver = %q, want /orders/:id", got)
	}
}

// --- requestCarrier --------------------------------------------------------

// TestRequestCarrierIsReadOnly — injecting into a request this service is
// *receiving* would rewrite the caller's headers, which is never what anyone
// wants. Set is deliberately inert.
func TestRequestCarrierIsReadOnly(t *testing.T) {
	req := newRequest(breeze.GET, "/orders", map[string]string{HeaderTraceparent: "value"})
	ctx := breeze.NewContext(breeze.GET, "/orders")
	ctx.Req = req
	c := requestCarrier{ctx: ctx}

	c.Set(HeaderTraceparent, "overwritten")

	if got := req.Header[HeaderTraceparent]; got != "value" {
		t.Errorf("inbound header = %q, want the caller's original value", got)
	}
	if got, ok := c.Get(HeaderTraceparent); !ok || got != "value" {
		t.Errorf("Get = %q, %v; want value, true", got, ok)
	}
}

// TestRequestCarrierToleratesAnEmptyContext — Get runs on every request before
// anything is known to be present, so each missing layer must return cleanly.
func TestRequestCarrierToleratesAnEmptyContext(t *testing.T) {
	cases := map[string]requestCarrier{
		"nil context":    {ctx: nil},
		"no request":     {ctx: contextWithoutRequest()},
		"nil header map": {ctx: contextWithRequest(&breeze.HTTPRequest{Method: breeze.GET, Path: "/"})},
		"empty header":   {ctx: contextWithRequest(newRequest(breeze.GET, "/", nil))},
	}
	for desc, c := range cases {
		t.Run(desc, func(t *testing.T) {
			if got, ok := c.Get(HeaderTraceparent); ok || got != "" {
				t.Errorf("Get = %q, %v; want empty, false", got, ok)
			}
		})
	}
}

// contextWithRequest is a small constructor for the carrier cases above.
func contextWithRequest(req *breeze.HTTPRequest) *breeze.Context {
	ctx := breeze.NewContext(req.Method, req.Path)
	ctx.Req = req
	return ctx
}

// contextWithoutRequest returns a context carrying no request at all.
//
// breeze.NewContext always populates Req, so this state cannot be built through
// it — but it is reachable in production, since a context is recycled between
// requests and the outbound half of the middleware runs after the handler has
// returned. Every function that reads ctx.Req has to survive it.
func contextWithoutRequest() *breeze.Context {
	ctx := breeze.NewContext(breeze.GET, "/")
	ctx.Req = nil
	return ctx
}

// --- Buffer-reuse safety ---------------------------------------------------

// TestMiddlewareClonesRouteOffTheReadBuffer is the subtlest correctness property
// in this file. Under breeze's zero-copy header parsing, ctx.Req strings view the
// connection's read buffer, which is reused by the next request on that
// connection. A span outlives its request — it sits in a ring buffer and is
// marshalled later, on another goroutine — so any borrowed string would silently
// rewrite itself into fragments of whatever arrived next.
//
// The buffer reuse is simulated by overwriting the request's fields after the
// chain returns, which is exactly what the parser does to a recycled request.
func TestMiddlewareClonesRouteOffTheReadBuffer(t *testing.T) {
	path := "/orders/42/charge"
	req := newRequest(breeze.POST, path, nil)

	rt := newRecordingTransport()
	tr := New(TracerConfig{
		Enabled: true, SampleRate: 1.0,
		ServiceName: "orders-service", AggregatorURL: "http://aggregator", Transport: rt,
	})

	ctx := breeze.NewContext(req.Method, req.Path)
	ctx.Req = req
	ctx.SetMiddlewareChain([]breeze.HandlerFunc{Middleware(tr)}, ok)
	ctx.Next()

	// The connection is recycled: the parser overwrites the request in place
	// for the next request on this socket.
	req.Path = "/completely/different/path"
	req.Method = breeze.GET

	closeTracer(t, tr)

	s := exactlyOne(t, rt.exportedSpans())
	if s.Route != path {
		t.Errorf("Route = %q, want %q — the span is holding a view into a recycled buffer rather than its own copy", s.Route, path)
	}
	if s.Method != "POST" {
		t.Errorf("Method = %q, want POST — the method was read after the request was recycled", s.Method)
	}
}

// TestMiddlewareSpanTagsAreASnapshot — the request state is recycled while the
// span it produced waits in the ring buffer, so handing the live tag map over
// would let a later request mutate a span already queued for export.
func TestMiddlewareSpanTagsAreASnapshot(t *testing.T) {
	req := newRequest(breeze.GET, "/orders/123", nil)
	rt := newRecordingTransport()
	tr := New(TracerConfig{
		Enabled: true, SampleRate: 1.0,
		ServiceName: "svc", AggregatorURL: "http://aggregator", Transport: rt,
	})

	var captured *requestState
	ctx := breeze.NewContext(req.Method, req.Path)
	ctx.Req = req
	ctx.SetMiddlewareChain([]breeze.HandlerFunc{Middleware(tr)}, func(c *breeze.Context) error {
		Tag(c, "order_id", "123")
		captured, _ = stateFrom(c)
		c.Status(200)

		return nil
	})
	ctx.Next()

	// Simulate the recycled state being reused by a later request.
	captured.tags["order_id"] = "999"

	closeTracer(t, tr)

	s := exactlyOne(t, rt.exportedSpans())
	if s.Tags["order_id"] != "123" {
		t.Errorf("Tags[order_id] = %q, want 123 — the span shares its map with the recycled request state", s.Tags["order_id"])
	}
}
