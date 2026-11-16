package mcp_test

// Tests for breeze_get_timeline.
//
// The timelines are made by the dashboard's own TimelineRecorder, the way application code makes
// them, so the tool is held to the shape the recorder really returns: every step in one flat list
// and each nested step also inside its parent. A hand-written fixture would have agreed with
// whatever the tool assumed.

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"

	"github.com/nelthaarion/breeze/v2"
	"github.com/nelthaarion/breeze/v2/dashboard"
)

const (
	liveTimelineID      = "tl-embedded-live-new"
	liveTimelineOlderID = "tl-embedded-live-old"
	// liveTimelineSecret is a step's metadata the tool must not carry: the dashboard serves it to its
	// own UI, and a SQL statement is exactly the sort of thing code puts in a step.
	liveTimelineSecret = "SELECT pan FROM cards WHERE holder = 'must-not-leave-4242'"
)

// recordLiveTimeline records one request as application code would: authentication, then an ORM
// query that scanned rows, inside the step the middleware itself opens. The scan takes the longest by
// a margin no scheduler jitter closes, so the step that took the time is not in doubt.
func recordLiveTimeline(coll *dashboard.Collector, cfg dashboard.Config, id, path string) {
	rec := dashboard.NewTimelineRecorder(cfg)
	start := rec.Start()
	endRoot := rec.Step("Request Received")
	endAuth := rec.Step("Authentication")
	time.Sleep(time.Millisecond)
	endAuth(map[string]any{"scheme": "bearer"})
	endQuery := rec.Step("ORM Query")
	endScan := rec.Step("Row scan")
	time.Sleep(40 * time.Millisecond)
	endScan(map[string]any{"rows": 42, "cached": false, "sql": liveTimelineSecret})
	endQuery(map[string]any{"table": "orders"})
	endRoot(map[string]any{"method": "GET", "path": path, "ip": "10.1.2.3"})
	coll.RecordTimeline(dashboard.Timeline{
		ID: id, Time: start, Method: "GET", Path: path, Status: 200,
		Total: time.Since(start).Microseconds(), Steps: rec.Build(),
	})
}

// freshDashboard is a dashboard with nothing recorded, made the current one for this test.
func freshDashboard(t *testing.T) (*dashboard.Collector, dashboard.Config) {
	t.Helper()
	f := startLocalFixture(t)
	t.Cleanup(f.coll.MakeCurrent)

	cfg := dashboard.DefaultConfig()
	cfg.DisableAuth = true
	coll := dashboard.Install(nil, breeze.NewRouter(), cfg)
	coll.MakeCurrent()
	return coll, cfg
}

func timelineSteps(t *testing.T, v any) []map[string]any {
	t.Helper()
	return asRows(t, v)
}

// walkSteps visits every step of a tree.
func walkSteps(t *testing.T, steps []map[string]any, visit func(step map[string]any)) {
	t.Helper()
	for _, s := range steps {
		visit(s)
		if kids, ok := s["children"]; ok {
			walkSteps(t, timelineSteps(t, kids), visit)
		}
	}
}

