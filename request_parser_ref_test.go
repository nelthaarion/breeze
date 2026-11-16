package breeze

// Reference parser for differential testing.
//
// This is the request parser exactly as it was before the single-pass rewrite
// (fillHTTPRequest and its helpers, renamed with a ref prefix), with one
// deliberate difference: Content-Length must be 1*DIGIT, so a leading '+' or
// '-' is refused (see parseContentLength). It exists only so that
// request_parser_diff_test.go can feed identical bytes to both parsers and
// require identical results. Keep it frozen.

import (
	"bytes"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

func legacyAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

var legacyTokenChar = func() (t [256]bool) {
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

func legacyFillHTTPRequest(req *HTTPRequest, data []byte, ownHeaders bool, maxBody int64) (int, error) {
	scanLen := len(data)
	if scanLen > maxRequestHeaderBytes {
		scanLen = maxRequestHeaderBytes
	}
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

	header := data[:headerEnd]
	if ownHeaders {
		owned := make([]byte, headerEnd)
		copy(owned, header)
		header = owned
		req.owned = owned
	}

	lineEnd := bytes.Index(header, []byte("\r\n"))
	if lineEnd < 0 {
		return 0, fmt.Errorf("malformed request line")
	}
	requestLine := header[:lineEnd]

	s1 := bytes.IndexByte(requestLine, ' ')
	if s1 <= 0 {
		return 0, fmt.Errorf("malformed request line")
	}
	s2rel := bytes.IndexByte(requestLine[s1+1:], ' ')
	if s2rel <= 0 {
		return 0, fmt.Errorf("malformed request line")
	}
	s2 := s1 + 1 + s2rel
	if s2+1 >= len(requestLine) || bytes.IndexByte(requestLine[s2+1:], ' ') >= 0 {
		return 0, fmt.Errorf("malformed request line")
	}

	methodBytes := requestLine[:s1]
	if !legacyValidHTTPToken(methodBytes) {
		return 0, fmt.Errorf("invalid request method")
	}
	rawPath := requestLine[s1+1 : s2]
	version := b2s(requestLine[s2+1:])
	if version != "HTTP/1.1" && version != "HTTP/1.0" {
		return 0, fmt.Errorf("unsupported HTTP version")
	}
	for _, c := range rawPath {
		if c <= 0x20 || c == 0x7f {
			return 0, fmt.Errorf("invalid request target")
		}
	}
	path, query := legacySplitPathQuery(rawPath)

	if req.Header == nil {
		req.Header = make(map[string]string, 8)
	}
	req.Method = legacyInternMethod(methodBytes)
	req.Path = b2s(path)

	if len(query) > 0 {
		q, err := url.ParseQuery(b2s(query))
		if err != nil {
			return 0, fmt.Errorf("invalid query string: %w", err)
		}
		req.Query = q
	}

	contentLength := -1
	hostSeen := false
	transferEncodingSeen := false
	connValue := ""
	headerLines := 0
	pos := lineEnd + 2 // skip past \r\n of the request line

	for pos < len(header) {
		rel := bytes.Index(header[pos:], []byte("\r\n"))
		end := len(header)
		if rel >= 0 {
			end = pos + rel
		}
		line := header[pos:end]
		pos = end
		if rel >= 0 {
			pos += 2
		}

		if len(line) == 0 {
			return 0, fmt.Errorf("unexpected empty header line")
		}
		if line[0] == ' ' || line[0] == '\t' {
			return 0, fmt.Errorf("obs-fold header is not permitted")
		}
		colon := bytes.IndexByte(line, ':')
		if colon <= 0 || !legacyValidHTTPToken(line[:colon]) {
			return 0, fmt.Errorf("malformed header line")
		}

		key := legacyLowerHeaderKey(line[:colon])
		valBytes := bytes.TrimSpace(line[colon+1:])
		if !legacyValidHeaderValue(valBytes) {
			return 0, fmt.Errorf("invalid header value")
		}
		val := b2s(valBytes)

		switch key {
		case "content-length":
			if contentLength != -1 {
				return 0, fmt.Errorf("duplicate content-length")
			}
			cl64, err := strconv.ParseInt(val, 10, 64)
			if err != nil || cl64 < 0 || cl64 > int64(^uint(0)>>1) || !legacyAllDigits(val) {
				return 0, fmt.Errorf("invalid content-length")
			}
			contentLength = int(cl64)
		case "transfer-encoding":
			transferEncodingSeen = true
		case "connection":
			if connValue == "" {
				connValue = val
			} else {
				connValue += "," + val
			}
		case "host":
			if hostSeen {
				return 0, fmt.Errorf("duplicate host")
			}
			hostSeen = true
		}
		req.Header[key] = val
		headerLines++
	}

	if len(req.Header) != headerLines {
		legacyJoinDuplicateHeaders(req, header[lineEnd+2:])
	}

	if transferEncodingSeen {
		return 0, fmt.Errorf("transfer-encoding is not supported")
	}
	if version == "HTTP/1.1" && !hostSeen {
		return 0, fmt.Errorf("missing host header")
	}
	req.connMode = legacyConnectionMode(version == "HTTP/1.0", connValue)

	if maxBody > 0 && int64(contentLength) > maxBody {
		return 0, ErrBodyTooLarge
	}

	consumed := headerEnd + 4
	if contentLength > 0 {
		if contentLength > len(data)-consumed {
			return 0, nil // body not fully received yet
		}
		total := consumed + contentLength
		req.Body = data[consumed:total]
		consumed = total
	}

	return consumed, nil
}

func legacyValidHTTPToken(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, c := range b {
		if !legacyTokenChar[c] {
			return false
		}
	}
	return true
}

func legacyValidHeaderValue(b []byte) bool {
	for _, c := range b {
		if c == '\t' || (c >= 0x20 && c != 0x7f) {
			continue
		}
		return false
	}
	return true
}

func legacySplitPathQuery(raw []byte) (path, query []byte) {
	i := bytes.IndexByte(raw, '?')
	if i < 0 {
		return raw, nil
	}
	return raw[:i], raw[i+1:]
}

func legacyLowerHeaderKey(b []byte) string {
	for i := 0; i < len(b); i++ {
		if c := b[i]; c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return b2s(b)
}

func legacyInternMethod(b []byte) Method {
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
		if b[0] == 'O' && b[1] == 'P' && b[2] == 'T' && b[3] == 'I' && b[4] == 'O' && b[5] == 'N' && b[6] == 'S' {
			return OPTIONS
		}
	}
	return Method(string(b))
}

func legacyConnectionMode(http10 bool, value string) uint8 {
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

func legacyJoinDuplicateHeaders(req *HTTPRequest, block []byte) {
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
