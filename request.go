package breeze

import (
	"bytes"
	"fmt"
	"net/url"
	"strconv"
	"unsafe"
)

// crlfcrlf is the header terminator we scan for once per request.
var crlfcrlf = []byte("\r\n\r\n")

// ParseHTTPRequest parses raw bytes into an HTTPRequest.
//
// The returned request is freshly allocated and owns a private copy of the
// header block, so every string on it stays valid for as long as the caller
// keeps the request — no matter what happens to data afterwards. The server's
// own hot path uses parsePooledRequest instead.
//
// A nil request with a nil error means the bytes are an incomplete request and
// the caller should wait for more data.
func ParseHTTPRequest(data []byte) (*HTTPRequest, int, error) {
	req := &HTTPRequest{Header: make(map[string]string, 8)}
	consumed, err := fillHTTPRequest(req, data, true)
	if err != nil {
		return nil, 0, err
	}
	if consumed == 0 {
		return nil, 0, nil
	}
	return req, consumed, nil
}

// parsePooledRequest is ParseHTTPRequest backed by requestPool. The request is
// returned to the pool when parsing does not yield one, so a stream of partial
// reads cannot drain it.
//
// ownHeaders selects whether the header block is copied into req.owned or
// parsed in place. See fillHTTPRequest.
func parsePooledRequest(data []byte, ownHeaders bool) (*HTTPRequest, int, error) {
	req := acquireRequest()
	consumed, err := fillHTTPRequest(req, data, ownHeaders)
	if err != nil || consumed == 0 {
		releaseRequest(req)
		return nil, 0, err
	}
	return req, consumed, nil
}

// promoteRequest re-parses data into req with owned headers, so req can safely
// outlive the buffer data points into — i.e. leave the event-loop goroutine.
//
// This is the counterpart to parsing with ownHeaders == false: parse cheaply,
// and pay for isolation only for the requests that actually need it. Only
// blocking routes are promoted, and a blocking route is about to do disk or
// network I/O, so a second pass over a few hundred bytes still in L1 is noise
// against what follows it.
//
// The re-parse cannot disagree with the first one. It reads the same bytes with
// the same code, and the only mutation involved — lowercasing header keys — is
// idempotent. So the caller keeps its own consumed count, and an error here
// would mean the first parse was wrong too.
//
// # Why Header has to be cleared rather than overwritten
//
// req.Header's keys are themselves views into data. Assigning to a Go map does
// not replace a key that compares equal to the one already stored, so simply
// re-inserting every header would leave the original key strings — and their
// pointers into the caller's buffer — in the map, which is precisely what this
// function exists to get rid of. clear() drops them; the buckets survive, so
// the re-parse still allocates nothing but the owned copy.
func promoteRequest(req *HTTPRequest, data []byte) error {
	clear(req.Header)
	req.Query = nil
	req.Body = nil
	_, err := fillHTTPRequest(req, data, true)
	return err
}

