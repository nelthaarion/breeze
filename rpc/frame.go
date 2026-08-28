package rpc

// frame.go — delimiting JSON-RPC messages on a raw TCP stream.
//
// JSON-RPC 2.0 defines a message but not a framing, so a server reading from a
// socket has to decide where one message ends and the next begins. This
// implementation scans for a structurally complete JSON value: it counts
// bracket depth outside of string literals and returns as soon as depth falls
// back to zero.
//
// The alternatives were both worse. Splitting on '\n' breaks the moment a
// client pretty-prints its request, and it is not something the specification
// entitles a server to require. A length-prefix header is unambiguous but is
// not JSON-RPC — a conforming client has no reason to send one, so the server
// would only interoperate with its own client library. Structural scanning
// accepts newline-delimited, whitespace-separated, and tightly packed streams
// without the client having to announce which it chose.

// scanState is the carry-over needed to resume a scan that ran out of bytes
// mid-value. It lives in the connection's gnet context alongside the pending
// bytes, so a message split across any number of reads costs no re-scanning of
// the part already examined.
type scanState struct {
	depth      int  // unclosed '{' and '[' seen so far
	inString   bool // cursor is inside a string literal
	escaped    bool // previous byte was a backslash inside a string
	started    bool // a non-whitespace byte has been seen for this value
	scanned    int  // bytes of the pending buffer already classified
	valueStart int  // offset of the in-progress value's first byte
}

// reset returns s to the state for "no value in progress".
func (s *scanState) reset() {
	s.depth = 0
	s.inString = false
	s.escaped = false
	s.started = false
	s.scanned = 0
	s.valueStart = 0
}

// pendingWhitespaceOnly reports whether the scanner has consumed the entire
// buffer without finding the start of a value.
//
// The caller uses this to discard a buffer that holds nothing but the
// separators between messages. Without it, a client that keeps a connection
// alive by sending newlines would grow the pending buffer until the
// max-message guard closed a connection that had done nothing wrong.
func (s *scanState) pendingWhitespaceOnly(buffered int) bool {
	return !s.started && s.scanned >= buffered
}

// scanResult reports what nextValue found.
type scanResult int

const (
	// scanIncomplete means the buffer holds the beginning of a value, or only
	// whitespace, and more bytes are needed.
	scanIncomplete scanResult = iota
	// scanComplete means buf[start:end] is exactly one structurally complete
	// JSON value.
	scanComplete
	// scanInvalid means the stream cannot be resynchronised: a value began with
	// a byte that cannot start any JSON value, or brackets closed below depth
	// zero. The connection is dropped, because there is no defined point at
	// which to resume reading.
	scanInvalid
)

// nextValue advances the scan over buf and reports whether a complete JSON
// value is available.
//
// On scanComplete, the value is buf[start:end] and s is reset so the next call
// begins a fresh value. On scanIncomplete, s remembers how far it got and the
// caller must retain buf. start is the offset of the value's first
// non-whitespace byte, which lets the caller drop leading separators without a
// second pass.
//
// The scan is a byte loop with no allocation and no decoding — it does not
// validate that the bytes are well-formed JSON beyond bracket and string
// structure. Full validation happens once, in the decoder, which has to walk
// the bytes anyway; doing it twice would be pure overhead. A value that is
// structurally closed but semantically broken (`{"a"}`) is therefore framed
// here and rejected as a parse error there, which is the correct outcome per
// spec §5.1 either way.
func nextValue(buf []byte, s *scanState) (start, end int, res scanResult) {
	i := s.scanned

	// Skip whitespace ahead of the value. Per RFC 8259 §2 the only whitespace
	// outside a string is space, tab, LF and CR, which is also exactly the set
	// a newline-delimited client will place between messages.
	if !s.started {
		for i < len(buf) && isSpace(buf[i]) {
			i++
		}
		if i == len(buf) {
			// Consume the whitespace so it is not rescanned, and report that
			// the buffer is effectively empty.
			s.scanned = i
			return 0, 0, scanIncomplete
		}
		if !canStartValue(buf[i]) {
			return 0, 0, scanInvalid
		}
		s.started = true
		s.valueStart = i
	}

	for ; i < len(buf); i++ {
		c := buf[i]

		if s.inString {
			switch {
			case s.escaped:
				// This byte is the escaped character; a '"' here does not
				// close the string and a '\\' here does not start an escape.
				s.escaped = false
			case c == '\\':
				s.escaped = true
			case c == '"':
				s.inString = false
			}
			continue
		}

		switch c {
		case '"':
			s.inString = true
		case '{', '[':
			s.depth++
		case '}', ']':
			s.depth--
			if s.depth < 0 {
				return 0, 0, scanInvalid
			}
			if s.depth == 0 {
				vs := s.valueStart
				s.reset()
				return vs, i + 1, scanComplete
			}
		default:
			// A bare scalar at depth zero — a number, true/false/null, or a
			// string, none of which are valid JSON-RPC messages but all of
			// which are valid JSON values that must be consumed and rejected
			// rather than left to jam the stream.
			//
			// §4.2 requires the server to answer a non-object, non-array value
			// with Invalid Request, so it has to be delimited first. A scalar
			// ends at the first whitespace or structural byte.
			if s.depth == 0 {
				return scanScalar(buf, s, i)
			}
		}
	}

	s.scanned = i
	return 0, 0, scanIncomplete
}

// scanScalar delimits a bare top-level scalar starting at i.
//
// It is only reached for input that is already invalid as a JSON-RPC message,
// so it is deliberately outside the hot loop above: the common path never calls
// it, and keeping it in its own function keeps nextValue's body small enough to
// stay cheap.
func scanScalar(buf []byte, s *scanState, i int) (int, int, scanResult) {
	if s.inString {
		// Unreachable: the caller only dispatches here from the non-string
		// branch. Guarded anyway so a future edit cannot silently mis-frame.
		return 0, 0, scanInvalid
	}
	for j := i; j < len(buf); j++ {
		switch buf[j] {
		case ' ', '\t', '\n', '\r', ',', '}', ']', '{', '[', '"':
			vs := s.valueStart
			s.reset()
			return vs, j, scanComplete
		}
	}
	// The scalar may continue into the next read: "12" could become "123".
	// Record progress and wait rather than guessing at a boundary.
	s.scanned = len(buf)
	return 0, 0, scanIncomplete
}

// isSpace reports whether c is JSON insignificant whitespace (RFC 8259 §2).
func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// canStartValue reports whether c can begin a JSON value.
//
// Anything else means the stream is not carrying JSON at all — a stray ':' or
// ',' at top level, or binary garbage — and there is no defined resynchronise
// point, so the connection is closed instead of guessing.
func canStartValue(c byte) bool {
	switch c {
	case '{', '[', '"', 't', 'f', 'n', '-':
		return true
	}
	return c >= '0' && c <= '9'
}
