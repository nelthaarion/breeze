package observability

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nelthaarion/breeze/v2/events"
)

// This file covers the paths the main suite leaves open: the remaining
// Query fields, the payload renderer's type switch, and the edge cases
// around async orphan spans and buffer overflow.

// ─── Query: the remaining filters ────────────────────────────────────────

func TestQueryTimeRange(t *testing.T) {
	col := NewCollector(Config{Capacity: 10, Metrics: true})
	defer col.Close()

	base := time.Now()
	col.Publish(Signal{Name: "old", Time: base.Add(-time.Hour)})
	col.Publish(Signal{Name: "new", Time: base})

	if got := col.Find(Query{Since: base.Add(-time.Minute)}); len(got) != 1 {
		t.Errorf("Since returned %d, want 1", len(got))
	}
	if got := col.Find(Query{Until: base.Add(-time.Minute)}); len(got) != 1 {
		t.Errorf("Until returned %d, want 1", len(got))
	}
	if got := col.Find(Query{
		Since: base.Add(-2 * time.Hour),
		Until: base.Add(time.Hour),
	}); len(got) != 2 {
		t.Errorf("full range returned %d, want 2", len(got))
	}
}

func TestQuerySlowerThan(t *testing.T) {
	col := NewCollector(Config{Capacity: 10, Metrics: true})
	defer col.Close()

	col.Publish(Signal{Name: "fast", Duration: time.Microsecond})
	col.Publish(Signal{Name: "slow", Duration: time.Second})

	got := col.Find(Query{SlowerThan: time.Millisecond})
	if len(got) != 1 {
		t.Fatalf("SlowerThan returned %d, want 1", len(got))
	}
	if got[0].Name != "slow" {
		t.Errorf("matched %q, want slow", got[0].Name)
	}
}

func TestQueryCorrelationAndRequestID(t *testing.T) {
	col := NewCollector(Config{Capacity: 10, Metrics: true})
	defer col.Close()

	col.Publish(Signal{Name: "a", RequestID: "req-1", CorrelationID: "corr-1"})
	col.Publish(Signal{Name: "b", RequestID: "req-2", CorrelationID: "corr-2"})

	if got := col.Find(Query{RequestID: "req-1"}); len(got) != 1 || got[0].Name != "a" {
		t.Errorf("RequestID filter returned %v", got)
	}
	if got := col.Find(Query{CorrelationID: "corr-2"}); len(got) != 1 || got[0].Name != "b" {
		t.Errorf("CorrelationID filter returned %v", got)
	}
	if got := col.Find(Query{RequestID: "nope"}); len(got) != 0 {
		t.Errorf("unmatched RequestID returned %d, want 0", len(got))
	}
}

func TestQueryNewestWithLimitKeepsLatest(t *testing.T) {
	col := NewCollector(Config{Capacity: 100, Metrics: true})
	defer col.Close()

	for i := 0; i < 10; i++ {
		col.Publish(Signal{Name: "x", Time: time.Now()})
	}

	newest := col.Find(Query{Limit: 3, Newest: true})
	oldest := col.Find(Query{Limit: 3})
	if len(newest) != 3 || len(oldest) != 3 {
		t.Fatalf("limits not applied: %d / %d", len(newest), len(oldest))
	}
	// Newest-first must start after oldest-first ends.
	if newest[0].ID <= oldest[len(oldest)-1].ID {
		t.Error("Newest did not return the most recent matches")
	}
}

func TestTopNamesTieBreaksByName(t *testing.T) {
	col := NewCollector(Config{Capacity: 100, Metrics: true})
	defer col.Close()

	// Equal counts: the tie must break alphabetically so the dashboard
	// does not reshuffle rows between polls.
	col.Publish(Signal{Name: "beta"})
	col.Publish(Signal{Name: "alpha"})

	got := col.TopNames(0) // no limit
	if len(got) != 2 {
		t.Fatalf("TopNames returned %d, want 2", len(got))
	}
	if got[0].Name != "alpha" {
		t.Errorf("tie broke to %q, want alpha", got[0].Name)
	}
}

func TestRateExcludesOldSignals(t *testing.T) {
	col := NewCollector(Config{Capacity: 100, Metrics: true})
	defer col.Close()

	old := time.Now().Add(-time.Hour)
	for i := 0; i < 5; i++ {
		col.Publish(Signal{Name: "old", Time: old})
	}
	for i := 0; i < 3; i++ {
		col.Publish(Signal{Name: "new", Time: time.Now()})
	}

	// A ten-second window should see only the three recent signals.
	if got := col.Rate(10 * time.Second); got != 0.3 {
		t.Errorf("Rate = %v, want 0.3 (3 signals / 10s)", got)
	}
}

