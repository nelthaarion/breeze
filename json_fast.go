package breeze

import (
	"math"
	"strconv"
	"unicode/utf8"

	"github.com/nelthaarion/go-json-plus"
)

// responseBodyWriter adapts *HTTPResponse.bodyBuf as an io.Writer for
// go-json-plus's Encoder. Because go-json-plus's Encoder.Encode encodes into
// its own pooled RuntimeContext buffer and calls Write once with a trailing
// '\n', writing into bodyBuf and trimming that '\n' produces the exact byte
// sequence of json.Marshal with zero per-response heap allocation.
type responseBodyWriter struct {
	res *HTTPResponse
}

func (w *responseBodyWriter) Write(p []byte) (int, error) {
	origLen := len(p)
	if origLen > 0 && p[origLen-1] == '\n' {
		p = p[:origLen-1]
	}
	w.res.bodyBuf = append(w.res.bodyBuf, p...)
	return origLen, nil
}

// encodeJSONInto encodes v directly into r.bodyBuf with zero allocations on
// common types (map[string]string, primitive map[string]any, and structs via
// the pooled go-json-plus Encoder). Output matches json.Marshal byte-for-byte.
func (r *HTTPResponse) encodeJSONInto(v any) error {
	switch m := v.(type) {
	case map[string]string:
		if len(m) <= maxFastJSONMapLen {
			r.bodyBuf = appendJSONStringMap(r.bodyBuf[:0], m)
			return nil
		}
	case map[string]any:
		if len(m) <= maxFastJSONMapLen {
			if buf, ok := appendJSONAnyMap(r.bodyBuf[:0], m); ok {
				r.bodyBuf = buf
				return nil
			}
		}
	}

	if r.jsonEnc == nil {
		r.jsonW.res = r
		r.jsonEnc = json.NewEncoder(&r.jsonW)
	}
	r.bodyBuf = r.bodyBuf[:0]
	return r.jsonEnc.Encode(v)
}

const maxFastJSONMapLen = 16

// jsonSafeByte marks ASCII bytes that require no escaping inside a JSON string
// with HTML escaping enabled (matching go-json-plus default Marshal semantics).
var jsonSafeByte = func() (t [256]bool) {
	for c := 0x20; c < 0x80; c++ {
		t[c] = true
	}
	t['"'] = false
	t['\\'] = false
	t['<'] = false
	t['>'] = false
	t['&'] = false
	return
}()

const hexDigits = "0123456789abcdef"

