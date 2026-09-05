package aggregator

// Assembly tests.
//
// These are hand-constructed span trees (§14.12) because assembly is where a
// wrong answer is most expensive: this code decides what a person looking at an
// incident is told broke. A root-cause marking that points at the wrong service
// sends someone to debug a victim instead of the cause, which is worse than
// showing three undifferentiated red rows and admitting nothing is known.

import (
	"strings"
	"testing"

	"github.com/nelthaarion/breeze/v2/fleet"
)

// span builds a span with plausible ids, so tests read as trees rather than hex.
//
// Ids go through the same sid/hexPad helpers as store_test.go, so a test can
// assert on sid("c") and get the id the fixture actually produced. Building them
// two different ways is how an assertion ends up comparing "c" against
// "c000000000000000" and failing on a correct result.
func span(id, parent, service string, startMs int64, durMs float64, status int) fleet.Span {
	sp := fleet.Span{
		TraceID: strings.Repeat("a", 32),
		SpanID:  sid(id),
		Service: service,

		Route:        "/" + service,
		Method:       "GET",
		Status:       status,
		StartNanoUTC: startMs * int64(1e6),
		DurationMs:   durMs,
	}
	if parent != "" {
		sp.ParentSpanID = sid(parent)
	}

	return sp
}

func TestAssembleLinksParentChild(t *testing.T) {
	tr := Assemble(strings.Repeat("a", 32), []fleet.Span{
		span("b", "a", "auth", 10, 5, 200),
		span("a", "", "gateway", 0, 40, 200),
		span("c", "b", "orders", 20, 10, 200),
	})

	if len(tr.Roots) != 1 {
		t.Fatalf("got %d roots, want 1: %+v", len(tr.Roots), tr.Roots)
	}
	root := tr.Roots[0]
	if root.Service != "gateway" {
		t.Errorf("root service = %q, want gateway", root.Service)
	}
	if len(root.Children) != 1 || root.Children[0].Service != "auth" {
		t.Fatalf("gateway's children = %+v, want [auth]", root.Children)
	}
	if kids := root.Children[0].Children; len(kids) != 1 || kids[0].Service != "orders" {
		t.Fatalf("auth's children = %+v, want [orders]", kids)
	}
	if tr.OrphanCount != 0 {
		t.Errorf("orphan count = %d, want 0", tr.OrphanCount)
	}
	if want := []string{"auth", "gateway", "orders"}; strings.Join(tr.Services, ",") != strings.Join(want, ",") {
		t.Errorf("services = %v, want %v", tr.Services, want)
	}
}

// TestAssembleDurationIsWallClockNotSum is the arithmetic that makes a trace
// readable: hops overlap, so summing durations would report a 40ms request as
// 55ms and make every trace look slower than it was.
func TestAssembleDurationIsWallClockNotSum(t *testing.T) {
	tr := Assemble(strings.Repeat("a", 32), []fleet.Span{
		span("a", "", "gateway", 0, 40, 200),
		span("b", "a", "auth", 10, 5, 200),
		span("c", "a", "orders", 20, 10, 200),
	})
	if tr.DurationMs != 40 {
		t.Errorf("duration = %vms, want 40 (wall clock, not the 55ms sum)", tr.DurationMs)
	}
}

// TestAssembleKeepsOrphans is §8.1.3: the spans of a service whose parent never
// reported are exactly the ones worth seeing, since a crashed parent is why the
// parent is missing.
func TestAssembleKeepsOrphans(t *testing.T) {
	tr := Assemble(strings.Repeat("a", 32), []fleet.Span{
		span("b", "missing", "orders", 10, 5, 200),
		span("c", "b", "notifications", 12, 2, 200),
	})

	if tr.SpanCount != 2 {
		t.Errorf("span count = %d, want 2 — no span may be dropped", tr.SpanCount)
	}
	if tr.OrphanCount != 1 {
		t.Errorf("orphan count = %d, want 1", tr.OrphanCount)
	}
	if len(tr.Roots) != 1 || !tr.Roots[0].Orphan {
		t.Fatalf("orphan was not re-rooted and flagged: %+v", tr.Roots)
	}
	// The orphan's own subtree must survive intact, or killing one service
	// would erase everything it called.
	if len(tr.Roots[0].Children) != 1 {
		t.Errorf("orphan lost its children: %+v", tr.Roots[0].Children)
	}
}

