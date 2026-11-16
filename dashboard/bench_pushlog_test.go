package dashboard

// What feeding the dashboard's http log tab from the generated logging block costs a request.
//
// The generated writer does, per request line: string(p), strings.TrimRight and Collector.PushLog, and
// then writes the line to stdout as it always did. The stdout write is the same with or without the
// feed, so the benchmarks measure the part that is new, against a writer that only discards.

import (
	"io"
	"strings"
	"testing"

	"github.com/nelthaarion/breeze/v2"
)

// requestLine is a typical line LoggingMiddleware writes.
var requestLine = []byte("[Breeze][2026-11-15T15:14:06Z] GET /api/orders/42 -> 200 (24.067µs)\n")

type discardLog struct{}

func (discardLog) Write(p []byte) (int, error) { return io.Discard.Write(p) }

// dashboardLog is the generated writer with stdout replaced by io.Discard.
type dashboardLog struct{ c *Collector }

func (w dashboardLog) Write(p []byte) (int, error) {
	if w.c != nil {
		w.c.PushLog("http", strings.TrimRight(string(p), "\r\n"), "http")
	}
	return io.Discard.Write(p)
}

func BenchmarkRequestLineWriterDiscardOnly(b *testing.B) {
	w := discardLog{}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = w.Write(requestLine)
	}
}

func BenchmarkRequestLineWriterFeedingTheDashboard(b *testing.B) {
	w := dashboardLog{c: newCollector(DefaultConfig(), breeze.NewRouter())}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = w.Write(requestLine)
	}
}

func BenchmarkRequestLineWriterFeedingTheDashboardParallel(b *testing.B) {
	w := dashboardLog{c: newCollector(DefaultConfig(), breeze.NewRouter())}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _ = w.Write(requestLine)
		}
	})
}
