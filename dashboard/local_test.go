package dashboard

import (
	"errors"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/nelthaarion/go-json-plus"

	"github.com/nelthaarion/breeze/v2"
	"github.com/nelthaarion/breeze/v2/events"
	"github.com/nelthaarion/breeze/v2/internal/livesource"
)

// newReadableDashboard is a dashboard with something in every place the readers look.
func newReadableDashboard(t *testing.T) (*breeze.Router, *Collector) {
	t.Helper()

	router := breeze.NewRouter()
	cfg := DefaultConfig()
	cfg.DisableAuth = true
	coll := Install(nil, router, cfg)

	router.Handle(breeze.GET, "/api/orders", func(ctx *breeze.Context) error { return ctx.JSON(map[string]any{"ok": true}) })

	now := time.Now()
	coll.RecordLog("app", LogEntry{Time: now.Add(-2 * time.Second), Message: "reader test app line", Source: "orders", TraceID: "t-local-1"})
	coll.RecordLog("error", LogEntry{Time: now.Add(-1 * time.Second), Message: "reader test failure", Source: "orders", TraceID: "t-local-1"})
	coll.RecordRequest(RequestRecord{ID: "r1", Time: now, Method: "GET", Path: "/api/orders", Route: "/api/orders", Status: 500, DurationMS: 12, Error: "HTTP 500"})
	coll.RecordRequest(RequestRecord{ID: "r2", Time: now, Method: "POST", Path: "/api/orders", Route: "/api/orders", Status: 201, DurationMS: 3})

	bus := events.New()
	detach := coll.AttachEvents(bus)
	t.Cleanup(detach)
	type readerEvent struct{ N int }
	type readerFine struct{ N int }
	events.OnBus(bus, readerEvent{}, func(*events.Context, readerEvent) error { return errors.New("listener failed") })
	events.OnBus(bus, readerFine{}, func(*events.Context, readerFine) error { return nil })
	// One that fails and one that does not, so a reader that drops the failed=1 filter answers
	// differently from the endpoint.
	_ = events.EmitBus(bus, readerEvent{N: 1})
	_ = events.EmitBus(bus, readerFine{N: 2})

	coll.RegisterHealthCheck("db", func() (string, string) { return "red", "refused" })
	coll.RegisterConnection(Connection{ID: "db", Name: "DB", Type: ConnDatabase, Driver: "postgres", Status: StatusDisconnected})

	// Queries as an application's data layer pushes them: a fast one, a slow one, a failed one. Their
	// arguments are held by the monitor and must stay there.
	coll.RecordQuery(QueryRecord{ID: "q1", Time: now, SQL: "SELECT * FROM orders WHERE id = $1", Args: []any{"arg-secret"}, Duration: 850, DurationMS: 0.85, Rows: 1, File: "orders.go", Line: 42})
	coll.RecordQuery(QueryRecord{ID: "q2", Time: now, SQL: "SELECT * FROM orders", Duration: 250000, DurationMS: 250, Rows: 5000, File: "orders.go", Line: 57})
	coll.RecordQuery(QueryRecord{ID: "q3", Time: now, SQL: "INSERT INTO orders VALUES ($1)", Args: []any{"arg-secret"}, Duration: 1200, DurationMS: 1.2, File: "orders.go", Line: 63, Error: "duplicate key"})

	// Two timelines made by the real recorder, so the readers see the shape it really returns: every
	// step in one flat list, and each nested step also inside its parent.
	recordFixtureTimeline(coll, "tl-0", "/api/orders/older")
	recordFixtureTimeline(coll, "tl-1", "/api/orders")
	return router, coll
}

// timelineSecret is a value a step's metadata carries that must never leave the process.
const timelineSecret = "SELECT card_number FROM cards WHERE pan = '4242'"