func TestAssembleEmpty(t *testing.T) {
	tr := Assemble(strings.Repeat("a", 32), nil)
	if tr.SpanCount != 0 || len(tr.Roots) != 0 || tr.HasError {
		t.Errorf("empty trace = %+v, want zero values", tr)
	}
}

// TestAssembleDeduplicatesRetriedSpans matters because the Tracer requeues a
// batch whose HTTP response was lost. Without dedupe, every such retry doubles
// the nodes in a tree.
func TestAssembleDeduplicatesRetriedSpans(t *testing.T) {
	dup := span("a", "", "gateway", 0, 10, 200)
	tr := Assemble(strings.Repeat("a", 32), []fleet.Span{dup, dup, dup})

	if tr.SpanCount != 1 {
		t.Errorf("span count = %d, want 1 after dedupe", tr.SpanCount)
	}
	if len(tr.Roots) != 1 {
		t.Errorf("got %d roots, want 1", len(tr.Roots))
	}
}

// TestAssembleSelfParentDoesNotHang guards the tree walk against a span that
// claims itself as its parent. Untreated this is an infinite loop in a request
// handler.
func TestAssembleSelfParentDoesNotHang(t *testing.T) {
	self := span("a", "a", "weird", 0, 1, 200)
	tr := Assemble(strings.Repeat("a", 32), []fleet.Span{self})
	if len(tr.Roots) != 1 || !tr.Roots[0].Orphan {
		t.Fatalf("self-parented span = %+v, want one flagged orphan", tr.Roots)
	}
}

// TestAssembleCycleStillRenders covers mutually-parented spans: no span has an
// absent parent, so nothing becomes a root by the normal rule, and without the
// fallback the trace would disappear entirely.
func TestAssembleCycleStillRenders(t *testing.T) {
	tr := Assemble(strings.Repeat("a", 32), []fleet.Span{
		span("a", "b", "one", 0, 5, 200),
		span("b", "a", "two", 1, 5, 200),
	})
	if len(tr.Roots) == 0 {
		t.Fatal("a cyclic trace rendered no roots, so it would vanish from the UI")
	}
	if tr.SpanCount != 2 {
		t.Errorf("span count = %d, want 2", tr.SpanCount)
	}
}

// --- Clock skew (§8.1.4) ---------------------------------------------------

func TestAssembleFlagsClockSkew(t *testing.T) {
	// The child claims to have started 5ms before its parent — impossible
	// causally, so the two machines disagree about the time.
	tr := Assemble(strings.Repeat("a", 32), []fleet.Span{
		span("a", "", "gateway", 100, 40, 200),
		span("b", "a", "auth", 95, 5, 200),
	})

	if !tr.SkewFlagged {
		t.Error("trace not flagged despite a child starting before its parent")
	}
	child := tr.Roots[0].Children[0]
	if !child.Skewed {
		t.Error("skewed span not marked, so the UI would render negative latency silently")
	}
}

func TestAssembleDoesNotFlagNormalTiming(t *testing.T) {
	tr := Assemble(strings.Repeat("a", 32), []fleet.Span{
		span("a", "", "gateway", 100, 40, 200),
		span("b", "a", "auth", 105, 5, 200),
	})
	if tr.SkewFlagged {
		t.Error("a normally-ordered trace was flagged as skewed — false positives train people to ignore the badge")
	}
}

// --- Root cause (§9B.1) ----------------------------------------------------

func TestRootCauseSingleError(t *testing.T) {
	tr := Assemble(strings.Repeat("a", 32), []fleet.Span{
		span("a", "", "gateway", 0, 40, 200),
		span("b", "a", "orders", 10, 20, 500),
	})

	if tr.RootCauseSpanID == "" {
		t.Fatal("a failing trace has no root cause marked")
	}
	failing := tr.Roots[0].Children[0]
	if !failing.RootCause {
		t.Error("the failing span is not marked as the root cause")
	}
	if failing.DerivedError {
		t.Error("the root cause is also marked as derived, which contradicts itself")
	}
	if !tr.HasError {
		t.Error("HasError is false for a trace containing a 500")
	}
}

