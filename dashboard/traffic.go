package dashboard

// traffic.go — the exact counts behind the sampled request records, and what was not kept.
//
// The requests endpoint serves records, and under the capture policy a record is a sample:
// a failure is kept up to a budget per route and window, a success only if it was slow or
// someone was watching. A reader who sees ten 404s cannot tell whether that is ten or ten
// thousand. This endpoint answers that: every request is counted, by route and status
// class, and the policy is stated in the payload rather than in a note someone has to keep
// in step with the code.

import (
	"sort"
	"strings"

	"github.com/nelthaarion/breeze/v2"
)

// RouteTraffic is the exact count of failures against one route, and how many of them were
// not kept as full records.
type RouteTraffic struct {
	Method  string `json:"method"`
	Pattern string `json:"pattern"`
	// Requests is every request the route served, whatever its status.
	Requests int64 `json:"requests"`
	// ClientErrors counts statuses 400 to 499, ServerErrors 500 and above.
	ClientErrors int64 `json:"client_errors"`
	ServerErrors int64 `json:"server_errors"`
	// NotRecorded is how many of those failures were counted but not stored in full,
	// because the route had used its budget for the window.
	NotRecorded int64 `json:"not_recorded"`
}

// CapturePolicy states what the dashboard keeps in full, so a reader can say precisely what
// it is and is not looking at.
type CapturePolicy struct {
	// Watching is true while someone has the dashboard open, which keeps every request.
	Watching bool `json:"watching"`
	// WindowSeconds is the interval a route's failure budget applies to.
	WindowSeconds int `json:"window_seconds"`
	// ClientErrorsPerWindow and ServerErrorsPerWindow are the full records kept per route
	// per window.
	ClientErrorsPerWindow int `json:"client_errors_per_window"`
	ServerErrorsPerWindow int `json:"server_errors_per_window"`
	// SlowRequestMS is the duration above which a request is kept in full; 0 disables it.
	SlowRequestMS int `json:"slow_request_ms"`
	// RingCapacity is how many full records the dashboard retains in all.
	RingCapacity int `json:"ring_capacity"`
}

// TrafficReport is what the traffic endpoint serves.
type TrafficReport struct {
	// Instrumented is true once Middleware has been created for this collector. A dashboard
	// that is installed without it counts nothing, and every route then reads zero.
	Instrumented bool `json:"instrumented"`
	// Enabled is the dashboard's own switch; a disabled dashboard counts nothing either.
	Enabled bool `json:"enabled"`

	Requests     int64 `json:"requests"`
	ClientErrors int64 `json:"client_errors"`
	ServerErrors int64 `json:"server_errors"`

	// Unmatched counts requests that ran the middleware chain and that no route served: a CORS
	// preflight, the auto-served index page. A request for a path that is not routed at all is
	// answered by the framework before any middleware runs, so it is not counted here or anywhere
	// else in the dashboard.
	Unmatched RouteTraffic `json:"unmatched"`
	// Routes lists the routes that have seen at least one failure, most failed first.
	Routes []RouteTraffic `json:"routes"`

	Capture CapturePolicy `json:"capture"`
}

func (c *Collector) handleTraffic(ctx *breeze.Context) error {
	return ctx.JSON(c.trafficPayload())
}

// trafficPayload builds the traffic report. The handler and the in-process reader (local.go)
// both call it.
func (c *Collector) trafficPayload() TrafficReport {
	// A route registered after the first request is unknown to the index until this runs; it
	// is a cold path, so it can afford the rebuild.
	if c.router != nil {
		c.refreshRouteIndex(false)
	}

	rep := TrafficReport{
		Instrumented: c.instrumented.Load(),
		Enabled:      c.cfg.Enabled,
		Requests:     c.requestsTotal.Load(),
		Routes:       []RouteTraffic{},
		Capture: CapturePolicy{
			WindowSeconds:         int(captureWindow.Seconds()),
			ClientErrorsPerWindow: clientErrorBudget,
			ServerErrorsPerWindow: serverErrorBudget,
			SlowRequestMS:         c.cfg.SlowRequestMs,
			RingCapacity:          c.cfg.MaxRequests,
		},
	}
	if c.hub != nil && c.hub.clientCount() > 0 {
		rep.Capture.Watching = true
	}

	rep.Unmatched = RouteTraffic{
		Method:       "*",
		Pattern:      "(no route matched)",
		Requests:     c.unmatched.acc.requests.Load(),
		ClientErrors: c.unmatched.acc.clientErrors.Load(),
		ServerErrors: c.unmatched.acc.errors.Load(),
		NotRecorded:  c.unmatched.notRecorded(),
	}
	rep.ClientErrors += rep.Unmatched.ClientErrors
	rep.ServerErrors += rep.Unmatched.ServerErrors

	// The drops live on the index's entries, the counts on the accumulators they share.
	dropped := map[*routeStatAccumulator]int64{}
	if ri := c.routeIdx.Load(); ri != nil {
		for i := range ri.methods {
			collectDropped(ri.methods[i].root, dropped)
		}
	}

	c.routeStatsMu.RLock()
	for _, acc := range c.routeStats {
		client, server := acc.clientErrors.Load(), acc.errors.Load()
		rep.ClientErrors += client
		rep.ServerErrors += server
		if client+server == 0 {
			continue
		}
		rep.Routes = append(rep.Routes, RouteTraffic{
			Method:       acc.method,
			Pattern:      acc.pattern,
			Requests:     acc.requests.Load(),
			ClientErrors: client,
			ServerErrors: server,
			NotRecorded:  dropped[acc],
		})
	}
	c.routeStatsMu.RUnlock()

	sort.Slice(rep.Routes, func(i, j int) bool {
		fi := rep.Routes[i].ClientErrors + rep.Routes[i].ServerErrors
		fj := rep.Routes[j].ClientErrors + rep.Routes[j].ServerErrors
		if fi != fj {
			return fi > fj
		}
		return strings.Compare(rep.Routes[i].Method+rep.Routes[i].Pattern, rep.Routes[j].Method+rep.Routes[j].Pattern) < 0
	})
	return rep
}

// collectDropped adds up, per accumulator, what its entry's samplers dropped.
func collectDropped(n *ixNode, into map[*routeStatAccumulator]int64) {
	if n == nil {
		return
	}
	for _, e := range []*routeEntry{n.exact, n.wild} {
		if e != nil {
			into[e.acc] += e.notRecorded()
		}
	}
	for _, kid := range n.kids {
		collectDropped(kid, into)
	}
	collectDropped(n.param, into)
}
