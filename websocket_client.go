package breeze

// websocket_client.go — the outbound half of Breeze's WebSocket support.
//
// # What was missing
//
// websocket.go and websocket_engine.go implement the inbound side completely: a
// server accepts a connection, upgrades it, and hands a *WSConn to a WSHandler.
// There was no way for a Breeze process to *dial* a WebSocket server. That is
// the blocker for a peer-to-peer layer, where every node is symmetric — it
// accepts connections from some peers and dials others, and the two kinds have to
// be interchangeable afterwards or the application ends up with two of every code
// path.
//
// # The connection type is shared, deliberately
//
// DialWS returns a *WSConn: the same type the upgrade handler passes to a
// handler. SendBinary, SendText, Send, Close and RemoteAddr are the same methods
// with the same semantics, so a peer table can hold one type and a dispatch loop
// cannot accidentally treat the two differently.
//
// Making that work needed one change to the existing type: WSConn.conn was
// gnet.Conn and is now wsRawConn, a three-method interface (AsyncWrite, Close,
// RemoteAddr) that gnet.Conn already satisfies. Nothing inbound changed
// behaviourally.
//
// The two roles are not symmetric on the wire, and that is confined to two
// places. A client MUST mask every frame it sends (§5.3) — buildWSFrameMasked —
// and MUST verify Sec-WebSocket-Accept on the handshake (§4.1), which a server
// never does. Everything after the handshake is the same framing code:
// parseWSFrame decodes both directions, since masking is its own inverse.
//
// # Why blocking dial and a goroutine per connection, not gnet
//
// client/client.go dials on gnet and documents why: one event-loop model for both
// directions. This does not, and the reason is that the two cases differ in a way
// that matters.
//
// gnet's client mode delivers reads to an event handler owned by the engine. A
// Breeze server already has an engine, whose handler is Breeze's own OnTraffic,
// dispatching by file descriptor to HTTP or WebSocket state. To dial on that same
// engine, an outbound connection would have to be registered into it and its
// frames routed through the same fd-keyed maps — meaning outbound connections
// would only work inside a running server, and DialWS could not be called by a
// process that does not serve. A P2P node's dial path frequently runs before, or
// entirely without, an inbound listener.
//
// Cost of the choice, since a consumer sizing resource use needs it stated: one
// goroutine per dialled connection, blocked in a read, plus one bufio.Reader
// (4 KiB). At a validator set's scale — tens to low hundreds of peers — that is a
// few hundred KiB and a few hundred goroutines, which the Go scheduler handles
// without noticing. At tens of thousands of outbound connections it would be the
// wrong design, and the fix then is to register dialled sockets into the server's
// engine with gnet's Enroll. The public shape does not change if that happens:
// DialWS returns a *WSConn either way.
//
// Sends do not use that goroutine. WSConn.Send goes through the adapter's
// AsyncWrite, which writes under a mutex, so any goroutine may send concurrently
// — the same guarantee the inbound side gives.
//
// # Out of scope
//
// Reconnection and backoff. A redial policy needs to know peer scoring, whether
// the address is still in the validator set, and how long to wait — none of which
// a connection primitive can answer. It belongs in the P2P layer's own loop,
// which is what OnClose is for.

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/panjf2000/gnet/v2"
)

// ─── Errors ───────────────────────────────────────────────────────────────────

var (
	// ErrWSHandshakeFailed reports a peer that answered something other than
	// 101 Switching Protocols. Wrapped with the status, so a 401 from an
	// authenticating peer is distinguishable from a 404 on the wrong path.
	ErrWSHandshakeFailed = errors.New("websocket: handshake failed")

	// ErrWSAcceptMismatch reports a 101 whose Sec-WebSocket-Accept does not
	// match the key sent. The peer is not a conforming endpoint — most often a
	// proxy answering on its behalf — and framing to it would produce bytes
	// nothing parses.
	ErrWSAcceptMismatch = errors.New("websocket: Sec-WebSocket-Accept mismatch")

	// ErrWSClosedByPeer reports a Close frame received from the peer.
	ErrWSClosedByPeer = errors.New("websocket: closed by peer")
)

// ─── Configuration ────────────────────────────────────────────────────────────

