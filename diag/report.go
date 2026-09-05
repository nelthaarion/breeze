package diag

// report.go — the small vocabulary probes are written in.
//
// Every probe in the framework builds its answer with these, for one reason: a
// consumer must be able to rely on the shapes. "off" has to mean the same thing
// in the events report and the fleet report, or a dashboard cannot render them
// in one table and an agent cannot compare them. Hand-rolling each Report would
// make that a matter of thirteen authors' discipline; these constructors make it
// a matter of one file.

import (
	"fmt"
	"strings"
)

// OK builds a healthy report.
func OK(summary string, detail map[string]any) Report {
	return Report{Status: StatusOK, Summary: summary, Detail: detail}
}

// Degraded builds a report for a subsystem that is running and unhappy.
//
// The summary is required to say what is wrong — a degraded report with a
// summary of "degraded" tells a reader only that they now have to go and look
// somewhere else, which is the failure mode this whole package exists to remove.
func Degraded(summary string, detail map[string]any) Report {
	return Report{Status: StatusDegraded, Summary: summary, Detail: detail}
}

// Off builds the not-installed report.
//
// reason is what the reader needs next, not an apology: "no event bus is
// attached; call coll.AttachEvents(events.Default)" is useful, "events are off"
// is not. Every Off in the framework names the call that would turn it on.
func Off(reason string) Report {
	return Report{Status: StatusOff, Summary: reason}
}

// WithNotes attaches notes to a report, returning it.
//
// Chained onto a constructor rather than passed to one, because notes are the
// exception: most reports have none, and threading an always-nil argument
// through every call site would make the common case read worse.
func (r Report) WithNotes(notes ...string) Report {
	for _, n := range notes {
		if trimmed := strings.TrimSpace(n); trimmed != "" {
			r.Notes = append(r.Notes, trimmed)
		}
	}
	return r
}

// WithDetail sets one detail key, returning the report.
//
// For the case where a fact is conditional — a signing secret's presence, a
// configured but unreachable endpoint — and building the whole map in one
// literal would need an if either side of it.
func (r Report) WithDetail(key string, value any) Report {
	if r.Detail == nil {
		r.Detail = make(map[string]any, 4)
	}
	r.Detail[key] = value
	return r
}

// panicText renders a recovered value for a note.
//
// %v rather than %#v: a panic value is usually an error or a string, and the Go
// syntax representation of a large struct would bury the message in a field
// dump. The type is named because a bare "runtime error: index out of range"
// with no indication it came from a panic reads like a finding rather than a
// broken probe.
func panicText(r any) string {
	return fmt.Sprintf("the probe panicked with %T: %v", r, r)
}