// fillHTTPRequest parses data into req, returning how many bytes the request
// occupies. A zero length with a nil error means "incomplete, need more bytes".
//
// # ownHeaders: who owns the bytes the strings point into
//
// String fields (Method, Path, header keys and values) are unsafe views rather
// than copies. ownHeaders decides what they view.
//
// ownHeaders == true copies data[:headerEnd] into req.owned and points every
// string at that private copy. The GC keeps it alive for exactly as long as
// *HTTPRequest is reachable, so the request can be handed to another goroutine
// and held indefinitely. This is what the exported ParseHTTPRequest does, and
// what the server does for any request it dispatches to the worker pool.
//
// ownHeaders == false parses data in place and copies nothing. The strings are
// views into the caller's buffer and are valid only for as long as those bytes
// are: on the server's inline path that is the duration of the OnTraffic call,
// which is also the entire lifetime of the request. This removes the last
// per-request allocation on the fast path.
//
// The caller is responsible for that distinction. OnTraffic re-parses with
// ownHeaders == true before letting a request cross a goroutine boundary.
//
// # Header keys are lowercased in place
//
// Note that lowercasing mutates the bytes being parsed, including when they are
// the caller's. That is safe here: the affected bytes are header keys inside a
// request that has already been consumed from the connection, gnet never reads
// them again, and lowercasing is idempotent, so even a re-parse of the same
// bytes produces an identical result.
//
// req.Body is always a zero-copy slice of data. Ownership of those bytes is the
// caller's problem: OnTraffic copies the body when it needs to outlive the
// buffer (see breeze.go).
//
// # Single-pass header parsing
//
// The previous version did two full scans of the header block: one pre-scan
// (indexHeaderValue) to find Content-Length so it could size the copy, and a
// second scan to build req.Header. This version does one scan that tracks
// Content-Length inline while building the map, then sets req.Body after.
//
// # Other performance decisions
//   - Manual byte scanner replaces bytes.Split → no [][]byte alloc.
//   - splitPathQuery uses bytes.IndexByte → no url.Parse overhead.
//   - lowerHeaderKey lowercases header keys in place, so building req.Header
//     allocates nothing at all.
//   - url.ParseQuery copies internally → b2s(query) is transient and safe.
//   - internMethod returns a package-level constant for the seven known methods.
func fillHTTPRequest(req *HTTPRequest, data []byte, ownHeaders bool) (int, error) {
	// ── Find header boundary ───────────────────────────────────────────────
	headerEnd := bytes.Index(data, crlfcrlf)
	if headerEnd < 0 {
		return 0, nil // incomplete — wait for more data
	}

	// ── Establish the bytes all strings will view ──────────────────────────
	header := data[:headerEnd]
	if ownHeaders {
		owned := make([]byte, headerEnd)
		copy(owned, header)
		header = owned
		req.owned = owned
	}

	// ── Parse request line ─────────────────────────────────────────────────
	lineEnd := bytes.IndexByte(header, '\r')
	if lineEnd < 0 {
		lineEnd = len(header)
	}
	requestLine := header[:lineEnd]

	s1 := bytes.IndexByte(requestLine, ' ')
	if s1 < 0 {
		return 0, fmt.Errorf("malformed request line")
	}
	s2 := bytes.IndexByte(requestLine[s1+1:], ' ')
	if s2 < 0 {
		s2 = len(requestLine) - s1 - 1
	}

	methodBytes := requestLine[:s1]
	rawPath := requestLine[s1+1 : s1+1+s2]
	path, query := splitPathQuery(rawPath)

	if req.Header == nil {
		req.Header = make(map[string]string, 8)
	}
	req.Method = internMethod(methodBytes)
	req.Path = b2s(path)

	if len(query) > 0 {
		// url.ParseQuery copies all keys/values — b2s(query) is transient.
		q, err := url.ParseQuery(b2s(query))
		if err == nil {
			req.Query = q
		}
	}

	// ── Single-pass header scan ────────────────────────────────────────────
	// Builds req.Header and extracts Content-Length in one traversal.
	contentLength := -1
	pos := lineEnd + 2 // skip past \r\n of the request line

	for pos < len(header) {
		end := bytes.IndexByte(header[pos:], '\r')
		if end < 0 {
			end = len(header) - pos
		}
		line := header[pos : pos+end]
		pos += end + 2

		if len(line) == 0 {
			continue
		}
		colon := bytes.IndexByte(line, ':')
		if colon <= 0 {
			continue
		}

		key := lowerHeaderKey(line[:colon])
		val := b2s(bytes.TrimSpace(line[colon+1:]))
		req.Header[key] = val

		// Capture Content-Length without a second scan.
		if contentLength == -1 && key == "content-length" {
			cl, err := strconv.Atoi(val)
			if err != nil || cl < 0 {
				return 0, fmt.Errorf("invalid content-length")
			}
			contentLength = cl
		}
	}

	// ── Body (zero-copy) ───────────────────────────────────────────────────
	consumed := headerEnd + 4
	if contentLength > 0 {
		total := consumed + contentLength
		if len(data) < total {
			return 0, nil // body not fully received yet
		}
		req.Body = data[consumed:total]
		consumed = total
	}

	return consumed, nil
}

