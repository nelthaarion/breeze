package observability

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nelthaarion/breeze/v2/events"
)

// ─── Fixtures ────────────────────────────────────────────────────────────

type UserCreated struct {
	UserID uint64
}

type OrderPlaced struct {
	OrderID uint64
	Amount  float64
}

// Credentials exercises the payload masker: Password must never appear in
// a rendered payload.
type Credentials struct {
	Username string
	Password string
	APIKey   string
}

// newTestSetup returns an isolated bus and collector, already attached.
func newTestSetup(t *testing.T) (*events.Bus, *Collector, func()) {
	t.Helper()
	bus := events.New(events.Config{Metrics: true})
	col := NewCollector(Config{Capacity: 100, Metrics: true})
	detach := AttachEvents(bus, col)
	return bus, col, func() {
		detach()
		col.Close()
		bus.Close()
	}
}

// waitFor polls cond until it holds or the deadline passes. Async
// listeners publish from their own goroutines, so assertions on them need
// a bounded wait rather than a fixed sleep.
func waitFor(t *testing.T, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return cond()
}

// ─── Signal capture ──────────────────────────────────────────────────────

func TestPublishesSignalPerDispatch(t *testing.T) {
	bus, col, done := newTestSetup(t)
	defer done()

	events.OnTypeBus[UserCreated](bus, func(ctx *events.Context, e UserCreated) error { return nil })

	if err := events.EmitBus(bus, UserCreated{UserID: 7}); err != nil {
		t.Fatalf("emit: %v", err)
	}

	sigs := col.Snapshot()
	if len(sigs) != 1 {
		t.Fatalf("want 1 signal, got %d", len(sigs))
	}
	s := sigs[0]
	if s.Source != SourceEvents {
		t.Errorf("Source = %q, want %q", s.Source, SourceEvents)
	}
	if s.Kind != KindDispatch {
		t.Errorf("Kind = %q, want %q", s.Kind, KindDispatch)
	}
	if !strings.Contains(s.Name, "UserCreated") {
		t.Errorf("Name = %q, want it to mention UserCreated", s.Name)
	}
	if s.Executed != 1 {
		t.Errorf("Executed = %d, want 1", s.Executed)
	}
	if s.Failed {
		t.Error("Failed = true, want false")
	}
	if s.ID == 0 {
		t.Error("ID not assigned")
	}
	if s.SourceID == 0 {
		t.Error("SourceID not carried from the dispatch")
	}
}

func TestSignalCarriesListenerSpans(t *testing.T) {
	bus, col, done := newTestSetup(t)
	defer done()

	events.OnTypeBus[UserCreated](bus, func(ctx *events.Context, e UserCreated) error { return nil }).Named("first")
	events.OnTypeBus[UserCreated](bus, func(ctx *events.Context, e UserCreated) error { return nil }).Named("second")

	events.EmitBus(bus, UserCreated{})

	s := col.Snapshot()[0]
	if len(s.Spans) != 2 {
		t.Fatalf("want 2 spans, got %d", len(s.Spans))
	}
	if s.Children != 2 {
		t.Errorf("Children = %d, want 2", s.Children)
	}
	names := []string{s.Spans[0].Name, s.Spans[1].Name}
	if names[0] != "first" || names[1] != "second" {
		t.Errorf("span names = %v, want [first second]", names)
	}
}

func TestSpansFollowPriorityOrder(t *testing.T) {
	bus, col, done := newTestSetup(t)
	defer done()

	events.OnTypeBus[UserCreated](bus, func(ctx *events.Context, e UserCreated) error { return nil }).
		Named("low").Priority(10)
	events.OnTypeBus[UserCreated](bus, func(ctx *events.Context, e UserCreated) error { return nil }).
		Named("high").Priority(100)

	events.EmitBus(bus, UserCreated{})

	s := col.Snapshot()[0]
	if s.Spans[0].Name != "high" {
		t.Errorf("first span = %q, want high", s.Spans[0].Name)
	}
	if s.Spans[0].Priority != 100 {
		t.Errorf("first span priority = %d, want 100", s.Spans[0].Priority)
	}
}

