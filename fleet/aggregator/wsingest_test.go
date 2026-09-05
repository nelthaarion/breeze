package aggregator_test

import (
	"context"
	"net"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/nelthaarion/breeze/v2"
	"github.com/nelthaarion/breeze/v2/fleet"
	"github.com/nelthaarion/breeze/v2/fleet/aggregator"
	"github.com/nelthaarion/breeze/v2/fleet/transport/wstransport"
)

// End-to-end coverage for the WebSocket export transport against the real
// aggregator hub. The wstransport package's own tests drive a stub server, which
// cannot catch a protocol misreading: the stub and the client were written from
// the same reading, so both would be wrong together and still pass. Only the
// real hub settles it.
//
// This lives in the aggregator package because it is the one place both halves
// are importable without an import cycle.

// Breeze exposes Run but no Stop or Shutdown, so a booted app owns its port for
// the rest of the process. Both tests therefore share one aggregator started
// once, rather than leaking an event loop per test.
var (
	sharedOnce sync.Once
	sharedPort int
	sharedAgg  *aggregator.Aggregator
)

const sharedIngestToken = "test-ingest-token"

func sharedAggregator(t *testing.T) (int, *aggregator.Aggregator) {
	t.Helper()
	sharedOnce.Do(func() {
		port := freePort(t)

		router := breeze.NewRouter()
		app := breeze.New(router, breeze.NewEventLoopWorkerPool(runtime.NumCPU()))

		agg := aggregator.InstallAggregator(app, router, aggregator.Config{
			BasePath:    "/fleet",
			IngestToken: sharedIngestToken,
		})

		go func() {
			// Blocks for the process lifetime. A bind failure surfaces as the
			// waitForListener timeout below rather than a silent hang.
			_ = app.Run(port, false)
		}()

		waitForListener(t, port, 10*time.Second)
		sharedPort, sharedAgg = port, agg
	})
	if sharedAgg == nil {
		t.Fatal("aggregator failed to start")
	}
	return sharedPort, sharedAgg
}

// freePort reserves a port and immediately releases it. There is a narrow race
// between release and rebind, but a hardcoded port collides far more often on a
// developer machine and in CI.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// waitForListener blocks until the event loop is accepting, so the test does
// not race gnet's startup.
func waitForListener(t *testing.T, port int, timeout time.Duration) {
	t.Helper()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("aggregator never started listening on %d", port)
}

func wsURL(port int) string {
	return "ws://127.0.0.1:" + strconv.Itoa(port) + "/fleet/ws"
}

// TestWSTransportIngestsIntoRealAggregator exports a span over WebSocket and
// reads it back out of the aggregator's store.
func TestWSTransportIngestsIntoRealAggregator(t *testing.T) {
	port, agg := sharedAggregator(t)

	tr := wstransport.New(wstransport.Config{
		AggregatorWSURL: wsURL(port),
		IngestToken:     sharedIngestToken,
		ServiceName:     "gateway",
		Timeout:         5 * time.Second,
	})
	defer func() { _ = tr.Close() }()

	span := fleet.Span{
		TraceID:      "0af7651916cd43dd8448eb211c80319c",
		SpanID:       "b7ad6b7169203331",
		Service:      "gateway",
		Route:        "/checkout",
		Method:       "POST",
		Status:       200,
		StartNanoUTC: time.Now().UnixNano(),
		DurationMs:   12.5,
	}

	if err := tr.ExportSpans(context.Background(), "", []fleet.Span{span}); err != nil {
		t.Fatalf("ExportSpans over websocket: %v", err)
	}

	// Publishing is fire-and-forget: the frame is written and not acknowledged,
	// so poll for arrival rather than assuming it has landed.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if trace, ok := agg.Store().Trace(span.TraceID); ok && len(trace.Roots) > 0 {
			got := trace.Roots[0].Span
			if got.Service != "gateway" || got.Route != "/checkout" {
				t.Fatalf("span arrived corrupted: %+v", got)
			}
			if got.Status != 200 {
				t.Errorf("status = %d, want 200", got.Status)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("span never reached the aggregator over the websocket transport")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestWSTransportBatchIngests covers a multi-span batch, which is the shape the
// Tracer actually flushes.
func TestWSTransportBatchIngests(t *testing.T) {
	port, agg := sharedAggregator(t)

	tr := wstransport.New(wstransport.Config{
		AggregatorWSURL: wsURL(port),
		IngestToken:     sharedIngestToken,
		ServiceName:     "orders",
		Timeout:         5 * time.Second,
	})
	defer func() { _ = tr.Close() }()

	const traceID = "2bf7651916cd43dd8448eb211c80319c"
	spans := []fleet.Span{
		{
			TraceID: traceID, SpanID: "aaaa6b7169203331",
			Service: "orders", Route: "/orders", Method: "POST", Status: 201,
			StartNanoUTC: time.Now().UnixNano(), DurationMs: 8,
		},
		{
			TraceID: traceID, SpanID: "bbbb6b7169203331", ParentSpanID: "aaaa6b7169203331",
			Service: "payments", Route: "/charge", Method: "POST", Status: 200,
			StartNanoUTC: time.Now().UnixNano(), DurationMs: 4,
		},
	}

	if err := tr.ExportSpans(context.Background(), "", spans); err != nil {
		t.Fatalf("ExportSpans: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		// The child is parented to the root, so the batch arrives as one root
		// with SpanCount 2 rather than as two roots.
		if trace, ok := agg.Store().Trace(traceID); ok && trace.SpanCount >= 2 {
			return
		}
		if time.Now().After(deadline) {
			trace, _ := agg.Store().Trace(traceID)
			t.Fatalf("batch did not fully arrive: got %d spans, want 2", trace.SpanCount)
		}

		time.Sleep(10 * time.Millisecond)
	}
}

// TestWSTransportWrongTokenIsRejected proves the hub enforces its ingest token
// against this client: a wrong token must fail loudly and store nothing.
func TestWSTransportWrongTokenIsRejected(t *testing.T) {
	port, agg := sharedAggregator(t)

	tr := wstransport.New(wstransport.Config{
		AggregatorWSURL: wsURL(port),
		IngestToken:     "wrong-token",
		ServiceName:     "gateway",
		Timeout:         5 * time.Second,
	})
	defer func() { _ = tr.Close() }()

	span := fleet.Span{
		TraceID:      "1af7651916cd43dd8448eb211c80319c",
		SpanID:       "c7ad6b7169203331",
		Service:      "gateway",
		Route:        "/checkout",
		Method:       "POST",
		Status:       200,
		StartNanoUTC: time.Now().UnixNano(),
	}

	if err := tr.ExportSpans(context.Background(), "", []fleet.Span{span}); err == nil {
		t.Fatal("export with a wrong ingest token succeeded")
	}

	// Give any erroneously-accepted publish time to land before asserting it
	// did not: checking immediately could pass simply by being early.
	time.Sleep(200 * time.Millisecond)
	if _, ok := agg.Store().Trace(span.TraceID); ok {
		t.Fatal("a span was stored despite failed authentication")
	}
}
