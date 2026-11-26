package mcp

// tools_queries.go — the SQL a service ran, which of it was slow, and which of it failed.
//
// The dashboard keeps a monitor of the statements an application pushed to it: the text, how long it
// took, how many rows it touched, where in the source it was issued and whether it failed. That is the
// only place a service keeps a query at all. breeze_get_signals does not: a query pushed to the
// dashboard reaches the observability collector as an event named database.query, with the source
// "events", no statement and no duration, and a statement that failed is not marked failed there. The
// richer record is pushed to a connected browser and stored nowhere else. So a service whose requests
// are slow because of one statement can be answered here and nowhere else.
//
// # What the monitor holds, and does not
//
// Only what the application pushed. The framework records no query by itself; the application's data
// layer has to call Collector.PushQuery from its SQL execution path, and the code `breeze add` generates
// does not. An empty monitor says the data layer is not wired to it, which is a finding, and not that no
// query ran. It is a ring buffer, so what is here is a window, not a history.
//
// # What is not carried
//
// The monitor holds each query's arguments and its endpoint leaves them out, because a bound value is
// application data: a card number, an email address, a token. They are not declared here either. The
// statement is carried, and so is any value an application wrote into it instead of binding it, which a
// note says; a statement is also cut at queryTextLimit, because a bulk insert can run to megabytes and
// everything this tool returns goes into a model's context.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/nelthaarion/go-json-plus"
)

const (
	// queriesDefaultScan is how many of the most recent queries are read, the dashboard's own default.
	queriesDefaultScan = 200
	// queriesMaxScan bounds that window.
	queriesMaxScan = 1000
	// queriesMaxRows bounds how many matching queries are returned, slowest first.
	queriesMaxRows = 50
	// queryTextLimit is the longest statement carried, in bytes.
	queryTextLimit = 400
)

// liveQuery mirrors the dashboard's queryEvent JSON. Its arguments are not in it: the endpoint does not
// serve them, and this type does not ask.
type liveQuery struct {
	ID         string `json:"id"`
	Time       string `json:"time"`
	SQL        string `json:"sql"`
	DurationUS int64  `json:"duration_us"`
	Rows       int64  `json:"rows"`
	File       string `json:"file"`
	Line       int    `json:"line"`
	Slow       bool   `json:"slow"`
	Error      string `json:"error,omitempty"`
}

// queryRow is one query of the answer.
type queryRow struct {
	ID           string  `json:"id"`
	Time         string  `json:"time"`
	SQL          string  `json:"sql"`
	SQLTruncated bool    `json:"sql_truncated,omitempty"`
	DurationMS   float64 `json:"duration_ms"`
	Rows         int64   `json:"rows"`
	// Where is the source file and line the statement was issued from, as the application reported it.
	Where string `json:"where,omitempty"`
	Slow  bool   `json:"slow"`
	Error string `json:"error,omitempty"`
}

type queriesReport struct {
	ServiceURL string `json:"service_url"`
	// Scanned is how many queries the monitor returned for the window asked, before any filter here. Slow
	// and Failed count those, so they stay the denominators of the whole window and not of the matches.
	Scanned int `json:"queries_scanned"`
	Slow    int `json:"slow"`
	Failed  int `json:"failed"`
	// Matched is how many of them passed the filters, and Count how many are listed: at most
	// queriesMaxRows, slowest first.
	Matched int        `json:"matched"`
	Count   int        `json:"count"`
	Queries []queryRow `json:"queries"`
	Notes   []string   `json:"notes,omitempty"`
}

type queriesArgs struct {
	liveArgs
	Query  string `json:"query"`
	Slow   bool   `json:"slow"`
	Failed bool   `json:"failed"`
	Limit  int    `json:"limit"`
}

func getQueriesTool() *tool {
	return &tool{
		name: "breeze_get_queries",
		description: "Read the SQL statements a running service reported to its dashboard's query monitor: the text, " +
			"duration, rows, the source file and line, and whether each was slow (over the dashboard's slow_query_ms) or " +
			"failed, slowest first, with how many of the window were slow and how many failed. This is the only place " +
			"queries are kept: breeze_get_signals does not carry them, and does not mark a failed statement failed. " +
			"The monitor holds only what the application's data layer pushed with Collector.PushQuery, which the " +
			"framework and the generated code do not do, so an empty answer says the data layer is not wired to the " +
			"dashboard and not that no query ran. Arguments are never served; a statement is cut at 400 bytes. " +
			"Requires the dashboard feature.",
		schema: objectSchema(liveProps(map[string]any{
			"query":  stringProp("Keep only statements containing this text, case-insensitively."),
			"slow":   boolProp("Keep only queries the dashboard flagged slow."),
			"failed": boolProp("Keep only queries that returned an error."),
			"limit": map[string]any{
				"type": "integer", "minimum": 1, "maximum": queriesMaxScan,
				"description": fmt.Sprintf("How many of the most recent queries to read, before filtering. Defaults to %d.", queriesDefaultScan),
			},
		}), "service_url"),
		run: func(raw json.RawMessage) toolCallResult {
			var a queriesArgs
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}
			return getQueries(a)
		},
	}
}