func TestFailureIsRecorded(t *testing.T) {
	bus, col, done := newTestSetup(t)
	defer done()

	boom := errors.New("boom")
	events.OnTypeBus[UserCreated](bus, func(ctx *events.Context, e UserCreated) error { return boom }).Named("bad")

	events.EmitBus(bus, UserCreated{})

	s := col.Snapshot()[0]
	if !s.Failed {
		t.Error("Failed = false, want true")
	}
	if !strings.Contains(s.Err, "boom") {
		t.Errorf("Err = %q, want it to mention boom", s.Err)
	}
	if len(s.Spans) != 1 || !s.Spans[0].Failed {
		t.Error("span not marked failed")
	}
}

func TestPanicIsRecorded(t *testing.T) {
	bus := events.New(events.Config{
		Metrics: true,
		OnPanic: func(*events.PanicError) {},
	})
	col := NewCollector(Config{Capacity: 10, Metrics: true})
	detach := AttachEvents(bus, col)
	defer func() { detach(); col.Close(); bus.Close() }()

	events.OnTypeBus[UserCreated](bus, func(ctx *events.Context, e UserCreated) error {
		panic("kaboom")
	}).Named("panicky")

	events.EmitBus(bus, UserCreated{})

	s := col.Snapshot()[0]
	if len(s.Spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(s.Spans))
	}
	if !s.Spans[0].Panicked {
		t.Error("span Panicked = false, want true")
	}
	if !s.Spans[0].Failed {
		t.Error("a panicked span should also count as failed")
	}
}

func TestStopIsCancelledNotFailed(t *testing.T) {
	bus, col, done := newTestSetup(t)
	defer done()

	events.OnTypeBus[UserCreated](bus, func(ctx *events.Context, e UserCreated) error {
		return events.Stop
	}).Named("stopper")
	events.OnTypeBus[UserCreated](bus, func(ctx *events.Context, e UserCreated) error {
		t.Error("listener after Stop should not run")
		return nil
	})

	events.EmitBus(bus, UserCreated{})

	s := col.Snapshot()[0]
	if !s.Cancelled {
		t.Error("Cancelled = false, want true")
	}
	if s.Failed {
		t.Error("stopping propagation must not be reported as a failure")
	}
	if !s.Spans[0].Stopped {
		t.Error("span Stopped = false, want true")
	}
	if s.Spans[0].Failed {
		t.Error("the stopping span must not be marked failed")
	}
}

func TestFilteredListenerIsMarkedSkipped(t *testing.T) {
	bus, col, done := newTestSetup(t)
	defer done()

	events.OnTypeBus[UserCreated](bus, func(ctx *events.Context, e UserCreated) error {
		t.Error("filtered listener should not execute")
		return nil
	}).Named("filtered").Where(func(e UserCreated) bool { return e.UserID > 100 })

	events.EmitBus(bus, UserCreated{UserID: 1})

	s := col.Snapshot()[0]
	if len(s.Spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(s.Spans))
	}
	if !s.Spans[0].Skipped {
		t.Error("span Skipped = false, want true")
	}
	if s.Executed != 0 {
		t.Errorf("Executed = %d, want 0", s.Executed)
	}
}

func TestAsyncDispatchIsRecorded(t *testing.T) {
	bus, col, done := newTestSetup(t)
	defer done()

	var ran sync.WaitGroup
	ran.Add(1)
	events.OnTypeBus[UserCreated](bus, func(ctx *events.Context, e UserCreated) error {
		ran.Done()
		return nil
	}).Named("async-listener")

	if err := events.EmitAsyncBus(bus, UserCreated{}); err != nil {
		t.Fatalf("emit async: %v", err)
	}
	ran.Wait()

	if !waitFor(t, func() bool { return col.Len() >= 1 }) {
		t.Fatal("no signal published for async dispatch")
	}
	found := false
	for _, s := range col.Snapshot() {
		if s.Async {
			found = true
		}
	}
	if !found {
		t.Error("no signal marked Async")
	}
}

// ─── Metrics ─────────────────────────────────────────────────────────────

