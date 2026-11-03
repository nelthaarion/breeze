package client

// diag.go — the HTTP client's diagnostic probe.
//
// The client is what a Breeze service uses to reach the rest of its fleet: Fleet's
// exporter posts spans through it, and a gateway calls its upstreams through it. When a
// downstream is down, this is the one subsystem that has seen it first-hand, and until
// there was a probe the only trace of that was a log line the caller happened to write.
// An agent asking "why is this service not reaching its dependencies" had no answer
// from the client itself.
//
// What is reported is deliberately narrow and aggregated across every Client in the
// process. A per-client breakdown would need a registry of clients, which is a strong
// reference to each one for the life of the process, and a service that builds a client
// per request would leak. The questions a diagnosis needs answered are process-wide:
// are requests getting responses, are connections being reused, is the far end failing
// or unreachable.
//
// Counting follows the rest of the framework (see package diag). The counters are
// diag.Counters, so with counting off — the default — each costs one atomic load and a
// predicted branch, and the report says the numbers were not measured instead of
// presenting zeroes that read as an idle client.

import (
	"fmt"
	"sync/atomic"
	"time"

	"github.com/nelthaarion/breeze/v2/diag"
)

// diagClient is the registry key.
const diagClient = "client"

// What each counter's generic fields mean here. diag.Counter documents that every
// subsystem gives Hits/Misses/Errors its own reading, and the probe labels them.
var (
	// requestsCounter: Hits are requests that received a response, whatever its status;
	// Errors are requests that ended without one (could not connect, the write failed,
	// the connection broke, the deadline passed, the caller cancelled); Bytes is
	// response body bytes.
	//
	// Only requests that reached the network are counted. One rejected before it was
	// sent (a nil request, a URL with no host) is a bug in the caller, and counting it
	// would make "every request failed" mean "the URLs are malformed", which is a
	// different finding with a different fix.
	requestsCounter diag.Counter

	// connsCounter: Hits are pooled connections reused, Misses are connections dialed
	// fresh, Errors are dials that failed. A low reuse rate on a busy service means the
	// pool is too small or peers are closing idle connections.
	connsCounter diag.Counter

	// timeoutsCounter: Hits are requests that waited the client's whole Timeout for a
	// response and got none. A subset of requestsCounter's Errors, kept separate because
	// "connected and the far end did not answer" and "could not connect" are different
	// diagnoses.
	timeoutsCounter diag.Counter

	// serverErrorsCounter: Hits are responses with a 5xx status. These count as
	// requestsCounter Hits too: the far end answered, and what it said was that it is
	// failing, which is the difference between a dependency that is down and one that
	// is broken.
	serverErrorsCounter diag.Counter
)

// anyStarted records that some client has started its event loop, which is the first
// request. It is what separates "no request was ever made" (off) from "requests were
// made and nobody was counting" (on, unmeasured).
var anyStarted atomic.Bool

// failure is the most recent request that ended without a response.
type failure struct {
	at  time.Time
	msg string
}

// lastFailure is the most recent failure, kept because the counts say that requests
// are failing and not why. It is only recorded while counting is on, like everything
// else here, and only for requests that reached the network: those errors carry an
// address and an operating system error, whereas a rejected URL's error quotes the URL,
// which can carry credentials a diagnostics endpoint must never serve.
var lastFailure atomic.Pointer[failure]

// maxFailureLen bounds the stored message. An error can wrap arbitrary text, and a
// diagnostics endpoint should not become a way to move it.
const maxFailureLen = 300

func noteFailure(err error) {
	if !diag.CountersEnabled() {
		return
	}
	msg := err.Error()
	if len(msg) > maxFailureLen {
		msg = msg[:maxFailureLen] + "..."
	}
	lastFailure.Store(&failure{at: time.Now(), msg: msg})
}

// countOutcome records how a request that reached the network ended.
func countOutcome(resp *Response, err error) {
	if err != nil {
		requestsCounter.Error()
		noteFailure(err)
		return
	}

	requestsCounter.Hit()
	if resp == nil {
		return
	}
	requestsCounter.Bytes(int64(len(resp.Body)))
	if resp.Status >= 500 {
		serverErrorsCounter.Hit()
	}
}

