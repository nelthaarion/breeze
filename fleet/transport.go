package fleet

// Propagation is the contract that makes a fleet's traces join up, and this file
// owns it: the transport-agnostic Carrier, the Transport interface each concrete
// transport implements (§5A.1), and the two package-level functions application
// code actually calls — Inject and Extract (§5.2).
//
// # Why a Carrier at all
//
// Trace context has to cross whatever a developer happens to call the next
// service over: an HTTP header map, gRPC metadata, an event envelope, or
// something invented in-house. Every one of those is "a bag of string keys and
// string values" and nothing more, so that is the entire interface. A protocol
// this package has never heard of becomes traceable by writing four lines:
//
//	type MyCarrier struct{ m map[string]string }
//	func (c MyCarrier) Set(k, v string)             { c.m[k] = v }
//	func (c MyCarrier) Get(k string) (string, bool) { v, ok := c.m[k]; return v, ok }
//	fleet.Inject(ctx, MyCarrier{m: myProtocolHeaders})
//
// # Propagation is per-call and explicit, by design
//
// Nothing here hooks net/http, monkey-patches a client, or installs a global
// interceptor. A developer names each outgoing call they want traced. That is a
// deliberate trade: slightly more code at the call site, in exchange for it
// always being obvious from reading a handler which calls are traced and which
// are not. Forgetting a call is not an error — the downstream service simply
// starts a new root trace.

import (
	"context"

	"github.com/nelthaarion/breeze/v2"
)

// Header keys used by every transport.
//
// The traceparent name is fixed by W3C and must be spelled exactly this way; an
// OTel-aware proxy anywhere in a mixed stack reads the same key.
const (
	// HeaderTraceparent carries trace id, parent span id and flags (§4).
	HeaderTraceparent = "traceparent"

	// HeaderService names the sending service. Strictly redundant — the
	// aggregator learns service names from spans — but it makes a captured
	// request readable in isolation, e.g. in a proxy log or a tcpdump,
	// without a lookup against the trace store.
	HeaderService = "x-breeze-service"

	// HeaderBaggage carries the tag key/values from §9C.1.
	//
	// Deliberately *not* the W3C `tracestate` header, despite the format
	// being tracestate-style. tracestate has assigned semantics — vendor-
	// prefixed keys, ordering rules that a compliant intermediary is
	// entitled to rewrite — so putting application tags in it invites a
	// well-behaved OTel component to reorder or drop them, and invites us to
	// clobber sampling state some other vendor put there. A separate,
	// unambiguously-ours header cannot collide with either.
	HeaderBaggage = "x-breeze-baggage"

	// HeaderIngestToken carries the aggregator's write credential (§11.1).
	//
	// Separate from the dashboard's Basic Auth on purpose: services pushing
	// spans need a write credential, humans reading traces need a read one,
	// and giving a service the viewer password so it can export would mean
	// every service in the fleet holds the credential a person logs in with.
	HeaderIngestToken = "x-fleet-token"
)

// Carrier is any bag of string keys and values that trace context can be
// written to or read from.
//
// Get returns ok=false for a missing key; implementations must not panic on
// unknown keys, since Extract probes for headers that are usually absent.
type Carrier interface {
	Set(key, value string)
	Get(key string) (string, bool)
}

// MapCarrier adapts a plain string map.
//
// This is the fallback for every protocol without a purpose-built adapter — an
// event envelope's metadata, a broker's message attributes, an in-house RPC's
// header bag. Having one generic adapter is why this package does not need a
// bespoke carrier type per transport.
type MapCarrier map[string]string

func (c MapCarrier) Set(key, value string) { c[key] = value }

func (c MapCarrier) Get(key string) (string, bool) {
	v, ok := c[key]
	return v, ok
}

// Transport is one way of moving trace context and spans between processes.
//
// Two responsibilities, deliberately together: how trace context is encoded on
// this transport's own carrier, and how a batch of spans reaches the aggregator
// over it. They belong in one interface because they must agree — a transport
// that injected a header its own aggregator endpoint could not read would
// produce traces that look propagated but never assemble.
//
// Application code does not call these. It calls the package-level Inject and
// Extract below, which delegate to whichever Transport the active Tracer holds.
type Transport interface {
	// Name identifies the transport in logs and metrics ("http", "ws",
	// "events", "gnet", "grpc").
	Name() string

	// Inject writes trace context and baggage onto c.
	Inject(tc TraceContext, baggage Baggage, c Carrier)

	// Extract reads trace context and baggage back off c. ok is false when
	// there is nothing usable to read, which the caller must treat as "start
	// a new root trace", never as an error.
	Extract(c Carrier) (TraceContext, Baggage, bool)

	// ExportSpans delivers a batch to the aggregator. Called only from the
	// Tracer's background goroutine, never from a request, so a slow
	// implementation delays export but cannot delay a response.
	//
	// Returning an error is normal and expected (the aggregator restarts,
	// the network blips); the Tracer retries with backoff and drops oldest
	// spans rather than growing without bound.
	ExportSpans(ctx context.Context, aggregatorAddr string, spans []Span) error

	// ExportHeartbeat delivers one heartbeat (§8.1.2).
	ExportHeartbeat(ctx context.Context, aggregatorAddr string, hb Heartbeat) error
}