func TestMetricsAccumulate(t *testing.T) {
	bus, col, done := newTestSetup(t)
	defer done()

	events.Name[UserCreated](bus, "user.created")
	events.OnTypeBus[UserCreated](bus, func(ctx *events.Context, e UserCreated) error { return nil })

	for i := 0; i < 5; i++ {
		events.EmitBus(bus, UserCreated{UserID: uint64(i)})
	}

	m := col.MetricFor("user.created")
	if m == nil {
		t.Fatal("no metric recorded for user.created")
	}
	if m.Count != 5 {
		t.Errorf("Count = %d, want 5", m.Count)
	}
	if m.Executed != 5 {
		t.Errorf("Executed = %d, want 5", m.Executed)
	}
	// Durations are asserted for consistency rather than a floor: a
	// trivial dispatch can legitimately measure as 0ns on a coarse clock.
	if m.Avg < 0 {
		t.Errorf("Avg = %v, want >= 0", m.Avg)
	}
	if m.Min > m.Max {
		t.Errorf("Min %v > Max %v", m.Min, m.Max)
	}
	if m.Total < m.Max {
		t.Errorf("Total %v < Max %v", m.Total, m.Max)
	}
	if m.AvgMS < 0 {
		t.Error("AvgMS negative")
	}
	if m.Last.IsZero() {
		t.Error("Last not set")
	}
}

func TestStatsTotals(t *testing.T) {
	bus, col, done := newTestSetup(t)
	defer done()

	events.OnTypeBus[UserCreated](bus, func(ctx *events.Context, e UserCreated) error { return nil })
	events.OnTypeBus[OrderPlaced](bus, func(ctx *events.Context, e OrderPlaced) error {
		return errors.New("nope")
	})

	events.EmitBus(bus, UserCreated{})
	events.EmitBus(bus, OrderPlaced{})

	st := col.Stats()
	if st.Signals != 2 {
		t.Errorf("Signals = %d, want 2", st.Signals)
	}
	if st.Failed != 1 {
		t.Errorf("Failed = %d, want 1", st.Failed)
	}
}

func TestFailureRate(t *testing.T) {
	m := Metric{Count: 4, Failed: 1}
	if got := m.FailureRate(); got != 0.25 {
		t.Errorf("FailureRate = %v, want 0.25", got)
	}
	if got := (Metric{}).FailureRate(); got != 0 {
		t.Errorf("empty FailureRate = %v, want 0", got)
	}
}

// ─── Query ───────────────────────────────────────────────────────────────

func TestQueryFilters(t *testing.T) {
	bus, col, done := newTestSetup(t)
	defer done()

	events.Name[UserCreated](bus, "user.created")
	events.Name[OrderPlaced](bus, "order.placed")
	events.OnTypeBus[UserCreated](bus, func(ctx *events.Context, e UserCreated) error { return nil })
	events.OnTypeBus[OrderPlaced](bus, func(ctx *events.Context, e OrderPlaced) error {
		return errors.New("declined")
	})

	events.EmitBus(bus, UserCreated{})
	events.EmitBus(bus, OrderPlaced{})
	events.EmitBus(bus, UserCreated{})

	if got := col.Find(Query{Name: "user.created"}); len(got) != 2 {
		t.Errorf("Name filter returned %d, want 2", len(got))
	}
	if got := col.Find(Query{FailedOnly: true}); len(got) != 1 {
		t.Errorf("FailedOnly returned %d, want 1", len(got))
	}
	if got := col.Find(Query{Source: SourceEvents}); len(got) != 3 {
		t.Errorf("Source filter returned %d, want 3", len(got))
	}
	if got := col.Find(Query{Source: SourceRouter}); len(got) != 0 {
		t.Errorf("wrong-source filter returned %d, want 0", len(got))
	}
	if got := col.Find(Query{NameContains: "ORDER"}); len(got) != 1 {
		t.Errorf("NameContains should be case-insensitive, got %d", len(got))
	}
	if got := col.Find(Query{Limit: 2}); len(got) != 2 {
		t.Errorf("Limit returned %d, want 2", len(got))
	}
}

func TestRecentReturnsNewestFirst(t *testing.T) {
	bus, col, done := newTestSetup(t)
	defer done()

	events.OnTypeBus[UserCreated](bus, func(ctx *events.Context, e UserCreated) error { return nil })
	for i := 0; i < 3; i++ {
		events.EmitBus(bus, UserCreated{UserID: uint64(i)})
	}

	got := col.Recent(2)
	if len(got) != 2 {
		t.Fatalf("Recent(2) returned %d", len(got))
	}
	if got[0].ID < got[1].ID {
		t.Error("Recent should return newest first")
	}
}

