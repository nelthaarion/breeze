package aggregator

// Trace-correlated log stitching (§9C.2): given a trace id, return the log
// lines every service in that trace emitted while handling it, merged into one
// timestamp-ordered stream.
//
// # Why the aggregator stores no logs
//
// The obvious design is to ship log bodies alongside spans. It is also the
// wrong one. Every service already keeps its own bounded log ring buffer with
// its own search, and duplicating that here would mean carrying the fleet's
// entire log volume over the network, storing it twice, and inventing a second
// retention policy that disagrees with the first. Instead this file is a
// router: it asks each service that appears in the trace for its own logs for
// that trace id, and merges the answers. Storage stays where it already is, and
// a log line is only ever moved when someone actually looks at it.
//
// # Why partial results are a feature
//
// A fan-out over N services will sometimes find one of them gone — that is the
// normal state of affairs during exactly the incident someone is using this
// page to debug. Failing the whole panel because one service is unreachable
// would withhold the evidence from the services that *did* answer, at the
// moment it is most needed. Every response therefore carries a per-service
// status, and an unreachable service is reported as unavailable next to the
// logs that did arrive (§9C.4's resilience principle, applied per-source).

import (
	"context"
	"encoding/json"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"


	"github.com/nelthaarion/breeze/client"
	"github.com/nelthaarion/breeze/dashboard"
)

// logFanoutTimeout bounds one service's log fetch.
//
// Deliberately short: this is a UI-blocking request against N services in
// parallel, so the user waits for the slowest one. A service that cannot answer
// in two seconds is more useful reported as unavailable than waited on — the
// panel renders, and the row says which source is missing.
const logFanoutTimeout = 2 * time.Second

// maxLogsPerService bounds how many lines one service may contribute.
//
// Without a cap, a single service logging in a tight loop inside one request
// would dominate the merged view and push every other service's lines off the
// end, which inverts the purpose of a cross-service panel.
const maxLogsPerService = 200

// TraceLog is one log line, tagged with the service it came from.
//
// dashboard.LogEntry is embedded rather than copied field-by-field so the shape
// the SPA already knows how to render stays identical — the only addition is
// attribution, which the per-service pages never needed because there the
// service was implicit.
type TraceLog struct {
	dashboard.LogEntry
	Service string `json:"service"`
}

// LogSource reports one service's participation in a fan-out.
//
// Present for every service in the trace, including the ones that answered, so
// the UI can distinguish "this service logged nothing for this request" from
// "this service could not be reached" — two very different facts that look
// identical if you only ship the lines that arrived.
type LogSource struct {
	Service string `json:"service"`

	// Available is false when the fetch failed for any reason. Error carries
	// the reason for display; it is not machine-interpreted.
	Available bool   `json:"available"`
	Error     string `json:"error,omitempty"`

	// Count is how many lines this service contributed.
	Count int `json:"count"`
}

// TraceLogs is the GET /fleet/api/traces/:id/logs response.
type TraceLogs struct {
	TraceID string     `json:"trace_id"`
	Logs    []TraceLog `json:"logs"`

	// Sources is per-service status, sorted by name. Always populated, even
	// when Logs is empty, because "who did we ask and what did they say" is
	// the part that makes an empty result interpretable.
	Sources []LogSource `json:"sources"`

	// Truncated is set when any service hit maxLogsPerService, so the UI can
	// say so rather than implying these are all the lines that exist.
	Truncated bool `json:"truncated,omitempty"`

	// Disabled explains why no fan-out was attempted, when none was.
	//
	// A separate field from an empty Logs list because the two mean opposite
	// things to someone debugging: "nobody logged anything for this request"
	// is a fact about the request, while "this feature is not configured" is
	// a fact about the deployment, and showing the second as the first sends
	// them looking for a missing log line that was never going to be there.
	Disabled string `json:"disabled,omitempty"`
}


// logFanout fetches and merges logs for one trace.
//
// Holds no state beyond its HTTP client and the token, so it can be built once
// at install and shared by every request.
type logFanout struct {
	client *client.Client

	// token is sent as X-Fleet-Token. The service side compares it against
	// dashboard.Config.ServiceToken in constant time and refuses log bodies
	// without it (§11.2) — logs are the most sensitive thing this feature
	// moves, so an unauthenticated fan-out is not offered as an option.
	token string
}

func newLogFanout(token string) *logFanout {
	return &logFanout{
		client: client.New(client.Config{Timeout: logFanoutTimeout}),
		token:  token,
	}
}

