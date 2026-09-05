package observability

import (
	"testing"

	"github.com/nelthaarion/breeze/events"
)

// The benchmarks in this file exist to defend one specific claim: that
// attaching observability is opt-in, and that a bus with no observer pays
// nothing for the hook's existence.
//
// Read them in pairs. BenchmarkDispatch_NoObserver against
// BenchmarkDispatch_Observer is the number that matters; the rest break
// down where the attached cost goes.

type benchEvent struct {
	ID   uint64
	Name string
}

func noopHandler(ctx *events.Context, e benchEvent) error { return nil }

// benchBus builds a bus with n listeners and no observer.
func benchBus(n int) *events.Bus {
	bus := events.New()
	for i := 0; i < n; i++ {
		events.OnTypeBus[benchEvent](bus, noopHandler)
	}
	return bus
}

// ─── The headline pair ───────────────────────────────────────────────────

// BenchmarkDispatch_NoObserver is the baseline: the cost of a dispatch on
// a bus that nobody is watching. This is what a production application
// pays when the dashboard is not attached.
func BenchmarkDispatch_NoObserver(b *testing.B) {
	bus := benchBus(1)
	defer bus.Close()

	e := benchEvent{ID: 1, Name: "bench"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = events.EmitBus(bus, e)
	}
}

// BenchmarkDispatch_Observer is the same dispatch with the collector
// attached. The delta against the baseline is the true cost of
// observability.
func BenchmarkDispatch_Observer(b *testing.B) {
	bus := benchBus(1)
	col := NewCollector(Config{Capacity: 1000, Metrics: true})
	detach := AttachEvents(bus, col)
	defer func() { detach(); col.Close(); bus.Close() }()

	e := benchEvent{ID: 1, Name: "bench"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = events.EmitBus(bus, e)
	}
}

// BenchmarkDispatch_ObserverNoMetrics isolates the metrics aggregation
// from the rest of the observer's work.
func BenchmarkDispatch_ObserverNoMetrics(b *testing.B) {
	bus := benchBus(1)
	col := NewCollector(Config{Capacity: 1000}) // Metrics off
	detach := AttachEvents(bus, col)
	defer func() { detach(); col.Close(); bus.Close() }()

	e := benchEvent{ID: 1, Name: "bench"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = events.EmitBus(bus, e)
	}
}

// BenchmarkDispatch_ObserverWithPayload measures the added cost of boxing
// the event and rendering it through the masker.
func BenchmarkDispatch_ObserverWithPayload(b *testing.B) {
	bus := benchBus(1)
	col := NewCollector(Config{Capacity: 1000, Metrics: true})
	detach := AttachEventsWithPayload(bus, col)
	defer func() { detach(); col.Close(); bus.Close() }()

	e := benchEvent{ID: 1, Name: "bench"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = events.EmitBus(bus, e)
	}
}

// BenchmarkDispatch_AfterDetach confirms that detaching restores the
// baseline rather than leaving residual cost behind. It should match
// BenchmarkDispatch_NoObserver.
func BenchmarkDispatch_AfterDetach(b *testing.B) {
	bus := benchBus(1)
	col := NewCollector(Config{Capacity: 1000, Metrics: true})
	detach := AttachEvents(bus, col)
	detach()
	defer func() { col.Close(); bus.Close() }()

	e := benchEvent{ID: 1, Name: "bench"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = events.EmitBus(bus, e)
	}
}

// ─── Scaling with listener count ─────────────────────────────────────────

func benchmarkListeners(b *testing.B, n int, observe bool) {
	bus := benchBus(n)
	var col *Collector
	if observe {
		col = NewCollector(Config{Capacity: 1000, Metrics: true})
		detach := AttachEvents(bus, col)
		defer detach()
		defer col.Close()
	}
	defer bus.Close()

	e := benchEvent{ID: 1, Name: "bench"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = events.EmitBus(bus, e)
	}
}

func BenchmarkListeners1_NoObserver(b *testing.B)    { benchmarkListeners(b, 1, false) }
func BenchmarkListeners1_Observer(b *testing.B)      { benchmarkListeners(b, 1, true) }
func BenchmarkListeners10_NoObserver(b *testing.B)   { benchmarkListeners(b, 10, false) }
func BenchmarkListeners10_Observer(b *testing.B)     { benchmarkListeners(b, 10, true) }
func BenchmarkListeners100_NoObserver(b *testing.B)  { benchmarkListeners(b, 100, false) }
func BenchmarkListeners100_Observer(b *testing.B)    { benchmarkListeners(b, 100, true) }
func BenchmarkListeners1000_NoObserver(b *testing.B) { benchmarkListeners(b, 1000, false) }
func BenchmarkListeners1000_Observer(b *testing.B)   { benchmarkListeners(b, 1000, true) }

// ─── Collector internals ─────────────────────────────────────────────────

