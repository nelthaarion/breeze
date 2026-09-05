package fleet

// Tags are what turn a trace from "POST /orders was slow" into "*this* order was
// slow". This file owns the developer-facing fleet.Tag call (§9C.1), the
// per-request state every tag is written into, and the caps that keep both the
// span and the propagated header bounded.
//
// # Why tags and per-request state live in one file
//
// A tag is only meaningful against the request currently being handled, so the
// state it mutates and the caps that bound it are the same concern. Splitting
// them would put the limits somewhere other than the only code that can enforce
// them.
//
// # The propagation rule
//
// Every tag is also baggage: tag order_id at the gateway and the orders service
// three hops later reports it too, without anyone re-tagging by hand. That is
// what makes "find everything that touched order 123" answerable across a fleet
// rather than one service at a time. The cost is that tags ride in a header on
// every downstream call, which is exactly why the caps below are not optional.

import (
	"sync/atomic"
	"time"

	"github.com/nelthaarion/breeze"
)

// Caps. Defaults come from §10 (max_tags_per_span, max_baggage_bytes); the key
// cap is defensive and has no spec counterpart.
//
// These bound a *header*, not just memory. Baggage is re-sent on every outgoing
// call for the rest of the trace, so an unbounded tag is not a local mistake —
// it inflates every hop downstream of the service that set it, and a large
// enough one gets the request rejected by an intermediary with a header limit.
// Truncating silently is therefore the safer failure: a shortened tag value
// costs some debuggability, while a rejected request costs the user their
// request.
const (
	// MaxTagsPerSpan is the number of distinct keys one span will keep.
	MaxTagsPerSpan = 32

	// MaxTagValueBytes bounds a single value. Long enough for an id, a uuid,
	// or a short status string, which is what tags are for; short enough
	// that MaxTagsPerSpan of them cannot dominate a span.
	MaxTagValueBytes = 128

	// MaxTagKeyBytes bounds a single key. Keys are written by developers,
	// not derived from user input, so this only guards against accidents.
	MaxTagKeyBytes = 64
)

// ctxStateKey is the single Context key this package uses.
//
// One key holding one struct, rather than a key per field, because
// breeze.Context's store is a plain map allocated lazily per request: each
// distinct key is another map insert on the hot path. It is also namespaced,
// since the store is shared with application code that has no idea fleet exists.
const ctxStateKey = "breeze.fleet.state"

// requestState is everything fleet knows about the in-flight request.
//
// Created by fleet.Middleware (§5.1), read by Tag and by the outgoing-propagation
// helpers, and consumed when the span is emitted. It is owned by exactly one
// request on one goroutine, which is what lets Tag mutate the maps below without
// a lock — see the note on baggage in tag().
type requestState struct {
	// tc is the inbound trace context: the trace id for the whole journey
	// and, in ParentSpanID, the *caller's* span id.
	tc TraceContext

	// spanID identifies this hop, generated fresh per request.
	spanID [8]byte

	// start is when this hop began.
	//
	// One time.Time serves both needs Span has, because a Time carries a
	// wall clock and a monotonic reading together: start.UnixNano() gives
	// the comparable-across-machines timestamp, and time.Since(start) uses
	// the monotonic part, so an NTP step mid-request cannot produce a
	// negative duration.
	start time.Time

	// sampled is the trace-wide decision from §7, inherited from the caller
	// when there was one. It gates the expensive captures (timeline,
	// payloads), never whether an error is reported.
	sampled bool

	// baggage arrived from upstream; tags added here are merged into it for
	// the outgoing hop.
	baggage Baggage

	// tags are this span's own attributes. Nil until the first Tag call, so
	// a request that never tags allocates nothing.
	tags map[string]string

	// tr is the tracer that created this state, held so Inject can use the
	// configured transport's own header encoding and so the finished span
	// has somewhere to go. Nil is tolerated everywhere it is read: a state
	// without a tracer still propagates trace context correctly, it just
	// cannot export.
	tr *Tracer

	// serviceName is copied from the tracer so the outgoing x-breeze-service
	// header can be written without dereferencing tr on every injection.
	serviceName string
}

// Counters surfaced on the Performance page. Package-level and atomic because
// they are written from every request goroutine and read rarely, by one.
var (
	tagsDropped     atomic.Uint64 // over MaxTagsPerSpan
	tagsTruncated   atomic.Uint64 // key or value shortened
	tagsNoTrace     atomic.Uint64 // Tag called with no active trace
	malformedHeader atomic.Uint64 // §4.1: unparseable traceparent
)

// Metrics is a snapshot of fleet's own counters, for the Performance page.
//
// A struct rather than a metrics-library registration because the dashboard
// already owns presentation; this package's job is to make the numbers
// available, not to decide how they are displayed.
type Metrics struct {
	// TagsDropped counts tags rejected because the span was already at
	// MaxTagsPerSpan. A non-zero value means someone is tagging in a loop.
	TagsDropped uint64 `json:"tags_dropped"`

	// TagsTruncated counts keys or values shortened to fit.
	TagsTruncated uint64 `json:"tags_truncated"`

	// TagsNoTrace counts Tag calls that had no active trace to attach to —
	// normally fleet.Middleware missing from the chain, or a Tag call from
	// outside a request.
	TagsNoTrace uint64 `json:"tags_no_trace"`

	// MalformedHeaderTotal is §4.1's fleet_malformed_header_total: inbound
	// traceparent headers that could not be parsed. Steadily rising means an
	// upstream is emitting a broken header, and every one of those requests
	// starts a new root trace instead of joining its real parent.
	MalformedHeaderTotal uint64 `json:"malformed_header_total"`
}

