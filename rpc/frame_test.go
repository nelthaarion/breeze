package rpc

import (
	"bytes"
	"strings"
	"sync"
	"testing"

	json "github.com/goccy/go-json"
	"github.com/panjf2000/gnet/v2"
)

// frame_test.go — the framer and the event-loop path.
//
// These tests exercise what Handle cannot reach: the code that turns a byte
// stream into messages, and the OnTraffic bookkeeping around it. That path is
// where the interesting failures live — a message split across two reads, two
// messages in one read, a partial message that never completes — and none of
// them are reachable through an API that takes a whole message at a time.

// fakeConn is a gnet.Conn that reads from a fixed buffer and records writes.
//
// The embedded interface is nil, so any method this package does not use will
// panic rather than silently return a zero value. That is deliberate: if a
// future change starts calling something else on the connection, the test says
// so instead of quietly passing against a fake that lied.
type fakeConn struct {
	gnet.Conn

	inbound []byte
	written bytes.Buffer
	ctx     any

	mu    sync.Mutex
	async bytes.Buffer
}

// Next returns the whole pending buffer, mirroring the c.Next(-1) the server
// calls.
func (f *fakeConn) Next(n int) ([]byte, error) {
	if n < 0 || n > len(f.inbound) {
		n = len(f.inbound)
	}
	buf := f.inbound[:n]
	f.inbound = f.inbound[n:]
	return buf, nil
}

func (f *fakeConn) Write(b []byte) (int, error) { return f.written.Write(b) }
func (f *fakeConn) Context() any                { return f.ctx }
func (f *fakeConn) SetContext(v any)            { f.ctx = v }

func (f *fakeConn) AsyncWrite(b []byte, _ gnet.AsyncCallback) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.async.Write(b)
	return nil
}

func (f *fakeConn) asyncBytes() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.async.Bytes()...)
}

// feed delivers data as one read event and returns the action the server took.
func feed(s *Server, c *fakeConn, data string) gnet.Action {
	c.inbound = append(c.inbound, data...)
	return s.OnTraffic(c)
}

// TestFramingSingleMessage is the baseline: one message, one read.
func TestFramingSingleMessage(t *testing.T) {
	s := testServer(t)
	c := &fakeConn{}

	feed(s, c, `{"jsonrpc":"2.0","method":"sum","params":[1,2],"id":1}`)

	m := decode(t, c.written.Bytes())
	if got := assertResult(t, m); got != float64(3) {
		t.Errorf("result = %v, want 3", got)
	}
}

// TestFramingNewlineDelimited covers the framing most clients use.
func TestFramingNewlineDelimited(t *testing.T) {
	s := testServer(t)
	c := &fakeConn{}

	feed(s, c, `{"jsonrpc":"2.0","method":"sum","params":[1],"id":1}`+"\n"+
		`{"jsonrpc":"2.0","method":"sum","params":[2],"id":2}`+"\n")

	// Two responses arrive back to back in one write, which is what makes
	// pipelining one syscall instead of two. They are separate JSON values, so
	// a stream decoder is the right way to read them.
	got := decodeStream(t, c.written.Bytes())
	if len(got) != 2 {
		t.Fatalf("got %d responses, want 2: %s", len(got), c.written.Bytes())
	}
	if got[0]["id"] != float64(1) || got[1]["id"] != float64(2) {
		t.Errorf("ids = %v, %v; want 1, 2", got[0]["id"], got[1]["id"])
	}
}

// TestFramingPackedWithNoSeparator covers messages concatenated with nothing
// between them, which is legal on a stream and which a newline-splitting server
// would treat as one unparseable blob.
func TestFramingPackedWithNoSeparator(t *testing.T) {
	s := testServer(t)
	c := &fakeConn{}

	feed(s, c, `{"jsonrpc":"2.0","method":"sum","params":[1],"id":1}`+
		`{"jsonrpc":"2.0","method":"sum","params":[2],"id":2}`+
		`{"jsonrpc":"2.0","method":"sum","params":[3],"id":3}`)

	got := decodeStream(t, c.written.Bytes())
	if len(got) != 3 {
		t.Fatalf("got %d responses, want 3: %s", len(got), c.written.Bytes())
	}
}

