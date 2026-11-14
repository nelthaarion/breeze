package mcp

// embedded_live.go — the live tools of an embedded endpoint, reading their own process.
//
// An endpoint embedded in a service answers breeze_diagnose_service about that service
// without a URL by reading the diagnostic registry (tools_diagnose.go). This does the same
// for the tools that read the dashboard: routes, requests, logs, signals, health and
// performance. Without it they went out through the service's own listener and came back
// through its router, which cost three things that had nothing to do with the question.
//
//   - The dashboard had to be installed with credentials the endpoint was handed.
//   - The application's rate limiter applied. After a 40-request burst against a limit of
//     30 a minute, /dashboard/api/logs, /requests and /health all answered 429 and
//     breeze_get_logs failed: the tool was refused exactly when the service was rejecting
//     traffic, which is when its logs are wanted.
//   - A service that left the dashboard's HTTP surface out of production could not be
//     read by its own control plane.
//
// The reader comes from internal/livesource, which dashboard.Install fills. Nothing here
// imports the dashboard: the tools still decode the same JSON into the same mirror types
// and shape it with the same code, the only difference is where the bytes come from, so
// there is no second copy of the shaping to drift (see dashboard/local.go for the other
// half, and the drift tests that hold the two transports to one answer).
//
// Only an embedded server does this, for the reason useLocalDiagnostics gives: a
// standalone breeze-mcp has no dashboard of its own, and the process it lives in
// describes nothing the caller cares about. With a service_url a tool behaves exactly as
// it did, and reads that service over HTTP.

import (
	"encoding/json"
	"strings"

	"github.com/nelthaarion/breeze/v2/internal/livesource"
)

// embeddedLiveNote is appended to the description of a tool that can read its own process.
const embeddedLiveNote = " On this endpoint, omit service_url to read the process the endpoint is embedded in: " +
	"the dashboard's collector is read directly from memory, so no credentials are needed and the " +
	"application's own rate limiter cannot refuse the call, which matters most while the service is " +
	"rejecting traffic. Pass service_url to read another service over HTTP."

// embeddedServiceURLDescription replaces the service_url description on those tools.
const embeddedServiceURLDescription = "Base URL of ANOTHER running service, e.g. http://127.0.0.1:8080. A bare host:port " +
	"is accepted. Omit it to read the process this endpoint is embedded in."

// localLiveTools maps each tool an embedded server answers from its own process to the
// entry point that runs it with the reader attached.
//
// A table rather than a loop over names, because each tool decodes its own argument type
// and the reader has to be set on that type: the field is unexported precisely so that no
// caller can set it through the tool's arguments.
var localLiveTools = map[string]func(raw json.RawMessage, src livesource.Reader) toolCallResult{
	"breeze_get_routes":        bindReader(getRoutes, func(a *liveArgs) *liveArgs { return a }),
	"breeze_get_performance":   bindReader(getPerformance, func(a *liveArgs) *liveArgs { return a }),
	"breeze_get_health":        bindReader(getHealth, func(a *liveArgs) *liveArgs { return a }),
	"breeze_get_recent_errors": bindReader(getRecentErrors, func(a *recentErrorsArgs) *liveArgs { return &a.liveArgs }),
	"breeze_get_logs":          bindReader(getLogs, func(a *logsArgs) *liveArgs { return &a.liveArgs }),
	"breeze_get_signals":       bindReader(getSignals, func(a *signalsArgs) *liveArgs { return &a.liveArgs }),
}

// bindReader adapts a tool's typed entry point: decode its arguments, point the call at the
// reader and label the result as this process, then run it.
func bindReader[A any](run func(A) toolCallResult, live func(*A) *liveArgs) func(json.RawMessage, livesource.Reader) toolCallResult {
	return func(raw json.RawMessage, src livesource.Reader) toolCallResult {
		var a A
		if err := decodeArgs(raw, &a); err != nil {
			return errorResult("arguments: " + err.Error())
		}
		la := live(&a)
		la.ServiceURL = thisProcess
		la.local = src
		return run(a)
	}
}

// useLocalDashboard lets an embedded server answer the dashboard-reading tools about the
// process it lives in, when no service_url is given.
func (s *Server) useLocalDashboard() {
	for name, runLocal := range localLiveTools {
		t, ok := s.tools[name]
		if !ok {
			continue
		}
		remote := t.run
		runLocal := runLocal
		toolName := name

		t.description += embeddedLiveNote
		t.schema = urlOptional(t.schema, embeddedServiceURLDescription)
		t.run = func(raw json.RawMessage) toolCallResult {
			// Whether a URL was given is the only thing decided here. The rest of the arguments
			// are decoded by the tool's own type, so an unknown field or a wrong type is
			// reported by the same code in either mode.
			var probe struct {
				ServiceURL string `json:"service_url"`
			}
			if len(raw) > 0 && string(raw) != "null" {
				if err := json.Unmarshal(raw, &probe); err != nil {
					return errorResult("arguments: " + err.Error())
				}
			}
			if strings.TrimSpace(probe.ServiceURL) != "" {
				return remote(raw)
			}

			src := livesource.Current()
			if src == nil {
				return liveResultError("reading "+strings.TrimPrefix(toolName, "breeze_get_"), &liveError{
					Kind: "missing",
					Message: "no dashboard is installed in this process, so there is nothing to read directly. " +
						"Install it with breeze add dashboard, or pass service_url to read another service over HTTP.",
				})
			}
			return runLocal(raw, src)
		}
	}
}

// urlOptional returns a tool schema with service_url no longer required and described as
// the other service it now means. Every other property and requirement is kept.
func urlOptional(raw json.RawMessage, description string) json.RawMessage {
	var s map[string]any
	if err := json.Unmarshal(raw, &s); err != nil {
		return raw
	}
	if required, ok := s["required"].([]any); ok {
		kept := make([]any, 0, len(required))
		for _, r := range required {
			if name, _ := r.(string); name != "service_url" {
				kept = append(kept, r)
			}
		}
		if len(kept) == 0 {
			delete(s, "required")
		} else {
			s["required"] = kept
		}
	}
	if props, ok := s["properties"].(map[string]any); ok {
		if p, ok := props["service_url"].(map[string]any); ok {
			p["description"] = description
		}
	}
	return mustSchema(s)
}
