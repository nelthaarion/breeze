package aggregator

// Aggregator benchmarks (§12.5, §12.7).
//
// The aggregator is the one process that sees every request in the fleet, so its
// ingest path is the system's throughput ceiling and its read path is what the
// dashboard polls. Two properties are being defended here:
//
//   - §12.5 — ingest throughput, so the documented spans/sec ceiling is a measured
//     number rather than a guess. v1 is a single process by design, which makes
//     that ceiling a real operational limit someone has to plan around.
//   - §12.7 — ingest cost must not grow with what is already stored. Assembly,
//     sorting, and tree-building belong on the read path; if any of them leaked
//     into Add, a long-lived trace or a full store would slow ingest down for
//     every service at once.
//
// Run:
//
//	go test -run '^$' -bench . -benchmem ./fleet/aggregator/

import (
	"fmt"
	"testing"
	"time"

	"github.com/nelthaarion/breeze/v2/fleet"
)

// --- §12.5: ingest throughput ----------------------------------------------

// BenchmarkStoreAddNewTraces is the worst realistic ingest case: every span
// starts a new trace, so every call takes the slow path that serializes on the
// eviction queue. A fleet at steady state is a mix of this and the fast path
// below, and the gap between the two is the cost of that serialization.
func BenchmarkStoreAddNewTraces(b *testing.B) {
	s := NewMemStore(Config{}.withDefaults())
	sp := traceSpan("a1", "a", "", "gateway", 200)
	now := time.Now()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sp.TraceID = fmt.Sprintf("%032x", i+1)
		s.Add(sp, now)
	}
}

// BenchmarkStoreAddExistingTrace is the fast path — a span joining a trace some
// earlier span created, which is what most spans do. This one must stay sharded
// and cheap; it is the number that actually sets the ingest ceiling.
func BenchmarkStoreAddExistingTrace(b *testing.B) {
	s := NewMemStore(Config{MaxSpansPerTrace: 1 << 20}.withDefaults())
	sp := traceSpan("a1", "a", "", "gateway", 200)
	now := time.Now()
	s.Add(sp, now)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sp.SpanID = fmt.Sprintf("%016x", i+1)
		s.Add(sp, now)
	}
}

// BenchmarkStoreAddParallel is the shape that matters: many services POST
// batches concurrently, and each batch is unpacked into Add calls. This is what
// the sharding exists to make scale, so a flat line here as -cpu rises would mean
// the shard count is not doing its job.
func BenchmarkStoreAddParallel(b *testing.B) {
	s := NewMemStore(Config{MaxTraces: 5000}.withDefaults())
	now := time.Now()

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			i++
			sp := traceSpan("a1", "a", "", "gateway", 200)
			// Spread across traces so writers land on different shards,
			// which is the realistic pattern: concurrent requests are
			// almost never the same trace.
			sp.TraceID = fmt.Sprintf("%032x", i)
			s.Add(sp, now)
		}
	})
}

// BenchmarkStoreAddWithTags measures what the §9C.1 tag index costs on ingest.
// Reported separately because tagging is optional, and its price should be
// attributable to the feature rather than hidden in the baseline.
func BenchmarkStoreAddWithTags(b *testing.B) {
	s := NewMemStore(Config{MaxTraces: 5000}.withDefaults())
	now := time.Now()
	sp := traceSpan("a1", "a", "", "gateway", 200)
	sp.Tags = map[string]string{"order_id": "12345", "user_id": "u-99"}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sp.TraceID = fmt.Sprintf("%032x", i+1)
		s.Add(sp, now)
	}
}

// --- §12.7: ingest cost must not scale with stored data --------------------

// BenchmarkStoreAddAtCapacity runs against a store already at MaxTraces, so
// every insert also evicts. If eviction were anything worse than O(1) amortized —
// a scan for the oldest entry, say — this would diverge from
// BenchmarkStoreAddNewTraces, and a full aggregator would ingest more slowly than
// an empty one.
func BenchmarkStoreAddAtCapacity(b *testing.B) {
	const cap = 2000
	s := NewMemStore(Config{MaxTraces: cap}.withDefaults())
	now := time.Now()
	sp := traceSpan("a1", "a", "", "gateway", 200)

	for i := 0; i < cap; i++ {
		sp.TraceID = fmt.Sprintf("%032x", i+1)
		s.Add(sp, now)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sp.TraceID = fmt.Sprintf("%032x", cap+i+1)
		s.Add(sp, now)
	}
}