func TestByID(t *testing.T) {
	bus, col, done := newTestSetup(t)
	defer done()

	events.OnTypeBus[UserCreated](bus, func(ctx *events.Context, e UserCreated) error { return nil })
	events.EmitBus(bus, UserCreated{})

	want := col.Snapshot()[0]
	got, ok := col.ByID(want.ID)
	if !ok {
		t.Fatal("ByID did not find the signal")
	}
	if got.ID != want.ID {
		t.Errorf("ByID returned id %d, want %d", got.ID, want.ID)
	}
	if _, ok := col.ByID(99999); ok {
		t.Error("ByID found a signal that does not exist")
	}
}

func TestSlowestAndTopNames(t *testing.T) {
	bus, col, done := newTestSetup(t)
	defer done()

	events.Name[UserCreated](bus, "user.created")
	events.OnTypeBus[UserCreated](bus, func(ctx *events.Context, e UserCreated) error { return nil })
	events.OnTypeBus[OrderPlaced](bus, func(ctx *events.Context, e OrderPlaced) error {
		time.Sleep(2 * time.Millisecond)
		return nil
	})

	events.EmitBus(bus, UserCreated{})
	events.EmitBus(bus, UserCreated{})
	events.EmitBus(bus, OrderPlaced{})

	slow := col.Slowest(1)
	if len(slow) != 1 {
		t.Fatalf("Slowest(1) returned %d", len(slow))
	}
	if !strings.Contains(slow[0].Name, "Order") {
		t.Errorf("slowest = %q, want the OrderPlaced dispatch", slow[0].Name)
	}

	top := col.TopNames(1)
	if len(top) != 1 || top[0].Name != "user.created" {
		t.Errorf("TopNames = %v, want user.created first", top)
	}
}

func TestClearAndReset(t *testing.T) {
	bus, col, done := newTestSetup(t)
	defer done()

	events.OnTypeBus[UserCreated](bus, func(ctx *events.Context, e UserCreated) error { return nil })
	events.EmitBus(bus, UserCreated{})

	col.Clear()
	if col.Len() != 0 {
		t.Error("Clear did not empty the ring buffer")
	}
	if len(col.Metrics()) == 0 {
		t.Error("Clear must retain metrics")
	}

	col.Reset()
	if len(col.Metrics()) != 0 {
		t.Error("Reset did not clear metrics")
	}
	if len(col.Graph()) != 0 {
		t.Error("Reset did not clear the graph")
	}
}

func TestRingBufferEvicts(t *testing.T) {
	bus := events.New()
	col := NewCollector(Config{Capacity: 3, Metrics: true})
	detach := AttachEvents(bus, col)
	defer func() { detach(); col.Close(); bus.Close() }()

	events.OnTypeBus[UserCreated](bus, func(ctx *events.Context, e UserCreated) error { return nil })
	for i := 0; i < 10; i++ {
		events.EmitBus(bus, UserCreated{UserID: uint64(i)})
	}

	if col.Len() != 3 {
		t.Errorf("Len = %d, want the capacity of 3", col.Len())
	}
	// Lifetime metrics survive eviction.
	if col.Stats().Signals != 10 {
		t.Errorf("Stats.Signals = %d, want 10", col.Stats().Signals)
	}
}

// ─── Graph ───────────────────────────────────────────────────────────────

func TestGraphReflectsObservedExecution(t *testing.T) {
	bus, col, done := newTestSetup(t)
	defer done()

	events.Name[UserCreated](bus, "user.created")
	events.OnTypeBus[UserCreated](bus, func(ctx *events.Context, e UserCreated) error { return nil }).
		Named("validate").Priority(100)
	events.OnTypeBus[UserCreated](bus, func(ctx *events.Context, e UserCreated) error { return nil }).
		Named("save").Priority(50)

	events.EmitBus(bus, UserCreated{})

	g := col.Graph()
	if len(g) != 1 {
		t.Fatalf("graph has %d nodes, want 1", len(g))
	}
	if g[0].Name != "user.created" {
		t.Errorf("node name = %q", g[0].Name)
	}
	if len(g[0].Edges) != 2 {
		t.Fatalf("node has %d edges, want 2", len(g[0].Edges))
	}
	if g[0].Edges[0].Target != "validate" {
		t.Errorf("first edge = %q, want validate (higher priority)", g[0].Edges[0].Target)
	}
}

