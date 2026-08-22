package breeze

import (
	"strconv"
	"sync"
)

// statusTexts maps common status codes without allocating a map per call.
var statusTexts = [600]string{}

// statusLines holds the fully rendered "HTTP/1.1 <code> <text>\r\n" prefix for
// every code present in statusTexts.
//
// Emitting a response used to cost three appends plus a strconv.AppendInt for
// the status code. Because the set of codes is fixed at init time, the whole
// line can be rendered once and copied with a single append on the hot path.
var statusLines = [600][]byte{}

func init() {
	statusTexts[200] = "OK"
	statusTexts[201] = "Created"
	statusTexts[204] = "No Content"
	statusTexts[301] = "Moved Permanently"
	statusTexts[302] = "Found"
	statusTexts[304] = "Not Modified"
	statusTexts[400] = "Bad Request"
	statusTexts[401] = "Unauthorized"
	statusTexts[403] = "Forbidden"
	statusTexts[404] = "Not Found"
	statusTexts[405] = "Method Not Allowed"
	statusTexts[408] = "Request Timeout"
	statusTexts[409] = "Conflict"
	statusTexts[422] = "Unprocessable Entity"
	statusTexts[429] = "Too Many Requests"
	statusTexts[500] = "Internal Server Error"
	statusTexts[502] = "Bad Gateway"
	statusTexts[503] = "Service Unavailable"

	for code, text := range statusTexts {
		if text != "" {
			statusLines[code] = []byte("HTTP/1.1 " + strconv.Itoa(code) + " " + text + "\r\n")
		}
	}
}

// clHeader is the Content-Length key, kept as a constant so the length used to
// size the response buffer cannot drift from the bytes actually written.
const clHeader = "Content-Length: "

// statusLine returns the precomputed status line for code, rendering one on
// demand for codes outside the table.
//
// A zero Status means "the handler set headers or a body but never called
// Status", which is the 200 case. The previous version emitted the code
// verbatim there, producing a malformed "HTTP/1.1 0 OK" status line.
func statusLine(code int) []byte {
	if code == 0 {
		code = 200
	}
	if code > 0 && code < len(statusLines) {
		if line := statusLines[code]; line != nil {
			return line
		}
	}
	return []byte("HTTP/1.1 " + strconv.Itoa(code) + " Status\r\n")
}

// wireBufMaxKeep caps the capacity a buffer may have and still be worth
// pooling. A response that served a large file would otherwise pin megabytes
// in the pool for the lifetime of the process.
const wireBufMaxKeep = 64 << 10

// wireBufPool holds scratch buffers for serializing a response inline on the
// event loop.
//
// Reuse is only sound because gnet's synchronous Conn.Write has fully consumed
// the slice by the time it returns: on Unix it either completes the
// unix.Write or copies the remainder into the connection's outbound buffer
// (elastic.Buffer.Write copies into its ring, and its list-buffer PushBack
// copies too); on Windows it delegates to a blocking net.Conn.Write. Neither
// keeps a reference.
//
// AsyncWrite is a different story — it hands the slice to the poller and
// returns before anything is written — so the pooled path must not use these
// buffers. See breeze.go.
var wireBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 2048)
		return &b
	},
}

// acquireWireBuf returns an empty buffer for response serialization.
func acquireWireBuf() *[]byte {
	return wireBufPool.Get().(*[]byte)
}

// releaseWireBuf returns bp to the pool unless it grew past wireBufMaxKeep.
func releaseWireBuf(bp *[]byte) {
	if cap(*bp) > wireBufMaxKeep {
		return
	}
	*bp = (*bp)[:0]
	wireBufPool.Put(bp)
}

// AppendTo serializes the response onto buf and returns the extended slice.
//
// Performance decisions:
//   - The status line is a single append of a precomputed slice.
//   - When the response carries one of the standard content types, its headers
//     are also a precomputed slice (see rawHeaders), so the common path never
//     iterates a map. Ranging over even a one-entry map costs a mapiterinit
//     plus a bucket walk, which measurably outweighs the bytes being copied.
//   - The buffer is grown once, to the exact size for the precomputed path.
//   - strconv.AppendInt writes the content length straight into the buffer.
func (r *HTTPResponse) AppendTo(buf []byte) []byte {
	line := statusLine(r.Status)

	// Fast path: precomputed headers, one grow to the exact final size.
	if r.rawHeaders != nil {
		need := len(line) + len(r.rawHeaders) + len(clHeader) + 24 + len(r.Body)
		buf = grow(buf, need)
		buf = append(buf, line...)
		buf = append(buf, r.rawHeaders...)
		buf = append(buf, clHeader...)
		buf = strconv.AppendInt(buf, int64(len(r.Body)), 10)
		buf = append(buf, "\r\n\r\n"...)
		return append(buf, r.Body...)
	}

	need := len(line) + len(r.Headers)*48 + len(clHeader) + 24 + len(r.Body)
	buf = grow(buf, need)
	buf = append(buf, line...)

	for k, v := range r.Headers {
		buf = append(buf, k...)
		buf = append(buf, ": "...)
		buf = append(buf, v...)
		buf = append(buf, "\r\n"...)
	}

	buf = append(buf, clHeader...)
	buf = strconv.AppendInt(buf, int64(len(r.Body)), 10)
	buf = append(buf, "\r\n\r\n"...)
	return append(buf, r.Body...)
}

// grow ensures buf can take n more bytes without reallocating, reallocating
// once if it cannot.
func grow(buf []byte, n int) []byte {
	if cap(buf)-len(buf) >= n {
		return buf
	}
	next := make([]byte, len(buf), len(buf)+n)
	copy(next, buf)
	return next
}

// Bytes serializes the HTTPResponse to raw HTTP/1.1 bytes in a freshly
// allocated slice.
//
// Use this whenever the bytes are handed to AsyncWrite, which returns before
// the data is flushed and therefore needs a slice it can keep. The inline
// response path uses AppendTo with a pooled buffer instead — see wireBufPool.
func (r *HTTPResponse) Bytes() []byte {
	return r.AppendTo(nil)
}
