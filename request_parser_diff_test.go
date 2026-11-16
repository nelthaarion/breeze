package breeze

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unsafe"
)

// Differential tests for the single-pass request parser.
//
// fillHTTPRequest is compared with legacyFillHTTPRequest (request_parser_ref_test.go),
// the parser as it was before the rewrite, on identical bytes. Everything the
// caller can observe must match: the consumed count, the error text, every
// field of the request, the body, and — on success — the bytes of the buffer
// itself, because the parser lowercases header names in place and the
// zero-copy contract depends on exactly which bytes that touches.
//
// The single intended difference is Content-Length: the reference applies the
// same 1*DIGIT rule as the new parser (see parseContentLength), so "+5" and
// "-0" are refused by both. TestParserContentLengthStrict pins that change
// against the original behaviour explicitly.

type parseOutcome struct {
	consumed int
	err      string
	tooLarge bool // errors.Is(err, ErrHeadersTooLarge)
	bodyBig  bool // errors.Is(err, ErrBodyTooLarge)

	method   Method
	path     string
	query    url.Values
	header   map[string]string
	body     []byte
	hasBody  bool
	bodyOff  int
	connMode uint8
	owned    []byte
	hasOwned bool

	buf []byte
}

type fillFunc func(req *HTTPRequest, data []byte, ownHeaders bool, maxBody int64) (int, error)

func runParser(fn fillFunc, data []byte, own bool, maxBody int64) parseOutcome {
	buf := append(make([]byte, 0, len(data)+16), data...)
	req := &HTTPRequest{Header: make(map[string]string, 8)}
	n, err := fn(req, buf, own, maxBody)
	out := parseOutcome{consumed: n, buf: buf}
	if err != nil {
		out.err = err.Error()
		out.tooLarge = errors.Is(err, ErrHeadersTooLarge)
		out.bodyBig = errors.Is(err, ErrBodyTooLarge)
		return out
	}
	out.method = req.Method
	out.path = req.Path
	out.query = req.Query
	out.header = req.Header
	out.connMode = req.connMode
	if req.Body != nil {
		out.hasBody = true
		out.body = append([]byte(nil), req.Body...)
		out.bodyOff = cap(buf) - cap(req.Body)
	}
	if req.owned != nil {
		out.hasOwned = true
		out.owned = append([]byte(nil), req.owned...)
	}
	return out
}

func diffOutcomes(a, b parseOutcome) string {
	switch {
	case a.err != b.err:
		return fmt.Sprintf("error: legacy=%q new=%q", a.err, b.err)
	case a.tooLarge != b.tooLarge || a.bodyBig != b.bodyBig:
		return "sentinel error identity differs"
	case a.consumed != b.consumed:
		return fmt.Sprintf("consumed: legacy=%d new=%d", a.consumed, b.consumed)
	}
	if a.err != "" {
		return "" // on error nothing else is observable
	}
	switch {
	case a.method != b.method:
		return fmt.Sprintf("method: legacy=%q new=%q", a.method, b.method)
	case a.path != b.path:
		return fmt.Sprintf("path: legacy=%q new=%q", a.path, b.path)
	case !reflect.DeepEqual(a.query, b.query):
		return fmt.Sprintf("query: legacy=%v new=%v", a.query, b.query)
	case !reflect.DeepEqual(a.header, b.header):
		return fmt.Sprintf("header: legacy=%q new=%q", a.header, b.header)
	case a.connMode != b.connMode:
		return fmt.Sprintf("connMode: legacy=%d new=%d", a.connMode, b.connMode)
	case a.hasBody != b.hasBody || !bytes.Equal(a.body, b.body) || a.bodyOff != b.bodyOff:
		return fmt.Sprintf("body: legacy=%q@%d new=%q@%d", a.body, a.bodyOff, b.body, b.bodyOff)
	case a.hasOwned != b.hasOwned || !bytes.Equal(a.owned, b.owned):
		return fmt.Sprintf("owned block: legacy=%q new=%q", a.owned, b.owned)
	case a.consumed == 0 && a.method == "" && b.method != "":
		return "an incomplete head left the request filled in"
	case a.consumed == 0 && a.method == "":
		// The head is incomplete. The legacy parser touched nothing; the
		// in-place parse may already have lowercased the names of the header
		// lines that had arrived (see headNotComplete), and nothing else.
		if !bytes.Equal(asciiLower(a.buf), asciiLower(b.buf)) {
			return fmt.Sprintf("buffer after incomplete parse differs beyond case: legacy=%q new=%q", a.buf, b.buf)
		}
	case !bytes.Equal(a.buf, b.buf):
		return fmt.Sprintf("buffer after parse: legacy=%q new=%q", a.buf, b.buf)
	}
	return ""
}