// TestTimelineToolCountsEachStepOnce: the recorder lists a nested step twice, as a child and as an
// entry of the flat list. The tool's answer has to be the tree, with the time of each step counted
// once, or the parent looks twice as busy as it was.
func TestTimelineToolCountsEachStepOnce(t *testing.T) {
	f := startLocalFixture(t)

	// The premise: the dashboard's own endpoint really does hand out the flat list with four
	// entries, so the dedup below is doing something.
	raw, err := httpGetBody(f.url + "/dashboard/api/timeline/" + liveTimelineID)
	if err != nil {
		t.Fatalf("reading the timeline endpoint: %v", err)
	}
	var wire struct {
		Steps []map[string]any `json:"steps"`
	}
	if err := json.Unmarshal([]byte(raw), &wire); err != nil || len(wire.Steps) != 4 {
		t.Fatalf("the dashboard's flat list holds %d entries (%v), want the four a naive reader would count: %.300s", len(wire.Steps), err, raw)
	}

	got, summary, isErr := callLiveToolOn(t, embeddedServer(), "breeze_get_timeline", map[string]any{"id": liveTimelineID})
	if isErr {
		t.Fatalf("breeze_get_timeline: %s", summary)
	}

	if n, _ := got["step_count"].(float64); n != 4 {
		t.Errorf("step_count = %v, want 4: the root, authentication, the query and its scan", got["step_count"])
	}
	top := timelineSteps(t, got["steps"])
	if len(top) != 1 || top[0]["name"] != "Request Received" {
		t.Fatalf("the top level is %v, want the one step the framework records", top)
	}
	kids := timelineSteps(t, top[0]["children"])
	if len(kids) != 2 || kids[0]["name"] != "Authentication" || kids[1]["name"] != "ORM Query" {
		t.Fatalf("the root's children are %v, want Authentication then ORM Query", kids)
	}
	if grand := timelineSteps(t, kids[1]["children"]); len(grand) != 1 || grand[0]["name"] != "Row scan" {
		t.Errorf("the query's children are %v, want its Row scan", grand)
	}

	// Each step's own time, added up over the tree, is the root's duration: nothing counted twice
	// and nothing lost.
	var selfSum float64
	walkSteps(t, top, func(s map[string]any) { selfSum += s["self_ms"].(float64) })
	rootMS := top[0]["duration_ms"].(float64)
	if math.Abs(selfSum-rootMS) > 0.05 {
		t.Errorf("the steps' own times add up to %.3fms, but the root took %.3fms", selfSum, rootMS)
	}

	slowest, _ := got["slowest_step"].(map[string]any)
	if slowest["step"] != "Request Received > ORM Query > Row scan" {
		t.Errorf("slowest_step = %v, want the scan, where the 40ms went", slowest)
	}
	if share, _ := slowest["share_percent"].(float64); share < 70 {
		t.Errorf("the scan's share is %v%%, want most of the request", slowest["share_percent"])
	}
	if !strings.Contains(summary, "Request Received > ORM Query > Row scan") {
		t.Errorf("the summary does not say where the time went: %q", summary)
	}
}

// TestTimelineToolCarriesMeasurementsAndNotWhatCodeWroteIntoAStep: a step's metadata is whatever the
// code that recorded it put there. A number or a boolean is a measurement and is carried; anything
// else is named and left out.
func TestTimelineToolCarriesMeasurementsAndNotWhatCodeWroteIntoAStep(t *testing.T) {
	f := startLocalFixture(t)

	// The premise: the dashboard does hold the statement and the address, or this proves nothing.
	raw, err := httpGetBody(f.url + "/dashboard/api/timeline/" + liveTimelineID)
	if err != nil || !strings.Contains(raw, liveTimelineSecret) || !strings.Contains(raw, "10.1.2.3") {
		t.Fatalf("the fixture's dashboard does not hold the step metadata (%v), so this test proves nothing: %.300s", err, raw)
	}

	for _, srv := range map[string]func() map[string]any{
		"in-process": func() map[string]any {
			got, summary, isErr := callLiveToolOn(t, embeddedServer(), "breeze_get_timeline", map[string]any{"id": liveTimelineID})
			if isErr {
				t.Fatalf("in-process: %s", summary)
			}
			return got
		},
		"over HTTP": func() map[string]any {
			got, summary, isErr := callLiveTool(t, "breeze_get_timeline", map[string]any{"service_url": f.url, "id": liveTimelineID})
			if isErr {
				t.Fatalf("over HTTP: %s", summary)
			}
			return got
		},
	} {
		got := srv()
		text, _ := jsonText(got)
		for _, secret := range []string{liveTimelineSecret, "10.1.2.3", "must-not-leave-4242"} {
			if strings.Contains(text, secret) {
				t.Errorf("a step's metadata left the process through the tool: %.300s", text)
			}
		}

		byName := map[string]map[string]any{}
		walkSteps(t, timelineSteps(t, got["steps"]), func(s map[string]any) { byName[s["name"].(string)] = s })
		scan := byName["Row scan"]
		meta, _ := scan["metadata"].(map[string]any)
		if meta["rows"] != float64(42) || meta["cached"] != false {
			t.Errorf("the scan's measurements are %v, want rows 42 and cached false", meta)
		}
		if _, leaked := meta["sql"]; leaked {
			t.Errorf("the statement is in the scan's metadata: %v", meta)
		}
		if omitted, _ := scan["metadata_omitted"].([]any); len(omitted) != 1 || omitted[0] != "sql" {
			t.Errorf("metadata_omitted = %v, want the one key left out (sql)", scan["metadata_omitted"])
		}
		root := byName["Request Received"]
		if omitted, _ := root["metadata_omitted"].([]any); len(omitted) != 3 {
			t.Errorf("the root's omitted keys = %v, want ip, method and path", root["metadata_omitted"])
		}
	}
}

