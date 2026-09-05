package rpc

// Tests for the stdio transport.
//
// The dispatcher is already covered thoroughly by rpc_test.go, and these
// deliberately do not re-test it: what matters here is only what stdio adds,
// which is framing. So the assertions are about line boundaries, notifications
// producing no line at all, malformed input not killing the session, and the
// stream surviving a message too large to buffer.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// newStdioFixture returns a stdio server over the given input, with two methods
// registered: echo, which returns its params, and boom, which fails.
func newStdioFixture(t *testing.T, input string) (*StdioServer, *bytes.Buffer) {
	t.Helper()

	srv := NewServer(NewRegistry())
	srv.Register("echo", func(ctx *Context) {
		var v any
		if len(ctx.Params) > 0 {
			if err := ctx.Bind(&v); err != nil {
				ctx.Errorf(CodeInvalidParams, "params must be JSON")
				return
			}
		}
		ctx.Result(v)
	})
	srv.Register("boom", func(ctx *Context) {
		ctx.Errorf(-32001, "boom")
	})
	srv.Register("notify", func(ctx *Context) {
		// A notification handler whose result must still be suppressed.
		ctx.Result("ignored")
	})

	var out bytes.Buffer
	return NewStdioServer(srv, strings.NewReader(input), &out), &out
}

// responseLines splits the transcript into non-empty lines, failing if any line
// is not a JSON object — the peer parses per line, so a line that is not one
// message is a protocol violation regardless of its contents.
func responseLines(t *testing.T, out *bytes.Buffer) []map[string]any {
	t.Helper()

	var msgs []map[string]any
	for _, line := range bytes.Split(out.Bytes(), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("response line is not a JSON object: %q (%v)", line, err)
		}
		msgs = append(msgs, m)
	}
	return msgs
}

func TestStdioSingleRequest(t *testing.T) {
	s, out := newStdioFixture(t, `{"jsonrpc":"2.0","id":1,"method":"echo","params":[1,2]}`+"\n")

	if err := s.Serve(); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	msgs := responseLines(t, out)
	if len(msgs) != 1 {
		t.Fatalf("got %d response lines, want 1: %q", len(msgs), out.String())
	}
	if msgs[0]["jsonrpc"] != "2.0" {
		t.Errorf("jsonrpc = %v, want 2.0", msgs[0]["jsonrpc"])
	}
	if msgs[0]["id"] != float64(1) {
		t.Errorf("id = %v, want 1", msgs[0]["id"])
	}
	if _, ok := msgs[0]["result"]; !ok {
		t.Errorf("no result member: %v", msgs[0])
	}
}

// TestStdioOneMessagePerLine is the framing claim: three requests arriving in
// one read must produce three separately-parseable lines.
func TestStdioOneMessagePerLine(t *testing.T) {
	input := `{"jsonrpc":"2.0","id":1,"method":"echo","params":["a"]}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"echo","params":["b"]}` + "\n" +
		`{"jsonrpc":"2.0","id":3,"method":"echo","params":["c"]}` + "\n"

	s, out := newStdioFixture(t, input)
	if err := s.Serve(); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	msgs := responseLines(t, out)
	if len(msgs) != 3 {
		t.Fatalf("got %d response lines, want 3: %q", len(msgs), out.String())
	}
	for i, want := range []float64{1, 2, 3} {
		if msgs[i]["id"] != want {
			t.Errorf(
				"line %d has id %v, want %v — responses are out of order",
				i,
				msgs[i]["id"],
				want,
			)
		}
	}
}

// TestStdioNotificationWritesNothing — the spec is explicit that a notification
// gets no response, and on a line-framed stream an empty line would be read as a
// message by a stricter peer.
func TestStdioNotificationWritesNothing(t *testing.T) {
	s, out := newStdioFixture(t, `{"jsonrpc":"2.0","method":"notify","params":[1]}`+"\n")

	if err := s.Serve(); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("a notification produced output: %q", out.String())
	}
}

