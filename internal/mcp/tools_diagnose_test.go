package mcp_test

// tools_diagnose_test.go — breeze_diagnose_service against a real service.
//
// Same reasoning as tools_live_test.go: a stubbed endpoint would be written from
// the same reading of the dashboard's JSON that the tool is written from, so a
// misreading would pass. These reuse that file's fixture, which is a real Breeze
// app with the real dashboard installed — and therefore a real diag registry with
// every probe the fixture's imports registered.
//
// What is asserted is the tool's contract, not any one probe's wording: that the
// registry is reachable through the tool at all, that the counts add up, that the
// list is ordered problems-first, that filters work, and that an unknown subsystem
// is reported as unknown rather than as a missing dashboard.

import (
	"strings"
	"testing"
)

func TestDiagnoseServiceReportsEverySubsystemOfARunningService(t *testing.T) {
	fx := startFixture(t)

	got, summary, isErr := callLiveTool(t, "breeze_diagnose_service", creds(fx.url))
	if isErr {
		t.Fatalf("breeze_diagnose_service reported an error: %s", summary)
	}

	total := numberField(t, got, "total")
	if total < 5 {
		t.Fatalf("total = %v, want the framework's probes to be registered: %+v", total, got)
	}

	subsystems, ok := got["subsystems"].([]any)
	if !ok {
		t.Fatalf("subsystems is %T, want a list", got["subsystems"])
	}
	if len(subsystems) != int(total) {
		t.Errorf("total says %v but %d subsystem(s) were returned", total, len(subsystems))
	}

	// The counts must partition the list. A subsystem counted in none of the four
	// buckets, or in two, would make the summary line wrong in a way a caller
	// cannot detect.
	sum := numberField(t, got, "ok") + numberField(t, got, "degraded") +
		numberField(t, got, "off") + numberField(t, got, "unknown")
	if sum != total {
		t.Errorf("ok+degraded+off+unknown = %v but total = %v", sum, total)
	}

	// The fixture installed the dashboard, so its own probe must be present and
	// must be reporting rather than off — it is the thing serving this call.
	dash := findSubsystem(t, subsystems, "dashboard")
	if status := dash["status"]; status != "ok" && status != "degraded" {
		t.Errorf("the dashboard reports %v while serving this request", status)
	}

	// dashboard.Install enables counted diagnostics, so a fixture with the
	// dashboard must report counting on. This is the assertion that would catch
	// Install losing its diag.EnableCounters call.
	if counting, _ := got["counting"].(bool); !counting {
		t.Error("counting is false on a service with the dashboard installed")
	}
	if since, _ := got["counting_since"].(string); since == "" {
		t.Error("counting is on but counting_since is empty")
	}

	// The router probe is registered by breeze.New, so it is present on any
	// application at all, and its detail is the routing table.
	router := findSubsystem(t, subsystems, "router")
	detail, ok := router["detail"].(map[string]any)
	if !ok {
		t.Fatalf("the router report has no detail: %+v", router)
	}
	if routes, _ := detail["routes"].(float64); routes < 4 {
		t.Errorf("router detail reports %v route(s); the fixture registers at least 4", routes)
	}
}

// TestDiagnoseServiceOrdersProblemsFirst is the reason the tool sorts at all.
//
// A caller reading the first few entries of a fifteen-subsystem document should be
// reading the ones that need attention. Off last is the deliberate half: it is the
// largest group on a typical service and the least likely to be the fault.
func TestDiagnoseServiceOrdersProblemsFirst(t *testing.T) {
	fx := startFixture(t)

	got, _, isErr := callLiveTool(t, "breeze_diagnose_service", creds(fx.url))
	if isErr {
		t.Fatal("breeze_diagnose_service reported an error")
	}

	subsystems, _ := got["subsystems"].([]any)
	rank := map[string]int{"degraded": 0, "unknown": 1, "ok": 2, "off": 3}

	last := -1
	for i, raw := range subsystems {
		entry, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("subsystem %d is %T", i, raw)
		}
		status, _ := entry["status"].(string)
		r, known := rank[status]
		if !known {
			t.Fatalf("subsystem %v reported an unrecognised status %q", entry["subsystem"], status)
		}
		if r < last {
			t.Errorf("subsystem %d (%v, %s) sorts before a %s entry",
				i, entry["subsystem"], status, statusName(rank, last))
		}
		last = r
	}
}

