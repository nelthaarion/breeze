package mcp_test

// Tests for breeze_get_signals and breeze_get_health.
//
// Like the other live tools these read a real dashboard. The service here has a real event
// bus attached to a real observability collector, listeners that really fail, health checks
// that really report red, and a connection that really goes down: the tools mirror the
// dashboard's JSON, and a stub would only prove the mirror matches itself.
//
// Two dashboards are built and never closed, for the reason the fleet fixtures give: Breeze
// has no Stop, and a Cleanup registered by whichever test got here first would take the
// service from the others. One is fully wired, and one is a bare install with nothing
// attached, which is the state the "not wired" answers are about.

import (
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nelthaarion/breeze/v2"
	"github.com/nelthaarion/breeze/v2/dashboard"
	"github.com/nelthaarion/breeze/v2/events"
	"github.com/nelthaarion/breeze/v2/observability"
)

// obsSecret is in an event's payload. The dashboard serves payloads to its own UI; the tool
// must not.
const obsSecret = "pan-9999-do-not-leak"

type (
	obsOrderPlaced  struct{ ID, Card string }
	obsUserSignedUp struct{ Name string }
)

var (
	wiredOnce sync.Once
	wiredURL  string
	bareOnce  sync.Once
	bareURL   string
)

func startDashboard(t *testing.T, wire func(coll *dashboard.Collector)) string {
	t.Helper()

	port := freeLivePort(t)
	router := breeze.NewRouter()
	app := breeze.New(router, breeze.NewEventLoopWorkerPool(runtime.NumCPU()))

	cfg := dashboard.DefaultConfig()
	cfg.DisableAuth = true
	coll := dashboard.Install(app, router, cfg)
	if wire != nil {
		wire(coll)
	}

	go func() { _ = app.Run(port, false) }()
	waitForLivePort(t, port, 15*time.Second)
	return "http://127.0.0.1:" + strconv.Itoa(port)
}

// startWiredDashboard is a service with its insides instrumented and some of them broken.
func startWiredDashboard(t *testing.T) string {
	t.Helper()

	wiredOnce.Do(func() {
		wiredURL = startDashboard(t, func(coll *dashboard.Collector) {
			bus := events.New()
			// With payloads: the dashboard will serve them, so the tool's refusal to is
			// something the test can see.
			coll.AttachEventsWithPayload(bus)

			// One event type whose listener always fails, one that always succeeds.
			events.OnBus(bus, obsOrderPlaced{}, func(_ *events.Context, e obsOrderPlaced) error {
				return errors.New("charge declined for order " + e.ID)
			})
			events.OnBus(bus, obsUserSignedUp{}, func(_ *events.Context, _ obsUserSignedUp) error { return nil })

			for i := 0; i < 3; i++ {
				_ = events.EmitBus(bus, obsOrderPlaced{ID: fmt.Sprintf("o-%d", i), Card: obsSecret})
			}
			for i := 0; i < 5; i++ {
				_ = events.EmitBus(bus, obsUserSignedUp{Name: fmt.Sprintf("user-%d", i)})
			}

			// Health: a database that is not answering and a cache that is.
			coll.RegisterHealthCheck("postgres", func() (string, string) { return "red", "connection refused" })
			coll.RegisterHealthCheck("redis", func() (string, string) { return "green", "PONG" })

			// Connections: the database went down, the cache is up, the queue was never checked.
			coll.RegisterConnection(dashboard.Connection{
				ID: "primary-db", Name: "PostgreSQL Primary", Type: dashboard.ConnDatabase,
				Driver: "postgres", Host: "10.0.0.5:5432", Status: dashboard.StatusConnected,
			})
			coll.RegisterConnection(dashboard.Connection{
				ID: "cache", Name: "Redis", Type: dashboard.ConnCache,
				Driver: "redis", Host: "10.0.0.6:6379", Status: dashboard.StatusConnected,
			})
			coll.RegisterConnection(dashboard.Connection{
				ID: "jobs", Name: "RabbitMQ", Type: dashboard.ConnMessageQueue,
				Driver: "rabbitmq", Host: "10.0.0.7:5672", Status: dashboard.StatusUnknown,
			})
			coll.UpdateConnectionStatus("primary-db", dashboard.StatusDisconnected, 0, "dial tcp 10.0.0.5:5432: i/o timeout")
		})
	})
	return wiredURL
}

