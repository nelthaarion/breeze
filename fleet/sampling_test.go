package fleet

// Sampling is the one part of fleet that decides what data never exists. A bug
// here is invisible in production: you do not get an error, you get a trace that
// is silently missing, or a rate that quietly does not mean what it says. These
// tests exist to pin the two properties that make the policy trustworthy —
// a configured rate really is that rate, and a decision made upstream is never
// overturned downstream.

import (
	"math"
	"testing"
)

func TestNewSamplerClampsInsteadOfTrustingConfig(t *testing.T) {
	// Sampling must never fail a service's startup, so every nonsense value
	// has to resolve to something sane. NaN is the interesting one: it
	// compares false against every threshold, so an unclamped NaN would
	// disable tracing entirely while looking like a configured rate.
	for _, tc := range []struct {
		name string
		in   float64
		want float64
	}{
		{"nan becomes the default", math.NaN(), DefaultSampleRate},
		{"negative becomes zero", -0.25, 0},
		{"above one becomes one", 1.5, 1},
		{"zero stays zero", 0, 0},
		{"one stays one", 1, 1},
		{"a real rate is preserved", 0.25, 0.25},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := newSampler(tc.in).rate; got != tc.want {
				t.Fatalf("newSampler(%v).rate = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestSampleRootHonoursTheExtremes(t *testing.T) {
	// Rate 0 and rate 1 are the two settings users actually reason about
	// ("off" and "everything"), so neither may be probabilistic at the edges.
	never := newSampler(0)
	always := newSampler(1)
	for i := 0; i < 500; i++ {
		tc := NewTraceContext()
		if never.sampleRoot(tc) {
			t.Fatal("rate 0 sampled a trace")
		}
		if !always.sampleRoot(tc) {
			t.Fatal("rate 1 skipped a trace")
		}
	}
}

func TestSampleRootIsDeterministicForATraceID(t *testing.T) {
	// The decision is derived from the trace id rather than an RNG, which is
	// what lets the same trace be reasoned about after the fact. If this ever
	// became stateful, sampling would stop being reproducible and this test
	// is the only thing that would notice.
	s := newSampler(0.5)
	for i := 0; i < 200; i++ {
		tc := NewTraceContext()
		first := s.sampleRoot(tc)
		for j := 0; j < 5; j++ {
			if s.sampleRoot(tc) != first {
				t.Fatalf("same trace id sampled inconsistently")
			}
		}
	}
}

func TestSampleRootActuallyApproximatesTheConfiguredRate(t *testing.T) {
	// The real risk in deriving a decision from trace-id bits is a skewed
	// mapping — a "0.25" that is really 0.5, or 0.12. That would be
	// invisible without a statistical check, because every individual
	// decision looks perfectly plausible either way.
	const n = 20000
	const tolerance = 0.02

	for _, rate := range []float64{0.1, 0.25, 0.5, 0.9} {
		s := newSampler(rate)
		hits := 0
		for i := 0; i < n; i++ {
			if s.sampleRoot(NewTraceContext()) {
				hits++
			}
		}
		got := float64(hits) / n
		if math.Abs(got-rate) > tolerance {
			t.Fatalf("rate %v sampled %.4f of traces, outside ±%v", rate, got, tolerance)
		}
	}
}

func TestDecideNeverOverturnsAnUpstreamDecision(t *testing.T) {
	// This is the property that keeps traces whole. If a downstream service
	// re-rolled the dice, traces would come out with holes in the middle —
	// and a hole is indistinguishable from a service that crashed, which is
	// a far more alarming thing to see in a UI than a trace that was simply
	// never sampled.
	for _, rate := range []float64{0, 0.5, 1} {
		s := newSampler(rate)

		sampledUpstream := NewTraceContext()
		sampledUpstream.Sampled = true
		if !s.decide(sampledUpstream, true) {
			t.Fatalf("rate %v dropped a trace upstream had already sampled", rate)
		}

		notSampledUpstream := NewTraceContext()
		notSampledUpstream.Sampled = false
		if s.decide(notSampledUpstream, false /* inherited */) != s.sampleRoot(notSampledUpstream) {
			t.Fatal("a root decision did not come from the sampler")
		}
		if s.decide(notSampledUpstream, true) {
			t.Fatalf("rate %v sampled a trace upstream had declined", rate)
		}
	}
}

func TestExportForAlwaysReportsFailuresEvenWhenUnsampled(t *testing.T) {
	// §7's rule: the sample budget governs *detail*, never whether a failure
	// is reported at all. An unsampled 500 still produces a span, just a thin
	// one — because the expensive capture was declined at request start and
	// cannot be conjured up afterwards.
	for _, tc := range []struct {
		name    string
		sampled bool
		status  int
		errText string
		want    exportKind
	}{
		{"sampled success exports in full", true, 200, "", exportFull},
		{"sampled failure exports in full", true, 500, "boom", exportFull},
		{"unsampled success is dropped", false, 200, "", exportNone},
		{"unsampled 5xx still reports", false, 503, "", exportLightweight},
		{"unsampled error text still reports", false, 200, "recovered panic", exportLightweight},
		{"a 4xx is not a failure", false, 404, "", exportNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := exportFor(tc.sampled, tc.status, tc.errText); got != tc.want {
				t.Fatalf("exportFor(%v, %d, %q) = %v, want %v",
					tc.sampled, tc.status, tc.errText, got, tc.want)
			}
		})
	}
}
