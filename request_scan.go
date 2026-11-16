package breeze

import (
	"bytes"
	"encoding/binary"
	"net/url"
	"strings"
)

// Scanning primitives for fillHTTPRequest.
//
// Everything here reads the bytes it is given and writes nothing, with one
// deliberate exception documented on keyLower: header names are lowercased by
// fillHTTPRequest itself, in place, exactly as before.

// keyLower maps a byte of a header name to its lowercase form, and to 0 for
// every byte that may not appear in one. It is tokenChar and lowerHeaderKey
// fused into one lookup, so a header name is validated, lowercased and
// delimited in a single pass: the first byte that maps to 0 ends the name, and
// it is a valid name only if that byte is ':'.
var keyLower = func() (t [256]byte) {
	for c := 0; c < 256; c++ {
		if !tokenChar[c] {
			continue
		}
		if c >= 'A' && c <= 'Z' {
			t[c] = byte(c) + 32
		} else {
			t[c] = byte(c)
		}
	}
	return
}()

// valTab marks the bytes validHeaderValue accepts: HTAB, SP through '~', and
// the obs-text range 0x80-0xFF.
var valTab = func() (t [256]bool) {
	t['\t'] = true
	for c := 0x20; c < 0x7f; c++ {
		t[c] = true
	}
	for c := 0x80; c < 0x100; c++ {
		t[c] = true
	}
	return
}()

const (
	lanesOne  = 0x0101010101010101
	lanesHigh = 0x8080808080808080
)

// scanValue returns the index of the first byte of b at or after i that
// validHeaderValue would reject (or would not accept as part of a value that
// goes on): a control character other than HTAB, or DEL. It returns len(b)
// when every remaining byte is acceptable. A CR therefore stops the scan,
// which is what finds the end of a header line without a second pass.
//
// Sixteen, then eight, bytes are tested per step. A lane is flagged when it is below 0x20
// (the classic "has less than" test) or equal to 0x7f (the "has zero" test on
// the lane xor 0x7f). Both tests are exact for the lowest flagged lane and may
// over-report above it, which is harmless here: the moment any lane is
// flagged the loop drops to the exact per-byte table walk below, and it never
// resumes word-at-a-time, so a false flag costs time but never correctness.
// HTAB sits below 0x20, so a value with a tab in it finishes bytewise; tabs
// inside values are legal but rare.
func scanValue(b []byte, i int) int {
	n := len(b)
	// Sixteen bytes per step while the input is long enough: two words tested
	// together, so the loop overhead is paid once per two words. Header values
	// are the bulk of a request (User-Agent, Accept, Cookie), and most of them
	// are longer than eight bytes.
	for i+16 <= n {
		w1 := binary.LittleEndian.Uint64(b[i:])
		w2 := binary.LittleEndian.Uint64(b[i+8:])
		lt1 := (w1 - 0x2020202020202020) &^ w1 & lanesHigh
		lt2 := (w2 - 0x2020202020202020) &^ w2 & lanesHigh
		x1 := w1 ^ 0x7f7f7f7f7f7f7f7f
		x2 := w2 ^ 0x7f7f7f7f7f7f7f7f
		eq1 := (x1 - lanesOne) &^ x1 & lanesHigh
		eq2 := (x2 - lanesOne) &^ x2 & lanesHigh
		if lt1|lt2|eq1|eq2 != 0 {
			break
		}
		i += 16
	}
	for i+8 <= n {
		w := binary.LittleEndian.Uint64(b[i:])
		lt := (w - 0x2020202020202020) &^ w & lanesHigh
		x := w ^ 0x7f7f7f7f7f7f7f7f
		eq := (x - lanesOne) &^ x & lanesHigh
		if lt|eq != 0 {
			break
		}
		i += 8
	}
	for i < n && valTab[b[i]] {
		i++
	}
	return i
}