// startBareDashboard has nothing attached, registered or declared.
func startBareDashboard(t *testing.T) string {
	t.Helper()

	bareOnce.Do(func() { bareURL = startDashboard(t, nil) })
	return bareURL
}

// jsonText renders a decoded result back to text, for asserting on everything it contains
// rather than on the fields a test happened to think of.
func jsonText(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

// ── breeze_get_signals ───────────────────────────────────────────────────────

// TestGetSignalsFindsTheFailingNameAndNeverReturnsPayloads: a listener that fails is not an
// HTTP error, so no other tool shows it. The failing list names it with its rate, the
// rows carry the error, and the event's payload, which the dashboard serves, does not
// reach the caller.
func TestGetSignalsFindsTheFailingNameAndNeverReturnsPayloads(t *testing.T) {
	url := startWiredDashboard(t)

	got, summary, isErr := callLiveTool(t, "breeze_get_signals", creds(url))
	if isErr {
		t.Fatalf("breeze_get_signals failed: %s", summary)
	}
	if got["attached"] != true {
		t.Fatalf("attached = %v, want true", got["attached"])
	}

	failing := asRows(t, got["failing"])
	if len(failing) != 1 {
		t.Fatalf("failing = %v, want only the event whose listener fails", got["failing"])
	}
	f := failing[0]
	if name, _ := f["name"].(string); !strings.Contains(name, "obsOrderPlaced") {
		t.Errorf("failing name = %q, want the OrderPlaced event", name)
	}
	if f["failed"] != float64(3) || f["failure_rate"] != float64(1) {
		t.Errorf("failing counters = %v, want 3 failures at rate 1", f)
	}

	rows := asRows(t, got["recent"])
	if len(rows) != 3 {
		t.Fatalf("recent = %d rows, want the 3 failed dispatches and nothing else (failed-only is the default)", len(rows))
	}
	for _, r := range rows {
		if r["failed"] != true {
			t.Errorf("a row that did not fail was returned: %v", r)
		}
		if msg, _ := r["error"].(string); !strings.Contains(msg, "charge declined") {
			t.Errorf("the row does not carry the failure: %v", r)
		}
	}

	// The property that matters for something read into a model's context.
	raw, _ := jsonText(got)
	if strings.Contains(raw, obsSecret) || strings.Contains(raw, `"payload"`) {
		t.Errorf("an event payload reached the caller:\n%s", raw)
	}
}

func TestGetSignalsFailedFalseShowsEverything(t *testing.T) {
	url := startWiredDashboard(t)

	args := creds(url)
	args["failed"] = false
	got, summary, isErr := callLiveTool(t, "breeze_get_signals", args)
	if isErr {
		t.Fatalf("breeze_get_signals failed: %s", summary)
	}

	rows := asRows(t, got["recent"])
	if len(rows) < 8 {
		t.Errorf("recent = %d rows, want the 3 failed and the 5 that succeeded", len(rows))
	}
	succeeded := 0
	for _, r := range rows {
		if r["failed"] == false {
			succeeded++
		}
	}
	if succeeded < 5 {
		t.Errorf("only %d successful signals returned with failed=false", succeeded)
	}
}

func TestGetSignalsRefusesASourceItDoesNotKnow(t *testing.T) {
	url := startWiredDashboard(t)

	args := creds(url)
	args["source"] = "databse" // a typo: the dashboard would answer it with an empty list
	_, summary, isErr := callLiveTool(t, "breeze_get_signals", args)
	if !isErr {
		t.Fatal("an unknown source was accepted, so a typo reads as 'nothing failed there'")
	}
	for _, want := range []string{"unknown source", "databse", "database", "scheduler"} {
		if !strings.Contains(summary, want) {
			t.Errorf("the refusal %q does not mention %q", summary, want)
		}
	}

	args["source"] = "database"
	if _, summary, isErr := callLiveTool(t, "breeze_get_signals", args); isErr {
		t.Errorf("a real source was refused: %s", summary)
	}
}

// TestGetSignalsSaysWhenNothingIsAttached: with no event bus attached the collector records
// nothing, and an empty list would read as a service with nothing failing.
func TestGetSignalsSaysWhenNothingIsAttached(t *testing.T) {
	url := startBareDashboard(t)

	got, summary, isErr := callLiveTool(t, "breeze_get_signals", creds(url))
	if isErr {
		t.Fatalf("breeze_get_signals failed: %s", summary)
	}
	if got["attached"] != false || !strings.Contains(summary, "no observability collector") {
		t.Errorf("attached = %v, summary = %q, want it said plainly that nothing is recorded", got["attached"], summary)
	}
	notes := joinNotes(got)
	for _, want := range []string{"not evidence", "AttachEvents"} {
		if !strings.Contains(notes, want) {
			t.Errorf("the notes do not say %q:\n%s", want, notes)
		}
	}
}

// TestSignalSourcesMatchTheCollector pins the tool's copy of the source list to the
// constants it mirrors. A source added there and not here would be refused as unknown.
func TestSignalSourcesMatchTheCollector(t *testing.T) {
	want := []observability.Source{
		observability.SourceEvents, observability.SourceRouter, observability.SourceHTTP,
		observability.SourceCache, observability.SourceDatabase, observability.SourceScheduler,
		observability.SourceWebSocket, observability.SourceOAuth2, observability.SourcePlugin,
		observability.SourceWorkflow,
	}

	url := startWiredDashboard(t)
	for _, src := range want {
		args := creds(url)
		args["source"] = string(src)
		if _, summary, isErr := callLiveTool(t, "breeze_get_signals", args); isErr {
			t.Errorf("source %q is a real observability source and the tool refuses it: %s", src, summary)
		}
	}
}

// ── breeze_get_health ────────────────────────────────────────────────────────

func TestGetHealthListsTheDeadDependencyFirst(t *testing.T) {
	url := startWiredDashboard(t)

	got, summary, isErr := callLiveTool(t, "breeze_get_health", creds(url))
	if isErr {
		t.Fatalf("breeze_get_health failed: %s", summary)
	}
	if !strings.Contains(summary, "1 red") || !strings.Contains(summary, "1 disconnected") {
		t.Errorf("summary = %q, want the red check and the disconnected connection counted", summary)
	}

	// The two the application registered, and the dashboard's own, in the order the tool
	// promises: what is wrong first, then by name.
	checks := asRows(t, got["checks"])
	var names []string
	for _, c := range checks {
		names = append(names, fmt.Sprintf("%s:%s", c["name"], c["status"]))
	}
	if want := "postgres:red dashboard:green redis:green"; strings.Join(names, " ") != want {
		t.Fatalf("checks = %v, want %s", names, want)
	}
	if msg, _ := checks[0]["message"].(string); !strings.Contains(msg, "connection refused") {
		t.Errorf("the check does not carry why it is red: %v", checks[0])
	}

	conns := asRows(t, got["connections"])
	var order []string
	for _, c := range conns {
		order = append(order, fmt.Sprintf("%s:%s", c["id"], c["status"]))
	}
	if want := "primary-db:disconnected jobs:unknown cache:connected"; strings.Join(order, " ") != want {
		t.Errorf("connections = %v, want %s: broken first, then unchecked, then healthy", order, want)
	}

	// An unchecked connection is not an up one.
	if notes := joinNotes(got); !strings.Contains(notes, "unknown") || !strings.Contains(notes, "not the same as being up") {
		t.Errorf("an unchecked connection is not distinguished from a healthy one:\n%s", notes)
	}
}

func TestGetHealthSaysWhenNothingIsRegistered(t *testing.T) {
	url := startBareDashboard(t)

	got, summary, isErr := callLiveTool(t, "breeze_get_health", creds(url))
	if isErr {
		t.Fatalf("breeze_get_health failed: %s", summary)
	}
	// The dashboard's own check is there, and reports green. That is not a dependency check
	// and must not be allowed to read as one.
	if checks := asRows(t, got["checks"]); len(checks) != 1 || checks[0]["name"] != "dashboard" {
		t.Fatalf("checks = %v, want only the dashboard's own", got["checks"])
	}
	notes := joinNotes(got)
	for _, want := range []string{
		"RegisterHealthCheck", "dashboard's own", "says nothing about whether this service's dependencies are up",
		"RegisterConnection", "not evidence",
	} {
		if !strings.Contains(notes, want) {
			t.Errorf("an answer with nothing registered is not qualified (%q missing):\n%s", want, notes)
		}
	}
}
