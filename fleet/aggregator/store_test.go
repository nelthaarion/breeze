package aggregator

// Store tests.
//
// The store's contract is not "keeps traces" — it is "keeps traces *and stays
// bounded*". An aggregator ingesting from a whole fleet is the one process here
// that sees every request in the system, so an unbounded collection is not a slow
// leak but an OOM under the first traffic spike. Every bound therefore gets a
// test that proves eviction actually happens, not merely that the config field
// exists.

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nelthaarion/breeze/v2/fleet"
)

// Identifier helpers.
//
// These exist because the store rejects ids that are not exactly 32/16 lowercase
// hex characters, and an earlier draft of these tests used labels like "t1" and
// widths like %030d — neither of which is a valid id. The tests then "passed" a
// nonexistent code path: every span was rejected at the door and every lookup
// missed, which is indistinguishable from a store that silently drops data. So
// ids are constructed in exactly one place, valid by construction, and used by
// both the write and the read side of every test.

// hexPad right-pads a hex fragment to n characters.
func hexPad(s string, n int) string {
	if len(s) >= n {
		return s[:n]
	}
	return s + strings.Repeat("0", n-len(s))
}

// tid builds a valid 32-hex trace id from a hex fragment.
func tid(hexFragment string) string { return hexPad(hexFragment, 32) }

// sid builds a valid 16-hex span id from a hex fragment.
func sid(hexFragment string) string { return hexPad(hexFragment, 16) }

// nthTID builds the i'th distinct trace id.
//
// Zero-padded on the *left*, which is what makes these ids distinct. Padding on
// the right does not: "1" and "10" both become "1" followed by zeros, so trace 0
// and trace 15 were literally the same id under an earlier version of this
// helper. That silently turned a 25-distinct-traces eviction test into a
// 24-traces test and made the eviction count look wrong when it was correct.
//
// Numbered from 1, not 0: an all-zero trace id is what a broken or uninitialized
// propagation layer emits, so the store rejects it by design, and a generator
// starting at zero would lose its first trace.
func nthTID(i int) string { return fmt.Sprintf("%032x", i+1) }

// nthSID is the span-id equivalent.
func nthSID(i int) string { return fmt.Sprintf("%016x", i+1) }

// traceSpan builds a valid span. Ids are hex fragments, expanded here.
func traceSpan(traceFragment, spanFragment, parentFragment, service string, status int) fleet.Span {
	sp := fleet.Span{
		TraceID:      tid(traceFragment),
		SpanID:       sid(spanFragment),
		Service:      service,
		Route:        "/" + service,
		Method:       "GET",
		Status:       status,
		StartNanoUTC: time.Now().UnixNano(),
		DurationMs:   5,
	}
	if parentFragment != "" {
		sp.ParentSpanID = sid(parentFragment)
	}
	return sp
}

func testStore(t *testing.T, cfg Config) SpanStore {
	t.Helper()
	return NewMemStore(cfg.withDefaults())
}

func TestStoreAddAndGet(t *testing.T) {
	s := testStore(t, Config{})
	now := time.Now()

	s.Add(traceSpan("a1", "a", "", "gateway", 200), now)
	s.Add(traceSpan("a1", "b", "a", "orders", 200), now)

	tr, ok := s.Trace(tid("a1"))
	if !ok {
		t.Fatal("stored trace not found")
	}
	if tr.SpanCount != 2 {
		t.Errorf("span count = %d, want 2", tr.SpanCount)
	}
	// Retrieval must return an assembled tree, not a flat bag: the API serves
	// this straight to the UI.
	if len(tr.Roots) != 1 || len(tr.Roots[0].Children) != 1 {
		t.Errorf("trace was not assembled into a tree: %+v", tr.Roots)
	}
}

func TestStoreMissingTrace(t *testing.T) {
	s := testStore(t, Config{})
	if _, ok := s.Trace(tid("ff")); ok {
		t.Error("Trace reported success for an id that was never stored")
	}
}

