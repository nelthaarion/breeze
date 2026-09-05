package fleet

// Outgoing propagation for HTTP (§5.2). This file holds the http.Header carrier
// adapter, the PropagateFromHTTP one-liner, and the Client wrapper that folds
// propagation into an ordinary Do call.
//
// # Why propagation is explicit
//
// Nothing here installs itself. Breeze does not wrap http.DefaultTransport, does
// not patch net/http, and does not trace calls a developer did not ask it to
// trace. The cost is one call per outgoing request; the benefit is that reading
// the call site tells you whether a call is traced, and a library making its own
// unrelated HTTP calls never silently joins your trace.
//
// # What happens when propagation is forgotten
//
// The downstream service starts a new root trace. Two traces where there should
// be one, no error, no panic, no lost data — the failure is a gap in a picture,
// which is the right severity for forgetting an observability call.

import (
	"net/http"

	"github.com/nelthaarion/breeze"
)

// HTTPHeaderCarrier adapts http.Header to Carrier.
//
// Set goes through http.Header.Set, so the canonical MIME form ("Traceparent")
// is written. That is not a compatibility problem: HTTP header names are
// case-insensitive by definition, Go's server canonicalizes on read, and
// breeze's own parser lowercases — so every reader in the fleet, Breeze or not,
// sees the header regardless of the case it was written in.
func HTTPHeaderCarrier(h http.Header) Carrier { return httpHeaderCarrier{h: h} }

type httpHeaderCarrier struct{ h http.Header }

func (c httpHeaderCarrier) Set(key, value string) {
	if c.h == nil {
		return
	}
	c.h.Set(key, value)
}

func (c httpHeaderCarrier) Get(key string) (string, bool) {
	if c.h == nil {
		return "", false
	}
	v := c.h.Get(key)
	return v, v != ""
}

// PropagateFromHTTP copies the current request's trace context onto an outgoing
// HTTP request, so the service being called joins this trace instead of starting
// its own.
//
//	req, _ := http.NewRequest("POST", url, body)
//	fleet.PropagateFromHTTP(ctx, req)
//	resp, err := client.Do(req)
//
// Safe with a nil ctx, a nil req, or tracing disabled — in each case it does
// nothing, so call sites need no guards.
func PropagateFromHTTP(ctx *breeze.Context, req *http.Request) {

	if req == nil {
		return
	}
	if req.Header == nil {
		// http.NewRequest always populates Header, but a hand-built
		// &http.Request{} does not, and injecting into a nil map would
		// panic on a request that was otherwise usable.
		req.Header = make(http.Header)
	}
	Inject(ctx, HTTPHeaderCarrier(req.Header))
}

// Client is an http.Client that propagates trace context on every request it
// sends.
//
// It exists for the common case where a service holds one client for one
// downstream dependency and wants every call traced:
//
//	orders := fleet.WrapClient(http.DefaultClient, ctx)
//	resp, err := orders.Do(req)
//
// It is a convenience over PropagateFromHTTP + Do, not a second mechanism: Do
// calls exactly the same Inject path, so the two can never drift.
type Client struct {
	// Underlying is the client that actually performs the request. Nil uses
	// http.DefaultClient, so a zero Client is usable.
	Underlying *http.Client

	// ctx is the request whose trace this client propagates. A Client is
	// therefore per-request, not per-service — see WrapClient.
	ctx *breeze.Context
}

// WrapClient returns a Client that injects ctx's trace context into every
// request.
//
// The returned value is bound to one request's trace, so it must be created
// inside the handler rather than stored on a service struct at startup. That is
// a deliberate constraint of tracing over *net/http*: a long-lived client has no
// idea which request is calling it, and threading a trace through a shared client
// would require either a context argument on every call (which is
// PropagateFromHTTP) or goroutine-local state (which Go does not have).
//
// The wrapper is a small struct with no allocation beyond itself, so creating one
// per request is not a cost worth avoiding.
func WrapClient(underlying *http.Client, ctx *breeze.Context) *Client {

	return &Client{Underlying: underlying, ctx: ctx}
}

// Do injects trace context into req and sends it.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	if c == nil {
		return http.DefaultClient.Do(req)
	}
	PropagateFromHTTP(c.ctx, req)
	if c.Underlying == nil {
		return http.DefaultClient.Do(req)
	}
	return c.Underlying.Do(req)
}

// Get issues a traced GET.
func (c *Client) Get(url string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return c.Do(req)
}
