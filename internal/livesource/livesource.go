// Package livesource is the seam between the Developer Dashboard and an MCP
// endpoint that lives in the same process.
//
// The dashboard serves what it knows over HTTP, and the MCP tools that read a
// running service used to reach it only that way. For an endpoint embedded in
// the service it describes that is the wrong road: the read goes out through the
// listener and back in through the router, the app's own middleware (its rate
// limiter, its auth) stands in the way, and the call is refused exactly when the
// service is rejecting traffic, which is when it is wanted. Everything the
// endpoint serves is already in memory, in a collector the dashboard owns.
//
// This package carries no data and imports nothing from the framework. It holds
// the one process-wide handle the dashboard fills and the MCP server reads, for
// the same reason package diag is a leaf: the dashboard imports the root breeze
// package, the root package imports internal/mcp, so neither side may import the
// other, and a third package both can name is the only way across.
package livesource

import (
	"fmt"
	"net/url"
	"sync/atomic"
)

// Reader answers a dashboard read endpoint from memory.
//
// path is the endpoint as the dashboard's API names it below its base, for
// example "/logs" or "/timeline/ab12", and query is what would have followed the
// question mark. The returned value is the endpoint's payload, a value that
// marshals to the JSON the HTTP endpoint would have served: the caller decodes it
// with the same types, so the two transports cannot disagree about shape.
//
// A Reader answers what the endpoint answers and nothing else. It does not
// authenticate, because nothing reaches it that has not already passed the MCP
// endpoint's own token and scope; and it is not subject to the application's
// rate limiter, because it never touches the application's router.
type Reader interface {
	Read(path string, query url.Values) (any, error)
}

// Func adapts a function to a Reader.
//
// It exists so a package can publish a reader without exporting a method for it:
// the dashboard hands over a closure and its Collector gains no public Read.
type Func func(path string, query url.Values) (any, error)

// Read implements Reader.
func (f Func) Read(path string, query url.Values) (any, error) { return f(path, query) }

// Error is a failure a Reader reports with the status the HTTP endpoint would
// have answered with, so a caller can tell "no such record" from "broken".
type Error struct {
	// Status is the HTTP status the endpoint answers with for the same condition.
	Status int
	// Message is the endpoint's own explanation.
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("%d: %s", e.Status, e.Message) }

// holder exists because atomic.Pointer needs a concrete type and Reader is an
// interface; it also lets Set(nil) mean "none" without a typed-nil trap.
type holder struct{ r Reader }

var current atomic.Pointer[holder]

// Set publishes r as the reader for this process. The last call wins, which
// mirrors diag.Register: one process has one dashboard in practice, and a process
// that installs a second means the second.
//
// Set(nil) withdraws the reader.
func Set(r Reader) {
	if r == nil {
		current.Store(nil)
		return
	}
	current.Store(&holder{r: r})
}

// Current returns the process's reader, or nil when no dashboard has been
// installed in it.
func Current() Reader {
	if h := current.Load(); h != nil {
		return h.r
	}
	return nil
}
