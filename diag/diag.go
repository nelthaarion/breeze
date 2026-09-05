// Package diag is the framework's diagnostic registry: one place that can be
// asked "what is every subsystem of this process actually doing right now?".
//
// # Why this package exists at all
//
// Breeze has a dozen subsystems that each know something an operator or an agent
// needs — the event bus knows its listener counts, the fleet tracer knows how
// many spans it failed to export, the template engine knows whether it is
// re-parsing on every render. Before this package each of those facts was
// reachable only by holding a typed handle to that subsystem, which means a
// diagnostic tool had to be handed every handle the application constructed.
// Nothing had the whole picture, so nothing could report it.
//
// # Why it is a separate package with no dependencies
//
// It has to be. The import graph is:
//
//	breeze      → binding, rpc, scalar, internal/mcp
//	dashboard   → breeze, events, observability, video
//	fleet       → dashboard
//	workflow    → events, observability          (not breeze)
//
// A registry living in the root breeze package could not be used by events,
// workflow, observability or scalar, because breeze imports scalar and those
// packages are deliberately below it. A registry living in dashboard could not
// be used by anything, since almost everything is below dashboard. So the
// registry is a leaf: it imports nothing but the standard library, which makes
// it importable from every layer including the lowest.
//
// # Zero cost
//
// Nothing here runs on a request path. Registration happens once, while an
// application is being wired, and costs one slice append. Reading happens only
// when someone asks — a dashboard page, an MCP tool call — and is the only time
// a probe function is ever invoked.
//
// That is the whole performance story for the registry itself: a process that
// never reads its diagnostics pays for one pointer per registered subsystem and
// not one instruction per request. Subsystems that additionally want *counted*
// diagnostics use [Counters], which is documented separately and is off by
// default for the same reason.
//
// # The rule probes must follow
//
// A probe reports state a subsystem already has. It must not do I/O, must not
// take a lock it holds across a callback, and must not block: the diagnostics
// endpoint is frequently the thing an operator reaches for when the process is
// already unwell, and a probe that waits on a database is a probe that turns a
// slow query into an unanswerable endpoint. Where a fact genuinely requires I/O
// — how many migrations are pending, say — the probe reports the configuration
// and says the count needs a live query, which is honest and instant.
//
// # No probe reports a credential
//
// A Report goes out over HTTP. The dashboard endpoint that serves them is behind
// basic auth that is a *no-op* when Username or Password is empty — which is what
// `breeze add dashboard --no-auth` generates — so a probe must not assume its
// reader is trusted.
//
// Secrets are therefore reported by presence or by size, never by value:
//
//	"service_token": true          // it is configured
//	"signed_urls":   true          // a signing key is set
//	"secret_bytes":  32            // long enough
//
// and never:
//
//	"access_secret": opts.AccessSecret
//
// The distinction is not stylistic. Presence and length are what a reader needs —
// "is this wired up", "is this key long enough to be a key" — and neither can be
// turned back into the secret. A probe that holds a pointer to a config struct is
// one field access away from leaking one, so the convention is to copy out the
// facts at construction and let the secret stay where it was. See
// middlewares.jwtFacts for the shape.
package diag

import (
	"sort"
	"sync"
	"sync/atomic"
)

// Status is a subsystem's headline state.
//
// Four values rather than a bool, because "installed and idle", "installed and
// unhappy" and "not installed" are three different answers and collapsing them
// is what makes a dashboard lie. An empty Detail with StatusOK reads as a
// healthy quiet subsystem; the same Detail with StatusOff reads as one that was
// never wired up.
type Status string

const (
	// StatusOK is wired up and behaving.
	StatusOK Status = "ok"

	// StatusDegraded is wired up and reporting something an operator should
	// look at — export failures, a full queue, a rejected configuration.
	StatusDegraded Status = "degraded"

	// StatusOff is not installed, or installed and deliberately disabled. It is
	// not a fault, and a caller must not render it as one.
	StatusOff Status = "off"

	// StatusUnknown is registered but unable to answer, which is what a probe
	// that panicked is reported as. Distinct from StatusDegraded because the
	// subsystem may be perfectly healthy and only its probe broken.
	StatusUnknown Status = "unknown"
)