// TestStdioMalformedJSONDoesNotEndTheSession is the resilience claim. A peer that
// sends one bad line is still a live peer, and dropping the session would turn a
// recoverable mistake into a dead server.
func TestStdioMalformedJSONDoesNotEndTheSession(t *testing.T) {
	input := "{not json\n" +
		`{"jsonrpc":"2.0","id":2,"method":"echo","params":["after"]}` + "\n"

	s, out := newStdioFixture(t, input)
	if err := s.Serve(); err != nil {
		t.Fatalf("Serve returned an error for a malformed message: %v", err)
	}

	msgs := responseLines(t, out)
	if len(msgs) != 2 {
		t.Fatalf(
			"got %d response lines, want 2 (a parse error then a result): %q",
			len(msgs),
			out.String(),
		)
	}

	errObj, ok := msgs[0]["error"].(map[string]any)
	if !ok {
		t.Fatalf("first response is not an error: %v", msgs[0])
	}
	if code := errObj["code"]; code != float64(CodeParseError) {
		t.Errorf("code = %v, want %d (parse error)", code, CodeParseError)
	}
	if _, ok := msgs[1]["result"]; !ok {
		t.Errorf("the request after the malformed one was not answered: %v", msgs[1])
	}
}

// TestStdioBatch — batching is the dispatcher's job, but the whole batch
// response still has to arrive as exactly one line.
func TestStdioBatch(t *testing.T) {
	input := `[{"jsonrpc":"2.0","id":1,"method":"echo","params":["a"]},` +
		`{"jsonrpc":"2.0","method":"notify"},` +
		`{"jsonrpc":"2.0","id":2,"method":"boom"}]` + "\n"

	s, out := newStdioFixture(t, input)
	if err := s.Serve(); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	lines := bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n"))
	if len(lines) != 1 {
		t.Fatalf("a batch produced %d lines, want 1: %q", len(lines), out.String())
	}

	var batch []map[string]any
	if err := json.Unmarshal(lines[0], &batch); err != nil {
		t.Fatalf("batch response is not a JSON array: %v", err)
	}
	// Two responses, not three: the notification contributes nothing.
	if len(batch) != 2 {
		t.Fatalf("batch has %d responses, want 2: %s", len(batch), lines[0])
	}
}

// TestStdioAllNotificationBatchWritesNothing — a batch containing only
// notifications produces no response document at all.
func TestStdioAllNotificationBatchWritesNothing(t *testing.T) {
	s, out := newStdioFixture(t,
		`[{"jsonrpc":"2.0","method":"notify"},{"jsonrpc":"2.0","method":"notify"}]`+"\n")

	if err := s.Serve(); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("an all-notification batch produced output: %q", out.String())
	}
}

// TestStdioFinalLineWithoutNewline — a peer that closes immediately after
// writing, without a trailing newline, has still sent a message.
func TestStdioFinalLineWithoutNewline(t *testing.T) {
	s, out := newStdioFixture(t, `{"jsonrpc":"2.0","id":9,"method":"echo","params":["x"]}`)

	if err := s.Serve(); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	msgs := responseLines(t, out)
	if len(msgs) != 1 || msgs[0]["id"] != float64(9) {
		t.Errorf("an unterminated final message was not answered: %q", out.String())
	}
}

// TestStdioHandlesCRLF — a peer on Windows, or one written against a
// line-oriented library, may end lines with CRLF. The stray CR would make the
// JSON invalid if it were left on.
func TestStdioHandlesCRLF(t *testing.T) {
	s, out := newStdioFixture(t, `{"jsonrpc":"2.0","id":1,"method":"echo","params":["x"]}`+"\r\n")

	if err := s.Serve(); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	msgs := responseLines(t, out)
	if len(msgs) != 1 {
		t.Fatalf("got %d lines, want 1: %q", len(msgs), out.String())
	}
	if _, isErr := msgs[0]["error"]; isErr {
		t.Errorf("a CRLF-terminated request was answered with an error: %v", msgs[0])
	}
}

