package breeze

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"unsafe"
)

// ErrBodyTooLarge reports a request whose body exceeds the server's configured
// cap (SetMaxRequestBody).
//
// It is a sentinel rather than a plain error so OnTraffic can distinguish it
// from a malformed request and answer 413 instead of 400: the two have
// different meanings to a caller — one says "send less", the other says "send
// it correctly" — and both reach the same parse call site.
//
// Not returned by the exported ParseHTTPRequest, which has no server to read a
// cap from; see that function's doc.
var ErrBodyTooLarge = errors.New("request body exceeds the configured maximum")

// crlfcrlf is the header terminator we scan for once per request.
var crlfcrlf = []byte("\r\n\r\n")

// maxRequestHeaderBytes bounds the request-line + header block before the body
// is considered. Without a hard cap an attacker can keep a connection open and
// send bytes that never terminate with CRLFCRLF, forcing the event loop to retain
// an ever-growing buffer. 64 KiB is enough for normal HTTP traffic while keeping
// the memory cost bounded per connection.
const maxRequestHeaderBytes = 64 << 10

var ErrHeadersTooLarge = errors.New("request headers exceed the configured maximum")

// ParseHTTPRequest parses raw bytes into an HTTPRequest.
//
// The returned request is freshly allocated and owns a private copy of the
// header block, so every string on it stays valid for as long as the caller
// keeps the request — no matter what happens to data afterwards. The server's
// own hot path uses parsePooledRequest instead.
//
// A nil request with a nil error means the bytes are an incomplete request and
// the caller should wait for more data.
//
// Parsing is uncapped: the exported form has no server to read a limit from,
// so a Content-Length of any size is accepted here. The server enforces
// SetMaxRequestBody before this function's callers do anything with the body
// (see parsePooledRequest and fillHTTPRequest).
func ParseHTTPRequest(data []byte) (*HTTPRequest, int, error) {
	req := &HTTPRequest{Header: make(map[string]string, 8)}
	consumed, err := fillHTTPRequest(req, data, true, 0)
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
//
// maxBody is the server's request-body cap, from SetMaxRequestBody. Zero means
// unlimited. A Content-Length above it fails the parse before the body is
// awaited or sliced, so the caller can answer 413 without ever holding the
// bytes — which is the whole point of capping in the parser rather than in a
// handler or middleware, both of which run after the body has arrived.
func parsePooledRequest(data []byte, ownHeaders bool, maxBodies ...int64) (*HTTPRequest, int, error) {
	maxBody := int64(0)
	if len(maxBodies) > 0 {
		maxBody = maxBodies[0]
	}
	req := acquireRequest()
	consumed, err := fillHTTPRequest(req, data, ownHeaders, maxBody)
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
// against what follows.
//
// The re-parse cannot disagree with the first one. It reads the same bytes with
// the same code, and the only mutation involved — lowercasing header keys — is
// idempotent. So the caller keeps its own consumed count, and an error here
// would mean the first parse was wrong too.
//
// maxBody is the same cap the first parse ran under. The check is idempotent —
// the first parse already rejected an oversized Content-Length — but running
// it again is free and keeps the two passes from drifting apart if someone
// ever adds a second accumulation call site.
//
// # Why Header has to be cleared rather than overwritten
//
// req.Header's keys are themselves views into data. Assigning to a Go map does
// not replace a key that compares equal to the one already stored, so simply
// re-inserting every header would leave the original key strings — and their
// pointers into the caller's buffer — in the map, which is precisely what this
// function exists to get rid of. clear() drops them; the buckets survive, so
// the re-parse still allocates nothing but the owned copy.
func promoteRequest(req *HTTPRequest, data []byte, maxBodies ...int64) error {
	maxBody := int64(0)
	if len(maxBodies) > 0 {
		maxBody = maxBodies[0]
	}
	clear(req.Header)
	req.Query = nil
	req.Body = nil
	_, err := fillHTTPRequest(req, data, true, maxBody)
	return err
}

// fillHTTPRequest parses data into req, returning how many bytes the request
// occupies. A zero length with a nil error means "incomplete, need more bytes".
//
// maxBody is the request-body cap from SetMaxRequestBody: 0 means unlimited,
// and a request declaring or carrying more than it fails the parse with
// ErrBodyTooLarge before any body byte is awaited, sliced or accumulated. The
// check lives here — where Content-Length is first known — rather than in a
// middleware, because middleware runs after the body has already arrived and
// the memory has already been spent.
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
// The header block is walked once. Each line is delimited, validated and
// lowercased in the same pass over its bytes: the name is the run of token
// bytes up to ':' (keyLower validates and lowercases a byte in one lookup), and
// the value is the run of acceptable value bytes up to the CRLF (scanValue,
// sixteen bytes per step). Content-Length, Transfer-Encoding, Connection and
// Host are noted on the way, and req.Body is set after.
//
// A line the fast path does not take outright — anything odd — goes to the
// original per-line code (parseHeaderLineSlow, parseRequestLineSlow), so
// malformed and unusual input gets the same verdict and the same error it
// always did. request_parser_diff_test.go is the proof: it feeds identical
// bytes to this parser and to a frozen copy of the one it replaced and
// requires identical results, buffer contents included.
//
// # Finding the end of the header block
//
// With ownHeaders == true the end of the header block is found first (the copy
// has to be sized), with a search for CRLFCRLF. With ownHeaders == false that
// search is skipped when the data ends with CRLFCRLF — a request sent in one
// piece, or the last of a pipelined batch — because the line loop reaches the
// blank line anyway (fillInPlace). Anything that is not a clean parse to that
// blank line is redone on the exact path, which keeps the original order of
// decisions: first is the head complete, then what is wrong with it.
//
// # Other performance decisions
//   - Lines are walked, not split: no [][]byte.
//   - Header names are lowercased in place, so building req.Header allocates
//     nothing at all.
//   - A plain query string (no '%', '+', ';', few parameters) is parsed by
//     parseRequestQuery with one backing array for the values; everything else
//     goes to net/url. In both cases keys and values are substrings of the
//     query, not copies, so they live as long as the header block does (net/url
//     only copies a key or value that has an escape in it).
//   - internMethod returns a package-level constant for the known methods.
//   - Malformed requests return package-level errors: no allocation.
func fillHTTPRequest(req *HTTPRequest, data []byte, ownHeaders bool, maxBody int64) (int, error) {
	// Never scan beyond the configured header budget. The input may also
	// contain a large body or a large unterminated prefix, neither of which
	// should make header-boundary detection proportional to the whole buffer.
	scanLen := len(data)
	if scanLen > maxRequestHeaderBytes {
		scanLen = maxRequestHeaderBytes
	}

	// ── Zero-copy: let the parse find the end of the header block ──────────
	// A parse in place does not need to know where the header block ends
	// before it starts: the line loop walks every CRLF anyway, and the first
	// empty line is the end. Searching for CRLFCRLF first is a second pass
	// over the same bytes (a fifth or more of the whole parse), so it is
	// skipped here.
	//
	// It is only tried when the data ends with CRLFCRLF, which is a request
	// that was sent in one piece (and the last request of a pipelined batch):
	// then a blank line is certainly in there, so the parse cannot be wasted
	// on a head that is still arriving — that would cost several times what
	// the search for the terminator does, once per read of a fragmented head.
	// A request with a body, or a head cut short, takes the exact path below.
	//
	// Only a request that parses cleanly all the way to its blank line is
	// answered here; every other outcome — a malformed line, a line the fast
	// path does not accept — is redone from scratch below, where the original
	// order of decisions (is the head complete? then what is wrong with it?) is
	// kept exactly.
	if !ownHeaders && scanLen >= 4 &&
		data[scanLen-1] == '\n' && data[scanLen-2] == '\r' && data[scanLen-3] == '\n' && data[scanLen-4] == '\r' {
		if n, err, handled := fillInPlace(req, data, scanLen, maxBody); handled {
			return n, err
		}
		resetForReparse(req)
	}

	// ── Find header boundary ───────────────────────────────────────────────
	headerEnd := bytes.Index(data[:scanLen], crlfcrlf)
	if headerEnd < 0 {
		if len(data) > maxRequestHeaderBytes {
			return 0, ErrHeadersTooLarge
		}
		return 0, nil // incomplete — wait for more data
	}
	if headerEnd+4 > maxRequestHeaderBytes {
		return 0, ErrHeadersTooLarge
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
	lineEnd := indexCRLF(header)
	if lineEnd < 0 {
		return 0, errMalformedRequestLine
	}
	methodBytes, path, query, http10, err := parseRequestLine(header[:lineEnd])
	if err != nil {
		return 0, err
	}

	if req.Header == nil {
		req.Header = make(map[string]string, 8)
	}
	req.Method = internMethod(methodBytes)
	req.Path = b2s(path)

	if len(query) > 0 {
		// Keys and values are substrings of the query where nothing needed
		// unescaping (see parseRequestQuery), so they live as long as the
		// header block does, like Path.
		q, err := parseRequestQuery(b2s(query))
		if err != nil {
			return 0, fmt.Errorf("invalid query string: %w", err)
		}
		req.Query = q
	}

	// ── Header lines ───────────────────────────────────────────────────────
	st, _, err := scanHeaders(req, header, lineEnd+2, false)
	if err != nil {
		return 0, err
	}
	return finishRequest(req, data, header, lineEnd, headerEnd, &st, http10, maxBody)
}

// fillInPlace is the zero-copy parse that finds the end of the header block as
// it goes. handled == false means "no verdict from here": the caller must run
// the exact path, which also decides every error. handled == true carries the
// final answer — a request, an incomplete one, or an error that can only
// arise once the blank line has been seen.
func fillInPlace(req *HTTPRequest, data []byte, scanLen int, maxBody int64) (consumed int, err error, handled bool) {
	buf := data[:scanLen]

	lineEnd := indexCRLF(buf)
	if lineEnd < 0 {
		// No CRLF at all, so no CRLFCRLF either.
		return headNotComplete(req, len(data))
	}
	methodBytes, path, query, http10, perr := parseRequestLine(buf[:lineEnd])
	if perr != nil {
		return 0, nil, false
	}
	if req.Header == nil {
		req.Header = make(map[string]string, 8)
	}
	req.Method = internMethod(methodBytes)
	req.Path = b2s(path)
	if len(query) > 0 {
		q, qerr := parseRequestQuery(b2s(query))
		if qerr != nil {
			return 0, nil, false
		}
		req.Query = q
	}

	st, status, serr := scanHeaders(req, buf, lineEnd+2, true)
	switch {
	case status == scanNeedMore:
		return headNotComplete(req, len(data))
	case status == scanFallback, serr != nil:
		return 0, nil, false
	}

	// The loop stopped at the blank line: st.pos is where it starts, and the
	// header block ends with the CRLF just before it.
	headerEnd := st.pos - 2
	if headerEnd == lineEnd {
		return 0, nil, false // a request line and nothing else: the exact path words this error
	}
	n, ferr := finishRequest(req, data, buf[:headerEnd], lineEnd, headerEnd, &st, http10, maxBody)
	return n, ferr, true
}

// headNotComplete is the answer for a header block that has no blank line in
// the first scanLen bytes: more bytes are needed, unless there are already
// more than a header may be. The lines parsed so far are dropped, so the
// request is left as it was found, which is what the exact path does for an
// incomplete head. (The keys of those lines were lowercased in place on the
// way, which is harmless: lowercasing is idempotent, and the same bytes are
// parsed again once the rest has arrived.)
func headNotComplete(req *HTTPRequest, dataLen int) (int, error, bool) {
	resetForReparse(req)
	if dataLen > maxRequestHeaderBytes {
		return 0, ErrHeadersTooLarge, true
	}
	return 0, nil, true
}

// resetForReparse returns req to the state it had before a fillInPlace attempt
// that gave no verdict, so the exact path can start over on it.
func resetForReparse(req *HTTPRequest) {
	clear(req.Header)
	req.Method = ""
	req.Path = ""
	req.Query = nil
	req.Body = nil
	req.owned = nil
}

// headScan is what the header loop learned about a request.
type headScan struct {
	contentLength int
	connValue     string
	lines         int
	pos           int // offset just past the last header line (see scanHeaders)
	hostSeen      bool
	teSeen        bool
}

const (
	scanDone     = iota // the header lines were parsed (check err for a rejected one)
	scanNeedMore        // open: the data ended before the blank line
	scanFallback        // open: a line that is not plainly well-formed; start over exactly
)

// scanHeaders parses header lines into req.Header starting at pos.
//
// With open == false, buf is exactly the header block and the lines run to its
// end. Each line is delimited, validated and lowercased in one walk over its
// bytes (the fast path below); a line that is not plainly well-formed is handed
// to parseHeaderLineSlow, the original per-line code, so odd input keeps its
// exact error and its exact meaning.
//
// With open == true, buf is whatever has arrived (up to the header budget) and
// the header block ends at the first empty line, which this loop looks for
// itself. In that mode the loop only ever accepts what the fast path accepts:
// running out of data answers scanNeedMore, and a line the fast path does not
// take answers scanFallback without parsing it, because the exact path is the
// one that must decide what such a line means. st.pos is then the offset of
// the empty line.
func scanHeaders(req *HTTPRequest, buf []byte, pos int, open bool) (st headScan, status int, err error) {
	st.contentLength = -1
	n := len(buf)

	for {
		if open {
			if pos+1 >= n {
				return st, scanNeedMore, nil
			}
			if buf[pos] == '\r' && buf[pos+1] == '\n' {
				break
			}
		} else if pos >= n {
			break
		}

		var key, val string
		var next int

		// Fast path: "name:" OWS value OWS, terminated by CRLF (or, in a closed
		// block, by its end). The name is the run of token bytes up to the first
		// byte that is not one (lowercased in place as it is walked); the value
		// is the run of acceptable value bytes after the optional whitespace.
		// Anything else — a name that does not end in ':', a stopper inside
		// the value that is not a CRLF, a value that begins or ends with a
		// non-ASCII byte (bytes.TrimSpace would apply Unicode rules there) —
		// is not the fast path's business.
		ok := false
		i := pos
		for i < n {
			c := keyLower[buf[i]]
			if c == 0 {
				break
			}
			buf[i] = c
			i++
		}
		if i > pos && i < n && buf[i] == ':' {
			ke := i
			i++
			for i < n && (buf[i] == ' ' || buf[i] == '\t') {
				i++
			}
			vs := i
			i = scanValue(buf, i)
			terminated := false
			switch {
			case i < n && buf[i] == '\r' && i+1 < n && buf[i+1] == '\n':
				terminated = true
			case !open && i == n:
				terminated = true
			case open && (i == n || i == n-1 && buf[i] == '\r'):
				return st, scanNeedMore, nil // the line has not fully arrived
			}
			if terminated {
				ve := i
				for ve > vs && (buf[ve-1] == ' ' || buf[ve-1] == '\t') {
					ve--
				}
				if ve == vs || (buf[vs] < 0x80 && buf[ve-1] < 0x80) {
					key = b2s(buf[pos:ke])
					val = b2s(buf[vs:ve])
					next = i
					if i < n {
						next = i + 2
					}
					ok = true
				}
			}
		} else if open && i >= n {
			return st, scanNeedMore, nil // the name ran to the end of the data
		}
		if !ok {
			if open {
				return st, scanFallback, nil
			}
			key, val, next, err = parseHeaderLineSlow(buf, pos)
			if err != nil {
				return st, scanDone, err
			}
		}

		switch key {
		case "content-length":
			if st.contentLength != -1 {
				// Reject duplicates outright. Even identical duplicates are an unnecessary
				// framing ambiguity across intermediaries with different policies.
				return st, scanDone, errDuplicateContentLength
			}
			cl, ok := parseContentLength(val)
			if !ok {
				return st, scanDone, errInvalidContentLength
			}
			st.contentLength = cl
		case "transfer-encoding":
			st.teSeen = true
		case "connection":
			// Joined rather than overwritten: "Connection: keep-alive" and a
			// later "Connection: close" are both in effect, and close wins.
			if st.connValue == "" {
				st.connValue = val
			} else {
				st.connValue += "," + val
			}
		case "host":
			if st.hostSeen {
				return st, scanDone, errDuplicateHost
			}
			st.hostSeen = true
		}
		req.Header[key] = val
		st.lines++
		pos = next
	}
	st.pos = pos
	return st, scanDone, nil
}

// finishRequest does what follows the header loop: duplicate-header joining,
// the framing and Host checks, the connection mode, and the body. header is
// the header block (without its CRLFCRLF), headerEnd its length.
func finishRequest(req *HTTPRequest, data, header []byte, lineEnd, headerEnd int, st *headScan, http10 bool, maxBody int64) (int, error) {
	// Duplicates are detected for free: a repeated name overwrites its map entry,
	// so the map ends up shorter than the number of lines parsed. Only then is
	// the (rare) second pass paid for. Last-wins silently discarded the earlier
	// values, which is wrong for lists (X-Forwarded-For, Accept) and lets two
	// parties — a proxy and this server — disagree about which Authorization
	// header a request carried.
	if len(req.Header) != st.lines {
		joinDuplicateHeaders(req, header[lineEnd+2:])
	}

	if st.teSeen {
		return 0, errTransferEncoding
	}
	if !http10 && !st.hostSeen {
		return 0, errMissingHost
	}
	req.connMode = connectionMode(http10, st.connValue)

	// ── Body (zero-copy) ───────────────────────────────────────────────────
	// The cap is checked here, at the first moment the size of the body is
	// known, and before the "wait for the rest of the body" branch below — so
	// an oversized request is rejected without the connection ever holding its
	// bytes. A 0 maxBody skips the branch entirely, which is the default and
	// today's behaviour.
	if maxBody > 0 && int64(st.contentLength) > maxBody {
		return 0, ErrBodyTooLarge
	}

	consumed := headerEnd + 4
	if st.contentLength > 0 {
		// Check before adding: Content-Length can be int-sized while the
		// header offset makes the sum overflow int and panic on slicing.
		if st.contentLength > len(data)-consumed {
			return 0, nil // body not fully received yet
		}
		total := consumed + st.contentLength
		req.Body = data[consumed:total]
		consumed = total
	}

	return consumed, nil
}

// Errors the parser returns for malformed requests. They are package-level
// values rather than fmt.Errorf calls so that a flood of malformed requests
// does not allocate an error per request; the texts are unchanged.
var (
	errMalformedRequestLine   = errors.New("malformed request line")
	errInvalidMethod          = errors.New("invalid request method")
	errUnsupportedVersion     = errors.New("unsupported HTTP version")
	errInvalidTarget          = errors.New("invalid request target")
	errEmptyHeaderLine        = errors.New("unexpected empty header line")
	errObsFold                = errors.New("obs-fold header is not permitted")
	errMalformedHeaderLine    = errors.New("malformed header line")
	errInvalidHeaderValue     = errors.New("invalid header value")
	errDuplicateContentLength = errors.New("duplicate content-length")
	errInvalidContentLength   = errors.New("invalid content-length")
	errDuplicateHost          = errors.New("duplicate host")
	errTransferEncoding       = errors.New("transfer-encoding is not supported")
	errMissingHost            = errors.New("missing host header")
)

// parseRequestLine splits "METHOD SP target SP HTTP/1.x" (CRLF already
// removed) into the method, the path and the raw query of the target, and
// reports whether the version is HTTP/1.0.
//
// The fast path accepts exactly the plainly well-formed shape: a non-empty run
// of token bytes, one space, a non-empty run of target bytes, one space and a
// version that is exactly "HTTP/1.1" or "HTTP/1.0". For those the original
// checks cannot fail, so the result is the same by construction. Anything
// else goes to parseRequestLineSlow, which is the original code and decides
// which error a malformed line gets.
func parseRequestLine(rl []byte) (method, path, query []byte, http10 bool, err error) {
	n := len(rl)
	i := 0
	for i < n && tokenChar[rl[i]] {
		i++
	}
	if i > 0 && i < n && rl[i] == ' ' {
		ts := i + 1
		te := scanTarget(rl, ts)
		if te > ts && te < n && rl[te] == ' ' && n-te-1 == 8 {
			v := b2s(rl[te+1:])
			if v == "HTTP/1.1" || v == "HTTP/1.0" {
				target := rl[ts:te]
				path, query = splitPathQuery(target)
				return rl[:i], path, query, v == "HTTP/1.0", nil
			}
		}
	}
	return parseRequestLineSlow(rl)
}

// parseRequestLineSlow is the original request-line code, unchanged in what it
// accepts and in which error it reports first.
func parseRequestLineSlow(requestLine []byte) (method, path, query []byte, http10 bool, err error) {
	s1 := bytes.IndexByte(requestLine, ' ')
	if s1 <= 0 {
		return nil, nil, nil, false, errMalformedRequestLine
	}
	s2rel := bytes.IndexByte(requestLine[s1+1:], ' ')
	if s2rel <= 0 {
		return nil, nil, nil, false, errMalformedRequestLine
	}
	s2 := s1 + 1 + s2rel
	if s2+1 >= len(requestLine) || bytes.IndexByte(requestLine[s2+1:], ' ') >= 0 {
		return nil, nil, nil, false, errMalformedRequestLine
	}

	methodBytes := requestLine[:s1]
	if !validHTTPToken(methodBytes) {
		return nil, nil, nil, false, errInvalidMethod
	}
	rawPath := requestLine[s1+1 : s2]
	version := b2s(requestLine[s2+1:])
	if version != "HTTP/1.1" && version != "HTTP/1.0" {
		return nil, nil, nil, false, errUnsupportedVersion
	}
	for _, c := range rawPath {
		if c <= 0x20 || c == 0x7f {
			return nil, nil, nil, false, errInvalidTarget
		}
	}
	path, query = splitPathQuery(rawPath)
	return methodBytes, path, query, version == "HTTP/1.0", nil
}

// parseHeaderLineSlow parses the header line that starts at pos in header the
// way the parser always did: split at the next CRLF, reject an empty line,
// obs-fold and a malformed name, lowercase the name, trim and validate the
// value. It returns the lowercased name, the value, and the offset of the next
// line. The fast path in fillHTTPRequest defers to it for every line it does
// not accept outright.
func parseHeaderLineSlow(header []byte, pos int) (key, val string, next int, err error) {
	rel := bytes.Index(header[pos:], crlfBytes)
	end := len(header)
	if rel >= 0 {
		end = pos + rel
	}
	line := header[pos:end]
	next = end
	if rel >= 0 {
		next += 2
	}

	if len(line) == 0 {
		return "", "", 0, errEmptyHeaderLine
	}
	if line[0] == ' ' || line[0] == '\t' {
		return "", "", 0, errObsFold
	}
	colon := bytes.IndexByte(line, ':')
	if colon <= 0 || !validHTTPToken(line[:colon]) {
		return "", "", 0, errMalformedHeaderLine
	}

	key = lowerHeaderKey(line[:colon])
	valBytes := bytes.TrimSpace(line[colon+1:])
	if !validHeaderValue(valBytes) {
		return "", "", 0, errInvalidHeaderValue
	}
	return key, b2s(valBytes), next, nil
}

var crlfBytes = []byte("\r\n")

func validHTTPToken(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, c := range b {
		if !tokenChar[c] {
			return false
		}
	}
	return true
}

// tokenChar is the RFC 9110 tchar set as a lookup table. The header-name check
// runs for every header of every request, and '-' (in most real header names)
// used to fall through to a strings.ContainsRune scan.
var tokenChar = func() (t [256]bool) {
	for c := 'a'; c <= 'z'; c++ {
		t[c] = true
	}
	for c := 'A'; c <= 'Z'; c++ {
		t[c] = true
	}
	for c := '0'; c <= '9'; c++ {
		t[c] = true
	}
	for _, c := range "!#$%&'*+-.^_`|~" {
		t[c] = true
	}
	return
}()

func validHeaderValue(b []byte) bool {
	for _, c := range b {
		if c == '\t' || (c >= 0x20 && c != 0x7f) {
			continue
		}
		return false
	}
	return true
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
		if b[0] == 'H' && b[1] == 'E' && b[2] == 'A' && b[3] == 'D' {
			return Method("HEAD") // constant string: no copy
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
		if b[0] == 'O' && b[1] == 'P' && b[2] == 'T' && b[3] == 'I' && b[4] == 'O' && b[5] == 'N' && b[6] == 'S' {
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

// Connection modes, from the request's HTTP version and Connection header.
const (
	connModeKeepAlive   uint8 = iota // HTTP/1.1 default: keep the connection open
	connModeClose                    // close after the response
	connModeKeepAlive10              // HTTP/1.0 that asked to keep alive: say so back
)

// connectionMode applies RFC 9112 §9.3: HTTP/1.1 persists unless a "close" token
// is present; HTTP/1.0 closes unless a "keep-alive" token is present.
//
// The server used to ignore all of this, which is wrong in a way that is easy to
// miss: a client that sends "Connection: close", or any HTTP/1.0 client such as
// ApacheBench without -k, waits for the server to close the socket to know the
// response has ended, and the server never did.
func connectionMode(http10 bool, value string) uint8 {
	// A single "keep-alive" or "close" token, in any ASCII case, is nearly every
	// Connection header there is. Answer it without splitting and folding; the
	// general loop below still decides everything else, including values that
	// only Unicode case folding would match.
	switch len(value) {
	case 10:
		if asciiEqualFold(value, "keep-alive") {
			if http10 {
				return connModeKeepAlive10
			}
			return connModeKeepAlive
		}
	case 5:
		if asciiEqualFold(value, "close") {
			return connModeClose
		}
	}
	closeTok, keepTok := false, false
	for len(value) > 0 {
		tok := value
		if i := strings.IndexByte(value, ','); i >= 0 {
			tok, value = value[:i], value[i+1:]
		} else {
			value = ""
		}
		tok = strings.TrimSpace(tok)
		switch {
		case strings.EqualFold(tok, "close"):
			closeTok = true
		case strings.EqualFold(tok, "keep-alive"):
			keepTok = true
		}
	}
	switch {
	case closeTok:
		return connModeClose
	case http10 && keepTok:
		return connModeKeepAlive10
	case http10:
		return connModeClose
	}
	return connModeKeepAlive
}

// asciiEqualFold reports whether s equals lower once ASCII capitals in s are
// folded. lower must be all lowercase ASCII. Non-ASCII bytes never match, so a
// false result means "ask the general code", not "different".
func asciiEqualFold(s, lower string) bool {
	if len(s) != len(lower) {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != lower[i] {
			return false
		}
	}
	return true
}

// joinDuplicateHeaders rebuilds the values of repeated request headers as RFC 9110
// §5.3 prescribes: field values joined with ", " in the order received ("; " for
// Cookie, per RFC 6265). A repeated Authorization then reads "Bearer a, Bearer b",
// which no token parser accepts, so it fails closed instead of authenticating
// whichever copy happened to come last.
func joinDuplicateHeaders(req *HTTPRequest, block []byte) {
	seen := make(map[string]string, len(req.Header))
	dup := make(map[string]bool)
	for len(block) > 0 {
		end := bytes.Index(block, []byte("\r\n"))
		line := block
		if end >= 0 {
			line, block = block[:end], block[end+2:]
		} else {
			block = nil
		}
		colon := bytes.IndexByte(line, ':')
		if colon <= 0 {
			continue
		}
		key := string(bytes.ToLower(line[:colon]))
		val := string(bytes.TrimSpace(line[colon+1:]))
		if prev, ok := seen[key]; ok {
			sep := ", "
			if key == "cookie" {
				sep = "; "
			}
			seen[key] = prev + sep + val
			dup[key] = true
		} else {
			seen[key] = val
		}
	}
	for key := range dup {
		req.Header[key] = seen[key]
	}
}
