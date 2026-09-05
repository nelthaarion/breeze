package diag

// diag_test.go — the registry's own behaviour.
//
// Four properties are worth a test here, and they are the four a caller relies on
// without being able to see:
//
//   - Register replaces rather than appends, so a process that wires a subsystem
//     twice has one report and not two.
//   - Snapshot is sorted, so two reads of an unchanged process are comparable.
//   - A panicking probe is contained, so one broken subsystem cannot hide the
//     others.
//   - Counting is off until enabled, and every counter-backed number says which.
//
// These use the unexported reset, so they must live in the package. That is also
// why they cannot run in parallel with each other: the registry is process-wide by
// design, and pretending otherwise in a test would be testing a different thing.

import (
	"sync"
	"testing"
	"time"
)

func TestRegisterReplacesRatherThanAppends(t *testing.T) {
	reset()
	t.Cleanup(reset)

	Register("events", func() Report { return OK("first", nil) })
	Register("events", func() Report { return OK("second", nil) })

	got := Snapshot()
	if len(got) != 1 {
		t.Fatalf(
			"registering the same name twice produced %d report(s), want 1: %+v",
			len(got),
			got,
		)
	}
	if got[0].Summary != "second" {
		t.Errorf("summary = %q, want the last registration to win", got[0].Summary)
	}
}

func TestUnregisterRemovesTheProbe(t *testing.T) {
	reset()
	t.Cleanup(reset)

	Register("events", func() Report { return OK("here", nil) })
	Register("router", func() Report { return OK("here", nil) })
	Unregister("events")

	if names := Registered(); len(names) != 1 || names[0] != "router" {
		t.Errorf("Registered() = %v, want [router]", names)
	}
	if _, found := Get("events"); found {
		t.Error("Get found an unregistered subsystem")
	}
}

// TestRegisterIgnoresAnEmptyName is the guard against a probe filed under "".
//
// A report with no subsystem name is unusable by anything downstream — the
// dashboard renders a blank row, the MCP tool cannot be asked for it by name — so
// the registry refuses it at the door rather than storing something unreachable.
func TestRegisterIgnoresAnEmptyName(t *testing.T) {
	reset()
	t.Cleanup(reset)

	Register("", func() Report { return OK("nameless", nil) })
	if names := Registered(); len(names) != 0 {
		t.Errorf("Registered() = %v, want empty", names)
	}
}

func TestSnapshotIsSortedBySubsystem(t *testing.T) {
	reset()
	t.Cleanup(reset)

	for _, name := range []string{"workflow", "events", "router", "compression"} {
		Register(name, func() Report { return OK("ok", nil) })
	}

	got := Snapshot()
	want := []string{"compression", "events", "router", "workflow"}
	if len(got) != len(want) {
		t.Fatalf("got %d report(s), want %d", len(got), len(want))
	}
	for i, name := range want {
		if got[i].Subsystem != name {
			t.Errorf("report %d is %q, want %q", i, got[i].Subsystem, name)
		}
	}
}

// TestSnapshotOverwritesTheReportedName is why run does not trust the report.
//
// A probe copy-pasted from another subsystem keeps its Subsystem assignment, and
// filing that report under the original's name would make one subsystem invisible
// and another appear twice. The registry key is authoritative.
func TestSnapshotOverwritesTheReportedName(t *testing.T) {
	reset()
	t.Cleanup(reset)

	Register("etag", func() Report {
		r := OK("copied from elsewhere", nil)
		r.Subsystem = "compression"
		return r
	})

	got := Snapshot()
	if len(got) != 1 || got[0].Subsystem != "etag" {
		t.Fatalf("got %+v, want the report filed under its registry key", got)
	}
}

// TestAPanickingProbeIsContainedAndTheOthersStillRun is the property that makes
// this endpoint usable on a process that is already unwell.
func TestAPanickingProbeIsContainedAndTheOthersStillRun(t *testing.T) {
	reset()
	t.Cleanup(reset)

	Register("aaa", func() Report { return OK("fine", nil) })
	Register("bbb", func() Report { panic("probe is broken") })
	Register("ccc", func() Report { return OK("also fine", nil) })

	got := Snapshot()
	if len(got) != 3 {
		t.Fatalf("got %d report(s), want 3 — a panic must not drop the others", len(got))
	}

	if got[1].Status != StatusUnknown {
		t.Errorf("the panicking probe reported %q, want %q", got[1].Status, StatusUnknown)
	}
	if len(got[1].Notes) == 0 {
		t.Error("the panicking probe produced no note explaining why")
	}
	for _, i := range []int{0, 2} {
		if got[i].Status != StatusOK {
			t.Errorf("%s reported %q, want it unaffected", got[i].Subsystem, got[i].Status)
		}
	}
}