// WSClientConfig configures an outbound dial. The zero value is usable.
type WSClientConfig struct {
	// HandshakeTimeout bounds the TCP connect, the TLS handshake and the
	// upgrade exchange together. Zero means wsDefaultHandshakeTimeout.
	//
	// It does not bound anything afterwards: a P2P connection is long-lived and
	// idle for most of its life, so a read deadline would close healthy peers.
	// Liveness is Ping's job.
	HandshakeTimeout time.Duration

	// Header carries extra request headers — an Authorization header, a peer
	// identity, a network id. The handshake's own headers (Host, Upgrade,
	// Connection, Sec-WebSocket-Key, Sec-WebSocket-Version) cannot be
	// overridden: supplying them is always a mistake, and a wrong
	// Sec-WebSocket-Key would break the accept check that exists to catch a
	// non-WebSocket peer.
	Header map[string]string

	// Subprotocols are offered in Sec-WebSocket-Protocol, in preference order.
	// The peer's choice is available afterwards from WSConn.Subprotocol.
	//
	// A peer that selects nothing is not an error here. Whether an unnegotiated
	// subprotocol is acceptable is the application's call, and it can check.
	Subprotocols []string

	// TLSConfig is used for wss. Nil means a default with the URL's hostname as
	// ServerName and TLS 1.2 as the floor. A supplied config with an empty
	// ServerName is cloned and has the hostname filled in, so verification
	// cannot silently pass against the wrong certificate.
	TLSConfig *tls.Config

	// MaxPayload caps a single inbound message after reassembly. Zero means
	// wsMaxPayloadDefault (4 MiB). A peer announcing more closes the connection
	// rather than allocating.
	MaxPayload int

	// DisableAutoPong stops the read pump answering an inbound Ping with a Pong.
	// Answering is the default because a peer that pings to check liveness will
	// otherwise drop a connection that is working.
	DisableAutoPong bool
}

// wsDefaultHandshakeTimeout is the dial and upgrade budget when none is given.
//
// Ten seconds: long enough for a TLS handshake to a distant peer on a slow link,
// short enough that a dial loop over an unreachable address list finishes.
const wsDefaultHandshakeTimeout = 10 * time.Second

// wsClientReadBuffer is the bufio.Reader size on a dialled connection. Frame
// headers are at most 14 bytes and payloads are read straight through, so this
// only needs to make header reads cheap.
const wsClientReadBuffer = 4 << 10

// ─── Client-side connection state ─────────────────────────────────────────────

// wsClientState is the part of a dialled connection an accepted one has no use
// for: the callbacks, and the guarantee that OnClose runs exactly once.
//
// Callbacks are stored behind a mutex rather than being constructor arguments so
// that DialWS can return before they are set. The alternative — callbacks in the
// config — means a handler that has to be written before it can name the
// connection it belongs to, which is the wrong way round for a peer table that
// keys on the connection.
//
// Frames that arrive before OnMessage is registered are dropped, not queued. A
// queue here would be an unbounded buffer fed by a peer, which is a memory
// exhaustion vector; the window is between DialWS returning and the next few
// statements, and a P2P handshake that races it was going to race the network
// anyway.
type wsClientState struct {
	mu        sync.Mutex
	onMessage func(opcode byte, payload []byte)
	onClose   func(code uint16, reason string)

	// closeFired makes OnClose exactly-once. Both Close and the read pump reach
	// it — a local Close and a peer disconnect can happen in the same instant —
	// and an application decrementing a peer count in OnClose must not be told
	// twice.
	closeFired sync.Once

	subprotocol string
}

// fireClose runs the close callback once, whoever calls it first.
func (cs *wsClientState) fireClose(code uint16, reason string) {
	cs.closeFired.Do(func() {
		cs.mu.Lock()
		fn := cs.onClose
		cs.mu.Unlock()
		if fn != nil {
			fn(code, reason)
		}
	})
}

// deliver hands a complete message to the registered callback.
func (cs *wsClientState) deliver(opcode byte, payload []byte) {
	cs.mu.Lock()
	fn := cs.onMessage
	cs.mu.Unlock()
	if fn != nil {
		fn(opcode, payload)
	}
}

// ─── net.Conn as a wsRawConn ──────────────────────────────────────────────────

// wsNetConn adapts a dialled net.Conn to the three methods WSConn needs.
//
// AsyncWrite is synchronous here despite the name. The name comes from gnet,
// where a write is queued onto an event loop; a blocking socket has nowhere to
// queue it to, and spawning a goroutine per write would reorder frames — fatal,
// because a fragmented message's continuations must arrive in sequence. So the
// write happens inline under a mutex, which preserves order and keeps the "safe
// from any goroutine" contract WSConn.Send documents.
type wsNetConn struct {
	nc net.Conn

	// mu serialises writes. Without it two concurrent Sends could interleave
	// their bytes mid-frame and desynchronise the peer's parser permanently.
	mu sync.Mutex

	// writeTimeout bounds one write. A peer that stops reading must not be able
	// to block a sender forever, which is what an unbounded write to a full
	// socket buffer does.
	writeTimeout time.Duration
}

