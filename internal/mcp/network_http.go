package mcp

// network_http.go — the request half of the Streamable HTTP transport.
//
// The order of the checks below is the interesting part, and it is deliberate:
//
//  1. Method. GET and DELETE are 405 before anything else, because they are
//     answered identically whether or not the caller is authenticated, and
//     saying so first keeps the auth path from having to reason about them.
//  2. Origin. 403 for an unrecognised one. This precedes authentication so a
//     DNS-rebinding attempt from a browser is refused without the page ever
//     learning whether it also had a valid token.
//  3. Authentication. 401 with WWW-Authenticate. Nothing past this point runs
//     for an anonymous caller — including initialize.
//  4. Accept and Content-Type. Protocol shape, checked after auth so an
//     unauthenticated caller cannot distinguish a wrong header from a wrong
//     token.
//  5. Protocol version. 400 for an unsupported one, per the specification.
//  6. Session. 400 when required and absent, 404 when unknown.
//
// Every refusal carries a JSON-RPC error object with no id, which is the shape
// the specification permits for a transport-level rejection, so a client sees a
// reason rather than an empty body it has to infer from the status code.

import (
	"crypto/subtle"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// serveMCP is the MCP endpoint.
func (n *NetworkServer) serveMCP(w http.ResponseWriter, r *http.Request) {
	// Recorded once. Every refusal below reports it, because a refusal nobody can
	// attribute is a refusal nobody can act on — and RemoteAddr is the only identity an
	// unauthenticated caller has.
	remote := r.RemoteAddr

	switch r.Method {
	case http.MethodPost:
		// The only method this transport implements.
	case http.MethodGet, http.MethodDelete:
		// Permitted by the handshake-era specification for a server that offers
		// neither the standalone SSE stream nor client-terminated sessions, and
		// prescribed by the 2026-07-28 revision for a server that no longer
		// implements them.
		w.Header().Set("Allow", http.MethodPost)
		n.refuse(http.StatusMethodNotAllowed, ReasonMethodNotAllowed, remote)
		writeRPCError(w, http.StatusMethodNotAllowed,
			r.Method+" is not supported on this endpoint: this server has no server-initiated "+
				"stream and does not accept client session termination. Send JSON-RPC as POST.")
		return
	default:
		w.Header().Set("Allow", http.MethodPost)
		n.refuse(http.StatusMethodNotAllowed, ReasonMethodNotAllowed, remote)
		writeRPCError(w, http.StatusMethodNotAllowed, r.Method+" is not supported; use POST")
		return
	}

	if origin := r.Header.Get("Origin"); origin != "" && !n.originAllowed(origin) {
		// 403, per the specification's first security requirement. The rejected
		// value is echoed because the legitimate cause is a proxy or a dashboard
		// on an address the operator has not allowlisted, and that is only
		// fixable if they can see which string to add.
		//
		// The log line does not carry the origin: it is caller-supplied and would be
		// the one place an attacker chooses what gets written to a log file.
		n.refuse(http.StatusForbidden, ReasonOriginRejected, remote)
		writeRPCError(w, http.StatusForbidden,
			fmt.Sprintf("Origin %q is not allowed; pass --allow-origin to permit it", origin))
		return
	}

	if !n.authorised(r) {
		// The scheme is advertised so a client knows what kind of credential is
		// missing. Nothing about the expected value appears in the response.
		//
		// This is the refusal worth having in a log. A run of these from one address is
		// a token-guessing attempt, and it is otherwise entirely invisible.
		n.refuse(http.StatusUnauthorized, ReasonNoToken, remote)
		w.Header().Set("WWW-Authenticate", `Bearer realm="breeze-mcp"`)
		writeRPCError(w, http.StatusUnauthorized,
			"this endpoint requires a bearer token: send Authorization: Bearer <token>. "+
				"The token is printed once at startup, or set with --token / BREEZE_MCP_TOKEN.")
		return
	}

	if err := checkAccept(r.Header.Get("Accept")); err != nil {
		n.refuse(http.StatusNotAcceptable, ReasonBadAccept, remote)
		writeRPCError(w, http.StatusNotAcceptable, err.Error())
		return
	}
	if err := checkContentType(r.Header.Get("Content-Type")); err != nil {
		n.refuse(http.StatusUnsupportedMediaType, ReasonBadContentType, remote)
		writeRPCError(w, http.StatusUnsupportedMediaType, err.Error())
		return
	}
	if err := checkProtocolVersion(r.Header.Get(protocolHeader)); err != nil {
		// The specification is explicit that this case is 400.
		n.refuse(http.StatusBadRequest, ReasonBadProtocol, remote)
		writeRPCError(w, http.StatusBadRequest, err.Error())
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
	if err != nil {
		n.refuse(http.StatusBadRequest, ReasonBodyUnreadable, remote)
		writeRPCError(w, http.StatusBadRequest, "the request body could not be read: "+err.Error())
		return
	}
	if len(body) > maxRequestBytes {
		n.refuse(http.StatusRequestEntityTooLarge, ReasonBodyTooLarge, remote)
		writeRPCError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("the message exceeds the %d byte limit", maxRequestBytes))
		return
	}

	// The session rule depends on what the message is, so it is settled after the
	// body is in hand but before dispatch: an initialize creates a session,
	// everything else must present one.
	newSession, herr := n.resolveSession(r, body)
	if herr != nil {
		n.refuse(herr.status, herr.reason, remote)
		writeRPCError(w, herr.status, herr.message)
		return
	}

	// One call, the same one rpc/stdio.go makes. Everything above is framing.
	out := n.rpc.Handle(body)

	if newSession != "" {
		// Per §Session Management, the id is returned on the initialize response
		// and the client repeats it afterwards.
		w.Header().Set(sessionHeader, newSession)
	}

	if len(out) == 0 {
		// A notification, or a batch of them. The specification requires 202 with
		// no body, which is also what rpc reports by returning no bytes: the two
		// agree without this layer having to parse the message to find out.
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// application/json rather than text/event-stream. Both are permitted for a
	// POST carrying a request; a single JSON object is the honest one here,
	// because this server produces exactly one response per request and never
	// interleaves progress notifications. An SSE stream carrying one event would
	// be a stream in name only, and it would hold a connection open for the
	// duration of a `go test` run to no benefit.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// originAllowed reports whether an Origin header may proceed.
func (n *NetworkServer) originAllowed(origin string) bool {
	if n.anyOrigin {
		return true
	}
	return n.origins[strings.ToLower(strings.TrimSuffix(strings.TrimSpace(origin), "/"))]
}

// authorised reports whether the request carries the bearer token.
//
// The comparison is constant-time. A byte-by-byte early return would leak the
// token's prefix to a caller willing to time enough attempts, and the token is
// the only thing standing between a network peer and a code generator.
func (n *NetworkServer) authorised(r *http.Request) bool {
	header := r.Header.Get("Authorization")
	if len(header) <= len(bearerPrefix) ||
		!strings.EqualFold(header[:len(bearerPrefix)], bearerPrefix) {
		return false
	}
	presented := strings.TrimSpace(header[len(bearerPrefix):])
	return subtle.ConstantTimeCompare([]byte(presented), []byte(n.token)) == 1
}

// httpError is a refusal with the status that goes with it.
//
// reason is the fixed constant for the log; message is the sentence for the caller. They
// are separate fields because the caller's message may quote what was presented and the
// log line must not.
type httpError struct {
	status  int
	reason  string
	message string
}

// resolveSession applies §Session Management to one message.
//
// It returns the newly minted session id when the message was an initialize, and
// an httpError when the message needed a session it did not have.
func (n *NetworkServer) resolveSession(r *http.Request, body []byte) (string, *httpError) {
	presented := strings.TrimSpace(r.Header.Get(sessionHeader))

	if isInitialize(body) {
		id, err := newSessionID()
		if err != nil {
			return "", &httpError{http.StatusInternalServerError, ReasonSessionFailed, err.Error()}
		}
		n.rememberSession(id)
		return id, nil
	}

	if presented == "" {
		// A notification is allowed through without one: notifications/initialized
		// follows initialize immediately, and a client may send it before it has
		// read the response header carrying the id. Refusing it would break the
		// handshake over a message that has no reply anyway.
		if isNotification(body) {
			return "", nil
		}
		return "", &httpError{
			http.StatusBadRequest, ReasonSessionMissing,
			"this request needs the " + sessionHeader + " header returned by initialize",
		}
	}

	if !n.knownSession(presented) {
		// 404 is the specified answer, and it has a specific meaning to a client:
		// start a new session by sending initialize without an id.
		return "", &httpError{
			http.StatusNotFound, ReasonSessionUnknown,
			"this session is no longer valid; send initialize again without " + sessionHeader,
		}
	}
	return "", nil
}

// rememberSession records a session, evicting the least recently used when full.
func (n *NetworkServer) rememberSession(id string) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if len(n.sessions) >= maxSessions {
		var (
			oldest   string
			oldestAt time.Time
		)
		for candidate, at := range n.sessions {
			if oldest == "" || at.Before(oldestAt) {
				oldest, oldestAt = candidate, at
			}
		}
		delete(n.sessions, oldest)
	}
	n.sessions[id] = time.Now()
}

// knownSession reports whether a session id is live, and refreshes its timestamp
// so eviction removes the least recently used rather than the earliest created.
func (n *NetworkServer) knownSession(id string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()

	if _, ok := n.sessions[id]; !ok {
		return false
	}
	n.sessions[id] = time.Now()
	return true
}
