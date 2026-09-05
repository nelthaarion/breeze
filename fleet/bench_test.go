package fleet

// Benchmarks required by §12.
//
// These exist because the spec makes falsifiable performance claims, and a claim
// about allocation counts is worthless as prose. The two that actually gate the
// feature's acceptance:
//
//   - §12.1 — disabled tracing must cost 0 allocations and single-digit
//     nanoseconds. This is what "omitting the fleet block changes nothing" means
//     in practice, and it is the promise that lets fleet ship in the base module
//     without every existing user paying for it.
//   - §12.3 — ParseTraceparent must not allocate on a well-formed header. It runs
//     once per request per service, so an allocation here lands on the hot path
//     of every traced request in the fleet.
//
// Run:
//
//	go test -run '^$' -bench . -benchmem ./fleet/
//
// The -run '^$' is not decoration: without it the entire test suite runs before
// the benchmarks on every invocation.

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// nopTransport is a Transport that does nothing, successfully.
//
// The benchmarks below measure the *recording* path — the work a request pays
// for — and deliberately not export, which happens on the background goroutine.
// A real transport here would put a network stack inside the numbers and measure
// the wrong thing entirely. Returning nil from the export methods also keeps the
// flush loop from entering backoff, which would otherwise perturb long runs.
type nopTransport struct{}

func (nopTransport) Name() string                          { return "nop" }
func (nopTransport) Inject(TraceContext, Baggage, Carrier) {}

func (nopTransport) Extract(
	Carrier,
) (TraceContext, Baggage, bool) {
	return TraceContext{}, nil, false
}
func (nopTransport) ExportSpans(context.Context, string, []Span) error        { return nil }
func (nopTransport) ExportHeartbeat(context.Context, string, Heartbeat) error { return nil }

// benchTracer builds an enabled Tracer whose background loop will not interfere.
//
// FlushInterval is an hour so the flush tick never fires mid-benchmark: the point
// is to measure RecordSpan in isolation, and a concurrent drain would show up as
// lock contention that no real service at this span rate would see.
func benchTracer(b *testing.B) *Tracer {
	b.Helper()
	t := New(TracerConfig{
		ServiceName:       "bench-service",
		AggregatorURL:     "http://127.0.0.1:1/fleet",
		Enabled:           true,
		FlushInterval:     time.Hour,
		HeartbeatInterval: time.Hour,
		Transport:         nopTransport{},
	})
	b.Cleanup(func() {
		// Bounded context: teardown must not hang a benchmark run even if
		// the flush loop is mid-attempt.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = t.Close(ctx)
	})
	return t
}

// benchSpan is a representative span: realistic route, no timeline, no payloads.
// Built once outside every timed loop, since the cost under measurement is
// RecordSpan's and not the caller's span construction.
func benchSpan() Span {
	return Span{
		TraceID:      "4bf92f3577b34da6a3ce929d0e0e4736",
		SpanID:       "00f067aa0ba902b7",
		ParentSpanID: "0123456789abcdef",
		Service:      "orders-service",
		Route:        "/orders/:id/charge",
		Method:       "POST",
		Status:       200,
		StartNanoUTC: time.Now().UnixNano(),
		DurationMs:   12.5,
	}
}

// --- §12.1: disabled must be free ------------------------------------------

// BenchmarkRecordSpanDisabled is the headline number.
//
// A disabled Tracer must be one branch and a return. A nonzero alloc count here
// breaks the "zero overhead when disabled" guarantee, and with it the argument
// for shipping fleet in the base module at all.
func BenchmarkRecordSpanDisabled(b *testing.B) {
	t := New(TracerConfig{Enabled: false})
	s := benchSpan()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		t.RecordSpan(s)
	}
}

// BenchmarkRecordSpanNilTracer covers the other disabled shape: the nil *Tracer
// a service that never constructed one passes around. It must be exactly as
// cheap, because "never configured fleet" and "explicitly turned it off" should
// be indistinguishable at runtime.
func BenchmarkRecordSpanNilTracer(b *testing.B) {
	var t *Tracer
	s := benchSpan()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		t.RecordSpan(s)
	}
}