// recordFixtureTimeline records a request that went through authentication and an ORM query that
// scanned rows, as application code would with the recorder the middleware attaches.
func recordFixtureTimeline(coll *Collector, id, path string) {
	rec := NewTimelineRecorder(coll.cfg)
	start := rec.Start()
	endRoot := rec.Step("Request Received")
	endAuth := rec.Step("Authentication")
	time.Sleep(time.Millisecond)
	endAuth(map[string]any{"scheme": "bearer"})
	endQuery := rec.Step("ORM Query")
	endScan := rec.Step("Row scan")
	time.Sleep(15 * time.Millisecond)
	endScan(map[string]any{"rows": 42, "sql": timelineSecret, "cached": false})
	endQuery(map[string]any{"table": "orders"})
	endRoot(map[string]any{"method": "GET", "path": path, "ip": "10.1.2.3"})
	coll.RecordTimeline(Timeline{
		ID: id, Time: start, Method: "GET", Path: path, Status: 200,
		Total: time.Since(start).Microseconds(), Steps: rec.Build(),
	})
}

// volatileKeys move between two reads of the same collector.
var volatileKeys = map[string]bool{"latency_us": true, "checked": true, "rate_per_sec": true}

func dropVolatile(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, val := range x {
			if !volatileKeys[k] {
				out[k] = dropVolatile(val)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = dropVolatile(val)
		}
		return out
	}
	return v
}

// sortedByName orders a list of objects by their name field.
func sortedByName(v any) any {
	list, ok := v.([]any)
	if !ok {
		return v
	}
	out := append([]any(nil), list...)
	sort.SliceStable(out, func(i, j int) bool {
		a, _ := out[i].(map[string]any)["name"].(string)
		b, _ := out[j].(map[string]any)["name"].(string)
		return a < b
	})
	return out
}

// jsonShape reduces a value to its field names and kinds.
func jsonShape(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, val := range x {
			out[k] = jsonShape(val)
		}
		return out
	case []any:
		return "array"
	case float64:
		return "number"
	}
	return reflect.TypeOf(v).String()
}

// TestReadEndpointsAnswerWhatTheHTTPEndpointsServe walks the table of in-process endpoints and
// holds each to the HTTP endpoint it stands for, through the real router. An entry that points at
// the wrong function, or at a path the dashboard does not serve, fails here and not in an agent's
// hands.
func TestReadEndpointsAnswerWhatTheHTTPEndpointsServe(t *testing.T) {
	router, coll := newReadableDashboard(t)
	base := strings.TrimSuffix(coll.cfg.BasePath, "/")

	// A query for each endpoint that narrows it, so a reader that ignores its query is caught too.
	queries := map[string]string{
		"/logs":     "level=error&trace_id=t-local-1",
		"/requests": "status=5xx",
		"/events":   "failed=1&source=events",
		// One of the two timelines, so a reader that ignores the limit answers differently.
		"/timeline": "limit=1",
		// Two of the three queries are over the slow threshold or match the statement: a reader that
		// ignores either answers more.
		"/queries": "slow=1&q=orders",
	}
	// A path to ask for where the entry has a :name segment.
	concrete := map[string]string{"/timeline/:id": "/timeline/tl-1"}

	paths := make([]string, 0, len(readEndpoints))
	for p := range readEndpoints {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	if len(paths) < 10 {
		t.Fatalf("the table holds %d endpoints, want at least the ten the MCP tools read: %v", len(paths), paths)
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			at := path
			if c, ok := concrete[path]; ok {
				at = c
			}
			target := base + "/api" + at
			if q := queries[path]; q != "" {
				target += "?" + q
			}
			ctx := invokeAPI(t, router, target)
			if ctx.Res == nil || (ctx.Res.Status != 0 && ctx.Res.Status != 200) {
				t.Fatalf("the HTTP endpoint answered %+v", ctx.Res)
			}
			var viaHTTP any
			if err := json.Unmarshal(ctx.Res.Body, &viaHTTP); err != nil {
				t.Fatalf("the HTTP body is not JSON: %v: %s", err, ctx.Res.Body)
			}

			values, _ := url.ParseQuery(queries[path])
			payload, err := coll.readEndpoint(at, values)
			if err != nil {
				t.Fatalf("readEndpoint: %v", err)
			}
			encoded, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("the payload does not encode: %v", err)
			}
			var viaReader any
			if err := json.Unmarshal(encoded, &viaReader); err != nil {
				t.Fatal(err)
			}

			if path == "/health" {
				// The dashboard runs its checks out of a map, so two reads answer in different orders.
				viaHTTP, viaReader = sortedByName(viaHTTP), sortedByName(viaReader)
			}
			if path == "/performance" {
				// Every counter in it moves between two reads.
				viaHTTP, viaReader = jsonShape(viaHTTP), jsonShape(viaReader)
			} else {
				viaHTTP, viaReader = dropVolatile(viaHTTP), dropVolatile(viaReader)
			}
			if !reflect.DeepEqual(viaHTTP, viaReader) {
				t.Errorf("the reader and the HTTP endpoint disagree\n  HTTP:   %v\n  reader: %v", viaHTTP, viaReader)
			}
			// Agreeing on nothing proves nothing.
			switch v := viaHTTP.(type) {
			case []any:
				if len(v) == 0 {
					t.Errorf("both answered an empty list, so agreement here is vacuous")
				}
				if path == "/timeline" && len(v) != 1 {
					t.Errorf("limit=1 over two timelines answered %d, so the limit was not applied: %v", len(v), v)
				}
				if path == "/queries" && len(v) != 1 {
					t.Errorf("slow=1 over three queries answered %d, so the filter was not applied: %v", len(v), v)
				}
			case map[string]any:
				for _, key := range map[string][]string{"/events": {"recent", "metrics"}, "/architecture": {"connections"}, "/performance": {"current"}, "/timeline/:id": {"steps"}}[path] {
					if inner, ok := v[key].([]any); ok && len(inner) == 0 {
						t.Errorf("both answered an empty %s, so agreement here is vacuous", key)
					}
				}
			}
		})
	}
}