// BenchmarkPublish measures the collector in isolation, with no bus
// involved, so the ring buffer and metrics cost can be read directly.
func BenchmarkPublish(b *testing.B) {
	col := NewCollector(Config{Capacity: 1000, Metrics: true})
	defer col.Close()

	sig := Signal{
		Source:   SourceEvents,
		Kind:     KindDispatch,
		Name:     "bench.signal",
		Executed: 1,
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		col.Publish(sig)
	}
}

// BenchmarkPublishWithSpans measures publishing with per-listener detail,
// which is the realistic shape for a multi-listener dispatch.
func BenchmarkPublishWithSpans(b *testing.B) {
	col := NewCollector(Config{Capacity: 1000, Metrics: true})
	defer col.Close()

	sig := Signal{
		Source:   SourceEvents,
		Kind:     KindDispatch,
		Name:     "bench.signal",
		Executed: 4,
		Children: 4,
		Spans: []Span{
			{Name: "one"}, {Name: "two"}, {Name: "three"}, {Name: "four"},
		},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		col.Publish(sig)
	}
}

// BenchmarkPublishWithSubscriber measures the fan-out cost with a live
// subscriber draining the channel, which is the dashboard's steady state.
func BenchmarkPublishWithSubscriber(b *testing.B) {
	col := NewCollector(Config{Capacity: 1000, Metrics: true})
	defer col.Close()

	ch, unsub := col.Stream()
	defer unsub()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range ch {
		}
	}()

	sig := Signal{Source: SourceEvents, Kind: KindDispatch, Name: "bench.signal"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		col.Publish(sig)
	}
	b.StopTimer()
	unsub()
	<-done
}

// BenchmarkSnapshot measures the read path the dashboard uses on every
// poll.
func BenchmarkSnapshot(b *testing.B) {
	col := NewCollector(Config{Capacity: 1000, Metrics: true})
	defer col.Close()
	for i := 0; i < 1000; i++ {
		col.Publish(Signal{Source: SourceEvents, Name: "bench.signal"})
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = col.Snapshot()
	}
}

// BenchmarkFind measures a filtered query over a full buffer.
func BenchmarkFind(b *testing.B) {
	col := NewCollector(Config{Capacity: 1000, Metrics: true})
	defer col.Close()
	for i := 0; i < 1000; i++ {
		name := "bench.a"
		if i%2 == 0 {
			name = "bench.b"
		}
		col.Publish(Signal{Source: SourceEvents, Name: name})
	}

	q := Query{Name: "bench.a", Limit: 50, Newest: true}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = col.Find(q)
	}
}

// BenchmarkGraph measures building the graph view.
func BenchmarkGraph(b *testing.B) {
	col := NewCollector(Config{Capacity: 1000, Metrics: true})
	defer col.Close()
	for i := 0; i < 200; i++ {
		col.Publish(Signal{
			Source: SourceEvents,
			Name:   "bench.signal",
			Spans:  []Span{{Name: "one"}, {Name: "two"}},
		})
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = col.Graph()
	}
}

// BenchmarkDescribePayload measures the masking reflection walk, which
// runs only when payload capture is enabled.
func BenchmarkDescribePayload(b *testing.B) {
	e := Credentials{Username: "alice", Password: "hunter2", APIKey: "sk-x"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = describePayload(e)
	}
}

// BenchmarkIsSensitive measures the field-name check, which runs once per
// field of every captured payload.
func BenchmarkIsSensitive(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = IsSensitive("user_id")
		_ = IsSensitive("password")
	}
}

// ─── Concurrency ─────────────────────────────────────────────────────────

// BenchmarkPublishParallel measures contention on the collector when many
// goroutines publish at once. This is the case the sharded in-flight map
// and the short critical sections are designed for.
func BenchmarkPublishParallel(b *testing.B) {
	col := NewCollector(Config{Capacity: 1000, Metrics: true})
	defer col.Close()

	sig := Signal{Source: SourceEvents, Kind: KindDispatch, Name: "bench.signal"}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			col.Publish(sig)
		}
	})
}

// BenchmarkDispatchParallel_NoObserver and its Observer counterpart show
// how the observer behaves under concurrent dispatch.
func BenchmarkDispatchParallel_NoObserver(b *testing.B) {
	bus := benchBus(1)
	defer bus.Close()

	e := benchEvent{ID: 1, Name: "bench"}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = events.EmitBus(bus, e)
		}
	})
}

func BenchmarkDispatchParallel_Observer(b *testing.B) {
	bus := benchBus(1)
	col := NewCollector(Config{Capacity: 1000, Metrics: true})
	detach := AttachEvents(bus, col)
	defer func() { detach(); col.Close(); bus.Close() }()

	e := benchEvent{ID: 1, Name: "bench"}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = events.EmitBus(bus, e)
		}
	})
}
