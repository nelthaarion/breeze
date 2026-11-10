package rpc

// diag_test.go — the JSON-RPC server's probe.
//
// The probe exists for one failure: a listener that works perfectly and answers
// -32601 to everything, because the methods were registered on a different
// Registry than the one the server was built with. Nothing else in the framework
// can see that — a JSON-RPC server has no routes, and its calls do not reach the
// dashboard — so the empty-method-table case is asserted first and hardest.

import (
	"net"
	"strings"
	"testing"

	"github.com/nelthaarion/breeze/v2/diag"
)

func TestProbeReportsAnEmptyMethodTableAsDegraded(t *testing.T) {
	s := NewServer(NewRegistry())

	report := s.probe()
	if report.Status != diag.StatusDegraded {
		t.Fatalf("status = %q for a server with no methods, want %q: %s",
			report.Status, diag.StatusDegraded, report.Summary)
	}
	if !strings.Contains(report.Summary, "-32601") {
		t.Errorf("the summary does not name the error clients will see: %s", report.Summary)
	}
	// The note has to name the actual cause, because "no methods registered" alone
	// does not tell someone who believes they registered them where to look.
	joined := strings.Join(report.Notes, " ")
	if !strings.Contains(joined, "different Registry") {
		t.Errorf("no note explains the wrong-registry cause: %q", report.Notes)
	}
}

func TestProbeListsTheRegisteredMethodsSorted(t *testing.T) {
	reg := NewRegistry()
	reg.Register("zeta.method", func(*Context) {})
	reg.Register("alpha.method", func(*Context) {})
	reg.RegisterBlocking("beta.blocking", func(*Context) {})

	s := NewServer(reg)
	report := s.probe()

	if report.Status != diag.StatusOK {
		t.Fatalf("status = %q, want %q: %s", report.Status, diag.StatusOK, report.Summary)
	}

	methods, ok := report.Detail["methods"].([]string)
	if !ok {
		t.Fatalf("methods is %T, want []string", report.Detail["methods"])
	}
	want := []string{"alpha.method", "beta.blocking", "zeta.method"}
	if len(methods) != len(want) {
		t.Fatalf("methods = %v, want %v", methods, want)
	}
	for i := range want {
		if methods[i] != want[i] {
			t.Errorf("methods = %v, want them sorted as %v", methods, want)
			break
		}
	}

	if report.Detail["blocking_methods"] != 1 {
		t.Errorf("blocking_methods = %v, want 1", report.Detail["blocking_methods"])
	}
	if report.Detail["inline_methods"] != 2 {
		t.Errorf("inline_methods = %v, want 2", report.Detail["inline_methods"])
	}
}

// TestProbeWarnsAboutBlockingMethodsWithNoPool is the second silent
// misconfiguration: blocking methods still run, but on an unbounded number of
// fresh goroutines.
func TestProbeWarnsAboutBlockingMethodsWithNoPool(t *testing.T) {
	reg := NewRegistry()
	reg.RegisterBlocking("slow.query", func(*Context) {})
	s := NewServer(reg)

	if got := strings.Join(s.probe().Notes, " "); !strings.Contains(got, "no worker pool") {
		t.Errorf("no note about the missing pool: %q", s.probe().Notes)
	}

	s.SetPool(stubPool{})
	if got := strings.Join(s.probe().Notes, " "); strings.Contains(got, "no worker pool") {
		t.Errorf("the missing-pool note survived SetPool: %q", s.probe().Notes)
	}
	if s.probe().Detail["worker_pool"] != true {
		t.Error("worker_pool is false after SetPool")
	}
}

// TestUnknownMethodCallsAreCountedUngated is the one number here that must be
// exact regardless of whether counted diagnostics were enabled: it is what explains
// a client seeing nothing but -32601.
func TestUnknownMethodCallsAreCountedUngated(t *testing.T) {
	diag.DisableCounters()
	t.Cleanup(diag.DisableCounters)

	reg := NewRegistry()
	reg.Register("known", func(ctx *Context) { ctx.Result("ok") })
	s := NewServer(reg)

	before, _ := s.probe().Detail["unknown_methods"].(uint64)

	s.Handle([]byte(`{"jsonrpc":"2.0","id":1,"method":"does.not.exist"}`))
	// A notification for an unregistered method gets no response at all, which is
	// the harder case to diagnose and so is counted too.
	s.Handle([]byte(`{"jsonrpc":"2.0","method":"also.missing"}`))

	report := s.probe()
	got, _ := report.Detail["unknown_methods"].(uint64)
	if got != before+2 {
		t.Errorf("unknown_methods = %d, want %d — the count must not depend on the gate",
			got, before+2)
	}
	if last, _ := report.Detail["last_unknown_method"].(string); last != "also.missing" {
		t.Errorf("last_unknown_method = %q, want also.missing", last)
	}
	if report.Detail["counting"] != false {
		t.Errorf("counting = %v; this test runs with the gate closed", report.Detail["counting"])
	}
}

// TestProbeIsDegradedWhenMostCallsNameAnUnknownMethod covers the client/server
// disagreement, which no amount of correct wiring on one side fixes.
func TestProbeIsDegradedWhenMostCallsNameAnUnknownMethod(t *testing.T) {
	diag.EnableCounters()
	t.Cleanup(diag.DisableCounters)

	reg := NewRegistry()
	reg.Register("known", func(ctx *Context) { ctx.Result("ok") })
	s := NewServer(reg)

	s.Handle([]byte(`{"jsonrpc":"2.0","id":1,"method":"known"}`))
	for i := 0; i < 5; i++ {
		s.Handle([]byte(`{"jsonrpc":"2.0","id":1,"method":"typo"}`))
	}

	report := s.probe()
	if report.Status != diag.StatusDegraded {
		t.Fatalf("status = %q with most calls unknown, want %q: %s",
			report.Status, diag.StatusDegraded, report.Summary)
	}
	if got, _ := report.Detail["calls_dispatched"].(uint64); got == 0 {
		t.Error("calls_dispatched is zero with counting on after a successful call")
	}
}

