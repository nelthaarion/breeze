package middleware

// diag_recovery_test.go — the recovery probe.
//
// This probe reports the one number in the package that is deliberately not gated:
// how many handler panics were recovered. The reasoning is in RecoveryMiddleware —
// the count must be trustworthy on a process that never enabled counted
// diagnostics, because it is the number that says the application is broken.
//
// The tests below therefore run with counting off, which is the default and the
// harder case: an ungated count that only worked with counters on would pass a test
// written the other way round.

import (
	"strings"
	"testing"

	"github.com/nelthaarion/breeze/v2"
	"github.com/nelthaarion/breeze/v2/diag"
)

func TestRecoveryProbeReportsOffBeforeInstallation(t *testing.T) {
	// recoveryInstalled is process-wide and sticky, so this only holds before any
	// other test in this package has constructed the middleware. Reading the flag
	// directly rather than asserting unconditionally keeps the test honest about
	// the ordering it cannot control.
	if recoveryInstalled.Load() {
		t.Skip("the recovery middleware was already constructed by another test in this package")
	}

	report := recoveryProbe()
	if report.Status != diag.StatusOff {
		t.Fatalf("status = %q, want %q before RecoveryMiddleware is called",
			report.Status, diag.StatusOff)
	}
	// The note has to say what happens without it, because "off" alone reads as a
	// missing convenience rather than as a connection that gets no response.
	if len(report.Notes) == 0 || !strings.Contains(report.Notes[0], "no response") {
		t.Errorf("the off report does not explain the consequence: %q", report.Notes)
	}
}

func TestRecoveryProbeCountsAPanicWithCountingOff(t *testing.T) {
	diag.DisableCounters()
	t.Cleanup(diag.DisableCounters)

	before := recoveredPanics.Load()

	mw := RecoveryMiddleware()
	runRecoveryChain(t, mw, func(*breeze.Context) error {
		panic("deliberate failure in a handler")
	})

	report := recoveryProbe()
	if report.Status != diag.StatusDegraded {
		t.Fatalf("status = %q after a recovered panic, want %q: %s",
			report.Status, diag.StatusDegraded, report.Summary)
	}

	got, ok := report.Detail["panics_recovered"].(uint64)
	if !ok {
		t.Fatalf("panics_recovered is %T, want uint64", report.Detail["panics_recovered"])
	}
	if got != before+1 {
		t.Errorf("panics_recovered = %d, want %d — the count must not depend on the gate",
			got, before+1)
	}
	if report.Detail["counting"] != false {
		t.Errorf("counting = %v; this test runs with the gate closed", report.Detail["counting"])
	}
	if last, _ := report.Detail["last_panic"].(string); !strings.Contains(
		last,
		"deliberate failure",
	) {
		t.Errorf("last_panic = %q, want the panic value", last)
	}
	if report.Detail["last_panic_at"] == nil {
		t.Error("last_panic_at is absent after a recovered panic")
	}
}

// TestRecoveryProbeTruncatesAHugePanicValue keeps the diagnostics endpoint from
// becoming a way to move megabytes: a panic value can be an arbitrary struct.
func TestRecoveryProbeTruncatesAHugePanicValue(t *testing.T) {
	diag.DisableCounters()
	t.Cleanup(diag.DisableCounters)

	mw := RecoveryMiddleware()
	runRecoveryChain(t, mw, func(*breeze.Context) error {
		panic(strings.Repeat("x", 5000))
	})

	last, _ := recoveryProbe().Detail["last_panic"].(string)
	if len(last) > 400 {
		t.Errorf("last_panic is %d bytes; it should be truncated", len(last))
	}
	if !strings.HasSuffix(last, "…") {
		t.Errorf("a truncated value does not say so: %q", last[max(0, len(last)-20):])
	}
}

func TestRecoveryProbeSurvivesAHandlerThatDoesNotPanic(t *testing.T) {
	diag.EnableCounters()
	t.Cleanup(diag.DisableCounters)

	before, _ := recoveryProbe().Detail["panics_recovered"].(uint64)

	mw := RecoveryMiddleware()
	runRecoveryChain(t, mw, func(ctx *breeze.Context) error {
		return ctx.WriteString("fine")
	})

	after, _ := recoveryProbe().Detail["panics_recovered"].(uint64)
	if after != before {
		t.Errorf("panics_recovered moved from %d to %d for a handler that did not panic",
			before, after)
	}
	if got, _ := recoveryProbe().Detail["requests_completed"].(uint64); got == 0 {
		t.Error("requests_completed is zero with counting on after a successful request")
	}
}

// runRecoveryChain drives one request through [recovery, handler].
//
// Uses the same NewContext/SetMiddlewareChain pair the locale tests use, which is
// the package's established way to exercise a middleware without a router.
func runRecoveryChain(t *testing.T, mw, handler breeze.HandlerFunc) {
	t.Helper()

	ctx := breeze.NewContext(breeze.GET, "/")
	ctx.SetMiddlewareChain([]breeze.HandlerFunc{mw}, handler)
	// The error is discarded on purpose: a panicking handler is the case under
	// test, and the middleware converts it into a 500 rather than an error.
	_ = ctx.Next()
}