// TestFramingSplitAcrossReads is the case the scan state exists for.
//
// TCP does not preserve message boundaries, so a request can arrive in as many
// pieces as the network chooses. Every prefix must produce no response, and the
// final byte must produce exactly one.
func TestFramingSplitAcrossReads(t *testing.T) {
	msg := `{"jsonrpc":"2.0","method":"sum","params":[1,2,3],"id":7}`

	// Split at every possible offset, so this covers a break in the middle of a
	// key, a value, a string, and a number — not just one arbitrary point.
	for split := 1; split < len(msg); split++ {
		s := testServer(t)
		c := &fakeConn{}

		if feed(s, c, msg[:split]) != gnet.None {
			t.Fatalf("split %d: server closed the connection on a partial message", split)
		}
		if c.written.Len() != 0 {
			t.Fatalf("split %d: partial message produced a response: %s", split, c.written.Bytes())
		}

		feed(s, c, msg[split:])

		m := decode(t, c.written.Bytes())
		if got := assertResult(t, m); got != float64(6) {
			t.Errorf("split %d: result = %v, want 6", split, got)
		}
	}
}

// TestFramingSplitByteByByte is the pathological case: one byte per read.
//
// It catches a scan-state bug that a two-way split would miss, because it forces
// the resume path through every single offset in sequence rather than once.
func TestFramingSplitByteByByte(t *testing.T) {
	s := testServer(t)
	c := &fakeConn{}

	msg := `{"jsonrpc":"2.0","method":"subtract","params":{"minuend":42,"subtrahend":23},"id":"x"}`
	for i := 0; i < len(msg); i++ {
		feed(s, c, msg[i:i+1])
		if i < len(msg)-1 && c.written.Len() != 0 {
			t.Fatalf("responded after %d of %d bytes: %s", i+1, len(msg), c.written.Bytes())
		}
	}

	m := decode(t, c.written.Bytes())
	if got := assertResult(t, m); got != float64(19) {
		t.Errorf("result = %v, want 19", got)
	}
}

// TestFramingBatchSplitAcrossReads covers a batch arriving in pieces.
func TestFramingBatchSplitAcrossReads(t *testing.T) {
	s := testServer(t)
	c := &fakeConn{}

	batch := `[{"jsonrpc":"2.0","method":"sum","params":[1],"id":1},` +
		`{"jsonrpc":"2.0","method":"sum","params":[2],"id":2}]`

	half := len(batch) / 2
	feed(s, c, batch[:half])
	if c.written.Len() != 0 {
		t.Fatalf("partial batch produced a response: %s", c.written.Bytes())
	}
	feed(s, c, batch[half:])

	got := decodeBatch(t, c.written.Bytes())
	if len(got) != 2 {
		t.Fatalf("got %d responses, want 2", len(got))
	}
}

// TestFramingStringsWithBracesAndQuotes is the scanner's correctness case.
//
// A brace inside a string literal must not change the depth count, an escaped
// quote must not end the string, and an escaped backslash must not escape the
// quote that follows it. Getting any of these wrong frames the message at the
// wrong byte, and the failure looks like a parse error on input that is
// perfectly valid.
func TestFramingStringsWithBracesAndQuotes(t *testing.T) {
	reg := NewRegistry()
	reg.Register("echo", func(ctx *Context) {
		var p []string
		if err := ctx.Bind(&p); err != nil {
			return
		}
		ctx.Result(p[0])
	})
	s := NewServer(reg)

	for name, payload := range map[string]string{
		"braces in string":         `{}[]`,
		"quote escaped":            `he said \"hi\"`,
		"backslash then quote":     `ends with backslash\\`,
		"json inside a string":     `{\"nested\":[1,2,{\"deep\":true}]}`,
		"unbalanced brace":         `{{{`,
		"unbalanced close":         `}]}`,
		"comma and colon":          `a,b:c`,
		"newline escape":           `line1\nline2`,
		"backslash before escaped": `\\\"`,
	} {
		t.Run(name, func(t *testing.T) {
			c := &fakeConn{}
			feed(s, c, `{"jsonrpc":"2.0","method":"echo","params":["`+payload+`"],"id":1}`)

			out := c.written.Bytes()
			if len(out) == 0 {
				t.Fatalf("no response — the message was mis-framed")
			}
			m := decode(t, out)
			if _, isErr := m["error"]; isErr {
				t.Fatalf("mis-framed, got error %v", m["error"])
			}
		})
	}
}

