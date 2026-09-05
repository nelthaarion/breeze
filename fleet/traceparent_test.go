package fleet

// Tests for the propagation core.
//
// The bar here is set by what a wrong answer costs downstream. This file is the
// only thing standing between the aggregator and a corrupt trace graph: it
// groups spans by trace-id string, so anything that lets two spellings of one id
// through, or lets an all-zero id through, produces traces that look plausible
// and are wrong. Those cases get explicit tests rather than being left to the
// round-trip check.

import (
	"strings"
	"testing"
)

const (
	goodTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	goodSpanID  = "00f067aa0ba902b7"
	goodSampled = "00-" + goodTraceID + "-" + goodSpanID + "-01"
)

func TestParseTraceparentValid(t *testing.T) {
	tc, ok := ParseTraceparent(goodSampled)
	if !ok {
		t.Fatal("well-formed traceparent was rejected")
	}
	if got := tc.TraceIDHex(); got != goodTraceID {
		t.Errorf("trace id = %q, want %q", got, goodTraceID)
	}
	if got := tc.SpanIDHex(); got != goodSpanID {
		t.Errorf("span id = %q, want %q", got, goodSpanID)
	}
	if !tc.Sampled {
		t.Error("flags 01 did not set Sampled")
	}
	if !tc.Valid() {
		t.Error("a parsed context reports itself invalid")
	}
}

func TestParseTraceparentUnsampledFlag(t *testing.T) {
	tc, ok := ParseTraceparent("00-" + goodTraceID + "-" + goodSpanID + "-00")
	if !ok {
		t.Fatal("rejected an unsampled but well-formed header")
	}
	if tc.Sampled {
		t.Error("flags 00 set Sampled")
	}
}

// TestParseTraceparentFlagsOtherBitsIgnored guards against reading the flags
// byte as a boolean. Bit 0 is the sampling decision; the remaining bits are
// reserved, and a sender that sets one must not be read as unsampled.
func TestParseTraceparentFlagsOtherBitsIgnored(t *testing.T) {
	tc, ok := ParseTraceparent("00-" + goodTraceID + "-" + goodSpanID + "-03")
	if !ok {
		t.Fatal("rejected a header with a reserved flag bit set")
	}
	if !tc.Sampled {
		t.Error("flags 03 has bit 0 set but Sampled is false")
	}
}

// TestParseTraceparentRejects is the important half of this file. Every case
// here would, if accepted, put something in the aggregator that corrupts trace
// assembly rather than merely losing one hop.
func TestParseTraceparentRejects(t *testing.T) {
	cases := map[string]string{
		"empty":                "",
		"too short":            "00-abc-def-01",
		"truncated":            goodSampled[:len(goodSampled)-1],
		"trailing data on v00": goodSampled + "-extra",
		"reserved version ff":  "ff-" + goodTraceID + "-" + goodSpanID + "-01",
		// Uppercase is the case that silently splits one trace in two, because
		// the aggregator keys on the hex string.
		"uppercase trace id": "00-4BF92F3577B34DA6A3CE929D0E0E4736-" + goodSpanID + "-01",
		"uppercase span id":  "00-" + goodTraceID + "-00F067AA0BA902B7-01",
		// An all-zero id would merge every request that carried it into one
		// enormous bogus trace.
		"zero trace id":    "00-" + strings.Repeat("0", 32) + "-" + goodSpanID + "-01",
		"zero span id":     "00-" + goodTraceID + "-" + strings.Repeat("0", 16) + "-01",
		"non-hex trace id": "00-zzf92f3577b34da6a3ce929d0e0e4736-" + goodSpanID + "-01",
		"non-hex flags":    "00-" + goodTraceID + "-" + goodSpanID + "-zz",
		"missing dashes":   "00" + goodTraceID + goodSpanID + "01",
		"wrong dash spot":  "00-" + goodTraceID + "_" + goodSpanID + "-01",
		"spaces":           "   ",
	}
	for desc, header := range cases {
		t.Run(desc, func(t *testing.T) {
			tc, ok := ParseTraceparent(header)
			if ok {
				t.Fatalf("accepted %q", header)
			}
			// A rejected header must also leave nothing usable behind, so a
			// caller that ignores the bool cannot propagate a half-parsed id.
			if tc.Valid() {
				t.Error("rejected header still produced a valid-looking context")
			}
		})
	}
}

// TestParseTraceparentFutureVersionExactLength is a regression test for an