// BenchmarkMiddlewareDisabled measures what an existing app pays for having the
// fleet middleware installed but switched off. §16 requires this to be
// indistinguishable from not installing it, so the middleware returns the bare
// next handler when disabled rather than a wrapper that checks a flag per
// request — this benchmark is what would expose a regression on that.
func BenchmarkMiddlewareDisabled(b *testing.B) {
	t := New(TracerConfig{Enabled: false})
	h := Middleware(t)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = h
	}
}

// --- §12.2: enabled must be bounded ----------------------------------------

// BenchmarkRecordSpanEnabled is §12.2: bounded allocations on the enabled path.
//
// The ring buffer is preallocated and spans are copied in by value, so the steady
// state should be allocation-free: the buffer never grows and nothing escapes.
func BenchmarkRecordSpanEnabled(b *testing.B) {
	t := benchTracer(b)
	s := benchSpan()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		t.RecordSpan(s)
	}
}

// BenchmarkRecordSpanEnabledWithTags shows what the §9C.1 tag feature costs,
// separately from the baseline, so the tag map's price is visible rather than
// folded invisibly into the headline enabled number.
func BenchmarkRecordSpanEnabledWithTags(b *testing.B) {
	t := benchTracer(b)
	s := benchSpan()
	s.Tags = map[string]string{"order_id": "12345", "user_id": "u-99"}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		t.RecordSpan(s)
	}
}

// BenchmarkRecordSpanContended is the realistic shape: a gnet server records
// spans from many goroutines at once. Contention on the buffer's single mutex
// would surface here as the core count rises, which is the signal that the
// locking strategy needs revisiting — and the reason to measure it now rather
// than discover it in production.
func BenchmarkRecordSpanContended(b *testing.B) {
	t := benchTracer(b)
	s := benchSpan()

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			t.RecordSpan(s)
		}
	})
}

// --- §12.3: traceparent parsing --------------------------------------------

// BenchmarkParseTraceparent is §12.3: zero allocations on a well-formed header.
// Every service pays this on every hop of every traced request.
func BenchmarkParseTraceparent(b *testing.B) {
	const header = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tc, ok := ParseTraceparent(header)
		if !ok {
			b.Fatal("well-formed header rejected — this is benchmarking the failure path")
		}
		_ = tc
	}
}

// BenchmarkParseTraceparentMalformed measures rejection, which any buggy or
// hostile neighbour can drive as hard as it likes. It must also not allocate: a
// bad header is an early return, not an error value.
func BenchmarkParseTraceparentMalformed(b *testing.B) {
	const header = "00-4bf92f3577b34da6a3ce929d0e0e47-00f067aa0ba902b7-01" // trace id too short

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := ParseTraceparent(header); ok {
			b.Fatal("malformed header accepted — this is benchmarking the wrong path")
		}
	}
}

// BenchmarkTraceparentString measures rendering, paid once per outgoing traced
// call. One string allocation is unavoidable (the header value has to exist);
// this exists to keep it at exactly one.
func BenchmarkTraceparentString(b *testing.B) {
	tc := NewTraceContext()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = tc.String()
	}
}

// BenchmarkNewTraceContext and BenchmarkNewChildSpanID both read crypto/rand,
// which is the dominant cost in each. Worth isolating: if they show up hot in a
// profile, the answer is a buffered CSPRNG, not anything about span handling.
func BenchmarkNewTraceContext(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = NewTraceContext()
	}
}

func BenchmarkNewChildSpanID(b *testing.B) {
	tc := NewTraceContext()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = tc.NewChildSpanID()
	}
}

// --- Baggage (§9C.1) -------------------------------------------------------

func BenchmarkParseBaggage(b *testing.B) {
	const header = "order_id=12345,user_id=u-99,tenant=acme"

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bg, _ := ParseBaggage(header)
		_ = bg
	}
}

// BenchmarkBaggageString is the encode paid on every outgoing call once a service
// uses tags. Baggage is the feature most likely to be switched on everywhere and
// forgotten, so its cost should be known rather than discovered later.
func BenchmarkBaggageString(b *testing.B) {
	bg := Baggage{"order_id": "12345", "user_id": "u-99", "tenant": "acme"}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = bg.String()
	}
}

