package wstransport

import (
	"bufio"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// Benchmark-only fixtures. These are kept apart from the correctness stub in
// ws_test.go because that one records every frame under a mutex, which would put
// the harness's own bookkeeping inside the measurement.

// newBenchStubServer completes the handshake, replies auth_ok to the first
// frame, then drains and discards everything without recording it.
func newBenchStubServer(b *testing.B) *stubServer {
	b.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("listen: %v", err)
	}
	s := &stubServer{ln: ln}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				benchHandle(conn)
			}()
		}
	}()

	b.Cleanup(func() {
		_ = ln.Close()
		wg.Wait()
	})
	return s
}

func benchHandle(conn net.Conn) {
	br := bufio.NewReader(conn)

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

	_, _ = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + acceptKey(key) + "\r\n\r\n"))

	first := true
	for {
		opcode, _, _, err := readServerFrame(br)
		if err != nil {
			return
		}
		if opcode == opClose {
			return
		}
		if first && opcode == opText {
			first = false
			if err := writeServerText(conn, `{"type":"auth_ok"}`); err != nil {
				return
			}
		}
	}
}

// discardConn is a net.Conn that throws writes away, so BenchmarkWriteFrame
// measures framing and masking without a socket in the number.
type discardConn struct{}

func (discardConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (discardConn) Write(p []byte) (int, error)      { return len(p), nil }
func (discardConn) Close() error                     { return nil }
func (discardConn) LocalAddr() net.Addr              { return benchAddr{} }
func (discardConn) RemoteAddr() net.Addr             { return benchAddr{} }
func (discardConn) SetDeadline(time.Time) error      { return nil }
func (discardConn) SetReadDeadline(time.Time) error  { return nil }
func (discardConn) SetWriteDeadline(time.Time) error { return nil }

type benchAddr struct{}

func (benchAddr) Network() string { return "tcp" }
func (benchAddr) String() string  { return "127.0.0.1:0" }