// TestAnEmptyStatusBecomesUnknown covers the probe that forgot to set one.
//
// The zero Status is the empty string, which would serialise as "" and read as a
// missing field rather than as an unanswered question.
func TestAnEmptyStatusBecomesUnknown(t *testing.T) {
	reset()
	t.Cleanup(reset)

	Register("silent", func() Report { return Report{Summary: "no status set"} })

	report, found := Get("silent")
	if !found {
		t.Fatal("Get did not find the registered probe")
	}
	if report.Status != StatusUnknown {
		t.Errorf("status = %q, want %q", report.Status, StatusUnknown)
	}
}

// TestConcurrentRegistrationAndSnapshotDoNotRace exercises the copy-on-write.
//
// Without -race this proves only that nothing panics or deadlocks, which is still
// the failure mode worth catching: a torn slice read here would take down the
// diagnostics endpoint of a process that was being diagnosed.
func TestConcurrentRegistrationAndSnapshotDoNotRace(t *testing.T) {
	reset()
	t.Cleanup(reset)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = Snapshot()
				_ = Registered()
			}
		}
	}()

	for i := 0; i < 200; i++ {
		Register("subsystem", func() Report { return OK("churn", nil) })
		Unregister("subsystem")
	}
	close(stop)
	wg.Wait()
}

func TestGetReportsWhetherTheSubsystemExists(t *testing.T) {
	reset()
	t.Cleanup(reset)

	Register("router", func() Report { return OK("2 routes", nil) })

	if report, found := Get("router"); !found || report.Summary != "2 routes" {
		t.Errorf("Get(router) = %+v, %v", report, found)
	}
	if _, found := Get("nothing-here"); found {
		t.Error("Get invented a subsystem")
	}
}

// TestRegisteredDoesNotRunProbes is the reason Registered exists at all.
//
// "Which subsystems can answer" must be cheaper than "what does every subsystem
// say", or a caller listing the names pays for every probe to render a menu.
func TestRegisteredDoesNotRunProbes(t *testing.T) {
	reset()
	t.Cleanup(reset)

	ran := false
	Register("events", func() Report {
		ran = true
		return OK("ok", nil)
	})

	_ = Registered()
	if ran {
		t.Error("Registered ran a probe")
	}
	_ = Snapshot()
	if !ran {
		t.Error("Snapshot did not run the probe")
	}
}

// TestNotesAreTrimmedAndEmptiesDropped keeps the wire format free of blank notes.
func TestNotesAreTrimmedAndEmptiesDropped(t *testing.T) {
	report := OK("fine", nil).WithNotes("  spaced  ", "", "   ", "second")
	if len(report.Notes) != 2 {
		t.Fatalf("notes = %q, want the empties dropped", report.Notes)
	}
	if report.Notes[0] != "spaced" || report.Notes[1] != "second" {
		t.Errorf("notes = %q, want them trimmed", report.Notes)
	}
}

func TestWithDetailBuildsTheMapWhenAbsent(t *testing.T) {
	report := Off("not installed").WithDetail("routes", 0).WithDetail("dir", "./public")
	if report.Detail["routes"] != 0 || report.Detail["dir"] != "./public" {
		t.Errorf("detail = %v", report.Detail)
	}
}

// TestOffIsNotDegraded is a contract test for consumers.
//
// The dashboard renders these differently and the MCP tool sorts on them, so "not
// installed" collapsing into "unhappy" would make every un-added feature look like
// an incident.
func TestOffIsNotDegraded(t *testing.T) {
	if Off("nope").Status == Degraded("bad", nil).Status {
		t.Fatal("Off and Degraded produced the same status")
	}
	if Off("nope").Status != StatusOff {
		t.Errorf("Off produced %q", Off("nope").Status)
	}
}

// ─── counters ────────────────────────────────────────────────────────────────

// TestCountersAreOffUntilEnabled is the zero-cost claim, tested from the outside:
// a counter incremented while the gate is closed reports nothing.
func TestCountersAreOffUntilEnabled(t *testing.T) {
	DisableCounters()
	t.Cleanup(DisableCounters)

	var c Counter
	for i := 0; i < 10; i++ {
		c.Hit()
		c.Miss()
		c.Error()
		c.Bytes(100)
		c.Saved(50)
		c.HitBytes(100, 50)
	}

	snap := c.Snapshot()
	if snap.Counting {
		t.Error("Counting is true with the gate closed")
	}
	if snap.Hits != 0 || snap.Misses != 0 || snap.Errors != 0 || snap.Bytes != 0 ||
		snap.BytesSaved != 0 {
		t.Errorf("counters moved with the gate closed: %+v", snap)
	}
	if snap.Last != "" {
		t.Errorf("Last = %q, want empty with the gate closed", snap.Last)
	}
}

