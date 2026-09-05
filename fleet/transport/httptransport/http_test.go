package httptransport

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nelthaarion/breeze/v2/fleet"
)

// receive captures what the transport actually put on the wire, so the tests
// assert the observable payload rather than the transport's internal state.
type received struct {
	path        string
	token       string
	service     string
	contentType string
	encoding    string
	body        []byte
}

// newTestServer returns a server that decodes gzip when present, so a test can
// assert on the JSON regardless of whether compression kicked in.
func newTestServer(t *testing.T, status int, out chan<- received) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body io.Reader = r.Body
		if r.Header.Get("Content-Encoding") == "gzip" {
			zr, err := gzip.NewReader(r.Body)
			if err != nil {
				t.Errorf("server: gzip reader: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			defer zr.Close()
			body = zr
		}
		b, _ := io.ReadAll(body)
		out <- received{
			path:        r.URL.Path,
			token:       r.Header.Get(fleet.HeaderIngestToken),
			service:     r.Header.Get(fleet.HeaderService),
			contentType: r.Header.Get("Content-Type"),
			encoding:    r.Header.Get("Content-Encoding"),
			body:        b,
		}
		w.WriteHeader(status)
	}))
}

func TestExportSpansPostsBatch(t *testing.T) {
	got := make(chan received, 1)
	srv := newTestServer(t, http.StatusOK, got)
	defer srv.Close()

	tr := New(Config{
		ServiceName: "orders-service",
		IngestToken: "s3cret",
	})

	spans := []fleet.Span{{
		TraceID: strings.Repeat("a", 32),
		SpanID:  strings.Repeat("b", 16),
		Service: "orders-service",
		Route:   "/orders/:id",
		Method:  "GET",
		Status:  200,
	}}
	if err := tr.ExportSpans(context.Background(), srv.URL, spans); err != nil {
		t.Fatalf("ExportSpans: %v", err)
	}

	r := <-got
	if r.path != PathSpans {
		t.Errorf("path = %q, want %q", r.path, PathSpans)
	}
	// The write credential must be on every ingest request, not just the
	// first: an aggregator with a token configured rejects anything without
	// it, so a transport that sent it inconsistently would fail intermittently.
	if r.token != "s3cret" {
		t.Errorf("ingest token = %q, want %q", r.token, "s3cret")
	}
	if r.service != "orders-service" {
		t.Errorf("service header = %q, want %q", r.service, "orders-service")
	}

	var batch []fleet.Span
	if err := json.Unmarshal(r.body, &batch); err != nil {
		t.Fatalf("decode span array: %v (body=%s)", err, r.body)
	}
	if len(batch) != 1 || batch[0].Route != "/orders/:id" {
		t.Fatalf("batch = %+v, want one span for /orders/:id", batch)
	}
}

func TestExportHeartbeatPostsToHeartbeatPath(t *testing.T) {
	got := make(chan received, 1)
	srv := newTestServer(t, http.StatusOK, got)
	defer srv.Close()

	tr := New(Config{ServiceName: "auth-service"})
	hb := fleet.Heartbeat{Service: "auth-service", InstanceID: "abc123", RPS: 12.5}
	if err := tr.ExportHeartbeat(context.Background(), srv.URL, hb); err != nil {
		t.Fatalf("ExportHeartbeat: %v", err)
	}

	r := <-got
	if r.path != PathHeartbeat {
		t.Errorf("path = %q, want %q", r.path, PathHeartbeat)
	}
	// A heartbeat is small by construction, so compressing it would cost CPU
	// and framing for no gain.
	if r.encoding != "" {
		t.Errorf("Content-Encoding = %q, want none for a heartbeat", r.encoding)
	}
	var out fleet.Heartbeat
	if err := json.Unmarshal(r.body, &out); err != nil {
		t.Fatalf("decode heartbeat: %v", err)
	}
	if out.Service != "auth-service" || out.InstanceID != "abc123" {
		t.Errorf("heartbeat = %+v, want service/instance preserved", out)
	}
}