// asciiLower lowercases A-Z only. (bytes.ToLower would apply Unicode rules.)
func asciiLower(b []byte) []byte {
	out := append([]byte(nil), b...)
	for i, c := range out {
		if c >= 'A' && c <= 'Z' {
			out[i] = c + 32
		}
	}
	return out
}

func compareParsers(data []byte) string {
	for _, own := range []bool{true, false} {
		for _, maxBody := range []int64{0, 16, 1 << 20} {
			a := runParser(legacyFillHTTPRequest, data, own, maxBody)
			b := runParser(fillHTTPRequest, data, own, maxBody)
			if d := diffOutcomes(a, b); d != "" {
				return fmt.Sprintf("own=%v maxBody=%d: %s", own, maxBody, d)
			}
		}
	}
	return ""
}

// parserDiffCorpus is every request shape the rewrite has to keep meaning the
// same thing: the plain ones, and the ones that exercise a decision the
// original code made.
func parserDiffCorpus() []string {
	var c []string
	add := func(s ...string) { c = append(c, s...) }

	add(
		"GET / HTTP/1.1\r\nHost: a\r\n\r\n",
		"GET /a/b/c?x=1&y=2 HTTP/1.1\r\nHost: a\r\nUser-Agent: t\r\n\r\n",
		"GET / HTTP/1.0\r\nUser-Agent: t\r\n\r\n",
		"GET / HTTP/1.0\r\n\r\n",
		"GET / HTTP/1.1\r\n\r\n",
		"POST /u HTTP/1.1\r\nHost: a\r\nContent-Length: 5\r\n\r\nhello",
		"POST /u HTTP/1.1\r\nHost: a\r\nContent-Length: 5\r\n\r\nhel",
		"POST /u HTTP/1.1\r\nHost: a\r\nContent-Length: 5\r\n\r\nhello world",
		"GET /1 HTTP/1.1\r\nHost: a\r\n\r\nGET /2 HTTP/1.1\r\nHost: a\r\n\r\n",
		string(rawGET), string(rawGETBrowser),
	)

	// Content-Length values.
	for _, v := range []string{"0", "00", "5", "+5", "-0", "-5", "5 ", " 5", "5,5", "0x5", "", " ", "٣", "5\t", "05",
		"9223372036854775807", "9223372036854775808", "99999999999999999999", "0000000000000000000000005",
		"1_0", "5.0", "５"} {
		add("POST /u HTTP/1.1\r\nHost: a\r\nContent-Length: " + v + "\r\n\r\nhello")
	}
	add(
		"POST /u HTTP/1.1\r\nHost: a\r\nContent-Length: 5\r\nContent-Length: 5\r\n\r\nhello",
		"POST /u HTTP/1.1\r\nHost: a\r\nContent-Length: 5\r\ncontent-length: 5\r\n\r\nhello",
		"POST /u HTTP/1.1\r\nHost: a\r\nHost: b\r\n\r\n",
		"POST /u HTTP/1.1\r\nHost: a\r\nhost: b\r\n\r\n",
		"POST /u HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\n\r\n",
		"POST /u HTTP/1.1\r\nTransfer-Encoding: chunked\r\n\r\n",
		"POST /u HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\nContent-Length: 5\r\n\r\nhello",
		"POST /u HTTP/1.1\r\nHost: a\r\nContent-Length: 5\r\nTransfer-Encoding: chunked\r\n\r\nhello",
		"POST /u HTTP/1.0\r\nContent-Length: 3\r\n\r\nabc",
	)

	// Connection.
	for _, v := range []string{"keep-alive", "Keep-Alive", "KEEP-ALIVE", "close", "CLOSE", "Close", "keep-alive, close",
		"close, keep-alive", " close ", "upgrade", "Keep-Alive\t", "clo\u017fe", "\u212aeep-alive", "keep-aliv", "keep-alivee",
		"closed", ",", "close,", ",close", "keep-alive,keep-alive", ""} {
		add("GET / HTTP/1.1\r\nHost: a\r\nConnection: " + v + "\r\n\r\n")
		add("GET / HTTP/1.0\r\nConnection: " + v + "\r\n\r\n")
	}
	add(
		"GET / HTTP/1.1\r\nHost: a\r\nConnection: keep-alive\r\nConnection: close\r\n\r\n",
		"GET / HTTP/1.0\r\nConnection: keep-alive\r\nConnection: Keep-Alive\r\n\r\n",
		"GET / HTTP/1.0\r\nconnection: close\r\nCONNECTION: keep-alive\r\n\r\n",
	)

	// Whitespace and control bytes around values.
	for _, v := range []string{"v", " v", "v ", "  v  ", "\tv\t", " \t v \t ", "", " ", "\t", "   \t  ",
		"a\x0b", "\x0bv", "v\x0c", "\x0cv\x0c", "a\rb", "a\nb", "a\x00b", "a\x7fb", "\x7f", "v\x7f", "\x7fv",
		"v\r", "\rv", "v\n", "\nv", "a\tb", "a \t b", "a\x01", "\x01a", "a\x1f", "a\x08b",
		"é", "\u00a0v", "v\u00a0", "v\u0085", "\u0085v", "\u2003v\u2003", "v\xa0", "\xffv", "é ", " é", "\xc2", "v\xc2",
		"\xe1\x9a\x80v", "v\xe3\x80\x80", "日本語", "a\u2028", "\u2029a"} {
		add("GET / HTTP/1.1\r\nHost: a\r\nX: " + v + "\r\n\r\n")
		add("GET / HTTP/1.1\r\nHost: a\r\nX:" + v + "\r\nY: 1\r\n\r\n")
	}
	add(
		"GET / HTTP/1.1\r\nHost: a\r\nX: v\r\r\n\r\n",
		"GET / HTTP/1.1\r\nHost: a\r\nX: v\r\r\nY: 1\r\n\r\n",
		"GET / HTTP/1.1\r\nHost: a\r\nX: v\r\n\r\r\n\r\n",
		"GET / HTTP/1.1\r\nHost: a\r\nX: v\r",
		"GET / HTTP/1.1\r\nHost: a\r\nX: v\n\r\n\r\n",
	)

	// Header names and structure.
	for _, n := range []string{"Content-Type", "content-type", "CONTENT-TYPE", "X-A_b.c~d!e", "X Y", "X\t", "", "X-é",
		"Host ", " Host", "\tHost", "X:Y", "X;Y", "X@", "X\x00", "X\x7f", "x-y-z-0-1-2-3-4-5-6-7-8-9-aaaaaaaaaaaaaaaaaaaaaa"} {
		add("GET / HTTP/1.1\r\nHost: a\r\n" + n + ": v\r\n\r\n")
		add("GET / HTTP/1.1\r\nHost: a\r\n" + n + ":\r\n\r\n")
	}
	add(
		"GET / HTTP/1.1\r\nHost: a\r\nNoColonHere\r\n\r\n",
		"GET / HTTP/1.1\r\nHost: a\r\nX: a\r\n b\r\n\r\n",
		"GET / HTTP/1.1\r\nHost: a\r\nX: a\r\n\tb\r\n\r\n",
		"GET / HTTP/1.1\r\n Host: a\r\n\r\n",
		"GET / HTTP/1.1\r\nHost: a\r\n: v\r\n\r\n",
		"GET / HTTP/1.1\r\nHost: a\r\nX:\r\nY:\r\nZ:\r\n\r\n",
		"GET / HTTP/1.1\r\nHost:a\r\nX:v:w\r\n\r\n",
		"GET / HTTP/1.1\r\nHost: a\r\nX: 1\r\nx: 2\r\nX: 3\r\n\r\n",
		"GET / HTTP/1.1\r\nHost: a\r\nCookie: a=1\r\nCookie: b=2\r\nX-Forwarded-For: p\r\nx-forwarded-for: q\r\n\r\n",
		"GET / HTTP/1.1\r\nHost: a\r\nAccept: a\r\naccept: b\r\nACCEPT: c\r\n\r\n",
	)

	// Request line.
	for _, rl := range []string{"get / HTTP/1.1", "G@T / HTTP/1.1", "GET  / HTTP/1.1", "GET / HTTP/1.1 ", " GET / HTTP/1.1",
		"GET /\t HTTP/1.1", "GET / HTTP/2.0", "GET / HTTP/1.11", "GET / http/1.1", "GET /a b HTTP/1.1", "GET /\x01 HTTP/1.1",
		"GET /\x7f HTTP/1.1", "GET /é HTTP/1.1", "GET /\xff HTTP/1.1", "GET http://example.com/x?y=1 HTTP/1.1",
		"OPTIONS * HTTP/1.1", "CONNECT a:443 HTTP/1.1", "BREW /pot HTTP/1.1", "GET /?a=%zz HTTP/1.1", "GET /?a;b HTTP/1.1",
		"GET /?%41=%42&c=d+e HTTP/1.1", "GET /# HTTP/1.1", "GET /? HTTP/1.1", "GET /?& HTTP/1.1", "GET /?=x HTTP/1.1",
		"GET /?a=1&a=2&b HTTP/1.1", "GET HTTP/1.1", "GET / ", "GET /", "GET", "", " ", "  ", "GET / HTTP/1.1\r",
		"POST /a HTTP/1.0", "DELETE /x?y HTTP/1.1", "PUT / HTTP/1.1", "PATCH / HTTP/1.1", "OPTIONS / HTTP/1.1", "HEAD /x HTTP/1.1", "HEAD", "HEA / HTTP/1.1", "HEADS / HTTP/1.1", "head / HTTP/1.1",
		"G / HTTP/1.1", "GETGETG / HTTP/1.1", "GETGETGE / HTTP/1.1", "GET /aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa HTTP/1.1",
		"GET /abcdefg HTTP/1.1", "GET /abcdefgh HTTP/1.1", "GET /abcdefghi HTTP/1.1", "GET /abcdefg\x01 HTTP/1.1",
		"GET /abcdefgh\x01 HTTP/1.1", "\x00 / HTTP/1.1", "GET\t/ HTTP/1.1", "GET / HTTP/1.1\x00"} {
		add(rl+"\r\nHost: a\r\n\r\n", rl+"\r\nUser-Agent: z\r\n\r\n")
	}
	add(
		"GET / HTTP/1.1\r\r\nHost: a\r\n\r\n",
		"GET / HTTP/1.1\nHost: a\n\n",
		"GET / HTTP/1.1\r\nHost: a\r\n\r\n\r\n",
		"\r\nGET / HTTP/1.1\r\nHost: a\r\n\r\n",
		"GET / HTTP/1.1\r\nHost: a\r\r\n\r\n",
		"GET / HTTP/1.1\r\nHost: a\r\n\r\r\n\r\n",
		"\r\n\r\n",
		"\r\n",
		"",
	)

	// SWAR block boundaries: value lengths 0..40, and one bad byte at each
	// position of a 24- and a 41-byte value, for every kind of bad byte.
	for n := 0; n <= 40; n++ {
		add("GET / HTTP/1.1\r\nHost: a\r\nX: " + strings.Repeat("a", n) + "\r\nY: " + strings.Repeat("b", n) + "\r\n\r\n")
		add("GET /" + strings.Repeat("p", n) + " HTTP/1.1\r\nHost: a\r\n\r\n")
	}
	for _, n := range []int{24, 41} {
		for _, bad := range []string{"\x00", "\x01", "\x08", "\t", "\x0b", "\x0c", "\r", "\n", "\x1f", "\x7f", " ", "\x80", "\xa0", "\xc2\xa0"} {
			for pos := 0; pos < n; pos++ {
				v := []byte(strings.Repeat("v", n))
				v = append(v[:pos], append([]byte(bad), v[pos:]...)...)
				add("GET / HTTP/1.1\r\nHost: a\r\nX: " + string(v) + "\r\nY: 1\r\n\r\n")
				if pos < 12 {
					p := []byte(strings.Repeat("p", n))
					p = append(p[:pos], append([]byte(bad), p[pos:]...)...)
					add("GET /" + string(p) + " HTTP/1.1\r\nHost: a\r\n\r\n")
				}
			}
		}
	}

	// Queries: parameter counts around the fast path's limit, repeated keys,
	// empty segments, and the bytes that send a query to net/url.
	for _, n := range []int{0, 1, 2, 5, 8, 15, 16, 17, 18, 40} {
		var q []string
		for i := 0; i < n; i++ {
			q = append(q, "k"+strconv.Itoa(i)+"=v"+strconv.Itoa(i))
		}
		add("GET /?" + strings.Join(q, "&") + " HTTP/1.1\r\nHost: a\r\n\r\n")
		add("GET /p?" + strings.Join(append(q, "k0=again", "k1"), "&") + " HTTP/1.1\r\nHost: a\r\n\r\n")
	}
	for _, q := range []string{"a=1&a=2&a=3", "a&b&c", "a=&b=", "=", "==", "=x", "a==b", "a=b=c", "&", "&&", "a=1&&b=2", "&a=1&", "a=1&",
		"a", "a=%41", "a=%4", "a=%zz", "a=b+c", "a=b;c", "a;b", "%41=1", "k=" + strings.Repeat("v", 600), "a=é&b=ü", "a=\x01", "a=b#c", "a=b?c",
		"x=1&x=2&y=3&x=4", "a=1&b=2&c=3&d=4&e=5&f=6&g=7&h=8&i=9", strings.Repeat("a=1&", 16) + "z=2", strings.Repeat("a&", 15) + "a", strings.Repeat("a&", 16) + "a"} {
		add("GET /x?" + q + " HTTP/1.1\r\nHost: a\r\n\r\n")
	}

	// Many headers, long values.
	var many strings.Builder
	many.WriteString("GET /m HTTP/1.1\r\nHost: h\r\n")
	for i := 0; i < 100; i++ {
		many.WriteString("X-Hdr-" + strconv.Itoa(i) + ": value-" + strconv.Itoa(i) + "\r\n")
	}
	many.WriteString("\r\n")
	add(many.String())
	add("GET / HTTP/1.1\r\nHost: a\r\nCookie: " + strings.Repeat("k=v; ", 300) + "\r\n\r\n")
	add("GET / HTTP/1.1\r\nHost: a\r\nCookie: " + strings.Repeat("k=v; ", 300) + "\x01\r\n\r\n")

	return c
}

