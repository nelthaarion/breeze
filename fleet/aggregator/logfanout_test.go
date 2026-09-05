package aggregator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nelthaarion/breeze/dashboard"
	"github.com/nelthaarion/breeze/fleet"
)

// logStub stands in for one service's dashboard log endpoint.
//
// A real httptest server rather than a fake client, because the parts most
// likely to break are the ones a fake would paper over: the URL the fan-out
// builds, the header it sends, and the JSON shape it expects back.
func logStub(t *testing.T, service string, lines []dashboard.LogEntry) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/dashboard/api/logs" {
			t.Errorf("%s: unexpected path %q", service, r.URL.Path)
		}
		if r.URL.Query().Get("trace_id") == "" {
			t.Errorf("%s: fan-out sent no trace_id filter", service)
		}
		if got := r.Header.Get("X-Fleet-Token"); got != "shared-secret" {
			t.Errorf("%s: token = %q, want shared-secret", service, got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(lines)
	}))
	t.Cleanup(srv.Close)
	return srv, srv.URL + "/dashboard"
}

func entry(msg string, at time.Time) dashboard.LogEntry {
	return dashboard.LogEntry{Time: at, Level: "app", Message: msg, TraceID: "t"}
}

// TestLogFanoutMergesByTimestamp is the core promise of §9C.2: lines from
// several services arrive interleaved in real time order, not grouped by
// whichever service answered first.
func TestLogFanoutMergesByTimestamp(t *testing.T) {
	base := time.Now().Truncate(time.Second)
	_, gateway := logStub(t, "gateway", []dashboard.LogEntry{
		entry("gateway: received", base),
		entry("gateway: returning", base.Add(30*time.Millisecond)),
	})
	_, orders := logStub(t, "orders", []dashboard.LogEntry{
		entry("orders: charging", base.Add(10*time.Millisecond)),
		entry("orders: charged", base.Add(20*time.Millisecond)),
	})

	f := newLogFanout("shared-secret")
	got := f.Collect(context.Background(),
		Trace{TraceID: "abc", Services: []string{"gateway", "orders"}},
		map[string]string{"gateway": gateway, "orders": orders})

	want := []string{
		"gateway: received",
		"orders: charging",
		"orders: charged",
		"gateway: returning",
	}
	if len(got.Logs) != len(want) {
		t.Fatalf("merged %d lines, want %d: %+v", len(got.Logs), len(want), got.Logs)
	}
	for i, msg := range want {
		if got.Logs[i].Message != msg {
			t.Errorf("line %d = %q, want %q (merge is not timestamp-ordered)", i, got.Logs[i].Message, msg)
		}
	}

	// Attribution must survive the merge, or an interleaved panel is
	// unreadable — the whole point is knowing who said what.
	if got.Logs[1].Service != "orders" {
		t.Errorf("line 1 attributed to %q, want orders", got.Logs[1].Service)
	}
	for _, src := range got.Sources {
		if !src.Available || src.Count != 2 {
			t.Errorf("source %+v: want available with 2 lines", src)
		}
	}
}

// TestLogFanoutPartialFailure is the §9C.4 resilience requirement: one dead
// service must not take the panel down with it. This is the case that matters
// most, because a service being unreachable is the normal state during the
// incident someone opened this panel to investigate.
func TestLogFanoutPartialFailure(t *testing.T) {
	base := time.Now()
	_, alive := logStub(t, "alive", []dashboard.LogEntry{entry("alive: ok", base)})

	// A server that is closed immediately: a connection refused, which is
	// what a crashed service actually looks like to a caller.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL + "/dashboard"
	dead.Close()

	f := newLogFanout("shared-secret")
	got := f.Collect(context.Background(),
		Trace{TraceID: "abc", Services: []string{"alive", "dead", "unregistered"}},
		map[string]string{"alive": alive, "dead": deadURL})

	if len(got.Logs) != 1 || got.Logs[0].Message != "alive: ok" {
		t.Fatalf("lost the reachable service's logs: %+v", got.Logs)
	}
	if len(got.Sources) != 3 {
		t.Fatalf("got %d sources, want 3 (every service in the trace is accounted for)", len(got.Sources))
	}

	byName := map[string]LogSource{}
	for _, s := range got.Sources {
		byName[s.Service] = s
	}
	if !byName["alive"].Available {
		t.Error("alive service marked unavailable")
	}
	if byName["dead"].Available || byName["dead"].Error == "" {
		t.Errorf("dead service = %+v, want unavailable with a reason", byName["dead"])
	}
	// A service in the trace with no known address is a distinct failure
	// from an unreachable one, and must not be silently dropped: omitting
	// it would read as "this service logged nothing".
	if byName["unregistered"].Available || byName["unregistered"].Error == "" {
		t.Errorf("unregistered service = %+v, want unavailable with a reason", byName["unregistered"])
	}
}