// BenchmarkBaggageStringOverLimit measures the truncation path that runs once a
// service has accumulated more baggage than max_baggage_bytes allows. It sorts to
// make the drop order deterministic, so it is strictly more expensive than the
// common case — which is exactly why it should stay visible.
func BenchmarkBaggageStringOverLimit(b *testing.B) {
	bg := Baggage{}
	for i := 0; i < 40; i++ {
		bg["key_"+strconv.Itoa(i)] = "value_that_is_not_short_" + strconv.Itoa(i)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = bg.String()
	}
}

// --- Sampling (§7) ---------------------------------------------------------

// BenchmarkSampleRoot covers the three interesting rates.
//
// Rates 1.0 and 0.0 must short-circuit before touching the trace id at all:
// 1.0 is the default for small fleets and 0.0 is how a service opts out of rate
// sampling while still reporting errors, so both are common enough to deserve
// being free.
//
// The trace context is generated once, outside the loop, so this measures the
// sampling arithmetic rather than crypto/rand. That does make the branch
// perfectly predictable for a fixed id, which flatters the 0.2 case slightly —
// the arithmetic being measured is identical either way.
func BenchmarkSampleRoot(b *testing.B) {
	tc := NewTraceContext()

	for _, c := range []struct {
		name string
		rate float64
	}{
		{"AlwaysSample", 1.0},
		{"NeverSample", 0.0},
		{"Rate20Percent", 0.2},
	} {
		b.Run(c.name, func(b *testing.B) {
			s := newSampler(c.rate)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = s.sampleRoot(tc)
			}
		})
	}
}

// BenchmarkSamplerDecide measures the per-request entry point, including the
// inherited-decision path that every non-edge service takes. Inheritance must be
// cheaper than rolling: it is a field read, and it is the common case in any
// fleet more than one service deep.
func BenchmarkSamplerDecide(b *testing.B) {
	s := newSampler(0.2)
	tc := NewTraceContext()

	b.Run("Inherited", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = s.decide(tc, true)
		}
	})
	b.Run("Root", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = s.decide(tc, false)
		}
	})
}

// BenchmarkSampleRootContended exists because the sampler is shared by every
// request goroutine. Deriving the decision from the trace id means there is no
// shared RNG to lock; if this ever fails to scale, that property has been lost.
func BenchmarkSampleRootContended(b *testing.B) {
	s := newSampler(0.2)
	tc := NewTraceContext()

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = s.sampleRoot(tc)
		}
	})
}

func BenchmarkExportFor(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = exportFor(false, 500, "")
	}
}

// --- Ring buffer -----------------------------------------------------------

// BenchmarkSpanRingDrain measures the background goroutine's side of the
// handoff. It runs once per flush rather than per request, so its absolute cost
// matters less than the fact that it does not allocate per span: a drain that
// allocated proportionally to batch size would convert a traffic spike into GC
// pressure exactly when the process is least able to absorb it.
func BenchmarkSpanRingDrain(b *testing.B) {
	const batch = 200
	r := newSpanRing(4096)
	s := benchSpan()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := 0; j < batch; j++ {
			r.push(s)
		}
		if got := r.drain(batch); len(got) != batch {
			b.Fatalf("drained %d spans, want %d", len(got), batch)
		}
	}
}

// BenchmarkSpanRingPush is the per-request write, isolated from the Tracer.
func BenchmarkSpanRingPush(b *testing.B) {
	r := newSpanRing(4096)
	s := benchSpan()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.push(s)
	}
}

// BenchmarkSpanRingPushWhenFull is the overflow path: a service whose aggregator
// is unreachable pushes into a permanently full buffer. Dropping the oldest span
// must stay O(1) and allocation-free, because this is the state a broken fleet
// sits in indefinitely and it must never degrade the service hosting it.
func BenchmarkSpanRingPushWhenFull(b *testing.B) {
	r := newSpanRing(256)
	s := benchSpan()
	for i := 0; i < 300; i++ {
		r.push(s)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.push(s)
	}
}

// BenchmarkSpanRingPushContended is the shape that matters most: many request
// goroutines writing while the flush goroutine drains.
func BenchmarkSpanRingPushContended(b *testing.B) {
	r := newSpanRing(4096)
	s := benchSpan()

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			r.push(s)
		}
	})
}