// wsClientWriteTimeout bounds a single frame write to a dialled peer.
//
// Ten seconds is far longer than any healthy write needs and short enough to
// notice a peer that has stopped reading. A frame that cannot be written in that
// time means the peer is not consuming, and the connection is no longer useful.
const wsClientWriteTimeout = 10 * time.Second

func (w *wsNetConn) AsyncWrite(buf []byte, callback gnet.AsyncCallback) error {
	w.mu.Lock()
	var err error
	if w.writeTimeout > 0 {
		// Ignored deliberately: a deadline failure means the socket is already
		// closed, which the Write below reports with a more useful error.
		_ = w.nc.SetWriteDeadline(time.Now().Add(w.writeTimeout))
	}
	_, err = w.nc.Write(buf)
	w.mu.Unlock()

	// gnet invokes the callback on the event loop after the write completes. The
	// contract is the same here — after the write, with its error — so a caller
	// passing one is not silently ignored.
	if callback != nil {
		_ = callback(nil, err)
	}
	return err
}

func (w *wsNetConn) Close() error { return w.nc.Close() }

func (w *wsNetConn) RemoteAddr() net.Addr { return w.nc.RemoteAddr() }

// ─── DialWS ───────────────────────────────────────────────────────────────────

// DialWS dials a WebSocket server and returns a connection on which the opening
// handshake has already completed.
//
// rawURL uses the ws or wss scheme. A missing port defaults to 80 or 443 to match
// the scheme; a missing path becomes "/".
//
//	conn, err := breeze.DialWS("ws://peer:9000/p2p", breeze.WSClientConfig{
//	    HandshakeTimeout: 5 * time.Second,
//	    Header:           map[string]string{"Authorization": "Bearer " + token},
//	})
//	if err != nil {
//	    return err
//	}
//	conn.OnMessage(func(opcode byte, payload []byte) { peer.handle(opcode, payload) })
//	conn.OnClose(func(code uint16, reason string) { peers.drop(conn) })
//	conn.SendBinary(hello)
//
// The returned connection is a *WSConn — the same type the inbound side hands a
// WSHandler — so a peer table holds one type for connections it accepted and
// connections it dialled.
//
// Callbacks are registered after the dial, not passed in, so a handler can refer
// to the connection it belongs to. The read pump does not start until the first
// OnMessage or OnClose registration, so a message cannot be dropped between the
// return and the registration. It also starts on the first Recv.
//
// No reconnection: a closed connection is closed. Redial policy is the caller's.
func DialWS(rawURL string, cfg WSClientConfig) (*WSConn, error) {
	u, secure, addr, err := parseWSURL(rawURL)
	if err != nil {
		return nil, err
	}

	timeout := cfg.HandshakeTimeout
	if timeout <= 0 {
		timeout = wsDefaultHandshakeTimeout
	}

	nc, err := dialWSSocket(addr, u.Hostname(), secure, timeout, cfg.TLSConfig)
	if err != nil {
		return nil, err
	}

	// One deadline across the whole handshake. Cleared on success, because a P2P
	// connection is idle for most of its life and a lingering read deadline would
	// close a healthy peer.
	if err := nc.SetDeadline(time.Now().Add(timeout)); err != nil {
		_ = nc.Close()
		return nil, fmt.Errorf("websocket: set handshake deadline: %w", err)
	}

	br := bufio.NewReaderSize(nc, wsClientReadBuffer)
	subprotocol, err := wsClientHandshake(nc, br, u, cfg)
	if err != nil {
		_ = nc.Close()
		return nil, err
	}

	if err := nc.SetDeadline(time.Time{}); err != nil {
		_ = nc.Close()
		return nil, fmt.Errorf("websocket: clear handshake deadline: %w", err)
	}

	state := &wsClientState{subprotocol: subprotocol}
	wc := &WSConn{
		conn:        &wsNetConn{nc: nc, writeTimeout: wsClientWriteTimeout},
		client:      true,
		clientState: state,
	}

	maxPayload := cfg.MaxPayload
	if maxPayload <= 0 {
		maxPayload = wsMaxPayloadDefault
	}
	wc.clientReader = &wsClientReader{
		wc:         wc,
		br:         br,
		maxPayload: maxPayload,
		autoPong:   !cfg.DisableAutoPong,
		recv:       make(chan wsMessage, wsRecvQueue),
	}
	return wc, nil
}