// TestStoreRejectsInvalidSpans is defence in depth. Validation also happens at
// the HTTP boundary, but every transport funnels here, and an all-zero or
// malformed trace id would either create a junk bucket or merge unrelated
// requests into one fictional trace.
func TestStoreRejectsInvalidSpans(t *testing.T) {
	s := testStore(t, Config{})
	now := time.Now()

	bad := []fleet.Span{
		{TraceID: "", SpanID: sid("b")},
		{TraceID: tid("a"), SpanID: ""},
		{TraceID: "tooshort", SpanID: sid("b")},
		{
			TraceID: strings.Repeat("0", 32),
			SpanID:  sid("b"),
		}, // all-zero: a broken propagation layer
		{TraceID: strings.Repeat("A", 32), SpanID: sid("b")}, // uppercase: not the wire format
		{TraceID: strings.Repeat("z", 32), SpanID: sid("b")}, // not hex at all
		{TraceID: tid("a"), SpanID: strings.Repeat("0", 16)}, // all-zero span id
	}
	for _, sp := range bad {
		s.Add(sp, now)
	}

	st := s.Stats()
	if st.Traces != 0 {
		t.Errorf("%d traces stored from invalid spans, want 0", st.Traces)
	}
	if st.SpansRejected != uint64(len(bad)) {
		t.Errorf("rejected counter = %d, want %d", st.SpansRejected, len(bad))
	}
}

// TestStoreEvictsOldestTraceOverCap is the core bound: an aggregator that
// ignored MaxTraces would grow until the process died.
func TestStoreEvictsOldestTraceOverCap(t *testing.T) {
	s := testStore(t, Config{MaxTraces: 10})
	now := time.Now()

	for i := 0; i < 25; i++ {
		s.Add(traceSpan(nthTID(i), "a", "", "svc", 200), now)
	}

	st := s.Stats()
	if st.Traces > 10 {
		t.Errorf("retained %d traces, cap is 10", st.Traces)
	}
	if st.TracesEvicted != 15 {
		t.Errorf("evicted %d, want 15", st.TracesEvicted)
	}
	// Oldest-first: the newest must still be present, the earliest gone.
	if _, ok := s.Trace(nthTID(24)); !ok {
		t.Error("the newest trace was evicted, so eviction is not oldest-first")
	}
	if _, ok := s.Trace(nthTID(0)); ok {
		t.Error("the oldest trace survived past the cap")
	}
}

// TestStoreDropsOldestSpansOverPerTraceCap covers the runaway-trace case: a retry
// loop inside one request must not become an unbounded allocation.
func TestStoreDropsOldestSpansOverPerTraceCap(t *testing.T) {
	s := testStore(t, Config{MaxSpansPerTrace: 5})
	now := time.Now()

	for i := 0; i < 20; i++ {
		s.Add(traceSpan("a1", nthSID(i), "", "svc", 200), now)
	}

	tr, ok := s.Trace(tid("a1"))
	if !ok {
		t.Fatal("trace missing")
	}
	if tr.SpanCount > 5 {
		t.Errorf("trace holds %d spans, cap is 5", tr.SpanCount)
	}
	if st := s.Stats(); st.SpansDropped != 15 {
		t.Errorf("dropped counter = %d, want 15", st.SpansDropped)
	}
	// The trace must report its own incompleteness, or a truncated tree reads
	// as a whole one — misleading exactly when a fan-out storm is being
	// debugged.
	if tr.SpansDropped == 0 {
		t.Error("trace does not report that spans were dropped")
	}
	// Drop-oldest: the most recent spans are the ones worth keeping, since a
	// failure usually appears at the end of a runaway trace.
	var found bool
	for _, root := range tr.Roots {
		if root.SpanID == nthSID(19) {
			found = true
		}
	}
	if !found {
		t.Error("the newest span was dropped instead of the oldest")
	}
}

