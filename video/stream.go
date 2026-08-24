package video

import (
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/panjf2000/gnet/v2"
)

// sink is where a response's bytes go.
//
// The handler writes through this interface rather than to gnet.Conn
// directly for one reason: a test must be able to read back the exact
// bytes that would have hit the wire. Without it, verifying that a 206
// carries the right Content-Range, or that a stream is chunked at the
// configured size, would need a live socket and a parsed client — which
// tests the network stack, not this package.
type sink interface {
	// write sends p. Implementations must not retain p; the caller
	// reuses the buffer on the next iteration.
	write(p []byte) error
}

// connSink writes to a gnet connection.
type connSink struct {
	conn gnet.Conn

	// bufs supplies the hand-off buffers described in write. It is the
	// mount's pool, so the buffers are already the right size.
	bufs *sync.Pool

	// wrote records whether any byte has left. Once a head is on the
	// wire the status is fixed, so a mid-stream failure must not try to
	// send an error response on top of it.
	wrote bool
}

// write forwards p to the connection, copying it first.
//
// The copy is not incidental — it is what makes streaming correct.
// AsyncWrite is asynchronous: it hands the slice to the event loop and
// returns before the bytes are flushed. The caller's buffer is reused on
// the next read, so passing it directly lets chunk N+1 overwrite chunk N
// while gnet still has it queued. The client then receives bytes that are
// the right *length* but the wrong *content*, which for video means a
// player that loads, reports a duration, and refuses to decode.
//
// So each write borrows a buffer from the pool, copies into it, and hands
// ownership to gnet; the completion callback returns it. Steady state is
// still allocation-free, and the memcpy is far cheaper than the syscall it
// accompanies.
func (s *connSink) write(p []byte) error {
	if len(p) == 0 {
		return nil
	}

	var (
		out  []byte
		done func()
	)
	if bufp := s.bufs.Get().(*[]byte); cap(*bufp) >= len(p) {
		out = (*bufp)[:len(p)]
		done = func() { s.bufs.Put(bufp) }
	} else {
		// A head longer than the chunk size cannot happen with the
		// headers this package sets, but sizing off the pool rather than
		// assuming keeps the copy safe if that ever changes.
		s.bufs.Put(bufp)
		out = make([]byte, len(p))
		done = func() {}
	}
	copy(out, p)

	if err := s.conn.AsyncWrite(out, func(gnet.Conn, error) error {
		done()
		return nil
	}); err != nil {
		done()
		return err
	}
	s.wrote = true
	return nil
}

// bufferPool hands out read buffers of a fixed size.
//
// A pool per mount rather than one global pool, because chunk size is a
// per-mount setting and mixing sizes in one pool would either hand back
// buffers that are too small or waste the difference.
func newBufferPool(size int) *sync.Pool {
	return &sync.Pool{
		New: func() any {
			b := make([]byte, size)
			return &b
		},
	}
}

// copyRange streams r's bytes from f into w, returning how many bytes of
// body were sent.
//
// The count is returned even on error because the caller needs it: bytes
// already on the wire cannot be recalled, and reporting a partial transfer
// as "0 sent" would make a client disconnect at 90% look identical to one
// that never started.
func (m *mount) copyRange(w sink, f *os.File, r byteRange) (int64, error) {
	if r.Start > 0 {
		if _, err := f.Seek(r.Start, io.SeekStart); err != nil {
			return 0, err
		}
	}

	bufp := m.bufs.Get().(*[]byte)
	defer m.bufs.Put(bufp)
	buf := *bufp

	var sent int64
	remaining := r.Length()
	for remaining > 0 {
		n := int64(len(buf))
		if remaining < n {
			n = remaining
		}
		read, rerr := f.Read(buf[:n])
		if read > 0 {
			if werr := w.write(buf[:read]); werr != nil {
				// The peer went away, which is the single most common
				// outcome in video: every seek and every closed tab
				// aborts a transfer in flight. It is not a server fault.
				return sent, werr
			}
			sent += int64(read)
			remaining -= int64(read)
		}
		if rerr == io.EOF {
			// Fewer bytes exist than Content-Length promised, meaning
			// the file was truncated after it was stat'd. Nothing can
			// be done — the head is already sent — so stop and let the
			// caller report a short transfer.
			break
		}
		if rerr != nil {
			return sent, rerr
		}
	}
	return sent, nil
}

