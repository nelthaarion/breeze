package wstransport

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nelthaarion/breeze/v2/fleet"
)

// The stub server below is deliberately a raw net.Listener speaking RFC 6455 by
// hand rather than Breeze's own WebSocket engine. Decoding the client's frames
// independently is the only way to prove the client masks them: a test that
// round-tripped through this package's own encoder would pass even if the mask
// bit were never set, which is exactly the bug that would make the aggregator
// reject every frame in production.

type stubServer struct {
	ln net.Listener

	mu       sync.Mutex
	received [][]byte // decoded text payloads from the client
	unmasked int      // frames that arrived without the mask bit

	// authReply overrides the reply to the auth frame. Empty means auth_ok.
	authReply string
	// closeOnAuth closes the connection instead of replying, which is what the
	// hub does when the ingest token is wrong.
	closeOnAuth bool
	// badAccept sends a wrong Sec-WebSocket-Accept value.
	badAccept bool
	// handshakeStatus, when non-zero, is returned instead of 101.
	handshakeStatus int

	wg sync.WaitGroup
}

func newStubServer(t *testing.T) *stubServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &stubServer{ln: ln}
	s.wg.Add(1)
	go s.serve()
	t.Cleanup(func() {
		_ = ln.Close()
		s.wg.Wait()
	})
	return s
}

func (s *stubServer) url() string { return "ws://" + s.ln.Addr().String() + "/fleet/ws" }

func (s *stubServer) serve() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer conn.Close()
			s.handle(conn)
		}()
	}
}

func (s *stubServer) handle(conn net.Conn) {
	br := bufio.NewReader(conn)

	// Read the request line and headers, capturing the key.
	var key string
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if colon := strings.IndexByte(line, ':'); colon > 0 {
			if strings.EqualFold(strings.TrimSpace(line[:colon]), "sec-websocket-key") {
				key = strings.TrimSpace(line[colon+1:])
			}
		}
	}

	if s.handshakeStatus != 0 {
		_, _ = conn.Write([]byte("HTTP/1.1 401 Unauthorized\r\nContent-Length: 0\r\n\r\n"))
		return
	}

	accept := acceptKey(key)
	if s.badAccept {
		accept = "wrongwrongwrongwrongwrongwro="
	}
	_, _ = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"))

	first := true
	for {
		opcode, payload, masked, err := readServerFrame(br)
		if err != nil {
			return
		}
		if opcode == opClose {
			return
		}
		if opcode != opText {
			continue
		}

		s.mu.Lock()
		if !masked {
			s.unmasked++
		}
		s.received = append(s.received, payload)
		s.mu.Unlock()

		if first {
			first = false
			if s.closeOnAuth {
				_ = writeServerText(conn, `{"type":"error","error":"authentication required"}`)
				return
			}
			reply := s.authReply
			if reply == "" {
				reply = `{"type":"auth_ok"}`
			}
			_ = writeServerText(conn, reply)
		}
	}
}

// frames returns the decoded payloads received so far.
func (s *stubServer) frames() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]byte, len(s.received))
	copy(out, s.received)
	return out
}

func (s *stubServer) unmaskedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unmasked
}

// waitFrames polls until n frames have arrived or the deadline passes.
func (s *stubServer) waitFrames(t *testing.T, n int) [][]byte {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := s.frames(); len(got) >= n {
			return got
		}
		time.Sleep(2 * time.Millisecond)
	}
	got := s.frames()
	t.Fatalf("timed out waiting for %d frames, got %d", n, len(got))
	return nil
}

