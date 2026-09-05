package mcp

// observe.go — structured diagnostics for a running MCP server.
//
// # The problem this solves
//
// A running breeze-mcp prints its startup banner and then nothing. There is no record of
// which tools were called, whether they succeeded, how long they took, or that a network
// peer was refused — so "the agent says the tool failed" and "someone is probing the
// control port" are both invisible. That is a poor position for an endpoint that can
// write files and start containers.
//
// # Why events rather than a log call at each site
//
// Every field below is either a fixed string chosen in this package or a number. Nothing
// here can carry a tool argument, a file's contents, a captured stdout, or a token,
// because there is no field of a shape that would hold one. A `Log(fmt.Sprintf(...))`
// interface would have made every future call site a place where a value could be
// interpolated by accident; this makes that a compile error instead.
//
// The one field derived from caller data is ArgNames, and it is *names only*. See
// argumentNames for why that is a categorical guarantee rather than a redaction list.

import (
	"encoding/json"
	"sort"
	"time"
)

// EventKind is what happened. A closed set, so a consumer can switch on it.
type EventKind string

const (
	// EventToolCall is one completed tools/call.
	EventToolCall EventKind = "tool_call"
	// EventToolUnknown is a call for a name this server does not serve.
	EventToolUnknown EventKind = "tool_unknown"
	// EventToolRefused is a call this token's scope does not permit.
	EventToolRefused EventKind = "tool_refused"
	// EventToolPanic is a tool that panicked and was converted to a failed result.
	EventToolPanic EventKind = "tool_panic"
	// EventHandshake is a completed initialize.
	EventHandshake EventKind = "handshake"
	// EventTransportRefusal is a request refused before dispatch: a bad Origin, a missing
	// or wrong token, an unsupported protocol version, an unknown session.
	//
	// This is the security-relevant one. It is the only signal that something is trying
	// the control port, and without it a token-guessing attempt leaves no trace at all.
	EventTransportRefusal EventKind = "transport_refusal"
)

// Event is one thing worth recording.
//
// # Every field is safe to write to a log
//
// That is the invariant this type exists to hold, and it holds by construction:
//
//   - Kind, Outcome and Reason are constants declared in this package. A call site
//     chooses among them; it cannot compose one out of request data.
//   - Tool is a registered tool name — public API — or the name a caller asked for, which
//     is the caller's own string and the only thing that makes an unknown-tool line
//     useful. A tool name is not a credential.
//   - ArgNames is argument *keys*, never values. See argumentNames.
//   - Status and Duration are numbers.
//   - Remote is a peer address, present only on a transport refusal, because an auth
//     failure nobody can attribute is an auth failure nobody can act on.
//
// There is deliberately no field for a body, an argument value, a path, or a token.
type Event struct {
	Kind     EventKind
	Tool     string
	Outcome  string
	Reason   string
	ArgNames []string
	Status   int
	Duration time.Duration
	Remote   string
}

// Outcome values for EventToolCall.
const (
	OutcomeOK    = "ok"
	OutcomeError = "error"
)

// Logger receives events. Implementations must be safe for concurrent use: the network
// transport serves from a net/http goroutine per request.
type Logger interface {
	LogEvent(Event)
}

// argumentNames lists the keys of a tools/call arguments object, sorted.
//
// # Why names and not values, even redacted
//
// Tool arguments carry credentials as a matter of course: `password` and `token` on the
// fleet and live tools, `service_token` on provisioning, `token` on simulate. A redacting
// formatter would need a list of sensitive key names, and that list is wrong the moment
// somebody adds a field — the failure being a secret in a log file, found later by
// whoever reads the log.
//
// Names alone answer the question a log is for: which tool ran, with which inputs
// supplied. "breeze_get_logs with service_url, token" is the useful line. The value of
// token is not part of the useful line at any verbosity.
//
// Malformed arguments yield nil rather than an error. This is a diagnostic path; the
// dispatcher reports a malformed body itself, and a logger that failed a call would be
// worse than one that logged less.
func argumentNames(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil
	}
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// SetLogger attaches a logger to this server's tool dispatch. nil disables logging, which
// is the default: a library that logs unasked is a library that writes to somebody else's
// stdout.
func (s *Server) SetLogger(l Logger) { s.logger = l }

// emit sends an event when a logger is attached.
//
// A method rather than an inline nil check at each site, so the check exists once and the
// cost of logging being disabled is a single comparison.
func (s *Server) emit(ev Event) {
	if s.logger != nil {
		s.logger.LogEvent(ev)
	}
}

// SetLogger attaches a logger to the transport, for refusals that never reach dispatch.
//
// It also installs the same logger on the Server behind it, so one call covers both
// halves: a caller wanting a log wants tool calls *and* refusals, and making that two
// calls would mean every caller could get half of it.
func (n *NetworkServer) SetLogger(l Logger) {
	n.logger = l
	if n.dispatch != nil {
		n.dispatch.SetLogger(l)
	}
}

// refuse records a pre-dispatch refusal.
//
// reason must be one of the constants below: it is written to a log verbatim, and an
// interpolated one would be exactly the hole this file exists to close.
func (n *NetworkServer) refuse(status int, reason, remote string) {
	if n.logger != nil {
		n.logger.LogEvent(Event{
			Kind:   EventTransportRefusal,
			Status: status,
			Reason: reason,
			Remote: remote,
		})
	}
}

// Refusal reasons — one per branch in serveMCP, so a log line names the check that
// refused without quoting anything the caller sent.
const (
	ReasonMethodNotAllowed = "method not allowed on the MCP endpoint"
	ReasonOriginRejected   = "Origin not in the allowlist"
	ReasonNoToken          = "missing or invalid bearer token"
	ReasonBadAccept        = "Accept excludes application/json"
	ReasonBadContentType   = "Content-Type is not application/json"
	ReasonBadProtocol      = "unsupported MCP-Protocol-Version"
	ReasonBodyUnreadable   = "request body could not be read"
	ReasonBodyTooLarge     = "request body exceeds the size limit"
	ReasonSessionMissing   = "no session header on a request that needs one"
	ReasonSessionUnknown   = "session id is not live"
	ReasonSessionFailed    = "a session id could not be generated"
)