// BenchmarkStoreAddToLongTrace is the same argument at trace scope: a span
// joining a trace that already holds MaxSpansPerTrace spans must cost the same as
// one joining an empty trace. This is the retry-storm case, where the trace that
// is hardest to ingest is also the one someone is trying to read.
func BenchmarkStoreAddToLongTrace(b *testing.B) {
	s := NewMemStore(Config{MaxSpansPerTrace: 512}.withDefaults())
	now := time.Now()
	sp := traceSpan("a1", "a", "", "gateway", 200)

	for i := 0; i < 512; i++ {
		sp.SpanID = fmt.Sprintf("%016x", i+1)
		s.Add(sp, now)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sp.SpanID = fmt.Sprintf("%016x", 512+i+1)
		s.Add(sp, now)
	}
}

// --- Read path -------------------------------------------------------------

// benchTrace builds a store holding one realistic trace: a gateway fanning out
// to depth-3 chains, which is the shape assembly actually has to handle.
func benchTrace(b *testing.B, spans int) (SpanStore, string) {
	b.Helper()
	s := NewMemStore(Config{MaxSpansPerTrace: spans + 1}.withDefaults())
	now := time.Now()
	id := "a1"

	root := traceSpan(id, "1", "", "gateway", 200)
	s.Add(root, now)
	for i := 1; i < spans; i++ {
		sp := traceSpan(id, fmt.Sprintf("%016x", i+1), fmt.Sprintf("%016x", i/3+1), "svc", 200)
		sp.StartNanoUTC = now.Add(time.Duration(i) * time.Millisecond).UnixNano()
		s.Add(sp, now)
	}
	return s, tid(id)
}

// BenchmarkStoreTraceAssembly is the per-request cost of opening one trace in the
// UI. Assembly deliberately lives here rather than on ingest — this is the price
// of that choice, paid once per view instead of once per span.
func BenchmarkStoreTraceAssembly(b *testing.B) {
	for _, n := range []int{10, 100, 512} {
		b.Run(fmt.Sprintf("Spans%d", n), func(b *testing.B) {
			s, id := benchTrace(b, n)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, ok := s.Trace(id); !ok {
					b.Fatal("trace missing")
				}
			}
		})
	}
}

// BenchmarkStoreRecent is the trace-list poll, which the dashboard runs on a
// timer for as long as anyone has the tab open. It walks the eviction queue
// backwards and summarizes, so its cost is bounded by the limit rather than by
// how much is stored — the reason Recent returns summaries and not traces.
func BenchmarkStoreRecent(b *testing.B) {
	s := NewMemStore(Config{MaxTraces: 2000}.withDefaults())
	now := time.Now()
	sp := traceSpan("a1", "a", "", "gateway", 200)
	for i := 0; i < 2000; i++ {
		sp.TraceID = fmt.Sprintf("%032x", i+1)
		s.Add(sp, now)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := s.Recent(TraceQuery{Limit: 100}); len(got) != 100 {
			b.Fatalf("got %d summaries, want 100", len(got))
		}
	}
}

// BenchmarkStoreRecentByTag is the §9C.1 "find everything that touched order 123"
// query. It must be answered from the inverted index rather than by scanning, so
// this should be roughly independent of how many traces are stored — that
// independence is the whole reason the index exists.
func BenchmarkStoreRecentByTag(b *testing.B) {
	s := NewMemStore(Config{MaxTraces: 2000}.withDefaults())
	now := time.Now()
	for i := 0; i < 2000; i++ {
		sp := traceSpan("a1", "a", "", "gateway", 200)
		sp.TraceID = fmt.Sprintf("%032x", i+1)
		sp.Tags = map[string]string{"order_id": fmt.Sprintf("%d", i)}
		s.Add(sp, now)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := s.Recent(TraceQuery{TagKey: "order_id", TagValue: "1500"}); len(got) != 1 {
			b.Fatalf("tag query returned %d rows, want 1", len(got))
		}
	}
}