// scanTarget returns the index of the first byte of b at or after i that may
// not appear in a request target: SP, a control character or DEL (the bytes
// fillHTTPRequest rejects as "invalid request target"), or len(b). The word
// test is scanValue's, with the lower bound moved from 0x20 to 0x21 so that a
// space is flagged too.
func scanTarget(b []byte, i int) int {
	n := len(b)
	for i+8 <= n {
		w := binary.LittleEndian.Uint64(b[i:])
		lt := (w - 0x2121212121212121) &^ w & lanesHigh
		x := w ^ 0x7f7f7f7f7f7f7f7f
		eq := (x - lanesOne) &^ x & lanesHigh
		if lt|eq != 0 {
			break
		}
		i += 8
	}
	for i < n && b[i] > 0x20 && b[i] != 0x7f {
		i++
	}
	return i
}

// indexCRLF returns the index of the first "\r\n" in b, or -1. A lone CR does
// not count.
func indexCRLF(b []byte) int {
	off := 0
	for {
		i := bytes.IndexByte(b[off:], '\r')
		if i < 0 {
			return -1
		}
		i += off
		if i+1 < len(b) && b[i+1] == '\n' {
			return i
		}
		off = i + 1
	}
}

// parseContentLength parses a Content-Length field value: one or more ASCII
// digits and nothing else, no larger than the largest int. ok is false for
// anything else.
//
// strconv.ParseInt, which this replaces, also accepted a leading '+' or '-'
// ("+5", "-0"). Neither is a valid Content-Length (RFC 9110 §8.6: 1*DIGIT), and
// a front end that rejects or ignores such a header while this server honours
// it is a classic request-smuggling disagreement, so they are refused now.
func parseContentLength(s string) (n int, ok bool) {
	if len(s) == 0 {
		return 0, false
	}
	const maxInt = uint64(^uint(0) >> 1)
	var v uint64
	for i := 0; i < len(s); i++ {
		d := uint64(s[i]) - '0'
		if d > 9 {
			return 0, false
		}
		if v > (maxInt-d)/10 {
			return 0, false
		}
		v = v*10 + d
	}
	return int(v), true
}

// queryFastOK reports whether parseRequestQuery may answer small queries
// itself. net/url refuses queries with too many parameters (Go 1.27 added a
// limit that GODEBUG=urlmaxqueryparams can lower), and the fast path never
// looks at that limit, so it is only used when a probe of the largest query it
// will accept passes through net/url untouched.
var queryFastOK = func() bool {
	_, err := url.ParseQuery(strings.Repeat("a&", maxFastQueryParams-1) + "a")
	return err == nil
}()

const (
	maxFastQueryParams = 16
	maxFastQueryBytes  = 512
)

// parseRequestQuery is url.ParseQuery for the raw query of a request target.
//
// Most queries are a handful of plain "k=v" pairs, and for those net/url does
// work this server does not need: a map, and one separately allocated
// one-element slice per key. When the query has no '%', '+' or ';' —
// the only bytes net/url acts on besides '&' and '=' — and at most
// maxFastQueryParams parameters, every key and value is exactly the substring
// it was cut from, which is also what net/url returns, so the result is built
// directly with one backing array for all first values. Anything else is
// handed to net/url unchanged, so escapes, semicolons, parameter limits and
// every error stay its business.
//
// The returned strings are substrings of raw, exactly as with net/url when
// nothing needs unescaping: they live as long as the header block does.
func parseRequestQuery(raw string) (url.Values, error) {
	if queryFastOK && len(raw) <= maxFastQueryBytes {
		n := 1
		plain := true
		for i := 0; i < len(raw); i++ {
			switch raw[i] {
			case '%', '+', ';':
				plain = false
			case '&':
				n++
			}
		}
		if plain && n <= maxFastQueryParams {
			m := make(url.Values, n)
			vals := make([]string, 0, n)
			for rest := raw; rest != ""; {
				var seg string
				seg, rest, _ = strings.Cut(rest, "&")
				if seg == "" {
					continue
				}
				key, value, _ := strings.Cut(seg, "=")
				if old, dup := m[key]; dup {
					m[key] = append(old, value)
				} else {
					vals = append(vals, value)
					m[key] = vals[len(vals)-1 : len(vals) : len(vals)]
				}
			}
			return m, nil
		}
	}
	return url.ParseQuery(raw)
}
