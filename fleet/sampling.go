package fleet

// Sampling decides which requests pay for tracing. This file owns that decision
// (§7) and nothing else, so the policy can be read and tested in one place
// rather than inferred from scattered branches in the middleware.
//
// # Two decisions, not one
//
// There are genuinely two questions here, and conflating them is what makes
// sampling confusing in most tracing systems:
//
//  1. At request *start*: do we pay for expensive capture (nested timeline,
//     request/response payloads)? This must be decided up front, because you
//     cannot retroactively record steps that already ran.
//  2. At request *end*: do we export a span at all? This can react to the
//     outcome, because by then the outcome is known.
//
// Keeping them separate is what lets §7's "always sample errors" rule work
// without either lying about what was captured or forcing full capture on every
// request just in case it fails.

import "encoding/binary"

// DefaultSampleRate traces everything. A framework that silently dropped data by
// default would make "why is my trace missing?" the first question every new
// user asks; explicit opt-down is kinder than implicit loss.
const DefaultSampleRate = 1.0

// sampler holds a validated rate.
//
// A type rather than a bare float64 so the clamping in newSampler happens once
// at construction, instead of every caller having to remember that a
// misconfigured 1.5 or -0.2 must not be trusted.
type sampler struct {
	// rate is clamped to [0,1]. 0 means errors-only (see exportFor), 1 means
	// everything.
	rate float64
}

// newSampler validates and clamps a configured rate.
//
// Out-of-range values are clamped rather than rejected because sampling is not
// worth failing a service's startup over: a typo'd 1.5 obviously means "all",
// and refusing to boot over it would be a worse outcome than tracing more than
// intended. NaN is treated as "unset" and becomes the default, since NaN
// compares false against everything and would otherwise silently disable
// tracing entirely.
func newSampler(rate float64) sampler {
	switch {
	case rate != rate: // NaN
		return sampler{rate: DefaultSampleRate}
	case rate <= 0:
		return sampler{rate: 0}
	case rate >= 1:
		return sampler{rate: 1}
	default:
		return sampler{rate: rate}
	}
}

// sampleRoot decides whether a brand-new trace is sampled.
//
// The dice are the trace id itself. It is already 16 crypto-random bytes
// generated microseconds earlier by NewTraceContext, so reusing its low half as
// the random draw costs one load and no RNG call, allocates nothing, and needs
// no lock or per-P state on a path taken by every request that arrives without a
// traceparent.
//
// It also makes the decision *deterministic for a given trace id*, which is the
// property that matters if this ever needs to be reproduced or cross-checked:
// the same trace id always samples the same way at the same rate. This is the
// same trace-id-ratio approach OpenTelemetry uses, arrived at here for the
// cheapness rather than the compatibility.
//
// Only the top 53 bits are used, so the comparison is exact in float64 and
// cannot overflow the way `uint64(rate * 1<<64)` does as rate approaches 1.
func (s sampler) sampleRoot(tc TraceContext) bool {
	if s.rate <= 0 {
		return false
	}
	if s.rate >= 1 {
		return true
	}
	// Low 8 bytes: the high half is equally random, but taking the low half
	// keeps this obviously independent of any future scheme that might
	// encode structure into the leading bytes of a trace id.
	v := binary.BigEndian.Uint64(tc.TraceID[8:])
	// >>11 leaves 53 bits, the exact integer range of a float64 mantissa.
	return float64(v>>11)/float64(uint64(1)<<53) < s.rate
}

// decide returns the sampling decision for the request currently arriving.
//
// The rule that matters: if the request carried a valid trace context, the
// upstream decision is *inherited*, never re-rolled. A trace is sampled as a
// whole or not at all — re-deciding per hop would produce traces with holes in
// the middle, which is worse than either sampling or dropping the whole thing,
// because a hole looks identical to a service that failed to report.
//
// Only the first service to see a request (the one that finds no usable
// traceparent) actually rolls the dice.
func (s sampler) decide(tc TraceContext, inherited bool) bool {
	if inherited {
		return tc.Sampled
	}
	return s.sampleRoot(tc)
}

// exportKind is what to do with a finished request.
type exportKind uint8

const (
	// exportNone drops the span. The request was not sampled and did not
	// fail, so it contributes nothing the aggregate counters do not already
	// carry.
	exportNone exportKind = iota

	// exportLightweight sends the span's identity, timing, and error, but
	// no timeline and no payloads.
	//
	// This is §7's always-sample-errors rule. A failed request is worth
	// knowing about even when the sample budget said no — but the expensive
	// parts were never captured, because that decision was made at request
	// start and cannot be undone. So the span is real and correctly
	// parent-linked, just thin.
	exportLightweight

	// exportFull sends everything captured, including timeline and any
	// payloads.
	exportFull
)

// exportFor decides what to do with a finished request.
//
// Deliberately takes only the three facts it needs rather than a *requestState,
// so the policy can be exhaustively unit-tested without constructing a request.
func exportFor(sampled bool, status int, errText string) exportKind {

	if sampled {
		return exportFull
	}
	if status >= 500 || errText != "" {
		return exportLightweight
	}
	return exportNone
}