// out-of-range panic found by the httptransport suite.
//
// A future-version header that added no trailing fields is exactly
// traceparentLen bytes long, and the forward-compatibility branch used to check
// for a delimiter at that offset — reading one byte past the end. §4.1 requires
// that a malformed header never panic; this input was not even malformed.
func TestParseTraceparentFutureVersionExactLength(t *testing.T) {
	tc, ok := ParseTraceparent("01-" + goodTraceID + "-" + goodSpanID + "-01")
	if !ok {
		t.Fatal("rejected a future-version header of exactly the v00 length")
	}
	if tc.TraceIDHex() != goodTraceID {
		t.Errorf("trace id = %s, want %s", tc.TraceIDHex(), goodTraceID)
	}
	if !tc.Sampled {
		t.Error("sampled flag lost on a future-version header")
	}
}

// TestParseTraceparentFutureVersion pins the forward-compatibility rule. Interop
// is the stated reason this format was chosen, so a newer sender we could have
// understood must not be discarded.
func TestParseTraceparentFutureVersion(t *testing.T) {
	tc, ok := ParseTraceparent("01-" + goodTraceID + "-" + goodSpanID + "-01-somethingnew")

	if !ok {
		t.Fatal("a version-01 header with a well-formed prefix was rejected")
	}
	if tc.TraceIDHex() != goodTraceID {
		t.Errorf("trace id = %q, want %q", tc.TraceIDHex(), goodTraceID)
	}
}

func TestTraceparentRoundTrip(t *testing.T) {
	tc := NewTraceContext()
	tc.Sampled = true

	got, ok := ParseTraceparent(tc.String())
	if !ok {
		t.Fatalf("could not parse our own output %q", tc.String())
	}
	if got != tc {
		t.Errorf("round trip changed the context:\n got %+v\nwant %+v", got, tc)
	}

	tc.Sampled = false
	got, ok = ParseTraceparent(tc.String())
	if !ok || got.Sampled {
		t.Errorf("unsampled round trip: ok=%v sampled=%v", ok, got.Sampled)
	}
}

func TestStringIsWireLength(t *testing.T) {
	if got := len(NewTraceContext().String()); got != traceparentLen {
		t.Errorf("rendered length = %d, want %d", got, traceparentLen)
	}
}

// TestNewTraceContextIsRandomAndUnsampled covers both properties a root context
// must have: ids that cannot be guessed, and no sampling decision of its own —
// the policy in §7 is the single owner of that bit.
func TestNewTraceContextIsRandomAndUnsampled(t *testing.T) {
	a, b := NewTraceContext(), NewTraceContext()
	if a.TraceID == b.TraceID {
		t.Error("two fresh contexts share a trace id")
	}
	if a.ParentSpanID == b.ParentSpanID {
		t.Error("two fresh contexts share a span id")
	}
	if a.Sampled {
		t.Error("NewTraceContext presumed a sampling decision")
	}
	if !a.Valid() {
		t.Error("a fresh context is not valid")
	}
}

func TestNewChildSpanIDDoesNotMutate(t *testing.T) {
	tc := NewTraceContext()
	before := tc.ParentSpanID

	child := tc.NewChildSpanID()
	if tc.ParentSpanID != before {
		t.Error("NewChildSpanID renumbered the span the caller is still recording")
	}
	if child == before {
		t.Error("child span id equals the parent's")
	}
}

func TestZeroValueContextIsNotValid(t *testing.T) {
	var tc TraceContext
	if tc.Valid() {
		t.Error("the zero value reports itself as a usable context")
	}
}

// --- Baggage ---------------------------------------------------------------

func TestParseBaggage(t *testing.T) {
	b, ok := ParseBaggage("tenant=acme,tier=pro")
	if !ok {
		t.Fatal("well-formed baggage was rejected")
	}
	if b["tenant"] != "acme" || b["tier"] != "pro" {
		t.Errorf("baggage = %v", b)
	}
}

// TestParseBaggageSkipsOnlyTheBadEntry is the rule that matters: a broken tag
// must never break the trace it rides on, so one malformed pair costs that pair
// and nothing else.
func TestParseBaggageSkipsOnlyTheBadEntry(t *testing.T) {
	b, ok := ParseBaggage("tenant=acme,garbage,=novalue,key=,tier=pro")
	if !ok {
		t.Fatal("a partially malformed value produced nothing usable")
	}
	if len(b) != 2 {
		t.Errorf("kept %d entries, want the 2 well-formed ones: %v", len(b), b)
	}
	if b["tenant"] != "acme" || b["tier"] != "pro" {
		t.Errorf("baggage = %v", b)
	}
}

