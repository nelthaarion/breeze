package wstransport

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/nelthaarion/breeze/v2/fleet"
)

// Benchmarks for the export hot path: marshal the envelope, mask the payload,
// one write.
//
// Connection setup is warmed up before the timer starts, because it happens once
// per process rather than once per batch — folding it in would report a cost no
// production workload actually pays per export.
//
// The stub does not acknowledge publishes, which is faithful to the protocol:
// publishing is fire-and-forget, so the client never waits for the aggregator.
// What these measure is therefore the client's send path, which is the only part
// this package controls.

func benchBatch(n int) []fleet.Span {
	spans := make([]fleet.Span, n)
	for i := range spans {
		spans[i] = fleet.Span{
			TraceID:      "0af7651916cd43dd8448eb211c80319c",
			SpanID:       "b7ad6b716920333" + strconv.Itoa(i%10),
			Service:      "gateway",
			Route:        "/checkout",
			Method:       "POST",
			Status:       200,
			StartNanoUTC: time.Now().UnixNano(),
			DurationMs:   12.5,
		}
	}
	return spans
}

// benchTransport returns a transport already connected and authenticated
// against s, so the measured loop only pays for publishing.
func benchTransport(b *testing.B, s *stubServer, batch []fleet.Span) *Transport {
	b.Helper()
	tr := New(Config{
		AggregatorWSURL: s.url(),
		IngestToken:     "bench-token",
		ServiceName:     "gateway",
		Timeout:         5 * time.Second,
	})
	b.Cleanup(func() { _ = tr.Close() })

	if err := tr.ExportSpans(context.Background(), "", batch); err != nil {
		b.Fatalf("warmup export: %v", err)
	}
	return tr
}

func benchmarkExport(b *testing.B, n int) {
	s := newBenchStubServer(b)
	batch := benchBatch(n)
	tr := benchTransport(b, s, batch)
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := tr.ExportSpans(ctx, "", batch); err != nil {
			b.Fatalf("export: %v", err)
		}
	}
}

func BenchmarkExportSpansSingle(b *testing.B)   { benchmarkExport(b, 1) }
func BenchmarkExportSpansBatch10(b *testing.B)  { benchmarkExport(b, 10) }
func BenchmarkExportSpansBatch100(b *testing.B) { benchmarkExport(b, 100) }

// BenchmarkWriteFrame isolates framing and masking from JSON and the socket, so
// a regression can be attributed to the right layer. It writes to a discarding
// connection rather than a real socket.
func BenchmarkWriteFrame(b *testing.B) {
	for _, size := range []int{64, 1024, 16384} {
		b.Run(strconv.Itoa(size)+"B", func(b *testing.B) {
			c := &wsConn{nc: discardConn{}}
			payload := make([]byte, size)

			b.ReportAllocs()
			b.SetBytes(int64(size))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := c.writeFrame(opText, payload); err != nil {
					b.Fatalf("writeFrame: %v", err)
				}
			}
		})
	}
}
