package fleet

// Tests for the propagation primitives (§5.2, §5A.1).
//
// Inject and Extract are the narrowest, most load-bearing pair of functions in
// the package: every trace that spans two services depends on them agreeing, and
// on Inject writing *this* hop's span id into the parent position — the single
// fact that turns a pile of spans into a tree. Get that backwards and every trace
// still looks plausible while being wrong, which is why it is asserted directly
// rather than inferred from an assembled trace.
//
// The other theme is that a broken neighbour must not be able to break this
// service: a garbled header costs the trace's parent link and nothing more.

import (
	"context"
	"testing"

	"github.com/nelthaarion/breeze"
)

// newTestContext returns a bare request context, as breeze hands one to a
// handler. Tests here drive the propagation functions directly rather than
// through a router, so a propagation bug cannot be masked by request plumbing.
func newTestContext() *breeze.Context {
	return breeze.NewContext(breeze.GET, "/")
}

// capturingTransport records whether the Tracer's own transport was consulted for
// propagation, and encodes via InjectInto so it behaves like the real ones.
type capturingTransport struct {
	injected int
	extracts int
}

func (c *capturingTransport) Name() string { return "capturing" }

func (c *capturingTransport) Inject(tc TraceContext, baggage Baggage, carrier Carrier) {
	c.injected++
	InjectInto(tc, baggage, "", carrier)
}

func (c *capturingTransport) Extract(carrier Carrier) (TraceContext, Baggage, bool) {
	c.extracts++
	return ExtractFrom(carrier)
}

func (c *capturingTransport) ExportSpans(context.Context, string, []Span) error { return nil }

func (c *capturingTransport) ExportHeartbeat(context.Context, string, Heartbeat) error { return nil }

// injectedContext returns a context carrying request state as the middleware
// would leave it, plus the state itself so a test can read the span id Inject is
// expected to advertise.
func injectedContext(tr *Tracer, service string) (*breeze.Context, *requestState) {
	ctx := newTestContext()
	tc := NewTraceContext()
	tc.Sampled = true
	st := &requestState{
		tc:          tc,
		spanID:      tc.NewChildSpanID(),
		sampled:     true,
		tr:          tr,
		serviceName: service,
	}
	ctx.Set(ctxStateKey, st)
	return ctx, st
}

// --- MapCarrier ------------------------------------------------------------

func TestMapCarrier(t *testing.T) {
	c := MapCarrier{}

	if _, ok := c.Get("absent"); ok {
		t.Error("Get on an empty carrier reported a value")
	}
	c.Set("traceparent", "value")
	got, ok := c.Get("traceparent")
	if !ok || got != "value" {
		t.Errorf("Get = %q, %v; want value, true", got, ok)
	}
	// Overwrite rather than append: a carrier holds one value per key, and two
	// traceparents on one hop would be ambiguous.
	c.Set("traceparent", "second")
	if got, _ := c.Get("traceparent"); got != "second" {
		t.Errorf("Get after overwrite = %q, want second", got)
	}
}

// --- Inject ----------------------------------------------------------------

// TestInjectAdvertisesThisHopAsParent is the property the whole trace tree rests
// on. The callee must record *this* span as its parent, so the outgoing header
// carries this hop's span id in the parent-span-id position — not the parent id
// this hop itself received.
func TestInjectAdvertisesThisHopAsParent(t *testing.T) {
	ctx, st := injectedContext(nil, "gateway")
	carrier := MapCarrier{}

	Inject(ctx, carrier)

	raw, ok := carrier.Get(HeaderTraceparent)
	if !ok {
		t.Fatal("Inject wrote no traceparent")
	}
	got, ok := ParseTraceparent(raw)
	if !ok {
		t.Fatalf("Inject wrote an unparseable traceparent %q", raw)
	}
	if got.TraceID != st.tc.TraceID {
		t.Error("trace id changed across a hop — the two services would appear in separate traces")
	}
	if got.ParentSpanID != st.spanID {
		t.Error("outgoing parent-span-id is not this hop's span id, so the callee will link to the wrong parent")
	}
	if !got.Sampled {
		t.Error("sampled bit was not carried; the callee would make its own decision and split the trace")
	}
}

