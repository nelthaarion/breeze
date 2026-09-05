package events

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// benchEvent is the payload used by every benchmark. It is deliberately
// small: the point is to measure the bus, not the cost of copying a
// large struct.
type benchEvent struct {
	ID   uint64
	Name string
}

// sink absorbs listener work so the compiler cannot optimise the handler
// body away.
var sink atomic.Uint64

func noopHandler(_ *Context, e benchEvent) error {
	sink.Add(e.ID)
	return nil
}

// newBenchBus returns a bus with n listeners registered for benchEvent.
func newBenchBus(n int, cfg ...Config) *Bus {
	b := New(cfg...)
	for i := 0; i < n; i++ {
		OnBus(b, benchEvent{}, noopHandler)
	}
	return b
}

// BenchmarkEmit measures synchronous dispatch as the listener count
// grows. The per-listener cost should stay flat: dispatch walks a
// pre-sorted slice and does no allocation per listener.
func BenchmarkEmit(bm *testing.B) {
	for _, n := range []int{1, 10, 100, 1000} {
		bm.Run(fmt.Sprintf("listeners=%d", n), func(bm *testing.B) {
			b := newBenchBus(n)
			ev := benchEvent{ID: 1, Name: "bench"}

			bm.ReportAllocs()
			bm.ResetTimer()
			for i := 0; i < bm.N; i++ {
				_ = EmitBus(b, ev)
			}
		})
	}
}

// BenchmarkEmitNoListeners measures the cost of emitting an event nobody
// listens to — the case a framework pays on every unused hook. It should
// be a map lookup and nothing else.
func BenchmarkEmitNoListeners(bm *testing.B) {
	b := New()
	ev := benchEvent{ID: 1}

	bm.ReportAllocs()
	bm.ResetTimer()
	for i := 0; i < bm.N; i++ {
		_ = EmitBus(b, ev)
	}
}

// BenchmarkEmitParallel measures dispatch under contention. The
// copy-on-write snapshot means emitters take no locks, so this should
// scale with cores.
func BenchmarkEmitParallel(bm *testing.B) {
	for _, n := range []int{1, 10, 100} {
		bm.Run(fmt.Sprintf("listeners=%d", n), func(bm *testing.B) {
			b := newBenchBus(n)
			ev := benchEvent{ID: 1}

			bm.ReportAllocs()
			bm.ResetTimer()
			bm.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					_ = EmitBus(b, ev)
				}
			})
		})
	}
}

// BenchmarkEmitPriority measures dispatch over listeners spread across
// priorities. Sorting happens at registration, so this should match the
// unsorted case.
func BenchmarkEmitPriority(bm *testing.B) {
	for _, n := range []int{10, 100} {
		bm.Run(fmt.Sprintf("listeners=%d", n), func(bm *testing.B) {
			b := New()
			for i := 0; i < n; i++ {
				OnBus(b, benchEvent{}, noopHandler).Priority(i % 5 * 100)
			}
			ev := benchEvent{ID: 1}

			bm.ReportAllocs()
			bm.ResetTimer()
			for i := 0; i < bm.N; i++ {
				EmitBus(b, ev)
			}
		})
	}
}

// BenchmarkEmitFiltered measures the saving a filter buys. Half the
// listeners reject the event before their handler is entered.
func BenchmarkEmitFiltered(bm *testing.B) {
	bm.Run("all-match", func(bm *testing.B) {
		b := New()
		for i := 0; i < 100; i++ {
			OnBus(b, benchEvent{}, noopHandler).
				Where(func(e benchEvent) bool { return e.ID > 0 })
		}
		ev := benchEvent{ID: 1}

		bm.ReportAllocs()
		bm.ResetTimer()
		for i := 0; i < bm.N; i++ {
			EmitBus(b, ev)
		}
	})

	bm.Run("none-match", func(bm *testing.B) {
		b := New()
		for i := 0; i < 100; i++ {
			OnBus(b, benchEvent{}, noopHandler).
				Where(func(e benchEvent) bool { return e.ID > 1000 })
		}
		ev := benchEvent{ID: 1}

		bm.ReportAllocs()
		bm.ResetTimer()
		for i := 0; i < bm.N; i++ {
			EmitBus(b, ev)
		}
	})
}

// BenchmarkEmitWithMiddleware measures the per-dispatch cost of the
// middleware chain, which is built once per dispatch.
func BenchmarkEmitWithMiddleware(bm *testing.B) {
	for _, n := range []int{0, 1, 3, 5} {
		bm.Run(fmt.Sprintf("middleware=%d", n), func(bm *testing.B) {
			b := newBenchBus(10)
			for i := 0; i < n; i++ {
				b.Use(func(_ *Context, next Next) error { return next() })
			}
			ev := benchEvent{ID: 1}

			bm.ReportAllocs()
			bm.ResetTimer()
			for i := 0; i < bm.N; i++ {
				EmitBus(b, ev)
			}
		})
	}
}