func TestStoreSweepEvictsIdleTraces(t *testing.T) {
	s := testStore(t, Config{TraceTTL: time.Minute})
	old := time.Now().Add(-2 * time.Minute)
	fresh := time.Now()

	s.Add(traceSpan("a1", "a", "", "svc", 200), old)
	s.Add(traceSpan("b2", "a", "", "svc", 200), fresh)

	if n := s.Sweep(time.Now()); n != 1 {
		t.Errorf("swept %d traces, want 1", n)
	}
	if _, ok := s.Trace(tid("a1")); ok {
		t.Error("an idle trace survived the sweep")
	}
	if _, ok := s.Trace(tid("b2")); !ok {
		t.Error("the sweep took a fresh trace")
	}
	// A second sweep must be a no-op, not a repeat eviction: the counter
	// feeds the Overview page, and double-counting would misreport retention.
	if n := s.Sweep(time.Now()); n != 0 {
		t.Errorf("second sweep evicted %d, want 0", n)
	}
}

// TestStoreSweepKeepsTraceAliveWhileSpansArrive covers a long-running trace: TTL
// is measured from the last span, not the first, or a slow request would be
// evicted while still in flight.
func TestStoreSweepKeepsTraceAliveWhileSpansArrive(t *testing.T) {
	s := testStore(t, Config{TraceTTL: time.Minute})
	start := time.Now().Add(-5 * time.Minute)

	s.Add(traceSpan("a1", "a", "", "gateway", 200), start)
	s.Add(traceSpan("a1", "b", "a", "orders", 200), time.Now())

	if n := s.Sweep(time.Now()); n != 0 {
		t.Errorf("swept %d, want 0 — TTL must count from the last span", n)
	}
}

// --- Recent / filtering ----------------------------------------------------

func TestRecentReturnsNewestFirst(t *testing.T) {
	s := testStore(t, Config{})
	now := time.Now()
	for i := 0; i < 5; i++ {
		s.Add(traceSpan(nthTID(i), "a", "", "svc", 200), now)
	}

	got := s.Recent(TraceQuery{})
	if len(got) != 5 {
		t.Fatalf("got %d summaries, want 5", len(got))
	}
	if got[0].TraceID != nthTID(4) {
		t.Errorf("first row = %s, want the newest trace %s", got[0].TraceID, nthTID(4))
	}
}

// TestRecentRootServiceIsTheEntryPointNotTheFirstReporter pins the bug that
// made the trace list disagree with itself between reloads.
//
// Spans arrive in whatever order services happen to flush, which is unrelated to
// causal order: a leaf service with a shorter flush interval routinely reports
// before the gateway that called it. RootService must therefore be derived from
// parentage — the span with no parent is the entry point — and not from arrival
// position. An earlier implementation's fallback fired unconditionally on the
// first span it saw, which made the real-root check below it dead code and the
// answer a function of network timing.
func TestRecentRootServiceIsTheEntryPointNotTheFirstReporter(t *testing.T) {
	now := time.Now()

	// The gateway is the entry point (no parent) but reports last.
	root := traceSpan("a1", "a", "", "gateway", 200)
	mid := traceSpan("a1", "b", "a", "auth", 200)
	leaf := traceSpan("a1", "c", "b", "orders", 200)

	// Causal order, so arrival order is the only variable under test.
	root.StartNanoUTC = now.UnixNano()
	mid.StartNanoUTC = root.StartNanoUTC + int64(time.Millisecond)
	leaf.StartNanoUTC = mid.StartNanoUTC + int64(time.Millisecond)

	for _, arrival := range [][]fleet.Span{
		{root, mid, leaf}, // entry point first
		{leaf, mid, root}, // entry point last — the failing case
		{mid, leaf, root}, // entry point last, children out of order
	} {
		s := testStore(t, Config{})
		for _, sp := range arrival {
			s.Add(sp, now)
		}
		got := s.Recent(TraceQuery{})
		if len(got) != 1 {
			t.Fatalf("arrival %v: got %d rows, want 1", serviceOrder(arrival), len(got))
		}
		if got[0].RootService != "gateway" {
			t.Errorf(
				"arrival %v: root service = %q, want gateway — arrival order must not decide the entry point",
				serviceOrder(arrival),
				got[0].RootService,
			)
		}
		if got[0].Route != "/gateway" {
			t.Errorf(
				"arrival %v: route = %q, want the entry point's route",
				serviceOrder(arrival),
				got[0].Route,
			)
		}
	}
}