// TestFramingWhitespaceOnlyIsDiscarded covers a client that heartbeats with
// newlines.
//
// Those bytes are not a partial message, and treating them as one would grow the
// pending buffer until the size guard closed a connection that had done nothing
// wrong.
func TestFramingWhitespaceOnlyIsDiscarded(t *testing.T) {
	s := testServer(t)
	c := &fakeConn{}

	for i := 0; i < 100; i++ {
		if act := feed(s, c, "\n\r\t "); act != gnet.None {
			t.Fatalf("whitespace closed the connection after %d heartbeats", i)
		}
	}

	if c.ctx != nil {
		if st, ok := c.ctx.(*connState); ok && len(st.pending) > 0 {
			t.Errorf("whitespace accumulated %d pending bytes", len(st.pending))
		}
	}
	if c.written.Len() != 0 {
		t.Errorf("whitespace produced a response: %s", c.written.Bytes())
	}

	// A real message after the heartbeats still works.
	feed(s, c, `{"jsonrpc":"2.0","method":"sum","params":[5],"id":1}`)
	if got := assertResult(t, decode(t, c.written.Bytes())); got != float64(5) {
		t.Errorf("result = %v, want 5", got)
	}
}

// TestFramingGarbageClosesConnection covers input that is not JSON at all.
//
// There is no defined point at which to resume reading a stream that is not
// carrying JSON, so the server answers with a parse error and closes rather than
// spinning on bytes that will never frame.
func TestFramingGarbageClosesConnection(t *testing.T) {
	for name, garbage := range map[string]string{
		"binary":       "\x00\x01\x02\xff",
		"stray colon":  `:`,
		"stray comma":  `,`,
		"http request": "GET / HTTP/1.1\r\n\r\n",
		"close first":  `}`,
	} {
		t.Run(name, func(t *testing.T) {
			s := testServer(t)
			c := &fakeConn{}

			if act := feed(s, c, garbage); act != gnet.Close {
				t.Errorf("action = %v, want Close", act)
			}
			assertErrorCode(t, decode(t, c.written.Bytes()), CodeParseError)
		})
	}
}

// TestFramingRecoversAfterAParseErrorInAFramedMessage checks that a message
// which frames but does not parse leaves the stream usable.
//
// This is the distinction between the framer's job and the decoder's: `{"a"}`
// is structurally complete, so the framer consumes exactly it, and the next
// message is still readable. Closing the connection here would be wrong — the
// client sent one bad message, not a corrupt stream.
func TestFramingRecoversAfterAParseErrorInAFramedMessage(t *testing.T) {
	s := testServer(t)
	c := &fakeConn{}

	act := feed(s, c, `{"a"}`+"\n"+`{"jsonrpc":"2.0","method":"sum","params":[9],"id":2}`)
	if act == gnet.Close {
		t.Fatal("a single malformed message closed the connection")
	}

	got := decodeStream(t, c.written.Bytes())
	if len(got) != 2 {
		t.Fatalf("got %d responses, want 2: %s", len(got), c.written.Bytes())
	}
	assertErrorCode(t, got[0], CodeParseError)
	if r := assertResult(t, got[1]); r != float64(9) {
		t.Errorf("second result = %v, want 9", r)
	}
}