// TestInjectDoesNotMutateRequestState — Inject rewrites the parent position for
// the outgoing hop, and doing that in place would corrupt this request's own
// record of who called it, mislinking its span after the second outgoing call.
func TestInjectDoesNotMutateRequestState(t *testing.T) {
	ctx, st := injectedContext(nil, "gateway")
	inheritedParent := st.tc.ParentSpanID
	ownSpan := st.spanID

	Inject(ctx, MapCarrier{})
	Inject(ctx, MapCarrier{})

	if st.tc.ParentSpanID != inheritedParent {
		t.Error("Inject overwrote the inherited parent span id on the request's own state")
	}
	if st.spanID != ownSpan {
		t.Error("Inject changed this hop's span id")
	}
}

// TestInjectCarriesBaggage — §9C.1's tags only reach downstream services if
// baggage rides along with the trace context.
func TestInjectCarriesBaggage(t *testing.T) {
	ctx, st := injectedContext(nil, "gateway")
	st.baggage = Baggage{"order_id": "123"}
	carrier := MapCarrier{}

	Inject(ctx, carrier)

	raw, ok := carrier.Get(HeaderBaggage)
	if !ok {
		t.Fatal("baggage was not written")
	}
	bag, ok := ParseBaggage(raw)
	if !ok || bag["order_id"] != "123" {
		t.Errorf("baggage round-trip = %v, %v; want order_id=123", bag, ok)
	}
}

func TestInjectOmitsEmptyBaggage(t *testing.T) {
	ctx, _ := injectedContext(nil, "gateway")
	carrier := MapCarrier{}

	Inject(ctx, carrier)

	if _, ok := carrier.Get(HeaderBaggage); ok {
		t.Error("an empty baggage header was written; every hop would pay for a value carrying nothing")
	}
}

func TestInjectWritesServiceName(t *testing.T) {
	ctx, _ := injectedContext(nil, "gateway")
	carrier := MapCarrier{}

	Inject(ctx, carrier)

	if got, _ := carrier.Get(HeaderService); got != "gateway" {
		t.Errorf("%s = %q, want gateway", HeaderService, got)
	}
}