func getQueries(a queriesArgs) toolCallResult {
	limit := a.Limit
	if limit <= 0 {
		limit = queriesDefaultScan
	}
	if limit > queriesMaxScan {
		limit = queriesMaxScan
	}

	req := a.request("/queries", "dashboard")
	req.path += fmt.Sprintf("?limit=%d", limit)
	var held []liveQuery
	if err := fetchLiveJSON(req, &held); err != nil {
		return liveResultError("reading queries", err)
	}

	report := queriesReport{ServiceURL: a.ServiceURL, Scanned: len(held), Queries: []queryRow{}}
	needle := strings.ToLower(strings.TrimSpace(a.Query))
	var matched []liveQuery
	for _, q := range held {
		if q.Slow {
			report.Slow++
		}
		failed := q.Error != ""
		if failed {
			report.Failed++
		}
		if (a.Slow && !q.Slow) || (a.Failed && !failed) {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(q.SQL), needle) {
			continue
		}
		matched = append(matched, q)
	}
	report.Matched = len(matched)

	// Slowest first; of two equally slow, the later.
	sort.SliceStable(matched, func(i, j int) bool {
		if matched[i].DurationUS != matched[j].DurationUS {
			return matched[i].DurationUS > matched[j].DurationUS
		}
		return matched[i].Time > matched[j].Time
	})
	if len(matched) > queriesMaxRows {
		matched = matched[:queriesMaxRows]
	}
	for _, q := range matched {
		report.Queries = append(report.Queries, shapeQuery(q))
	}
	report.Count = len(report.Queries)

	switch {
	case report.Scanned == 0:
		report.Notes = append(report.Notes, "The dashboard's query monitor is empty. It holds only the statements the application's "+
			"data layer pushed with Collector.PushQuery(sql, args, durationUS, rows, file, line, err) from its SQL execution path; "+
			"the framework does not record a query itself and the code `breeze add` generates does not push one. Either that call "+
			"is not wired, or the dashboard's Queries setting is off. This is not evidence that no query ran, and a slow statement "+
			"is not visible here until the data layer reports it.")
	default:
		report.Notes = append(report.Notes, fmt.Sprintf("The monitor is a ring buffer of the most recent statements the application reported; "+
			"%d were read, which is a window and not the history. A statement is 'slow' when the dashboard flagged it against its "+
			"slow_query_ms.", report.Scanned))
		if report.Matched > report.Count {
			report.Notes = append(report.Notes, fmt.Sprintf("%d queries matched and the %d slowest are listed.", report.Matched, report.Count))
		}
		if report.Matched == 0 {
			report.Notes = append(report.Notes, "No query in the window matches the filters.")
		}
	}
	report.Notes = append(report.Notes, "A query's arguments are held by the monitor and are neither served by the dashboard nor carried here. "+
		"The statement is carried as the application wrote it: a value written into it instead of bound is in it.")

	summary := fmt.Sprintf("%d of %d recent quer(ies) listed; %d slow, %d failed in the window", report.Count, report.Scanned, report.Slow, report.Failed)
	return structuredResult(summary, report)
}

func shapeQuery(q liveQuery) queryRow {
	row := queryRow{
		ID: q.ID, Time: q.Time, SQL: q.SQL, DurationMS: msOf(q.DurationUS),
		Rows: q.Rows, Slow: q.Slow, Error: q.Error,
	}
	if len(row.SQL) > queryTextLimit {
		row.SQL = strings.ToValidUTF8(row.SQL[:queryTextLimit], "") + "…"
		row.SQLTruncated = true
	}
	if q.File != "" {
		row.Where = q.File
		if q.Line > 0 {
			row.Where = fmt.Sprintf("%s:%d", q.File, q.Line)
		}
	}
	return row
}