func TestNamesIsSortedAndDeduped(t *testing.T) {
	bus, col, done := newTestSetup(t)
	defer done()

	events.Name[UserCreated](bus, "user.created")
	events.Name[OrderPlaced](bus, "order.placed")
	events.OnTypeBus[UserCreated](bus, func(ctx *events.Context, e UserCreated) error { return nil })
	events.OnTypeBus[OrderPlaced](bus, func(ctx *events.Context, e OrderPlaced) error { return nil })

	events.EmitBus(bus, UserCreated{})
	events.EmitBus(bus, UserCreated{})
	events.EmitBus(bus, OrderPlaced{})

	got := col.Names()
	if len(got) != 2 {
		t.Fatalf("Names = %v, want 2 entries", got)
	}
	if got[0] != "order.placed" || got[1] != "user.created" {
		t.Errorf("Names = %v, want sorted", got)
	}
}

func TestDedupe(t *testing.T) {
	got := dedupe([]string{"a", "a", "b", "b", "b", "c"})
	if len(got) != 3 {
		t.Errorf("dedupe = %v, want 3 entries", got)
	}
	if len(dedupe([]string{"only"})) != 1 {
		t.Error("dedupe mishandled a single-element slice")
	}
	if len(dedupe(nil)) != 0 {
		t.Error("dedupe mishandled nil")
	}
}

// ─── Stream ──────────────────────────────────────────────────────────────