// TestInjectDelegatesToTheConfiguredTransport — §5A.1: the package-level function
// is the API, but the encoding belongs to the active transport, so a transport
// with its own header conventions is actually consulted.
func TestInjectDelegatesToTheConfiguredTransport(t *testing.T) {
	ct := &capturingTransport{}
	tr := New(TracerConfig{
		Enabled:       true,
		ServiceName:   "gateway",
		AggregatorURL: "http://aggregator",
		Transport:     ct,
	})
	defer func() {
		if err := tr.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	ctx, _ := injectedContext(tr, "gateway")
	carrier := MapCarrier{}

	Inject(ctx, carrier)

	if ct.injected != 1 {
		t.Errorf("transport.Inject called %d times, want 1", ct.injected)
	}
	if _, ok := carrier.Get(HeaderTraceparent); !ok {
		t.Error("no traceparent written")
	}
	// The service header is Inject's own responsibility, so a transport that
	// only encodes trace context still identifies its service.
	if got, _ := carrier.Get(HeaderService); got != "gateway" {
		t.Errorf("%s = %q, want gateway", HeaderService, got)
	}
}

// TestInjectWithoutATracerStillPropagates is what keeps a partially-configured
// service from silently breaking traces that pass through it. Every transport
// writes traceparent identically, so the fallback encoding is correct, not a
// degraded guess.
func TestInjectWithoutATracerStillPropagates(t *testing.T) {
	ctx, st := injectedContext(nil, "gateway")
	carrier := MapCarrier{}

	Inject(ctx, carrier)

	raw, _ := carrier.Get(HeaderTraceparent)
	got, ok := ParseTraceparent(raw)
	if !ok {
		t.Fatalf("no usable traceparent from a tracer-less state: %q", raw)
	}
	if got.TraceID != st.tc.TraceID || got.ParentSpanID != st.spanID {
		t.Error("fallback encoding does not match what a configured transport writes")
	}
}

// TestInjectIsSafeWithoutATrace covers every way a call site can be untraced.
// Propagation calls are written once and then run on every request, including in
// services where tracing is off — none of these may panic or write anything.
func TestInjectIsSafeWithoutATrace(t *testing.T) {
	carrier := MapCarrier{}

	Inject(nil, carrier)
	Inject(newTestContext(), carrier) // no middleware ran

	untyped := newTestContext()
	untyped.Set(ctxStateKey, "not a requestState")
	Inject(untyped, carrier)

	if len(carrier) != 0 {
		t.Errorf("carrier = %v, want nothing written without an active trace", carrier)
	}

	// A nil carrier is the other half: a call site that built one from a nil
	// map, or a transport with nothing to inject into.
	ctx, _ := injectedContext(nil, "gateway")
	Inject(ctx, nil)
	InjectInto(NewTraceContext(), nil, "svc", nil)
}

// --- Extract ---------------------------------------------------------------

func TestExtractRoundTrip(t *testing.T) {
	tc := NewTraceContext()
	tc.Sampled = true
	bag := Baggage{"order_id": "123", "user_id": "u-9"}
	carrier := MapCarrier{}

	InjectInto(tc, bag, "gateway", carrier)
	got, gotBag, ok := Extract(carrier)

	if !ok {
		t.Fatal("Extract rejected what InjectInto wrote")
	}
	if got.TraceID != tc.TraceID {
		t.Error("trace id did not survive the round trip")
	}
	if got.ParentSpanID != tc.ParentSpanID {
		t.Error("parent span id did not survive the round trip")
	}
	if !got.Sampled {
		t.Error("sampled bit lost")
	}
	if gotBag["order_id"] != "123" || gotBag["user_id"] != "u-9" {
		t.Errorf("baggage = %v, want both keys", gotBag)
	}
}

// TestExtractMissingHeaderIsNotAnError — a request from outside the fleet has no
// traceparent, which is entirely normal and must not be counted as malformed, or
// the malformed counter would just measure inbound public traffic.
func TestExtractMissingHeaderIsNotAnError(t *testing.T) {
	before := ReadMetrics().MalformedHeaderTotal

	_, _, ok := Extract(MapCarrier{})
	if ok {
		t.Error("Extract reported success with no header present")
	}
	// An empty value is the same case: some proxies forward the key with
	// nothing in it.
	if _, _, ok := Extract(MapCarrier{HeaderTraceparent: ""}); ok {
		t.Error("Extract accepted an empty traceparent")
	}
	if got := ReadMetrics().MalformedHeaderTotal; got != before {
		t.Errorf("malformed counter moved from %d to %d for an absent header", before, got)
	}
}

// TestExtractMalformedHeaderIsCounted — §4.1: a broken header must not propagate
// a broken trace, and must be visible on the Performance page so the upstream
// emitting it can be found.
func TestExtractMalformedHeaderIsCounted(t *testing.T) {
	before := ReadMetrics().MalformedHeaderTotal

	_, _, ok := Extract(MapCarrier{HeaderTraceparent: "this is not a traceparent"})
	if ok {
		t.Error("Extract accepted a malformed traceparent")
	}
	if got := ReadMetrics().MalformedHeaderTotal; got <= before {
		t.Error("a malformed traceparent was dropped without incrementing fleet_malformed_header_total")
	}
}

// TestExtractBrokenBaggageKeepsTheTrace is the §4.1 degradation rule: a broken
// tag costs the tags, never the trace it is attached to.
func TestExtractBrokenBaggageKeepsTheTrace(t *testing.T) {
	tc := NewTraceContext()
	carrier := MapCarrier{}
	InjectInto(tc, nil, "gateway", carrier)
	carrier.Set(HeaderBaggage, "=====")

	got, bag, ok := Extract(carrier)
	if !ok {
		t.Fatal("broken baggage caused the whole extraction to fail")
	}
	if got.TraceID != tc.TraceID {
		t.Error("trace id lost alongside the bad baggage")
	}
	if len(bag) != 0 {
		t.Errorf("baggage = %v, want empty", bag)
	}
}

func TestExtractNilCarrier(t *testing.T) {
	if _, _, ok := Extract(nil); ok {
		t.Error("Extract(nil) reported success")
	}
	if _, _, ok := ExtractFrom(nil); ok {
		t.Error("ExtractFrom(nil) reported success")
	}
}

// TestExtractIgnoresAllZeroIDs — all-zero ids are invalid per W3C and would
// produce a trace every service shares.
func TestExtractIgnoresAllZeroIDs(t *testing.T) {
	zeros := "00-00000000000000000000000000000000-0000000000000000-01"
	if _, _, ok := Extract(MapCarrier{HeaderTraceparent: zeros}); ok {
		t.Error("an all-zero traceparent was accepted")
	}
}

// TestInjectExtractIsATwoHopChain simulates what actually happens across a
// service boundary: A injects, B extracts, and B's parent must be A's span.
func TestInjectExtractIsATwoHopChain(t *testing.T) {
	// Hop A.
	ctxA, stA := injectedContext(nil, "gateway")
	wire := MapCarrier{}
	Inject(ctxA, wire)

	// Hop B receives it.
	tcB, _, ok := Extract(wire)
	if !ok {
		t.Fatal("hop B could not extract what hop A injected")
	}
	if tcB.TraceID != stA.tc.TraceID {
		t.Error("hop B joined a different trace")
	}
	if tcB.ParentSpanID != stA.spanID {
		t.Error("hop B's parent is not hop A's span")
	}

	// Hop B injects onward to C: same trace, B's own span as parent.
	stB := &requestState{
		tc:          tcB,
		spanID:      tcB.NewChildSpanID(),
		sampled:     tcB.Sampled,
		serviceName: "orders",
	}
	ctxB := newTestContext()
	ctxB.Set(ctxStateKey, stB)

	onward := MapCarrier{}
	Inject(ctxB, onward)

	raw, _ := onward.Get(HeaderTraceparent)
	tcC, ok := ParseTraceparent(raw)
	if !ok {
		t.Fatal("hop C received an unparseable traceparent")
	}
	if tcC.TraceID != stA.tc.TraceID {
		t.Error("the trace id did not survive two hops")
	}
	if tcC.ParentSpanID != stB.spanID {
		t.Error("hop C's parent is not hop B's span")
	}
	if tcC.ParentSpanID == stA.spanID {
		t.Error("hop C was told its parent is A, which would flatten the tree")
	}
	if got, _ := onward.Get(HeaderService); got != "orders" {
		t.Errorf("%s = %q, want orders", HeaderService, got)
	}
}

// TestInjectIntoOmitsEmptyService — the sub-package entry point must not write an
// empty service header for a transport that has no name to report.
func TestInjectIntoOmitsEmptyService(t *testing.T) {
	carrier := MapCarrier{}
	InjectInto(NewTraceContext(), nil, "", carrier)

	if _, ok := carrier.Get(HeaderService); ok {
		t.Error("an empty service header was written")
	}
	if _, ok := carrier.Get(HeaderTraceparent); !ok {
		t.Error("traceparent missing")
	}
}

// TestUnsampledContextPropagatesTheDecision — §7: the root decides, every hop
// honours it. Losing the flag downstream would produce half-sampled traces.
func TestUnsampledContextPropagatesTheDecision(t *testing.T) {
	tc := NewTraceContext()
	tc.Sampled = false
	carrier := MapCarrier{}

	InjectInto(tc, nil, "gateway", carrier)
	got, _, ok := Extract(carrier)

	if !ok {
		t.Fatal("Extract failed")
	}
	if got.Sampled {
		t.Error("an unsampled trace arrived sampled, so downstream would capture timelines the root declined")
	}
}