// TestRecentRootServiceFallsBackWhenNoRootReported covers the orphan case the
// fallback exists for: if the entry point never reported, the row still has to
// name something, and the earliest span is the best available answer.
func TestRecentRootServiceFallsBackWhenNoRootReported(t *testing.T) {
	s := testStore(t, Config{})
	now := time.Now()

	// Both spans claim a parent that was never reported.
	early := traceSpan("a1", "b", "a", "auth", 200)
	late := traceSpan("a1", "c", "b", "orders", 200)
	early.StartNanoUTC = now.UnixNano()
	late.StartNanoUTC = early.StartNanoUTC + int64(time.Millisecond)

	s.Add(late, now)
	s.Add(early, now)

	got := s.Recent(TraceQuery{})
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	if got[0].RootService != "auth" {
		t.Errorf(
			"root service = %q, want auth — the earliest span when no entry point reported",
			got[0].RootService,
		)
	}
}

func serviceOrder(spans []fleet.Span) []string {
	out := make([]string, len(spans))
	for i, sp := range spans {
		out[i] = sp.Service
	}
	return out
}

func TestRecentFilters(t *testing.T) {
	s := testStore(t, Config{})
	now := time.Now()

	s.Add(traceSpan("a1", "a", "", "gateway", 200), now)
	s.Add(traceSpan("b2", "a", "", "orders", 500), now)
	slow := traceSpan("c3", "a", "", "gateway", 200)
	slow.DurationMs = 500
	s.Add(slow, now)

	if got := s.Recent(
		TraceQuery{Service: "orders"},
	); len(got) != 1 ||
		got[0].RootService != "orders" {
		t.Errorf("service filter returned %+v", got)
	}
	if got := s.Recent(TraceQuery{Status: 500}); len(got) != 1 {
		t.Errorf("status filter returned %d rows, want 1", len(got))
	}
	if got := s.Recent(TraceQuery{OnlyErrors: true}); len(got) != 1 {
		t.Errorf("error filter returned %d rows, want 1", len(got))
	}
	if got := s.Recent(TraceQuery{MinDurationMs: 100}); len(got) != 1 {
		t.Errorf("duration filter returned %d rows, want 1", len(got))
	}
	if got := s.Recent(TraceQuery{Service: "nonexistent"}); len(got) != 0 {
		t.Errorf("unmatched filter returned %d rows, want 0", len(got))
	}
}

// TestRecentCapsLimit keeps one request from copying the entire store, which is
// what an unbounded limit on a live view amounts to.
func TestRecentCapsLimit(t *testing.T) {
	s := testStore(t, Config{})
	now := time.Now()
	for i := 0; i < 150; i++ {
		s.Add(traceSpan(nthTID(i), "a", "", "svc", 200), now)
	}

	if got := s.Recent(TraceQuery{Limit: 0}); len(got) != 100 {
		t.Errorf("default limit returned %d rows, want 100", len(got))
	}
	if got := s.Recent(TraceQuery{Limit: 100000}); len(got) > 500 {
		t.Errorf("absurd limit returned %d rows, want it capped", len(got))
	}
	if got := s.Recent(TraceQuery{Limit: 7}); len(got) != 7 {
		t.Errorf("explicit limit returned %d rows, want 7", len(got))
	}
}

// --- Tag index (§9C.1) -----------------------------------------------------

func TestRecentByTag(t *testing.T) {
	s := testStore(t, Config{})
	now := time.Now()

	tagged := traceSpan("a1", "a", "", "gateway", 200)
	tagged.Tags = map[string]string{"order_id": "123"}
	s.Add(tagged, now)

	other := traceSpan("b2", "a", "", "gateway", 200)
	other.Tags = map[string]string{"order_id": "999"}
	s.Add(other, now)

	s.Add(traceSpan("c3", "a", "", "gateway", 200), now)

	got := s.Recent(TraceQuery{TagKey: "order_id", TagValue: "123"})
	if len(got) != 1 {
		t.Fatalf("tag query returned %d rows, want 1: %+v", len(got), got)
	}
	if got[0].TraceID != tid("a1") {
		t.Errorf("tag query returned trace %s, want %s", got[0].TraceID, tid("a1"))
	}
	if hit := s.Recent(TraceQuery{TagKey: "order_id", TagValue: "nope"}); len(hit) != 0 {
		t.Errorf("unmatched tag returned %d rows, want 0", len(hit))
	}
}

