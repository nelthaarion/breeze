package mcp_test

// Tests for breeze_get_queries, over the dashboard's real collector and its real HTTP endpoint.

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nelthaarion/breeze/v2/dashboard"
)

// liveQueryArg is a bound value the monitor holds and must never leave the process.
const liveQueryArg = "arg-card-4242-must-not-leave"

// recordLiveQueries records what an application's data layer would push: a fast query, a slow one and
// one that failed. The arguments are held by the monitor, as they are in a real one.
func recordLiveQueries(coll *dashboard.Collector) {
	now := time.Now()
	coll.RecordQuery(dashboard.QueryRecord{ID: "lq1", Time: now.Add(-3 * time.Second), SQL: "SELECT * FROM orders WHERE id = $1",
		Args: []any{liveQueryArg}, Duration: 850, DurationMS: 0.85, Rows: 1, File: "/srv/app/orders.go", Line: 42})
	coll.RecordQuery(dashboard.QueryRecord{ID: "lq2", Time: now.Add(-2 * time.Second), SQL: "SELECT * FROM orders",
		Duration: 250000, DurationMS: 250, Rows: 5000, File: "/srv/app/orders.go", Line: 57})
	coll.RecordQuery(dashboard.QueryRecord{ID: "lq3", Time: now.Add(-1 * time.Second), SQL: "INSERT INTO orders VALUES ($1)",
		Args: []any{liveQueryArg}, Duration: 1200, DurationMS: 1.2, File: "/srv/app/orders.go", Line: 63, Error: "duplicate key"})
}

func queriesOf(t *testing.T, args map[string]any) map[string]any {
	t.Helper()
	got, summary, isErr := callLiveToolOn(t, embeddedServer(), "breeze_get_queries", args)
	if isErr {
		t.Fatalf("breeze_get_queries %v: %s", args, summary)
	}
	return got
}

func sqlOf(t *testing.T, got map[string]any) []string {
	t.Helper()
	var out []string
	for _, row := range asRows(t, got["queries"]) {
		out = append(out, row["sql"].(string))
	}
	return out
}

// TestQueriesToolListsTheSlowestFirstAndCountsTheWholeWindow: the list is what to look at first, and the
// counts are those of the window, so a filter does not change what "3 of them were slow" means.
func TestQueriesToolListsTheSlowestFirstAndCountsTheWholeWindow(t *testing.T) {
	startLocalFixture(t)

	got := queriesOf(t, nil)
	if got["queries_scanned"] != float64(3) || got["slow"] != float64(1) || got["failed"] != float64(1) || got["matched"] != float64(3) {
		t.Fatalf("scanned/slow/failed/matched = %v/%v/%v/%v, want 3/1/1/3", got["queries_scanned"], got["slow"], got["failed"], got["matched"])
	}
	want := []string{"SELECT * FROM orders", "INSERT INTO orders VALUES ($1)", "SELECT * FROM orders WHERE id = $1"}
	if g := sqlOf(t, got); strings.Join(g, "|") != strings.Join(want, "|") {
		t.Fatalf("the order is %q, want the slowest first: %q", g, want)
	}
	first := asRows(t, got["queries"])[0]
	if first["duration_ms"] != float64(250) || first["rows"] != float64(5000) || first["slow"] != true || first["where"] != "/srv/app/orders.go:57" {
		t.Errorf("the slowest query is %v", first)
	}

	// A filter narrows the list and leaves the window's counts alone.
	for name, tc := range map[string]struct {
		args map[string]any
		want []string
	}{
		"slow only":         {map[string]any{"slow": true}, []string{"SELECT * FROM orders"}},
		"failed only":       {map[string]any{"failed": true}, []string{"INSERT INTO orders VALUES ($1)"}},
		"by text":           {map[string]any{"query": "INSERT"}, []string{"INSERT INTO orders VALUES ($1)"}},
		"by lowercase text": {map[string]any{"query": "where id"}, []string{"SELECT * FROM orders WHERE id = $1"}},
	} {
		got := queriesOf(t, tc.args)
		if g := sqlOf(t, got); strings.Join(g, "|") != strings.Join(tc.want, "|") {
			t.Errorf("%s: listed %q, want %q", name, g, tc.want)
		}
		if got["queries_scanned"] != float64(3) || got["slow"] != float64(1) || got["failed"] != float64(1) {
			t.Errorf("%s: the window's counts changed to %v/%v/%v", name, got["queries_scanned"], got["slow"], got["failed"])
		}
	}

	// limit is the window read, before any filter: the most recent one is the failed insert.
	one := queriesOf(t, map[string]any{"limit": 1})
	if g := sqlOf(t, one); len(g) != 1 || g[0] != "INSERT INTO orders VALUES ($1)" || one["queries_scanned"] != float64(1) {
		t.Errorf("limit 1 read %q (scanned %v), want only the most recent", g, one["queries_scanned"])
	}

	none := queriesOf(t, map[string]any{"query": "no such table"})
	if none["matched"] != float64(0) || !strings.Contains(notesOf(none), "No query in the window matches") {
		t.Errorf("a filter that matches nothing: matched=%v notes=%s", none["matched"], notesOf(none))
	}
}