func TestReadEndpointRefusesWhatItDoesNotServe(t *testing.T) {
	_, coll := newReadableDashboard(t)

	_, err := coll.readEndpoint("/db/tables", nil)
	var rerr *livesource.Error
	if !errors.As(err, &rerr) || rerr.Status != 404 {
		t.Fatalf("an endpoint the reader does not serve answered %v, want a 404 livesource.Error", err)
	}
	// Writes and the database browser are not readable this way, by design.
	for _, p := range []string{"/cache/clear", "/api-explorer", "/db/tables/users/rows"} {
		if _, ok := readEndpoints[p]; ok {
			t.Errorf("%s is in the table of in-process reads", p)
		}
	}
}

// TestInstallPublishesTheCollectorAndTheLastOneWins: an application never has to publish its
// dashboard, and one with two means the later.
func TestInstallPublishesTheCollectorAndTheLastOneWins(t *testing.T) {
	livesource.Set(nil)

	first := Install(nil, breeze.NewRouter(), func() Config { c := DefaultConfig(); c.DisableAuth = true; return c }())
	first.RecordLog("app", LogEntry{Time: time.Now(), Message: "line-of-the-first"})
	if livesource.Current() == nil {
		t.Fatal("Install published no reader")
	}

	second := Install(nil, breeze.NewRouter(), func() Config { c := DefaultConfig(); c.DisableAuth = true; return c }())
	second.RecordLog("app", LogEntry{Time: time.Now(), Message: "line-of-the-second"})

	read := func() string {
		v, err := livesource.Current().Read("/logs", url.Values{"q": {"line-of-the"}})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(v)
		return string(b)
	}
	if got := read(); !strings.Contains(got, "line-of-the-second") || strings.Contains(got, "line-of-the-first") {
		t.Errorf("the last install did not win: %s", got)
	}

	first.MakeCurrent()
	if got := read(); !strings.Contains(got, "line-of-the-first") || strings.Contains(got, "line-of-the-second") {
		t.Errorf("MakeCurrent did not select the first dashboard: %s", got)
	}
}

// TestReadEndpointTakesAParameterFromThePath: a segment written :name in the table is bound from
// the path, and it wins over a query value of the same name, as the router's parameter does.
func TestReadEndpointTakesAParameterFromThePath(t *testing.T) {
	_, coll := newReadableDashboard(t)

	got, err := coll.readEndpoint("/timeline/tl-1", url.Values{"id": {"tl-0"}})
	if err != nil {
		t.Fatalf("readEndpoint: %v", err)
	}
	tl, ok := got.(Timeline)
	if !ok || tl.ID != "tl-1" {
		t.Fatalf("answered timeline %q (%T), want the one named in the path (tl-1) and not the one in the query (tl-0)", tl.ID, got)
	}

	for _, path := range []string{"/timeline/", "/timeline/tl-1/extra", "/timeline//"} {
		if _, err := coll.readEndpoint(path, nil); err == nil {
			t.Errorf("%q was answered; a path with no id, or with more segments than the pattern, is not the pattern", path)
		}
	}
}

