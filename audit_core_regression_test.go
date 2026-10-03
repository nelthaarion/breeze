package breeze

// Regression tests for the audit's performance / correctness findings in the core:
//
//   - 206/307/308/416 and others had no reason phrase ("Status");
//   - the response slow path lower-cased every header key (an allocation each);
//   - repeated request headers silently kept only the last value;
//   - static files with %-escapes 404'd, an encoded ".." must stay contained,
//     and there was no conditional GET or size bound.

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestStatusLinesHaveRealReasonPhrases(t *testing.T) {
	want := map[int]string{
		206: "Partial Content", 307: "Temporary Redirect", 308: "Permanent Redirect",
		410: "Gone", 413: "Content Too Large", 415: "Unsupported Media Type",
		416: "Range Not Satisfiable", 501: "Not Implemented", 504: "Gateway Timeout",
	}
	for code, text := range want {
		line := string(statusLine(code))
		if !strings.Contains(line, " "+text+"\r\n") {
			t.Errorf("status %d line = %q, want reason %q", code, line, text)
		}
	}
	if strings.Contains(string(statusLine(299)), "Status") == false {
		t.Errorf("unknown codes keep the generic fallback")
	}
}

func TestResponseSlowPathHeaderHandling(t *testing.T) {
	r := &HTTPResponse{Status: 200, Body: []byte("hi"), Headers: map[string]string{
		"X-Custom":          "v",
		"Content-Length":    "9999", // application framing is ignored
		"transfer-encoding": "chunked",
		"Connection":        "keep-alive",
		"Set-Cookie":        "a=1\r\nb=2",
		"Bad Header":        "x", // invalid token: dropped
		"X-Inject":          "ok\r\nEvil: 1",
	}}
	out := string(r.AppendTo(nil))
	for _, must := range []string{"X-Custom: v\r\n", "Set-Cookie: a=1\r\n", "Set-Cookie: b=2\r\n", "Content-Length: 2\r\n"} {
		if !strings.Contains(out, must) {
			t.Errorf("response lacks %q:\n%s", must, out)
		}
	}
	for _, mustNot := range []string{"9999", "chunked", "keep-alive", "Bad Header", "Evil: 1"} {
		if strings.Contains(out, mustNot) {
			t.Errorf("response must not contain %q:\n%s", mustNot, out)
		}
	}
}

func TestResponseSlowPathDoesNotAllocatePerHeader(t *testing.T) {
	r := &HTTPResponse{Status: 200, Body: []byte("hi"), Headers: map[string]string{
		"Content-Type": "text/plain", "X-A": "1", "X-B": "2", "X-C": "3", "X-D": "4",
	}}
	buf := make([]byte, 0, 1024)
	// The map iteration and nothing else: five headers used to cost five
	// strings.ToLower allocations on top of it.
	if n := testing.AllocsPerRun(500, func() { buf = r.AppendTo(buf[:0]) }); n > 1 {
		t.Fatalf("AppendTo allocated %v times for 5 headers, want <= 1", n)
	}
}

func TestRepeatedRequestHeadersAreJoined(t *testing.T) {
	raw := "GET / HTTP/1.1\r\nHost: x\r\n" +
		"X-Forwarded-For: 203.0.113.1\r\n" +
		"Accept: text/html\r\n" +
		"Cookie: a=1\r\n" +
		"x-forwarded-for: 10.0.0.1, 10.0.0.2\r\n" +
		"Cookie: b=2\r\n" +
		"Authorization: Bearer one\r\n" +
		"Authorization: Bearer two\r\n\r\n"
	req, _, err := parsePooledRequest([]byte(raw), true)
	if err != nil || req == nil {
		t.Fatalf("parse: %v", err)
	}
	defer releaseRequest(req)
	check := map[string]string{
		"x-forwarded-for": "203.0.113.1, 10.0.0.1, 10.0.0.2",
		"cookie":          "a=1; b=2",
		"authorization":   "Bearer one, Bearer two",
		"accept":          "text/html",
	}
	for k, want := range check {
		if got := req.Header[k]; got != want {
			t.Errorf("header %q = %q, want %q", k, got, want)
		}
	}
}