// TestStdioBlankLinesAreSkipped — stray newlines are common from hand-driven
// peers and shells, and an unsolicited parse error in reply to one is noise.
func TestStdioBlankLinesAreSkipped(t *testing.T) {
	s, out := newStdioFixture(
		t,
		"\n\n"+`{"jsonrpc":"2.0","id":1,"method":"echo","params":["x"]}`+"\n\n",
	)

	if err := s.Serve(); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if msgs := responseLines(t, out); len(msgs) != 1 {
		t.Errorf("got %d responses, want exactly 1: %q", len(msgs), out.String())
	}
}

// TestStdioOversizedMessageDoesNotWedgeTheStream is why readLine is hand-written
// rather than a bufio.Scanner.
//
// A Scanner reports a too-long line as a terminal error with no way to
// resynchronise, so one oversized message would end an otherwise healthy
// session. Here the oversized line is consumed and the next message still
// arrives.
func TestStdioOversizedMessageDoesNotWedgeTheStream(t *testing.T) {
	huge := `{"jsonrpc":"2.0","id":1,"method":"echo","params":["` +
		strings.Repeat("x", 512<<10) + `"]}`

	input := huge + "\n" + `{"jsonrpc":"2.0","id":2,"method":"echo","params":["small"]}` + "\n"

	s, out := newStdioFixture(t, input)
	s.SetMaxLine(64 << 10) // smaller than the first message

	if err := s.Serve(); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	msgs := responseLines(t, out)
	if len(msgs) == 0 {
		t.Fatal("the oversized message wedged the stream; the following request was never answered")
	}
	last := msgs[len(msgs)-1]
	if last["id"] != float64(2) {
		t.Errorf(
			"last response has id %v, want 2 — the message after the oversized one was lost",
			last["id"],
		)
	}
}

