package mcp

// network.go — the MCP Streamable HTTP transport for this server.
//
// # What this is, and what it is not
//
// This is a second transport over the *same* Server: the tool table, the
// schemas, the dispatcher and the three MCP methods are untouched, and every
// request that arrives here is handed to the identical rpc.Server.Handle call
// that rpc/stdio.go uses. Nothing about a tool's behaviour can differ between
// the two transports, because there is only one implementation of a tool.
//
// # Which revision of the transport
//
// Verified against the published specification rather than from memory:
//
//   - modelcontextprotocol.io/specification/2025-06-18/basic/transports
//   - modelcontextprotocol.io/specification/2025-11-25/basic/transports
//   - modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http
//
// The current revision at the time of writing is 2026-07-28, and it changed the
// transport in two ways that matter here: it removed protocol-level sessions and
// the Mcp-Session-Id header, and it removed the initialize /
// notifications/initialized handshake in favour of per-request `_meta` version
// declaration. This server's vocabulary is handshake-based — it answers
// initialize and reports a negotiated protocolVersion — which the 2026-07-28
// specification calls a "legacy" server.
//
// Serving the 2026-07-28 shape would mean rewriting that vocabulary, which is
// out of scope: the stdio behaviour must not change and the existing tools must
// not be touched. So what is implemented here is Streamable HTTP exactly as
// specified for the handshake-based revisions — 2025-03-26, 2025-06-18 and
// 2025-11-25, whose transport wording for POST, sessions and the protocol header
// is identical — and the 2026-07-28 revision's own backwards-compatibility rules
// are followed for the mechanisms it removed: a GET or DELETE from a modern
// client is answered 405 Method Not Allowed, which is what that revision
// prescribes for a server that does not implement them.
//
// # Sessions, and why GET is 405
//
// A session id is minted on a successful initialize and required afterwards, per
// §Session Management. The standalone GET stream is refused with 405, which the
// specification explicitly permits: this server never initiates a request or a
// notification of its own, so a stream it may only write to would be an open
// connection that produces nothing.
//
// # Security
//
// The specification's three transport security requirements are all mandatory
// here rather than optional, because what is exposed is not data — it is a code
// generator with filesystem access:
//
//   - Origin is validated, and an unrecognised Origin is 403. A browser is not a
//     client of this server, so a request carrying an Origin at all is either a
//     DNS-rebinding attempt or a proxy that has to be named explicitly.
//   - The default bind is 127.0.0.1. --host is the only way to widen it.
//   - Authentication is not optional. Every request must carry a bearer token,
//     including initialize. There is no anonymous surface at all.
//
// The discipline is the one mcp_route.go established for Auto-MCP: fail closed,
// and reject with a structured JSON-RPC error rather than an empty body, so the
// caller learns which refusal it hit. That file protects an application's routes
// and this protects the control plane itself, but a rejection a client cannot
// read is equally useless in both.

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// DefaultEndpointPath is the MCP endpoint. The specification requires a single
// path; this is it, and it is not configurable, so every client configuration
// and every document in this repository can name the same URL.
const DefaultEndpointPath = "/mcp"

// DefaultNetworkHost is the bind address when --host is not given.
//
// Loopback, per the specification's second security requirement and because the
// alternative is a code generator reachable from the LAN. A token is mandatory
// even here; the narrow bind is the second lock, not the first.
const DefaultNetworkHost = "127.0.0.1"

// maxRequestBytes bounds one POST body. It matches rpc's stdio line cap, so a
// message that is too large is too large on both transports.
const maxRequestBytes = 8 << 20

// maxSessions bounds the session table.
//
// A session costs a map entry and nothing else — MCP sessions here carry no
// state — but an authenticated client that reconnects in a loop should not be
// able to grow a map without bound. Reaching the cap evicts the oldest session,
// which a client recovers from by re-initializing: that is the documented
// meaning of the 404 this produces.
const maxSessions = 1024

const (
	sessionHeader  = "Mcp-Session-Id"
	protocolHeader = "MCP-Protocol-Version"
	bearerPrefix   = "Bearer "
)

// supportedProtocolVersions are the MCP-Protocol-Version values this transport
// accepts.
//
// The set is the handshake-based revisions. protocolVersion — what initialize
// reports, and what a well-behaved client echoes back here — is the first entry.
// The later three are accepted because a client that speaks them sends its own
// revision in this header before it has seen the handshake result, and answering
// 400 to a client this server can in fact serve would be a refusal with no
// cause.
//
// 2026-07-28 is deliberately absent: a client speaking it expects no handshake
// and no session, which this server does not provide, and the specification's
// own answer for that case is to say so rather than to half-serve it.
var supportedProtocolVersions = map[string]bool{
	protocolVersion: true, // 2024-11-05, this server's negotiated revision
	"2025-03-26":    true,
	"2025-06-18":    true,
	"2025-11-25":    true,
}

