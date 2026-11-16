package breeze

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
)

// Parser benchmark corpus.
//
// Every case runs through the server's real entry point, parsePooledRequest,
// in both modes: owned (the default: the header block is copied) and zero-copy
// (SetZeroCopyHeaders: the header block is parsed in place in the caller's
// buffer). The release call is inside the loop on purpose: it is part of the
// per-request cost the event loop pays, and it is what keeps the pooled header
// map warm.
//
// Run:
//
//	go test -run '^$' -bench 'BenchmarkParser' -benchmem -count 8 .

type parserBenchCase struct {
	name       string
	raw        []byte
	incomplete bool // parse is expected to answer "need more bytes"
}

func parserBenchBody(n int) []byte {
	return bytes.Repeat([]byte("0123456789abcdef"), n/16+1)[:n]
}

func parserBenchPOST(path string, body []byte) []byte {
	var sb strings.Builder
	sb.WriteString("POST " + path + " HTTP/1.1\r\n")
	sb.WriteString("Host: api.example.com\r\n")
	sb.WriteString("User-Agent: Go-http-client/1.1\r\n")
	sb.WriteString("Content-Type: application/json\r\n")
	sb.WriteString("Accept: application/json\r\n")
	sb.WriteString("Content-Length: " + strconv.Itoa(len(body)) + "\r\n")
	sb.WriteString("\r\n")
	return append([]byte(sb.String()), body...)
}

var parserBenchCases = func() []parserBenchCase {
	var cases []parserBenchCase
	add := func(name string, raw []byte, incomplete bool) {
		cases = append(cases, parserBenchCase{name: name, raw: raw, incomplete: incomplete})
	}

	add("tiny", []byte("GET /health HTTP/1.1\r\nHost: localhost\r\n\r\n"), false)
	add("get4", rawGET, false)
	add("browser", rawGETBrowser, false)

	// A request that carries a session cookie, a bearer token and the usual
	// proxy headers: ~17 headers and about 2 KB of head.
	cookie := "session=" + strings.Repeat("abcdef0123456789", 24) + "; theme=dark; lang=en-US; _ga=GA1.2.123456789.1700000000"
	token := strings.Repeat("eyJhbGciOiJIUzI1NiJ9", 14)
	heavy := "GET /dashboard/overview?range=7d HTTP/1.1\r\n" +
		"Host: app.example.com\r\n" +
		"Connection: keep-alive\r\n" +
		"Cache-Control: max-age=0\r\n" +
		"Upgrade-Insecure-Requests: 1\r\n" +
		"User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36\r\n" +
		"Accept: text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8\r\n" +
		"Accept-Encoding: gzip, deflate, br, zstd\r\n" +
		"Accept-Language: en-US,en;q=0.9,fa;q=0.8\r\n" +
		"Sec-Fetch-Dest: document\r\n" +
		"Sec-Fetch-Mode: navigate\r\n" +
		"Sec-Fetch-Site: same-origin\r\n" +
		"Sec-Ch-Ua: \"Chromium\";v=\"126\", \"Not.A/Brand\";v=\"24\"\r\n" +
		"Referer: https://app.example.com/dashboard\r\n" +
		"X-Forwarded-For: 203.0.113.7, 10.0.0.3\r\n" +
		"X-Request-Id: 7f3c2a9e-5b1d-4c8a-9e60-0a1b2c3d4e5f\r\n" +
		"Authorization: Bearer " + token + "\r\n" +
		"Cookie: " + cookie + "\r\n" +
		"\r\n"
	add("heavy", []byte(heavy), false)

	// Thirty short headers: more than a pooled map's first group.
	var many strings.Builder
	many.WriteString("GET /m HTTP/1.1\r\nHost: h\r\n")
	for i := 0; i < 29; i++ {
		many.WriteString("X-Hdr-" + strconv.Itoa(i) + ": v" + strconv.Itoa(i) + "\r\n")
	}
	many.WriteString("\r\n")
	add("many30", []byte(many.String()), false)

	add("queryPlain", []byte("GET /api/items?category=books&page=2&limit=50&sort=price HTTP/1.1\r\nHost: api.example.com\r\nUser-Agent: Go-http-client/1.1\r\nAccept: application/json\r\n\r\n"), false)
	add("queryEscaped", []byte("GET /search?q=hello%20world&tag=a%2Fb&name=J+Doe&x=1 HTTP/1.1\r\nHost: api.example.com\r\nUser-Agent: Go-http-client/1.1\r\nAccept: application/json\r\n\r\n"), false)

	add("postSmall", parserBenchPOST("/api/users", parserBenchBody(64)), false)
	add("post16K", parserBenchPOST("/api/upload", parserBenchBody(16<<10)), false)
	add("post1M", parserBenchPOST("/api/upload", parserBenchBody(1<<20)), false)

	// Head complete, body only half there: the parse answers "wait", and it
	// does so again on every later read until the body is complete.
	full := parserBenchPOST("/api/upload", parserBenchBody(64<<10))
	add("postPartial", full[:len(full)-32<<10], true)

	// Head cut in the middle of a header: no terminator yet.
	add("headSplit", rawGETBrowser[:len(rawGETBrowser)-60], true)

	return cases
}()

// parserBenchHeadLen is the length of the request line plus headers including
// the blank line — the part a zero-copy parse rewrites (lowercased keys).
func parserBenchHeadLen(raw []byte) int {
	if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
		return i + 4
	}
	return len(raw)
}

func benchParseOne(b *testing.B, c parserBenchCase, own bool) {
	buf := rawCopy(c.raw)
	head := parserBenchHeadLen(c.raw)
	b.ReportAllocs()
	b.SetBytes(int64(head))
	b.ResetTimer()
	for b.Loop() {
		if !own {
			// A fresh network read delivers capitalised keys every time. Put
			// them back so a zero-copy parse always has the recasing to do.
			copy(buf[:head], c.raw[:head])
		}
		req, _, err := parsePooledRequest(buf, own)
		if err != nil {
			b.Fatal(err)
		}
		if req == nil {
			if !c.incomplete {
				b.Fatal("parse unexpectedly incomplete")
			}
			continue
		}
		if c.incomplete {
			b.Fatal("parse unexpectedly complete")
		}
		releaseRequest(req)
	}
}

func BenchmarkParser(b *testing.B) {
	for _, c := range parserBenchCases {
		b.Run(c.name+"/owned", func(b *testing.B) { benchParseOne(b, c, true) })
		b.Run(c.name+"/zerocopy", func(b *testing.B) { benchParseOne(b, c, false) })
	}
}

// BenchmarkParserPipelined parses eight back-to-back requests out of one
// buffer the way OnTraffic does, advancing by the consumed count. ns/op is for
// the whole batch.
func BenchmarkParserPipelined(b *testing.B) {
	for _, own := range []bool{true, false} {
		name := "zerocopy"
		if own {
			name = "owned"
		}
		b.Run("get4x8/"+name, func(b *testing.B) {
			raw := bytes.Repeat(rawGET, 8)
			buf := rawCopy(raw)
			b.ReportAllocs()
			b.SetBytes(int64(len(raw)))
			b.ResetTimer()
			for b.Loop() {
				if !own {
					copy(buf, raw)
				}
				rest := buf
				for len(rest) > 0 {
					req, n, err := parsePooledRequest(rest, own)
					if err != nil || req == nil {
						b.Fatalf("parse failed: %v", err)
					}
					releaseRequest(req)
					rest = rest[n:]
				}
			}
		})
	}
}
