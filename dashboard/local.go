package dashboard

// local.go — the dashboard's read endpoints, answered without HTTP.
//
// # Why this exists
//
// An MCP endpoint embedded in a service reads the dashboard to answer "what is this
// service doing". Reading it over HTTP means going out through the service's listener
// and back in through its router, past the application's own middleware, to ask a
// question whose answer is already in memory. Two things followed from that. The
// dashboard had to be installed with credentials the endpoint was told, and the call was
// rate limited with everything else: measured against a service with a 30-per-minute
// limit, a 40-request burst made /dashboard/api/logs, /requests and /health all answer
// 429, and the logs tool failed, in the one situation where "what is it logging" matters
// most.
//
// So an endpoint in the same process reads the collector directly. The seam is
// internal/livesource: Install publishes a Reader there, the MCP server finds it, and
// no import runs from one package to the other.
//
// # One payload function per endpoint
//
// Every endpoint below has a function that builds its payload from the collector and the
// query. The HTTP handler calls it and writes JSON; the Reader calls it and hands the
// value back. The alternative was a second implementation in the MCP package, which is
// how the diagnostics endpoint is built (shapeDiagnostics) and which survives only
// because a test compares the two. Here the shaping is not copied at all, so there is
// nothing to drift.
//
// # What is not here
//
// Authentication, because the MCP endpoint has authenticated its caller by its own token
// and scope before any tool runs. The rate limiter, because a read in memory never
// touches the router. Writes: every endpoint in the table is a GET.

import (
	"fmt"
	"net/url"

	"github.com/nelthaarion/breeze/v2/internal/livesource"
)

// query is what a payload function takes from the request: the one method of
// *breeze.Context the handlers use, so an HTTP request and an in-process read are
// interchangeable to the code that answers them.
type query interface {
	Query(name string) string
}

// queryValues adapts url.Values to query with the same rule as Context.Query: the first
// value of the key, or "".
type queryValues url.Values

func (v queryValues) Query(name string) string { return url.Values(v).Get(name) }

// readEndpoints are the endpoints a Reader can answer, keyed by their path below
// <base>/api. The set is deliberately a table: a test walks it and checks each entry
// against the HTTP endpoint it stands for.
var readEndpoints = map[string]func(c *Collector, q query) (any, error){
	"/routes":       func(c *Collector, _ query) (any, error) { return c.routesPayload(), nil },
	"/requests":     func(c *Collector, q query) (any, error) { return c.requestsPayload(q), nil },
	"/logs":         func(c *Collector, q query) (any, error) { return c.logsPayload(q), nil },
	"/performance":  func(c *Collector, _ query) (any, error) { return c.performancePayload(), nil },
	"/traffic":      func(c *Collector, _ query) (any, error) { return c.trafficPayload(), nil },
	"/health":       func(c *Collector, _ query) (any, error) { return c.RunHealthChecks(), nil },
	"/architecture": func(c *Collector, _ query) (any, error) { return c.architecturePayload(), nil },
	"/events":       func(c *Collector, q query) (any, error) { return c.eventsView(q), nil },
}

// readEndpoint answers one read endpoint from memory.
func (c *Collector) readEndpoint(path string, params url.Values) (any, error) {
	read, ok := readEndpoints[path]
	if !ok {
		return nil, &livesource.Error{
			Status:  404,
			Message: fmt.Sprintf("the dashboard has no in-process reader for %q", path),
		}
	}
	return read(c, queryValues(params))
}

// MakeCurrent makes c the collector that a Breeze MCP endpoint in this process reads.
//
// Install does this, so an application never needs to: one process has one dashboard and
// the last one installed is the one read, as with the diagnostic registry. It is public
// for the process that installs more than one (tests, mostly) and wants a particular one
// to be the answer.
func (c *Collector) MakeCurrent() {
	livesource.Set(livesource.Func(c.readEndpoint))
}
