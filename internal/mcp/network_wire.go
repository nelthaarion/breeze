package mcp

// network_wire.go — header validation and message classification.
//
// Two jobs, both small enough that inlining them into the handler would have
// hidden what they are: checking the headers MCP requires, and answering two
// questions about a message body that the transport needs but the dispatcher
// does not expose — "is this the handshake?" and "does this expect a reply?".
//
// The classification is deliberately shallow. It decodes only method and id, and
// it never decides whether a message is *valid*: that is rpc's job, and a second
// opinion here would be a second implementation of JSON-RPC that could disagree
// with the first. An unparseable body classifies as neither an initialize nor a
// notification, so it reaches the dispatcher and is answered with the
// dispatcher's own -32700 — the correct error, and the one stdio would produce.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// codeTransport is the JSON-RPC error code used for a transport-level refusal.
//
// -32000 is the top of the implementation-defined server-error range, which is
// where a refusal that is not one of JSON-RPC's predefined conditions belongs:
// the message was never dispatched, so -32600 (invalid request) and -32601
// (method not found) would both be claims about content this layer never looked
// at. The HTTP status is the primary signal; this exists so the body is
// well-formed JSON-RPC rather than prose.
const codeTransport = -32000

// assumedProtocolVersion is what an absent MCP-Protocol-Version means. The
// specification names this exact value for this exact case.
const assumedProtocolVersion = "2025-03-26"

// checkAccept enforces the Accept requirement.
//
// The specification requires a client to list both application/json and
// text/event-stream. Only the first is enforced here, because this server never
// replies with an event stream: refusing a client that omitted text/event-stream
// would reject a working conversation over a content type that will never be
// sent. A missing Accept is accepted for the same reason — many clients omit it
// and every one of them can read JSON. What is refused is an Accept that
// positively excludes JSON, which is a client asking for something this server
// cannot produce.
func checkAccept(accept string) error {
	accept = strings.TrimSpace(accept)
	if accept == "" {
		return nil
	}
	lower := strings.ToLower(accept)
	if strings.Contains(lower, "application/json") ||
		strings.Contains(lower, "application/*") ||
		strings.Contains(lower, "*/*") {
		return nil
	}
	return fmt.Errorf("this endpoint replies with application/json; Accept was %q. "+
		"MCP clients should send: Accept: application/json, text/event-stream", accept)
}

// checkContentType requires a JSON body.
//
// A missing Content-Type is refused rather than assumed: on a POST it is the one
// header that says what the bytes are, and guessing would mean accepting a
// browser form submission as a JSON-RPC message. Parameters are permitted, so
// "application/json; charset=utf-8" passes.
func checkContentType(contentType string) error {
	base := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	switch base {
	case "application/json":
		return nil
	case "":
		return fmt.Errorf("a Content-Type of application/json is required on POST")
	default:
		return fmt.Errorf("Content-Type %q is not supported; send application/json", base)
	}
}

// checkProtocolVersion enforces the MCP-Protocol-Version header.
//
// An absent header is not an error: the specification says to assume 2025-03-26,
// and a client pinned to 2024-11-05 predates the header entirely. An unsupported
// value is 400, which is also specified, and the supported set is listed in the
// message so the client can pick one rather than guess again.
func checkProtocolVersion(version string) error {
	version = strings.TrimSpace(version)
	if version == "" {
		version = assumedProtocolVersion
	}
	if supportedProtocolVersions[version] {
		return nil
	}
	return fmt.Errorf("MCP-Protocol-Version %q is not supported; this server speaks %s. It is a "+
		"handshake-based server: it implements initialize and "+sessionHeader+", not the stateless "+
		"per-request _meta of 2026-07-28", version, supportedVersionList())
}

// supportedVersionList renders the accepted revisions, newest first.
//
// Sorted rather than iterated: a client reading this wants the newest usable one
// first, and map order would make the message unstable between calls and
// untestable.
func supportedVersionList() string {
	versions := make([]string, 0, len(supportedProtocolVersions))
	for v := range supportedProtocolVersions {
		versions = append(versions, v)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(versions)))
	return strings.Join(versions, ", ")
}

// ─── message classification ──────────────────────────────────────────────────

// wireProbe is the part of a message this transport needs to see.
type wireProbe struct {
	Method string          `json:"method"`
	ID     json.RawMessage `json:"id"`
}

// probe decodes those two fields, reporting failure as "not a single message".
//
// A batch — a JSON array — probes as neither an initialize nor a notification,
// which is correct: a batch cannot be the handshake, and whether it expects a
// reply is rpc's business.
func probe(body []byte) (wireProbe, bool) {
	var p wireProbe
	if err := json.Unmarshal(body, &p); err != nil {
		return wireProbe{}, false
	}
	return p, true
}

// isInitialize reports whether a message is the handshake request.
//
// The id is required: an "initialize" with no id is a notification, gets no
// response, and minting a session for it would produce an id the client never
// receives — followed by a 400 on its next request for a session it was never
// told about.
func isInitialize(body []byte) bool {
	p, ok := probe(body)
	return ok && p.Method == "initialize" && hasID(p.ID)
}

// isNotification reports whether a message is a notification: a method with no
// id. Anything the probe could not read is not treated as one, so it reaches the
// dispatcher and is answered there.
func isNotification(body []byte) bool {
	p, ok := probe(body)
	if !ok || p.Method == "" {
		return false
	}
	return !hasID(p.ID)
}

// hasID reports whether an id member is present and not null. JSON-RPC treats a
// null id as absent for this purpose, and so does rpc's dispatcher.
func hasID(id json.RawMessage) bool {
	return len(id) > 0 && string(id) != "null"
}

// writeRPCError writes a transport-level refusal as a JSON-RPC error response.
//
// The id is null because there may be no message at all — a refused GET has no
// body to take an id from — and because a refusal that happened before dispatch
// cannot honestly claim to be the answer to a particular request. This is the
// shape the specification permits for these rejections.
func writeRPCError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      nil,
		"error": map[string]any{
			"code":    codeTransport,
			"message": message,
		},
	})
	if err != nil {
		// message is the only variable part and it is always a string, so this is
		// unreachable; writing the status alone still beats a panic in a handler.
		return
	}
	_, _ = w.Write(payload)
}