func TestExportSpansGzipsLargeBatches(t *testing.T) {
	got := make(chan received, 1)
	srv := newTestServer(t, http.StatusOK, got)
	defer srv.Close()

	tr := NewWithGzip(Config{ServiceName: "gateway"})

	// Enough spans to clear gzipMinBytes comfortably.
	spans := make([]fleet.Span, 100)
	for i := range spans {
		spans[i] = fleet.Span{
			TraceID: strings.Repeat("a", 32),
			SpanID:  strings.Repeat("b", 16),
			Service: "gateway",
			Route:   "/some/reasonably/long/route/pattern/:id",
			Method:  "POST",
			Status:  200,
		}
	}
	if err := tr.ExportSpans(context.Background(), srv.URL, spans); err != nil {
		t.Fatalf("ExportSpans: %v", err)
	}

	r := <-got
	if r.encoding != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip for a large batch", r.encoding)
	}
	// The server decompressed it, so a successful decode here proves the
	// compressed bytes were valid gzip and not a truncated buffer — the exact
	// failure a mishandled pooled writer would produce.
	var batch []fleet.Span
	if err := json.Unmarshal(r.body, &batch); err != nil {
		t.Fatalf("decode gzipped span array: %v", err)
	}
	if len(batch) != len(spans) {
		t.Errorf("got %d spans, want %d", len(batch), len(spans))
	}
}

func TestExportSpansEmptyBatchSendsNothing(t *testing.T) {
	got := make(chan received, 1)
	srv := newTestServer(t, http.StatusOK, got)
	defer srv.Close()

	tr := New(Config{ServiceName: "svc"})
	if err := tr.ExportSpans(context.Background(), srv.URL, nil); err != nil {
		t.Fatalf("ExportSpans(nil): %v", err)
	}
	select {
	case r := <-got:
		t.Fatalf("empty batch sent a request to %q, want none", r.path)
	default:
	}
}

func TestExportErrorClassification(t *testing.T) {
	// Retrying a 401 or a 400 produces the same failure forever, so those
	// spans should be dropped with a clear error rather than occupying the
	// buffer; a 503 is precisely what backoff is for. Getting this backwards
	// means either losing recoverable data or retrying a misconfiguration
	// until the buffer overflows.
	cases := []struct {
		status        int
		wantPermanent bool
	}{
		{http.StatusBadRequest, true},
		{http.StatusUnauthorized, true},
		{http.StatusForbidden, true},
		{http.StatusNotFound, true},
		{http.StatusRequestEntityTooLarge, true},
		{http.StatusTooManyRequests, false},
		{http.StatusServiceUnavailable, false},
		{http.StatusInternalServerError, false},
		{http.StatusBadGateway, false},
	}
	for _, tc := range cases {
		got := make(chan received, 1)
		srv := newTestServer(t, tc.status, got)

		tr := New(Config{ServiceName: "svc"})
		err := tr.ExportSpans(context.Background(), srv.URL, []fleet.Span{{
			TraceID: strings.Repeat("a", 32),
			SpanID:  strings.Repeat("b", 16),
		}})
		<-got
		srv.Close()

		if err == nil {
			t.Errorf("status %d: got nil error, want failure", tc.status)
			continue
		}
		ee, ok := err.(*ExportError)
		if !ok {
			t.Errorf("status %d: error type %T, want *ExportError", tc.status, err)
			continue
		}
		if ee.Permanent() != tc.wantPermanent {
			t.Errorf(
				"status %d: Permanent() = %v, want %v",
				tc.status,
				ee.Permanent(),
				tc.wantPermanent,
			)
		}
	}
}