// appendJSONString appends s as a quoted JSON string to dst, matching
// go-json-plus's Marshal (HTMLEscapeOption | NormalizeUTF8Option) byte-for-byte.
func appendJSONString(dst []byte, s string) []byte {
	n := len(s)
	i := 0
	for i < n && jsonSafeByte[s[i]] {
		i++
	}
	if i == n {
		dst = append(dst, '"')
		dst = append(dst, s...)
		return append(dst, '"')
	}

	dst = append(dst, '"')
	start := 0
	for i < n {
		c := s[i]
		if jsonSafeByte[c] {
			i++
			continue
		}
		if c < utf8.RuneSelf {
			if start < i {
				dst = append(dst, s[start:i]...)
			}
			switch c {
			case '"':
				dst = append(dst, '\\', '"')
			case '\\':
				dst = append(dst, '\\', '\\')
			case '\b':
				dst = append(dst, '\\', 'b')
			case '\t':
				dst = append(dst, '\\', 't')
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\f':
				dst = append(dst, '\\', 'f')
			case '\r':
				dst = append(dst, '\\', 'r')
			default:
				// Control bytes (< 0x20) and HTML-escaped bytes ('<', '>', '&').
				dst = append(dst, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
			}
			i++
			start = i
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			if start < i {
				dst = append(dst, s[start:i]...)
			}
			dst = append(dst, 0xef, 0xbf, 0xbd) // UTF-8 U+FFFD replacement
			i++
			start = i
			continue
		}
		if r == '\u2028' || r == '\u2029' {
			if start < i {
				dst = append(dst, s[start:i]...)
			}
			if r == '\u2028' {
				dst = append(dst, `\u2028`...)
			} else {
				dst = append(dst, `\u2029`...)
			}
			i += size
			start = i
			continue
		}
		// Valid non-ASCII UTF-8 rune: keep in the current unescaped run.
		i += size
	}
	if start < len(s) {
		dst = append(dst, s[start:]...)
	}
	return append(dst, '"')
}

type jsonKVStr struct {
	k string
	v string
}

func appendJSONStringMap(dst []byte, m map[string]string) []byte {
	if m == nil {
		return append(dst, "null"...)
	}
	n := len(m)
	if n == 0 {
		return append(dst, '{', '}')
	}
	if n == 1 {
		dst = append(dst, '{')
		for k, v := range m {
			dst = appendJSONString(dst, k)
			dst = append(dst, ':')
			dst = appendJSONString(dst, v)
		}
		return append(dst, '}')
	}
	var entries [maxFastJSONMapLen]jsonKVStr
	i := 0
	for k, v := range m {
		// Insertion sort in place as keys arrive (tiny n <= 16).
		j := i
		for j > 0 && entries[j-1].k > k {
			entries[j] = entries[j-1]
			j--
		}
		entries[j] = jsonKVStr{k: k, v: v}
		i++
	}
	dst = append(dst, '{')
	for j := 0; j < n; j++ {
		if j > 0 {
			dst = append(dst, ',')
		}
		dst = appendJSONString(dst, entries[j].k)
		dst = append(dst, ':')
		dst = appendJSONString(dst, entries[j].v)
	}
	return append(dst, '}')
}

type jsonKVAny struct {
	k string
	v any
}

func appendJSONAnyMap(dst []byte, m map[string]any) ([]byte, bool) {
	if m == nil {
		return append(dst, "null"...), true
	}
	n := len(m)
	if n == 0 {
		return append(dst, '{', '}'), true
	}
	origLen := len(dst)
	if n == 1 {
		dst = append(dst, '{')
		for k, v := range m {
			dst = appendJSONString(dst, k)
			dst = append(dst, ':')
			var ok bool
			dst, ok = appendJSONPrimitive(dst, v)
			if !ok {
				return dst[:origLen], false
			}
		}
		return append(dst, '}'), true
	}
	var entries [maxFastJSONMapLen]jsonKVAny
	i := 0
	for k, v := range m {
		j := i
		for j > 0 && entries[j-1].k > k {
			entries[j] = entries[j-1]
			j--
		}
		entries[j] = jsonKVAny{k: k, v: v}
		i++
	}
	dst = append(dst, '{')
	for j := 0; j < n; j++ {
		if j > 0 {
			dst = append(dst, ',')
		}
		dst = appendJSONString(dst, entries[j].k)
		dst = append(dst, ':')
		var ok bool
		dst, ok = appendJSONPrimitive(dst, entries[j].v)
		if !ok {
			return dst[:origLen], false
		}
	}
	return append(dst, '}'), true
}

// appendJSONPrimitive encodes exact built-in primitive types without reflection
// or allocation. Named types (which might implement json.Marshaler) do not
// match concrete built-in cases and return ok=false so the full encoder runs.
func appendJSONPrimitive(dst []byte, v any) ([]byte, bool) {
	switch x := v.(type) {
	case nil:
		return append(dst, "null"...), true
	case string:
		return appendJSONString(dst, x), true
	case int:
		return strconv.AppendInt(dst, int64(x), 10), true
	case int64:
		return strconv.AppendInt(dst, x, 10), true
	case int32:
		return strconv.AppendInt(dst, int64(x), 10), true
	case uint:
		return strconv.AppendUint(dst, uint64(x), 10), true
	case uint64:
		return strconv.AppendUint(dst, x, 10), true
	case uint32:
		return strconv.AppendUint(dst, uint64(x), 10), true
	case float64:
		abs := math.Abs(x)
		if math.IsNaN(x) || math.IsInf(x, 0) || (abs != 0 && (abs < 1e-6 || abs >= 1e21)) {
			return dst, false
		}
		return strconv.AppendFloat(dst, x, 'f', -1, 64), true
	case bool:
		if x {
			return append(dst, "true"...), true
		}
		return append(dst, "false"...), true
	default:
		return dst, false
	}
}