// readServerFrame decodes one frame from the client, reporting whether it was
// masked so the test can assert RFC 6455 §5.3 compliance.
func readServerFrame(br *bufio.Reader) (opcode byte, payload []byte, masked bool, err error) {
	var head [2]byte
	if _, err = io.ReadFull(br, head[:]); err != nil {
		return 0, nil, false, err
	}
	opcode = head[0] & 0x0F
	masked = head[1]&0x80 != 0

	length := uint64(head[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(br, ext[:]); err != nil {
			return 0, nil, false, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(br, ext[:]); err != nil {
			return 0, nil, false, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}

	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(br, mask[:]); err != nil {
			return 0, nil, false, err
		}
	}
	payload = make([]byte, length)
	if length > 0 {
		if _, err = io.ReadFull(br, payload); err != nil {
			return 0, nil, false, err
		}
		if masked {
			for i := range payload {
				payload[i] ^= mask[i&3]
			}
		}
	}
	return opcode, payload, masked, nil
}

// writeServerText writes an unmasked text frame, as a server must.
func writeServerText(conn net.Conn, s string) error {
	payload := []byte(s)
	var header []byte
	switch {
	case len(payload) < 126:
		header = []byte{0x81, byte(len(payload))}
	default:
		header = []byte{0x81, 126, 0, 0}
		binary.BigEndian.PutUint16(header[2:4], uint16(len(payload)))
	}
	_, err := conn.Write(append(header, payload...))
	return err
}

func testSpans() []fleet.Span {
	return []fleet.Span{{
		TraceID: "0af7651916cd43dd8448eb211c80319c",
		SpanID:  "b7ad6b7169203331",
		Service: "gateway",
		Route:   "/checkout",
		Method:  "POST",
		Status:  200,
	}}
}

func newTransport(t *testing.T, s *stubServer, token string) *Transport {
	t.Helper()
	tr := New(Config{
		AggregatorWSURL: s.url(),
		IngestToken:     token,
		ServiceName:     "gateway",
		Timeout:         2 * time.Second,
	})
	t.Cleanup(func() { _ = tr.Close() })
	return tr
}

func TestName(t *testing.T) {
	if got := New(Config{}).Name(); got != "ws" {
		t.Fatalf("Name() = %q, want ws", got)
	}
}

// TestExportSpansSendsAuthThenPublish pins the protocol the hub requires: the
// first frame must be an ingest auth, and only then a publish on the spans
// topic carrying the batch.
func TestExportSpansSendsAuthThenPublish(t *testing.T) {
	s := newStubServer(t)
	tr := newTransport(t, s, "secret-token")

	if err := tr.ExportSpans(context.Background(), "", testSpans()); err != nil {
		t.Fatalf("ExportSpans: %v", err)
	}

	frames := s.waitFrames(t, 2)

	var auth wsEnvelope
	if err := json.Unmarshal(frames[0], &auth); err != nil {
		t.Fatalf("auth frame is not JSON: %v", err)
	}
	if auth.Type != "auth" {
		t.Errorf("first frame type = %q, want auth", auth.Type)
	}
	if auth.Role != "ingest" {
		t.Errorf("auth role = %q, want ingest — the hub rejects publish without it", auth.Role)
	}
	if auth.Token != "secret-token" {
		t.Errorf("auth token = %q, want secret-token", auth.Token)
	}

	var pub wsEnvelope
	if err := json.Unmarshal(frames[1], &pub); err != nil {
		t.Fatalf("publish frame is not JSON: %v", err)
	}
	if pub.Type != "publish" {
		t.Errorf("second frame type = %q, want publish", pub.Type)
	}
	if pub.Topic != "fleet.spans" {
		t.Errorf("topic = %q, want fleet.spans", pub.Topic)
	}

	var spans []fleet.Span
	if err := json.Unmarshal(pub.Payload, &spans); err != nil {
		t.Fatalf("payload is not a span batch: %v", err)
	}
	if len(spans) != 1 || spans[0].TraceID != "0af7651916cd43dd8448eb211c80319c" {
		t.Fatalf("payload did not survive the wire: %+v", spans)
	}
}

// TestClientMasksEveryFrame is the RFC 6455 §5.3 requirement. The aggregator's
// decoder rejects unmasked client frames, so this failing means nothing is
// ingested at all.
func TestClientMasksEveryFrame(t *testing.T) {
	s := newStubServer(t)
	tr := newTransport(t, s, "")

	if err := tr.ExportSpans(context.Background(), "", testSpans()); err != nil {
		t.Fatalf("ExportSpans: %v", err)
	}
	s.waitFrames(t, 2)

	if n := s.unmaskedCount(); n != 0 {
		t.Fatalf("%d client frames arrived unmasked; RFC 6455 §5.3 requires all be masked", n)
	}
}

// TestLargePayloadUsesExtendedLength covers the 126..65535 header branch, which
// a small-batch test never reaches.
func TestLargePayloadUsesExtendedLength(t *testing.T) {
	s := newStubServer(t)
	tr := newTransport(t, s, "")

	spans := make([]fleet.Span, 40)
	for i := range spans {
		spans[i] = fleet.Span{
			TraceID: "0af7651916cd43dd8448eb211c80319c",
			SpanID:  "b7ad6b7169203331",
			Service: "gateway",
			Route:   "/checkout/" + strings.Repeat("x", 40),
			Method:  "POST",
			Status:  200,
		}
	}
	if err := tr.ExportSpans(context.Background(), "", spans); err != nil {
		t.Fatalf("ExportSpans: %v", err)
	}

	frames := s.waitFrames(t, 2)
	if len(frames[1]) < 126 {
		t.Fatalf(
			"payload is %d bytes, too small to exercise the extended-length path",
			len(frames[1]),
		)
	}
	var pub wsEnvelope
	if err := json.Unmarshal(frames[1], &pub); err != nil {
		t.Fatalf("extended-length frame did not decode: %v", err)
	}
	var got []fleet.Span
	if err := json.Unmarshal(pub.Payload, &got); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if len(got) != 40 {
		t.Fatalf("got %d spans, want 40", len(got))
	}
}

// TestConnectionIsReused proves the connection is persistent: a second export
// must not re-authenticate.
func TestConnectionIsReused(t *testing.T) {
	s := newStubServer(t)
	tr := newTransport(t, s, "")

	for i := 0; i < 3; i++ {
		if err := tr.ExportSpans(context.Background(), "", testSpans()); err != nil {
			t.Fatalf("export %d: %v", i, err)
		}
	}

	frames := s.waitFrames(t, 4) // 1 auth + 3 publishes
	auths := 0
	for _, f := range frames {
		var env wsEnvelope
		if json.Unmarshal(f, &env) == nil && env.Type == "auth" {
			auths++
		}
	}
	if auths != 1 {
		t.Fatalf(
			"got %d auth frames across 3 exports, want 1 — connection is not being reused",
			auths,
		)
	}
}

func TestExportHeartbeatUsesHeartbeatTopic(t *testing.T) {
	s := newStubServer(t)
	tr := newTransport(t, s, "")

	if err := tr.ExportHeartbeat(
		context.Background(),
		"",
		fleet.Heartbeat{Service: "gateway"},
	); err != nil {
		t.Fatalf("ExportHeartbeat: %v", err)
	}

	frames := s.waitFrames(t, 2)
	var pub wsEnvelope
	if err := json.Unmarshal(frames[1], &pub); err != nil {
		t.Fatalf("publish frame: %v", err)
	}
	if pub.Topic != "fleet.heartbeat" {
		t.Fatalf("topic = %q, want fleet.heartbeat", pub.Topic)
	}
}

// TestRejectedTokenIsAnError is the case that must never silently succeed: if
// auth failure were swallowed, spans would vanish with no error anywhere.
func TestRejectedTokenIsAnError(t *testing.T) {
	s := newStubServer(t)
	s.closeOnAuth = true
	tr := newTransport(t, s, "wrong-token")

	err := tr.ExportSpans(context.Background(), "", testSpans())
	if err == nil {
		t.Fatal("ExportSpans succeeded against a server that refused auth")
	}
	if !strings.Contains(err.Error(), "authentication") && !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("error does not identify an auth failure: %v", err)
	}
}

func TestUnexpectedAuthReplyIsAnError(t *testing.T) {
	s := newStubServer(t)
	s.authReply = `{"type":"something_else"}`
	tr := newTransport(t, s, "")

	if err := tr.ExportSpans(context.Background(), "", testSpans()); err == nil {
		t.Fatal("export succeeded despite the server never sending auth_ok")
	}
}

func TestHandshakeRejectionIsAnError(t *testing.T) {
	s := newStubServer(t)
	s.handshakeStatus = 401
	tr := newTransport(t, s, "")

	err := tr.ExportSpans(context.Background(), "", testSpans())
	if err == nil {
		t.Fatal("export succeeded despite a 401 handshake")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("error should name the status: %v", err)
	}
}

// TestAcceptMismatchIsRejected covers the §4.1 check. Without it the client
// would happily frame-encode to a peer that never negotiated WebSocket.
func TestAcceptMismatchIsRejected(t *testing.T) {
	s := newStubServer(t)
	s.badAccept = true
	tr := newTransport(t, s, "")

	err := tr.ExportSpans(context.Background(), "", testSpans())
	if err == nil {
		t.Fatal("export succeeded despite a bad Sec-WebSocket-Accept")
	}
	if !strings.Contains(err.Error(), "accept mismatch") {
		t.Fatalf("want an accept-mismatch error, got: %v", err)
	}
}

// TestReconnectsAfterServerClose is the aggregator-restart case: the first
// write lands on a dead socket, and the batch must still get through.
func TestReconnectsAfterServerClose(t *testing.T) {
	s := newStubServer(t)
	tr := newTransport(t, s, "")

	if err := tr.ExportSpans(context.Background(), "", testSpans()); err != nil {
		t.Fatalf("first export: %v", err)
	}
	s.waitFrames(t, 2)

	// Kill the connection underneath the transport, the way a restarting
	// aggregator would.
	tr.mu.Lock()
	if tr.conn != nil {
		_ = tr.conn.nc.Close()
	}
	tr.mu.Unlock()

	if err := tr.ExportSpans(context.Background(), "", testSpans()); err != nil {
		t.Fatalf("export after server close did not recover: %v", err)
	}

	// A fresh connection means a second auth frame.
	frames := s.waitFrames(t, 4)
	auths := 0
	for _, f := range frames {
		var env wsEnvelope
		if json.Unmarshal(f, &env) == nil && env.Type == "auth" {
			auths++
		}
	}
	if auths < 2 {
		t.Fatalf("got %d auth frames, want 2 — the transport did not reconnect", auths)
	}
}

func TestEmptyBatchIsNotSent(t *testing.T) {
	s := newStubServer(t)
	tr := newTransport(t, s, "")

	if err := tr.ExportSpans(context.Background(), "", nil); err != nil {
		t.Fatalf("empty export: %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	if got := s.frames(); len(got) != 0 {
		t.Fatalf("an empty batch produced %d frames; it should not even connect", len(got))
	}
}

func TestCanceledContextDoesNotSend(t *testing.T) {
	s := newStubServer(t)
	tr := newTransport(t, s, "")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := tr.ExportSpans(ctx, "", testSpans()); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if got := s.frames(); len(got) != 0 {
		t.Fatalf("frames were sent on a canceled context: %d", len(got))
	}
}

func TestExportAfterCloseFails(t *testing.T) {
	s := newStubServer(t)
	tr := newTransport(t, s, "")

	if err := tr.ExportSpans(context.Background(), "", testSpans()); err != nil {
		t.Fatalf("export: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := tr.ExportSpans(context.Background(), "", testSpans()); err == nil {
		t.Fatal("export succeeded after Close")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	s := newStubServer(t)
	tr := New(Config{AggregatorWSURL: s.url(), Timeout: time.Second})

	if err := tr.ExportSpans(context.Background(), "", testSpans()); err != nil {
		t.Fatalf("export: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := tr.Close(); err != nil {
			t.Fatalf("Close #%d: %v", i+1, err)
		}
	}
}

// TestNoWSURLFallsBackToHTTP protects the existing behaviour. Before this
// package dialed anything, `fleet.transport: ws` exported over HTTP; a config
// with no WebSocket URL must keep working rather than start failing.
func TestNoWSURLFallsBackToHTTP(t *testing.T) {
	tr := New(Config{ServiceName: "gateway", Timeout: time.Second})
	if tr.wsURL != "" {
		t.Fatalf("wsURL = %q, want empty", tr.wsURL)
	}
	// No aggregator is running, so this must fail as an HTTP error rather than
	// as a WebSocket dial: the point is which path was taken.
	err := tr.ExportSpans(context.Background(), "http://127.0.0.1:1", testSpans())
	if err == nil {
		t.Skip("unexpected success against a closed port; nothing to assert")
	}
	if strings.Contains(err.Error(), "wstransport:") {
		t.Fatalf("took the WebSocket path with no URL configured: %v", err)
	}
}

func TestUnsupportedSchemeIsRejected(t *testing.T) {
	tr := New(Config{AggregatorWSURL: "http://127.0.0.1:9/fleet/ws", Timeout: time.Second})
	t.Cleanup(func() { _ = tr.Close() })

	err := tr.ExportSpans(context.Background(), "", testSpans())
	if err == nil {
		t.Fatal("an http:// WebSocket URL was accepted")
	}
	if !strings.Contains(err.Error(), "unsupported scheme") {
		t.Fatalf("want an unsupported-scheme error, got: %v", err)
	}
}

// TestPropagationMatchesW3C keeps Inject/Extract on the shared encoding, so a
// trace started behind one transport is readable behind another.
func TestPropagationMatchesW3C(t *testing.T) {
	tr := New(Config{ServiceName: "gateway"})
	carrier := headerCarrier{}

	in := fleet.NewTraceContext()
	in.Sampled = true
	tr.Inject(in, fleet.Baggage{"tenant": "acme"}, carrier)

	out, bag, ok := tr.Extract(carrier)
	if !ok {
		t.Fatal("Extract found nothing after Inject")
	}
	if out.TraceID != in.TraceID {
		t.Fatalf("trace id did not survive the round trip: %x vs %x", out.TraceID, in.TraceID)
	}
	if !out.Sampled {
		t.Error("sampled flag lost in propagation")
	}

	if bag["tenant"] != "acme" {
		t.Fatalf("baggage lost: %+v", bag)
	}
}

type headerCarrier map[string]string

func (h headerCarrier) Set(key, value string) { h[strings.ToLower(key)] = value }
func (h headerCarrier) Get(key string) (string, bool) {
	v, ok := h[strings.ToLower(key)]
	return v, ok
}

// TestConcurrentExportsAreSerialised exists because the Tracer is documented as
// exporting from one goroutine, but Close can race with it. Under -race this
// catches a missing lock.
func TestConcurrentExportsAreSerialised(t *testing.T) {
	s := newStubServer(t)
	tr := newTransport(t, s, "")

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = tr.ExportSpans(context.Background(), "", testSpans())
		}()
	}
	wg.Wait()

	// Every frame must be a complete, parseable envelope: interleaved writes
	// would corrupt the stream and show up as a decode failure here.
	for i, f := range s.frames() {
		var env wsEnvelope
		if err := json.Unmarshal(f, &env); err != nil {
			t.Fatalf("frame %d is corrupt, writes interleaved: %v", i, err)
		}
	}
}