func init() {
	// Registered from init so the client is diagnosable whether or not anything has used
	// it. "No request has been made" is the answer to "is this service even trying to
	// reach its dependencies", and it is only available if something registered a probe
	// before the question was asked.
	diag.Register(diagClient, probe)
}

// outboundFacts is everything the report is built from, as plain values. Separating it
// from the globals keeps buildReport a pure function: every status and note below is
// tested without any process-wide state or a network.
type outboundFacts struct {
	started      bool
	requests     diag.CounterSnapshot
	conns        diag.CounterSnapshot
	timeouts     uint64
	serverErrors uint64
	lastFailure  *failure
}

func probe() diag.Report {
	return buildReport(outboundFacts{
		started:      anyStarted.Load(),
		requests:     requestsCounter.Snapshot(),
		conns:        connsCounter.Snapshot(),
		timeouts:     timeoutsCounter.Snapshot().Hits,
		serverErrors: serverErrorsCounter.Snapshot().Hits,
		lastFailure:  lastFailure.Load(),
	})
}

// notCountingNote mirrors the note every counter-backed probe adds when the gate is
// closed. It exists because the alternative is indistinguishable from a client that is
// in use and healthy, and those want opposite next actions.
const notCountingNote = "Counted diagnostics are off for this process, so request counts were not " +
	"measured: nothing here says the client is idle or healthy. Installing the dashboard or an " +
	"in-process MCP endpoint enables counting, as does calling diag.EnableCounters()."

func buildReport(f outboundFacts) diag.Report {
	if !f.started {
		return diag.Off("no request has been made through the framework's HTTP client in this process")
	}

	if !f.requests.Counting {
		return diag.OK("the HTTP client is in use; request counts are not measured", map[string]any{
			"counting": false,
		}).WithNotes(notCountingNote)
	}

	r := f.requests
	detail := map[string]any{
		"responses":          r.Hits,
		"failures":           r.Errors,
		"response_bytes":     r.Bytes,
		"connections_reused": f.conns.Hits,
		"connections_dialed": f.conns.Misses,
		"dial_failures":      f.conns.Errors,
		"timeouts":           f.timeouts,
		"server_errors":      f.serverErrors,
		"counting":           true,
	}
	if total := r.Hits + r.Errors; total > 0 {
		detail["failure_rate"] = float64(r.Errors) / float64(total)
	}
	if r.Last != "" {
		detail["last"] = r.Last
	}
	if f.lastFailure != nil {
		detail["last_failure"] = f.lastFailure.msg
		detail["last_failure_at"] = f.lastFailure.at.UTC().Format(time.RFC3339)
	}

	summary := fmt.Sprintf("%d response(s), %d failure(s) (%d timeout(s), %d dial failure(s)); "+
		"%d connection(s) reused, %d dialed",
		r.Hits, r.Errors, f.timeouts, f.conns.Errors, f.conns.Hits, f.conns.Misses)

	report := diag.OK(summary, detail)
	var notes []string

	// The one reading strong enough to be a finding from cumulative counts: nothing has
	// ever succeeded. A service whose dependency is merely flaky has failures and
	// responses both; one that has never had an answer is not reaching it at all.
	if r.Hits == 0 && r.Errors > 0 {
		report = diag.Degraded(summary, detail)
		notes = append(notes, "Every outbound request counted so far has failed. Dial failures mean the target "+
			"could not be reached from this process (wrong address, DNS, a firewall, or the service is down); "+
			"timeouts mean a connection was made and no response came within the client's Timeout. "+
			"detail.last_failure is the most recent error.")
	}

	if f.serverErrors > 0 {
		notes = append(notes, fmt.Sprintf("%d response(s) were 5xx: the far end answered and said it is failing, "+
			"which is a different problem from it being unreachable. Read that service's own logs.", f.serverErrors))
	}

	return report.WithNotes(notes...)
}