// parseWSURL validates the scheme and resolves the dial address.
func parseWSURL(rawURL string) (u *url.URL, secure bool, addr string, err error) {
	u, err = url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, false, "", fmt.Errorf("websocket: parse url: %w", err)
	}
	switch u.Scheme {
	case "ws":
	case "wss":
		secure = true
	default:
		// http/https are rejected rather than coerced. A caller passing one has
		// a different mental model of what this function does, and silently
		// accepting it would hide that until the peer refused the upgrade.
		return nil, false, "", fmt.Errorf("websocket: unsupported scheme %q (want ws or wss)", u.Scheme)
	}
	if u.Host == "" {
		return nil, false, "", errors.New("websocket: url has no host")
	}

	addr = u.Host
	if u.Port() == "" {
		if secure {
			addr = net.JoinHostPort(u.Hostname(), "443")
		} else {
			addr = net.JoinHostPort(u.Hostname(), "80")
		}
	}
	return u, secure, addr, nil
}

// dialWSSocket opens the TCP or TLS connection.
//
// TLS is crypto/tls's job, matching client/client.go: gnet has no TLS support and
// this package is not the place to acquire an implementation of it.
func dialWSSocket(addr, hostname string, secure bool, timeout time.Duration, tlsCfg *tls.Config) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: timeout}
	if !secure {
		nc, err := dialer.Dial("tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("websocket: dial %s: %w", addr, err)
		}
		return nc, nil
	}

	cfg := tlsCfg
	switch {
	case cfg == nil:
		cfg = &tls.Config{ServerName: hostname, MinVersion: tls.VersionTLS12}
	case cfg.ServerName == "":
		// Cloned rather than mutated: the caller may be reusing one config
		// across several dials, and writing a hostname into it would make the
		// second dial verify against the first peer's name.
		clone := cfg.Clone()
		clone.ServerName = hostname
		cfg = clone
	}
	nc, err := tls.DialWithDialer(dialer, "tcp", addr, cfg)
	if err != nil {
		return nil, fmt.Errorf("websocket: tls dial %s: %w", addr, err)
	}
	return nc, nil
}

// ─── Handshake ────────────────────────────────────────────────────────────────

// wsClientHandshake sends the upgrade request and validates the response.
// It returns the negotiated subprotocol, which is "" when none was selected.
func wsClientHandshake(nc net.Conn, br *bufio.Reader, u *url.URL, cfg WSClientConfig) (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("websocket: handshake nonce: %w", err)
	}
	key := base64.StdEncoding.EncodeToString(nonce[:])

	req := buildWSUpgradeRequest(u, key, cfg)
	if _, err := nc.Write(req); err != nil {
		return "", fmt.Errorf("websocket: write handshake: %w", err)
	}

	status, headers, err := readWSHandshakeResponse(br)
	if err != nil {
		return "", err
	}
	if status != 101 {
		// The status is the useful part: 401 from an authenticating peer, 404 on
		// the wrong path and 426 from a plain HTTP server are three different
		// problems with three different fixes.
		return "", fmt.Errorf("%w: peer answered %d", ErrWSHandshakeFailed, status)
	}
	if !strings.EqualFold(headers["upgrade"], "websocket") {
		return "", fmt.Errorf("%w: 101 without Upgrade: websocket", ErrWSHandshakeFailed)
	}
	if got := headers["sec-websocket-accept"]; got != wsAcceptKey(key) {
		return "", ErrWSAcceptMismatch
	}

	selected := headers["sec-websocket-protocol"]
	if selected != "" && !wsOffered(cfg.Subprotocols, selected) {
		// A peer selecting a subprotocol that was never offered is not following
		// §4.1, and continuing would mean speaking a protocol this side did not
		// agree to.
		return "", fmt.Errorf("%w: peer selected unoffered subprotocol %q", ErrWSHandshakeFailed, selected)
	}
	return selected, nil
}