func TestStreamDeliversLiveSignals(t *testing.T) {
	bus, col, done := newTestSetup(t)
	defer done()

	ch, unsub := col.Stream()
	defer unsub()

	events.OnTypeBus[UserCreated](bus, func(ctx *events.Context, e UserCreated) error { return nil })
	events.EmitBus(bus, UserCreated{UserID: 42})

	select {
	case s := <-ch:
		if !strings.Contains(s.Name, "UserCreated") {
			t.Errorf("streamed name = %q", s.Name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no signal arrived on the stream")
	}
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	bus, col, done := newTestSetup(t)
	defer done()

	ch, unsub := col.Stream()
	if col.Subscribers() != 1 {
		t.Fatalf("Subscribers = %d, want 1", col.Subscribers())
	}
	unsub()
	if col.Subscribers() != 0 {
		t.Errorf("Subscribers = %d after unsubscribe, want 0", col.Subscribers())
	}
	// The channel must be closed, not merely detached.
	if _, open := <-ch; open {
		t.Error("channel still open after unsubscribe")
	}
	// Unsubscribing twice must not panic.
	unsub()

	events.OnTypeBus[UserCreated](bus, func(ctx *events.Context, e UserCreated) error { return nil })
	events.EmitBus(bus, UserCreated{})
}

func TestSlowSubscriberDropsRatherThanBlocks(t *testing.T) {
	bus, col, done := newTestSetup(t)
	defer done()

	// Subscribe and never read.
	_, unsub := col.Stream()
	defer unsub()

	events.OnTypeBus[UserCreated](bus, func(ctx *events.Context, e UserCreated) error { return nil })

	// Publish well past the subscriber buffer. If publishing blocked on a
	// full subscriber, this would hang and the test would time out.
	for i := 0; i < subscriberBuffer+50; i++ {
		events.EmitBus(bus, UserCreated{UserID: uint64(i)})
	}

	if col.Dropped() == 0 {
		t.Error("expected drops for a subscriber that never reads")
	}
}

func TestClosedCollectorStopsPublishing(t *testing.T) {
	bus := events.New()
	col := NewCollector(Config{Capacity: 10, Metrics: true})
	detach := AttachEvents(bus, col)
	defer func() { detach(); bus.Close() }()

	events.OnTypeBus[UserCreated](bus, func(ctx *events.Context, e UserCreated) error { return nil })
	col.Close()
	col.Close() // idempotent

	events.EmitBus(bus, UserCreated{})
	if col.Len() != 0 {
		t.Error("a closed collector must not record signals")
	}

	// Subscribing to a closed collector yields a closed channel.
	ch, _ := col.Stream()
	if _, open := <-ch; open {
		t.Error("stream on a closed collector should be closed")
	}
}

// ─── Masking ─────────────────────────────────────────────────────────────

func TestIsSensitive(t *testing.T) {
	sensitive := []string{
		"password", "Password", "user_password", "APIKey", "api_key",
		"Authorization", "X-Auth-Token", "session_id", "client_secret",
		"refresh_token", "csrf", "PIN",
	}
	for _, k := range sensitive {
		if !IsSensitive(k) {
			t.Errorf("IsSensitive(%q) = false, want true", k)
		}
	}
	for _, k := range []string{"user_id", "amount", "email", "name", "count"} {
		if IsSensitive(k) {
			t.Errorf("IsSensitive(%q) = true, want false", k)
		}
	}
}

func TestMaskAttrs(t *testing.T) {
	got := MaskAttrs(map[string]string{
		"user_id":  "42",
		"password": "hunter2",
	})
	if got["user_id"] != "42" {
		t.Errorf("non-sensitive value altered: %q", got["user_id"])
	}
	if got["password"] == "hunter2" {
		t.Error("password was not masked")
	}
	if got["password"] != maskedValue {
		t.Errorf("mask = %q, want %q", got["password"], maskedValue)
	}
	if MaskAttrs(nil) != nil {
		t.Error("MaskAttrs(nil) should return nil")
	}
}

func TestPayloadCaptureMasksSecrets(t *testing.T) {
	bus := events.New()
	col := NewCollector(Config{Capacity: 10, Metrics: true})
	detach := AttachEventsWithPayload(bus, col)
	defer func() { detach(); col.Close(); bus.Close() }()

	events.OnTypeBus[Credentials](bus, func(ctx *events.Context, e Credentials) error { return nil })
	events.EmitBus(bus, Credentials{
		Username: "alice",
		Password: "hunter2",
		APIKey:   "sk-live-abc123",
	})

	s := col.Snapshot()[0]
	payload := s.Attrs["payload"]
	if payload == "" {
		t.Fatal("payload not captured")
	}
	if !strings.Contains(payload, "alice") {
		t.Errorf("payload lost the non-sensitive field: %q", payload)
	}
	if strings.Contains(payload, "hunter2") {
		t.Errorf("PASSWORD LEAKED into payload: %q", payload)
	}
	if strings.Contains(payload, "sk-live-abc123") {
		t.Errorf("API KEY LEAKED into payload: %q", payload)
	}
}

func TestNoPayloadCaptureByDefault(t *testing.T) {
	bus, col, done := newTestSetup(t)
	defer done()

	events.OnTypeBus[Credentials](bus, func(ctx *events.Context, e Credentials) error { return nil })
	events.EmitBus(bus, Credentials{Password: "hunter2"})

	s := col.Snapshot()[0]
	if s.Attrs["payload"] != "" {
		t.Errorf("payload captured without opting in: %q", s.Attrs["payload"])
	}
}

func TestDescribePayloadShapes(t *testing.T) {
	if got := describePayload(nil); got != "" {
		t.Errorf("nil payload = %q, want empty", got)
	}
	if got := describePayload(UserCreated{UserID: 5}); !strings.Contains(got, "5") {
		t.Errorf("struct payload = %q, want it to include 5", got)
	}
	// A long string is truncated rather than stored whole.
	long := describePayload(struct{ Body string }{strings.Repeat("x", 500)})
	if len(long) > maxPayloadChars+8 {
		t.Errorf("payload not truncated: %d chars", len(long))
	}
	// Slices report their length, not their contents.
	if got := describePayload(struct{ Items []int }{[]int{1, 2, 3}}); !strings.Contains(got, "3 items") {
		t.Errorf("slice payload = %q, want an item count", got)
	}
	// A nil pointer must not panic.
	var p *UserCreated
	if got := describePayload(struct{ P *UserCreated }{p}); !strings.Contains(got, "nil") {
		t.Errorf("nil pointer payload = %q", got)
	}
}

// ─── Observer resilience ─────────────────────────────────────────────────

// panickyObserver models a buggy third-party observer.
type panickyObserver struct{}

func (o *panickyObserver) OnEventStart(events.DispatchInfo)     { panic("observer start boom") }
func (o *panickyObserver) OnEventEnd(events.DispatchResult)     { panic("observer end boom") }
func (o *panickyObserver) OnListenerStart(events.ListenerCall)  {}
func (o *panickyObserver) OnListenerEnd(events.ListenerOutcome) {}

func TestPanickingObserverDoesNotBreakDispatch(t *testing.T) {
	var panics int
	var mu sync.Mutex
	bus := events.New(events.Config{
		OnPanic: func(*events.PanicError) {
			mu.Lock()
			panics++
			mu.Unlock()
		},
	})
	defer bus.Close()

	bus.SetObserver(&panickyObserver{})

	ran := false
	events.OnTypeBus[UserCreated](bus, func(ctx *events.Context, e UserCreated) error {
		ran = true
		return nil
	})

	// The dispatch must complete normally despite the observer panicking.
	if err := events.EmitBus(bus, UserCreated{}); err != nil {
		t.Errorf("emit returned %v, want nil", err)
	}
	if !ran {
		t.Error("listener did not run because the observer panicked")
	}
	mu.Lock()
	got := panics
	mu.Unlock()
	if got == 0 {
		t.Error("observer panic was not reported to OnPanic")
	}
}

func TestDetachRestoresCleanBus(t *testing.T) {
	bus := events.New()
	col := NewCollector(Config{Capacity: 10, Metrics: true})
	defer col.Close()
	defer bus.Close()

	detach := AttachEvents(bus, col)
	if !bus.ObserverEnabled() {
		t.Error("ObserverEnabled = false after attach")
	}
	if bus.Observer() == nil {
		t.Error("Observer() = nil after attach")
	}

	detach()
	if bus.ObserverEnabled() {
		t.Error("ObserverEnabled = true after detach")
	}
	if bus.Observer() != nil {
		t.Error("Observer() non-nil after detach")
	}

	events.OnTypeBus[UserCreated](bus, func(ctx *events.Context, e UserCreated) error { return nil })
	events.EmitBus(bus, UserCreated{})
	if col.Len() != 0 {
		t.Error("collector still receiving signals after detach")
	}
}

func TestAttachToNilCollectorUsesDefault(t *testing.T) {
	o := NewEventObserver(nil)
	if o.col != Default() {
		t.Error("a nil collector should fall back to Default()")
	}
}

// ─── Concurrency ─────────────────────────────────────────────────────────

func TestConcurrentDispatchAndRead(t *testing.T) {
	bus, col, done := newTestSetup(t)
	defer done()

	events.OnTypeBus[UserCreated](bus, func(ctx *events.Context, e UserCreated) error { return nil })

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Emitters.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				events.EmitBus(bus, UserCreated{UserID: uint64(id*200 + j)})
			}
		}(i)
	}

	// Readers, hammering every read path while dispatches are in flight.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					col.Snapshot()
					col.Metrics()
					col.Stats()
					col.Graph()
					col.Names()
					col.Recent(10)
					col.Slowest(5)
					col.TopNames(5)
					col.Rate(time.Second)
					col.Find(Query{FailedOnly: true})
				}
			}
		}()
	}

	// Subscribers churning while signals flow.
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				ch, unsub := col.Stream()
				select {
				case <-ch:
				case <-time.After(time.Millisecond):
				}
				unsub()
			}
		}()
	}

	// Wait for emitters, then stop readers.
	go func() {
		time.Sleep(300 * time.Millisecond)
		close(stop)
	}()
	wg.Wait()
	select {
	case <-stop:
	default:
		close(stop)
	}

	if col.Stats().Signals == 0 {
		t.Error("no signals recorded under concurrency")
	}
}