func TestCountersRecordOnceEnabled(t *testing.T) {
	DisableCounters()
	EnableCounters()
	t.Cleanup(DisableCounters)

	var c Counter
	c.Hit()
	c.Hit()
	c.Miss()
	c.Error()
	c.HitBytes(1000, 400)

	snap := c.Snapshot()
	if !snap.Counting {
		t.Fatal("Counting is false after EnableCounters")
	}
	if snap.Hits != 3 {
		t.Errorf("Hits = %d, want 3 (two Hit plus one HitBytes)", snap.Hits)
	}
	if snap.Misses != 1 || snap.Errors != 1 {
		t.Errorf("Misses = %d, Errors = %d, want 1 and 1", snap.Misses, snap.Errors)
	}
	if snap.Bytes != 1000 || snap.BytesSaved != 400 {
		t.Errorf("Bytes = %d, BytesSaved = %d, want 1000 and 400", snap.Bytes, snap.BytesSaved)
	}
	if snap.Last == "" {
		t.Error("Last is empty after counted activity")
	}
	if snap.Total() != 4 {
		t.Errorf("Total() = %d, want 4", snap.Total())
	}
	if got := snap.Rate(); got != 0.75 {
		t.Errorf("Rate() = %v, want 0.75", got)
	}
}

// TestDisableCountersKeepsWhatWasCounted is why Disable does not zero.
//
// A caller that turned counting off for a benchmark must not thereby destroy the
// evidence it had already collected.
func TestDisableCountersKeepsWhatWasCounted(t *testing.T) {
	DisableCounters()
	EnableCounters()
	t.Cleanup(DisableCounters)

	var c Counter
	c.Hit()
	DisableCounters()
	c.Hit() // ignored

	snap := c.Snapshot()
	if snap.Hits != 1 {
		t.Errorf(
			"Hits = %d, want the pre-disable count kept and the post-disable one ignored",
			snap.Hits,
		)
	}
	if snap.Counting {
		t.Error("Counting is true after DisableCounters")
	}
}

func TestCountersSinceReportsTheWindow(t *testing.T) {
	DisableCounters()
	t.Cleanup(DisableCounters)

	if _, on := CountersSince(); on {
		t.Fatal("counting is on before EnableCounters")
	}

	before := time.Now()
	EnableCounters()
	since, on := CountersSince()
	if !on {
		t.Fatal("counting is off after EnableCounters")
	}
	if since.Before(before.Add(-time.Second)) || since.After(time.Now().Add(time.Second)) {
		t.Errorf("CountersSince() = %v, want a time around %v", since, before)
	}
}

// TestEnableCountersIsIdempotentAndKeepsTheOriginalWindow matters because both
// dashboard.Install and mcp.StartInProcess call it, and a process with both must
// not have its window reset by whichever ran second.
func TestEnableCountersIsIdempotentAndKeepsTheOriginalWindow(t *testing.T) {
	DisableCounters()
	EnableCounters()
	t.Cleanup(DisableCounters)

	first, _ := CountersSince()
	time.Sleep(2 * time.Millisecond)
	EnableCounters()
	second, _ := CountersSince()

	if !first.Equal(second) {
		t.Errorf("the window moved from %v to %v on a second EnableCounters", first, second)
	}
}

// TestANilCounterIsSafe covers the subsystem that has not been given one.
func TestANilCounterIsSafe(t *testing.T) {
	EnableCounters()
	t.Cleanup(DisableCounters)

	var c *Counter
	c.Hit()
	c.Miss()
	c.Error()
	c.Bytes(10)
	c.Saved(10)
	c.HitBytes(10, 10)

	snap := c.Snapshot()
	if snap.Hits != 0 || snap.Total() != 0 || snap.Rate() != 0 {
		t.Errorf("a nil counter reported %+v", snap)
	}
	if !snap.Counting {
		t.Error("a nil counter's snapshot lost the gate state")
	}
}

func TestBytesIgnoresNonPositiveValues(t *testing.T) {
	EnableCounters()
	t.Cleanup(DisableCounters)

	var c Counter
	c.Bytes(0)
	c.Bytes(-100)
	c.Saved(-1)
	if snap := c.Snapshot(); snap.Bytes != 0 || snap.BytesSaved != 0 {
		t.Errorf("negative and zero byte counts were recorded: %+v", snap)
	}
}