func TestProbeIsRegisteredByNewServer(t *testing.T) {
	NewServer(NewRegistry())
	if _, found := diag.Get("jsonrpc"); !found {
		t.Errorf("no \"jsonrpc\" probe after NewServer; registered: %v", diag.Registered())
	}
}

// stubPool satisfies Pool without starting anything.
type stubPool struct{}

func (stubPool) Submit(f func()) { f() }

// TestProbeReportsAListenerThatNeverStarted is the other failure this probe exists for. A
// JSON-RPC server owns its own listener, and the usual way to start it is on a goroutine
// whose only report is one log line: gnet.Run blocks while the listener lives, so it returns
// only when the listener is gone. The commonest cause is the port being taken, and the
// process that results looks healthy, serves every HTTP route, and answers nothing on the
// JSON-RPC port. The probe said "ok" for it, because listen_port is the port that was asked
// for and not proof that anything is bound to it.
func TestProbeReportsAListenerThatNeverStarted(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	port := taken.Addr().(*net.TCPAddr).Port

	reg := NewRegistry()
	reg.Register("sum", func(*Context) {})
	s := NewServer(reg)

	// Wildcard, like the generated block: the port is taken on loopback, so the wildcard
	// bind to the same port fails.
	if err := s.Run(port, false); err == nil {
		t.Fatal("Run returned nil on a port that is already in use; the test cannot show the failure")
	}

	report := s.probe()
	if report.Status != diag.StatusDegraded {
		t.Fatalf("status = %q for a listener that is not running, want %q: %s", report.Status, diag.StatusDegraded, report.Summary)
	}
	if !strings.Contains(report.Summary, "not running") {
		t.Errorf("the summary does not say the listener is not running: %s", report.Summary)
	}
	if listening, _ := report.Detail["listening"].(bool); listening {
		t.Errorf("detail.listening = true for a listener that failed to bind: %v", report.Detail)
	}
	// The error gnet returned, whatever it was. Which one depends on the host: a port that
	// is taken gives "address already in use", and a host without IPv6 fails the wildcard
	// bind before it gets that far. The probe's job is to carry it, not to know it.
	reason, _ := report.Detail["stop_reason"].(string)
	if reason == "" {
		t.Errorf("detail.stop_reason is empty: the reader is told the listener is down and not why: %v", report.Detail)
	}
	if !strings.Contains(report.Summary, reason) {
		t.Errorf("the summary %q does not carry the reason %q", report.Summary, reason)
	}
}

// A server that was never asked to listen has no listener to be missing: it may be served
// over stdio or embedded, and reporting it degraded would be a false alarm on every one of
// them.
func TestProbeDoesNotFaultAServerThatWasNeverRun(t *testing.T) {
	reg := NewRegistry()
	reg.Register("sum", func(*Context) {})

	// Not asserted on the overall status: the unknown-method counters are package-level and
	// earlier tests leave them non-zero, which can legitimately make this report degraded
	// for a reason that has nothing to do with a listener. What is under test is that no
	// verdict about a listener is given.
	report := NewServer(reg).probe()
	if strings.Contains(report.Summary, "listener") {
		t.Errorf("a server that never ran is reported as having a listener problem: %s", report.Summary)
	}
	for _, key := range []string{"listening", "stop_reason", "stopped_at"} {
		if _, present := report.Detail[key]; present {
			t.Errorf("a server that never ran reports %s: %v", key, report.Detail)
		}
	}
}

// TestAServerBuiltWithoutDiagnosticsDoesNotReplaceTheApplicationsProbe: the registry keeps one
// "jsonrpc" entry and the last server constructed takes it. The MCP endpoint builds an
// rpc.Server to speak its protocol, and each one it built replaced the application's entry with
// a report of five MCP methods and no listener, so an application with both could not see its
// own JSON-RPC server.
func TestAServerBuiltWithoutDiagnosticsDoesNotReplaceTheApplicationsProbe(t *testing.T) {
	app := NewRegistry()
	app.Register("orders.create", func(*Context) {})
	NewServer(app)

	adapter := NewRegistry()
	adapter.Register("tools/list", func(*Context) {})
	NewServer(adapter, WithoutDiagnostics())

	report, ok := diag.Get(diagName)
	if !ok {
		t.Fatal("no jsonrpc diagnostic is registered")
	}
	methods, _ := report.Detail["methods"].([]string)
	got := strings.Join(methods, ",")
	if !strings.Contains(got, "orders.create") || strings.Contains(got, "tools/list") {
		t.Errorf("the diagnostic describes %q: the adapter's server replaced the application's", got)
	}

	// And the default is unchanged: a second ordinary server still takes the entry.
	other := NewRegistry()
	other.Register("billing.charge", func(*Context) {})
	NewServer(other)
	report, _ = diag.Get(diagName)
	methods, _ = report.Detail["methods"].([]string)
	if !strings.Contains(strings.Join(methods, ","), "billing.charge") {
		t.Errorf("an ordinary server no longer takes the entry: %v", methods)
	}
}