// NetworkConfig is how a network-mode instance is configured.
type NetworkConfig struct {
	// Mode is the server kind: ModeGenerator or ModeAppRuntime. Required, with no
	// default — see mode.go for why neither value is a safe fallback.
	Mode ServerMode

	// Host is the bind address. Empty means DefaultNetworkHost.
	Host string

	// Port is the TCP port. Zero asks the operating system for one, which is
	// what tests use; a real deployment names it.
	Port int

	// Token is the bearer token every request must carry. Empty means one is
	// generated, and NewNetworkServer reports it so the caller can print it
	// once. It is never regenerated per request or per session.
	Token string

	// Scope is what the token may do. The zero value is unscoped — every
	// capability — which is the documented default and today's behaviour.
	//
	// Scope is set when a token is minted and never changes for its lifetime, which
	// is what lets initialize report it as a one-time snapshot rather than making an
	// agent poll for it.
	Scope Scope

	// AllowedOrigins extends the Origin allowlist beyond loopback. Each entry is
	// an exact scheme://host[:port] string as a browser would send it. The
	// single entry "*" disables Origin checking, which is then a deliberate act
	// visible in a process listing.
	AllowedOrigins []string
}

// NetworkServer serves one Server over MCP Streamable HTTP.
type NetworkServer struct {
	rpc rpcHandler

	token string

	origins   map[string]bool
	anyOrigin bool

	// mu guards sessions. Sessions are created by initialize and read by every
	// subsequent request, from whichever net/http goroutine served it.
	mu       sync.Mutex
	sessions map[string]time.Time

	listener net.Listener
	http     *http.Server

	// scope is the capability set of the token this transport authenticates.
	//
	// Held here as well as on the Server because GET /mcp/features answers without
	// dispatching a JSON-RPC message, so it needs the value at the transport layer.
	// Both are set from one NetworkConfig field in NewNetworkServer, so there is no
	// path by which they disagree.
	scope Scope

	// serverKind is reported by GET /mcp/features, so a human checking a token does
	// not have to open an MCP session to learn which kind of server holds it.
	serverKind ServerMode

	// toolNames is what this token may call, sorted, for the same endpoint.
	toolNames []string

	// logger receives an Event for each pre-dispatch refusal, or nil. Refusals are the
	// events dispatch cannot see: a wrong token never reaches a tool.
	logger Logger

	// dispatch is the Server whose tools this transport serves, held so SetLogger can
	// reach it.
	//
	// The transport sees refusals and the dispatcher sees calls, and an operator asking
	// for a log wants both — so one SetLogger has to install both, or every caller would
	// have to remember two. nil when the transport was built over a bare rpcHandler,
	// which is what the transport-only tests do.
	dispatch *Server
}

// rpcHandler is the narrow view of the dispatcher this file needs.
//
// Declared as an interface so a test can assert what the transport does with a
// dispatcher's output — 202 for no bytes, 200 for bytes — without booting the
// full tool table, and so nothing here can reach past Handle into rpc's
// internals.
type rpcHandler interface {
	Handle(msg []byte) []byte
}

// NewNetworkServer builds a network-mode server.
//
// The token it returns is the one in force: the caller's, if it supplied one, or
// a freshly generated one otherwise. It is returned rather than only stored
// because a generated token that is never shown is a server nobody can talk to.
func NewNetworkServer(srv *Server, cfg NetworkConfig) (*NetworkServer, string, error) {
	if srv == nil {
		return nil, "", errors.New("mcp: a network server needs a Server to serve")
	}
	// Validated here as well as in NewServerForMode because a caller can reach this
	// with a hand-built Server, and a transport that served a mode-less server would
	// answer initialize with no breezeServerKind — the exact ambiguity Part 9
	// removes.
	if err := cfg.Mode.validate(); err != nil {
		return nil, "", err
	}
	// The Server and the transport must agree. They can disagree only if a caller
	// built one with NewServerForMode and passed a different Mode here, which is a
	// programming error whose symptom would otherwise be a handshake that advertises
	// one kind while serving the other's tools.
	if srv.mode != ModeUnset && srv.mode != cfg.Mode {
		return nil, "", fmt.Errorf("mcp: server was built for mode %q but the transport was "+
			"configured for %q; these must match", srv.mode, cfg.Mode)
	}
	if srv.mode == ModeUnset {
		srv.mode = cfg.Mode
	}
	// The transport authenticated the token, so the transport is what knows the scope
	// that came with it. Pushing it onto the Server here means tools/list, tools/call
	// and the initialize payload all read one value.
	srv.SetScope(cfg.Scope)

	ns, token, err := newNetworkServer(srv.RPCServer(), cfg)
	if err != nil {
		return nil, "", err
	}
	// Kept so GET /mcp/features can answer from the same value the handshake reports,
	// rather than from a second copy that could drift.
	ns.scope = cfg.Scope
	ns.serverKind = cfg.Mode
	ns.toolNames = srv.visibleNames()
	ns.dispatch = srv
	return ns, token, nil
}

