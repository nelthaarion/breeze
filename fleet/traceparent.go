// Package fleet implements distributed trace propagation and span export for
// Breeze services.
//
// This file is the propagation core: the wire format every transport in
// fleet/transport/ encodes and decodes, and the only part of fleet that a
// non-Breeze service must understand to keep a trace intact through its hop.
//
// # Why W3C Trace Context rather than a Breeze-specific format
//
// The format is not ours to invent. A trace is only useful if every hop agrees
// on it, and a fleet is rarely all one framework — a Python or Java service that
// has never heard of Breeze can keep a trace whole just by copying one header
// onto its outgoing calls. W3C Trace Context is the format that buys that for
// free, it is a fixed-width text encoding with no dependency, and it means a
// user who later fronts the fleet with something OTel-based does not have to
// re-instrument anything.
//
//	traceparent: 00-<32 hex trace-id>-<16 hex parent-span-id>-<2 hex flags>
//
// # Strictness is a correctness property, not pedantry
//
// Parsing is deliberately strict — lowercase hex only, no all-zero ids, no
// reserved version. The aggregator assembles a trace by grouping spans on the
// trace-id *string*. So if one service spelled an id in uppercase and another in
// lowercase, the two would group separately and a single request would render as
// two unrelated traces. Rejecting the malformed value and starting a clean root
// trace loses one hop's parentage; accepting both spellings silently corrupts
// every trace that passes through such a sender.
package fleet

import (
	"crypto/rand"
	"encoding/hex"
	"sort"
	"strings"
)

// traceparentLen is the wire length of a version-00 traceparent value:
// 2 version + 1 dash + 32 trace-id + 1 dash + 16 span-id + 1 dash + 2 flags.
const traceparentLen = 55

// Byte offsets within a version-00 traceparent. Named because three of them are
// dash positions checked before any decoding, and a bare 35 in that check reads
// as a magic number.
const (
	offVersion = 0
	offDash1   = 2
	offTraceID = 3
	offDash2   = 35
	offSpanID  = 36
	offDash3   = 52
	offFlags   = 53
)

// flagSampled is bit 0 of the flags byte: the sampling decision, made once at
// the root and then honoured unchanged by every downstream hop (§7). A trace is
// sampled as a whole or not at all, which is what keeps a sampled trace from
// rendering with holes where an unsampled service should have been.
const flagSampled = 0x01

// DefaultMaxBaggageBytes bounds the rendered length of a baggage header.
//
// Baggage rides on every hop of every request, so it is a per-request tax on
// every service downstream of whoever set it. The cap exists so one service
// attaching a generous tag cannot inflate the header budget of the entire fleet.
const DefaultMaxBaggageBytes = 512

// TraceContext is one hop's view of a trace: which trace it belongs to, whose
// span caused it, and whether this trace is being recorded.
//
// It is a value type with no pointers so that passing it around, storing it on a
// request, and copying it into a child span cost nothing and allocate nothing.
type TraceContext struct {
	// TraceID identifies the whole request journey. Generated once, by the
	// first service to see a request carrying no traceparent, and then
	// unchanged through every hop.
	TraceID [16]byte

	// ParentSpanID is the span that caused this hop. On a context parsed from
	// an incoming header it is the caller's span; on a freshly created root it
	// is this hop's own span id, which is what the next hop will call its
	// parent.
	ParentSpanID [8]byte

	// Sampled mirrors bit 0 of the wire flags. NewTraceContext leaves it
	// false: the sampling policy in §7 is the single owner of this decision
	// for a root trace, and a parsed context inherits whatever its caller
	// decided. Nothing else should set it.
	Sampled bool
}

// NewTraceContext starts a fresh root trace with a random trace id and span id.
//
// Sampled is left false by design — see the field comment. The caller that owns
// the sampling policy sets it.
func NewTraceContext() TraceContext {
	var tc TraceContext
	// crypto/rand.Read is documented never to return an error as of Go 1.24;
	// it panics internally if the system source fails. There is no error to
	// handle here, and inventing a fallback to math/rand would trade a loud
	// failure for trace ids an attacker could predict and forge.
	_, _ = rand.Read(tc.TraceID[:])
	_, _ = rand.Read(tc.ParentSpanID[:])
	return tc
}