// TestDiagnoseServiceReadsOneSubsystem covers the path that does not run every
// probe, which is a different response shape rather than a filtered document.
func TestDiagnoseServiceReadsOneSubsystem(t *testing.T) {
	fx := startFixture(t)

	args := creds(fx.url)
	args["subsystem"] = "router"

	got, summary, isErr := callLiveTool(t, "breeze_diagnose_service", args)
	if isErr {
		t.Fatalf("breeze_diagnose_service reported an error: %s", summary)
	}

	if got["subsystem"] != "router" {
		t.Errorf("subsystem = %v, want router", got["subsystem"])
	}
	if _, present := got["subsystems"]; present {
		t.Error("a single-subsystem read returned the whole document")
	}
	if got["status"] == nil || got["summary"] == nil {
		t.Errorf("the single report is missing status or summary: %+v", got)
	}
}

// TestDiagnoseServiceReportsAnUnknownSubsystemAsUnknown is why liveRequest's
// notFound override is used here.
//
// The endpoint answers 404 both for "the dashboard is not installed" and for "no
// such subsystem". The default reading of a 404 is the first, which would send a
// caller to reinstall a feature that is working.
func TestDiagnoseServiceReportsAnUnknownSubsystemAsUnknown(t *testing.T) {
	fx := startFixture(t)

	args := creds(fx.url)
	args["subsystem"] = "not-a-real-subsystem"

	_, summary, isErr := callLiveTool(t, "breeze_diagnose_service", args)
	if !isErr {
		t.Fatalf("an unknown subsystem was not reported as an error: %s", summary)
	}
	if !strings.Contains(summary, "not-a-real-subsystem") {
		t.Errorf("the error does not name the subsystem asked for: %s", summary)
	}
	// The failure must not read as a missing dashboard, because the dashboard is
	// installed and reinstalling it would not help.
	if strings.Contains(summary, "dashboard is not installed") {
		t.Errorf("an unknown subsystem was reported as a missing dashboard: %s", summary)
	}
}

// TestDiagnoseServiceFiltersByStatus covers the follow-up call after "three things
// are degraded".
func TestDiagnoseServiceFiltersByStatus(t *testing.T) {
	fx := startFixture(t)

	args := creds(fx.url)
	args["status"] = "off"

	got, summary, isErr := callLiveTool(t, "breeze_diagnose_service", args)
	if isErr {
		t.Fatalf("breeze_diagnose_service reported an error: %s", summary)
	}

	subsystems, _ := got["subsystems"].([]any)
	for _, raw := range subsystems {
		entry, _ := raw.(map[string]any)
		if entry["status"] != "off" {
			t.Errorf("a %v subsystem survived the off filter: %v", entry["status"], entry["subsystem"])
		}
	}

	// The counts are deliberately not filtered — they describe the whole service,
	// and a filtered document that also filtered its counts would give a caller no
	// way to know what it was not shown.
	if off := numberField(t, got, "off"); int(off) != len(subsystems) {
		t.Errorf("off = %v but %d subsystem(s) returned; the filter should match the count",
			off, len(subsystems))
	}
	if numberField(t, got, "total") <= numberField(t, got, "off") {
		t.Error("total was filtered along with the list; it should describe the whole service")
	}
	if notes, _ := got["notes"].([]any); len(notes) == 0 {
		t.Error("a filtered document carries no note saying it was filtered")
	}
}

// TestDiagnoseServiceRejectsMissingCredentials keeps this tool's failure reporting
// aligned with the other live tools: unauthorized is not the same as missing.
func TestDiagnoseServiceRejectsMissingCredentials(t *testing.T) {
	fx := startFixture(t)

	_, summary, isErr := callLiveTool(t, "breeze_diagnose_service", map[string]any{
		"service_url": fx.url,
	})
	if !isErr {
		t.Fatalf("an unauthenticated call succeeded: %s", summary)
	}
	if strings.Contains(summary, "not installed") {
		t.Errorf("an auth failure was reported as a missing feature: %s", summary)
	}
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// numberField reads a JSON number field, which decodes as float64.
func numberField(t *testing.T, m map[string]any, key string) float64 {
	t.Helper()
	v, ok := m[key].(float64)
	if !ok {
		t.Fatalf("%s is %T (%v), want a number", key, m[key], m[key])
	}
	return v
}

// findSubsystem returns one report from the list, failing if it is absent.
func findSubsystem(t *testing.T, subsystems []any, name string) map[string]any {
	t.Helper()
	for _, raw := range subsystems {
		entry, ok := raw.(map[string]any)
		if ok && entry["subsystem"] == name {
			return entry
		}
	}
	t.Fatalf("no %q subsystem in the report", name)
	return nil
}

// statusName reverses the rank map, for an ordering failure message.
func statusName(rank map[string]int, want int) string {
	for name, r := range rank {
		if r == want {
			return name
		}
	}
	return "unknown"
}