func TestParserDifferentialCorpus(t *testing.T) {
	corpus := parserDiffCorpus()
	failures := 0
	check := func(data []byte, what string) {
		if d := compareParsers(data); d != "" {
			failures++
			if failures <= 25 {
				t.Errorf("%s %q\n   %s", what, data, d)
			}
		}
	}
	for _, s := range corpus {
		check([]byte(s), "input")
	}
	// Every prefix of every hand-written case: a request split across reads
	// reaches the parser as each of its prefixes in turn, and the answer for
	// each — incomplete, error, or request — must match.
	for i, s := range corpus {
		if len(s) > 700 || i > 60 && strings.HasPrefix(s, "GET / HTTP/1.1\r\nHost: a\r\nX: ") && len(s) > 80 {
			continue
		}
		for n := 0; n < len(s); n++ {
			check([]byte(s[:n]), fmt.Sprintf("prefix %d/%d of %q:", n, len(s), s[:min(len(s), 40)]))
		}
	}
	if failures > 0 {
		t.Fatalf("%d mismatches against the legacy parser", failures)
	}
}

// TestParserDifferentialMutations perturbs every corpus entry at random:
// flipping, inserting, deleting and duplicating bytes, biased toward the bytes
// the parser makes decisions on.
func TestParserDifferentialMutations(t *testing.T) {
	rng := rand.New(rand.NewSource(20261115))
	interesting := []byte("\r\n :\t?%+;,\x00\x01\x0b\x0c\x7f\x80\xa0\xc2\xe2\xff0123456789AaHhTtPp/.-_")
	corpus := parserDiffCorpus()
	failures := 0
	rounds := 40
	if testing.Short() {
		rounds = 8
	}
	for _, s := range corpus {
		if len(s) > 1500 {
			continue
		}
		for r := 0; r < rounds; r++ {
			b := []byte(s)
			for k := rng.Intn(4) + 1; k > 0 && len(b) > 0; k-- {
				pos := rng.Intn(len(b))
				switch rng.Intn(5) {
				case 0:
					b[pos] = interesting[rng.Intn(len(interesting))]
				case 1:
					b = append(b[:pos], b[pos+1:]...)
				case 2:
					b = append(b[:pos], append([]byte{interesting[rng.Intn(len(interesting))]}, b[pos:]...)...)
				case 3:
					end := min(len(b), pos+1+rng.Intn(8))
					b = append(b[:end], append(append([]byte(nil), b[pos:end]...), b[end:]...)...)
				case 4:
					b = b[:pos]
				}
			}
			if d := compareParsers(b); d != "" {
				failures++
				if failures <= 25 {
					t.Errorf("mutation of %q\n   input %q\n   %s", s[:min(len(s), 40)], b, d)
				}
			}
		}
	}
	if failures > 0 {
		t.Fatalf("%d mismatches against the legacy parser", failures)
	}
}

