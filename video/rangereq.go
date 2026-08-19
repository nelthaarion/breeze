package video

import (
	"strconv"
	"strings"
)

// byteRange is a resolved, absolute byte interval inside a file. Both ends
// are inclusive because that is what the wire format uses: the first 1024
// bytes of a file are "bytes 0-1023/<size>", not 0-1024.
type byteRange struct {
	Start int64
	End   int64
}

// Length is the number of bytes the range covers.
func (r byteRange) Length() int64 { return r.End - r.Start + 1 }

// contentRange renders the RFC 9110 §14.4 Content-Range value for a file
// of size bytes: "bytes 0-1023/146515".
func (r byteRange) contentRange(size int64) string {
	var b strings.Builder
	b.Grow(40)
	b.WriteString("bytes ")
	b.WriteString(strconv.FormatInt(r.Start, 10))
	b.WriteByte('-')
	b.WriteString(strconv.FormatInt(r.End, 10))
	b.WriteByte('/')
	b.WriteString(strconv.FormatInt(size, 10))
	return b.String()
}

// parseRange resolves a Range header value against a file of size bytes.
//
// The three-way return is what makes the caller's logic honest, because
// HTTP distinguishes three outcomes that are easy to conflate:
//
//   - (nil, nil): serve the whole file with 200. This covers an absent
//     header and, deliberately, a malformed one: RFC 9110 §14.2 requires
//     a server to ignore a Range it cannot parse rather than reject it,
//     so a broken proxy that mangles the header degrades to a normal
//     download instead of breaking playback.
//   - (r, nil): serve r with 206.
//   - (nil, ErrRangeNotSatisfiable): the header parsed but asks for bytes
//     that do not exist. This is 416, and the caller must add
//     Content-Range: bytes * /size so the client can correct itself.
//
// Only the first range of a multi-range request is honoured. Emitting
// multipart/byteranges would be legal, but no video client asks for it,
// and returning a single satisfiable range is explicitly allowed. The
// alternative — ignoring the header — would send the entire file, which
// is far worse for a seek request.
func parseRange(header string, size int64) (*byteRange, error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return nil, nil
	}

	// Only the bytes unit exists in practice. Anything else is a unit we
	// do not implement, which the spec treats as ignorable.
	const prefix = "bytes="
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return nil, nil
	}
	spec := header[len(prefix):]

	// Take the first spec; validate that the rest at least looks like a
	// range list, so "bytes=0-1, garbage" is treated as malformed.
	if i := strings.IndexByte(spec, ','); i >= 0 {
		spec = spec[:i]
	}
	spec = strings.TrimSpace(spec)

	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return nil, nil // no dash at all: malformed, ignore
	}
	startTxt := strings.TrimSpace(spec[:dash])
	endTxt := strings.TrimSpace(spec[dash+1:])

	// A zero-length file cannot satisfy any range. Reporting 416 here
	// rather than 200-with-empty-body tells the client the truth and
	// keeps Content-Range's "*/0" meaningful.
	if size == 0 {
		return nil, ErrRangeNotSatisfiable
	}

	switch {
	case startTxt == "":
		// Suffix form, "bytes=-N": the last N bytes. Used by players to
		// grab the MP4 moov atom when it sits at the end of the file.
		if endTxt == "" {
			return nil, nil // "bytes=-" is malformed
		}
		n, err := strconv.ParseInt(endTxt, 10, 64)
		if err != nil || n < 0 {
			return nil, nil
		}
		if n == 0 {
			// "the last zero bytes" is unsatisfiable by definition.
			return nil, ErrRangeNotSatisfiable
		}
		if n > size {
			n = size // asking for more tail than exists yields the file
		}
		return &byteRange{Start: size - n, End: size - 1}, nil

	case endTxt == "":
		// Open-ended, "bytes=N-": from N to the end of the file.
		start, err := strconv.ParseInt(startTxt, 10, 64)
		if err != nil || start < 0 {
			return nil, nil
		}
		if start >= size {
			return nil, ErrRangeNotSatisfiable
		}
		return &byteRange{Start: start, End: size - 1}, nil

	default:
		start, err1 := strconv.ParseInt(startTxt, 10, 64)
		end, err2 := strconv.ParseInt(endTxt, 10, 64)
		if err1 != nil || err2 != nil || start < 0 || end < 0 {
			return nil, nil
		}
		if start > end {
			// An inverted range is an invalid ranges-specifier, which
			// the spec says to ignore rather than reject.
			return nil, nil
		}
		if start >= size {
			return nil, ErrRangeNotSatisfiable
		}
		if end >= size {
			// Asking past EOF is normal and legal: clamp. A client that
			// requests "bytes=0-99999" on a 500-byte file gets 0-499.
			end = size - 1
		}
		return &byteRange{Start: start, End: end}, nil
	}
}

// clamp shortens r to at most max bytes, reporting whether it changed.
//
// This is what keeps "bytes=0-" from streaming a whole 4 GiB file into one
// response. Returning a shorter range than requested is legal because
// Content-Range states exactly what was sent, and every player follows up
// with another request for the next span.
func (r byteRange) clamp(max int64) (byteRange, bool) {
	if max <= 0 || r.Length() <= max {
		return r, false
	}
	return byteRange{Start: r.Start, End: r.Start + max - 1}, true
}