// Two subsystems may legitimately publish the same name. Their statistics
// must stay separate, which is what keeps the layer usable for the router,
// scheduler and database once they start publishing alongside events.
func TestMetricsAreKeyedBySourceAndName(t *testing.T) {
	col := NewCollector(Config{Capacity: 10, Metrics: true})
	defer col.Close()

	col.Publish(Signal{Source: SourceEvents, Name: "user.created", Duration: time.Millisecond})
	col.Publish(Signal{Source: SourceRouter, Name: "user.created", Duration: 2 * time.Millisecond})
	col.Publish(Signal{Source: SourceRouter, Name: "user.created", Duration: 4 * time.Millisecond})

	all := col.Metrics()
	if len(all) != 2 {
		t.Fatalf("got %d metrics, want 2 (one per source)", len(all))
	}

	ev := col.MetricForSource(SourceEvents, "user.created")
	rt := col.MetricForSource(SourceRouter, "user.created")
	if ev == nil || rt == nil {
		t.Fatal("MetricForSource returned nil for a published name")
	}
	if ev.Count != 1 {
		t.Errorf("events count = %d, want 1", ev.Count)
	}
	if rt.Count != 2 {
		t.Errorf("router count = %d, want 2", rt.Count)
	}
	if ev.Max != time.Millisecond {
		t.Errorf("events max = %v, want 1ms — durations bled across sources", ev.Max)
	}

	if col.MetricForSource(SourceCache, "user.created") != nil {
		t.Error("MetricForSource matched a source that never published")
	}
}

func TestMetricForUnknownName(t *testing.T) {

	col := NewCollector(Config{Capacity: 10, Metrics: true})
	defer col.Close()
	if m := col.MetricFor("never.published"); m != nil {
		t.Errorf("MetricFor returned %v for an unknown name, want nil", m)
	}
}

func TestMetricForReturnsCopy(t *testing.T) {
	col := NewCollector(Config{Capacity: 10, Metrics: true})
	defer col.Close()
	col.Publish(Signal{Name: "x"})

	m := col.MetricFor("x")
	m.Count = 9999 // mutate the copy

	if again := col.MetricFor("x"); again.Count == 9999 {
		t.Error("MetricFor leaked a live pointer into the collector")
	}
}

// ─── Payload renderer ────────────────────────────────────────────────────

func TestWriteValueCoversKinds(t *testing.T) {
	type inner struct{ N int }
	type all struct {
		Str   string
		B     bool
		I     int
		I8    int8
		U     uint
		U8    uint8
		F32   float32
		F64   float64
		M     map[string]int
		Sl    []string
		Arr   [2]int
		Inner inner
		Ptr   *inner
		Iface any
	}

	got := describePayload(all{
		Str: "hi", B: true, I: -1, I8: 2, U: 3, U8: 4,
		F32: 1.5, F64: 2.5,
		M:   map[string]int{"a": 1},
		Sl:  []string{"x", "y"},
		Arr: [2]int{1, 2},
		// Inner is nested to exercise the recursive branch.
		Inner: inner{N: 7},
		Ptr:   &inner{N: 8},
		Iface: 42,
	})

	for _, want := range []string{
		`"hi"`, "true", "-1", "1 keys", "2 items", "N:7",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("payload %q missing %q", got, want)
		}
	}
}

func TestWriteValueDepthLimit(t *testing.T) {
	type l4 struct{ V int }
	type l3 struct{ L4 l4 }
	type l2 struct{ L3 l3 }
	type l1 struct{ L2 l2 }
	type l0 struct{ L1 l1 }

	// Deep nesting must terminate rather than recurse without bound.
	got := describePayload(l0{})
	if !strings.Contains(got, "…") {
		t.Errorf("deep struct %q was not truncated by the depth limit", got)
	}
}

func TestWriteValueSkipsUnexported(t *testing.T) {
	type withPrivate struct {
		Public  string
		private string
	}
	got := describePayload(withPrivate{Public: "shown", private: "hidden"})
	if !strings.Contains(got, "shown") {
		t.Errorf("payload %q lost the exported field", got)
	}
	if strings.Contains(got, "hidden") {
		t.Errorf("payload %q exposed an unexported field", got)
	}
}

func TestWriteValueNilInterface(t *testing.T) {
	var iface any
	got := describePayload(struct{ I any }{iface})
	if !strings.Contains(got, "nil") {
		t.Errorf("nil interface rendered as %q", got)
	}
}

func TestDescribePayloadLongStringField(t *testing.T) {
	got := describePayload(struct{ S string }{strings.Repeat("a", 200)})
	// The per-string cap applies before the overall cap.
	if !strings.Contains(got, "…") {
		t.Errorf("long string field was not truncated: %q", got)
	}
}

// ─── Async orphan spans ──────────────────────────────────────────────────

