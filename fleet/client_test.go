package fleet

// Tests for the HTTP propagation sugar (§5.2).
//
// These functions are thin by design — PropagateFromHTTP is one Inject call, and
// Client.Do is that plus a send. What is worth testing is not the injection logic
// (that is transport_test.go's job) but the wrapper's edges: a hand-built request
// with a nil header map, a zero-value Client, a nil Client, and the promise that
// Do and PropagateFromHTTP produce byte-identical headers. That last one is the
// reason both APIs can be documented without warning users they might diverge.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// --- HTTPHeaderCarrier -----------------------------------------------------

func TestHTTPHeaderCarrier(t *testing.T) {
	h := http.Header{}
	c := HTTPHeaderCarrier(h)

	if _, ok := c.Get(HeaderTraceparent); ok {
		t.Error("Get on an empty header reported a value")
	}
	c.Set(HeaderTraceparent, "value")
	if got, ok := c.Get(HeaderTraceparent); !ok || got != "value" {
		t.Errorf("Get = %q, %v; want value, true", got, ok)
	}
}

// TestHTTPHeaderCarrierIsCaseInsensitive — http.Header canonicalises keys, so a
// value written as "traceparent" must be readable however it is spelled. Go's own
// server canonicalises inbound keys, so a case-sensitive lookup here would fail
// on every real request.
func TestHTTPHeaderCarrierIsCaseInsensitive(t *testing.T) {
	h := http.Header{}
	HTTPHeaderCarrier(h).Set(HeaderTraceparent, "value")

	for _, spelling := range []string{"traceparent", "Traceparent", "TRACEPARENT"} {
		if got, ok := HTTPHeaderCarrier(h).Get(spelling); !ok || got != "value" {
			t.Errorf("Get(%q) = %q, %v; want value, true", spelling, got, ok)
		}
	}
}

// TestHTTPHeaderCarrierNilHeader — a &http.Request{} built by hand has no header
// map, and tracing must not be the thing that panics on it.
func TestHTTPHeaderCarrierNilHeader(t *testing.T) {
	c := HTTPHeaderCarrier(nil)

	c.Set(HeaderTraceparent, "value") // must not panic
	if _, ok := c.Get(HeaderTraceparent); ok {
		t.Error("Get on a nil header reported a value")
	}
}

// --- PropagateFromHTTP -----------------------------------------------------

