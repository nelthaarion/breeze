package dashboard

import (
	"encoding/json"
	"errors"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

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
	return router, coll
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
	}

	paths := make([]string, 0, len(readEndpoints))
	for p := range readEndpoints {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	if len(paths) < 7 {
		t.Fatalf("the table holds %d endpoints, want at least the seven the MCP tools read: %v", len(paths), paths)
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			target := base + "/api" + path
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
			payload, err := coll.readEndpoint(path, values)
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
			case map[string]any:
				for _, key := range map[string][]string{"/events": {"recent", "metrics"}, "/architecture": {"connections"}, "/performance": {"current"}}[path] {
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