// TestFramingNotificationWritesNothing checks the event-loop path agrees with
// Handle: a notification produces no bytes on the wire at all.
func TestFramingNotificationWritesNothing(t *testing.T) {
	s := testServer(t)
	c := &fakeConn{}

	feed(s, c, `{"jsonrpc":"2.0","method":"update","params":[1]}`+"\n")

	if c.written.Len() != 0 {
		t.Errorf("notification wrote %s, want nothing", c.written.Bytes())
	}
}

// TestFramingMixedNotificationsAndRequests covers a pipelined stream where only
// some messages are answered.
func TestFramingMixedNotificationsAndRequests(t *testing.T) {
	s := testServer(t)
	c := &fakeConn{}

	feed(s, c, strings.Join([]string{
		`{"jsonrpc":"2.0","method":"update","params":[0]}`,
		`{"jsonrpc":"2.0","method":"sum","params":[1],"id":1}`,
		`{"jsonrpc":"2.0","method":"update","params":[0]}`,
		`{"jsonrpc":"2.0","method":"sum","params":[2],"id":2}`,
	}, "\n"))

	got := decodeStream(t, c.written.Bytes())
	if len(got) != 2 {
		t.Fatalf("got %d responses, want 2: %s", len(got), c.written.Bytes())
	}
	if got[0]["id"] != float64(1) || got[1]["id"] != float64(2) {
		t.Errorf("ids = %v, %v; want 1, 2", got[0]["id"], got[1]["id"])
	}
}

// TestMaxMessageBytesClosesConnection covers the memory guard.
//
// Without it, a client can open a brace and go quiet, and the server holds the
// partial buffer for the life of the connection — with the size chosen by the
// client, across as many connections as it opens.
func TestMaxMessageBytesClosesConnection(t *testing.T) {
	s := testServer(t)
	s.SetMaxMessageBytes(256)
	c := &fakeConn{}

	// An array that is never closed: structurally incomplete forever.
	feed(s, c, `[`+strings.Repeat(`{"jsonrpc":"2.0","method":"sum","params":[1],"id":1},`, 20))

	if act := s.OnTraffic(c); act != gnet.Close && c.written.Len() == 0 {
		t.Errorf("oversized partial message was neither answered nor closed (action %v)", act)
	}
}