// BenchmarkRecorder isolates the recorder's overhead, which is the
// question an operator asks before enabling it in production.
func BenchmarkRecorder(bm *testing.B) {
	bm.Run("disabled", func(bm *testing.B) {
		b := newBenchBus(10)
		ev := benchEvent{ID: 1}

		bm.ReportAllocs()
		bm.ResetTimer()
		for i := 0; i < bm.N; i++ {
			EmitBus(b, ev)
		}
	})

	bm.Run("enabled", func(bm *testing.B) {
		b := newBenchBus(10)
		b.EnableRecorder()
		ev := benchEvent{ID: 1}

		bm.ReportAllocs()
		bm.ResetTimer()
		for i := 0; i < bm.N; i++ {
			EmitBus(b, ev)
		}
	})

	bm.Run("enabled-with-payload", func(bm *testing.B) {
		b := newBenchBus(10)
		b.EnableRecorderWithPayload()
		ev := benchEvent{ID: 1}

		bm.ReportAllocs()
		bm.ResetTimer()
		for i := 0; i < bm.N; i++ {
			EmitBus(b, ev)
		}
	})
}

// BenchmarkMetrics isolates the metrics counters.
func BenchmarkMetrics(bm *testing.B) {
	bm.Run("enabled", func(bm *testing.B) {
		b := newBenchBus(10)
		ev := benchEvent{ID: 1}

		bm.ReportAllocs()
		bm.ResetTimer()
		for i := 0; i < bm.N; i++ {
			EmitBus(b, ev)
		}
	})

	bm.Run("disabled", func(bm *testing.B) {
		b := newBenchBus(10, Config{DisableMetrics: true})
		ev := benchEvent{ID: 1}

		bm.ReportAllocs()
		bm.ResetTimer()
		for i := 0; i < bm.N; i++ {
			EmitBus(b, ev)
		}
	})
}

// BenchmarkEmitAsync measures scheduling throughput, not listener
// throughput: EmitAsync returns once the work is handed off.
func BenchmarkEmitAsync(bm *testing.B) {
	for _, n := range []int{1, 10, 100} {
		bm.Run(fmt.Sprintf("goroutine/listeners=%d", n), func(bm *testing.B) {
			b := newBenchBus(n)
			ev := benchEvent{ID: 1}

			bm.ReportAllocs()
			bm.ResetTimer()
			for i := 0; i < bm.N; i++ {
				EmitAsyncBus(b, ev)
			}
		})

		bm.Run(fmt.Sprintf("pool/listeners=%d", n), func(bm *testing.B) {
			b := newBenchBus(n, Config{Async: AsyncWorkerPool})
			defer b.Close()
			ev := benchEvent{ID: 1}

			bm.ReportAllocs()
			bm.ResetTimer()
			for i := 0; i < bm.N; i++ {
				EmitAsyncBus(b, ev)
			}
		})
	}
}

// BenchmarkEmitAsyncWait measures end-to-end async dispatch, including
// the time listeners take to complete.
func BenchmarkEmitAsyncWait(bm *testing.B) {
	for _, n := range []int{1, 10, 100} {
		bm.Run(fmt.Sprintf("listeners=%d", n), func(bm *testing.B) {
			b := newBenchBus(n)
			ev := benchEvent{ID: 1}

			bm.ReportAllocs()
			bm.ResetTimer()
			for i := 0; i < bm.N; i++ {
				EmitAsyncWaitBus(b, ev)
			}
		})
	}
}

// BenchmarkContextMetadata measures the lazily allocated metadata map.
// A dispatch that never calls Set must not pay for it.
func BenchmarkContextMetadata(bm *testing.B) {
	bm.Run("unused", func(bm *testing.B) {
		b := New()
		OnBus(b, benchEvent{}, noopHandler)
		ev := benchEvent{ID: 1}

		bm.ReportAllocs()
		bm.ResetTimer()
		for i := 0; i < bm.N; i++ {
			EmitBus(b, ev)
		}
	})

	bm.Run("used", func(bm *testing.B) {
		b := New()
		OnBus(b, benchEvent{}, func(ctx *Context, e benchEvent) error {
			ctx.Set("id", e.ID)
			_, _ = ctx.Get("id")
			return nil
		})
		ev := benchEvent{ID: 1}

		bm.ReportAllocs()
		bm.ResetTimer()
		for i := 0; i < bm.N; i++ {
			EmitBus(b, ev)
		}
	})
}

// BenchmarkRegister measures registration, which happens at startup and
// is allowed to be the expensive operation: it sorts and republishes the
// listener snapshot.
func BenchmarkRegister(bm *testing.B) {
	bm.ReportAllocs()
	for i := 0; i < bm.N; i++ {
		b := New()
		for j := 0; j < 10; j++ {
			OnBus(b, benchEvent{}, noopHandler)
		}
	}
}

// BenchmarkRegisterConcurrent measures registration under contention,
// where the copy-on-write snapshot is rebuilt while readers are active.
func BenchmarkRegisterConcurrent(bm *testing.B) {
	b := New()
	var mu sync.Mutex
	subs := make([]*Subscription[benchEvent], 0, 128)

	bm.ReportAllocs()
	bm.ResetTimer()
	bm.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			s := OnBus(b, benchEvent{}, noopHandler)
			mu.Lock()
			subs = append(subs, s)
			if len(subs) == cap(subs) {
				for _, x := range subs {
					x.Unsubscribe()
				}
				subs = subs[:0]
			}
			mu.Unlock()
		}
	})
}

// BenchmarkInspect measures the dashboard's read path, which must never
// interfere with dispatch.
func BenchmarkInspect(bm *testing.B) {
	b := newBenchBus(50)
	EmitBus(b, benchEvent{ID: 1})

	bm.ReportAllocs()
	bm.ResetTimer()
	for i := 0; i < bm.N; i++ {
		_ = Inspect[benchEvent](b)
	}
}
