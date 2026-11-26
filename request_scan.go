package breeze

import (
	"bytes"
	"encoding/binary"
	"math/bits"
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
	// Sixteen bytes per step while the input is long enough. When any lane is
	// flagged (< 0x20 or == 0x7f), the lowest flagged bit gives the exact byte
	// index via TZCNT, jumping straight to CR without a redundant 8-byte pass
	// or scalar walk. Only HTAB (legal but rare inside values) resumes scanning.
	for i+16 <= n {
		w1 := binary.LittleEndian.Uint64(b[i:])
		x1 := w1 ^ 0x7f7f7f7f7f7f7f7f
		if m1 := (((w1 - 0x2020202020202020) &^ w1) | ((x1 - lanesOne) &^ x1)) & lanesHigh; m1 != 0 {
			i += bits.TrailingZeros64(m1) >> 3
			if b[i] != '\t' {
				return i
			}
			i++
			continue
		}
		w2 := binary.LittleEndian.Uint64(b[i+8:])
		x2 := w2 ^ 0x7f7f7f7f7f7f7f7f
		if m2 := (((w2 - 0x2020202020202020) &^ w2) | ((x2 - lanesOne) &^ x2)) & lanesHigh; m2 != 0 {
			i += 8 + (bits.TrailingZeros64(m2) >> 3)
			if b[i] != '\t' {
				return i
			}
			i++
			continue
		}
		i += 16
	}
	for i+8 <= n {
		w := binary.LittleEndian.Uint64(b[i:])
		x := w ^ 0x7f7f7f7f7f7f7f7f
		if m := (((w - 0x2020202020202020) &^ w) | ((x - lanesOne) &^ x)) & lanesHigh; m != 0 {
			i += bits.TrailingZeros64(m) >> 3
			if b[i] != '\t' {
				return i
			}
			i++
			continue
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
		x := w ^ 0x7f7f7f7f7f7f7f7f
		if m := (((w - 0x2121212121212121) &^ w) | ((x - lanesOne) &^ x)) & lanesHigh; m != 0 {
			return i + (bits.TrailingZeros64(m) >> 3)
		}
		i += 8
	}
	for i < n && b[i] > 0x20 && b[i] != 0x7f {
		i++
	}
	return i
}

// indexCRLFCRLF returns the index of the first "\r\n\r\n" in b, or -1.
// Small header blocks (<= 512 bytes, covering nearly every HTTP request) scan
// 8 bytes at a time via inline SWAR to avoid per-line bytes.IndexByte call
// overhead; larger buffers use SIMD bytes.IndexByte for maximum bandwidth.
func indexCRLFCRLF(b []byte) int {
	n := len(b)
	if n < 4 {
		return -1
	}
	limit := n - 2
	if n <= 512 {
		const nlMask = 0x0a0a0a0a0a0a0a0a
		i := 1
		for i+8 <= limit {
			w := binary.LittleEndian.Uint64(b[i:]) ^ nlMask
			if m := (w - lanesOne) &^ w & lanesHigh; m != 0 {
				pos := i + (bits.TrailingZeros64(m) >> 3)
				if b[pos-1] == '\r' && b[pos+1] == '\r' && b[pos+2] == '\n' {
					return pos - 1
				}
				i = pos + 1
				continue
			}
			i += 8
		}
		for ; i < limit; i++ {
			if b[i] == '\n' && b[i-1] == '\r' && b[i+1] == '\r' && b[i+2] == '\n' {
				return i - 1
			}
		}
		return -1
	}
	off := 1
	for off < limit {
		rel := bytes.IndexByte(b[off:limit], '\n')
		if rel < 0 {
			return -1
		}
		i := off + rel
		if b[i-1] == '\r' && b[i+1] == '\r' && b[i+2] == '\n' {
			return i - 1
		}
		off = i + 1
	}
	return -1
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
// directly with a freshly allocated map and one backing array for all first
// values. Anything else is handed to net/url unchanged, so escapes,
// semicolons, parameter limits and every error stay its business.
//
// Plainness validation and '&'/'=' splitting are fused into a single walk over
// raw: all five special bytes ('%', '&', '+', ';', '=') are <= '=' (0x3d), so
// ordinary ASCII identifier/value bytes skip the switch in one comparison.
//
// The returned url.Values map and its backing []string array are freshly
// allocated and never reused or cleared across requests, so callers may retain
// req.Query safely. The returned strings are substrings of raw, exactly as
// with net/url when nothing needs unescaping: they live as long as the header
// block does.
func parseRequestQuery(raw string) (url.Values, error) {
	if queryFastOK && len(raw) <= maxFastQueryBytes {
		var (
			keys  [maxFastQueryParams]string
			rvals [maxFastQueryParams]string
			count int
		)
		segStart := 0
		eqPos := -1
		seps := 0
		plain := true
		for i := 0; i < len(raw); i++ {
			c := raw[i]
			if c > '=' {
				continue
			}
			switch c {
			case '&':
				seps++
				if seps >= maxFastQueryParams {
					plain = false
					break
				}
				if i > segStart {
					if eqPos >= 0 {
						keys[count] = raw[segStart:eqPos]
						rvals[count] = raw[eqPos+1 : i]
					} else {
						keys[count] = raw[segStart:i]
						rvals[count] = ""
					}
					count++
				}
				segStart = i + 1
				eqPos = -1
			case '=':
				if eqPos < 0 {
					eqPos = i
				}
			case '%', '+', ';':
				plain = false
			}
			if !plain {
				break
			}
		}
		if plain && segStart < len(raw) {
			if count >= maxFastQueryParams {
				plain = false
			} else {
				if eqPos >= 0 {
					keys[count] = raw[segStart:eqPos]
					rvals[count] = raw[eqPos+1:]
				} else {
					keys[count] = raw[segStart:]
					rvals[count] = ""
				}
				count++
			}
		}
		if plain {
			if count == 0 {
				return make(url.Values), nil
			}
			m := make(url.Values, count)
			vals := make([]string, count)

			vIdx := 0
			for i := 0; i < count; i++ {
				key := keys[i]
				val := rvals[i]
				dup := false
				for j := 0; j < vIdx; j++ {
					if keys[j] == key {
						dup = true
						break
					}
				}
				if dup {
					m[key] = append(m[key], val)
				} else {
					keys[vIdx] = key
					vals[vIdx] = val
					m[key] = vals[vIdx : vIdx+1 : vIdx+1]
					vIdx++
				}
			}
			return m, nil
		}
	}
	return url.ParseQuery(raw)
}