// TestTimelineToolListsNewestFirstWithinTheLimit.
func TestTimelineToolListsNewestFirstWithinTheLimit(t *testing.T) {
	startLocalFixture(t)

	got, summary, isErr := callLiveToolOn(t, embeddedServer(), "breeze_get_timeline", map[string]any{})
	if isErr {
		t.Fatalf("list: %s", summary)
	}
	rows := asRows(t, got["timelines"])
	if len(rows) != 2 || rows[0]["id"] != liveTimelineID || rows[1]["id"] != liveTimelineOlderID {
		t.Fatalf("timelines = %v, want the newest first: %s then %s", rows, liveTimelineID, liveTimelineOlderID)
	}
	if rows[0]["step_count"] != float64(4) {
		t.Errorf("step_count = %v, want 4 (a nested step is one step, not two)", rows[0]["step_count"])
	}
	slow, _ := rows[0]["slowest_step"].(map[string]any)
	if slow["step"] != "Request Received > ORM Query > Row scan" {
		t.Errorf("slowest_step = %v", slow)
	}

	limited, _, _ := callLiveToolOn(t, embeddedServer(), "breeze_get_timeline", map[string]any{"limit": 1})
	rows = asRows(t, limited["timelines"])
	if len(rows) != 1 || rows[0]["id"] != liveTimelineID {
		t.Errorf("limit 1 answered %v, want the newest only", rows)
	}
}

// TestTimelineToolSaysWhyThereIsNothingToShow: an empty list is the ordinary answer of a dashboard
// nobody was watching, and the caller cannot tell that from a service with nothing slow unless the
// answer says so.
func TestTimelineToolSaysWhyThereIsNothingToShow(t *testing.T) {
	freshDashboard(t)

	got, summary, isErr := callLiveToolOn(t, embeddedServer(), "breeze_get_timeline", map[string]any{})
	if isErr {
		t.Fatalf("list: %s", summary)
	}
	if got["count"] != float64(0) {
		t.Fatalf("count = %v, want an empty dashboard", got["count"])
	}
	notes := notesOf(got)
	for _, want := range []string{"browser", "not evidence that no request was slow", "breeze_get_routes"} {
		if !strings.Contains(notes, want) {
			t.Errorf("the notes do not say %q: %s", want, notes)
		}
	}

	// An id from a request record, for a request nobody was watching: not held, and the answer says
	// the id is the ordinary kind to find missing.
	_, summary, isErr = callLiveToolOn(t, embeddedServer(), "breeze_get_timeline", map[string]any{"id": "7f3a9c"})
	if !isErr {
		t.Fatalf("an id that is not held answered %q without an error", summary)
	}
	for _, want := range []string{"no timeline with id", "whether or not a timeline was recorded", "without an id"} {
		if !strings.Contains(summary, want) {
			t.Errorf("the refusal %q does not say %q", summary, want)
		}
	}
}

