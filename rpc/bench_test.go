package rpc

import (
	"strconv"
	"strings"
	"testing"

	json "github.com/goccy/go-json"
)

// bench_test.go — the hot path.
//
// Two levels are measured separately, because they answer different questions:
//
//   - BenchmarkHandle* measures dispatch and serialization alone. This is the
//     number to watch when changing the decoder, the registry or the wire
//     format.
//   - BenchmarkOnTraffic* measures the full event-loop path, including framing
//     and the connection write. This is the number that corresponds to a real
//     request arriving on a socket.
//
// benchConn is the write sink for the OnTraffic benchmarks. It discards rather
// than buffering: a bytes.Buffer would grow across iterations and the
// reallocation would be attributed to the server.

type benchConn struct {
	fakeConn
	discarded int
}

func (b *benchConn) Write(p []byte) (int, error) {
	b.discarded += len(p)
	return len(p), nil
}

// benchServer registers the methods the benchmarks call.
//
// The handlers are deliberately trivial. The point is to measure the framework's
// overhead, and a handler that did real work would bury it — the same reason the
// router benchmarks in the root package use empty handlers.
func benchServer() *Server {
	reg := NewRegistry()

	reg.Register("noop", func(ctx *Context) {
		ctx.Result(true)
	})

	reg.Register("sum", func(ctx *Context) {
		var nums []int
		if err := ctx.Bind(&nums); err != nil {
			return
		}
		total := 0
		for _, n := range nums {
			total += n
		}
		ctx.Result(total)
	})

	reg.Register("echo_raw", func(ctx *Context) {
		// The zero-decode path: params forwarded straight back out with no
		// unmarshal and no re-encode.
		ctx.ResultRaw(json.RawMessage(ctx.Params))
	})

	reg.Register("notify", func(ctx *Context) {})

	return NewServer(reg)
}

// ─── Single request ──────────────────────────────────────────────────────────

