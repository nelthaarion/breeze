package mcp

// live.go — the shared transport for the tools that read a running service.
//
// Everything above this file works on source: it parses a tree, runs the
// generator, reports what it found. The tools built on this file are different in
// kind — they ask a process that is already serving traffic what it is currently
// doing. That difference is worth isolating, because it brings failure modes the
// source tools do not have, and each one needs an answer an agent can act on.
//
// Four failures matter, and the message for each has to distinguish them:
//
//   - Nothing is listening. The agent should start the service, not retry.
//   - Something is listening and it is not this. A wrong port produces a
//     perfectly valid response full of the wrong shape; the caller has to be
//     told that, or it will report the absence of routes as a finding.
//   - The endpoint exists but is protected. That is a configuration answer
//     (a token), not a reason to conclude the feature is missing.
//   - The feature is not installed. `breeze add dashboard` is the fix, and that
//     is a different sentence from any of the above.
//
// The client is the framework's own, from package client. Using net/http here
// would mean this repository's own tooling did not exercise the client it ships,
// and the gnet loop is exactly what a Breeze service is being served by.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/nelthaarion/breeze/client"
)

// liveTimeout bounds one call to a service.
//
// Short on purpose: these tools sit in an interactive loop, and a caller waiting
// thirty seconds to be told a port is closed has been given the answer far too
// late to be useful. A service that cannot answer an inspection endpoint in five
// seconds is itself the finding.
const liveTimeout = 5 * time.Second

// liveRequest is one call to a running service.
type liveRequest struct {
	// baseURL is the service root as the caller gave it, e.g. http://127.0.0.1:8080.
	baseURL string
	// path is the absolute path to fetch, including any query.
	path string
	// token, when set, is sent as X-Fleet-Token. Only the endpoints wrapped by
	// the dashboard's wrapService accept it — the logs endpoint is the one that
	// matters here — and the Fleet aggregator uses the same header.
	token string
	// username and password, when set, are sent as HTTP Basic.
	//
	// Both mechanisms are carried because the dashboard does not accept one
	// credential everywhere: its ordinary API endpoints (routes, requests,
	// performance) go through an auth middleware that takes a session cookie or
	// Basic, and will reject a service token outright, while the logs endpoint
	// additionally takes the token. A caller told to "pass the token" for
	// breeze_get_routes would get a 401 it could not explain.
	username string
	password string
	// feature names the thing being read, so a 404 can say which feature is
	// missing rather than only which path was not found.
	feature string
	// notFound overrides the 404 message.
	//
	// The default reading of a 404 — "this feature is not installed" — is right
	// for a collection endpoint and wrong for one that addresses a single
	// record. The aggregator returns 404 for a trace that has aged out of its
	// retention window, and telling an agent that Fleet is not installed when
	// the trace was merely too old would send it to reinstall a working
	// feature. Endpoints that can legitimately 404 set this instead.
	notFound string
}

// liveError is a failed call, kept as a type so a tool can add its own context
// without losing the distinction between the four failures above.
type liveError struct {
	// Kind is one of unreachable, unauthorized, missing, malformed, status.
	Kind string
	// Message is written for the agent that has to act on it.
	Message string
}

func (e *liveError) Error() string { return e.Message }

// normaliseBaseURL turns what a caller is likely to pass into something that can
// be joined with a path.
//
// A bare host:port is accepted because that is what a person reads off a startup
// log, and rejecting it would be pedantry. A trailing slash is trimmed so that
// joining never produces a double slash, which some routers treat as a different
// path.
func normaliseBaseURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("a service URL is required, for example http://127.0.0.1:8080")
	}

	if !strings.Contains(trimmed, "://") {
		trimmed = "http://" + trimmed
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("%q is not a usable URL: %w", raw, err)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("%q has no host, so there is nothing to connect to", raw)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("%q uses scheme %q; only http and https can be inspected", raw, parsed.Scheme)
	}

	return strings.TrimSuffix(parsed.Scheme+"://"+parsed.Host+parsed.Path, "/"), nil
}