func newNetworkServer(handler rpcHandler, cfg NetworkConfig) (*NetworkServer, string, error) {
	token := strings.TrimSpace(cfg.Token)
	if token == "" {
		generated, err := NewToken()
		if err != nil {
			return nil, "", err
		}
		token = generated
	}

	ns := &NetworkServer{
		rpc:      handler,
		token:    token,
		origins:  defaultOrigins(cfg.Host, cfg.Port),
		sessions: make(map[string]time.Time, 8),
	}
	for _, origin := range cfg.AllowedOrigins {
		origin = strings.TrimSpace(origin)
		switch origin {
		case "":
		case "*":
			ns.anyOrigin = true
		default:
			ns.origins[strings.ToLower(strings.TrimSuffix(origin, "/"))] = true
		}
	}
	return ns, token, nil
}

// defaultOrigins is the loopback allowlist.
//
// Both loopback spellings are included, with and without the configured port,
// because a client that sends an Origin at all sends whichever of these its own
// URL produced, and the difference between http://localhost:2000 and
// http://127.0.0.1:2000 is not a security boundary.
func defaultOrigins(host string, port int) map[string]bool {
	hosts := []string{"localhost", "127.0.0.1", "[::1]"}
	if h := strings.TrimSpace(host); h != "" && h != "0.0.0.0" && h != "::" {
		hosts = append(hosts, strings.ToLower(h))
	}

	out := make(map[string]bool, len(hosts)*4)
	for _, h := range hosts {
		for _, scheme := range []string{"http://", "https://"} {
			out[scheme+h] = true
			if port > 0 {
				out[fmt.Sprintf("%s%s:%d", scheme, h, port)] = true
			}
		}
	}
	return out
}

// ─── lifecycle ───────────────────────────────────────────────────────────────

// Endpoint is the path the MCP endpoint is served on.
func (n *NetworkServer) Endpoint() string { return DefaultEndpointPath }

// Handler returns the HTTP handler, so a caller can mount it and a test can
// drive it without binding a port.
func (n *NetworkServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(DefaultEndpointPath, n.serveMCP)
	// The permissions convenience, network mode only — there is no HTTP surface on
	// stdio to mount it on. Registered before the catch-all so the more specific
	// pattern wins regardless of ServeMux ordering rules.
	mux.HandleFunc(featuresPath, n.serveFeatures)
	// Anything else answers with a JSON-RPC body rather than net/http's HTML, so
	// a client that has the wrong URL is told in the language it speaks.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeRPCError(
			w,
			http.StatusNotFound,
			fmt.Sprintf(
				"no MCP endpoint at %s; it is served at %s",
				r.URL.Path,
				DefaultEndpointPath,
			),
		)
	})
	return mux
}

// Listen binds the configured address without serving, so a caller can learn the
// port — which matters when Port was zero — before traffic starts.
func (n *NetworkServer) Listen(cfg NetworkConfig) error {
	host := strings.TrimSpace(cfg.Host)
	if host == "" {
		host = DefaultNetworkHost
	}

	ln, err := net.Listen("tcp", net.JoinHostPort(host, fmt.Sprint(cfg.Port)))
	if err != nil {
		return fmt.Errorf("mcp: cannot listen on %s:%d: %w", host, cfg.Port, err)
	}
	n.listener = ln
	n.http = &http.Server{
		Handler: n.Handler(),
		// A tool call can legitimately take minutes (go build, go test), so
		// there is no write timeout: cutting a verify_project response off
		// half-way would report a working project as a broken transport. Reads
		// are bounded instead, which is where an unauthenticated peer can
		// misbehave.
		ReadHeaderTimeout: 10 * time.Second,
	}
	return nil
}

// Scope reports the capability set of the token this transport authenticates.
//
// Exported so a command can assert the flag it parsed actually arrived, and so an
// embedding application can log what it granted without reconstructing it.
func (n *NetworkServer) Scope() Scope { return n.scope }

// Kind reports whether this is a generator or an app-runtime server — the same value
// initialize returns as breezeServerKind.
func (n *NetworkServer) Kind() ServerMode { return n.serverKind }

// Addr reports the bound address, or nil before Listen.
func (n *NetworkServer) Addr() net.Addr {
	if n.listener == nil {
		return nil
	}
	return n.listener.Addr()
}

// Serve serves until Close. A closed listener is a clean stop, not a failure.
func (n *NetworkServer) Serve() error {
	if n.listener == nil {
		return errors.New("mcp: Serve called before Listen")
	}
	if err := n.http.Serve(n.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Close stops serving.
func (n *NetworkServer) Close() error {
	if n.http != nil {
		return n.http.Close()
	}
	if n.listener != nil {
		return n.listener.Close()
	}
	return nil
}
