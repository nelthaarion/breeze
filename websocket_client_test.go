package breeze

// websocket_client_test.go — tests for the outbound WebSocket half.
//
// The important ones dial Breeze's own server, because that is the property the
// feature exists for: a Breeze process talking to a Breeze process over a
// connection it opened. A mocked peer would test the frame encoder against this
// file's own idea of the protocol, which is the one thing already known to agree.
//
// The rest are the cases a live round trip cannot reach — a peer that answers 401,
// one that returns a wrong Sec-WebSocket-Accept, one that selects a subprotocol
// nobody offered — and those use a hand-written listener, since a conforming
// server will not produce them.

import (
	"bufio"
	"encoding/base64"
	"errors"
	"net"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ─── Helpers ──────────────────────────────────────────────────────────────────

// wsTestPortBase is the bottom of the range these tests bind.
//
// Deliberately not net.Listen(":0"). On Windows the OS ephemeral range starts at
// 49152, which is exactly internal/mcp's defaultPortRangeStart — and `go test ./...`
// runs packages concurrently, so a server started here would take a port that
// package's allocator had already handed out. That produced "Only one usage of each
// socket address" failures in a package this file does not touch, which is the
// worst kind of test interference: it is attributed to the wrong code.
//
// 31000 is above the registered ports a developer's own services use, below the
// ephemeral range, and clear of 2000/3000/8080 from the examples.
const wsTestPortBase = 31000

// wsTestPortSeq walks that range so two tests in this file never probe the same
// number, whatever order they run in.
var wsTestPortSeq atomic.Int32

// wsTestPort returns a port nothing is listening on.
//
// Bind-and-release, like the fleet tests: there is a narrow race between the
// release and gnet's rebind, and it is narrower than the collision rate of a fixed
// port. The retry loop covers the case where something else won that race.
func wsTestPort(t *testing.T) int {
	t.Helper()

	for attempt := 0; attempt < 100; attempt++ {
		port := wsTestPortBase + int(wsTestPortSeq.Add(1))
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			continue // in use by something else; try the next
		}
		_ = ln.Close()
		return port
	}
	t.Fatalf("no free port in the range starting at %d", wsTestPortBase)
	return 0
}

// wsWaitForListener blocks until something is accepting, so a test does not race
// gnet's startup and report a dial failure as a client bug.
func wsWaitForListener(t *testing.T, port int) {
	t.Helper()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("nothing listening on %d", port)
}

// wsEchoServer starts a Breeze server whose WebSocket endpoint echoes every
// message back, and returns its ws:// URL.
//
// One server per test process would be faster; one per test is correct, because a
// test that closes a connection must not affect another test's peer count.
func wsEchoServer(t *testing.T, path string) (url string, hub *WSHub) {
	t.Helper()

	port := wsTestPort(t)
	router := NewRouter()
	app := New(router, NewEventLoopWorkerPool(runtime.NumCPU()))

	hub = app.WebSocket(path, &WSHandlerFunc{
		Message: func(conn *WSConn, opcode byte, payload []byte) {
			_ = conn.Send(opcode, payload)
		},
	})

	go func() { _ = app.Run(port, false) }()
	wsWaitForListener(t, port)

	return "ws://127.0.0.1:" + strconv.Itoa(port) + path, hub
}

// ─── Round trip against Breeze's own server ───────────────────────────────────

