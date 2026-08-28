package fleet

// Span is the unit of data every transport carries and the aggregator assembles
// traces from. This file defines that wire contract, plus the two other payloads
// a service sends (a heartbeat, and a batch envelope for the in-process bus).
//
// # Why these types live together
//
// All three are the same kind of thing: a shape that crosses a process boundary
// and therefore cannot change without breaking a fleet mid-deploy. Keeping them
// in one file makes the whole wire surface reviewable at a glance, rather than
// scattered across the transports that happen to send them.
//
// # One schema, many envelopes
//
// Every transport sends *this* JSON. Only the framing differs: httptransport
// POSTs an array of Span, the WS path wraps the same array in a small envelope,
// and the in-process bus passes SpanBatch as a Go value with no serialization at
// all. That is deliberate — a fleet mixing transports must still assemble into
// one trace, so the payload cannot vary by how it was delivered.

import (
	"encoding/json"

	"github.com/nelthaarion/breeze/dashboard"
)

// Span is one service's record of handling one request.
//
// Field order matches the aggregator's read patterns (identity, then attribution,
// then timing, then optional bulk) rather than the order fields were specified —
// the bulk fields at the end are the ones most often absent, and `omitempty`
// keeps them off the wire entirely when they are.
type Span struct {
	// TraceID and SpanID are lowercase hex, exactly as they appear in the
	// traceparent header. They are strings rather than [16]byte/[8]byte
	// because the aggregator keys maps on them and JSON has no byte-array
	// form — see ValidHex below for why the encoding is enforced on ingest.
	TraceID string `json:"trace_id"`
	SpanID  string `json:"span_id"`

	// ParentSpanID is empty on a root span. The aggregator treats a
	// non-empty parent that it has never seen as an orphan and still renders
	// it (§8.1.3) — a missing parent means the caller could not report, not
	// that this span is invalid.
	ParentSpanID string `json:"parent_span_id,omitempty"`

	Service string `json:"service"`
	Route   string `json:"route"`
	Method  string `json:"method"`
	Status  int    `json:"status"`

	// StartNanoUTC is wall-clock Unix nanoseconds, not a monotonic reading:
	// it has to be comparable against a *different machine's* timestamps to
	// merge a cross-service timeline, and monotonic clocks are only
	// meaningful within one process. The cost is clock skew between hosts,
	// which the aggregator detects and flags rather than silently rendering
	// a hop that starts before its parent finished (§8.1.4).
	StartNanoUTC int64 `json:"start_ns"`

	// DurationMs is measured with a monotonic clock (time.Since) even though
	// StartNanoUTC is wall-clock, so a clock adjustment mid-request cannot
	// produce a negative or wildly wrong duration.
	DurationMs float64 `json:"duration_ms"`

	// Error carries the failure text when the hop failed. It is scrubbed at
	// the source service before export (§11.3) — error strings are a classic
	// accidental-secret channel, since they routinely interpolate a
	// connection string or token that a well-behaved header masker would
	// never have let through.
	Error string `json:"error,omitempty"`

	// Tags are the developer-attached attributes from fleet.Tag (§9C.1), the
	// thing that makes a trace searchable by order_id rather than only by
	// route. Bounded by MaxTagsPerSpan / MaxTagValueBytes.
	Tags map[string]string `json:"tags,omitempty"`

	// Timeline is the dashboard's own nested step capture for this request,
	// reused verbatim rather than re-instrumented. dashboard.TimelineStep is
	// imported rather than mirrored so there is exactly one definition of
	// "what a timeline step looks like" — a parallel struct here would drift
	// from the dashboard's the first time either changed.
	//
	// Present only on sampled requests. See §7 for why an unsampled request
	// that errors still gets a span, but not this.
	Timeline []dashboard.TimelineStep `json:"timeline,omitempty"`

	// RequestPayload/ResponsePayload are the captured bodies that live
	// contract validation (§9A) checks against the callee's own OpenAPI
	// schema. They are the only fields here that can carry user data, so two
	// rules are absolute:
	//
	//  1. Capture is gated on the sampling decision — never every request.
	//  2. Sensitive field names are redacted *at the source service*, before
	//     export (§11.6). The aggregator never scrubs, because by the time it
	//     could, the data has already crossed the network. A leak here is a
	//     data-security bug, not a cosmetic one.
	//
	// json.RawMessage keeps an already-encoded body from being decoded and
	// re-encoded on every hop just to be forwarded.
	RequestPayload  json.RawMessage `json:"request_payload,omitempty"`
	ResponsePayload json.RawMessage `json:"response_payload,omitempty"`
}