// TestMaxMessageBytesAllowsMessagesUnderTheCap is the other half of that guard:
// a legitimate large batch must still work.
func TestMaxMessageBytesAllowsMessagesUnderTheCap(t *testing.T) {
	s := testServer(t)
	s.SetMaxMessageBytes(1 << 20)
	c := &fakeConn{}

	var sb strings.Builder
	sb.WriteByte('[')
	for i := 0; i < 200; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"jsonrpc":"2.0","method":"sum","params":[1],"id":1}`)
	}
	sb.WriteByte(']')

	if act := feed(s, c, sb.String()); act != gnet.None {
		t.Fatalf("action = %v, want None", act)
	}
	if got := decodeBatch(t, c.written.Bytes()); len(got) != 200 {
		t.Errorf("got %d responses, want 200", len(got))
	}
}

// TestSetMaxMessageBytesZeroRestoresDefault covers the argument guard.
func TestSetMaxMessageBytesZeroRestoresDefault(t *testing.T) {
	s := testServer(t)
	s.SetMaxMessageBytes(0)
	if s.maxMessageBytes != defaultMaxMessageBytes {
		t.Errorf(
			"maxMessageBytes = %d, want the default %d",
			s.maxMessageBytes,
			defaultMaxMessageBytes,
		)
	}
	s.SetMaxMessageBytes(-5)
	if s.maxMessageBytes != defaultMaxMessageBytes {
		t.Errorf("maxMessageBytes = %d after a negative value, want the default", s.maxMessageBytes)
	}
}

// TestConnStateClearedWhenNothingPending checks that the connection context is
// released once the stream is drained, so the pending array can be collected.
func TestConnStateClearedWhenNothingPending(t *testing.T) {
	s := testServer(t)
	c := &fakeConn{}

	feed(s, c, `{"jsonrpc":"2.0","method":"sum","params":[1],`)
	if c.ctx == nil {
		t.Fatal("partial message did not store connection state")
	}

	feed(s, c, `"id":1}`)
	if c.ctx != nil {
		t.Errorf("connection state = %v after the message completed, want nil", c.ctx)
	}
}

// TestOnCloseClearsContext covers the teardown path.
func TestOnCloseClearsContext(t *testing.T) {
	s := testServer(t)
	c := &fakeConn{}

	feed(s, c, `{"jsonrpc":"2.0","method":"sum",`)
	if c.ctx == nil {
		t.Fatal("expected pending state")
	}

	s.OnClose(c, nil)
	if c.ctx != nil {
		t.Errorf("context = %v after OnClose, want nil", c.ctx)
	}
}

// TestEmptyReadIsANoOp covers the guard at the top of OnTraffic.
func TestEmptyReadIsANoOp(t *testing.T) {
	s := testServer(t)
	c := &fakeConn{}

	if act := s.OnTraffic(c); act != gnet.None {
		t.Errorf("action = %v on an empty read, want None", act)
	}
	if c.written.Len() != 0 {
		t.Errorf("empty read wrote %s", c.written.Bytes())
	}
}

// ─── Blocking handoff ────────────────────────────────────────────────────────

// syncPool runs submitted work immediately, so the handoff path is testable
// without waiting on a real pool's goroutines.
//
// It is not a realistic pool — the point is to verify that the work is routed to
// the pool at all and that the reply goes out via AsyncWrite, which is the part
// a real pool would make racy to observe.
type syncPool struct{ submitted int }

func (p *syncPool) Submit(fn func()) {
	p.submitted++
	fn()
}

// TestBlockingMethodGoesToThePool checks that a blocking method is handed off
// and answered asynchronously.
//
// The distinction matters because a blocking handler run inline stalls every
// connection pinned to that event loop — the failure is invisible in a
// single-connection test and catastrophic under load.
func TestBlockingMethodGoesToThePool(t *testing.T) {
	reg := NewRegistry()
	reg.RegisterBlocking("slow", func(ctx *Context) {
		ctx.Result("done")
	})
	s := NewServer(reg)
	pool := &syncPool{}
	s.SetPool(pool)

	c := &fakeConn{}
	feed(s, c, `{"jsonrpc":"2.0","method":"slow","id":1}`)

	if pool.submitted != 1 {
		t.Errorf("pool received %d submissions, want 1", pool.submitted)
	}
	// The reply must go out through AsyncWrite, not the synchronous Write: off
	// the event loop, Write is not safe to call.
	if c.written.Len() != 0 {
		t.Errorf("blocking reply used the synchronous Write: %s", c.written.Bytes())
	}
	if got := assertResult(t, decode(t, c.asyncBytes())); got != "done" {
		t.Errorf("result = %v, want \"done\"", got)
	}
}

// TestInlineMethodDoesNotGoToThePool is the other half: an ordinary method must
// stay on the event loop and use the synchronous write.
func TestInlineMethodDoesNotGoToThePool(t *testing.T) {
	reg := NewRegistry()
	reg.Register("fast", func(ctx *Context) { ctx.Result("ok") })
	reg.RegisterBlocking("slow", func(ctx *Context) { ctx.Result("slow") })
	s := NewServer(reg)
	pool := &syncPool{}
	s.SetPool(pool)

	c := &fakeConn{}
	feed(s, c, `{"jsonrpc":"2.0","method":"fast","id":1}`)

	if pool.submitted != 0 {
		t.Errorf("inline method was submitted to the pool %d times", pool.submitted)
	}
	if got := assertResult(t, decode(t, c.written.Bytes())); got != "ok" {
		t.Errorf("result = %v, want \"ok\"", got)
	}
}

// TestBlockingBatchIsDeferredWhole checks that a batch containing any blocking
// method is handed off as a unit.
//
// Splitting it would mean writing part of the response array from the event loop
// and the rest from a worker, and the two halves could interleave with another
// connection's write — producing bytes no client can parse.
func TestBlockingBatchIsDeferredWhole(t *testing.T) {
	reg := NewRegistry()
	reg.Register("fast", func(ctx *Context) { ctx.Result("f") })
	reg.RegisterBlocking("slow", func(ctx *Context) { ctx.Result("s") })
	s := NewServer(reg)
	pool := &syncPool{}
	s.SetPool(pool)

	c := &fakeConn{}
	feed(
		s,
		c,
		`[{"jsonrpc":"2.0","method":"fast","id":1},{"jsonrpc":"2.0","method":"slow","id":2}]`,
	)

	if pool.submitted != 1 {
		t.Errorf("pool received %d submissions, want 1 for the whole batch", pool.submitted)
	}
	if c.written.Len() != 0 {
		t.Errorf("part of the batch was written synchronously: %s", c.written.Bytes())
	}

	got := decodeBatch(t, c.asyncBytes())
	if len(got) != 2 {
		t.Fatalf("got %d responses, want 2", len(got))
	}
}

// TestBlockingWithoutAPoolStillRunsOffTheLoop covers the no-pool fallback.
func TestBlockingWithoutAPoolStillRunsOffTheLoop(t *testing.T) {
	done := make(chan struct{})
	reg := NewRegistry()
	reg.RegisterBlocking("slow", func(ctx *Context) {
		ctx.Result("ok")
		close(done)
	})
	s := NewServer(reg)

	c := &fakeConn{}
	feed(s, c, `{"jsonrpc":"2.0","method":"slow","id":1}`)

	<-done // fails the test by timeout if the handler never ran
	if c.written.Len() != 0 {
		t.Errorf("blocking reply used the synchronous Write: %s", c.written.Bytes())
	}
}

// TestBlockingHandoffCopiesTheMessage is the memory-safety case for the handoff.
//
// The framed bytes point into gnet's inbound buffer, which is reused after
// OnTraffic returns. A handoff that passed that view to a worker would have the
// worker decoding whatever arrived next — reading another client's bytes in the
// worst case. The copy is what prevents it, and this test would catch its
// removal by mutating the buffer before the worker runs.
func TestBlockingHandoffCopiesTheMessage(t *testing.T) {
	var deferred []func()

	reg := NewRegistry()
	reg.RegisterBlocking("slow", func(ctx *Context) {
		var p []int
		if err := ctx.Bind(&p); err != nil {
			return
		}
		ctx.Result(p)
	})
	s := NewServer(reg)
	// A pool that queues instead of running, so the inbound buffer can be
	// overwritten in between — exactly what gnet does on the next read.
	s.SetPool(deferPool{&deferred})

	c := &fakeConn{}
	raw := []byte(`{"jsonrpc":"2.0","method":"slow","params":[1,2,3],"id":1}`)
	c.inbound = raw
	s.OnTraffic(c)

	// Scribble over the original bytes. If handoff kept a view rather than a
	// copy, the worker now sees garbage.
	for i := range raw {
		raw[i] = 'X'
	}

	for _, fn := range deferred {
		fn()
	}

	m := decode(t, c.asyncBytes())
	res := assertResult(t, m)
	nums, ok := res.([]any)
	if !ok || len(nums) != 3 {
		t.Fatalf("result = %v, want [1 2 3] — the handoff did not copy the message", res)
	}
}

// deferPool queues work instead of running it.
type deferPool struct{ into *[]func() }

func (p deferPool) Submit(fn func()) { *p.into = append(*p.into, fn) }

// ─── Scanner unit tests ──────────────────────────────────────────────────────

// TestNextValueDelimitsCorrectly exercises the scanner directly, where the
// boundaries are visible.
func TestNextValueDelimitsCorrectly(t *testing.T) {
	for name, tc := range map[string]struct {
		in   string
		want string
		res  scanResult
	}{
		"object":            {`{"a":1}`, `{"a":1}`, scanComplete},
		"array":             {`[1,2]`, `[1,2]`, scanComplete},
		"nested":            {`{"a":{"b":[1,{"c":2}]}}`, `{"a":{"b":[1,{"c":2}]}}`, scanComplete},
		"leading space":     {"  \n\t" + `{"a":1}`, `{"a":1}`, scanComplete},
		"trailing extra":    {`{"a":1}{"b":2}`, `{"a":1}`, scanComplete},
		"brace in string":   {`{"a":"}"}`, `{"a":"}"}`, scanComplete},
		"escaped quote":     {`{"a":"\""}`, `{"a":"\""}`, scanComplete},
		"escaped backslash": {`{"a":"\\"}`, `{"a":"\\"}`, scanComplete},
		"incomplete object": {`{"a":1`, ``, scanIncomplete},
		"incomplete string": {`{"a":"unterminated`, ``, scanIncomplete},
		"only whitespace":   {"   \n", ``, scanIncomplete},
		"empty":             {``, ``, scanIncomplete},
		"unopened close":    {`}`, ``, scanInvalid},
		"stray colon":       {`:`, ``, scanInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			var st scanState
			start, end, res := nextValue([]byte(tc.in), &st)
			if res != tc.res {
				t.Fatalf("result = %v, want %v", res, tc.res)
			}
			if res == scanComplete {
				if got := tc.in[start:end]; got != tc.want {
					t.Errorf("value = %q, want %q", got, tc.want)
				}
			}
		})
	}
}