func FuzzParserDifferential(f *testing.F) {
	for _, s := range parserDiffCorpus() {
		if len(s) <= 2048 {
			f.Add([]byte(s))
		}
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > maxRequestHeaderBytes+64 {
			return
		}
		if d := compareParsers(data); d != "" {
			t.Fatalf("input %q\n   %s", data, d)
		}
	})
}

// TestParserSizeLimitBoundary pins the header-size limit at its edges, where
// the early-exit logic of the parser is easiest to get wrong.
func TestParserSizeLimitBoundary(t *testing.T) {
	mk := func(headLen int) []byte {
		// A head of exactly headLen bytes including the final CRLFCRLF.
		const pre = "GET / HTTP/1.1\r\nHost: a\r\nX: "
		pad := headLen - len(pre) - len("\r\n\r\n")
		return []byte(pre + strings.Repeat("v", pad) + "\r\n\r\n")
	}
	for _, headLen := range []int{maxRequestHeaderBytes - 2, maxRequestHeaderBytes - 1, maxRequestHeaderBytes, maxRequestHeaderBytes + 1, maxRequestHeaderBytes + 2} {
		for _, extra := range []int{0, 1, 5, 100} {
			data := append(mk(headLen), bytes.Repeat([]byte("z"), extra)...)
			if d := compareParsers(data); d != "" {
				t.Errorf("headLen=%d extra=%d: %s", headLen, extra, d)
			}
			// the same head without its terminator
			if d := compareParsers(data[:headLen-4]); d != "" {
				t.Errorf("unterminated headLen=%d: %s", headLen, d)
			}
		}
	}
}

