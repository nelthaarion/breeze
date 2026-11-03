package dashboard

// The dashboard has five log tabs: app, http, error, panic and warning. The rest of
// the repository does not speak only those five. Fleet's tracer reports at "warn",
// and the fleet example logs at "info". A switch that matched the five names exactly
// threw every other line away without a sound, which meant the tracer's own account
// of why spans were not being exported — the line an agent debugging a missing trace
// is looking for — never reached the dashboard it was wired into.
//
// These tests use the level names the framework really passes, taken from its call
// sites, not names chosen to suit the code.

import "testing"

func messages(entries []LogEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Message
	}
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPushLogKeepsEveryLevelTheFrameworkPasses(t *testing.T) {
	c := newCollector(Config{Enabled: true}, nil)

	// One line per call site that exists in the repository, plus the documented tabs.
	c.PushLog("info", "gateway received order 123", "app")                                     // fleet example
	c.PushLog("warn", "fleet span export failed, retrying in 2s: connection refused", "fleet") // fleet/tracer.go
	c.PushLog("app", "plain app line", "app")
	c.PushLog("http", "GET / 200", "http")
	c.PushLog("error", "auth-service could not reach orders-service", "app")
	c.PushLog("panic", "handler panicked", "recovery")
	c.PushLog("warning", "slow query", "db")

	want := map[string][]string{
		"app":     {"gateway received order 123", "plain app line"},
		"http":    {"GET / 200"},
		"error":   {"auth-service could not reach orders-service"},
		"panic":   {"handler panicked"},
		"warning": {"fleet span export failed, retrying in 2s: connection refused", "slow query"},
	}
	for tab, lines := range want {
		if got := messages(c.Logs(tab, 100)); !sameStrings(got, lines) {
			t.Errorf("tab %q holds %q, want %q", tab, got, lines)
		}
	}
}

// TestPushLogNeverDiscardsALine is the property behind the one above. A level nobody
// anticipated is filed under app rather than nowhere: the caller took the trouble to
// send the line, and a missing line cannot be told apart from a line that was never sent.
func TestPushLogNeverDiscardsALine(t *testing.T) {
	c := newCollector(Config{Enabled: true}, nil)

	for _, level := range []string{"debug", "notice", "fatal", "", "  INFO ", "something-new"} {
		before := len(c.Logs("app", 100))
		c.PushLog(level, "line at "+level, "test")
		if after := len(c.Logs("app", 100)); after != before+1 {
			t.Errorf("a line at level %q was not kept (app tab went from %d to %d lines)", level, before, after)
		}
	}
}

// TestStoredEntriesCarryTheTabTheyAreOn keeps LogEntry.Level to what types.go
// documents it as (app / http / error / panic / warning), so a consumer that switches
// on it is not handed "warn" or "info" that no tab is called.
func TestStoredEntriesCarryTheTabTheyAreOn(t *testing.T) {
	c := newCollector(Config{Enabled: true}, nil)

	c.PushLog("warn", "w", "fleet")
	c.PushLog("info", "i", "app")

	if got := c.Logs("warning", 10); len(got) != 1 || got[0].Level != "warning" {
		t.Errorf("a line sent at warn is stored with level %+v, want warning", got)
	}
	if got := c.Logs("app", 10); len(got) != 1 || got[0].Level != "app" {
		t.Errorf("a line sent at info is stored with level %+v, want app", got)
	}
}

// TestLogsReadsByTheNamesThatWritingAccepts: a caller that logged at "warn" will ask
// for "warn". It must find the line, and an unknown name must not look like an empty
// tab, because an empty tab says nothing was logged.
func TestLogsReadsByTheNamesThatWritingAccepts(t *testing.T) {
	c := newCollector(Config{Enabled: true}, nil)
	c.PushLog("warn", "w", "fleet")
	c.PushLog("info", "i", "app")

	if got := messages(c.Logs("warn", 10)); !sameStrings(got, []string{"w"}) {
		t.Errorf("reading warn returned %q, want the warning tab", got)
	}
	if got := messages(c.Logs("info", 10)); !sameStrings(got, []string{"i"}) {
		t.Errorf("reading info returned %q, want the app tab", got)
	}
	if got := c.Logs("all", 10); got != nil {
		t.Errorf("reading an unknown level returned %q instead of nil, so it looks like an empty tab", messages(got))
	}
}