func TestPropagateFromHTTP(t *testing.T) {
	ctx, st := injectedContext(nil, "gateway")
	req, err := http.NewRequest(http.MethodPost, "http://orders/orders", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	PropagateFromHTTP(ctx, req)

	tc, ok := ParseTraceparent(req.Header.Get(HeaderTraceparent))
	if !ok {
		t.Fatalf("no usable traceparent on the outgoing request: %q", req.Header.Get(HeaderTraceparent))
	}
	if tc.TraceID != st.tc.TraceID {
		t.Error("outgoing request joined a different trace")
	}
	if tc.ParentSpanID != st.spanID {
		t.Error("callee will link to the wrong parent")
	}
	if got := req.Header.Get(HeaderService); got != "gateway" {
		t.Errorf("%s = %q, want gateway", HeaderService, got)
	}
}

// TestPropagateFromHTTPPopulatesNilHeaderMap — a hand-built request is a normal
// thing to have, and injecting into its nil map would panic on a request that
// would otherwise have worked. Tracing must never be what breaks a call.
func TestPropagateFromHTTPPopulatesNilHeaderMap(t *testing.T) {
	ctx, _ := injectedContext(nil, "gateway")
	req := &http.Request{} // no Header

	PropagateFromHTTP(ctx, req)

	if req.Header == nil {
		t.Fatal("Header still nil")
	}
	if req.Header.Get(HeaderTraceparent) == "" {
		t.Error("no traceparent written")
	}
}

// TestPropagateFromHTTPIsSafeWithoutATrace — these calls sit on the request path
// of services where tracing may be off, so every degenerate combination must be
// a no-op rather than a panic.
func TestPropagateFromHTTPIsSafeWithoutATrace(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "http://orders/", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	PropagateFromHTTP(nil, req)              // no context
	PropagateFromHTTP(newTestContext(), req) // context, but no middleware ran
	PropagateFromHTTP(nil, nil)              // nothing at all
	PropagateFromHTTP(newTestContext(), nil) // context, no request

	if got := req.Header.Get(HeaderTraceparent); got != "" {
		t.Errorf("traceparent = %q, want nothing written without an active trace", got)
	}
}

// TestPropagateFromHTTPOverwritesAStaleHeader — a retried or cloned request may
// already carry a traceparent from a previous attempt. Leaving it would attribute
// the retry to the old span.
func TestPropagateFromHTTPOverwritesAStaleHeader(t *testing.T) {
	ctx, st := injectedContext(nil, "gateway")
	req, err := http.NewRequest(http.MethodGet, "http://orders/", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	stale := NewTraceContext()
	req.Header.Set(HeaderTraceparent, stale.String())

	PropagateFromHTTP(ctx, req)

	values := req.Header.Values(HeaderTraceparent)
	if len(values) != 1 {
		t.Fatalf("traceparent appears %d times, want exactly 1 — a duplicated header is ambiguous", len(values))
	}
	tc, ok := ParseTraceparent(values[0])
	if !ok {
		t.Fatalf("unparseable traceparent %q", values[0])
	}
	if tc.TraceID == stale.TraceID {
		t.Error("the stale trace id survived, so this call is attributed to the wrong trace")
	}
	if tc.TraceID != st.tc.TraceID {
		t.Error("outgoing trace id is neither the stale one nor the current one")
	}
}

// --- Client ----------------------------------------------------------------

// TestClientDoPropagates asserts the whole convenience path end to end against a
// real server, so what the callee would actually receive is what is checked.
func TestClientDoPropagates(t *testing.T) {
	var gotTraceparent, gotService string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTraceparent = r.Header.Get(HeaderTraceparent)
		gotService = r.Header.Get(HeaderService)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, st := injectedContext(nil, "gateway")
	client := WrapClient(srv.Client(), ctx)

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	tc, ok := ParseTraceparent(gotTraceparent)
	if !ok {
		t.Fatalf("callee received no usable traceparent: %q", gotTraceparent)
	}
	if tc.TraceID != st.tc.TraceID {
		t.Error("callee joined a different trace")
	}
	if tc.ParentSpanID != st.spanID {
		t.Error("callee's parent is not the calling hop's span")
	}
	if gotService != "gateway" {
		t.Errorf("%s = %q, want gateway", HeaderService, gotService)
	}
}

// TestClientDoAndPropagateFromHTTPAgree is the anti-drift check §5.2 asks for:
// the wrapper must be the generic path plus a send, not a second implementation.
func TestClientDoAndPropagateFromHTTPAgree(t *testing.T) {
	var viaClient http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		viaClient = r.Header.Clone()
	}))
	defer srv.Close()

	ctx, _ := injectedContext(nil, "gateway")

	resp, err := WrapClient(srv.Client(), ctx).Get(srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	_ = resp.Body.Close()

	manual, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	PropagateFromHTTP(ctx, manual)

	// Span ids are stable for a request, so both paths must produce the same
	// value — not merely both produce something parseable.
	if got, want := viaClient.Get(HeaderTraceparent), manual.Header.Get(HeaderTraceparent); got != want {
		t.Errorf("Client.Do wrote %q, PropagateFromHTTP wrote %q — the two paths have diverged", got, want)
	}
	if got, want := viaClient.Get(HeaderService), manual.Header.Get(HeaderService); got != want {
		t.Errorf("%s: Client.Do wrote %q, PropagateFromHTTP wrote %q", HeaderService, got, want)
	}
}

// TestClientZeroValueUsesDefaultClient — the documented "a zero Client is usable"
// claim. It sends with http.DefaultClient and propagates nothing, since it has no
// request context to propagate from.
func TestClientZeroValueUsesDefaultClient(t *testing.T) {
	reached := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		if got := r.Header.Get(HeaderTraceparent); got != "" {
			t.Errorf("traceparent = %q, want none from a context-less client", got)
		}
	}))
	defer srv.Close()

	var c Client
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	_ = resp.Body.Close()

	if !reached {
		t.Error("the request never arrived")
	}
}

// TestNilClientStillSends — a nil *Client is what a service holds when its setup
// code returned nothing. It must degrade to an untraced call rather than panic:
// losing a trace is acceptable, losing the request is not.
func TestNilClientStillSends(t *testing.T) {
	reached := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))
	defer srv.Close()

	var c *Client
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("Get on a nil client: %v", err)
	}
	_ = resp.Body.Close()

	if !reached {
		t.Error("a nil client dropped the request instead of sending it untraced")
	}
}

// TestClientGetPropagatesRequestErrors — a malformed URL must surface as the
// error http.NewRequest produced, not be swallowed into a nil response the caller
// then dereferences.
func TestClientGetPropagatesRequestErrors(t *testing.T) {
	ctx, _ := injectedContext(nil, "gateway")
	client := WrapClient(http.DefaultClient, ctx)

	resp, err := client.Get("http://[::1]:namedport/")
	if err == nil {
		if resp != nil {
			_ = resp.Body.Close()
		}
		t.Fatal("a malformed URL produced no error")
	}
	if resp != nil {
		t.Error("both a response and an error were returned")
	}
}

// TestClientDoWithoutATraceStillSends — tracing disabled must not stop traffic.
// This is the same guarantee as the nil client, reached a different way: a
// perfectly good client whose context has no trace on it.
func TestClientDoWithoutATraceStillSends(t *testing.T) {
	reached := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		if got := r.Header.Get(HeaderTraceparent); got != "" {
			t.Errorf("traceparent = %q, want none when tracing is off", got)
		}
	}))
	defer srv.Close()

	client := WrapClient(srv.Client(), newTestContext())
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	_ = resp.Body.Close()

	if !reached {
		t.Error("the request never arrived")
	}
}