// TestParserContentLengthStrict documents the one deliberate behaviour change
// of the rewrite: Content-Length is 1*DIGIT. strconv.ParseInt, which the old
// code used, also took a leading sign.
func TestParserContentLengthStrict(t *testing.T) {
	ok := map[string]int{"0": 0, "5": 5, "05": 5, "0000000000000000000000005": 5, "9": 9}
	for v, want := range ok {
		raw := "POST /u HTTP/1.1\r\nHost: a\r\nContent-Length: " + v + "\r\n\r\n" + strings.Repeat("x", want)
		req, n, err := ParseHTTPRequest([]byte(raw))
		if err != nil || req == nil || len(req.Body) != want || n != len(raw) {
			t.Errorf("Content-Length %q: req=%v n=%d err=%v", v, req, n, err)
		}
	}
	for _, v := range []string{"+5", "-0", "-5", "5 5", "5,5", "0x5", "", "٣", "5.0", "1_0", "9223372036854775808", "99999999999999999999"} {
		raw := "POST /u HTTP/1.1\r\nHost: a\r\nContent-Length: " + v + "\r\n\r\nhello"
		if _, _, err := ParseHTTPRequest([]byte(raw)); err == nil || err.Error() != "invalid content-length" {
			t.Errorf("Content-Length %q: err=%v, want invalid content-length", v, err)
		}
	}
	// And the old parser did take the signs, which is why they are listed here.
	if v, err := strconv.ParseInt("+5", 10, 64); err != nil || v != 5 {
		t.Fatalf("premise changed: ParseInt(+5) = %d, %v", v, err)
	}
}