// TestQueriesToolNeverCarriesArguments: the monitor holds a query's bound values and its endpoint leaves
// them out. They are application data, and everything the tool returns goes into a model's context.
func TestQueriesToolNeverCarriesArguments(t *testing.T) {
	f := startLocalFixture(t)

	// The premise: the monitor does hold the argument, so what keeps it out is the endpoint and the tool
	// and not its absence.
	held := false
	for _, q := range f.coll.Queries(0) {
		for _, a := range q.Args {
			held = held || a == liveQueryArg
		}
	}
	if !held {
		t.Fatal("the fixture's monitor does not hold the argument, so this test proves nothing")
	}

	viaHTTP, summary, isErr := callLiveTool(t, "breeze_get_queries", map[string]any{"service_url": f.url})
	if isErr {
		t.Fatalf("over HTTP: %s", summary)
	}
	for name, got := range map[string]map[string]any{"in-process": queriesOf(t, nil), "over HTTP": viaHTTP} {
		if text, _ := jsonText(got); strings.Contains(text, liveQueryArg) {
			t.Errorf("%s: a bound value left the process through the tool: %.300s", name, text)
		}
	}
}

// TestQueriesToolCutsALongStatementAtACharacter: a bulk insert can be megabytes, and the cut must not
// leave half a character behind.
func TestQueriesToolCutsALongStatementAtACharacter(t *testing.T) {
	coll, _ := freshDashboard(t)
	// A one-byte prefix of odd length puts the 400th byte in the middle of a two-byte character.
	long := "SELECT 'x" + strings.Repeat("é", 600) + "' AS tail_marker"
	coll.RecordQuery(dashboard.QueryRecord{ID: "long", Time: time.Now(), SQL: long, Duration: 10, DurationMS: 0.01})

	got := queriesOf(t, nil)
	rows := asRows(t, got["queries"])
	if len(rows) != 1 {
		t.Fatalf("listed %d queries, want the one", len(rows))
	}
	sql := rows[0]["sql"].(string)
	if rows[0]["sql_truncated"] != true || len(sql) > 400+len("…") || !utf8.ValidString(sql) || strings.Contains(sql, "tail_marker") {
		t.Errorf("the long statement was carried as %d bytes (truncated=%v, valid=%v): %.80q", len(sql), rows[0]["sql_truncated"], utf8.ValidString(sql), sql)
	}
	if !utf8.ValidString(sql) {
		t.Errorf("the cut statement is not valid UTF-8")
	}
}

// TestQueriesToolSaysWhyTheMonitorIsEmpty: nothing in the framework pushes a query, so an empty monitor
// is the ordinary state of a generated service, and it must not read as "no query ran".
func TestQueriesToolSaysWhyTheMonitorIsEmpty(t *testing.T) {
	freshDashboard(t)

	got := queriesOf(t, nil)
	if got["queries_scanned"] != float64(0) {
		t.Fatalf("scanned %v queries on an empty dashboard", got["queries_scanned"])
	}
	notes := notesOf(got)
	for _, want := range []string{"PushQuery", "not evidence that no query ran", "`breeze add` generates does not"} {
		if !strings.Contains(notes, want) {
			t.Errorf("the notes do not say %q: %s", want, notes)
		}
	}
}