// ParseTraceparent decodes a traceparent header value.
//
// It allocates nothing: the ids decode straight into the returned value's fixed
// arrays, and the header is indexed as a string rather than converted to bytes.
// This runs on every inbound request of every traced service, so it is on the
// hot path by definition.
//
// A false return means "this header is not usable" — the caller starts a new
// root trace (§4.1) rather than propagating something broken. It never panics
// on arbitrary input.
func ParseTraceparent(header string) (TraceContext, bool) {
	if len(header) < traceparentLen {
		return TraceContext{}, false
	}
	if header[offDash1] != '-' || header[offDash2] != '-' || header[offDash3] != '-' {
		return TraceContext{}, false
	}

	version, ok := unhexByte(header[offVersion], header[offVersion+1])
	if !ok {
		return TraceContext{}, false
	}
	// 0xff is reserved by the spec and never a real version.
	if version == 0xff {
		return TraceContext{}, false
	}
	switch version {
	case 0x00:
		// Version 00 is exactly this long. Trailing data means a sender that
		// does not agree with us about the format.
		if len(header) != traceparentLen {
			return TraceContext{}, false
		}
	default:
		// A future version may append fields. W3C's rule is to parse the
		// known prefix and ignore the rest, which is the whole reason this
		// format was chosen for interop — so honour it rather than rejecting
		// a newer sender we could have understood.
		//
		// The length check comes first and is not merely defensive: a
		// future-version header that happens to be exactly traceparentLen
		// bytes long (a v01 sender that added no fields) is perfectly
		// valid, and indexing traceparentLen on it reads one past the end.
		// Only a *longer* header has a delimiter to verify, and requiring
		// one is what stops "00-<ids>-01garbage" from being accepted as if
		// the trailing bytes were a legitimate extension field.
		if len(header) > traceparentLen && header[traceparentLen] != '-' {
			return TraceContext{}, false
		}
	}

	var tc TraceContext
	if !unhexInto(tc.TraceID[:], header[offTraceID:offDash2]) {
		return TraceContext{}, false
	}
	if !unhexInto(tc.ParentSpanID[:], header[offSpanID:offDash3]) {
		return TraceContext{}, false
	}
	// An all-zero id is invalid per the spec, and dangerous here specifically:
	// the aggregator groups spans by trace id, so a fleet where several
	// services emitted the zero trace id would merge unrelated requests into
	// one enormous bogus trace.
	if isZero(tc.TraceID[:]) || isZero(tc.ParentSpanID[:]) {
		return TraceContext{}, false
	}

	flags, ok := unhexByte(header[offFlags], header[offFlags+1])
	if !ok {
		return TraceContext{}, false
	}
	tc.Sampled = flags&flagSampled != 0

	return tc, true
}

// String renders the traceparent header value for this context.
//
// One allocation, for the returned string. That is once per outgoing call, not
// once per inbound request, so it is off the hot path that matters.
func (tc TraceContext) String() string {
	var b [traceparentLen]byte
	b[offVersion], b[offVersion+1] = '0', '0'
	b[offDash1], b[offDash2], b[offDash3] = '-', '-', '-'
	hex.Encode(b[offTraceID:offDash2], tc.TraceID[:])
	hex.Encode(b[offSpanID:offDash3], tc.ParentSpanID[:])
	b[offFlags] = '0'
	if tc.Sampled {
		b[offFlags+1] = '1'
	} else {
		b[offFlags+1] = '0'
	}
	return string(b[:])
}

// NewChildSpanID returns a fresh span id for the next hop. It does not mutate
// the receiver: a TraceContext is treated as immutable per hop, so the caller
// decides explicitly what to send onward and cannot accidentally renumber the
// span it is currently recording.
func (tc TraceContext) NewChildSpanID() [8]byte {
	var id [8]byte
	_, _ = rand.Read(id[:])
	return id
}

// TraceIDHex is the trace id as the aggregator keys it.
func (tc TraceContext) TraceIDHex() string { return hex.EncodeToString(tc.TraceID[:]) }

// SpanIDHex is the parent span id in the same encoding used on the wire.
func (tc TraceContext) SpanIDHex() string { return hex.EncodeToString(tc.ParentSpanID[:]) }

