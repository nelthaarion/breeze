package wstransport

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
	"strings"
	"sync"
	"time"
)

// This file is a minimal RFC 6455 *client*. Breeze already has a complete
// server-side WebSocket implementation (websocket.go, websocket_engine.go), but
// it only ever decodes client frames and encodes server frames — the two roles
// are not symmetric. A client MUST mask every frame it sends (§5.3) and MUST
// verify Sec-WebSocket-Accept on the handshake (§4.1); a server does neither.
// Reusing the server engine here is therefore not possible, and pulling in
// gorilla/websocket would add the first external dependency to this module for
// the sake of ~150 lines, so the client half is written out.
//
// Scope is deliberately the subset the aggregator's hub actually speaks: text
// frames, no extensions, no compression, no fragmentation on send. Anything
// outside that is either handled defensively or reported as an error rather
// than silently tolerated.

// wsGUID is the RFC 6455 §1.3 handshake constant.
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

const (
	opText  = 0x1
	opClose = 0x8
	opPing  = 0x9
	opPong  = 0xA
)

// maxFramePayload caps an inbound frame. The hub only ever sends us small
// control envelopes ({"type":"auth_ok"}, an error string, a pong), so a large
// declared length means a desynchronised stream or a hostile peer, not a
// legitimate message worth allocating for.
const maxFramePayload = 1 << 20

// wsConn is a client-side WebSocket connection. It is not safe for concurrent
// use; the transport serialises access with its own mutex.
type wsConn struct {
	nc net.Conn
	br *bufio.Reader

	// closeOnce guards the close handshake so a write error and an explicit
	// Close cannot both try to send a close frame on a dead connection.
	closeOnce sync.Once
}

// dialWS performs the TCP/TLS connect and the opening handshake against rawURL,
// which must use the ws or wss scheme.
func dialWS(rawURL string, timeout time.Duration, tlsCfg *tls.Config) (*wsConn, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("wstransport: parse url: %w", err)
	}

	var secure bool
	switch u.Scheme {
	case "ws":
	case "wss":
		secure = true
	default:
		return nil, fmt.Errorf("wstransport: unsupported scheme %q (want ws or wss)", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("wstransport: url has no host")
	}

	addr := u.Host
	if u.Port() == "" {
		if secure {
			addr = net.JoinHostPort(u.Hostname(), "443")
		} else {
			addr = net.JoinHostPort(u.Hostname(), "80")
		}
	}

	dialer := &net.Dialer{Timeout: timeout}

	var nc net.Conn
	if secure {
		// Mirrors client/client.go, which also delegates TLS to crypto/tls
		// rather than implementing it: same trade-off, same reasoning.
		cfg := tlsCfg
		if cfg == nil {
			cfg = &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12}
		} else if cfg.ServerName == "" {
			clone := cfg.Clone()
			clone.ServerName = u.Hostname()
			cfg = clone
		}
		nc, err = tls.DialWithDialer(dialer, "tcp", addr, cfg)
	} else {
		nc, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("wstransport: dial %s: %w", addr, err)
	}

	c := &wsConn{nc: nc, br: bufio.NewReader(nc)}
	if err := c.handshake(u, timeout); err != nil {
		_ = nc.Close()
		return nil, err
	}
	return c, nil
}

// handshake sends the GET upgrade request and validates the 101 response.
func (c *wsConn) handshake(u *url.URL, timeout time.Duration) error {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("wstransport: handshake nonce: %w", err)
	}
	key := base64.StdEncoding.EncodeToString(nonce[:])

	path := u.RequestURI()
	if path == "" {
		path = "/"
	}

	// "Connection: Upgrade" exactly — the aggregator's upgrade handler accepts
	// only that spelling or "keep-alive, Upgrade".
	req := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + u.Host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"

	if err := c.setDeadline(timeout); err != nil {
		return err
	}
	if _, err := c.nc.Write([]byte(req)); err != nil {
		return fmt.Errorf("wstransport: write handshake: %w", err)
	}

	status, headers, err := readHandshakeResponse(c.br)
	if err != nil {
		return err
	}
	if status != 101 {
		return fmt.Errorf("wstransport: handshake rejected with status %d", status)
	}
	if got := headers["sec-websocket-accept"]; got != acceptKey(key) {
		// A wrong accept value means the peer is not a conforming WebSocket
		// endpoint (a proxy answering on its behalf, most likely). Continuing
		// would frame-encode into something that never parses it.
		return errors.New("wstransport: handshake accept mismatch")
	}
	return nil
}

// readHandshakeResponse reads the status line and headers of the upgrade
// response, stopping at the blank line so any frames that follow stay buffered.
func readHandshakeResponse(br *bufio.Reader) (int, map[string]string, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return 0, nil, fmt.Errorf("wstransport: read status line: %w", err)
	}
	parts := strings.SplitN(strings.TrimSpace(line), " ", 3)
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "HTTP/1.") {
		return 0, nil, fmt.Errorf("wstransport: malformed status line %q", strings.TrimSpace(line))
	}
	var status int
	if _, err := fmt.Sscanf(parts[1], "%d", &status); err != nil {
		return 0, nil, fmt.Errorf("wstransport: malformed status code %q", parts[1])
	}

	headers := make(map[string]string, 8)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return 0, nil, fmt.Errorf("wstransport: read headers: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			return status, headers, nil
		}
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(line[:colon]))
		headers[name] = strings.TrimSpace(line[colon+1:])
	}
}

