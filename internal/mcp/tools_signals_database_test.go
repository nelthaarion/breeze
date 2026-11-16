package mcp_test

// The dashboard has a query monitor that breeze_get_queries reads. These pin what made it necessary: a
// query pushed to the dashboard does not reach breeze_get_signals as a query, and the notes that say so
// have to stay true. If the dashboard starts recording queries as database signals, these fail, and the
// description of both tools wants rewriting with them.

import (
	"errors"
	"strings"
	"testing"

	"github.com/nelthaarion/breeze/v2/events"
)

func TestQueriesAreNotSignalsAndTheSignalsToolSaysWhereTheyAre(t *testing.T) {
	coll, _ := freshDashboard(t)
	detach := coll.AttachEvents(events.New())
	t.Cleanup(detach)

	coll.PushQuery("SELECT * FROM orders WHERE id = $1", []any{liveQueryArg}, 850, 1, "orders.go", 42, nil)
	coll.PushQuery("INSERT INTO orders VALUES ($1)", []any{liveQueryArg}, 1200, 0, "orders.go", 63, errors.New("duplicate key"))

	// The monitor has both statements, the failure among them.
	monitor := queriesOf(t, nil)
	if monitor["queries_scanned"] != float64(2) || monitor["failed"] != float64(1) {
		t.Fatalf("the monitor holds %v queries with %v failed, want the two pushed and one failed", monitor["queries_scanned"], monitor["failed"])
	}

	// The signals do not: the source database is empty, and says why.
	byDatabase, summary, isErr := callLiveToolOn(t, embeddedServer(), "breeze_get_signals", map[string]any{"source": "database", "failed": false})
	if isErr {
		t.Fatalf("source=database: %s", summary)
	}
	if byDatabase["count"] != float64(0) {
		t.Errorf("source=database holds %v signals: queries are recorded there now, and the tools' descriptions are out of date", byDatabase["count"])
	}
	if notes := notesOf(byDatabase); !strings.Contains(notes, "breeze_get_queries") {
		t.Errorf("the empty database source does not say where queries are read: %s", notes)
	}

	// What a query becomes in the collector is an event with no statement, and the failed one is not
	// marked failed.
	all, _, _ := callLiveToolOn(t, embeddedServer(), "breeze_get_signals", map[string]any{"failed": false})
	rows := asRows(t, all["recent"])
	if len(rows) != 2 {
		t.Fatalf("the collector recorded %d signals for two queries", len(rows))
	}
	for _, row := range rows {
		if row["name"] != "database.query" || row["source"] != "events" || row["failed"] == true || row["query"] != nil {
			t.Errorf("a pushed query became %v, want an event named database.query with no query detail and not failed", row)
		}
	}
	if text, _ := jsonText(all); strings.Contains(text, "INSERT INTO orders") || strings.Contains(text, liveQueryArg) {
		t.Errorf("a statement or an argument reached the signals: %.300s", text)
	}
}

// TestSignalsSaysWhenASourceHasNoProducer: of the ten sources the collector can name, seven have no
// producer in the framework, and an empty answer from one of them is not the answer it looks like.
func TestSignalsSaysWhenASourceHasNoProducer(t *testing.T) {
	coll, _ := freshDashboard(t)
	detach := coll.AttachEvents(events.New())
	t.Cleanup(detach)

	for _, source := range []string{"router", "cache", "scheduler", "websocket", "oauth2", "plugin"} {
		got, summary, isErr := callLiveToolOn(t, embeddedServer(), "breeze_get_signals", map[string]any{"source": source, "failed": false})
		if isErr {
			t.Fatalf("source=%s: %s", source, summary)
		}
		if notes := notesOf(got); !strings.Contains(notes, "Nothing in the framework records into the "+source+" source") {
			t.Errorf("source=%s: the notes do not say that nothing records there: %s", source, notes)
		}
	}
	for _, source := range []string{"events", "workflow", "http"} {
		got, _, _ := callLiveToolOn(t, embeddedServer(), "breeze_get_signals", map[string]any{"source": source, "failed": false})
		if strings.Contains(notesOf(got), "Nothing in the framework records") {
			t.Errorf("source=%s has a producer, and the notes say it has none: %s", source, notesOf(got))
		}
	}
}
