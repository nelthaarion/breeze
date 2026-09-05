package rpc

import (
	"strconv"
	"sync"

	json "github.com/goccy/go-json"
)

// wire.go — serializing responses.
//
// Responses are appended into a caller-supplied buffer rather than marshalled
// into a fresh []byte, for the same reason HTTPResponse has AppendTo: a batch
// of N replies is one buffer and one write, not N marshals and N writes. The
// envelope is fixed by the specification, so it is emitted as literal byte
// appends instead of by encoding a struct — there is nothing for a reflective
// encoder to discover about `{"jsonrpc":"2.0","result":` that is not already
// known at compile time.

// Envelope fragments. Declared as constants so append copies them from
// read-only data with no allocation.
const (
	envPrefix    = `{"jsonrpc":"2.0",`
	envResult    = `"result":`
	envError     = `"error":{"code":`
	envMessage   = `,"message":`
	envData      = `,"data":`
	envID        = `,"id":`
	envNullRes   = `null`
	envErrorEnd  = `}`
	envObjectEnd = `}`
)

// wireBufMaxKeep caps the capacity a buffer may have and still be worth
// pooling, so one oversized batch does not pin memory in the pool for the life
// of the process. It matches the root package's threshold.
const wireBufMaxKeep = 64 << 10

// wireBufPool holds scratch buffers for serializing replies on the event loop.
//
// Reuse is sound only on the synchronous path: gnet's Conn.Write has finished
// with the slice by the time it returns, either completing the syscall or
// copying the remainder into the connection's outbound buffer. AsyncWrite keeps
// the slice until the poller drains it, so the blocking path serializes into a
// freshly allocated slice instead. This is the same rule, for the same reason,
// as wireBufPool in the root package — getting it backwards produces torn
// replies only under load, which is the worst possible time to find out.
var wireBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 2048)
		return &b
	},
}

func acquireWireBuf() *[]byte {
	return wireBufPool.Get().(*[]byte)
}

func releaseWireBuf(bp *[]byte) {
	if cap(*bp) > wireBufMaxKeep {
		return
	}
	*bp = (*bp)[:0]
	wireBufPool.Put(bp)
}

// appendResponse appends one response object for ctx onto buf.
//
// Member order is result-or-error first, then id. The specification does not
// constrain member order — JSON objects are unordered — so the order is chosen
// to put the fixed-size prefix first and let the variable-length payload
// follow.
func appendResponse(buf []byte, ctx *Context) []byte {
	buf = append(buf, envPrefix...)

	switch {
	case ctx.err != nil:
		buf = appendError(buf, ctx.err)
	case ctx.result != nil:
		// Pre-encoded result from ResultRaw, copied verbatim.
		buf = append(buf, envResult...)
		buf = append(buf, ctx.result...)
	case ctx.hasResult:
		buf = append(buf, envResult...)
		encoded, err := json.Marshal(ctx.resultValue)
		if err != nil {
			// The handler produced a value the encoder cannot represent — an
			// unsupported type, or a cycle. That is a server-side fault, so
			// §5.1 makes it -32603 Internal error. The partially written
			// result member has to be rolled back first, which is why the
			// buffer length is captured rather than assumed.
			return appendInternalErrorResponse(
				buf[:len(buf)-len(envPrefix)-len(envResult)],
				ctx.ID,
				err,
			)
		}
		buf = append(buf, encoded...)
	default:
		// A handler that set nothing. The result member is mandatory on a
		// success response, so null is emitted rather than omitting it, which
		// would produce an object that is neither a valid success nor a valid
		// error response.
		buf = append(buf, envResult...)
		buf = append(buf, envNullRes...)
	}

	buf = append(buf, envID...)
	if len(ctx.ID) == 0 {
		buf = append(buf, nullID...)
	} else {
		buf = append(buf, ctx.ID...)
	}
	return append(buf, envObjectEnd...)
}