// ReadMetrics returns the current counter values.
func ReadMetrics() Metrics {
	return Metrics{
		TagsDropped:          tagsDropped.Load(),
		TagsTruncated:        tagsTruncated.Load(),
		TagsNoTrace:          tagsNoTrace.Load(),
		MalformedHeaderTotal: malformedHeader.Load(),
	}
}

// noteMalformedHeader records one unparseable inbound traceparent (§4.1).
func noteMalformedHeader() { malformedHeader.Add(1) }

// Tag attaches a key/value attribute to the current request's span, and
// propagates it to every service downstream of this one.
//
//	fleet.Tag(ctx, "order_id", orderID)
//	fleet.Tag(ctx, "payment_provider", "stripe")
//
// Tag never fails and never panics. If fleet tracing is disabled, the middleware
// is not installed, or ctx is not a request context, the call is a map lookup
// and a return — tagging code does not need to be guarded by an "is tracing on"
// check, because that check is the whole implementation.
//
// Values are truncated rather than rejected when over the caps above, and tags
// past MaxTagsPerSpan are dropped rather than growing the span without bound.
// Both are counted (see ReadMetrics) so silent truncation is discoverable
// instead of merely silent.
func Tag(ctx *breeze.Context, key, value string) {
	if ctx == nil || key == "" {
		return
	}
	st, ok := stateFrom(ctx)
	if !ok {
		// No active trace. Counted rather than ignored: a developer who
		// tags diligently and sees nothing in the UI is otherwise left
		// guessing, and "the middleware isn't installed" is by far the
		// most common cause.
		tagsNoTrace.Add(1)
		return
	}
	st.tag(key, value)
}

// tag applies one key/value to the state, enforcing the caps.
//
// Unexported and called only from Tag so the exported entry point stays a
// lookup plus a branch, and so the state's own invariants are enforced in one
// place rather than at each call site.
func (st *requestState) tag(key, value string) {
	if len(key) > MaxTagKeyBytes {
		key = key[:MaxTagKeyBytes]
		tagsTruncated.Add(1)
	}
	if len(value) > MaxTagValueBytes {
		// Byte truncation can split a multi-byte rune. That is
		// acceptable here: the value is display/search data, not
		// something re-parsed, and trimming to a rune boundary would
		// cost a scan on every oversized tag to save a mojibake
		// character at the very end of a value already too long to be
		// read in full.
		value = value[:MaxTagValueBytes]
		tagsTruncated.Add(1)
	}

	if st.tags == nil {
		// Sized to the cap's small side rather than MaxTagsPerSpan:
		// most requests tag two or three things, and pre-sizing for 32
		// would waste the allocation on nearly every request that tags
		// at all.
		st.tags = make(map[string]string, 4)
	} else if len(st.tags) >= MaxTagsPerSpan {
		// Overwriting a key already present is not growth, so let it
		// through; only a genuinely new key is dropped at the cap.
		if _, exists := st.tags[key]; !exists {
			tagsDropped.Add(1)
			return
		}
	}
	st.tags[key] = value

	// Mirror into baggage so downstream hops inherit the tag.
	//
	// Baggage.With is the immutable API for callers outside a request, and
	// it allocates a new map per call by design. Here the map belongs to
	// exactly one in-flight request on one goroutine, so mutating it in
	// place is safe and keeps tagging O(1) instead of O(n) per tag. The
	// size cap is applied when baggage is rendered to a header, not here,
	// because that is the only point where the byte budget is knowable.
	if st.baggage == nil {
		st.baggage = make(Baggage, 4)
	}
	st.baggage[key] = value
}

// tagsSnapshot returns a copy of the tags for embedding in a Span.
//
// A copy, because the state is recycled with the request while the span it
// produced outlives it in a ring buffer and is later serialized by another
// goroutine. Handing the live map over would let a recycled request mutate a
// span already queued for export — a data race that would surface as
// occasional wrong tags on the wrong trace, which is close to impossible to
// diagnose from the UI.
func (st *requestState) tagsSnapshot() map[string]string {
	if len(st.tags) == 0 {
		return nil
	}
	out := make(map[string]string, len(st.tags))
	for k, v := range st.tags {
		out[k] = v
	}
	return out
}

// stateFrom returns the fleet state attached to ctx, if any.
//
// The single reader used by Tag and the propagation helpers, so the key and the
// type assertion exist in one place.
func stateFrom(ctx *breeze.Context) (*requestState, bool) {
	if ctx == nil {
		return nil, false
	}
	v, ok := ctx.Get(ctxStateKey)
	if !ok {
		return nil, false
	}
	st, ok := v.(*requestState)
	if !ok || st == nil {
		return nil, false
	}
	return st, true
}