func TestConcurrentAttachDetach(t *testing.T) {
	bus := events.New()
	col := NewCollector(Config{Capacity: 50, Metrics: true})
	defer func() { col.Close(); bus.Close() }()

	events.OnTypeBus[UserCreated](bus, func(ctx *events.Context, e UserCreated) error { return nil })

	var wg sync.WaitGroup

	// Flip the observer on and off while dispatches run. This is the race
	// the atomic pointer exists to make safe.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			detach := AttachEvents(bus, col)
			detach()
		}
	}()

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				events.EmitBus(bus, UserCreated{UserID: uint64(j)})
			}
		}()
	}

	wg.Wait()
}

func TestRingBufferConcurrent(t *testing.T) {
	r := newRingBuffer[int](64)
	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				r.Push(base + j)
			}
		}(i * 500)
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				r.Snapshot()
				r.Len()
			}
		}()
	}
	wg.Wait()

	if r.Len() != 64 {
		t.Errorf("Len = %d, want 64", r.Len())
	}
	if r.Cap() != 64 {
		t.Errorf("Cap = %d, want 64", r.Cap())
	}
}

func TestRingBufferOrderAndClear(t *testing.T) {
	r := newRingBuffer[int](3)
	for i := 1; i <= 5; i++ {
		r.Push(i)
	}
	got := r.Snapshot()
	want := []int{3, 4, 5} // oldest first, after eviction
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Snapshot = %v, want %v", got, want)
			break
		}
	}
	r.Clear()
	if r.Len() != 0 {
		t.Error("Clear did not empty the buffer")
	}
	// A zero capacity is coerced to something usable rather than panicking.
	if newRingBuffer[int](0).Cap() != 1 {
		t.Error("zero capacity should be coerced to 1")
	}
}

