package fleet

// The service side of §9C.2. The aggregator's merge logic is tested in
// fleet/aggregator/logfanout_test.go; what is tested here is the half that has
// to happen inside the traced service, before the aggregator can find anything:
// a log line emitted during a request must carry that request's trace id.
//
// This is worth its own test because the failure is silent. A missing trace id
// does not break logging — the line still shows on the Logs page — it only makes
// the line invisible to the one view that matters during a distributed incident.

import (
	"testing"

	"github.com/nelthaarion/breeze"
	"github.com/nelthaarion/breeze/dashboard"
)

// newTestCollector builds a Collector the way an application does. Install is
// the only exported constructor, and going through it is what makes these tests
// exercise the same wiring a real service gets — including the init-time
// resolver registration this feature depends on.
func newTestCollector(t *testing.T) *dashboard.Collector {
	t.Helper()
	cfg := dashboard.DefaultConfig()
	// No persistence: these tests assert on in-memory ring buffers, and a
	// save loop would write a state file into the repo as a side effect.
	cfg.StorageType = "memory"
	return dashboard.Install(nil, breeze.NewRouter(), cfg)
}


// TestPushLogCtxStampsTraceID is the core of the service-side contract: the
// resolver fleet registers on the dashboard actually resolves, so a log line
// emitted mid-request is retrievable by trace id afterwards.
func TestPushLogCtxStampsTraceID(t *testing.T) {
	coll := newTestCollector(t)
	ctx, st := tracedContext()

	coll.PushLogCtx(ctx, "app", "charging card", "orders")


	logs := coll.Logs("app", 10)
	if len(logs) != 1 {
		t.Fatalf("got %d log lines, want 1", len(logs))
	}
	if got, want := logs[0].TraceID, st.tc.TraceIDHex(); got != want {
		t.Errorf("TraceID = %q, want %q — this line is invisible to the trace's log panel", got, want)
	}
}

// TestPushLogCtxOutsideRequestIsStillLogged covers background work: a scheduler
// tick or a startup line has no request and therefore no trace. It must still be
// recorded, just without a trace id.
func TestPushLogCtxOutsideRequestIsStillLogged(t *testing.T) {
	coll := newTestCollector(t)


	coll.PushLogCtx(nil, "app", "cache warmed", "startup")
	// A request that exists but was never traced — fleet.Middleware absent,
	// which is the normal state of an app that has not enabled fleet.
	coll.PushLogCtx(&breeze.Context{}, "app", "untraced request", "orders")

	logs := coll.Logs("app", 10)
	if len(logs) != 2 {
		t.Fatalf("got %d log lines, want 2 — a line without a trace must not be dropped", len(logs))
	}
	for _, e := range logs {
		if e.TraceID != "" {
			t.Errorf("line %q carries trace id %q, want empty", e.Message, e.TraceID)
		}
	}
}

// TestPushLogCtxMasks confirms the trace-aware path did not skip redaction.
// Adding a field to a log entry is exactly the kind of change that quietly
// bypasses an existing scrub, and these lines now leave the process on the
// fan-out path, so a leak here travels further than the local Logs page.
func TestPushLogCtxMasks(t *testing.T) {
	coll := newTestCollector(t)
	ctx, _ := tracedContext()

	coll.PushLogCtx(ctx, "app", "authorizing with password=hunter2", "orders")


	logs := coll.Logs("app", 10)
	if len(logs) != 1 {
		t.Fatalf("got %d log lines, want 1", len(logs))
	}
	if got := logs[0].Message; got == "authorizing with password=hunter2" {
		t.Errorf("message stored unmasked: %q", got)
	}
}

// TestTraceIDOf covers the exported accessor directly, since services using
// their own logger depend on it rather than on PushLogCtx.
func TestTraceIDOf(t *testing.T) {
	ctx, st := tracedContext()

	if got, want := TraceIDOf(ctx), st.tc.TraceIDHex(); got != want {
		t.Errorf("TraceIDOf = %q, want %q", got, want)
	}
	if got := TraceIDOf(&breeze.Context{}); got != "" {
		t.Errorf("TraceIDOf on an untraced request = %q, want empty", got)
	}
	if got := TraceIDOf(nil); got != "" {
		t.Errorf("TraceIDOf(nil) = %q, want empty", got)
	}
}