// TestRootCauseCascadingFailure is the case the whole feature exists for: three
// services report errors, and only one of them actually broke.
//
// The gateway starts first and fails last, because it sits waiting on the callees
// that failed underneath it. Blaming the earliest-*starting* failure therefore
// always accuses the outermost caller — which is the manual correlation this
// feature exists to remove, inverted. The origin is the failure with nothing
// broken beneath it.
func TestRootCauseCascadingFailure(t *testing.T) {
	tr := Assemble(strings.Repeat("a", 32), []fleet.Span{
		span("a", "", "gateway", 0, 100, 500),
		span("b", "a", "notifications", 40, 20, 500),
		span("c", "a", "orders", 30, 40, 500),
	})

	// Two independent leaf failures; orders started first, so between
	// siblings — neither waiting on the other — it is the origin.
	if tr.RootCauseSpanID != sid("c") {
		t.Errorf("root cause = %q, want %q (orders: earliest failure with no failing callee)",
			tr.RootCauseSpanID, sid("c"))
	}
	for _, root := range tr.Roots {
		walk(root, func(_, n *SpanNode) {
			if n.SpanID == sid("a") && n.RootCause {

				t.Error("gateway marked root cause: it was waiting on failing callees, so its 500 is inherited")
			}
		})
	}

	var derived, causes int
	for _, root := range tr.Roots {
		walk(root, func(_, n *SpanNode) {
			if n.RootCause {
				causes++
			}
			if n.DerivedError {
				derived++
			}
		})
	}
	if causes != 1 {
		t.Errorf("%d spans marked root cause, want exactly 1", causes)
	}
	if derived != 2 {
		t.Errorf("%d spans marked derived, want 2", derived)
	}
}

// TestRootCauseDeepSynchronousCascade pins the shape observed live in
// cmd/fleet-example: gateway → auth → orders, where orders is the only service
// that actually broke and the two above it merely relayed its 500.
//
// This is the regression guard for ranking failures by start time. The gateway
// starts first in every synchronous trace, so that rule reported "gateway failed"
// for a chaos failure injected into orders — pointing the reader at the top of
// the tree while the cause sat at the bottom. Written as a deliberately deeper
// chain than the sibling case above, because the bug only shows once the caller
// fully encloses its callee in time, which is exactly what a real blocking call
// does.
func TestRootCauseDeepSynchronousCascade(t *testing.T) {
	tr := Assemble(strings.Repeat("a", 32), []fleet.Span{
		span("a", "", "gateway", 0, 30, 500),       // starts first, fails last
		span("b", "a", "auth-service", 5, 20, 500), // relays orders' failure
		span("c", "b", "orders-service", 10, 8, 500),
	})

	if tr.RootCauseSpanID != sid("c") {
		t.Errorf("root cause = %q, want %q (orders-service: the deepest failure)",
			tr.RootCauseSpanID, sid("c"))
	}

	byID := map[string]*SpanNode{}
	for _, root := range tr.Roots {
		walk(root, func(_, n *SpanNode) { byID[n.SpanID] = n })
	}
	for _, id := range []string{"a", "b"} {
		n := byID[sid(id)]

		if n == nil {
			t.Fatalf("span %s missing from the assembled tree", id)
		}
		if n.RootCause {
			t.Errorf("%s marked root cause; it was waiting on a failing callee", n.Service)
		}
		if !n.DerivedError {
			t.Errorf("%s not marked derived; its 500 came from downstream", n.Service)
		}
	}
	if !strings.HasPrefix(tr.Summary, "orders-service failed") {
		t.Errorf("summary blames the wrong service:\n%s", tr.Summary)
	}
}

func TestRootCauseAbsentWhenNothingFailed(t *testing.T) {

	tr := Assemble(strings.Repeat("a", 32), []fleet.Span{
		span("a", "", "gateway", 0, 40, 200),
		span("b", "a", "auth", 10, 5, 204),
	})
	if tr.RootCauseSpanID != "" {
		t.Errorf("root cause = %q on a clean trace, want empty", tr.RootCauseSpanID)
	}
	if tr.Summary != "" {
		t.Errorf("summary = %q on a clean trace, want empty", tr.Summary)
	}
	for _, root := range tr.Roots {
		walk(root, func(_, n *SpanNode) {
			if n.RootCause || n.DerivedError {
				t.Errorf("span %s marked on a clean trace", n.SpanID)
			}
		})
	}
}