// ─── Config ──────────────────────────────────────────────────────────────

func TestConfigDefaults(t *testing.T) {
	col := NewCollector(Config{})
	defer col.Close()
	if col.Config().Capacity != 1000 {
		t.Errorf("default Capacity = %d, want 1000", col.Config().Capacity)
	}
}

func TestMetricsDisabled(t *testing.T) {
	bus := events.New()
	col := NewCollector(Config{Capacity: 10}) // Metrics off
	detach := AttachEvents(bus, col)
	defer func() { detach(); col.Close(); bus.Close() }()

	events.OnTypeBus[UserCreated](bus, func(ctx *events.Context, e UserCreated) error { return nil })
	events.EmitBus(bus, UserCreated{})

	if len(col.Metrics()) != 0 {
		t.Error("metrics recorded while disabled")
	}
	// Signals are still retained; only the aggregation is skipped.
	if col.Len() != 1 {
		t.Errorf("Len = %d, want 1", col.Len())
	}
}

func TestErrSinkIsOptional(t *testing.T) {
	col := NewCollector(Config{})
	defer col.Close()
	col.report(errors.New("no sink configured")) // must not panic

	var got error
	col2 := NewCollector(Config{ErrSink: func(err error) { got = err }})
	defer col2.Close()
	col2.report(errors.New("captured"))
	if got == nil {
		t.Error("ErrSink was not called")
	}
}

func TestRateWindow(t *testing.T) {
	bus, col, done := newTestSetup(t)
	defer done()

	events.OnTypeBus[UserCreated](bus, func(ctx *events.Context, e UserCreated) error { return nil })
	for i := 0; i < 10; i++ {
		events.EmitBus(bus, UserCreated{})
	}

	if r := col.Rate(time.Minute); r <= 0 {
		t.Errorf("Rate = %v, want > 0", r)
	}
	if r := col.Rate(0); r != 0 {
		t.Errorf("Rate(0) = %v, want 0", r)
	}
}

// TestDefaultCollectorIsStable pins the package-level collector to one instance.
//
// The two calls are bound to variables before the comparison rather than
// compared inline. `Default() != Default()` is SA4000 — staticcheck reads the
// identical expressions on both sides as a probable typo, and it is right to:
// an assertion written that way tells a reader nothing about which call is
// expected to differ. Naming them makes the claim explicit and makes the failure
// message able to say what it got.
//
// The concurrent half is the part that would actually catch a regression. A
// Default() built on a plain nil check instead of sync.Once passes the
// sequential comparison every time — the race only appears when two goroutines
// reach the check before either assigns.
func TestDefaultCollectorIsStable(t *testing.T) {
	first := Default()
	second := Default()

	if first == nil {
		t.Fatal("Default() = nil, want a collector")
	}
	if first != second {
		t.Errorf("Default() returned %p then %p, want the same collector", first, second)
	}

	const goroutines = 16
	got := make([]*Collector, goroutines)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i] = Default()
		}(i)
	}
	wg.Wait()

	for i, c := range got {
		if c != first {
			t.Errorf("goroutine %d got %p, want %p — Default() is not race-safe", i, c, first)
		}
	}
}