// TestNextValueDelimitsBareScalars checks that a top-level scalar is consumed
// rather than left to jam the stream.
//
// None of these are valid JSON-RPC messages, but all are valid JSON values, and
// §4.2 requires the server to answer them with Invalid Request — which it cannot
// do without first knowing where they end.
func TestNextValueDelimitsBareScalars(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`1 `, `1`},
		{`123,`, `123`},
		{`true `, `true`},
		{`null}`, `null`},
		{`-4.5 `, `-4.5`},
		{`1 2`, `1`},
	} {
		var st scanState
		start, end, res := nextValue([]byte(tc.in), &st)
		if res != scanComplete {
			t.Errorf("nextValue(%q) = %v, want scanComplete", tc.in, res)
			continue
		}
		if got := tc.in[start:end]; got != tc.want {
			t.Errorf("nextValue(%q) delimited %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestNextValueScalarAtEndWaitsForMore covers the case where a scalar might
// continue into the next read: "12" could become "123".
func TestNextValueScalarAtEndWaitsForMore(t *testing.T) {
	var st scanState
	if _, _, res := nextValue([]byte(`12`), &st); res != scanIncomplete {
		t.Errorf("result = %v, want scanIncomplete — a trailing scalar may still grow", res)
	}
}

// TestNextValueResumesAcrossCalls checks the scan state carries correctly.
func TestNextValueResumesAcrossCalls(t *testing.T) {
	full := `{"a":{"b":"}}}"},"c":[1,2]}`

	for split := 1; split < len(full); split++ {
		var st scanState

		if _, _, res := nextValue([]byte(full[:split]), &st); res != scanIncomplete {
			t.Fatalf("split %d: prefix %q gave %v, want scanIncomplete", split, full[:split], res)
		}
		start, end, res := nextValue([]byte(full), &st)
		if res != scanComplete {
			t.Fatalf("split %d: resumed scan gave %v, want scanComplete", split, res)
		}
		if got := full[start:end]; got != full {
			t.Errorf("split %d: value = %q, want %q", split, got, full)
		}
	}
}

// ─── Helper ──────────────────────────────────────────────────────────────────

// decodeStream reads a sequence of concatenated JSON objects, which is what a
// pipelined write produces.
func decodeStream(t *testing.T, b []byte) []map[string]any {
	t.Helper()
	if len(b) == 0 {
		t.Fatal("expected responses, got none")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	var out []map[string]any
	for {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			break
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		t.Fatalf("no decodable responses in %s", b)
	}
	return out
}
