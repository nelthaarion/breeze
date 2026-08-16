package video

import (
	"strconv"
	"strings"
	"time"
)

// This file builds the response head by hand.
//
// It has to. The framework's HTTPResponse.Bytes always appends
// "Content-Length: len(Body)" and always writes Body, so it can describe a
// complete in-memory response and nothing else. A 206 whose Content-Length
// is the length of one slice of a file, a 304 with no body at all, and a
// multi-write stream where the head goes out before the bytes exist are all
// outside what it can express. So the handler writes to the connection
// itself, and this file is the serialiser it uses.

// httpReason maps the status codes this package emits to reason phrases.
//
// Only these codes appear, so a map is honest about the contract; an
// unknown code would be a bug, and rendering it blank is preferable to
// inventing a phrase.
var httpReason = map[int]string{
	200: "OK",
	206: "Partial Content",
	304: "Not Modified",
	403: "Forbidden",
	404: "Not Found",
	405: "Method Not Allowed",
	412: "Precondition Failed",
	416: "Range Not Satisfiable",
	500: "Internal Server Error",
}

// header is one response field. A slice of these rather than a map because
// header order must be stable: tests compare bytes, and debugging a stream
// is far easier when Content-Range always sits in the same place.
type header struct {
	Key   string
	Value string
}

// head is a response head under construction.
type head struct {
	status  int
	headers []header
}

// newHead starts a head with the given status.
func newHead(status int) *head {
	return &head{status: status, headers: make([]header, 0, 12)}
}

// set appends a field, skipping empty values so callers can pass an
// unconditional set for an optional header without branching.
func (h *head) set(key, value string) *head {
	if value == "" {
		return h
	}
	h.headers = append(h.headers, header{Key: key, Value: value})
	return h
}

// setInt appends an integer-valued field.
func (h *head) setInt(key string, value int64) *head {
	return h.set(key, strconv.FormatInt(value, 10))
}

// bytes serialises the head, including the blank line that terminates it.
//
// Header values are sanitised rather than trusted. Every value here is
// derived from a file name, a size or a config string, and a file name can
// contain anything a filesystem allows — including CR and LF. Letting
// those through would end the head early and let the rest of the name be
// read as further headers or as a second response, which is HTTP response
// splitting. Stripping them at the single point where bytes reach the wire
// means no caller can forget.
func (h *head) bytes() []byte {
	var b strings.Builder
	b.Grow(256)

	b.WriteString("HTTP/1.1 ")
	b.WriteString(strconv.Itoa(h.status))
	if reason := httpReason[h.status]; reason != "" {
		b.WriteByte(' ')
		b.WriteString(reason)
	}
	b.WriteString("\r\n")

	for _, f := range h.headers {
		b.WriteString(stripCTL(f.Key))
		b.WriteString(": ")
		b.WriteString(stripCTL(f.Value))
		b.WriteString("\r\n")
	}
	b.WriteString("\r\n")
	return []byte(b.String())
}

// stripCTL removes CR, LF and NUL from a header token.
//
// Replacing rather than rejecting is deliberate: a file whose name has a
// newline in it is bizarre but not an attack in itself, and refusing to
// serve it would be a denial of service against a legitimate library. What
// matters is that the byte never reaches the wire.
func stripCTL(s string) string {
	if !strings.ContainsAny(s, "\r\n\x00") {
		return s // the overwhelmingly common case allocates nothing
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if c := s[i]; c != '\r' && c != '\n' && c != 0 {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// httpTime formats a Unix second count as an IMF-fixdate, the only format
// a server may emit for Last-Modified (RFC 9110 §5.6.7).
func httpTime(unix int64) string {
	return time.Unix(unix, 0).UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT")
}

// etagFor derives a strong validator from a file's size and mtime.
//
// Size and mtime together are what every static file server uses, because
// hashing the content would mean reading gigabytes to answer a HEAD. It is
// not perfect: a change that preserves both is invisible. That is why the
// tag is emitted as strong but paired with Last-Modified, and why the
// default Cache-Control keeps a revalidation window rather than declaring
// the file immutable.
//
// Quoting is part of the value, not decoration — an unquoted entity-tag is
// malformed and some caches will drop it.
func etagFor(size, modTime int64) string {
	var b strings.Builder
	b.Grow(32)
	b.WriteByte('"')
	b.WriteString(strconv.FormatInt(modTime, 16))
	b.WriteByte('-')
	b.WriteString(strconv.FormatInt(size, 16))
	b.WriteByte('"')
	return b.String()
}

// etagMatches reports whether an If-None-Match or If-Match value selects
// the given tag.
//
// The weak prefix is ignored on comparison because If-None-Match uses the
// weak comparison function: W/"x" and "x" both match "x" for the purposes
// of a conditional GET.
func etagMatches(headerValue, tag string) bool {
	headerValue = strings.TrimSpace(headerValue)
	if headerValue == "" {
		return false
	}
	// "*" matches any existing representation.
	if headerValue == "*" {
		return true
	}
	for _, candidate := range strings.Split(headerValue, ",") {
		candidate = strings.TrimSpace(candidate)
		candidate = strings.TrimPrefix(candidate, "W/")
		if candidate == tag {
			return true
		}
	}
	return false
}

// notModified reports whether the client's cached copy is still current.
//
// If-None-Match wins outright when present: an entity-tag is a precise
// validator, while a date is only second-accurate, so honouring the date
// as well could keep serving a stale body for up to a second after an
// edit. RFC 9110 §13.1.3 requires exactly this precedence.
func notModified(ifNoneMatch, ifModifiedSince, tag string, modTime int64) bool {
	if ifNoneMatch != "" {
		return etagMatches(ifNoneMatch, tag)
	}
	if ifModifiedSince == "" {
		return false
	}
	since, err := parseHTTPTime(ifModifiedSince)
	if err != nil {
		return false // an unparseable date is ignored, not honoured
	}
	// Truncate to whole seconds before comparing. HTTP dates carry no
	// sub-second component, so a file modified 200ms after the date the
	// client holds would otherwise look newer on every request and
	// defeat caching forever.
	return modTime <= since
}

// parseHTTPTime accepts the three date formats a client may legally send.
//
// Only IMF-fixdate may be produced, but RFC 9110 §5.6.7 requires a
// recipient to accept the two obsolete forms as well, and old proxies
// still emit them.
func parseHTTPTime(value string) (int64, error) {
	var firstErr error
	for _, layout := range []string{
		time.RFC1123,                    // IMF-fixdate
		time.RFC850,                     // obsolete RFC 850
		"Mon Jan _2 15:04:05 2006",      // ANSI C asctime
		"Mon, 02 Jan 2006 15:04:05 GMT", // fixdate with literal GMT
	} {
		if t, err := time.Parse(layout, strings.TrimSpace(value)); err == nil {
			return t.UTC().Unix(), nil
		} else if firstErr == nil {
			firstErr = err
		}
	}
	return 0, firstErr
}