// appendInternalErrorResponse writes a complete -32603 response onto a buffer
// that has been rewound to the start of the response.
//
// It exists so a marshal failure mid-response cannot emit a half-written result
// member followed by an error member.
func appendInternalErrorResponse(buf []byte, id json.RawMessage, cause error) []byte {
	buf = append(buf, envPrefix...)
	buf = appendError(buf, NewErrorData(CodeInternalError, msgInternalError, cause.Error()))
	buf = append(buf, envID...)
	if len(id) == 0 {
		buf = append(buf, nullID...)
	} else {
		buf = append(buf, id...)
	}
	return append(buf, envObjectEnd...)
}

// appendError appends the error member, including its enclosing braces.
func appendError(buf []byte, e *Error) []byte {
	buf = append(buf, envError...)
	buf = strconv.AppendInt(buf, int64(e.Code), 10)
	buf = append(buf, envMessage...)
	buf = appendJSONString(buf, e.Message)
	if e.Data != nil {
		// A data member that cannot be encoded is dropped rather than
		// escalated: the code and message are the parts the client acts on,
		// and replacing a real application error with an internal one because
		// its diagnostic attachment failed to marshal would hide the actual
		// problem.
		if encoded, err := json.Marshal(e.Data); err == nil {
			buf = append(buf, envData...)
			buf = append(buf, encoded...)
		}
	}
	return append(buf, envErrorEnd...)
}

// appendJSONString appends s as a quoted, escaped JSON string.
//
// Error messages are almost always ASCII literals from this package or from
// application code, so the common case is a bounds check per byte and a bulk
// copy. Delegating to the encoder instead would allocate a []byte for a string
// that needs no escaping at all.
func appendJSONString(buf []byte, s string) []byte {
	buf = append(buf, '"')
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' {
			continue
		}
		buf = append(buf, s[start:i]...)
		switch c {
		case '"':
			buf = append(buf, '\\', '"')
		case '\\':
			buf = append(buf, '\\', '\\')
		case '\n':
			buf = append(buf, '\\', 'n')
		case '\r':
			buf = append(buf, '\\', 'r')
		case '\t':
			buf = append(buf, '\\', 't')
		default:
			// Other control characters have no short escape and must be
			// emitted as \u00XX; JSON forbids them raw inside a string.
			buf = append(buf, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xF])
		}
		start = i + 1
	}
	buf = append(buf, s[start:]...)
	return append(buf, '"')
}

const hexDigits = "0123456789abcdef"

// appendErrorResponse appends a standalone error response with the given id.
//
// This is the path for failures detected before any handler runs — a parse
// error, an invalid request, an unknown method — where there is no Context to
// carry the reply.
func appendErrorResponse(buf []byte, e *Error, id json.RawMessage) []byte {
	buf = append(buf, envPrefix...)
	buf = appendError(buf, e)
	buf = append(buf, envID...)
	if len(id) == 0 {
		buf = append(buf, nullID...)
	} else {
		buf = append(buf, id...)
	}
	return append(buf, envObjectEnd...)
}

// grow ensures buf can take n more bytes without reallocating, reallocating
// once if it cannot. It mirrors the root package's helper of the same name.
func grow(buf []byte, n int) []byte {
	if cap(buf)-len(buf) >= n {
		return buf
	}
	next := make([]byte, len(buf), len(buf)+n)
	copy(next, buf)
	return next
}

// Note on pre-sizing the response buffer for the non-pooled entry points
// (Handle and the blocking handoff, which both start from a nil slice and let
// append grow it):
//
// Pre-allocating reqLen+64 up front was tried and measured. It removes one
// allocation on a single request (15 → 14 allocs/op) at a cost of 56 more bytes,
// but for a batch it is a net loss — N=1000 went from 641987 B/16011 allocs to
// 708562 B/16012 allocs, because appendBatch already performs its own single
// grow, so the pre-sized buffer is allocated and then immediately superseded.
// ns/op could not adjudicate it: on the development machine this benchmark
// varies 848–2007 ns/op run to run with byte-identical allocation counters, so
// the timing signal is far below the noise floor.
//
// The change was therefore reverted rather than kept on the strength of the
// single-request column alone. If this is revisited, the batch and single paths
// need to be sized separately, and it needs a quieter machine to measure on.