func TestParseBaggageDegradesToEmpty(t *testing.T) {
	for _, header := range []string{"", "garbage", ",,,", "=", "novalue="} {
		b, ok := ParseBaggage(header)
		if ok {
			t.Errorf("ParseBaggage(%q) reported success", header)
		}
		if len(b) != 0 {
			t.Errorf("ParseBaggage(%q) = %v, want empty", header, b)
		}
	}
}

func TestBaggageRoundTrip(t *testing.T) {
	in := Baggage{"tenant": "acme", "tier": "pro", "region": "eu"}
	got, ok := ParseBaggage(in.String())
	if !ok {
		t.Fatalf("could not parse our own output %q", in.String())
	}
	if len(got) != len(in) {
		t.Fatalf("round trip = %v, want %v", got, in)
	}
	for k, v := range in {
		if got[k] != v {
			t.Errorf("key %q = %q, want %q", k, got[k], v)
		}
	}
}

// TestBaggageStringIsDeterministic is why rendering sorts. A map iterates in
// random order; if the header were rendered in that order, the same baggage
// would serialize differently on each hop, and a truncated set would drop
// different keys each time — a tag would appear to blink in and out as it
// crossed services.
func TestBaggageStringIsDeterministic(t *testing.T) {
	b := Baggage{"a": "1", "b": "2", "c": "3", "d": "4", "e": "5"}
	first := b.String()
	for i := 0; i < 50; i++ {
		if got := b.String(); got != first {
			t.Fatalf("render %d = %q, first was %q", i, got, first)
		}
	}
	if want := "a=1,b=2,c=3,d=4,e=5"; first != want {
		t.Errorf("render = %q, want %q", first, want)
	}
}

// TestBaggageStringRespectsCap keeps one service from taxing the whole fleet's
// header budget.
func TestBaggageStringRespectsCap(t *testing.T) {
	b := Baggage{}
	for i := 0; i < 200; i++ {
		b[strings.Repeat("k", 10)+string(rune('a'+i%26))+string(rune('a'+i/26))] = strings.Repeat(
			"v",
			20,
		)
	}
	got := b.String()
	if len(got) > DefaultMaxBaggageBytes {
		t.Errorf("rendered %d bytes, cap is %d", len(got), DefaultMaxBaggageBytes)
	}
	if got == "" {
		t.Error("cap dropped everything rather than truncating")
	}
	// Truncation must not leave a trailing separator or a half-written pair —
	// the result still has to parse.
	if _, ok := ParseBaggage(got); !ok {
		t.Errorf("truncated output does not parse: %q", got)
	}
	if strings.HasSuffix(got, ",") {
		t.Error("truncated output ends in a separator")
	}
}

func TestBaggageWithDoesNotMutate(t *testing.T) {
	orig := Baggage{"tenant": "acme"}
	next := orig.With("tier", "pro")

	if _, found := orig["tier"]; found {
		t.Error("With mutated the receiver, so one hop's tag can leak into a sibling's")
	}
	if next["tenant"] != "acme" || next["tier"] != "pro" {
		t.Errorf("With returned %v", next)
	}
}

func TestBaggageWithOverwrites(t *testing.T) {
	got := (Baggage{"tier": "free"}).With("tier", "pro")["tier"]
	if got != "pro" {
		t.Errorf("tier = %q, want %q", got, "pro")
	}
}

func TestNilBaggageIsUsable(t *testing.T) {
	var b Baggage
	if b.String() != "" {
		t.Error("nil baggage rendered a non-empty header")
	}
	if got := b.With("k", "v"); got["k"] != "v" {
		t.Errorf("With on nil baggage = %v", got)
	}
}

// --- Allocation budget -----------------------------------------------------

// TestParseTraceparentDoesNotAllocate is a test, not just a benchmark, because
// §4.1 states zero allocation on the hot path as a requirement — and this runs
// on every inbound request of every traced service. A regression here is a
// fleet-wide cost, so it should fail the suite rather than wait for someone to
// read a benchmark.
func TestParseTraceparentDoesNotAllocate(t *testing.T) {
	if n := testing.AllocsPerRun(100, func() {
		if _, ok := ParseTraceparent(goodSampled); !ok {
			t.Fatal("parse failed")
		}
	}); n != 0 {
		t.Errorf("ParseTraceparent allocates %v times per call, want 0", n)
	}
}

// The traceparent benchmarks live in bench_test.go alongside the rest of §12's
// required measurements, so the whole performance contract can be read and run as
// one set. The zero-allocation *assertion* stays here, immediately above, because
// it is a correctness test that happens to be about allocations: it fails the
// build, whereas a benchmark only reports a number nobody may be reading.
