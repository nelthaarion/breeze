package rpc

// stdio.go — JSON-RPC 2.0 over a byte stream, one message per line.
//
// The gnet server in server.go is the primary transport: it owns an event loop,
// frames messages out of a connection buffer, and writes responses back. This
// file is the other transport, and it exists because some peers do not speak TCP
// at all. The Model Context Protocol, for one, runs a server as a child process
// and talks to it over the child's stdin and stdout.
//
// Both go through Server.Handle, so dispatch, middleware, batching and the error
// codes are shared. What differs is only framing and where the bytes come from:
//
//	gnet    length-prefixed-ish scanning over a reused connection buffer, because
//	        a TCP stream has no message boundaries and a connection may deliver
//	        half a request.
//	stdio   one JSON value per line, because that is what the peer writes.
//
// Newline framing is safe here for the same reason it is safe in MCP: encoding/json
// escapes literal newlines inside strings as \n, so a marshalled JSON value never
// contains a bare newline. A line is therefore always a whole message.

import (
	"bufio"
	"errors"
	"io"
	"os"
	"sync"
)

// defaultStdioMaxLine caps a single line. A stdio peer is usually a local
// process rather than an untrusted network client, but "usually" is not a
// security model: without a cap, a peer that never sends a newline makes the
// scanner buffer grow until the process dies, and the failure looks like a
// memory leak rather than a bad message.
const defaultStdioMaxLine = 8 << 20 // 8 MiB

// StdioServer serves JSON-RPC over a reader/writer pair.
//
// It is not a second implementation of anything: it holds a *Server and defers
// every decision about what a message means to it.
type StdioServer struct {
	srv *Server

	in  io.Reader
	out io.Writer

	// maxLine bounds one message. Zero means defaultStdioMaxLine.
	maxLine int

	// mu serialises writes. Handle is called from the read loop today, so
	// responses cannot interleave, but a handler that keeps its context and
	// answers later — or any future notification pushed from elsewhere — would
	// write concurrently, and two interleaved half-lines are unparseable to the
	// peer. Guarding the writer is cheaper than discovering that.
	mu sync.Mutex
}

// NewStdioServer returns a server reading from in and writing to out.
func NewStdioServer(srv *Server, in io.Reader, out io.Writer) *StdioServer {
	return &StdioServer{srv: srv, in: in, out: out}
}

// NewStdioServerOS returns a server on the process's own stdin and stdout.
//
// Anything a handler prints to stdout would land in the middle of the protocol
// stream and corrupt it, so a program using this should log to stderr.
func NewStdioServerOS(srv *Server) *StdioServer {
	return NewStdioServer(srv, os.Stdin, os.Stdout)
}

// SetMaxLine overrides the per-message cap. A non-positive value restores the
// default.
func (s *StdioServer) SetMaxLine(n int) {
	if n <= 0 {
		n = defaultStdioMaxLine
	}
	s.maxLine = n
}

// Server returns the underlying dispatcher, so a caller can register methods
// without holding both objects.
func (s *StdioServer) Server() *Server { return s.srv }

// Serve reads messages until the input ends, dispatching each one.
//
// It returns nil on a clean end of input: a peer closing its side is how a
// stdio session normally finishes, not a failure. A malformed message is not a
// reason to stop either — Handle answers it with a parse error and the loop
// continues, because the peer is still there and still able to send something
// valid. Only a broken stream ends the loop.
func (s *StdioServer) Serve() error {
	max := s.maxLine
	if max <= 0 {
		max = defaultStdioMaxLine
	}

	br := bufio.NewReaderSize(s.in, 64<<10)

	for {
		line, err := readLine(br, max)

		// A final line without a trailing newline is still a message, so it is
		// dispatched before the EOF is acted on.
		if len(line) > 0 {
			if werr := s.handleLine(line); werr != nil {
				return werr
			}
		}

		if err != nil {
			switch {
			case errors.Is(err, io.EOF):
				return nil

			case errors.Is(err, errLineTooLong):
				// Recoverable: readLine consumed through the newline, so the
				// stream is positioned at the start of the next message and the
				// session can continue.
				//
				// The peer is told rather than left waiting, and it is told the
				// same thing the gnet transport says when a frame exceeds its
				// limit. The id has to be null because the message was never
				// parsed — there is no id to echo.
				if werr := s.write(appendErrorResponse(nil,
					NewError(CodeInvalidRequest, "request too large"), nullID)); werr != nil {
					return werr
				}

			default:
				return err
			}
		}
	}
}

// handleLine dispatches one message and writes its response, if it has one.
func (s *StdioServer) handleLine(line []byte) error {
	line = trimLeadingSpace(line)
	if len(line) == 0 {
		// A blank line is not a message. Answering it with a parse error would
		// be defensible, but peers emit stray newlines and an unsolicited error
		// response is more confusing than silence.
		return nil
	}

	resp := s.srv.Handle(line)
	if len(resp) == 0 {
		// A notification, or a batch of them: the spec says nothing is written.
		return nil
	}
	return s.write(resp)
}

// write emits one framed message.
//
// The newline is part of the message as far as the peer is concerned, so it is
// written under the same lock as the body: releasing between the two would let
// another writer split a message in half.
func (s *StdioServer) write(msg []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.out.Write(msg); err != nil {
		return err
	}
	if _, err := s.out.Write(newline); err != nil {
		return err
	}
	if f, ok := s.out.(interface{ Flush() error }); ok {
		return f.Flush()
	}
	return nil
}

var newline = []byte{'\n'}

// readLine reads one newline-terminated line, returning it without the newline.
//
// bufio.Scanner would be the obvious choice and is not used: it reports a line
// over its buffer as a terminal error and gives no way to resynchronise, so one
// oversized message would end an otherwise healthy session. Reading by hand
// means an oversized line can be drained and reported while the loop survives.
//
// The returned slice is only valid until the next call.
func readLine(br *bufio.Reader, max int) ([]byte, error) {
	var (
		line     []byte
		overflow bool
	)

	for {
		chunk, err := br.ReadSlice('\n')

		switch {
		case err == nil:
			if len(line) == 0 {
				// The whole line was in the buffer, which is the common case:
				// return it directly rather than copying.
				return dropCR(chunk[:len(chunk)-1]), nil
			}
			line = append(line, chunk...)
			if overflow {
				return nil, errLineTooLong
			}
			return dropCR(line[:len(line)-1]), nil

		case errors.Is(err, bufio.ErrBufferFull):
			// Partial line. Keep accumulating, but stop growing once past the
			// cap: the rest is still consumed so the next line starts clean.
			if len(line)+len(chunk) > max {
				overflow = true
			}
			if !overflow {
				line = append(line, chunk...)
			}

		default:
			// Usually io.EOF. A stream can end without a final newline, and the
			// bytes before the end are still a message: ReadSlice returns them
			// alongside the error, so they have to be claimed here or the last
			// request of every session that does not end in a newline is
			// silently dropped.
			if overflow {
				return nil, err
			}
			if len(chunk) > 0 {
				line = append(line, chunk...)
			}
			if len(line) > 0 {
				return dropCR(line), err
			}
			return nil, err
		}
	}
}

var errLineTooLong = errors.New("rpc: stdio message exceeds the maximum line length")

// dropCR removes a trailing carriage return, so a peer writing CRLF is not
// handed a stray byte that makes its JSON invalid.
func dropCR(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] == '\r' {
		return b[:len(b)-1]
	}
	return b
}
