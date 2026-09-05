package video

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/panjf2000/gnet/v2"
)

// queuedConn models the part of gnet.Conn that matters here: AsyncWrite
// queues a slice and returns, flushing later.
//
// The embedded interface supplies the rest of the (large) method set. It is
// nil, so calling anything else panics — which is the point: this fake must
// stay honest about what the streaming path actually touches.
type queuedConn struct {
	gnet.Conn

	queued [][]byte
	acks   []gnet.AsyncCallback
}

// AsyncWrite retains p without copying, exactly as a queue would, and
// defers the completion callback. Retaining is what exposes the bug: if the
// caller hands over a buffer it intends to reuse, the queued bytes change
// underneath the queue.
func (c *queuedConn) AsyncWrite(p []byte, cb gnet.AsyncCallback) error {
	c.queued = append(c.queued, p)
	c.acks = append(c.acks, cb)
	return nil
}

// flush runs the deferred callbacks, as gnet does once bytes are on the wire.
func (c *queuedConn) flush() {
	for _, cb := range c.acks {
		if cb != nil {
			_ = cb(nil, nil)
		}
	}
	c.acks = nil
}

// patternFile writes n blocks of size chunk, each filled with a distinct
// letter, and returns the path and the expected contents.
//
// Distinct blocks are what make corruption visible: if a chunk is
// overwritten in flight, the wrong letter appears where the right one
// should be, instead of two identical bytes that hide the swap.
func patternFile(t *testing.T, dir string, chunk, n int) (string, []byte) {
	t.Helper()
	var want []byte
	for i := 0; i < n; i++ {
		want = append(want, bytes.Repeat([]byte{byte('A' + i)}, chunk)...)
	}
	path := filepath.Join(dir, "clip.mp4")
	if err := os.WriteFile(path, want, 0o644); err != nil {
		t.Fatal(err)
	}
	return path, want
}

// TestConnSinkDoesNotAliasCallerBuffer is a regression test for silent
// stream corruption.
//
// copyRange reads into one pooled buffer and reuses it for every chunk, so
// a sink that forwards that slice to an asynchronous writer lets chunk N+1
// overwrite chunk N while the queue still holds it. The client then receives
// the right number of bytes with the wrong contents — a video that loads and
// reports a duration but will not decode. Nothing in the status line or the
// headers reveals it.
//
// The original suite missed this because its fake sink copied on entry,
// making the test double safer than the real connection.
func TestConnSinkDoesNotAliasCallerBuffer(t *testing.T) {
	const (
		chunk  = 4
		blocks = 5
		size   = chunk * blocks
	)

	dir := t.TempDir()
	path, want := patternFile(t, dir, chunk, blocks)

	m, _ := testMount(t, nil, func(c *Config) {
		c.Root = dir
		c.ChunkSize = chunk
	})

	conn := &queuedConn{}
	sink := &connSink{conn: conn, bufs: m.bufs}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	sent, err := m.copyRange(sink, f, byteRange{Start: 0, End: size - 1})
	if err != nil {
		t.Fatalf("copyRange: %v", err)
	}
	if sent != size {
		t.Fatalf("sent = %d, want %d", sent, size)
	}

	// Inspect the queue *before* the callbacks run: that is the window in
	// which the queued bytes must already be independent of the read
	// buffer. Checking after the flush would pass even when broken.
	var got []byte
	for _, q := range conn.queued {
		got = append(got, q...)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("queued bytes corrupted by buffer reuse:\n got %q\nwant %q", got, want)
	}

	conn.flush()
}

// aliasSink is the sink this package used to have: it forwards the caller's
// slice to the connection without copying.
//
// It exists to prove the harness above can actually detect the bug. A
// regression test that passes against both the broken and the fixed code is
// worthless, so the old behaviour is kept here and asserted to corrupt.
type aliasSink struct{ conn *queuedConn }

func (s *aliasSink) write(p []byte) error {
	return s.conn.AsyncWrite(p, nil)
}

// TestAliasingSinkCorruptsStream demonstrates the bug.
//
// Same file, same chunking, same queue: the only difference is that the sink
// hands over the read buffer instead of a copy. Every queued chunk then
// shows the last chunk's contents, because they are all the same array.
// This is exactly what reached the browser — correct length, correct
// headers, wrong bytes.
func TestAliasingSinkCorruptsStream(t *testing.T) {
	const (
		chunk  = 4
		blocks = 5
		size   = chunk * blocks
	)

	dir := t.TempDir()
	path, want := patternFile(t, dir, chunk, blocks)

	m, _ := testMount(t, nil, func(c *Config) {
		c.Root = dir
		c.ChunkSize = chunk
	})

	conn := &queuedConn{}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if _, err := m.copyRange(
		&aliasSink{conn: conn},
		f,
		byteRange{Start: 0, End: size - 1},
	); err != nil {
		t.Fatalf("copyRange: %v", err)
	}

	var got []byte
	for _, q := range conn.queued {
		got = append(got, q...)
	}
	if bytes.Equal(got, want) {
		t.Fatal(
			"aliasing sink produced correct bytes: the harness cannot detect the bug it exists to catch",
		)
	}
	t.Logf("aliasing corrupts the stream as expected: got %q, want %q", got, want)
}

// TestConnSinkReturnsBuffersOnAck checks that hand-off buffers go back to the
// pool once flushed.
//
// Without this the pool would allocate a fresh buffer per chunk and the
// package's per-request allocation guarantee — the whole reason for pooling
// — would quietly disappear under load.
func TestConnSinkReturnsBuffersOnAck(t *testing.T) {
	m, _ := testMount(t, nil, func(c *Config) { c.ChunkSize = 8 })

	conn := &queuedConn{}
	sink := &connSink{conn: conn, bufs: m.bufs}

	payload := []byte("12345678")
	if err := sink.write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(conn.queued) != 1 {
		t.Fatalf("queued %d writes, want 1", len(conn.queued))
	}

	// The queued slice must not be the caller's array.
	if &payload[0] == &conn.queued[0][0] {
		t.Fatal("queued slice aliases the caller's buffer")
	}

	conn.flush()

	// After the ack the buffer is back in the pool, so the next write reuses
	// it rather than allocating.
	reused := m.bufs.Get().(*[]byte)
	if cap(*reused) < len(payload) {
		t.Fatalf("pooled buffer has cap %d, want >= %d", cap(*reused), len(payload))
	}
	m.bufs.Put(reused)
}