// writeBody opens the file and streams the selected range.
func (m *mount) writeBody(w sink, res resolved, r byteRange) (int64, error) {
	f, err := os.Open(res.Path)
	if err != nil {
		// The file existed at stat time and does not now, or permissions
		// changed underneath. Either way the client learns nothing.
		return 0, err
	}
	defer f.Close()
	return m.copyRange(w, f, r)
}

// errText is the body sent with an error status.
//
// The text is the generic reason phrase, never the internal error. A
// message like `open /srv/media/../../etc/shadow: permission denied`
// confirms both the traversal and the file's existence; "Not Found"
// confirms nothing. The detailed error goes to OnError and the collector.
func errText(status int) string {
	if r := httpReason[status]; r != "" {
		return r
	}
	return "Error"
}

// writeError sends a complete error response.
//
// It is a no-op once anything has been written: a 404 appended to a
// half-streamed 206 would be parsed by the client as body content, which
// is worse than an abruptly closed connection.
func (m *mount) writeError(w sink, origin string, status int, size int64) error {
	if cs, ok := w.(*connSink); ok && cs.wrote {
		return nil
	}
	body := errText(status)
	h := newHead(status).
		set("Content-Type", "text/plain; charset=utf-8").
		setInt("Content-Length", int64(len(body))).
		set("Cache-Control", "no-store")

	// A 416 must state the true length so the client can retry with a
	// range that exists; "*/size" is the whole point of the status.
	if status == 416 {
		h.set("Content-Range", "bytes */"+strconv.FormatInt(size, 10))
	}

	// Advertising range support on a rejection is what lets a player
	// recover instead of falling back to a full download.
	h.set("Accept-Ranges", "bytes")
	m.applyCORS(h, origin)

	if err := w.write(h.bytes()); err != nil {
		return err
	}
	return w.write([]byte(body))
}

// applyCORS adds the cross-origin headers when the request's origin is
// allowed.
//
// Vary: Origin is not optional. The reply differs per origin, so a shared
// cache that stored one origin's response and replayed it to another would
// either leak access or wrongly deny it. It is emitted even when the
// origin is refused, because the refusal is itself origin-dependent.
func (m *mount) applyCORS(h *head, origin string) {
	if !m.anyOrigin && len(m.origins) == 0 {
		return
	}
	h.set("Vary", "Origin")
	if origin == "" {
		return
	}
	allowed := m.anyOrigin
	if !allowed {
		_, allowed = m.origins[origin]
	}
	if !allowed {
		return
	}
	if m.anyOrigin {
		// Echoing "*" rather than the origin keeps the response cacheable
		// for every origin at once, which is safe precisely because no
		// credentials are involved.
		h.set("Access-Control-Allow-Origin", "*")
	} else {
		h.set("Access-Control-Allow-Origin", origin)
	}
	// Without this, JavaScript can read the body but not the headers, so
	// a player cannot discover the total length from Content-Range.
	h.set("Access-Control-Expose-Headers", "Content-Length, Content-Range, Accept-Ranges, ETag, Last-Modified")
}

// isPeerGone reports whether err is a transport failure rather than a
// server fault.
//
// These are separated so a dashboard is not filled with red for what is
// ordinary viewer behaviour: seeking, pausing, or closing a tab all abort
// an in-flight response.
func isPeerGone(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.ErrClosedPipe) || errors.Is(err, os.ErrClosed) {
		return true
	}
	s := err.Error()
	for _, frag := range []string{
		"broken pipe",
		"connection reset",
		"connection is closed",
		"use of closed",
		"aborted",
		"forcibly closed",
	} {
		if strings.Contains(s, frag) {
			return true
		}
	}
	return false
}
