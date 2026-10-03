package breeze

import (
	"strconv"
	"strings"
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
	for code, text := range map[int]string{
		100: "Continue", 101: "Switching Protocols", 102: "Processing", 103: "Early Hints",
		200: "OK", 201: "Created", 202: "Accepted", 203: "Non-Authoritative Information",
		204: "No Content", 205: "Reset Content", 206: "Partial Content", 207: "Multi-Status",
		300: "Multiple Choices", 301: "Moved Permanently", 302: "Found", 303: "See Other",
		304: "Not Modified", 307: "Temporary Redirect", 308: "Permanent Redirect",
		400: "Bad Request", 401: "Unauthorized", 402: "Payment Required", 403: "Forbidden",
		404: "Not Found", 405: "Method Not Allowed", 406: "Not Acceptable",
		407: "Proxy Authentication Required", 408: "Request Timeout", 409: "Conflict", 410: "Gone",
		411: "Length Required", 412: "Precondition Failed", 413: "Content Too Large",
		414: "URI Too Long", 415: "Unsupported Media Type", 416: "Range Not Satisfiable",
		417: "Expectation Failed", 418: "I'm a teapot", 421: "Misdirected Request",
		422: "Unprocessable Entity", 423: "Locked", 424: "Failed Dependency", 425: "Too Early",
		426: "Upgrade Required", 428: "Precondition Required", 429: "Too Many Requests",
		431: "Request Header Fields Too Large", 451: "Unavailable For Legal Reasons",
		500: "Internal Server Error", 501: "Not Implemented", 502: "Bad Gateway",
		503: "Service Unavailable", 504: "Gateway Timeout", 505: "HTTP Version Not Supported",
		507: "Insufficient Storage", 511: "Network Authentication Required",
	} {
		statusTexts[code] = text
	}

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
	if code < 100 || code > 599 {
		code = 500
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
		body := r.Body
		bodyAllowed := !responseMustNotHaveBody(r.Status)
		if !bodyAllowed {
			body = nil
		}
		need := len(line) + len(r.rawHeaders) + len(clHeader) + 24 + len(body)
		buf = grow(buf, need)
		buf = append(buf, line...)
		buf = append(buf, r.rawHeaders...)
		if bodyAllowed {
			buf = append(buf, clHeader...)
			buf = strconv.AppendInt(buf, int64(len(body)), 10)
			buf = append(buf, "\r\n"...)
		}
		buf = append(buf, "\r\n"...)
		return append(buf, body...)
	}

	body := r.Body
	if responseMustNotHaveBody(r.Status) {
		body = nil
	}

	need := len(line) + len(r.Headers)*48 + len(clHeader) + 24 + len(body)
	buf = grow(buf, need)
	buf = append(buf, line...)

	for k, v := range r.Headers {
		// Compared case-insensitively in place. Lower-casing the key allocated a
		// new string for every header of every response that took this path.
		tk := strings.TrimSpace(k)
		// Framing is owned by the serializer. Never let application headers create
		// a second Content-Length/Transfer-Encoding, and never emit hop-by-hop
		// framing that could disagree with the connection writer.
		if strings.EqualFold(tk, "content-length") || strings.EqualFold(tk, "transfer-encoding") || strings.EqualFold(tk, "connection") {
			continue
		}
		// Set-Cookie is special: it may contain multiple cookies separated by \r\n.
		// Write each as a separate header line.
		if strings.EqualFold(tk, "set-cookie") {
			for _, cookie := range strings.Split(v, "\r\n") {
				cookie = strings.TrimSpace(cookie)
				if cookie == "" {
					continue
				}
				if !validResponseHeaderValue(cookie) {
					continue
				}
				buf = append(buf, "Set-Cookie: "...)
				buf = append(buf, cookie...)
				buf = append(buf, "\r\n"...)
			}
			continue
		}
		if !validResponseHeaderName(k) || !validResponseHeaderValue(v) {
			continue
		}
		buf = append(buf, k...)
		buf = append(buf, ": "...)
		buf = append(buf, v...)
		buf = append(buf, "\r\n"...)
	}

	if !responseMustNotHaveBody(r.Status) {
		buf = append(buf, clHeader...)
		buf = strconv.AppendInt(buf, int64(len(body)), 10)
		buf = append(buf, "\r\n"...)
	}
	buf = append(buf, "\r\n"...)
	return append(buf, body...)
}

func responseMustNotHaveBody(status int) bool {
	return (status >= 100 && status < 200) || status == 204 || status == 304
}

func validResponseHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '!' || c == '#' || c == '$' || c == '%' || c == '&' || c == '\'' || c == '*' ||
			c == '+' || c == '-' || c == '.' || c == '^' || c == '_' || c == '`' || c == '|' || c == '~':
		default:
			return false
		}
	}
	return true
}

func validResponseHeaderValue(value string) bool {
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c == '\t' || (c >= 0x20 && c != 0x7f) {
			continue
		}
		return false
	}
	return true
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