// buildWSUpgradeRequest renders the GET request for the opening handshake.
//
// The protocol's own headers are written first and a caller's Header cannot
// replace them: overriding Sec-WebSocket-Key would defeat the accept check, and
// overriding Upgrade or Connection would produce a request no server upgrades.
func buildWSUpgradeRequest(u *url.URL, key string, cfg WSClientConfig) []byte {
	path := u.RequestURI()
	if path == "" {
		path = "/"
	}

	var b strings.Builder
	b.Grow(256)
	b.WriteString("GET ")
	b.WriteString(path)
	b.WriteString(" HTTP/1.1\r\nHost: ")
	b.WriteString(u.Host)
	b.WriteString("\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: ")
	b.WriteString(key)
	b.WriteString("\r\nSec-WebSocket-Version: 13\r\n")

	if len(cfg.Subprotocols) > 0 {
		b.WriteString("Sec-WebSocket-Protocol: ")
		b.WriteString(strings.Join(cfg.Subprotocols, ", "))
		b.WriteString("\r\n")
	}

	for name, value := range cfg.Header {
		if wsReservedHeader(name) {
			continue
		}
		// CR and LF are dropped rather than escaped. A newline in a header value
		// is request splitting — it would let a caller-supplied value inject a
		// second header, or a second request — and there is no legitimate value
		// containing one.
		b.WriteString(strings.NewReplacer("\r", "", "\n", "").Replace(name))
		b.WriteString(": ")
		b.WriteString(strings.NewReplacer("\r", "", "\n", "").Replace(value))
		b.WriteString("\r\n")
	}

	b.WriteString("\r\n")
	return []byte(b.String())
}

// wsReservedHeader reports whether the handshake owns a header name.
func wsReservedHeader(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "host", "upgrade", "connection",
		"sec-websocket-key", "sec-websocket-version", "sec-websocket-protocol":
		return true
	}
	return false
}

// wsOffered reports whether selected appears in offers, case-insensitively.
func wsOffered(offers []string, selected string) bool {
	for _, offer := range offers {
		if strings.EqualFold(strings.TrimSpace(offer), selected) {
			return true
		}
	}
	return false
}

// readWSHandshakeResponse reads the status line and headers, stopping at the
// blank line so any frames the peer already sent stay buffered in br.
func readWSHandshakeResponse(br *bufio.Reader) (int, map[string]string, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return 0, nil, fmt.Errorf("websocket: read status line: %w", err)
	}
	parts := strings.SplitN(strings.TrimSpace(line), " ", 3)
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "HTTP/1.") {
		return 0, nil, fmt.Errorf("websocket: malformed status line %q", strings.TrimSpace(line))
	}
	status, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, nil, fmt.Errorf("websocket: malformed status code %q", parts[1])
	}

	headers := make(map[string]string, 8)
	for range wsMaxHandshakeHeaders {
		line, err := br.ReadString('\n')
		if err != nil {
			return 0, nil, fmt.Errorf("websocket: read headers: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			return status, headers, nil
		}
		if colon := strings.IndexByte(line, ':'); colon > 0 {
			name := strings.ToLower(strings.TrimSpace(line[:colon]))
			headers[name] = strings.TrimSpace(line[colon+1:])
		}
	}
	// A peer that never sends the blank line would otherwise keep this loop
	// reading until the deadline, growing the map with every line.
	return 0, nil, fmt.Errorf("websocket: handshake response exceeded %d headers", wsMaxHandshakeHeaders)
}

// wsMaxHandshakeHeaders bounds the handshake response. Any real 101 carries a
// handful; a peer sending thousands is either broken or feeding a memory leak.
const wsMaxHandshakeHeaders = 100

