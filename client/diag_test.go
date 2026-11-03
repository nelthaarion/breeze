package client

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nelthaarion/breeze/v2/diag"
)

// ── the report, as a pure function ───────────────────────────────────────────

// counting is a snapshot taken with the gate open, which is what separates a measured
// zero from an unmeasured one.
func counting(hits, misses, errs, bytes uint64) diag.CounterSnapshot {
	return diag.CounterSnapshot{Hits: hits, Misses: misses, Errors: errs, Bytes: bytes, Counting: true}
}

func TestReportIsOffUntilAClientHasBeenUsed(t *testing.T) {
	r := buildReport(outboundFacts{})
	if r.Status != diag.StatusOff {
		t.Errorf("status = %s, want off: nothing has made a request, which is not a fault", r.Status)
	}
}

// TestReportDoesNotPassOffUnmeasuredZeroesAsAHealthyClient is the property every
// counter-backed probe has to hold: with the gate closed the numbers are not a report
// of an idle client, and the report must say so rather than present them.
func TestReportDoesNotPassOffUnmeasuredZeroesAsAHealthyClient(t *testing.T) {
	r := buildReport(outboundFacts{started: true, requests: diag.CounterSnapshot{Counting: false}})

	if r.Status != diag.StatusOK {
		t.Errorf("status = %s, want ok", r.Status)
	}
	if _, present := r.Detail["responses"]; present {
		t.Errorf("an unmeasured report carries a response count that reads as zero: %v", r.Detail)
	}
	if len(r.Notes) != 1 || !strings.Contains(r.Notes[0], "not measured") {
		t.Errorf("notes = %q, want one saying the counts were not measured", r.Notes)
	}
}

func TestReportIsDegradedWhenNothingHasEverSucceeded(t *testing.T) {
	r := buildReport(outboundFacts{
		started:     true,
		requests:    counting(0, 0, 11, 0),
		conns:       counting(0, 0, 11, 0),
		lastFailure: &failure{at: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC), msg: "client: dial 127.0.0.1:9000: connection refused"},
	})

	if r.Status != diag.StatusDegraded {
		t.Fatalf("status = %s, want degraded: 11 requests and no response", r.Status)
	}
	if !strings.Contains(r.Summary, "0 response(s), 11 failure(s)") {
		t.Errorf("summary = %q", r.Summary)
	}
	if got := r.Detail["last_failure"]; got != "client: dial 127.0.0.1:9000: connection refused" {
		t.Errorf("last_failure = %v, want the error that says why", got)
	}
	if r.Detail["last_failure_at"] != "2026-11-01T12:00:00Z" {
		t.Errorf("last_failure_at = %v", r.Detail["last_failure_at"])
	}
	if len(r.Notes) != 1 || !strings.Contains(r.Notes[0], "Every outbound request") {
		t.Errorf("notes = %q", r.Notes)
	}
}

// A flaky dependency has failures and responses both. Cumulative counts cannot say it
// is unhealthy, so the status must not claim it.
func TestReportStaysOKWhenSomeRequestsSucceed(t *testing.T) {
	r := buildReport(outboundFacts{
		started:  true,
		requests: counting(990, 0, 10, 4096),
		conns:    counting(950, 40, 3, 0),
		timeouts: 7,
	})

	if r.Status != diag.StatusOK {
		t.Errorf("status = %s, want ok: 990 of 1000 requests were answered", r.Status)
	}
	for _, want := range []string{"990 response(s)", "10 failure(s)", "7 timeout(s)", "3 dial failure(s)", "950 connection(s) reused", "40 dialed"} {
		if !strings.Contains(r.Summary, want) {
			t.Errorf("summary %q lacks %q", r.Summary, want)
		}
	}
	if rate, _ := r.Detail["failure_rate"].(float64); rate != 0.01 {
		t.Errorf("failure_rate = %v, want 0.01", r.Detail["failure_rate"])
	}
	if len(r.Notes) != 0 {
		t.Errorf("a healthy client carries notes: %q", r.Notes)
	}
}

// A 5xx is the far end saying it is failing, which is a different problem from it being
// unreachable and sends the reader to a different place.
func TestReportTellsAFailingDependencyFromAnUnreachableOne(t *testing.T) {
	r := buildReport(outboundFacts{
		started:      true,
		requests:     counting(40, 0, 0, 0),
		serverErrors: 12,
	})

	if r.Status != diag.StatusOK {
		t.Errorf("status = %s: the client itself is working, it is the far end that is not", r.Status)
	}
	if len(r.Notes) != 1 || !strings.Contains(r.Notes[0], "12 response(s) were 5xx") ||
		!strings.Contains(r.Notes[0], "different problem") {
		t.Errorf("notes = %q", r.Notes)
	}
}

// ── the counters, against real sockets ───────────────────────────────────────

type tally struct{ responses, failures, reused, dialed, dialFailures, timeouts, serverErrors uint64 }

func takeTally() tally {
	return tally{
		responses:    requestsCounter.Snapshot().Hits,
		failures:     requestsCounter.Snapshot().Errors,
		reused:       connsCounter.Snapshot().Hits,
		dialed:       connsCounter.Snapshot().Misses,
		dialFailures: connsCounter.Snapshot().Errors,
		timeouts:     timeoutsCounter.Snapshot().Hits,
		serverErrors: serverErrorsCounter.Snapshot().Hits,
	}
}