// ── Zero-copy guards ─────────────────────────────────────────────────────────

func within(buf []byte, s string) bool {
	if len(s) == 0 {
		return true
	}
	p := uintptr(unsafe.Pointer(unsafe.StringData(s)))
	lo := uintptr(unsafe.Pointer(unsafe.SliceData(buf)))
	return p >= lo && p+uintptr(len(s)) <= lo+uintptr(len(buf))
}

// TestParserZeroCopyViews: with ownHeaders == false every string the parser
// hands out — path, header names, header values — and the body are views into
// the caller's buffer, and nothing was copied.
func TestParserZeroCopyViews(t *testing.T) {
	for _, c := range parserBenchCases {
		if c.incomplete {
			continue
		}
		buf := rawCopy(c.raw)
		req, _, err := parsePooledRequest(buf, false)
		if err != nil || req == nil {
			t.Fatalf("%s: parse failed: %v", c.name, err)
		}
		if req.owned != nil {
			t.Errorf("%s: zero-copy parse made an owned copy", c.name)
		}
		if !within(buf, req.Path) {
			t.Errorf("%s: path is not a view into the input buffer", c.name)
		}
		for k, v := range req.Header {
			if !within(buf, k) || !within(buf, v) {
				t.Errorf("%s: header %q is not a view into the input buffer", c.name, k)
			}
		}
		if len(req.Body) > 0 {
			bp := uintptr(unsafe.Pointer(unsafe.SliceData(req.Body)))
			lo := uintptr(unsafe.Pointer(unsafe.SliceData(buf)))
			if bp < lo || bp+uintptr(len(req.Body)) > lo+uintptr(len(buf)) {
				t.Errorf("%s: body is not a view into the input buffer", c.name)
			}
		}
		releaseRequest(req)
	}
}