// splitPathQuery splits rawPath at the first '?' without allocating.
func splitPathQuery(raw []byte) (path, query []byte) {
	i := bytes.IndexByte(raw, '?')
	if i < 0 {
		return raw, nil
	}
	return raw[:i], raw[i+1:]
}

// lowerHeaderKey lowercases b in place and returns a zero-copy string view of
// it. It never allocates.
//
// # Why mutating the caller's bytes is safe
//
// b is a subslice of the header block, which is either req.owned — the parser's
// private copy — or, with zero-copy headers, gnet's read buffer directly.
//
// Nothing reads those bytes in their original case either way. The request line
// is parsed before the header scan starts, the value side of a line is taken
// before the scan moves to the next line, and keys and values never overlap.
//
// Writing into gnet's buffer is likewise safe: OnTraffic gets those bytes from
// Conn.Next(-1), which consumes them as it hands them over, so gnet will never
// read them again — it only overwrites them on a later read.
//
// Two paths do scan the same bytes twice, and lowercasing is idempotent, so
// both are unaffected: promoteRequest re-parses a request into owned memory,
// and a request split across events is re-parsed after its leftover bytes are
// concatenated with the next read.
//
// # Why this is not the obvious "only allocate when recasing is needed"
//
// The version this replaces scanned for an uppercase byte and returned a
// zero-copy b2s view when it found none, allocating only otherwise:
//
//	buf := make([]byte, len(b))   // allocation 1
//	...
//	return string(buf)            // allocation 2
//
// The fast path never fired. Every header a real client puts on the wire is
// capitalised — Host, User-Agent, Accept, Accept-Encoding, Connection,
// Content-Type, Authorization — so the allocating branch was taken for
// essentially every header of every request, and it allocated twice: once for
// the scratch buffer and again for the string conversion. A plain four-header
// GET therefore paid eight heap allocations before it reached the router, none
// of which survived the request.
//
// Lowercasing in place costs one pass over bytes already in L1 and nothing
// else, and the result points into the same block every other string on the
// request points into, so it stays valid for exactly as long as they do.
func lowerHeaderKey(b []byte) string {
	for i := 0; i < len(b); i++ {
		if c := b[i]; c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return b2s(b)
}

// internMethod returns a package-level Method constant without allocation.
// Falls back to Method(string(b)) for unknown methods (not in hot path).
//
// FIX: Removed the 6-byte "OPTION" branch (not a real HTTP method) and added
// a 7-byte "OPTIONS" branch matching RFC 9110. The old code never matched a
// real OPTIONS preflight request, causing CORS preflight to 404.
func internMethod(b []byte) Method {
	switch len(b) {
	case 3:
		if b[0] == 'G' && b[1] == 'E' && b[2] == 'T' {
			return GET
		}
		if b[0] == 'P' && b[1] == 'U' && b[2] == 'T' {
			return PUT
		}
	case 4:
		if b[0] == 'P' && b[1] == 'O' && b[2] == 'S' && b[3] == 'T' {
			return POST
		}
	case 5:
		if b[0] == 'P' && b[1] == 'A' && b[2] == 'T' && b[3] == 'C' && b[4] == 'H' {
			return PATCH
		}
	case 6:
		if b[0] == 'D' && b[1] == 'E' && b[2] == 'L' && b[3] == 'E' && b[4] == 'T' && b[5] == 'E' {
			return DELETE
		}
	case 7:
		// FIX: OPTIONS is 7 bytes, not 6. The old code checked for the
		// non-existent 6-byte "OPTION" method and never matched real
		// CORS preflight requests.
		if b[0] == 'O' && b[1] == 'P' && b[2] == 'T' && b[3] == 'I' && b[4] == 'O' && b[5] == 'N' &&
			b[6] == 'S' {
			return OPTIONS
		}
	}
	return Method(string(b))
}

// b2s converts a byte slice to a string without allocation.
//
// SAFETY CONTRACT: the returned string must not outlive b. Within this package
// b is always a subslice of req.owned, which the GC keeps alive as long as
// *HTTPRequest is reachable. Do not use b2s on slices that are not anchored
// to a live GC-traced object.
func b2s(b []byte) string {
	return unsafe.String(unsafe.SliceData(b), len(b))
}