func (a tally) since(b tally) tally {
	return tally{a.responses - b.responses, a.failures - b.failures, a.reused - b.reused, a.dialed - b.dialed,
		a.dialFailures - b.dialFailures, a.timeouts - b.timeouts, a.serverErrors - b.serverErrors}
}

func TestCountersFollowRequestsOverRealSockets(t *testing.T) {
	diag.EnableCounters()
	t.Cleanup(diag.DisableCounters)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/boom":
			w.WriteHeader(http.StatusInternalServerError)
		case "/slow":
			time.Sleep(400 * time.Millisecond)
		}
		fmt.Fprint(w, "hello")
	}))
	defer srv.Close()

	c := New(Config{Timeout: 150 * time.Millisecond})
	defer c.Close()

	get := func(path string) (*Response, error) { return c.Get(srv.URL + path) }
	mark := takeTally()

	// A fresh connection, then the same one again from the pool.
	for i := 0; i < 2; i++ {
		if resp, err := get("/ok"); err != nil || resp.Status != 200 {
			t.Fatalf("GET /ok: %v %v", resp, err)
		}
	}
	got := takeTally().since(mark)
	if got.responses != 2 || got.dialed != 1 || got.reused != 1 {
		t.Errorf("two requests to one host: %+v, want 2 responses, 1 connection dialed and 1 reused", got)
	}

	// A 5xx is a response: the far end answered. It is counted as one, and as a failing
	// far end, not as a failure of the client.
	mark = takeTally()
	if resp, err := get("/boom"); err != nil || resp.Status != 500 {
		t.Fatalf("GET /boom: %v %v", resp, err)
	}
	got = takeTally().since(mark)
	if got.responses != 1 || got.serverErrors != 1 || got.failures != 0 {
		t.Errorf("a 500: %+v, want 1 response, 1 server error, 0 failures", got)
	}

	// A response that does not arrive within Timeout.
	mark = takeTally()
	if _, err := get("/slow"); err == nil {
		t.Fatal("GET /slow succeeded; the test server is slower than the client's timeout")
	}
	got = takeTally().since(mark)
	if got.failures != 1 || got.timeouts != 1 || got.responses != 0 {
		t.Errorf("a timeout: %+v, want 1 failure, 1 timeout, no response", got)
	}

	// Nothing listening: the dial fails, and the error that says why is kept.
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := closed.Addr().String()
	_ = closed.Close()

	mark = takeTally()
	if _, err := c.Get("http://" + deadAddr + "/"); err == nil {
		t.Fatal("a request to a closed port succeeded")
	}
	got = takeTally().since(mark)
	if got.failures != 1 || got.dialFailures != 1 {
		t.Errorf("a refused connection: %+v, want 1 failure and 1 dial failure", got)
	}
	if last := lastFailure.Load(); last == nil || !strings.Contains(last.msg, deadAddr) {
		t.Errorf("last failure = %+v, want the error naming %s", last, deadAddr)
	}

	// And the probe built from all of it reads as the diagnosis it is for.
	if r := probe(); r.Subsystem != "" && r.Status == diag.StatusOff {
		t.Errorf("the probe reports off after requests were made: %+v", r)
	}
}

// TestRejectedRequestsAreNotCountedAndNeverStoredAsAFailure: a URL that fails to parse is
// quoted by its error, and a URL can carry credentials. A diagnostics endpoint must never
// serve them, and a request that was never sent is a caller's bug, not an outbound failure.
func TestRejectedRequestsAreNotCountedAndNeverStoredAsAFailure(t *testing.T) {
	diag.EnableCounters()
	t.Cleanup(diag.DisableCounters)

	lastFailure.Store(nil)
	c := New()
	defer c.Close()

	mark := takeTally()
	for _, raw := range []string{
		"http://user:hunter2@[::1",    // does not parse; the error quotes the URL
		"ftp://user:hunter2@host/",    // not http
		"http://user:hunter2@:80/x#f", // no host
	} {
		if _, err := c.Do(NewRequest("GET", raw, nil)); err == nil {
			t.Fatalf("%q was accepted", raw)
		}
	}
	if _, err := c.Do(nil); err == nil {
		t.Fatal("a nil request was accepted")
	}

	if got := takeTally().since(mark); got.failures != 0 || got.responses != 0 {
		t.Errorf("requests that never reached the network were counted: %+v", got)
	}
	if last := lastFailure.Load(); last != nil {
		t.Errorf("a rejected request left a stored failure, which can carry its URL: %q", last.msg)
	}
}

// TestCountersCostNothingWhileCountingIsOff: the default. The gate is the whole cost,
// and a test is the only thing that will notice if a counter is ever added without it.
func TestCountersCostNothingWhileCountingIsOff(t *testing.T) {
	diag.DisableCounters()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") }))
	defer srv.Close()

	c := New()
	defer c.Close()

	mark := takeTally()
	lastFailure.Store(nil)
	if _, err := c.Get(srv.URL); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get("http://127.0.0.1:1/"); err == nil {
		t.Fatal("a request to port 1 succeeded")
	}

	if got := takeTally().since(mark); got != (tally{}) {
		t.Errorf("counters moved while counting was off: %+v", got)
	}
	if lastFailure.Load() != nil {
		t.Error("a failure was stored while counting was off")
	}
	if r := probe(); r.Status != diag.StatusOK || len(r.Notes) != 1 || !strings.Contains(r.Notes[0], "not measured") {
		t.Errorf("the probe does not say it is not measuring: %+v", r)
	}
}