// acceptKey computes the RFC 6455 §4.2.2 Sec-WebSocket-Accept value.
func acceptKey(key string) string {
	h := sha1.New()
	_, _ = h.Write([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func (c *wsConn) setDeadline(timeout time.Duration) error {
	if timeout <= 0 {
		return c.nc.SetDeadline(time.Time{})
	}
	return c.nc.SetDeadline(time.Now().Add(timeout))
}

// writeText sends payload as a single masked text frame.
func (c *wsConn) writeText(payload []byte, timeout time.Duration) error {
	if err := c.setDeadline(timeout); err != nil {
		return err
	}
	return c.writeFrame(opText, payload)
}

// writeFrame emits one final, masked frame. Masking is mandatory for clients
// (§5.3) and the aggregator's decoder rejects unmasked client frames, so the
// mask bit is set unconditionally rather than being an option.
func (c *wsConn) writeFrame(opcode byte, payload []byte) error {
	var header [14]byte
	header[0] = 0x80 | opcode // FIN

	n := len(payload)
	var hlen int
	switch {
	case n < 126:
		header[1] = 0x80 | byte(n)
		hlen = 2
	case n <= 0xFFFF:
		header[1] = 0x80 | 126
		binary.BigEndian.PutUint16(header[2:4], uint16(n))
		hlen = 4
	default:
		header[1] = 0x80 | 127
		binary.BigEndian.PutUint64(header[2:10], uint64(n))
		hlen = 10
	}

	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return fmt.Errorf("wstransport: mask key: %w", err)
	}
	copy(header[hlen:hlen+4], mask[:])
	hlen += 4

	// One buffer, one Write: two writes would let a concurrent close frame
	// interleave between header and payload and corrupt the stream.
	buf := make([]byte, hlen+n)
	copy(buf, header[:hlen])
	for i := 0; i < n; i++ {
		buf[hlen+i] = payload[i] ^ mask[i&3]
	}
	if _, err := c.nc.Write(buf); err != nil {
		return fmt.Errorf("wstransport: write frame: %w", err)
	}
	return nil
}

// readText reads frames until a text frame arrives, answering pings and
// surfacing a close frame as an error. timeout bounds the whole operation.
func (c *wsConn) readText(timeout time.Duration) ([]byte, error) {
	if err := c.setDeadline(timeout); err != nil {
		return nil, err
	}
	for {
		opcode, payload, err := c.readFrame()
		if err != nil {
			return nil, err
		}
		switch opcode {
		case opText:
			return payload, nil
		case opPing:
			if err := c.writeFrame(opPong, payload); err != nil {
				return nil, err
			}
		case opPong:
			// Nothing to do; keep waiting for the text reply.
		case opClose:
			return nil, errClosedByPeer
		default:
			return nil, fmt.Errorf("wstransport: unexpected opcode 0x%X", opcode)
		}
	}
}

var errClosedByPeer = errors.New("wstransport: connection closed by aggregator")

// readFrame reads one frame. Server frames are unmasked, but a mask is
// tolerated and undone so a conforming-but-unusual peer still works.
func (c *wsConn) readFrame() (byte, []byte, error) {
	var head [2]byte
	if _, err := io.ReadFull(c.br, head[:]); err != nil {
		return 0, nil, fmt.Errorf("wstransport: read frame header: %w", err)
	}
	opcode := head[0] & 0x0F
	masked := head[1]&0x80 != 0

	length := uint64(head[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return 0, nil, fmt.Errorf("wstransport: read length: %w", err)
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return 0, nil, fmt.Errorf("wstransport: read length: %w", err)
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	if length > maxFramePayload {
		return 0, nil, fmt.Errorf("wstransport: frame too large (%d bytes)", length)
	}

	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(c.br, mask[:]); err != nil {
			return 0, nil, fmt.Errorf("wstransport: read mask: %w", err)
		}
	}

	payload := make([]byte, length)
	if length > 0 {
		if _, err := io.ReadFull(c.br, payload); err != nil {
			return 0, nil, fmt.Errorf("wstransport: read payload: %w", err)
		}
		if masked {
			for i := range payload {
				payload[i] ^= mask[i&3]
			}
		}
	}
	return opcode, payload, nil
}

// close sends a close frame on a best-effort basis and shuts the socket down.
func (c *wsConn) close() error {
	var err error
	c.closeOnce.Do(func() {
		// 1000 (normal closure), per §7.4.1. A failure here is expected when
		// the peer has already gone, so it is not propagated.
		_ = c.nc.SetWriteDeadline(time.Now().Add(time.Second))
		var frame [2]byte
		binary.BigEndian.PutUint16(frame[:], 1000)
		_ = c.writeFrame(opClose, frame[:])
		err = c.nc.Close()
	})
	return err
}