// TestLogFanoutUnauthorizedIsExplained checks a token mismatch produces an
// actionable message. A bare "failed" here would send someone hunting a network
// problem when the real cause is two config values disagreeing.
func TestLogFanoutUnauthorizedIsExplained(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
	}))
	defer srv.Close()

	f := newLogFanout("wrong-secret")
	got := f.Collect(context.Background(),
		Trace{TraceID: "abc", Services: []string{"svc"}},
		map[string]string{"svc": srv.URL + "/dashboard"})

	if len(got.Sources) != 1 || got.Sources[0].Available {
		t.Fatalf("sources = %+v, want one unavailable", got.Sources)
	}
	if msg := got.Sources[0].Error; !strings.Contains(msg, "service_token") {

		t.Errorf("error = %q, want it to name service_token as the cause", msg)
	}
}

// TestLogFanoutTruncates guards the per-service cap. Without it, one service
// logging in a loop inside a single request would push every other service's
// lines out of the panel and invert its purpose.
func TestLogFanoutTruncates(t *testing.T) {
	base := time.Now()
	noisy := make([]dashboard.LogEntry, maxLogsPerService+50)
	for i := range noisy {
		noisy[i] = entry("line", base.Add(time.Duration(i)*time.Millisecond))
	}
	_, url := logStub(t, "noisy", noisy)

	f := newLogFanout("shared-secret")
	got := f.Collect(context.Background(),
		Trace{TraceID: "abc", Services: []string{"noisy"}},
		map[string]string{"noisy": url})

	if len(got.Logs) != maxLogsPerService {
		t.Errorf("kept %d lines, want the %d cap", len(got.Logs), maxLogsPerService)
	}
	if !got.Truncated {
		t.Error("Truncated not set; the UI would imply these are all the lines that exist")
	}
}

// TestLogFanoutEmptyTraceHasNonNilSlices matters because the SPA iterates these
// fields directly. A nil slice marshals to null, and null is not iterable.
func TestLogFanoutEmptyTraceHasNonNilSlices(t *testing.T) {
	f := newLogFanout("shared-secret")
	got := f.Collect(context.Background(), Trace{TraceID: "abc"}, nil)

	body, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded struct {
		Logs    []TraceLog  `json:"logs"`
		Sources []LogSource `json:"sources"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Logs == nil || decoded.Sources == nil {
		t.Errorf("empty response marshalled a null slice: %s", body)
	}
}

// TestDashboardBaseFromOpenAPI covers the address derivation. The heartbeat
// carries an OpenAPI URL, not a dashboard URL, so this conversion is the only
// thing standing between a trace and its logs.
func TestDashboardBaseFromOpenAPI(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"plain", "http://orders:8080/openapi.json", "http://orders:8080/dashboard"},
		{"nested path", "https://host/api/v2/openapi.json", "https://host/dashboard"},
		{"query stripped", "http://host/openapi.json?pretty=1", "http://host/dashboard"},
		{"empty", "", ""},
		{"no host", "/openapi.json", ""},
		{"garbage", "://nonsense", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := dashboardBaseFromOpenAPI(tc.in); got != tc.want {
				t.Errorf("dashboardBaseFromOpenAPI(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestLogEndpointsOnlyIncludesTraceServices confirms the fan-out asks only the
// services in the trace. A fleet may have hundreds registered; querying all of
// them for one trace's logs would turn a debugging click into a fleet-wide load
// event.
func TestLogEndpointsOnlyIncludesTraceServices(t *testing.T) {
	a := &Aggregator{cfg: DefaultConfig().withDefaults(), registry: NewServiceRegistry(DefaultConfig().withDefaults())}
	now := time.Now()
	for _, svc := range []string{"gateway", "orders", "unrelated"} {
		a.registry.Observe(fleet.Heartbeat{
			Service:    svc,
			InstanceID: svc + "-1",
			OpenAPIURL: "http://" + svc + ":8080/openapi.json",
		}, now)
	}

	got := a.logEndpoints([]string{"gateway", "orders"})
	if len(got) != 2 {
		t.Fatalf("got %d endpoints, want 2: %+v", len(got), got)
	}
	if _, ok := got["unrelated"]; ok {
		t.Error("included a service that never appeared in the trace")
	}
	if got["gateway"] != "http://gateway:8080/dashboard" {
		t.Errorf("gateway endpoint = %q", got["gateway"])
	}
}