// TestTimelineNotFoundIsA404OverBothTransports: an id that is not held is the same answer in
// memory and over HTTP, and an agent reading either must be able to tell it from a dashboard that
// is not there.
func TestTimelineNotFoundIsA404OverBothTransports(t *testing.T) {
	router, coll := newReadableDashboard(t)
	base := strings.TrimSuffix(coll.cfg.BasePath, "/")

	// The premise: the list is there and holds timelines, so a 404 below is about the id.
	if got := invokeAPI(t, router, base+"/api/timeline"); got.Res == nil || got.Res.Status > 200 {
		t.Fatalf("the list endpoint answered %+v", got.Res)
	}
	if ctx := invokeAPI(t, router, base+"/api/timeline/no-such-id"); ctx.Res == nil || ctx.Res.Status != 404 {
		t.Fatalf("an id that is not held answered status %d over HTTP, want 404", ctx.Res.Status)
	}
	_, err := coll.readEndpoint("/timeline/no-such-id", nil)
	var rerr *livesource.Error
	if !errors.As(err, &rerr) || rerr.Status != 404 {
		t.Fatalf("an id that is not held answered %v in memory, want a 404 livesource.Error", err)
	}
}

// TestTimelineFixtureHasTheShapeTheToolsRebuild is the premise the MCP timeline tool is written
// against: a nested step is both inside its parent and an entry of the flat list. If the recorder
// stops doing that, the tool's rebuilding is wrong, and this says so before an agent does.
func TestTimelineFixtureHasTheShapeTheToolsRebuild(t *testing.T) {
	_, coll := newReadableDashboard(t)

	tl, ok := coll.timelineByID("tl-1")
	if !ok {
		t.Fatal("the fixture's timeline is not held")
	}
	byName := map[string]int{}
	for _, s := range tl.Steps {
		byName[s.Name]++
	}
	for _, name := range []string{"Request Received", "Authentication", "ORM Query", "Row scan"} {
		if byName[name] != 1 {
			t.Errorf("the flat list holds %q %d times, want once: %v", name, byName[name], byName)
		}
	}
	root := tl.Steps[0]
	if root.Name != "Request Received" || len(root.Children) != 2 {
		t.Fatalf("the root step is %q with %d children, want Request Received with Authentication and ORM Query", root.Name, len(root.Children))
	}
	if q := root.Children[1]; q.Name != "ORM Query" || len(q.Children) != 1 || q.Children[0].Name != "Row scan" {
		t.Errorf("a grandchild is not nested under its parent: %+v", q)
	}
}

// TestQueriesPayloadNarrowsByLimitSlownessAndText: the three narrowings the endpoint has, each on its
// own and together, against the three queries the fixture holds. The drift test shows the reader and
// the endpoint agree; this shows what they agree on.
func TestQueriesPayloadNarrowsByLimitSlownessAndText(t *testing.T) {
	_, coll := newReadableDashboard(t)

	ids := func(params url.Values) string {
		var out []string
		for _, q := range coll.queriesPayload(queryValues(params)) {
			out = append(out, q.ID)
		}
		return strings.Join(out, ",")
	}
	for name, tc := range map[string]struct {
		params url.Values
		want   string
	}{
		"everything":               {url.Values{}, "q1,q2,q3"},
		"the most recent one":      {url.Values{"limit": {"1"}}, "q3"},
		"only the slow":            {url.Values{"slow": {"1"}}, "q2"},
		"only the slow, spelled":   {url.Values{"slow": {"true"}}, "q2"},
		"by text, any case":        {url.Values{"q": {"insert"}}, "q3"},
		"slow and by text apart":   {url.Values{"slow": {"1"}, "q": {"insert"}}, ""},
		"the window is read first": {url.Values{"limit": {"1"}, "slow": {"1"}}, ""},
	} {
		if got := ids(tc.params); got != tc.want {
			t.Errorf("%s: %v answered %q, want %q", name, tc.params, got, tc.want)
		}
	}
}