// BenchmarkHandleSingle is the headline number: one request in, one response out.
func BenchmarkHandleSingle(b *testing.B) {
	s := benchServer()
	req := []byte(`{"jsonrpc":"2.0","method":"sum","params":[1,2,3],"id":1}`)

	b.ReportAllocs()
	b.SetBytes(int64(len(req)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if out := s.Handle(req); len(out) == 0 {
			b.Fatal("no response")
		}
	}
}

// BenchmarkHandleSingleNoParams isolates the envelope cost from the params
// decode, so a regression can be attributed to one or the other.
func BenchmarkHandleSingleNoParams(b *testing.B) {
	s := benchServer()
	req := []byte(`{"jsonrpc":"2.0","method":"noop","id":1}`)

	b.ReportAllocs()
	b.SetBytes(int64(len(req)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if out := s.Handle(req); len(out) == 0 {
			b.Fatal("no response")
		}
	}
}

// BenchmarkHandleRawResult measures the proxy path, where the result is already
// encoded and is copied rather than marshalled.
func BenchmarkHandleRawResult(b *testing.B) {
	s := benchServer()
	req := []byte(`{"jsonrpc":"2.0","method":"echo_raw","params":{"a":1,"b":[2,3]},"id":1}`)

	b.ReportAllocs()
	b.SetBytes(int64(len(req)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if out := s.Handle(req); len(out) == 0 {
			b.Fatal("no response")
		}
	}
}

// BenchmarkHandleNotification measures the path that produces no output, which
// isolates dispatch from serialization entirely.
func BenchmarkHandleNotification(b *testing.B) {
	s := benchServer()
	req := []byte(`{"jsonrpc":"2.0","method":"notify","params":[1,2,3]}`)

	b.ReportAllocs()
	b.SetBytes(int64(len(req)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		s.Handle(req)
	}
}

// ─── Error paths ─────────────────────────────────────────────────────────────

// BenchmarkHandleMethodNotFound measures the cheapest failure, which a scanning
// client can generate in volume.
func BenchmarkHandleMethodNotFound(b *testing.B) {
	s := benchServer()
	req := []byte(`{"jsonrpc":"2.0","method":"nonexistent","id":1}`)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		s.Handle(req)
	}
}

// BenchmarkHandleParseError measures the malformed path, which includes the
// json.Valid second pass used to choose between -32700 and -32600.
//
// It is measured because it is the one an abusive client controls, and a
// disproportionately expensive error path is a denial-of-service surface even
// when the success path is fast.
//
// Measured result: this is the most expensive path in the package by a wide
// margin — ~5.9 µs and 39 allocs/op against ~1.6 µs and 15 allocs/op for a
// successful call, a 3.6× asymmetry. The cost is the decoder allocating a
// syntax-error value and then json.Valid re-walking the bytes to decide between
// -32700 and -32600. It was left alone deliberately: ~170k malformed messages
// per second per core is not an amplification vector, the framer only hands up
// structurally complete values, and SetMaxMessageBytes bounds the input. If this
// ever needs to be faster, the fix is to classify from the framer's own scan
// rather than by re-validating, which would remove the second pass entirely —
// but that couples the framer to the decoder's notion of validity, which is not
// a trade worth making on these numbers.
func BenchmarkHandleParseError(b *testing.B) {

	s := benchServer()
	req := []byte(`{"jsonrpc":"2.0","method":"sum","params":[1,2,3],"id":}`)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		s.Handle(req)
	}
}

// ─── Batch ───────────────────────────────────────────────────────────────────

// makeBatch builds a batch of n requests.
func makeBatch(n int) []byte {
	var sb strings.Builder
	sb.WriteByte('[')
	for i := 0; i < n; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"jsonrpc":"2.0","method":"sum","params":[1,2,3],"id":`)
		sb.WriteString(strconv.Itoa(i))
		sb.WriteByte('}')
	}
	sb.WriteByte(']')
	return []byte(sb.String())
}

// BenchmarkHandleBatch sweeps the batch size.
//
// The sweep is the point: per-request cost should stay roughly flat as N grows,
// because the responses accumulate in one buffer. A cost that climbs with N
// means the buffer is being reallocated per element, which the single-grow in
// appendBatch exists to prevent — and which only shows up at scale.
func BenchmarkHandleBatch(b *testing.B) {
	for _, n := range []int{1, 10, 100, 1000} {
		b.Run("N="+strconv.Itoa(n), func(b *testing.B) {
			s := benchServer()
			req := makeBatch(n)

			b.ReportAllocs()
			b.SetBytes(int64(len(req)))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				if out := s.Handle(req); len(out) == 0 {
					b.Fatal("no response")
				}
			}
		})
	}
}

// BenchmarkHandleBatchNotifications measures a batch that produces no output at
// all, isolating the dispatch cost of a batch from its serialization.
func BenchmarkHandleBatchNotifications(b *testing.B) {
	s := benchServer()

	var sb strings.Builder
	sb.WriteByte('[')
	for i := 0; i < 100; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"jsonrpc":"2.0","method":"notify","params":[1]}`)
	}
	sb.WriteByte(']')
	req := []byte(sb.String())

	b.ReportAllocs()
	b.SetBytes(int64(len(req)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		s.Handle(req)
	}
}

// ─── Full event-loop path ────────────────────────────────────────────────────

// BenchmarkOnTrafficSingle measures a request arriving on a connection: framing,
// dispatch, serialization and write.
//
// This is the closest number to what a client experiences, and the one that
// exercises the pooled write buffer — which Handle does not, because it must
// return an owned slice.
func BenchmarkOnTrafficSingle(b *testing.B) {
	s := benchServer()
	req := `{"jsonrpc":"2.0","method":"sum","params":[1,2,3],"id":1}`
	c := &benchConn{}

	b.ReportAllocs()
	b.SetBytes(int64(len(req)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		c.inbound = append(c.inbound, req...)
		s.OnTraffic(c)
	}
}

// BenchmarkOnTrafficPipelined measures ten requests delivered in one read.
//
// This is what a batching client produces without using a JSON-RPC batch, and it
// is the case the single accumulating write buffer is for: ten responses, one
// write.
func BenchmarkOnTrafficPipelined(b *testing.B) {
	s := benchServer()

	var sb strings.Builder
	for i := 0; i < 10; i++ {
		sb.WriteString(`{"jsonrpc":"2.0","method":"sum","params":[1,2,3],"id":`)
		sb.WriteString(strconv.Itoa(i))
		sb.WriteString("}\n")
	}
	req := sb.String()
	c := &benchConn{}

	b.ReportAllocs()
	b.SetBytes(int64(len(req)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		c.inbound = append(c.inbound, req...)
		s.OnTraffic(c)
	}
}

// BenchmarkOnTrafficBatch measures a batch over the connection path.
func BenchmarkOnTrafficBatch(b *testing.B) {
	for _, n := range []int{10, 100} {
		b.Run("N="+strconv.Itoa(n), func(b *testing.B) {
			s := benchServer()
			req := makeBatch(n)
			c := &benchConn{}

			b.ReportAllocs()
			b.SetBytes(int64(len(req)))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				c.inbound = append(c.inbound, req...)
				s.OnTraffic(c)
			}
		})
	}
}

// BenchmarkOnTrafficSplit measures a request arriving in two reads.
//
// It is the reassembly path's cost, which a client on a slow or lossy link hits
// routinely — and which is invisible in a benchmark that always delivers whole
// messages.
func BenchmarkOnTrafficSplit(b *testing.B) {
	s := benchServer()
	req := `{"jsonrpc":"2.0","method":"sum","params":[1,2,3],"id":1}`
	half := len(req) / 2
	c := &benchConn{}

	b.ReportAllocs()
	b.SetBytes(int64(len(req)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		c.inbound = append(c.inbound, req[:half]...)
		s.OnTraffic(c)
		c.inbound = append(c.inbound, req[half:]...)
		s.OnTraffic(c)
	}
}

// ─── Components ──────────────────────────────────────────────────────────────

// BenchmarkNextValue measures the framer alone, which is the only part of the
// hot path this package fully controls — everything else is dominated by the
// JSON decoder.
func BenchmarkNextValue(b *testing.B) {
	msg := []byte(`{"jsonrpc":"2.0","method":"sum","params":[1,2,3],"id":1}`)

	b.ReportAllocs()
	b.SetBytes(int64(len(msg)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		var st scanState
		if _, _, res := nextValue(msg, &st); res != scanComplete {
			b.Fatal("scan failed")
		}
	}
}

// BenchmarkRegistryLookup measures the method table.
func BenchmarkRegistryLookup(b *testing.B) {
	reg := NewRegistry()
	for i := 0; i < 100; i++ {
		reg.Register("method_"+strconv.Itoa(i), func(ctx *Context) {})
	}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if _, ok := reg.lookup("method_50"); !ok {
			b.Fatal("lookup failed")
		}
	}
}

// BenchmarkAppendResponse measures the serializer alone, with no decode in the
// way.
func BenchmarkAppendResponse(b *testing.B) {
	ctx := &Context{
		ID:          json.RawMessage(`1`),
		resultValue: 42,
		hasResult:   true,
	}
	buf := make([]byte, 0, 256)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		buf = appendResponse(buf[:0], ctx)
	}
}

// BenchmarkAppendErrorResponse measures the error envelope, which is the path a
// misbehaving or probing client drives.
func BenchmarkAppendErrorResponse(b *testing.B) {
	e := ErrMethodNotFound()
	buf := make([]byte, 0, 256)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		buf = appendErrorResponse(buf[:0], e, json.RawMessage(`1`))
	}
}