// Valid reports whether tc carries ids that may be put on the wire. A
// zero-value TraceContext is not valid, which makes an unset context
// distinguishable from a real one without a separate boolean.
func (tc TraceContext) Valid() bool {
	return !isZero(tc.TraceID[:]) && !isZero(tc.ParentSpanID[:])
}

// Baggage carries user-defined key/value tags alongside the trace context, so a
// tag set at the edge (tenant, plan tier, feature flag) is readable by every
// service downstream without any of them querying for it.
//
// It is a map for ergonomics, and treated as immutable per hop: With returns a
// copy rather than mutating, matching TraceContext's own style, so one handler
// cannot reach through a shared reference and change what a sibling propagates.
type Baggage map[string]string

// ParseBaggage decodes a "k1=v1,k2=v2" baggage value.
//
// Malformed entries are skipped one at a time rather than failing the whole
// header: a broken tag must never break the trace it is attached to. The bool
// reports whether anything usable was found, so a caller can tell "no baggage"
// from "baggage that was entirely garbage" — both degrade to an empty Baggage
// for that hop, but only the second is worth counting.
func ParseBaggage(header string) (Baggage, bool) {
	if header == "" {
		return nil, false
	}
	var b Baggage
	for len(header) > 0 {
		var entry string
		if i := strings.IndexByte(header, ','); i >= 0 {
			entry, header = header[:i], header[i+1:]
		} else {
			entry, header = header, ""
		}
		eq := strings.IndexByte(entry, '=')
		if eq <= 0 {
			// No key, or no separator at all. Skip this entry only.
			continue
		}
		key := strings.TrimSpace(entry[:eq])
		val := strings.TrimSpace(entry[eq+1:])
		if key == "" || val == "" {
			continue
		}
		if b == nil {
			b = make(Baggage, 4)
		}
		b[key] = val
	}
	return b, b != nil
}

// String renders baggage back to "k1=v1,k2=v2", bounded by
// DefaultMaxBaggageBytes.
//
// Keys are emitted in sorted order and dropped from the end when the budget is
// exceeded. Note this is a deliberate deviation from "drop oldest first": the
// declared type is a plain map, which records neither insertion order nor
// priority, so oldest-first is not expressible without changing the type.
// Sorted order at least makes the truncation deterministic — the same baggage
// renders the same way on every hop, so a tag does not blink in and out of
// existence as it crosses services.
func (b Baggage) String() string {
	if len(b) == 0 {
		return ""
	}
	keys := make([]string, 0, len(b))
	for k := range b {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	for _, k := range keys {
		entry := len(k) + 1 + len(b[k]) // k=v
		if sb.Len() > 0 {
			entry++ // the joining comma
		}
		if sb.Len()+entry > DefaultMaxBaggageBytes {
			break
		}
		if sb.Len() > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(k)
		sb.WriteByte('=')
		sb.WriteString(b[k])
	}
	return sb.String()
}

// With returns a copy of b with one key set. The receiver is unchanged, so a
// caller may hold a baggage value across a fan-out without any callee's tag
// leaking into a sibling's.
func (b Baggage) With(key, value string) Baggage {
	out := make(Baggage, len(b)+1)
	for k, v := range b {
		out[k] = v
	}
	out[key] = value
	return out
}

// unhexInto decodes lowercase hex from s into dst. It reports false rather than
// decoding partially, so a caller never sees half-filled ids.
//
// Uppercase is rejected: see the package comment — two spellings of one trace id
// would split a single trace in the aggregator.
func unhexInto(dst []byte, s string) bool {
	if len(s) != len(dst)*2 {
		return false
	}
	for i := range dst {
		v, ok := unhexByte(s[i*2], s[i*2+1])
		if !ok {
			return false
		}
		dst[i] = v
	}
	return true
}

// unhexByte decodes two lowercase hex digits into one byte.
func unhexByte(hi, lo byte) (byte, bool) {
	h, ok := unhexDigit(hi)
	if !ok {
		return 0, false
	}
	l, ok := unhexDigit(lo)
	if !ok {
		return 0, false
	}
	return h<<4 | l, true
}

func unhexDigit(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	default:
		return 0, false
	}
}

func isZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}