// Inject writes the current request's trace context onto c, so the service being
// called joins this trace instead of starting its own.
//
//	req, _ := http.NewRequest("POST", url, body)
//	fleet.Inject(ctx, fleet.HTTPHeaderCarrier(req.Header))
//
// The span id written is *this* hop's, which becomes the callee's parent — that
// single fact is what turns a pile of independent spans into a tree.
//
// Safe to call unconditionally. With tracing disabled, no middleware installed,
// or a nil ctx, it writes nothing and returns; call sites never need a guard.
func Inject(ctx *breeze.Context, c Carrier) {

	if c == nil {
		return
	}
	st, ok := stateFrom(ctx)
	if !ok {
		return
	}

	// The context handed to the transport describes the *outgoing* hop: same
	// trace, but with this hop's span id in the parent position, since that
	// is what the callee will record as its parent. Copying by value here is
	// what keeps that rewrite from corrupting the state's own view of who
	// called *us*.
	out := TraceContext{
		TraceID:      st.tc.TraceID,
		ParentSpanID: st.spanID,
		Sampled:      st.sampled,
	}

	tr := st.tr
	if tr == nil || tr.transport == nil {
		// No tracer means no configured encoding. Falling back to the
		// standard header spelling still propagates the trace correctly,
		// because every transport in this package writes traceparent
		// identically — only span *export* differs between them.
		defaultInject(out, st.baggage, st.serviceName, c)
		return
	}
	tr.transport.Inject(out, st.baggage, c)
	if st.serviceName != "" {
		c.Set(HeaderService, st.serviceName)
	}
}

// Extract reads trace context and baggage off c.
//
// Called by fleet.Middleware on the receiving side; most developers never call
// it directly. ok is false when there is nothing usable, which is not an error —
// it means this service is the edge and should start a new root trace.
func Extract(c Carrier) (TraceContext, Baggage, bool) {
	return defaultExtract(c)
}

// InjectInto writes trace context, baggage and service name onto c using the
// standard header encoding.
//
// Exported for the transport sub-packages under fleet/transport/, which live in
// their own packages (so that importing fleet never pulls in gRPC or a broker
// client) and therefore cannot reach the unexported implementation. Every
// built-in transport's Inject is a one-line call to this, which is precisely why
// a fleet can mix transports and still assemble traces: the propagation encoding
// has exactly one implementation, and only span *export* varies.
//
// Application code should call Inject instead — it takes the trace context from
// the request rather than requiring the caller to assemble one.
func InjectInto(tc TraceContext, baggage Baggage, service string, c Carrier) {
	if c == nil {
		return
	}
	defaultInject(tc, baggage, service, c)
}

// ExtractFrom is InjectInto's inverse, exported for the same reason.
//
// Malformed input degrades to ok=false (start a new root trace) and increments
// the malformed-header counter; it never returns an error, because a broken
// header from a neighbour must not stop this service from tracing.
func ExtractFrom(c Carrier) (TraceContext, Baggage, bool) {
	return defaultExtract(c)
}

// defaultInject is the one implementation of "write trace context onto a bag of
// strings", shared by every transport that has no reason to differ.
//
// Every built-in transport delegates here rather than formatting the header
// itself. That is what makes the transports genuinely interchangeable: a service
// exporting spans over WebSocket still emits byte-identical propagation headers
// to one exporting over HTTP, so mixing them in a fleet assembles correctly.
func defaultInject(tc TraceContext, baggage Baggage, service string, c Carrier) {
	c.Set(HeaderTraceparent, tc.String())
	if len(baggage) > 0 {
		if s := baggage.String(); s != "" {
			c.Set(HeaderBaggage, s)
		}
	}
	if service != "" {
		c.Set(HeaderService, service)
	}
}

// defaultExtract is defaultInject's inverse.
//
// Malformed input degrades rather than failing: an unparseable traceparent is
// counted and reported as "nothing usable" so the caller starts a clean root
// trace, and broken baggage costs only the baggage. A neighbour's bug must never
// be able to stop this service from tracing.
func defaultExtract(c Carrier) (TraceContext, Baggage, bool) {
	if c == nil {
		return TraceContext{}, nil, false
	}
	raw, ok := c.Get(HeaderTraceparent)
	if !ok || raw == "" {
		// No header at all is the normal edge case — a request arriving
		// from outside the fleet. Not counted as malformed, because
		// there is nothing wrong with it.
		return TraceContext{}, nil, false
	}
	tc, ok := ParseTraceparent(raw)
	if !ok {
		noteMalformedHeader()
		return TraceContext{}, nil, false
	}
	var bag Baggage
	if rawBag, ok := c.Get(HeaderBaggage); ok && rawBag != "" {
		// A failed baggage parse is intentionally ignored: ParseBaggage
		// already drops individual bad entries and keeps the good ones,
		// so reaching here means the whole value was unusable. Losing
		// tags is survivable; losing the trace is not.
		bag, _ = ParseBaggage(rawBag)
	}
	return tc, bag, true
}