// TestRootCauseCountsErrorFieldNotJustStatus covers a handler that returned 200
// while recording an error — a timeout swallowed by a fallback still needs to
// surface.
func TestRootCauseCountsErrorFieldNotJustStatus(t *testing.T) {
	s := span("a", "", "gateway", 0, 40, 200)
	s.Error = "upstream timeout"
	tr := Assemble(strings.Repeat("a", 32), []fleet.Span{s})

	if !tr.HasError || tr.RootCauseSpanID == "" {
		t.Error("a span with an error message but a 200 status was treated as clean")
	}
}

// --- Summary (§9B.1) -------------------------------------------------------

// TestSummaryStatesTheFacts reproduces the example summary from §9B.1.
//
// The fixture is a relay chain — analytics → notifications → orders — because
// that is the causality the sentence describes: orders is the leaf that actually
// broke, and the two services above it in the chain failed *because* it did.
// "Downstream" in the summary means "affected by", not "callee of"; an earlier
// version of this fixture hung the two affected services *underneath* orders,
// which asserts the opposite causality (orders failed because they did) while
// claiming the wording of this one.
func TestSummaryStatesTheFacts(t *testing.T) {
	cause := span("c", "d", "orders-service", 30, 4200, 500)
	cause.Route = "/orders/:id/charge"
	cause.Method = "POST"
	cause.Error = "payment_provider timeout"

	tr := Assemble(strings.Repeat("a", 32), []fleet.Span{
		span("e", "", "analytics-service", 10, 4290, 500),
		span("d", "e", "notifications-service", 20, 4270, 500),
		cause,
	})

	for _, want := range []string{
		"orders-service", "POST", "/orders/:id/charge",
		"500", "payment_provider timeout", "4200ms",
		"notifications-service", "analytics-service",
		"2 downstream services",
	} {
		if !strings.Contains(tr.Summary, want) {
			t.Errorf("summary is missing %q:\n%s", want, tr.Summary)
		}
	}
}

// TestSummaryCountsServicesNotSpans keeps a fan-out from inflating the reported
// blast radius: eight failing spans in one service is one affected service.
func TestSummaryCountsServicesNotSpans(t *testing.T) {
	spans := []fleet.Span{span("a", "", "gateway", 0, 100, 500)}
	for i := 0; i < 8; i++ {
		spans = append(spans, span(string(rune('b'+i)), "a", "orders", int64(10+i), 5, 500))
	}
	tr := Assemble(strings.Repeat("a", 32), spans)

	if !strings.Contains(tr.Summary, "1 downstream service ") {
		t.Errorf("summary should count 1 affected service, not 8 spans:\n%s", tr.Summary)
	}
}

func TestSummarySingularForOneService(t *testing.T) {
	tr := Assemble(strings.Repeat("a", 32), []fleet.Span{
		span("a", "", "gateway", 0, 100, 500),
		span("b", "a", "orders", 10, 5, 500),
	})
	if strings.Contains(tr.Summary, "1 downstream services") {
		t.Errorf("summary uses a plural for one service:\n%s", tr.Summary)
	}
}

// --- Status semantics ------------------------------------------------------

// TestTraceStatusIsTheEntryPoints pins a deliberate decision: the user got a 200,
// so the trace is a 200, even though something inside failed. HasError still
// flags it, so the failure is not hidden — but the list must not claim the caller
// saw an error they did not see.
func TestTraceStatusIsTheEntryPoints(t *testing.T) {
	tr := Assemble(strings.Repeat("a", 32), []fleet.Span{
		span("a", "", "gateway", 0, 40, 200),
		span("b", "a", "recommendations", 10, 5, 500),
	})
	if tr.Status != 200 {
		t.Errorf("status = %d, want the entry point's 200", tr.Status)
	}
	if !tr.HasError {
		t.Error("HasError must still be true so a degraded-but-successful request is findable")
	}
}