func TestExportSpansUnreachableAggregatorReturnsError(t *testing.T) {
	// Closing immediately gives a URL nothing is listening on, which is what
	// a restarting or misconfigured aggregator looks like. The requirement is
	// only that it reports an error rather than blocking or panicking — the
	// Tracer's backoff handles the rest.
	srv := newTestServer(t, http.StatusOK, make(chan received, 1))
	url := srv.URL
	srv.Close()

	tr := New(Config{ServiceName: "svc"})
	err := tr.ExportSpans(context.Background(), url, []fleet.Span{{
		TraceID: strings.Repeat("a", 32),
		SpanID:  strings.Repeat("b", 16),
	}})
	if err == nil {
		t.Fatal("export to a closed server returned nil, want an error")
	}
}

func TestInjectExtractRoundTrip(t *testing.T) {
	tr := New(Config{ServiceName: "gateway"})

	tc := fleet.NewTraceContext()
	tc.Sampled = true
	bag := fleet.Baggage{"order_id": "123", "user_id": "u-7"}

	carrier := fleet.MapCarrier{}
	tr.Inject(tc, bag, carrier)

	gotTC, gotBag, ok := tr.Extract(carrier)
	if !ok {
		t.Fatal("Extract after Inject returned ok=false")
	}
	if gotTC.TraceIDHex() != tc.TraceIDHex() {
		t.Errorf("trace id = %s, want %s", gotTC.TraceIDHex(), tc.TraceIDHex())
	}
	if gotTC.ParentSpanID != tc.ParentSpanID {
		t.Errorf("parent span id = %x, want %x", gotTC.ParentSpanID, tc.ParentSpanID)
	}
	if !gotTC.Sampled {
		t.Error("sampled flag lost in round trip")
	}
	for k, want := range bag {
		if gotBag[k] != want {
			t.Errorf("baggage[%q] = %q, want %q", k, gotBag[k], want)
		}
	}
	if carrier[fleet.HeaderService] != "gateway" {
		t.Errorf("service header = %q, want %q", carrier[fleet.HeaderService], "gateway")
	}
}

func TestExtractMalformedDegradesToNoContext(t *testing.T) {
	tr := New(Config{ServiceName: "svc"})
	for _, raw := range []string{
		"",
		"garbage",
		"00-tooshort-0000000000000001-01",
		// Reserved version. Note that "99" is *not* in this list: only ff
		// is reserved, so an unknown-but-legal version with a well-formed
		// prefix must be accepted (W3C forward compatibility), and the
		// fleet-side test for that lives in traceparent_test.go.
		"ff-" + strings.Repeat("a", 32) + "-" + strings.Repeat("b", 16) + "-01",
		// All-zero trace id: accepting it would merge unrelated requests
		// into one trace at the aggregator, since traces are grouped by id.
		"00-" + strings.Repeat("0", 32) + "-" + strings.Repeat("b", 16) + "-01",
	} {

		c := fleet.MapCarrier{fleet.HeaderTraceparent: raw}
		if _, _, ok := tr.Extract(c); ok {
			t.Errorf("Extract(%q) = ok, want not-ok so the caller starts a new root trace", raw)
		}
	}
}

func TestNormalizeAddr(t *testing.T) {
	// These are the forms people actually write in a config file; each must
	// end up as something the export paths can be appended to.
	cases := map[string]string{
		"http://host:9000":        "http://host:9000",
		"http://host:9000/":       "http://host:9000",
		"http://host:9000/fleet":  "http://host:9000/fleet",
		"http://host:9000/fleet/": "http://host:9000/fleet",
		"host:9000":               "http://host:9000",
		"  http://host:9000  ":    "http://host:9000",
		"":                        "",
	}
	for in, want := range cases {
		if got := NormalizeAddr(in); got != want {
			t.Errorf("NormalizeAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTransportImplementsInterface(t *testing.T) {
	// A compile-time assertion in test form: if the interface gains a method,
	// this fails here rather than at the call site inside the Tracer.
	var _ fleet.Transport = New(Config{})
}