// Report is one subsystem's answer.
type Report struct {
	// Subsystem is the registry key: "events", "fleet", "templates". Lower
	// case, stable, and the same string the feature is called on the command
	// line wherever one exists — an agent that read `breeze add ratelimit`
	// should be able to ask for "ratelimit" without a translation table.
	Subsystem string `json:"subsystem"`

	// Status is the headline.
	Status Status `json:"status"`

	// Summary is one line a human reads first. It should name the numbers that
	// matter rather than restating the status: "412 spans exported, 3 export
	// failures" beats "tracer is degraded".
	Summary string `json:"summary"`

	// Detail is the structured body. Values must be JSON-encodable scalars,
	// slices or maps; a probe returning a live handle here would leak it
	// through the endpoint.
	Detail map[string]any `json:"detail,omitempty"`

	// Notes record what this report could not determine, and why. A reader who
	// sees no note is entitled to treat the numbers as complete.
	Notes []string `json:"notes,omitempty"`
}

// Probe answers for one subsystem.
//
// It is called only when diagnostics are read. A probe is expected to be cheap
// and non-blocking; see the package comment for the rule.
type Probe func() Report

// entry pairs a name with its probe, so the registry can be sorted by name
// without depending on map iteration order.
type entry struct {
	name  string
	probe Probe
}

// registry holds the probes.
//
// Copy-on-write behind an atomic pointer: registration takes the mutex and
// publishes a new slice, and Snapshot does a single atomic load with no lock at
// all. Registration happens a handful of times at startup and reads happen when
// a human asks, so this is not chosen for throughput — it is chosen so that
// reading diagnostics can never contend with anything, including with another
// reader on a process that is already in trouble.
var registry struct {
	mu      sync.Mutex
	entries atomic.Pointer[[]entry]
}

// Register records a probe under name, replacing any probe already registered
// under it.
//
// Replacing rather than appending is deliberate. An application that installs
// the dashboard twice, or a test that constructs three event buses, would
// otherwise produce three "events" reports and no way to tell which is the live
// one. Last registration wins, which matches the wiring order: the handle the
// application ends up using is the one it registered last.
//
// A nil probe unregisters, so a subsystem that is torn down can stop claiming
// to exist.
func Register(name string, probe Probe) {
	if name == "" {
		return
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()

	current := deref(registry.entries.Load())
	next := make([]entry, 0, len(current)+1)
	for _, e := range current {
		if e.name != name {
			next = append(next, e)
		}
	}
	if probe != nil {
		next = append(next, entry{name: name, probe: probe})
	}
	sort.Slice(next, func(i, j int) bool { return next[i].name < next[j].name })

	registry.entries.Store(&next)
}

// Unregister removes a probe. Equivalent to registering nil, and named so a
// teardown path reads as one.
func Unregister(name string) { Register(name, nil) }

// Registered reports the names that currently have a probe, sorted.
//
// Useful on its own: "which subsystems can answer" is a cheaper question than
// "what does every subsystem say", and a caller listing what is available
// should not have to run every probe to find out.
func Registered() []string {
	entries := deref(registry.entries.Load())
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.name)
	}
	return out
}

// Snapshot runs every probe and returns the reports, sorted by subsystem.
//
// A probe that panics is reported as StatusUnknown with the panic value in a
// note, and the remaining probes still run. That containment is the point: the
// diagnostics endpoint is most valuable when something is already wrong, and one
// subsystem in a bad enough state to panic while describing itself must not be
// able to hide the twelve reports that would explain it.
func Snapshot() []Report {
	entries := deref(registry.entries.Load())
	out := make([]Report, 0, len(entries))
	for _, e := range entries {
		out = append(out, run(e))
	}
	return out
}

// Get runs one probe by name.
func Get(name string) (Report, bool) {
	for _, e := range deref(registry.entries.Load()) {
		if e.name == name {
			return run(e), true
		}
	}
	return Report{}, false
}

// run invokes one probe with panic containment, and normalises what it returned.
//
// The name is overwritten from the registry key rather than trusted from the
// report, so a copy-pasted probe cannot file its answer under another
// subsystem's name.
func run(e entry) (out Report) {
	defer func() {
		if r := recover(); r != nil {
			out = Report{
				Subsystem: e.name,
				Status:    StatusUnknown,
				Summary:   "the diagnostic probe for this subsystem panicked",
				Notes:     []string{panicText(r)},
			}
		}
	}()

	out = e.probe()
	out.Subsystem = e.name
	if out.Status == "" {
		out.Status = StatusUnknown
	}
	return out
}

// deref is the empty-slice reading of a nil pointer, so callers do not each
// repeat the nil check.
func deref(p *[]entry) []entry {
	if p == nil {
		return nil
	}
	return *p
}

// reset drops every probe. Test-only, and unexported so it cannot be reached
// from an application: a running process clearing its own diagnostics would be
// a way to hide from an operator.
func reset() {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.entries.Store(nil)
}
