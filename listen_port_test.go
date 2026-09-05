package breeze

// listen_port_test.go — ListenPort reports the port Run was given.
//
// # Why this is worth a test
//
// The value is consumed by the dashboard's API Explorer to pin its outbound
// request to this service. If ListenPort silently returned 0, the explorer would
// fall back to the port in the caller-supplied Host header — which is the field the
// SSRF fix exists to stop trusting. A wrong answer here is not a cosmetic bug, so
// the zero case is asserted as deliberately as the set case.
//
// Run is not called: it blocks for the process lifetime and binds a socket. The
// store is what is under test, so it is exercised directly.

import "testing"

func TestListenPortIsZeroBeforeRun(t *testing.T) {
	app := New(NewRouter(), NewEventLoopWorkerPool(1))

	// Zero rather than a guess. A subsystem reading this has to be able to tell
	// "not yet listening" from "listening on port 80", and any non-zero default
	// would collapse the two.
	if got := app.ListenPort(); got != 0 {
		t.Errorf("ListenPort() = %d before Run, want 0", got)
	}
}

func TestListenPortReportsWhatRunWasGiven(t *testing.T) {
	app := New(NewRouter(), NewEventLoopWorkerPool(1))

	// What Run does before handing off to gnet. Calling Run itself would block
	// and bind; this is the whole of the recording step.
	app.listenPort.Store(8443)

	if got := app.ListenPort(); got != 8443 {
		t.Errorf("ListenPort() = %d, want 8443", got)
	}
}
