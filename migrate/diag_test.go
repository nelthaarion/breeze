package migrate

// diag_test.go — the migration probe.
//
// This probe is the one place in the framework that reports the outcome of a
// migration after the fact. `breeze migrate` prints to a terminal that is gone; an
// application that migrates at startup logs into a stream nobody is tailing. A
// failed migration at boot leaves a process running and serving against a
// half-applied schema, and before this there was no way to ask a live service
// whether that had happened.
//
// So the tests below are about the recording, not about SQL: that every operation
// records, that a failure is reported as degraded with its error, and that the probe
// never queries — which is the constraint that makes it safe to call when the
// database is the thing that is broken.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/nelthaarion/breeze/v2/diag"
)

// noDialConnector is a *sql.DB that has never been connected and must not be.
//
// sql.OpenDB does not dial, so a Runner built with this has a real handle whose
// Stats are readable and whose connections are not. Connect fails the test rather
// than returning an error, because an error would be swallowed by a probe that
// ignored it and the point is to catch the attempt.
type noDialConnector struct{ t *testing.T }

func (c noDialConnector) Connect(context.Context) (driver.Conn, error) {
	c.t.Error("the probe opened a database connection; it must not do I/O")
	return nil, errors.New("no dialling in a probe")
}

func (noDialConnector) Driver() driver.Driver { return nil }

// configuredRunner returns a Runner with both a handle and a filesystem, so the
// probe gets past its "cannot work at all" branch and reports the last run.
func configuredRunner(t *testing.T) *Runner {
	t.Helper()
	db := sql.OpenDB(noDialConnector{t: t})
	t.Cleanup(func() { _ = db.Close() })
	return New(db, fstest.MapFS{})
}

func TestProbeReportsOffWithNoRunner(t *testing.T) {
	current.Store(nil)
	lastRun.Store(nil)
	t.Cleanup(func() { current.Store(nil); lastRun.Store(nil) })

	report := probe()
	if report.Status != diag.StatusOff {
		t.Fatalf("status = %q with no runner, want %q", report.Status, diag.StatusOff)
	}
	if !strings.Contains(report.Summary, "migrate.New") {
		t.Errorf("the summary does not name the call that would fix it: %s", report.Summary)
	}
}

// TestProbeIsDegradedWithoutADatabaseHandle covers the runner that cannot work.
//
// migrate.New accepts a nil *sql.DB — the exported fields make it constructible
// anyway — so this is reachable, and every operation on it fails.
func TestProbeIsDegradedWithoutADatabaseHandle(t *testing.T) {
	New(nil, fstest.MapFS{})
	t.Cleanup(func() { current.Store(nil); lastRun.Store(nil) })

	report := probe()
	if report.Status != diag.StatusDegraded {
		t.Fatalf("status = %q with a nil database, want %q: %s",
			report.Status, diag.StatusDegraded, report.Summary)
	}
	if report.Detail["database"] != false {
		t.Errorf("database = %v, want false", report.Detail["database"])
	}
}

// TestProbeDoesNotQuery is the constraint that makes this probe callable when the
// database is already the problem.
//
// A nil *sql.DB is the strongest available assertion: any query attempt panics
// rather than merely erroring, so a probe that reached for one would fail loudly
// here rather than passing quietly and hanging in production.
func TestProbeDoesNotQuery(t *testing.T) {
	New(nil, fstest.MapFS{})
	t.Cleanup(func() { current.Store(nil); lastRun.Store(nil) })

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("the probe touched the database handle: %v", r)
		}
	}()
	_ = probe()
}

func TestProbeReportsTheLastRun(t *testing.T) {
	configuredRunner(t)
	t.Cleanup(func() { current.Store(nil); lastRun.Store(nil) })

	record("up", time.Now().Add(-90*time.Second), 3, nil)

	report := probe()
	last, ok := report.Detail["last_run"].(map[string]any)
	if !ok {
		t.Fatalf("last_run is %T, want a map", report.Detail["last_run"])
	}
	if last["operation"] != "up" || last["count"] != 3 || last["ok"] != true {
		t.Errorf("last_run = %v", last)
	}
	// The summary carries a relative time, because "is this the run I just did" is
	// the question being asked and an RFC 3339 string makes the reader do
	// arithmetic. The absolute time stays in Detail.
	if !strings.Contains(report.Summary, "minute(s) ago") {
		t.Errorf("the summary does not say how long ago: %s", report.Summary)
	}
	if last["at"] == nil {
		t.Error("last_run has no absolute timestamp")
	}
}

// TestProbeIsDegradedAfterAFailedMigration is the case the probe exists for.
func TestProbeIsDegradedAfterAFailedMigration(t *testing.T) {
	configuredRunner(t)
	t.Cleanup(func() { current.Store(nil); lastRun.Store(nil) })

	record("up", time.Now(), 1, errors.New("migration 7 failed: syntax error near FROM"))

	report := probe()
	if report.Status != diag.StatusDegraded {
		t.Fatalf(
			"status = %q after a failed migration, want %q",
			report.Status,
			diag.StatusDegraded,
		)
	}
	if !strings.Contains(report.Summary, "syntax error near FROM") {
		t.Errorf("the summary does not carry the error: %s", report.Summary)
	}
	// The consequence has to be stated: the process is still up, serving requests
	// against whatever schema the last committed transaction produced.
	if !strings.Contains(strings.Join(report.Notes, " "), "still running") {
		t.Errorf("no note explains that the process is still serving: %q", report.Notes)
	}
}

// TestDownRecordsEvenWhenItRejectsItsArgument checks the deferred record covers
// every exit path, including the ones that return before touching the database.
func TestDownRecordsEvenWhenItRejectsItsArgument(t *testing.T) {
	r := New(nil, fstest.MapFS{})
	t.Cleanup(func() { current.Store(nil); lastRun.Store(nil) })
	lastRun.Store(nil)

	if err := r.Down(context.Background(), 0); err == nil {
		t.Fatal("Down accepted n = 0")
	}

	rec := lastRun.Load()
	if rec == nil {
		t.Fatal("Down returned early without recording")
	}
	if rec.Operation != "down" || rec.Err == nil || rec.Count != 0 {
		t.Errorf("recorded %+v, want a failed down that moved nothing", rec)
	}
}

// TestARunWithNoRegisteredRunnerIsStillReported covers the Runner built as a struct
// literal, which the exported fields allow.
func TestARunWithNoRegisteredRunnerIsStillReported(t *testing.T) {
	current.Store(nil)
	lastRun.Store(nil)
	t.Cleanup(func() { current.Store(nil); lastRun.Store(nil) })

	record("status", time.Now(), 4, nil)

	report := probe()
	if report.Status != diag.StatusOff {
		t.Fatalf("status = %q, want %q — no runner is registered", report.Status, diag.StatusOff)
	}
	if report.Detail["last_run"] == nil {
		t.Error("the run was not reported even though it happened")
	}
	if !strings.Contains(strings.Join(report.Notes, " "), "struct literal") {
		t.Errorf("no note explains why the configuration is absent: %q", report.Notes)
	}
}

func TestProbeIsRegisteredByNew(t *testing.T) {
	New(nil, fstest.MapFS{})
	t.Cleanup(func() { current.Store(nil); lastRun.Store(nil) })

	if _, found := diag.Get("migrate"); !found {
		t.Errorf("no \"migrate\" probe after New; registered: %v", diag.Registered())
	}
}