// TestStdioWriteErrorEndsServe — if the peer's pipe is gone there is nowhere to
// put responses, and continuing to read would spin.
func TestStdioWriteErrorEndsServe(t *testing.T) {
	srv := NewServer(NewRegistry())
	srv.Register("echo", func(ctx *Context) { ctx.Result(1) })

	s := NewStdioServer(srv,
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"echo"}`+"\n"),
		errWriter{})

	if err := s.Serve(); err == nil {
		t.Error("Serve returned nil after the output pipe failed")
	}
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("pipe closed") }

// TestStdioConcurrentWritesAreSerialised guards the mutex. Handle is called from
// the read loop today, so this cannot happen yet; the test exists because the
// cost of the lock is nothing and interleaved half-lines would be an
// exceptionally confusing bug to diagnose from a peer's parse error.
func TestStdioConcurrentWritesAreSerialised(t *testing.T) {
	srv := NewServer(NewRegistry())
	srv.Register("echo", func(ctx *Context) { ctx.Result(strings.Repeat("x", 4096)) })

	var out lockedBuffer
	s := NewStdioServer(srv, strings.NewReader(""), &out)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.handleLine([]byte(`{"jsonrpc":"2.0","id":1,"method":"echo"}`)); err != nil {
				t.Errorf("handleLine: %v", err)
			}
		}()
	}
	wg.Wait()

	// Every line must be a complete message. Interleaving would show up as a
	// line that does not parse.
	if msgs := responseLines(t, &out.buf); len(msgs) != 16 {
		t.Errorf("got %d intact response lines, want 16", len(msgs))
	}
}

// lockedBuffer is a bytes.Buffer that is safe for concurrent writes, so the test
// above measures the server's serialisation rather than racing on the buffer.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

var _ io.Writer = (*lockedBuffer)(nil)

// ─── Benchmarks ──────────────────────────────────────────────────────────────
//
// BenchmarkHandle* already covers dispatch, so these measure only what stdio
// adds on top of it: reading a line, and writing a framed response. Comparing a
// row here against its BenchmarkHandle counterpart is what shows whether the
// framing is close to free, which is the only claim this file makes about
// performance.
//
// The reader is a repeating stream rather than one message replayed, because a
// single-message reader would return EOF on the second iteration and measure
// nothing.

// repeatReader yields the same payload endlessly without allocating per read,
// so the benchmark measures the server rather than the fixture.
type repeatReader struct {
	msg []byte
	off int
}

func (r *repeatReader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		c := copy(p[n:], r.msg[r.off:])
		n += c
		r.off += c
		if r.off == len(r.msg) {
			r.off = 0
		}
		if c == 0 {
			break
		}
	}
	return n, nil
}

// countingDiscard is an io.Writer that records how much it was given, so a
// benchmark can report throughput without keeping the bytes.
type countingDiscard struct{ n int64 }

func (d *countingDiscard) Write(p []byte) (int, error) {
	d.n += int64(len(p))
	return len(p), nil
}

// benchmarkStdio runs the read-dispatch-write loop b.N times over a repeating
// stream of msg.
//
// Serve cannot be used directly: it runs until EOF, and the stream here never
// ends. The loop below is what Serve does per message, which is exactly the part
// worth measuring.
//
// wantResponse says whether the message shape should produce output. It is not
// decoration: without it the sanity check below cannot tell a benchmark that
// measured nothing from a notification benchmark working exactly as intended.
func benchmarkStdio(b *testing.B, msg string, wantResponse bool) {
	b.Helper()

	srv := NewServer(NewRegistry())
	srv.Register("echo", func(ctx *Context) { ctx.ResultRaw(ctx.Params) })

	in := &repeatReader{msg: []byte(msg + "\n")}
	out := &countingDiscard{}
	s := NewStdioServer(srv, in, out)

	br := bufio.NewReaderSize(in, 64<<10)

	b.SetBytes(int64(len(msg)))
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		line, err := readLine(br, defaultStdioMaxLine)
		if err != nil {
			b.Fatalf("readLine: %v", err)
		}
		if err := s.handleLine(line); err != nil {
			b.Fatalf("handleLine: %v", err)
		}
	}

	b.StopTimer()

	// A benchmark that measured an empty loop would report a flattering number
	// for doing nothing, so the expectation is asserted rather than assumed.
	switch {
	case wantResponse && out.n == 0:
		b.Fatal("nothing was written; the benchmark measured an empty loop")
	case !wantResponse && out.n != 0:
		b.Fatalf("a notification produced %d bytes of output", out.n)
	}
}

// BenchmarkStdioSingle is the round trip for one request: read a line, dispatch,
// write a framed response.
func BenchmarkStdioSingle(b *testing.B) {
	benchmarkStdio(b, `{"jsonrpc":"2.0","id":1,"method":"echo","params":[1,2,3]}`, true)
}

// BenchmarkStdioNotification isolates the read side: a notification is
// dispatched but produces no response, so the gap against BenchmarkStdioSingle
// is what serialising and writing one costs.
func BenchmarkStdioNotification(b *testing.B) {
	benchmarkStdio(b, `{"jsonrpc":"2.0","method":"echo","params":[1,2,3]}`, false)
}

// BenchmarkStdioBatch measures a batch of N arriving as one line, which is the
// shape a client uses to amortise round trips.
func BenchmarkStdioBatch(b *testing.B) {
	for _, n := range []int{1, 10, 100} {
		b.Run("N="+strconv.Itoa(n), func(b *testing.B) {
			var sb strings.Builder
			sb.WriteByte('[')
			for i := 0; i < n; i++ {
				if i > 0 {
					sb.WriteByte(',')
				}
				sb.WriteString(`{"jsonrpc":"2.0","id":`)
				sb.WriteString(strconv.Itoa(i))
				sb.WriteString(`,"method":"echo","params":[1,2,3]}`)
			}
			sb.WriteByte(']')
			benchmarkStdio(b, sb.String(), true)
		})
	}
}

// BenchmarkStdioReadLine is the framing cost on its own, with no dispatch at
// all: it is the number to look at if the rows above ever regress, because it
// separates "the transport got slower" from "the dispatcher got slower".
func BenchmarkStdioReadLine(b *testing.B) {
	msg := `{"jsonrpc":"2.0","id":1,"method":"echo","params":[1,2,3]}`
	in := &repeatReader{msg: []byte(msg + "\n")}
	br := bufio.NewReaderSize(in, 64<<10)

	b.SetBytes(int64(len(msg)))
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if _, err := readLine(br, defaultStdioMaxLine); err != nil {
			b.Fatalf("readLine: %v", err)
		}
	}
}