// Failed reports whether this span represents a failed hop.
//
// Both conditions matter: a 5xx with no Error text (the dashboard's own
// convention, which records "HTTP 500") and an Error set on a 2xx (a handler
// that caught something, recovered, and still returned a body). Root-cause
// marking (§9B.1) keys off this, so missing either case would mis-attribute a
// cascade.
func (s Span) Failed() bool { return s.Status >= 500 || s.Error != "" }

// IsRoot reports whether this span begins a trace.
func (s Span) IsRoot() bool { return s.ParentSpanID == "" }

// Valid reports whether a span is safe to store.
//
// Called on the ingestion path (§8.1.1) to reject malformed payloads with 400
// rather than letting them into the store. The check is deliberately about
// *identity* only — a span with a nonsense route or a negative duration is still
// useful data, but a span with a bad trace id would corrupt trace assembly by
// grouping under a key nothing else shares.
func (s Span) Valid() bool {
	if !validHex(s.TraceID, 32) || !validHex(s.SpanID, 16) {
		return false
	}
	if s.ParentSpanID != "" && !validHex(s.ParentSpanID, 16) {
		return false
	}
	return true
}

// validHex reports whether s is exactly n lowercase hex digits and not all
// zeroes.
//
// Lowercase-only mirrors ParseTraceparent's strictness for the same reason
// documented there: the aggregator groups spans on the id *string*, so accepting
// two spellings of one id would split a single request into two unrelated
// traces. All-zero is rejected because it would merge every request that carried
// it into one enormous bogus trace.
func validHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	zero := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			if c != '0' {
				zero = false
			}
		case c >= 'a' && c <= 'f':
			zero = false
		default:
			return false
		}
	}
	return !zero
}

// SpanBatch is the envelope for a flush of spans.
//
// It exists for the in-process events transport, where the Go *type* is the
// topic (the bus is type-keyed, not string-keyed): emitting SpanBatch is how a
// subscriber is addressed at all. Networked transports send the Spans slice
// directly, so this stays a thin wrapper rather than growing fields the wire
// format would then have to carry everywhere.
type SpanBatch struct {
	Spans []Span `json:"spans"`
}

// Heartbeat is a service's periodic "I am alive, and here is my shape" report
// (§8.1.2), sent on the same background goroutine as the span flush.
//
// It does double duty. The liveness half lets the aggregator mark a service down
// after ServiceTTL without polling it. The schema half is what makes live
// contract validation possible: OpenAPIHash is a content hash of the document
// the service is serving *right now*, so the aggregator re-fetches only when it
// actually changes. That is the whole trick behind §9A — the schema being
// validated against is generated from the running code, so it cannot go stale
// the way a hand-maintained contract file does.
type Heartbeat struct {
	Service string `json:"service"`

	// InstanceID distinguishes replicas of one service. Without it, three
	// pods of orders-service would look like one flapping instance as their
	// heartbeats interleaved.
	InstanceID string `json:"instance_id"`

	Version string `json:"version,omitempty"`

	// RPS and ErrorRate are the reporter's own recent numbers, sent so the
	// topology view can size and colour a node before any span for it has
	// been assembled into a trace.
	RPS       float64 `json:"rps"`
	ErrorRate float64 `json:"error_rate"`

	// OpenAPIHash changes only when the served document changes; the
	// aggregator compares it to what it cached and skips the fetch when
	// equal. OpenAPIURL is where to fetch it — carried explicitly rather
	// than derived, since a service may serve its document somewhere other
	// than the default /openapi.json.
	OpenAPIHash string `json:"openapi_hash,omitempty"`
	OpenAPIURL  string `json:"openapi_url,omitempty"`
}