func TestAsyncListenerPublishesOrphanSignal(t *testing.T) {
	bus, col, done := newTestSetup(t)
	defer done()

	var wg sync.WaitGroup
	wg.Add(1)
	events.OnTypeBus[UserCreated](bus, func(ctx *events.Context, e UserCreated) error {
		defer wg.Done()
		time.Sleep(5 * time.Millisecond) // finish well after the emit returns
		return nil
	}).Named("slow-async")

	events.EmitAsyncBus(bus, UserCreated{})
	wg.Wait()

	// The dispatch signal plus the listener's own orphan signal.
	if !waitFor(t, func() bool { return col.Len() >= 2 }) {
		t.Fatalf("want 2 signals, got %d", col.Len())
	}

	var orphan *Signal
	for _, s := range col.Snapshot() {
		if s.Kind == KindListener {
			cp := s
			orphan = &cp
		}
	}
	if orphan == nil {
		t.Fatal("no listener signal published for the late async listener")
	}
	if orphan.ParentID == 0 {
		t.Error("orphan signal has no ParentID linking it to its dispatch")
	}
	if !orphan.Async {
		t.Error("orphan signal not marked Async")
	}
	if len(orphan.Spans) != 1 || orphan.Spans[0].Name != "slow-async" {
		t.Errorf("orphan spans = %v", orphan.Spans)
	}
}

func TestAsyncOrphanRecordsFailure(t *testing.T) {
	bus := events.New(events.Config{
		Metrics: true,
		OnError: func(*events.Context, string, error) {},
	})
	col := NewCollector(Config{Capacity: 20, Metrics: true})
	detach := AttachEvents(bus, col)
	defer func() { detach(); col.Close(); bus.Close() }()

	var wg sync.WaitGroup
	wg.Add(1)
	events.OnTypeBus[UserCreated](bus, func(ctx *events.Context, e UserCreated) error {
		defer wg.Done()
		time.Sleep(5 * time.Millisecond)
		return errors.New("async boom")
	}).Named("failing-async")

	events.EmitAsyncBus(bus, UserCreated{})
	wg.Wait()

	found := false
	waitFor(t, func() bool {
		for _, s := range col.Snapshot() {
			if s.Kind == KindListener && s.Failed {
				found = true
				return true
			}
		}
		return false
	})
	if !found {
		t.Error("async listener failure was not recorded")
	}
}

// ─── Overflow guards ─────────────────────────────────────────────────────

func TestSpanTruncationKeepsCountAccurate(t *testing.T) {
	col := NewCollector(Config{Capacity: 10, Metrics: true})
	defer col.Close()
	o := NewEventObserver(col)

	const id = 42
	o.OnEventStart(events.DispatchInfo{EventID: id, EventName: "many"})

	// Push past the span cap.
	total := maxSpans + 25
	for i := 0; i < total; i++ {
		o.OnListenerEnd(events.ListenerOutcome{
			EventID:      id,
			EventName:    "many",
			ListenerName: "l",
		})
	}
	o.OnEventEnd(events.DispatchResult{EventID: id, EventName: "many"})

	s := col.Snapshot()[0]
	if len(s.Spans) != maxSpans {
		t.Errorf("stored %d spans, want the cap of %d", len(s.Spans), maxSpans)
	}
	// Detail is truncated, but the count still reflects reality.
	if s.Children != total {
		t.Errorf("Children = %d, want %d", s.Children, total)
	}
}

func TestInFlightOverflowIsBounded(t *testing.T) {
	col := NewCollector(Config{Capacity: 10, Metrics: true})
	defer col.Close()
	o := NewEventObserver(col)

	// Start dispatches that never end, all landing on one shard so the
	// per-shard cap is what gets exercised.
	for i := 0; i < maxInFlight+100; i++ {
		o.OnEventStart(events.DispatchInfo{
			EventID:   uint64(i * inFlightShards), // same shard every time
			EventName: "leaky",
		})
	}

	sh := o.shardFor(0)
	sh.mu.Lock()
	n := len(sh.m)
	sh.mu.Unlock()

	if n > maxInFlight {
		t.Errorf("shard holds %d entries, want at most %d", n, maxInFlight)
	}
}

func TestOnListenerStartIsInert(t *testing.T) {
	col := NewCollector(Config{Capacity: 10, Metrics: true})
	defer col.Close()
	o := NewEventObserver(col)

	// Documented as a no-op; calling it must not publish or panic.
	o.OnListenerStart(events.ListenerCall{EventID: 1, EventName: "x"})
	if col.Len() != 0 {
		t.Error("OnListenerStart published a signal")
	}
}