// TestTagIndexShrinksWithEviction is the bound on the index itself. Without it
// the tag map is the one structure that grows forever in a system whose entire
// storage story is "everything is bounded".
func TestTagIndexShrinksWithEviction(t *testing.T) {
	s := testStore(t, Config{MaxTraces: 5})
	now := time.Now()

	for i := 0; i < 50; i++ {
		sp := traceSpan(nthTID(i), "a", "", "svc", 200)
		sp.Tags = map[string]string{"order_id": fmt.Sprintf("%d", i)}
		s.Add(sp, now)
	}

	ms := s.(*memStore)
	ms.tagMu.Lock()
	indexed := len(ms.tagIndex)
	ms.tagMu.Unlock()

	if indexed > 5 {
		t.Errorf(
			"tag index holds %d keys for 5 retained traces — it outlives the traces it points at",
			indexed,
		)
	}
	// An evicted trace's tag must no longer resolve, or the index would serve
	// ids that no longer exist.
	if got := s.Recent(TraceQuery{TagKey: "order_id", TagValue: "0"}); len(got) != 0 {
		t.Errorf("tag of an evicted trace still resolves: %+v", got)
	}
}

// --- Concurrency -----------------------------------------------------------

// TestStoreConcurrentAddAndRead is the test the sharding exists for. Every
// service in a fleet pushes at once while the dashboard polls, so ingest and read
// genuinely overlap; this is also the shape of bug the -race detector is here to
// find (§14.6).
func TestStoreConcurrentAddAndRead(t *testing.T) {
	s := testStore(t, Config{MaxTraces: 100, MaxSpansPerTrace: 20})

	var wg sync.WaitGroup
	const writers, readers, perWriter = 8, 4, 200

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				sp := traceSpan(nthTID((w*perWriter+i)%150), nthSID(i), "", "svc", 200)
				sp.Tags = map[string]string{"w": fmt.Sprintf("%d", w)}
				s.Add(sp, time.Now())
			}
		}(w)
	}
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				_ = s.Recent(TraceQuery{Limit: 20})
				_, _ = s.Trace(nthTID(i % 150))
				_ = s.Stats()
			}
		}()
	}
	// Sweeping concurrently too: eviction mutates the same maps the readers
	// walk, which is where a missing lock would actually bite.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			s.Sweep(time.Now())
		}
	}()
	wg.Wait()

	// The cap must hold under concurrency, not just single-threaded. An
	// earlier version released the eviction-queue lock before deleting, which
	// let concurrent writers push the live count past the cap — exactly the
	// bound this whole store exists to guarantee.
	if st := s.Stats(); st.Traces > 100 {
		t.Errorf("retained %d traces under concurrent load, cap is 100", st.Traces)
	}
}

// TestStoreStaysBoundedUnderSustainedLoad is §14.4's memory assertion in unit
// form: the structures must stay at their caps no matter how much is pushed
// through them.
func TestStoreStaysBoundedUnderSustainedLoad(t *testing.T) {
	s := testStore(t, Config{MaxTraces: 50, MaxSpansPerTrace: 10})
	now := time.Now()

	for i := 0; i < 20000; i++ {
		s.Add(traceSpan(nthTID(i%400), nthSID(i), "", "svc", 200), now)
	}

	st := s.Stats()
	if st.Traces > 50 {
		t.Errorf("traces = %d, cap is 50", st.Traces)
	}
	if max := 50 * 10; st.Spans > max {
		t.Errorf("spans = %d, hard ceiling is %d", st.Spans, max)
	}
	if st.SpansRejected != 0 {
		t.Fatalf(
			"%d spans were rejected as invalid — the test's own ids are malformed, so it proves nothing",
			st.SpansRejected,
		)
	}
}
