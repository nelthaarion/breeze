package workflow

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/nelthaarion/breeze/events"
)

// benchEngine isolates the benchmark from the process-wide bus and
// collector so the numbers measure the engine, not the observers.
func benchEngine() *Engine {
	return NewEngine(Config{Bus: events.New(), DisableObservability: true})
}

func noopStep(*Context) error { return nil }

// benchmarkSteps measures a sequential workflow of n steps, which is
// the shape most applications actually run.
func benchmarkSteps(b *testing.B, n int) {
	e := benchEngine()
	def := New(fmt.Sprintf("bench-%d", n))
	for i := range n {
		def.Step(fmt.Sprintf("s%d", i), noopStep)
	}
	if err := e.Register(def); err != nil {
		b.Fatal(err)
	}

	ctx := context.Background()
	name := def.Name()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := e.Run(ctx, name, nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRun1Step(b *testing.B)   { benchmarkSteps(b, 1) }
func BenchmarkRun10Steps(b *testing.B) { benchmarkSteps(b, 10) }
func BenchmarkRun50Steps(b *testing.B) { benchmarkSteps(b, 50) }

// BenchmarkRunParallel measures a fan-out level, where the cost of
// scheduling competes with the work itself.
func BenchmarkRunParallel(b *testing.B) {
	e := benchEngine()
	def := New("fanout").Step("root", noopStep)
	for i := range 10 {
		def.Step(fmt.Sprintf("p%d", i), noopStep, WithDependsOn("root"))
	}
	if err := e.Register(def); err != nil {
		b.Fatal(err)
	}

	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := e.Run(ctx, "fanout", nil); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRunConcurrent measures throughput when many executions of
// the same workflow overlap, which is the realistic server case.
func BenchmarkRunConcurrent(b *testing.B) {
	e := benchEngine()
	def := New("concurrent").Step("a", noopStep).Step("b", noopStep)
	if err := e.Register(def); err != nil {
		b.Fatal(err)
	}

	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := e.Run(ctx, "concurrent", nil); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkCompensation measures the rollback path: three steps
// succeed, the fourth fails, and every success is undone.
func BenchmarkCompensation(b *testing.B) {
	e := benchEngine()
	undo := func(*Context) error { return nil }
	def := New("saga").
		Step("a", noopStep, WithCompensation(undo)).
		Step("b", noopStep, WithCompensation(undo)).
		Step("c", noopStep, WithCompensation(undo)).
		Step("boom", func(*Context) error { return errors.New("fail") })
	if err := e.Register(def); err != nil {
		b.Fatal(err)
	}

	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, _ = e.Run(ctx, "saga", nil)
	}
}

// BenchmarkValidate measures registration-time cost, which is paid once
// per workflow at startup but grows with the graph.
func BenchmarkValidate(b *testing.B) {
	build := func() *Definition {
		d := New("graph")
		for i := range 50 {
			d.Step(fmt.Sprintf("s%d", i), noopStep)
		}
		return d
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := build().Validate(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkContextMetadata isolates the shared-metadata cost, since
// every step touches it.
func BenchmarkContextMetadata(b *testing.B) {
	c := newContext(context.Background(), "w", "e", "", nil, nil)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		c.Set("key", 1)
		_, _ = c.Get("key")
	}
}
