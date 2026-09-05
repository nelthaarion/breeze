package mcp

// network_features.go — GET /mcp/features, the human-facing permissions check.
//
// # Why this exists at all when initialize already reports scope
//
// It is a convenience, and deliberately not a second source of truth. `initialize`'s
// payload is the mechanism an agent uses; this answers the same question for a person
// with curl, or for external tooling that wants to verify a token without implementing
// an MCP handshake — three round trips and a session id to learn one fact.
//
// The values come from the same NetworkServer fields the handshake is built from, so
// the two cannot disagree. A test asserts that by comparing them.
//
// # Why it is not an MCP tool
//
// A `list_features` tool would be a third way to get the same answer, and the one most
// likely to drift: a tool result is shaped by whoever wrote the tool, while the
// handshake payload is shaped by the protocol. Part 8 says not to build it, and the
// reason holds — an agent that has completed a handshake already has this.
//
// # Why network mode only
//
// There is no HTTP surface on stdio or in-process-without-a-port, so there is nothing
// to mount it on. That is not a gap: the handshake carries the same information on
// every transport, so nothing is unreachable — only less convenient to reach by hand.

import (
	"encoding/json"
	"net/http"
)

// featuresPath is where the convenience endpoint lives.
//
// Under the same /mcp prefix as the protocol endpoint so one allowlist or proxy rule
// covers both, and named for what it reports rather than for the token, because a URL
// containing "token" invites someone to put one in it.
//
// FeaturesPath is the exported spelling, so a command can print the URL without
// hardcoding a path this package owns.
const (
	FeaturesPath = DefaultEndpointPath + "/features"
	featuresPath = FeaturesPath
)

// featuresResponse is the flat JSON this endpoint returns.
//
// Flat on purpose: it is read by a person or by a shell script, and a nested object
// would mean jq gymnastics for a question with a one-line answer.
type featuresResponse struct {
	// ServerKind is "generator" or "app-runtime", the same value initialize reports as
	// breezeServerKind.
	ServerKind string `json:"server_kind"`

	// Granted is what this token may do, sorted.
	Granted []string `json:"granted"`

	// Known is every capability this server understands, sorted, so Granted reads as a
	// subset rather than as an unanchored list.
	Known []string `json:"known"`

	// Scoped distinguishes an unscoped token from one minted with every capability.
	Scoped bool `json:"scoped"`

	// Tools is what this token may actually call, sorted. Included because "which
	// capabilities" and "which tools" are different questions, and an operator
	// checking a credential usually wants the second.
	Tools []string `json:"tools"`

	// Note points at the authoritative mechanism, so nobody builds automation on this
	// endpoint when the handshake would serve them better.
	Note string `json:"note"`
}

// featuresNote is that pointer, spelled once.
const featuresNote = "This endpoint is a convenience for humans and external tooling. " +
	"An MCP client learns the same information from initialize's breezeCapabilities and " +
	"breezeServerKind fields, which are available on every transport including stdio."

// serveFeatures answers GET /mcp/features.
//
// Authenticated, with the same bearer token and the same constant-time comparison as
// the protocol endpoint — the answer describes a credential's privileges, which is
// exactly the sort of thing an unauthenticated caller should not be able to enumerate.
// Origin is checked first, for the same reason it is checked first there.
func (n *NetworkServer) serveFeatures(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeRPCError(w, http.StatusMethodNotAllowed,
			r.Method+" is not supported on "+featuresPath+"; it answers GET only")
		return
	}

	if origin := r.Header.Get("Origin"); origin != "" && !n.originAllowed(origin) {
		writeRPCError(w, http.StatusForbidden,
			"Origin "+origin+" is not allowed; pass --allow-origin to permit it")
		return
	}

	if !n.authorised(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="breeze-mcp"`)
		writeRPCError(w, http.StatusUnauthorized,
			"this endpoint requires the same bearer token as "+DefaultEndpointPath+
				": send Authorization: Bearer <token>")
		return
	}

	body, err := json.MarshalIndent(n.featuresReport(), "", "  ")
	if err != nil {
		// Unreachable for these types; reported rather than ignored so a future field
		// that is not serialisable does not fail silently.
		writeRPCError(w, http.StatusInternalServerError, "the report could not be encoded")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(append(body, '\n'))
}

// featuresReport builds the response from the transport's own fields.
//
// Exported-in-spirit as a separate method so a test can compare it against the
// handshake payload without going over HTTP.
func (n *NetworkServer) featuresReport() featuresResponse {
	return featuresResponse{
		ServerKind: string(n.serverKind),
		Granted:    capabilityNames(n.scope.Granted()),
		Known:      capabilityNames(KnownCapabilities()),
		Scoped:     n.scope.IsScoped(),
		Tools:      n.toolNames,
		Note:       featuresNote,
	}
}