// fetchLiveJSON performs one GET and decodes the body into dst.
//
// dst is decoded strictly enough to catch the wrong-service case: a response that
// is not JSON at all means whatever answered is not one of these endpoints, and
// saying so is far more useful than reporting an empty result.
func fetchLiveJSON(req liveRequest, dst any) *liveError {
	base, err := normaliseBaseURL(req.baseURL)
	if err != nil {
		return &liveError{Kind: "malformed", Message: err.Error()}
	}

	c := client.New(client.Config{Timeout: liveTimeout})
	defer c.Close()

	target := base + req.path

	r := client.NewRequest("GET", target, nil)
	// Asked for explicitly, because the dashboard decides between an HTML login
	// page and a JSON 401 by looking at this header. Without it a protected
	// endpoint returns a page, and the decoder below would report "not JSON"
	// for what is really an auth failure.
	r.SetHeader("Accept", "application/json")
	if req.token != "" {
		r.SetHeader("X-Fleet-Token", req.token)
	}
	if req.username != "" || req.password != "" {
		r.SetHeader("Authorization", "Basic "+
			base64.StdEncoding.EncodeToString([]byte(req.username+":"+req.password)))
	}

	resp, callErr := c.Do(r)
	if callErr != nil {
		return &liveError{
			Kind: "unreachable",
			Message: fmt.Sprintf("could not reach %s: %v. Start the service, or check the host and port; "+
				"nothing was read, so this says nothing about the service's state.", target, callErr),
		}
	}

	switch {
	case resp.Status == 401 || resp.Status == 403:
		return &liveError{
			Kind: "unauthorized",
			Message: fmt.Sprintf("%s returned %d. This endpoint is protected. Pass username and password: "+
				"both the dashboard's API endpoints and the Fleet aggregator's read endpoints authenticate "+
				"with HTTP Basic. The token argument is a different credential — it is the service token, "+
				"which the dashboard's logs endpoint accepts and the aggregator requires for span ingestion, "+
				"and it will not authorise a read. The feature is installed; this is a credentials answer, "+
				"not a missing one.", target, resp.Status),
		}

	case resp.Status == 404:
		if req.notFound != "" {
			return &liveError{Kind: "missing", Message: req.notFound}
		}
		return &liveError{
			Kind: "missing",
			Message: fmt.Sprintf("%s returned 404. Either the %s feature is not installed in this service "+
				"(add it with breeze_add), or it is mounted under a different base path than the default.",
				target, req.feature),
		}

	case resp.Status < 200 || resp.Status > 299:
		return &liveError{
			Kind:    "status",
			Message: fmt.Sprintf("%s returned %d: %s", target, resp.Status, bodyExcerpt(resp.Body)),
		}
	}

	if err := json.Unmarshal(resp.Body, dst); err != nil {
		// The most likely cause by far is the wrong port — an unrelated server, or
		// this project's own frontend. Guessing that out loud saves a round of
		// confusion, because the alternative reading ("the service is broken") is
		// both more alarming and less often true.
		return &liveError{
			Kind: "malformed",
			Message: fmt.Sprintf("%s answered with %d but the body is not the JSON this endpoint returns (%v). "+
				"Whatever is listening on this port is probably not the %s endpoint of a Breeze service. "+
				"First bytes: %s", target, resp.Status, err, req.feature, bodyExcerpt(resp.Body)),
		}
	}

	return nil
}

// bodyExcerpt returns a short, single-line excerpt of a response body for an
// error message.
//
// Named for what it produces rather than "firstLine", which is what it was
// called: it takes the first line *and* truncates it *and* substitutes a
// placeholder for an empty body, and the name promising only the first of those
// invited a reader to assume it matched scalar.firstLine. That one really is just
// a first line, and the two are not interchangeable — swapping them here would
// paste an entire HTML error page into a tool result.
//
// Bodies here can be a whole HTML page, and pasting one into a tool result buries
// the sentence that matters.
func bodyExcerpt(body []byte) string {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return "(empty body)"
	}
	if idx := strings.IndexAny(text, "\r\n"); idx >= 0 {
		text = text[:idx]
	}
	const limit = 160
	if len(text) > limit {
		return text[:limit] + "…"
	}
	return text
}

// liveResultError converts a transport failure into a tool result.
//
// These are real failures — the caller asked for live state and did not get it —
// so IsError is set, unlike a check that ran and found violations.
func liveResultError(what string, err *liveError) toolCallResult {
	return structuredErrorResult(what+": "+err.Message, map[string]any{
		"error":  err.Kind,
		"detail": err.Message,
	})
}