// wsAcceptKey computes the RFC 6455 §4.2.2 Sec-WebSocket-Accept value.
//
// The same computation wsHandshakeResponse performs on the server side. It is
// written twice rather than shared because the two use it for opposite purposes —
// the server produces the value, the client verifies it — and a single helper
// would be a function whose name could only describe one of them.
func wsAcceptKey(key string) string {
	h := sha1.New()
	h.Write([]byte(key))
	h.Write([]byte(wsGUID))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// ─── Read pump ────────────────────────────────────────────────────────────────

// wsClientReader is the blocking read side of a dialled connection.
//
// It reads frames off the socket, answers control frames, reassembles fragmented
// messages, and hands complete ones to the connection's OnMessage. One goroutine
// per connection, started once — see the file comment for why this is not on the
// event loop.
type wsClientReader struct {
	wc         *WSConn
	br         *bufio.Reader
	maxPayload int
	autoPong   bool

	// started makes the pump idempotent. OnMessage, OnClose and Recv all want it
	// running and any of them may be called first, or several times.
	started sync.Once

	// fragBuf accumulates continuation frames. Separate from WSConn.fragBuf,
	// which the inbound engine owns; sharing it would mean two reassembly paths
	// writing one buffer.
	fragBuf []byte
	fragOp  byte

	// recv carries messages to Recv. Allocated up front rather than on first use:
	// a channel created after the read pump had already exited would never be
	// closed, and Recv would block forever on a connection that is already gone.
	// The cost is one small buffered channel per dialled connection.
	recv chan wsMessage

	// recvActive switches delivery from the callback to recv. Set by the first
	// Recv, read by the read goroutine on every message.
	recvActive atomic.Bool

	// recvClosed makes closing recv idempotent: every exit path in run reaches
	// finish, and closing a channel twice panics.
	recvClosed sync.Once
}

// wsMessage is one complete message delivered by Recv.
type wsMessage struct {
	Opcode  byte
	Payload []byte
}

// start launches the read loop at most once.
func (r *wsClientReader) start() {
	r.started.Do(func() { go r.run() })
}

// run is the read loop. It returns when the socket fails, the peer sends Close,
// or the local side closes — and fires OnClose on the way out, whatever the cause,
// so an application never has to distinguish "closed" from "failed" to know a peer
// is gone.
func (r *wsClientReader) run() {
	code, reason := uint16(WsCloseAbnormalClosure), ""

	for {
		frame, err := r.readFrame()
		if err != nil {
			if r.wc.closed.Load() {
				// A local Close already fired the callback with the real code;
				// this read failing afterwards is the expected consequence, not
				// news. finish is still called: it is idempotent, and it is what
				// closes the Recv channel — skipping it would leave a Recv loop
				// blocked on a connection this process closed itself.
				r.finish(WsCloseNormalClosure, "")
				return
			}
			break
		}

		switch frame.opcode {
		case wsOpPing:
			if r.autoPong {
				_ = r.wc.sendControl(wsOpPong, frame.payload)
			}
		case wsOpPong:
			// Liveness answered. Nothing to deliver: a Pong is evidence the
			// connection works, which the absence of an error already conveys.
		case wsOpClose:
			code, reason = parseClosePayload(frame.payload)
			// Echo before closing, per §5.5.1: a peer waiting for the reply to
			// its close frame otherwise sits until its own timeout.
			if !r.wc.closed.Swap(true) {
				_ = r.wc.sendControl(wsOpClose, frame.payload)
				_ = r.wc.conn.Close()
			}
			r.finish(code, reason)
			return
		case wsOpText, wsOpBinary:
			if !r.handleData(frame) {
				r.failProtocol()
				return
			}
		case wsOpContinuation:
			if !r.handleContinuation(frame) {
				r.failProtocol()
				return
			}
		default:
			// Reserved opcode. §5.2 requires failing the connection rather than
			// ignoring it: an unknown opcode means the stream is not what this
			// side thinks it is.
			r.failProtocol()
			return
		}
	}

	// Socket-level failure: no close frame, so 1006 per §7.1.5.
	r.wc.closed.Store(true)
	_ = r.wc.conn.Close()
	r.finish(code, reason)
}

// finish fires OnClose once and unblocks any Recv.
func (r *wsClientReader) finish(code uint16, reason string) {
	r.recvClosed.Do(func() { close(r.recv) })
	r.wc.clientState.fireClose(code, reason)
}

// failProtocol closes with 1002 after a framing violation.
func (r *wsClientReader) failProtocol() {
	if !r.wc.closed.Swap(true) {
		var payload [2]byte
		binary.BigEndian.PutUint16(payload[:], WsCloseProtocolError)
		_ = r.wc.sendControl(wsOpClose, payload[:])
		_ = r.wc.conn.Close()
	}
	r.finish(WsCloseProtocolError, "protocol error")
}

// handleData starts or completes a data message. It reports false on a framing
// violation — a new data frame arriving while a fragmented message is open.
func (r *wsClientReader) handleData(frame *wsFrame) bool {
	if r.fragOp != 0 {
		// §5.4: fragments of one message must not be interleaved with another
		// data frame. Continuing would silently splice two messages together.
		frame.release()
		return false
	}
	if frame.fin {
		opcode, payload := frame.opcode, frame.payload
		frame.release()
		r.deliver(opcode, payload)
		return true
	}
	r.fragOp = frame.opcode
	r.fragBuf = append(r.fragBuf[:0], frame.payload...)
	frame.release()
	return true
}

// handleContinuation appends to the open message, completing it on FIN.
func (r *wsClientReader) handleContinuation(frame *wsFrame) bool {
	if r.fragOp == 0 {
		// A continuation with nothing to continue.
		frame.release()
		return false
	}
	if len(r.fragBuf)+len(frame.payload) > r.maxPayload {
		// The per-frame limit is checked in readFrame; this is the reassembled
		// total, which is the number an attacker actually controls by sending
		// many small fragments.
		frame.release()
		return false
	}
	r.fragBuf = append(r.fragBuf, frame.payload...)
	fin := frame.fin
	frame.release()
	if !fin {
		return true
	}
	// Copied out: fragBuf is reused for the next message, and the callback may
	// keep what it is given for as long as it likes.
	payload := make([]byte, len(r.fragBuf))
	copy(payload, r.fragBuf)
	opcode := r.fragOp
	r.fragBuf = r.fragBuf[:0]
	r.fragOp = 0
	r.deliver(opcode, payload)
	return true
}

// deliver hands one complete message to whichever consumer is in use.
//
// Called from the read goroutine, so a slow callback applies backpressure to this
// connection only — no other peer's traffic is behind it. That is deliberate: the
// alternative, dispatching each message to a pool, would reorder a peer's
// messages, and a P2P protocol's ordering is usually load-bearing.
func (r *wsClientReader) deliver(opcode byte, payload []byte) {
	if r.recvActive.Load() {
		r.recv <- wsMessage{Opcode: opcode, Payload: payload}
		return
	}
	r.wc.clientState.deliver(opcode, payload)
}

// readFrame reads one frame from the socket.
//
// Written against bufio rather than reusing parseWSFrame because the two consume
// bytes differently: parseWSFrame is handed a buffer that may hold a partial frame
// and reports "not yet", which is what an event loop needs. A blocking reader can
// simply read exactly as many bytes as the header says, and asking for them is
// both simpler and free of the reassembly buffer the engine has to keep per fd.
func (r *wsClientReader) readFrame() (*wsFrame, error) {
	var head [2]byte
	if _, err := io.ReadFull(r.br, head[:]); err != nil {
		return nil, err
	}
	if head[0]&0x70 != 0 {
		// RSV bits set with no extension negotiated (§5.2).
		return nil, errors.New("websocket: reserved bits set")
	}
	fin := head[0]&0x80 != 0
	opcode := head[0] & 0x0F
	masked := head[1]&0x80 != 0
	length := uint64(head[1] & 0x7F)

	if opcode >= wsOpClose {
		// §5.5: control frames carry at most 125 bytes and are never fragmented.
		if length > wsMaxControlPayload || !fin {
			return nil, errors.New("websocket: malformed control frame")
		}
	}

	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(r.br, ext[:]); err != nil {
			return nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(r.br, ext[:]); err != nil {
			return nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	// Checked before allocating, so a peer cannot ask for a gigabyte and get it.
	if length > uint64(r.maxPayload) {
		return nil, fmt.Errorf("%w: peer announced %d bytes", ErrFrameTooLarge, length)
	}

	var mask [4]byte
	if masked {
		// A server must not mask (§5.1). Tolerated and undone anyway: the cost is
		// four bytes and an XOR, and refusing would break an otherwise working
		// peer over something that does no harm.
		if _, err := io.ReadFull(r.br, mask[:]); err != nil {
			return nil, err
		}
	}

	payload := make([]byte, length)
	if length > 0 {
		if _, err := io.ReadFull(r.br, payload); err != nil {
			return nil, err
		}
		if masked {
			unmaskXOR(payload, mask)
		}
	}

	f := wsFramePool.Get().(*wsFrame)
	f.opcode = opcode
	f.fin = fin
	f.payload = payload
	return f, nil
}

// ─── Client-side WSConn API ───────────────────────────────────────────────────

// OnMessage registers the callback for complete inbound messages and starts the
// read pump.
//
// opcode is WsOpText or WsOpBinary; fragmented messages are reassembled first, so
// a callback never sees a partial one. Control frames are handled internally and
// are not delivered.
//
// Called from the connection's read goroutine, one message at a time, in the order
// the peer sent them. A slow callback delays that peer only — which is why the
// ordering can be guaranteed at all.
//
// Registering a second time replaces the first. On an accepted connection this is
// a no-op: an inbound connection's messages go to the WSHandler given to
// [Breeze.WebSocket], which is the registration for that direction.
func (wc *WSConn) OnMessage(fn func(opcode byte, payload []byte)) {
	if wc.clientState == nil {
		return
	}
	wc.clientState.mu.Lock()
	wc.clientState.onMessage = fn
	wc.clientState.mu.Unlock()
	wc.clientReader.start()
}

// OnClose registers the callback for connection close and starts the read pump.
//
// It runs exactly once, whatever ended the connection: a local [WSConn.Close], a
// Close frame from the peer, a framing violation, or the socket failing. code is
// the peer's close code, or WsCloseAbnormalClosure (1006) when the connection
// dropped without one — 1006 is precisely the "no close frame arrived" signal, so
// a P2P layer can tell an orderly peer shutdown from a network failure.
//
// This is where a redial belongs. Reconnection is deliberately not built in; see
// the file comment.
func (wc *WSConn) OnClose(fn func(code uint16, reason string)) {
	if wc.clientState == nil {
		return
	}
	wc.clientState.mu.Lock()
	wc.clientState.onClose = fn
	wc.clientState.mu.Unlock()
	wc.clientReader.start()
}

// Ping sends a Ping frame. The peer's Pong is consumed internally and is not
// delivered to OnMessage.
//
// This is the liveness check, and it exists because there is no read deadline: a
// silent peer and a dead peer are indistinguishable from the socket alone. An
// application wanting keepalive calls this on a ticker and treats OnClose as the
// answer.
//
// payload may be nil and must not exceed 125 bytes (§5.5).
func (wc *WSConn) Ping(payload []byte) error {
	if len(payload) > wsMaxControlPayload {
		return fmt.Errorf("websocket: ping payload %d exceeds %d bytes",
			len(payload), wsMaxControlPayload)
	}
	if wc.closed.Load() {
		return errors.New("websocket: connection closed")
	}
	return wc.sendControl(wsOpPing, payload)
}

// Pong sends an unsolicited Pong frame, for a peer that expects heartbeats it did
// not ask for. Answering a Ping is automatic unless DisableAutoPong was set.
func (wc *WSConn) Pong(payload []byte) error {
	if len(payload) > wsMaxControlPayload {
		return fmt.Errorf("websocket: pong payload %d exceeds %d bytes",
			len(payload), wsMaxControlPayload)
	}
	if wc.closed.Load() {
		return errors.New("websocket: connection closed")
	}
	return wc.sendControl(wsOpPong, payload)
}

// sendControl writes a control frame without the closed check, so the close
// handshake can still emit its own frame while the connection is being closed.
func (wc *WSConn) sendControl(opcode byte, payload []byte) error {
	frame := wc.buildFrame(opcode, payload)
	if frame == nil {
		return errors.New("websocket: could not encode control frame")
	}
	return wc.conn.AsyncWrite(frame, nil)
}

// Subprotocol returns the subprotocol the peer selected during the handshake, or
// "" if none was negotiated. Empty for an accepted connection, whose upgrade path
// does not negotiate one.
func (wc *WSConn) Subprotocol() string {
	if wc.clientState == nil {
		return ""
	}
	return wc.clientState.subprotocol
}

// IsClient reports whether this connection was dialled rather than accepted.
//
// Application code should not normally need it — that is the point of the two
// sharing a type — but a peer table that logs direction, or a test asserting which
// half it is exercising, has no other way to ask.
func (wc *WSConn) IsClient() bool { return wc.client }

// Recv blocks until the next message arrives, for callers that prefer a loop to a
// callback.
//
//	for {
//	    opcode, payload, err := conn.Recv()
//	    if err != nil {
//	        return err
//	    }
//	    ...
//	}
//
// It returns ErrWSClosedByPeer once the connection has closed and every message
// already received has been returned. Mutually exclusive with OnMessage: the first
// call switches delivery to this path, and mixing the two would mean each message
// arriving at exactly one of them with no way to predict which.
//
// Not safe for concurrent use by multiple goroutines — two receivers would each
// get an arbitrary half of the stream, which is never what a protocol wants.
func (wc *WSConn) Recv() (opcode byte, payload []byte, err error) {
	if wc.clientState == nil {
		return 0, nil, errors.New("websocket: Recv is only valid on a dialled connection")
	}
	r := wc.clientReader
	r.recvActive.Store(true)
	r.start()

	msg, ok := <-r.recv
	if !ok {
		return 0, nil, ErrWSClosedByPeer
	}
	return msg.Opcode, msg.Payload, nil
}

// wsRecvQueue is how many messages Recv buffers ahead of the caller.
//
// Small and bounded on purpose. It absorbs a burst without letting a peer queue
// unbounded memory: once it is full the read goroutine blocks, which is
// backpressure on that one connection and stops there.
const wsRecvQueue = 32