func TestOnEventEndWithoutStart(t *testing.T) {
	col := NewCollector(Config{Capacity: 10, Metrics: true})
	defer col.Close()
	o := NewEventObserver(col)

	// An end with no matching start still publishes, using the result's
	// own counts rather than accumulated spans.
	o.OnEventEnd(events.DispatchResult{
		EventID:           7,
		EventName:         "orphan.end",
		ListenersExecuted: 3,
	})

	s := col.Snapshot()[0]
	if s.Children != 3 {
		t.Errorf("Children = %d, want the executed count of 3", s.Children)
	}
}

// ─── Stream lifecycle ────────────────────────────────────────────────────

func TestStreamCloseIsIdempotent(t *testing.T) {
	col := NewCollector(Config{Capacity: 10, Metrics: true})
	ch, _ := col.Stream()

	col.Close()
	col.Close() // second close must not panic on an already-closed channel

	if _, open := <-ch; open {
		t.Error("subscriber channel not closed by Collector.Close")
	}
}

func TestPublishAfterCloseIsIgnored(t *testing.T) {
	col := NewCollector(Config{Capacity: 10, Metrics: true})
	col.Close()

	col.Publish(Signal{Name: "after.close"})
	if col.Len() != 0 {
		t.Error("Publish recorded a signal after Close")
	}
}

func TestSubscriberSeesIndependentCopy(t *testing.T) {
	col := NewCollector(Config{Capacity: 10, Metrics: true})
	defer col.Close()

	ch, unsub := col.Stream()
	defer unsub()

	col.Publish(Signal{
		Name:  "shared",
		Spans: []Span{{Name: "one"}},
		Attrs: map[string]string{"k": "v"},
	})

	select {
	case got := <-ch:
		// Mutating the delivered copy must not corrupt what the ring
		// buffer holds for everyone else.
		got.Spans[0].Name = "mutated"
		got.Attrs["k"] = "mutated"

		stored := col.Snapshot()[0]
		if stored.Spans[0].Name != "one" {
			t.Error("subscriber mutation leaked into the stored signal's spans")
		}
		if stored.Attrs["k"] != "v" {
			t.Error("subscriber mutation leaked into the stored signal's attrs")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no signal delivered")
	}
}

// ─── Graph edges ─────────────────────────────────────────────────────────

func TestGraphRecordsSkippedAndFailedEdges(t *testing.T) {
	col := NewCollector(Config{Capacity: 10, Metrics: true})
	defer col.Close()

	col.Publish(Signal{
		Source: SourceEvents,
		Name:   "mixed",
		Spans: []Span{
			{Name: "ok"},
			{Name: "failed", Failed: true},
			{Name: "skipped", Skipped: true},
			{Name: "panicked", Panicked: true},
		},
	})

	g := col.Graph()
	if len(g) != 1 {
		t.Fatalf("graph has %d nodes, want 1", len(g))
	}

	byName := map[string]GraphEdge{}
	for _, e := range g[0].Edges {
		byName[e.Target] = e
	}
	if byName["failed"].Failed != 1 {
		t.Error("failed edge not counted")
	}
	if byName["panicked"].Failed != 1 {
		t.Error("panicked edge should count as failed")
	}
	if byName["skipped"].Skipped != 1 {
		t.Error("skipped edge not counted")
	}
	if byName["ok"].Failed != 0 {
		t.Error("healthy edge marked failed")
	}
}

func TestGraphNodeWithoutSpans(t *testing.T) {
	col := NewCollector(Config{Capacity: 10, Metrics: true})
	defer col.Close()

	col.Publish(Signal{Source: SourceEvents, Name: "childless"})

	g := col.Graph()
	if len(g) != 1 {
		t.Fatalf("graph has %d nodes, want 1", len(g))
	}
	if len(g[0].Edges) != 0 {
		t.Errorf("node has %d edges, want none", len(g[0].Edges))
	}
	if g[0].ID == "" {
		t.Error("node ID not set")
	}
}

func TestSignalSourceAndKindConstants(t *testing.T) {
	// The constants are part of the wire format the dashboard consumes,
	// so their values are pinned rather than assumed.
	pairs := map[Source]string{
		SourceEvents:    "events",
		SourceRouter:    "router",
		SourceHTTP:      "http",
		SourceScheduler: "scheduler",
		SourceDatabase:  "database",
		SourceWebSocket: "websocket",
		SourcePlugin:    "plugin",
		SourceOAuth2:    "oauth2",
		SourceCache:     "cache",
	}
	for src, want := range pairs {
		if string(src) != want {
			t.Errorf("Source %q != %q", src, want)
		}
	}
	if string(KindDispatch) != "dispatch" {
		t.Errorf("KindDispatch = %q", KindDispatch)
	}
	if string(KindListener) != "listener" {
		t.Errorf("KindListener = %q", KindListener)
	}
}
