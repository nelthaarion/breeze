package migrate

// diag.go — the migration runner's diagnostic probe.
//
// # What this can and cannot report
//
// Every other probe in the framework reports state its subsystem already holds.
// This one cannot: the questions someone actually asks about migrations — how many
// are pending, is the ledger consistent with the files — are answers that live in
// the database, and reaching them means a query. A probe that queries is a probe
// that hangs when the database is the thing that is wrong, which is precisely when
// the diagnostics endpoint is being read.
//
// So this probe reports two things instead, both instant:
//
//   - the configuration: whether a runner exists, whether it has a database handle
//     and a migration filesystem, and the handle's connection statistics, which
//     database/sql already keeps.
//   - the outcome of the last run in this process: which operation, when, how many
//     migrations it moved, and the error if it failed.
//
// The second is the useful half, and it is the half nothing else records. `breeze
// migrate` prints its result to a terminal that is gone by the time anyone asks;
// an application that runs migrations at startup logs a line into a stream nobody
// is tailing. A failed migration at boot is one of the few faults that leaves a
// process running and serving wrong answers, and before this there was no way to
// ask a live service whether that had happened.
//
// # Cost
//
// A migration is a transaction against a database. Recording three numbers around
// one is not measurable, so nothing here is gated: these counts are exact whether
// or not counted diagnostics were enabled.

import (
	"fmt"
	"sync/atomic"
	"time"

	"github.com/nelthaarion/breeze/diag"
)

// diagName is the registry key, matching the `breeze add migrator` feature name.
//
// "migrate" rather than "migrator": the feature installs a binary called migrate,
// the CLI verb is `breeze migrate`, and that is the word a reader will reach for.
const diagName = "migrate"

// lastRun is the outcome of the most recent Up, Down or Status in this process.
//
// A pointer swapped atomically rather than a struct behind a mutex, so the probe
// never blocks on a migration in flight — a long DDL statement holding a lock the
// probe wanted would be the same failure this file exists to avoid.
var lastRun atomic.Pointer[runRecord]

// runRecord is one completed migration operation.
type runRecord struct {
	Operation string
	At        time.Time
	Count     int
	Duration  time.Duration
	Err       error
}

// runner registration. A process constructs at most one Runner in practice — the
// migrate binary does, an application that migrates at startup does — so the
// last one registered is the one being asked about.
var current atomic.Pointer[Runner]

// registerDiagnostics publishes r as the process's migration diagnostic.
//
// Called by New. A Runner built by hand as a struct literal — which the exported
// fields allow — is not registered, and its runs still record into lastRun, so the
// probe reports the operations without the configuration. That is the honest
// outcome and better than either alternative: refusing to record, or claiming a
// configuration this package never saw.
func (r *Runner) registerDiagnostics() {
	if r == nil {
		return
	}
	current.Store(r)
	diag.Register(diagName, probe)
}

// record stores the outcome of one operation.
//
// Called from a defer in Up, Down and Status, so a panic or an early return still
// records — an operation that failed is the one worth reporting.
func record(operation string, start time.Time, count int, err error) {
	lastRun.Store(&runRecord{
		Operation: operation,
		At:        start,
		Count:     count,
		Duration:  time.Since(start),
		Err:       err,
	})
}

// probe reports the runner's configuration and the last operation.
func probe() diag.Report {
	r := current.Load()
	last := lastRun.Load()

	if r == nil {
		report := diag.Off("no migration runner is registered; build one with " +
			"migrate.New(db, fsys) (`breeze add migrator` generates cmd/migrate)")
		if last != nil {
			// A run with no registered runner means someone built a Runner as a
			// struct literal. Reporting the run is more useful than reporting the
			// absence of the thing that performed it.
			return report.WithDetail("last_run", describeRun(last)).
				WithNotes("A migration has run in this process even though no runner is " +
					"registered, which means the Runner was built as a struct literal rather " +
					"than with migrate.New. Its configuration is therefore not reported here.")
		}
		return report
	}

	detail := map[string]any{
		"database":   r.DB != nil,
		"migrations": r.FS != nil,
	}
	if r.DB != nil {
		// database/sql keeps these already; reading them is a mutex-guarded struct
		// copy and touches no connection.
		st := r.DB.Stats()
		detail["connections"] = map[string]any{
			"open":            st.OpenConnections,
			"in_use":          st.InUse,
			"idle":            st.Idle,
			"wait_count":      st.WaitCount,
			"wait_duration":   st.WaitDuration.String(),
			"max_open":        st.MaxOpenConnections,
			"max_idle_closed": st.MaxIdleClosed,
		}
	}
	if last != nil {
		detail["last_run"] = describeRun(last)
	}

	notes := []string{
		"Pending-migration counts are not reported here: answering that needs a query, and a " +
			"probe that queries hangs when the database is the problem — which is when this " +
			"endpoint gets read. Run `breeze migrate status` for the live answer.",
	}

	// A missing database handle is not a warning, it is a runner that cannot work.
	if r.DB == nil || r.FS == nil {
		missing := "database handle"
		if r.DB != nil {
			missing = "migration filesystem"
		}
		return diag.Degraded("the migration runner has no "+missing+", so every operation will "+
			"fail", detail).WithNotes(notes...)
	}

	if last == nil {
		return diag.OK("configured; no migration has run in this process", detail).
			WithNotes(notes...)
	}
	if last.Err != nil {
		return diag.Degraded(fmt.Sprintf("the last migration operation failed: %s %s: %v",
			last.Operation, humanAgo(last.At), last.Err), detail).
			WithNotes(append(notes, "A failed migration leaves the schema in whatever state the "+
				"last committed transaction produced. The process is still running and serving "+
				"requests against that schema.")...)
	}
	return diag.OK(fmt.Sprintf("configured; last operation %s moved %d migration(s) %s",
		last.Operation, last.Count, humanAgo(last.At)), detail).WithNotes(notes...)
}

// describeRun renders a runRecord for a report's Detail.
func describeRun(r *runRecord) map[string]any {
	out := map[string]any{
		"operation": r.Operation,
		"at":        r.At.UTC().Format(time.RFC3339Nano),
		"count":     r.Count,
		"duration":  r.Duration.String(),
		"ok":        r.Err == nil,
	}
	if r.Err != nil {
		out["error"] = r.Err.Error()
	}
	return out
}

// humanAgo renders how long ago t was, for a summary line.
//
// A relative time rather than a timestamp, because "4 minutes ago" answers "is
// this the run I just did" and an RFC 3339 string makes the reader do arithmetic.
// The absolute time is in Detail for anyone who needs it.
func humanAgo(t time.Time) string {
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d second(s) ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d minute(s) ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hour(s) ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%d day(s) ago", int(d.Hours()/24))
	}
}