// TestParserOwnedIsIndependent: with ownHeaders == true the strings view the
// request's private copy, so scribbling over the input afterwards changes
// nothing the request reports.
func TestParserOwnedIsIndependent(t *testing.T) {
	for _, c := range parserBenchCases {
		if c.incomplete {
			continue
		}
		buf := rawCopy(c.raw)
		req, _, err := parsePooledRequest(buf, true)
		if err != nil || req == nil {
			t.Fatalf("%s: parse failed: %v", c.name, err)
		}
		want := map[string]string{}
		for k, v := range req.Header {
			want[strings.Clone(k)] = strings.Clone(v)
		}
		path := strings.Clone(req.Path)
		for i := range buf {
			buf[i] = 0xAA
		}
		if req.Path != path {
			t.Errorf("%s: path changed after the input was overwritten", c.name)
		}
		for k, v := range want {
			if got, ok := req.Header[k]; !ok || got != v {
				t.Errorf("%s: header %q changed after the input was overwritten", c.name, k)
			}
		}
		releaseRequest(req)
	}
}

// TestParserZeroCopyAllocs: the zero-copy parse of a request without a query
// allocates nothing, and the owned parse allocates only the header block.
func TestParserZeroCopyAllocs(t *testing.T) {
	if raceEnabled {
		t.Skip("sync.Pool drops items under the race detector, so allocation counts of pooled paths are not meaningful")
	}
	for _, name := range []string{"tiny", "get4", "browser", "many30", "postSmall", "post1M"} {
		var c parserBenchCase
		for _, x := range parserBenchCases {
			if x.name == name {
				c = x
			}
		}
		buf := rawCopy(c.raw)
		head := parserBenchHeadLen(c.raw)
		warm := func(own bool) {
			for i := 0; i < 4; i++ {
				copy(buf[:head], c.raw[:head])
				if r, _, _ := parsePooledRequest(buf, own); r != nil {
					releaseRequest(r)
				}
			}
		}
		warm(false)
		if n := testing.AllocsPerRun(200, func() {
			copy(buf[:head], c.raw[:head])
			r, _, _ := parsePooledRequest(buf, false)
			releaseRequest(r)
		}); n != 0 {
			t.Errorf("%s: zero-copy parse allocates %v times per request, want 0", name, n)
		}
		warm(true)
		if n := testing.AllocsPerRun(200, func() {
			r, _, _ := parsePooledRequest(buf, true)
			releaseRequest(r)
		}); n > 1 {
			t.Errorf("%s: owned parse allocates %v times per request, want at most 1 (the header block)", name, n)
		}
	}
}