// BenchmarkStoreAddWhileReading is the aggregator's real steady state: services
// pushing while a dashboard polls. Ingest and read take the same shard locks, so
// this is where a read holding a lock across assembly would show up as an ingest
// stall — the reason Trace copies its spans and assembles outside the lock.
func BenchmarkStoreAddWhileReading(b *testing.B) {
	s := NewMemStore(Config{MaxTraces: 2000}.withDefaults())
	now := time.Now()
	sp := traceSpan("a1", "a", "", "gateway", 200)
	for i := 0; i < 500; i++ {
		sp.TraceID = fmt.Sprintf("%032x", i+1)
		s.Add(sp, now)
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				_ = s.Recent(TraceQuery{Limit: 50})
			}
		}
	}()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sp.TraceID = fmt.Sprintf("%032x", 500+i+1)
		s.Add(sp, now)
	}
	b.StopTimer()
	close(stop)
	<-done
}

// BenchmarkStoreSweep measures the TTL sweep, which runs on a ticker and does
// scan every shard. That scan is why it is on a ticker rather than in Add: it is
// O(traces), and paying it once every TraceTTL/4 is affordable in a way that
// paying a fraction of it per span would not be.
func BenchmarkStoreSweep(b *testing.B) {
	now := time.Now()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		s := NewMemStore(Config{MaxTraces: 2000, TraceTTL: time.Minute}.withDefaults())
		sp := traceSpan("a1", "a", "", "gateway", 200)
		// Half expired, half fresh, so the sweep does real work in both
		// directions rather than taking a uniform early exit.
		for j := 0; j < 1000; j++ {
			sp.TraceID = fmt.Sprintf("%032x", j+1)
			s.Add(sp, now.Add(-2*time.Minute))
		}
		for j := 1000; j < 2000; j++ {
			sp.TraceID = fmt.Sprintf("%032x", j+1)
			s.Add(sp, now)
		}
		b.StartTimer()

		s.Sweep(now)
	}
}

// --- Assembly in isolation -------------------------------------------------

// BenchmarkAssemble measures tree-building alone, without the store, so a
// regression in assembly is attributable rather than blended into the read-path
// numbers above.
func BenchmarkAssemble(b *testing.B) {
	for _, n := range []int{10, 100, 512} {
		b.Run(fmt.Sprintf("Spans%d", n), func(b *testing.B) {
			spans := make([]fleet.Span, 0, n)
			base := time.Now()
			for i := 0; i < n; i++ {
				parent := ""
				if i > 0 {
					parent = fmt.Sprintf("%016x", i/3+1)
				}
				sp := traceSpan("a1", fmt.Sprintf("%016x", i+1), parent, "svc", 200)
				sp.StartNanoUTC = base.Add(time.Duration(i) * time.Millisecond).UnixNano()
				spans = append(spans, sp)
			}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = Assemble(tid("a1"), spans)
			}
		})
	}
}

// BenchmarkAssembleDeepChain is the pathological shape: a 512-hop linear chain,
// which is what a runaway retry loop produces. Assembly walks the tree
// recursively, so this is the case that would blow the stack or go quadratic if
// the traversal were naive — worth measuring precisely because it is the input a
// broken fleet generates rather than a healthy one.
func BenchmarkAssembleDeepChain(b *testing.B) {
	const n = 512
	spans := make([]fleet.Span, 0, n)
	base := time.Now()
	for i := 0; i < n; i++ {
		parent := ""
		if i > 0 {
			parent = fmt.Sprintf("%016x", i)
		}
		sp := traceSpan("a1", fmt.Sprintf("%016x", i+1), parent, "svc", 200)
		sp.StartNanoUTC = base.Add(time.Duration(i) * time.Millisecond).UnixNano()
		spans = append(spans, sp)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Assemble(tid("a1"), spans)
	}
}