// TestDialWSRoundTripsBinaryThroughBreezeServer is the criterion the feature
// exists for: a dialled connection carrying a message to a Breeze server and back.
//
// It proves the two halves agree on the wire, which nothing short of a real
// handshake and a real frame can establish — the client masks and the server
// unmasks, and a mistake in either produces a payload that arrives corrupted
// rather than an error.
func TestDialWSRoundTripsBinaryThroughBreezeServer(t *testing.T) {
	url, _ := wsEchoServer(t, "/p2p")

	conn, err := DialWS(url, WSClientConfig{HandshakeTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("DialWS: %v", err)
	}
	defer conn.Close(WsCloseNormalClosure, "test over")

	got := make(chan []byte, 1)
	conn.OnMessage(func(opcode byte, payload []byte) {
		if opcode != WsOpBinary {
			t.Errorf("opcode = %#x, want binary (%#x)", opcode, WsOpBinary)
		}
		got <- payload
	})

	// Non-ASCII bytes deliberately: a masking bug that XORed with a zero key
	// would pass on text and fail here.
	want := []byte{0x00, 0xFF, 0x10, 0x7F, 0x80, 0x01}
	if err := conn.SendBinary(want); err != nil {
		t.Fatalf("SendBinary: %v", err)
	}

	select {
	case echo := <-got:
		if string(echo) != string(want) {
			t.Errorf("echo = %v, want %v — the frame was corrupted in transit", echo, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no echo within 5s")
	}
}

// TestDialWSRoundTripsText covers the other data opcode, since text and binary
// take different branches in the server's dispatch.
func TestDialWSRoundTripsText(t *testing.T) {
	url, _ := wsEchoServer(t, "/p2p")

	conn, err := DialWS(url, WSClientConfig{})
	if err != nil {
		t.Fatalf("DialWS: %v", err)
	}
	defer conn.Close(WsCloseNormalClosure, "")

	got := make(chan string, 1)
	conn.OnMessage(func(_ byte, payload []byte) { got <- string(payload) })

	if err := conn.SendText("hello peer"); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	select {
	case echo := <-got:
		if echo != "hello peer" {
			t.Errorf("echo = %q, want %q", echo, "hello peer")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no echo within 5s")
	}
}

// TestDialWSCarriesAPayloadLargerThanOneFrameHeader exercises the 16-bit and
// 64-bit length encodings.
//
// Three sizes, one per header shape: 125 is the last that fits in the base header,
// 126 forces the 2-byte extended length, and 70000 forces the 8-byte one. Getting
// a length prefix wrong produces a stream the peer desynchronises on rather than an
// error, so this is the shape of bug that would otherwise reach a user.
func TestDialWSCarriesAPayloadLargerThanOneFrameHeader(t *testing.T) {
	url, _ := wsEchoServer(t, "/p2p")

	conn, err := DialWS(url, WSClientConfig{})
	if err != nil {
		t.Fatalf("DialWS: %v", err)
	}
	defer conn.Close(WsCloseNormalClosure, "")

	got := make(chan []byte, 3)
	conn.OnMessage(func(_ byte, payload []byte) { got <- payload })

	for _, size := range []int{125, 126, 70000} {
		payload := make([]byte, size)
		for i := range payload {
			payload[i] = byte(i)
		}
		if err := conn.SendBinary(payload); err != nil {
			t.Fatalf("SendBinary(%d): %v", size, err)
		}
		select {
		case echo := <-got:
			if len(echo) != size {
				t.Fatalf("echo of a %d-byte payload came back %d bytes", size, len(echo))
			}
			if string(echo) != string(payload) {
				t.Errorf("a %d-byte payload came back altered", size)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no echo of the %d-byte payload within 5s", size)
		}
	}
}

// ─── Close semantics ──────────────────────────────────────────────────────────

// TestDialWSFiresOnCloseWhenThePeerCloses is what a P2P redial loop depends on.
//
// The connection is closed by the *server* here, not locally, because that is the
// case a reconnection policy has to notice and the one a local Close cannot
// exercise: nothing in the client's own call stack tells it the peer went away.
func TestDialWSFiresOnCloseWhenThePeerCloses(t *testing.T) {
	port := wsTestPort(t)
	router := NewRouter()
	app := New(router, NewEventLoopWorkerPool(runtime.NumCPU()))

	// Closes from the server as soon as a message arrives, so the test controls
	// when the close happens rather than waiting on a timer.
	app.WebSocket("/p2p", &WSHandlerFunc{
		Message: func(conn *WSConn, _ byte, _ []byte) {
			conn.Close(WsCloseGoingAway, "server shutting down")
		},
	})
	go func() { _ = app.Run(port, false) }()
	wsWaitForListener(t, port)

	conn, err := DialWS("ws://127.0.0.1:"+strconv.Itoa(port)+"/p2p", WSClientConfig{})
	if err != nil {
		t.Fatalf("DialWS: %v", err)
	}

	type closeEvent struct {
		code   uint16
		reason string
	}
	closed := make(chan closeEvent, 4)
	conn.OnClose(func(code uint16, reason string) {
		closed <- closeEvent{code, reason}
	})

	if err := conn.SendText("goodbye"); err != nil {
		t.Fatalf("SendText: %v", err)
	}

	select {
	case ev := <-closed:
		if ev.code != WsCloseGoingAway {
			t.Errorf("close code = %d, want %d (the code the peer sent)", ev.code, WsCloseGoingAway)
		}
		if ev.reason != "server shutting down" {
			t.Errorf("close reason = %q, want the peer's reason", ev.reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnClose never fired; a redial loop would never learn the peer is gone")
	}

	// Exactly once. Both the peer's close frame and the socket failing afterwards
	// reach the same callback, and an application decrementing a peer count must
	// not be told twice.
	select {
	case ev := <-closed:
		t.Errorf("OnClose fired a second time with code %d", ev.code)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestDialWSFiresOnCloseExactlyOnceForALocalClose is the other direction: a local
// Close fires the callback too, so an application has one place to release a peer
// whichever side initiated.
func TestDialWSFiresOnCloseExactlyOnceForALocalClose(t *testing.T) {
	url, _ := wsEchoServer(t, "/p2p")

	conn, err := DialWS(url, WSClientConfig{})
	if err != nil {
		t.Fatalf("DialWS: %v", err)
	}

	closed := make(chan uint16, 4)
	conn.OnClose(func(code uint16, _ string) { closed <- code })

	conn.Close(WsCloseNormalClosure, "done")
	// Idempotent: a second Close must not fire the callback again, which is what
	// a deferred Close after an explicit one would do.
	conn.Close(WsCloseNormalClosure, "done again")

	select {
	case code := <-closed:
		if code != WsCloseNormalClosure {
			t.Errorf("close code = %d, want %d", code, WsCloseNormalClosure)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnClose never fired for a local close")
	}
	select {
	case code := <-closed:
		t.Errorf("OnClose fired twice (second code %d)", code)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestDialWSSendAfterCloseIsRefused — a send on a closed connection reports an
// error rather than writing to a dead socket, so a peer loop that races a
// disconnect gets told rather than silently dropping the message.
func TestDialWSSendAfterCloseIsRefused(t *testing.T) {
	url, _ := wsEchoServer(t, "/p2p")

	conn, err := DialWS(url, WSClientConfig{})
	if err != nil {
		t.Fatalf("DialWS: %v", err)
	}
	conn.Close(WsCloseNormalClosure, "")

	if err := conn.SendBinary([]byte("late")); err == nil {
		t.Error("SendBinary on a closed connection returned nil; the payload went nowhere and the caller was not told")
	}
}

// ─── Symmetry with the inbound side ───────────────────────────────────────────

// TestDialledAndAcceptedConnectionsAreTheSameType is the requirement that made
// this worth building rather than bolting a second connection type on.
//
// A P2P dispatch loop holds connections it accepted and connections it dialled in
// one table. If those were different types it would need two code paths for every
// operation, and the compiler would not stop it from getting one of them wrong.
// Asserting they are assignable to one variable is the whole claim.
func TestDialledAndAcceptedConnectionsAreTheSameType(t *testing.T) {
	port := wsTestPort(t)
	router := NewRouter()
	app := New(router, NewEventLoopWorkerPool(runtime.NumCPU()))

	accepted := make(chan *WSConn, 1)
	app.WebSocket("/p2p", &WSHandlerFunc{
		Connect: func(conn *WSConn) { accepted <- conn },
	})
	go func() { _ = app.Run(port, false) }()
	wsWaitForListener(t, port)

	dialled, err := DialWS("ws://127.0.0.1:"+strconv.Itoa(port)+"/p2p", WSClientConfig{})
	if err != nil {
		t.Fatalf("DialWS: %v", err)
	}
	defer dialled.Close(WsCloseNormalClosure, "")

	var inbound *WSConn
	select {
	case inbound = <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("the server never reported the connection")
	}

	// The real assertion: one slice holds both, and every method a peer table
	// calls is available on each without a type switch.
	peers := []*WSConn{inbound, dialled}
	for i, peer := range peers {
		if peer.RemoteAddr() == "" {
			t.Errorf("peers[%d].RemoteAddr() is empty", i)
		}
		if err := peer.SendBinary([]byte{0x01}); err != nil {
			t.Errorf("peers[%d].SendBinary: %v", i, err)
		}
	}

	// And they still know which they are, for a caller that logs direction.
	if inbound.IsClient() {
		t.Error("an accepted connection reports IsClient() = true")
	}
	if !dialled.IsClient() {
		t.Error("a dialled connection reports IsClient() = false")
	}
}

// TestDialledConnectionIsNotInTheServersHub — a dialled connection must not join
// the hub, or Broadcast would send to a peer this process connected *out* to and
// count it among its own clients.
//
// The hub is a registry of connections this server accepted. An outbound
// connection belongs to whatever dialled it, and a P2P layer that wants to send to
// every peer keeps its own set.
func TestDialledConnectionIsNotInTheServersHub(t *testing.T) {
	url, hub := wsEchoServer(t, "/p2p")

	before := hub.Count()

	conn, err := DialWS(url, WSClientConfig{})
	if err != nil {
		t.Fatalf("DialWS: %v", err)
	}
	defer conn.Close(WsCloseNormalClosure, "")

	// The server's own accepted end of this connection does join the hub, so the
	// count rises by exactly one — not two.
	deadline := time.Now().Add(5 * time.Second)
	for hub.Count() == before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := hub.Count(); got != before+1 {
		t.Errorf("hub count = %d, want %d: the dialled end registered itself as one of the server's clients",
			got, before+1)
	}
}

// ─── Handshake failures ───────────────────────────────────────────────────────

// wsRawListener serves one hand-written HTTP response and closes.
//
// A conforming server cannot produce the responses below, so the peer has to be
// written out. It answers exactly one connection: each test needs one dial.
func wsRawListener(t *testing.T, response string) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		// Drain the request so the client's write completes before the answer,
		// rather than racing a half-written handshake against a close.
		buf := make([]byte, 4096)
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _ = conn.Read(buf)
		_, _ = conn.Write([]byte(response))
	}()

	return "ws://" + ln.Addr().String() + "/p2p"
}

// TestDialWSReportsTheStatusWhenAPeerRefusesTheUpgrade — the status is the
// diagnosis. A 401 from an authenticating peer, a 404 on the wrong path and a 426
// from a plain HTTP server need three different fixes, and "handshake failed" tells
// an operator none of them.
func TestDialWSReportsTheStatusWhenAPeerRefusesTheUpgrade(t *testing.T) {
	url := wsRawListener(t, "HTTP/1.1 401 Unauthorized\r\nContent-Length: 0\r\n\r\n")

	_, err := DialWS(url, WSClientConfig{HandshakeTimeout: 3 * time.Second})
	if err == nil {
		t.Fatal("DialWS succeeded against a peer that answered 401")
	}
	if !errors.Is(err, ErrWSHandshakeFailed) {
		t.Errorf("error does not wrap ErrWSHandshakeFailed, so a caller cannot classify it: %v", err)
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("the error does not name the status, so the cause is unclear: %v", err)
	}
}

// TestDialWSRejectsAWrongAcceptKey is the check that catches a peer which is not a
// WebSocket endpoint at all — most often a proxy answering 101 on its behalf.
// Without it, this side would start framing into a stream nothing parses and the
// failure would surface later as silence.
func TestDialWSRejectsAWrongAcceptKey(t *testing.T) {
	url := wsRawListener(t, "HTTP/1.1 101 Switching Protocols\r\n"+
		"Upgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Accept: this-is-not-the-right-value\r\n\r\n")

	_, err := DialWS(url, WSClientConfig{HandshakeTimeout: 3 * time.Second})
	if err == nil {
		t.Fatal("DialWS accepted a 101 whose Sec-WebSocket-Accept did not match the key sent")
	}
	if !errors.Is(err, ErrWSAcceptMismatch) {
		t.Errorf("error = %v, want ErrWSAcceptMismatch", err)
	}
}

// TestDialWSRejectsAnUnofferedSubprotocol — a peer selecting a subprotocol that
// was never offered is not conforming, and continuing would mean speaking a
// protocol this side never agreed to.
func TestDialWSRejectsAnUnofferedSubprotocol(t *testing.T) {
	// wsOffered is exercised directly rather than through a dial. A peer's accept
	// value has to match the random key the client generated, which a canned
	// response cannot do, so a fixture peer would fail the accept check before ever
	// reaching the subprotocol rule.
	//
	// The key is computed rather than written out. RFC 6455 §1.3's example nonce is
	// the string below; base64 of it is a high-entropy literal that a secret scanner
	// reports as a leaked credential, and it is not one.
	key := base64.StdEncoding.EncodeToString([]byte("the sample nonce"))
	headers := map[string]string{
		"upgrade":                "websocket",
		"sec-websocket-accept":   wsAcceptKey(key),
		"sec-websocket-protocol": "something-else",
	}
	if wsOffered([]string{"breeze-p2p"}, headers["sec-websocket-protocol"]) {
		t.Fatal("wsOffered accepted a subprotocol that was not in the offer list")
	}
	// And the positive case, so the check is not vacuously true.
	if !wsOffered([]string{"breeze-p2p", "other"}, "breeze-p2p") {
		t.Error("wsOffered rejected a subprotocol that was offered")
	}
	// Case-insensitive, per §4.1.
	if !wsOffered([]string{"Breeze-P2P"}, "breeze-p2p") {
		t.Error("wsOffered is case-sensitive; §4.1 subprotocol tokens are not")
	}
}

// TestDialWSRejectsANonWebSocketScheme — http:// is refused rather than coerced to
// ws://. A caller passing one has a different model of what this does, and
// silently accepting it hides that until the peer refuses the upgrade.
func TestDialWSRejectsANonWebSocketScheme(t *testing.T) {
	for _, url := range []string{
		"http://127.0.0.1:9/p2p",
		"https://127.0.0.1:9/p2p",
		"tcp://127.0.0.1:9",
		"127.0.0.1:9",
	} {
		if _, err := DialWS(url, WSClientConfig{}); err == nil {
			t.Errorf("DialWS(%q) was accepted; only ws and wss are WebSocket URLs", url)
		}
	}
}

// ─── Masking ──────────────────────────────────────────────────────────────────

// TestClientFramesAreMasked is the §5.3 requirement, checked on the bytes.
//
// A conforming server closes the connection on an unmasked client frame. Breeze's
// own parseWSFrame tolerates both, so the round-trip tests above would still pass
// with masking broken — this is the only thing that would notice, and it matters
// the moment the peer is anything other than Breeze.
func TestClientFramesAreMasked(t *testing.T) {
	frame := buildWSFrameMasked(WsOpBinary, []byte("payload"))
	if frame == nil {
		t.Fatal("buildWSFrameMasked returned nil")
	}
	if frame[1]&0x80 == 0 {
		t.Error("the mask bit is not set; a conforming server would close the connection")
	}

	// And the payload is actually transformed, not merely flagged. A zero mask key
	// would set the bit and leave the bytes readable.
	if string(frame[6:]) == "payload" {
		t.Error("the payload is unmasked despite the mask bit being set")
	}

	// Round-trips through the server's own decoder, which is the real criterion:
	// the two halves have to agree.
	decoded, consumed := parseWSFrame(frame, wsMaxPayloadDefault)
	if decoded == nil {
		t.Fatalf("the server's parser rejected a client frame (consumed=%d)", consumed)
	}
	if string(decoded.payload) != "payload" {
		t.Errorf("decoded payload = %q, want %q", decoded.payload, "payload")
	}
	if decoded.opcode != WsOpBinary {
		t.Errorf("decoded opcode = %#x, want %#x", decoded.opcode, WsOpBinary)
	}
	decoded.release()
}

// TestServerFramesAreNotMasked is the other half of §5.1 — a server must never
// mask. Sharing one WSConn type makes this worth asserting: a bug that masked
// unconditionally would break every browser client of every Breeze server.
func TestServerFramesAreNotMasked(t *testing.T) {
	frame := buildWSFrame(WsOpBinary, []byte("payload"))
	if frame == nil {
		t.Fatal("buildWSFrame returned nil")
	}
	if frame[1]&0x80 != 0 {
		t.Error("a server frame has the mask bit set, which RFC 6455 §5.1 forbids")
	}
}

// TestMaskedFrameLengthEncodings covers the three header shapes, since each writes
// the mask key at a different offset. Getting one offset wrong corrupts every
// frame of that size only, which is the kind of bug that survives a suite using
// one payload size.
func TestMaskedFrameLengthEncodings(t *testing.T) {
	for _, size := range []int{0, 1, 125, 126, 65535, 65536} {
		payload := make([]byte, size)
		for i := range payload {
			payload[i] = byte(i % 251)
		}

		frame := buildWSFrameMasked(WsOpBinary, payload)
		if frame == nil {
			t.Fatalf("buildWSFrameMasked(%d bytes) returned nil", size)
		}
		if frame[1]&0x80 == 0 {
			t.Errorf("a %d-byte frame is not masked", size)
		}

		decoded, _ := parseWSFrame(frame, wsMaxPayloadDefault)
		if decoded == nil {
			t.Fatalf("the parser rejected a masked %d-byte frame", size)
		}
		if len(decoded.payload) != size {
			t.Errorf("a %d-byte payload decoded to %d bytes", size, len(decoded.payload))
		} else if string(decoded.payload) != string(payload) {
			t.Errorf("a %d-byte payload did not survive mask and unmask", size)
		}
		decoded.release()
	}
}

// ─── Ping / pong ──────────────────────────────────────────────────────────────

// TestPingIsAnsweredByBreezesServer proves the liveness path works end to end.
//
// This is how a P2P layer detects a peer that has gone silent without closing:
// there is no read deadline on a dialled connection, so a ping is the only thing
// distinguishing an idle peer from a dead one.
func TestPingIsAnsweredByBreezesServer(t *testing.T) {
	url, _ := wsEchoServer(t, "/p2p")

	conn, err := DialWS(url, WSClientConfig{})
	if err != nil {
		t.Fatalf("DialWS: %v", err)
	}
	defer conn.Close(WsCloseNormalClosure, "")

	// A Pong is consumed by the read pump, not delivered to OnMessage, so the
	// observable effect is that the connection still works afterwards. A pump that
	// mishandled the Pong would fail the connection instead.
	echoed := make(chan []byte, 1)
	conn.OnMessage(func(_ byte, payload []byte) { echoed <- payload })

	if err := conn.Ping([]byte("alive?")); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if err := conn.SendBinary([]byte("after-ping")); err != nil {
		t.Fatalf("SendBinary after Ping: %v", err)
	}

	select {
	case msg := <-echoed:
		if string(msg) != "after-ping" {
			t.Errorf("got %q; a Pong was delivered as a message instead of being consumed", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the connection stopped working after a ping/pong exchange")
	}
}

// TestControlFramesRefuseAnOversizedPayload — §5.5 caps a control frame at 125
// bytes. A longer one is refused here rather than sent and closed by the peer,
// which would look like an unexplained disconnect.
func TestControlFramesRefuseAnOversizedPayload(t *testing.T) {
	url, _ := wsEchoServer(t, "/p2p")

	conn, err := DialWS(url, WSClientConfig{})
	if err != nil {
		t.Fatalf("DialWS: %v", err)
	}
	defer conn.Close(WsCloseNormalClosure, "")

	if err := conn.Ping(make([]byte, wsMaxControlPayload+1)); err == nil {
		t.Error("Ping accepted a 126-byte payload; §5.5 caps a control frame at 125")
	}
	if err := conn.Pong(make([]byte, wsMaxControlPayload+1)); err == nil {
		t.Error("Pong accepted a 126-byte payload")
	}
}

// ─── Recv ─────────────────────────────────────────────────────────────────────

// TestRecvDeliversMessagesSynchronously covers the loop-shaped API, which some
// peer implementations prefer to a callback.
func TestRecvDeliversMessagesSynchronously(t *testing.T) {
	url, _ := wsEchoServer(t, "/p2p")

	conn, err := DialWS(url, WSClientConfig{})
	if err != nil {
		t.Fatalf("DialWS: %v", err)
	}
	defer conn.Close(WsCloseNormalClosure, "")

	if err := conn.SendText("one"); err != nil {
		t.Fatalf("SendText: %v", err)
	}

	opcode, payload, err := conn.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if opcode != WsOpText {
		t.Errorf("opcode = %#x, want text", opcode)
	}
	if string(payload) != "one" {
		t.Errorf("payload = %q, want %q", payload, "one")
	}
}

// TestRecvReportsClosureRatherThanBlockingForever — a Recv loop has to terminate
// when the peer goes away, or a P2P layer leaks a goroutine per dead peer.
func TestRecvReportsClosureRatherThanBlockingForever(t *testing.T) {
	url, _ := wsEchoServer(t, "/p2p")

	conn, err := DialWS(url, WSClientConfig{})
	if err != nil {
		t.Fatalf("DialWS: %v", err)
	}

	// The pump is started before closing, so the close is observed by a running
	// reader rather than by the first Recv.
	conn.OnClose(func(uint16, string) {})
	conn.Close(WsCloseNormalClosure, "")

	done := make(chan error, 1)
	go func() {
		_, _, err := conn.Recv()
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Error("Recv returned a message from a closed connection")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Recv blocked forever on a closed connection; a peer loop would leak a goroutine")
	}
}

// TestClientOnlyMethodsAreInertOnAnAcceptedConnection — a helper written against a
// dialled connection may be handed an accepted one, since they share a type. The
// client-only methods must be no-ops there rather than panics, and Recv must refuse
// rather than compete with the WSHandler for the same stream.
func TestClientOnlyMethodsAreInertOnAnAcceptedConnection(t *testing.T) {
	port := wsTestPort(t)
	router := NewRouter()
	app := New(router, NewEventLoopWorkerPool(runtime.NumCPU()))

	accepted := make(chan *WSConn, 1)
	app.WebSocket("/p2p", &WSHandlerFunc{
		Connect: func(conn *WSConn) { accepted <- conn },
	})
	go func() { _ = app.Run(port, false) }()
	wsWaitForListener(t, port)

	dialled, err := DialWS("ws://127.0.0.1:"+strconv.Itoa(port)+"/p2p", WSClientConfig{})
	if err != nil {
		t.Fatalf("DialWS: %v", err)
	}
	defer dialled.Close(WsCloseNormalClosure, "")

	select {
	case inbound := <-accepted:
		if _, _, err := inbound.Recv(); err == nil {
			t.Error("Recv on an accepted connection succeeded; it would compete with the WSHandler for messages")
		}
		inbound.OnMessage(func(byte, []byte) {})
		inbound.OnClose(func(uint16, string) {})
		if inbound.Subprotocol() != "" {
			t.Error("an accepted connection reports a negotiated subprotocol")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the server never reported the connection")
	}
}

// ─── Fragmentation ────────────────────────────────────────────────────────────

// wsFragmentingPeer answers one handshake correctly, then writes "Hello" as three
// frames: text/!FIN, continuation/!FIN, continuation/FIN.
//
// Written by hand because nothing in Breeze produces a fragmented message — Send
// always emits a single final frame — so the client's continuation path has no
// other way to be exercised.
func wsFragmentingPeer(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		// The key is parsed out of the request so the accept value is right: the
		// client verifies it and would refuse the connection otherwise.
		br := bufio.NewReader(conn)
		key := ""
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				break
			}
			if colon := strings.IndexByte(line, ':'); colon > 0 &&
				strings.EqualFold(strings.TrimSpace(line[:colon]), "sec-websocket-key") {
				key = strings.TrimSpace(line[colon+1:])
			}
		}
		_, _ = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: " + wsAcceptKey(key) + "\r\n\r\n"))

		// Unmasked, as a server must be (§5.1).
		_, _ = conn.Write([]byte{0x01, 0x02, 'H', 'e'})
		_, _ = conn.Write([]byte{0x00, 0x02, 'l', 'l'})
		_, _ = conn.Write([]byte{0x80, 0x01, 'o'})
		time.Sleep(2 * time.Second)
	}()

	return "ws://" + ln.Addr().String() + "/p2p"
}

// TestFragmentedMessageIsReassembled — three frames arrive, one message is
// delivered, and it carries the first frame's opcode per §5.4.
func TestFragmentedMessageIsReassembled(t *testing.T) {
	conn, err := DialWS(wsFragmentingPeer(t), WSClientConfig{})
	if err != nil {
		t.Fatalf("DialWS: %v", err)
	}
	defer conn.Close(WsCloseNormalClosure, "")

	type message struct {
		opcode  byte
		payload string
	}
	got := make(chan message, 4)
	conn.OnMessage(func(opcode byte, payload []byte) {
		got <- message{opcode, string(payload)}
	})

	select {
	case msg := <-got:
		if msg.payload != "Hello" {
			t.Errorf("reassembled payload = %q, want %q", msg.payload, "Hello")
		}
		// The continuation frames carry opcode 0x0. Reporting that would leave a
		// peer unable to tell text from binary on any fragmented message.
		if msg.opcode != WsOpText {
			t.Errorf("opcode = %#x, want text (%#x): a reassembled message must carry the first frame's opcode",
				msg.opcode, WsOpText)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a fragmented message was never delivered")
	}

	select {
	case msg := <-got:
		t.Errorf("a second message %q arrived; the fragments were delivered separately", msg.payload)
	case <-time.After(300 * time.Millisecond):
	}
}