func TestSingleHeadersTakeTheFastPathUnchanged(t *testing.T) {
	raw := "GET / HTTP/1.1\r\nHost: x\r\nAccept: a\r\nUser-Agent: u\r\n\r\n"
	req, _, err := parsePooledRequest([]byte(raw), true)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseRequest(req)
	if req.Header["accept"] != "a" || req.Header["user-agent"] != "u" || len(req.Header) != 3 {
		t.Fatalf("unexpected headers: %v", req.Header)
	}
}

func staticApp(t *testing.T) (*Breeze, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "my file.txt"), []byte("spaced"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plain.txt"), []byte("plain"), 0o644); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(filepath.Dir(dir), "secret-"+filepath.Base(dir)+".txt")
	if err := os.WriteFile(secret, []byte("TOP-SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(secret) })
	router := NewRouter()
	app := New(router, NewEventLoopWorkerPool(runtime.NumCPU()))
	router.ServeStatic("/static", dir)
	return app, filepath.Base(secret)
}

func fetch(t *testing.T, port int, path string, extra string) (string, string) {
	t.Helper()
	c := dial(t, port)
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = c.Write([]byte("GET " + path + " HTTP/1.1\r\nHost: x\r\n" + extra + "\r\n"))
	return readResponse(t, bufio.NewReader(c))
}

func TestStaticDecodesPercentEscapes(t *testing.T) {
	app, _ := staticApp(t)
	port := startAuditServer(t, app)
	head, body := fetch(t, port, "/static/my%20file.txt", "")
	if !strings.HasPrefix(head, "HTTP/1.1 200") || body != "spaced" {
		t.Fatalf("an escaped space must resolve to the file: %q / %q", strings.SplitN(head, "\r\n", 2)[0], body)
	}
}

func TestStaticStaysContainedAgainstEncodedTraversal(t *testing.T) {
	app, secretName := staticApp(t)
	port := startAuditServer(t, app)
	for _, p := range []string{
		"/static/../" + secretName,
		"/static/%2e%2e/" + secretName,
		"/static/%2E%2E%2F" + secretName,
		"/static/..%2f" + secretName,
		"/static/%252e%252e/" + secretName,
		"/static/plain.txt%00.png",
		"/static/%zz",
	} {
		head, body := fetch(t, port, p, "")
		if strings.Contains(body, "TOP-SECRET") {
			t.Fatalf("%s escaped the static root and read a file outside it", p)
		}
		if strings.HasPrefix(head, "HTTP/1.1 200") {
			t.Errorf("%s answered 200 (body %q); a traversal or malformed escape must not succeed", p, body)
		}
	}
}

func TestStaticConditionalGet(t *testing.T) {
	app, _ := staticApp(t)
	port := startAuditServer(t, app)

	head, body := fetch(t, port, "/static/plain.txt", "")
	if body != "plain" {
		t.Fatalf("body %q", body)
	}
	var etag string
	for _, ln := range strings.Split(head, "\r\n") {
		if k, v, ok := strings.Cut(ln, ":"); ok && strings.EqualFold(k, "etag") {
			etag = strings.TrimSpace(v)
		}
	}
	if etag == "" {
		t.Fatalf("static responses must carry an ETag:\n%s", head)
	}

	head, body = fetch(t, port, "/static/plain.txt", "If-None-Match: "+etag+"\r\n")
	if !strings.HasPrefix(head, "HTTP/1.1 304") || body != "" {
		t.Fatalf("a matching validator must produce a bodyless 304, got %q body=%q", strings.SplitN(head, "\r\n", 2)[0], body)
	}

	head, body = fetch(t, port, "/static/plain.txt", `If-None-Match: W/"stale"`+"\r\n")
	if !strings.HasPrefix(head, "HTTP/1.1 200") || body != "plain" {
		t.Fatalf("a stale validator must get the full file, got %q", strings.SplitN(head, "\r\n", 2)[0])
	}
}

func TestEtagMatching(t *testing.T) {
	const e = `W/"a-b"`
	for hdr, want := range map[string]bool{
		`W/"a-b"`: true, `"a-b"`: true, `*`: true, `"x", W/"a-b"`: true, `"x"`: false, ``: false,
	} {
		if hdr == "" {
			continue
		}
		if got := etagMatches(hdr, e); got != want {
			t.Errorf("etagMatches(%q) = %v, want %v", hdr, got, want)
		}
	}
}