// TestTimelineToolDoesNotCallAMissingDashboardAnUnheldTimeline is the other half: a 404 for one id
// means the timeline is not held only if the dashboard is there to say so.
func TestTimelineToolDoesNotCallAMissingDashboardAnUnheldTimeline(t *testing.T) {
	f := startLocalFixture(t)

	// A dashboard that is there, an id that is not held.
	got, summary, isErr := callLiveTool(t, "breeze_get_timeline", map[string]any{"service_url": f.url, "id": "7f3a9c"})
	if !isErr || got["error"] != "missing" || !strings.Contains(summary, "no timeline with id") {
		t.Fatalf("an unheld id over HTTP: isErr=%v error=%v %q", isErr, got["error"], summary)
	}

	// No dashboard at that base path: the id says nothing.
	got, summary, isErr = callLiveTool(t, "breeze_get_timeline", map[string]any{
		"service_url": f.url, "id": liveTimelineID, "base_path": "/no-dashboard-here",
	})
	if !isErr || got["error"] != "missing" {
		t.Fatalf("a missing dashboard: isErr=%v error=%v %q", isErr, got["error"], summary)
	}
	if strings.Contains(summary, "no timeline with id") {
		t.Errorf("a dashboard that is not there was reported as a timeline that is not held: %q", summary)
	}
	if !strings.Contains(summary, "base path") && !strings.Contains(summary, "not installed") {
		t.Errorf("the message does not name the real causes: %q", summary)
	}
}

// TestTimelineToolSaysWhenAStepIsAllTheFrameworkRecorded: a timeline of the one step the framework
// records says how long the request took and nothing about where, and an agent has to be told that
// before it reads a breakdown into it. A step that never ended is named.
func TestTimelineToolSaysWhenAStepIsAllTheFrameworkRecorded(t *testing.T) {
	coll, cfg := freshDashboard(t)

	rec := dashboard.NewTimelineRecorder(cfg)
	start := rec.Start()
	rec.Step("Request Received")(map[string]any{"method": "GET"})
	coll.RecordTimeline(dashboard.Timeline{ID: "only-root", Time: start, Method: "GET", Path: "/a", Status: 200, Total: 5000, Steps: rec.Build()})

	// Every timeline held has the one step: the note says what the framework records and how to get more.
	list, _, _ := callLiveToolOn(t, embeddedServer(), "breeze_get_timeline", map[string]any{})
	if !strings.Contains(notesOf(list), "TimelineRecorder") {
		t.Errorf("a list of timelines that all have one step does not say where a finer one comes from: %s", notesOf(list))
	}

	leaky := dashboard.NewTimelineRecorder(cfg)
	leakyStart := leaky.Start()
	leaky.Step("Request Received")(nil)
	_ = leaky.Step("Leaked step") // its end is never called
	coll.RecordTimeline(dashboard.Timeline{ID: "leaky", Time: leakyStart, Method: "GET", Path: "/b", Status: 200, Total: 7000, Steps: leaky.Build()})

	// One of two: the count is said, and the all-one-step wording is not.
	list, _, _ = callLiveToolOn(t, embeddedServer(), "breeze_get_timeline", map[string]any{})
	if !strings.Contains(notesOf(list), "1 of the 2 timelines have only the one step") || strings.Contains(notesOf(list), "TimelineRecorder") {
		t.Errorf("a list in which one of two timelines has one step does not say so, or says what it need not: %s", notesOf(list))
	}

	one, summary, isErr := callLiveToolOn(t, embeddedServer(), "breeze_get_timeline", map[string]any{"id": "only-root"})
	if isErr {
		t.Fatalf("only-root: %s", summary)
	}
	if !strings.Contains(notesOf(one), "nothing about where the time went") {
		t.Errorf("a one-step timeline does not say it cannot say where the time went: %s", notesOf(one))
	}

	leaked, summary, isErr := callLiveToolOn(t, embeddedServer(), "breeze_get_timeline", map[string]any{"id": "leaky"})
	if isErr {
		t.Fatalf("leaky: %s", summary)
	}
	found := false
	walkSteps(t, timelineSteps(t, leaked["steps"]), func(s map[string]any) {
		if s["name"] == "Leaked step" && s["open"] == true {
			found = true
		}
	})
	if !found {
		t.Errorf("a step that never ended is not marked open: %v", leaked["steps"])
	}
	if !strings.Contains(notesOf(leaked), "never ended (Leaked step)") {
		t.Errorf("the notes do not name the step that never ended: %s", notesOf(leaked))
	}
}