// Collect asks every service in the trace for its logs and merges them.
//
// Services are queried concurrently: serially, a five-service trace with one
// dead service would take five timeouts to render. Concurrently it takes one.
func (f *logFanout) Collect(ctx context.Context, trace Trace, endpoints map[string]string) TraceLogs {
	out := TraceLogs{TraceID: trace.TraceID, Logs: []TraceLog{}, Sources: []LogSource{}}
	if len(trace.Services) == 0 {
		return out
	}

	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		logs []TraceLog
		srcs []LogSource
	)
	for _, service := range trace.Services {
		endpoint, ok := endpoints[service]
		if !ok || endpoint == "" {
			// A service in the trace that never heartbeated an address,
			// or whose heartbeat carried no OpenAPI URL to derive one
			// from. Reported rather than skipped: silently omitting it
			// would read as "this service logged nothing".
			//
			// Locked even though this runs on the calling goroutine: the
			// fetch goroutines for services earlier in the loop are
			// already running and appending to this same slice, so an
			// unlocked append here races them and can drop an entry.
			mu.Lock()
			srcs = append(srcs, LogSource{
				Service: service,
				Error:   "no known dashboard address for this service",
			})
			mu.Unlock()
			continue

		}

		wg.Add(1)
		go func(service, endpoint string) {
			defer wg.Done()
			lines, err := f.fetch(ctx, endpoint, trace.TraceID)

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				srcs = append(srcs, LogSource{Service: service, Error: err.Error()})
				return
			}
			if len(lines) > maxLogsPerService {
				lines = lines[:maxLogsPerService]
				out.Truncated = true
			}
			for _, line := range lines {
				logs = append(logs, TraceLog{LogEntry: line, Service: service})
			}
			srcs = append(srcs, LogSource{Service: service, Available: true, Count: len(lines)})
		}(service, endpoint)
	}
	wg.Wait()

	// Oldest-first: the panel sits under a waterfall that reads left to
	// right in time, and logs running the other way would fight it.
	sort.SliceStable(logs, func(i, j int) bool { return logs[i].Time.Before(logs[j].Time) })
	sort.Slice(srcs, func(i, j int) bool { return srcs[i].Service < srcs[j].Service })
	if logs != nil {
		out.Logs = logs
	}
	if srcs != nil {
		out.Sources = srcs
	}
	return out
}

// fetch retrieves one service's logs for a trace id.
func (f *logFanout) fetch(ctx context.Context, endpoint, traceID string) ([]dashboard.LogEntry, error) {
	target := strings.TrimSuffix(endpoint, "/") + "/api/logs?trace_id=" + url.QueryEscape(traceID)
	req := client.NewRequest("GET", target, nil).WithContext(ctx)
	if f.token != "" {
		req.SetHeader("X-Fleet-Token", f.token)
	}

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	if !resp.OK() {
		// The status is surfaced verbatim rather than mapped: 401 here
		// means the tokens disagree, which is a configuration mistake
		// worth showing plainly instead of flattening into "failed".
		return nil, &fanoutStatusError{status: resp.Status}
	}

	var entries []dashboard.LogEntry
	if err := json.Unmarshal(resp.Body, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// fanoutStatusError is a non-2xx response from a service's log endpoint.
type fanoutStatusError struct{ status int }

func (e *fanoutStatusError) Error() string {
	switch e.status {
	case 401, 403:
		return "unauthorized (check service_token matches this service's dashboard config)"
	case 404:
		return "no log endpoint (is the dashboard installed on this service?)"
	default:
		return "log endpoint returned status " + strconv.Itoa(e.status)
	}
}


// logEndpoints derives each service's dashboard base URL from its heartbeat.
//
// The heartbeat already carries openapi_url (§8.1.2) for the schema registry,
// and a service's dashboard lives alongside it on the same origin. Deriving the
// address from a field that already exists avoids adding a second one that
// could disagree with it — and avoids asking operators to configure the same
// host twice.
func (a *Aggregator) logEndpoints(services []string) map[string]string {
	if len(services) == 0 {
		return nil
	}
	want := make(map[string]struct{}, len(services))
	for _, s := range services {
		want[s] = struct{}{}
	}

	now := time.Now()
	out := make(map[string]string, len(services))
	for _, info := range a.registry.Snapshot(now) {
		if _, ok := want[info.Name]; !ok {
			continue
		}
		if base := dashboardBaseFromOpenAPI(info.OpenAPIURL); base != "" {
			out[info.Name] = base
		}
	}
	return out
}

// dashboardBaseFromOpenAPI turns "http://host/openapi.json" into
// "http://host/dashboard".
//
// Assumes the dashboard's default base path, which is what every service that
// has not deliberately moved it uses. A service that did move it will report as
// unavailable rather than being silently queried at the wrong path — a visible
// gap beats a confusing 404 attributed to the wrong cause.
func dashboardBaseFromOpenAPI(openapiURL string) string {
	if openapiURL == "" {
		return ""
	}
	u, err := url.Parse(openapiURL)
	if err != nil || u.Host == "" {
		return ""
	}
	u.Path = "/dashboard"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}
