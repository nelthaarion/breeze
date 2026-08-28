package client

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── parseHTTPResponse ──────────────────────────────────────────────────────

// The parser is the riskiest code in this package: it replaces net/http's
// battle-tested response reader with ~120 hand-written lines, and it runs on
// the event loop where a wrong answer either hangs a caller or corrupts the
// next request on that connection. These cases are therefore organised around
// the four-result contract rather than around "valid vs invalid" inputs:
// every case pins down which of (incomplete), (complete), (fatal) it is.
func TestParseHTTPResponse(t *testing.T) {
	const max = 1 << 20

	tests := []struct {
		name       string
		raw        string
		wantDone   bool
		wantErr    bool
		wantStatus int
		wantBody   string
		// wantConsumed is checked only when > 0; it matters for pipelining
		// and for leftover bytes staying in the connection buffer.
		wantConsumed int
	}{
		{
			name:       "content length body",
			raw:        "HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhello",
			wantDone:   true,
			wantStatus: 200,
			wantBody:   "hello",
		},
		{
			name:       "zero content length",
			raw:        "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n",
			wantDone:   true,
			wantStatus: 200,
			wantBody:   "",
		},
		{
			// 204 must not wait for a body even though no Content-Length
			// says so. Getting this wrong means every 204 blocks until the
			// call times out.
			name:       "204 has no body",
			raw:        "HTTP/1.1 204 No Content\r\n\r\n",
			wantDone:   true,
			wantStatus: 204,
		},
		{
			name:       "304 has no body",
			raw:        "HTTP/1.1 304 Not Modified\r\nContent-Length: 99\r\n\r\n",
			wantDone:   true,
			wantStatus: 304,
			wantBody:   "",
		},
		{
			name:       "status line without reason phrase",
			raw:        "HTTP/1.1 200\r\nContent-Length: 2\r\n\r\nhi",
			wantDone:   true,
			wantStatus: 200,
			wantBody:   "hi",
		},
		{
			name:       "header values are preserved",
			raw:        "HTTP/1.1 201 Created\r\nContent-Type: application/json\r\nContent-Length: 2\r\n\r\n{}",
			wantDone:   true,
			wantStatus: 201,
			wantBody:   "{}",
		},
		// ── Incomplete: keep reading, nothing is wrong ────────────────────
		{
			name:     "empty buffer",
			raw:      "",
			wantDone: false,
		},
		{
			name:     "headers truncated mid-block",
			raw:      "HTTP/1.1 200 OK\r\nContent-Len",
			wantDone: false,
		},
		{
			name:     "headers complete but body partial",
			raw:      "HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\nshort",
			wantDone: false,
		},
		{
			name:     "chunked truncated mid-chunk",
			raw:      "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhel",
			wantDone: false,
		},
		{
			name:     "chunked missing terminator",
			raw:      "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n",
			wantDone: false,
		},
		{
			// No Content-Length and not chunked: body runs until EOF, which
			// this parser cannot detect. Must report incomplete, not invent
			// an empty body.
			name:     "no length and not chunked",
			raw:      "HTTP/1.1 200 OK\r\nServer: x\r\n\r\nsome body bytes",
			wantDone: false,
		},
		// ── Fatal: stop, this can never become valid ──────────────────────
		{
			name:     "non numeric status",
			raw:      "HTTP/1.1 abc OK\r\nContent-Length: 0\r\n\r\n",
			wantDone: true,
			wantErr:  true,
		},
		{
			name:     "status out of range",
			raw:      "HTTP/1.1 999 Nope\r\nContent-Length: 0\r\n\r\n",
			wantDone: true,
			wantErr:  true,
		},
		{
			name:     "garbage status line",
			raw:      "NOT-HTTP-AT-ALL\r\n\r\n",
			wantDone: true,
			wantErr:  true,
		},
		{
			name:     "bad chunk size",
			raw:      "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\nzz\r\nhello\r\n0\r\n\r\n",
			wantDone: true,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, consumed, done, err := parseHTTPResponse([]byte(tt.raw), max)

			if done != tt.wantDone {
				t.Fatalf("done = %v, want %v (err=%v)", done, tt.wantDone, err)
			}
			if tt.wantErr {
				if err == nil {
					t.Fatal("want error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !done {
				return // incomplete: nothing further to assert
			}
			if resp.Status != tt.wantStatus {
				t.Errorf("status = %d, want %d", resp.Status, tt.wantStatus)
			}
			if string(resp.Body) != tt.wantBody {
				t.Errorf("body = %q, want %q", resp.Body, tt.wantBody)
			}
			if tt.wantConsumed > 0 && consumed != tt.wantConsumed {
				t.Errorf("consumed = %d, want %d", consumed, tt.wantConsumed)
			}
			if consumed > len(tt.raw) {
				t.Errorf("consumed %d exceeds input length %d", consumed, len(tt.raw))
			}
		})
	}
}

func TestParseHTTPResponseChunked(t *testing.T) {
	const max = 1 << 20

	t.Run("multiple chunks reassemble in order", func(t *testing.T) {
		raw := "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n" +
			"5\r\nhello\r\n" +
			"1\r\n \r\n" +
			"5\r\nworld\r\n" +
			"0\r\n\r\n"

		resp, consumed, done, err := parseHTTPResponse([]byte(raw), max)
		if err != nil || !done {
			t.Fatalf("done=%v err=%v", done, err)
		}
		if got := string(resp.Body); got != "hello world" {
			t.Errorf("body = %q, want %q", got, "hello world")
		}
		if consumed != len(raw) {
			t.Errorf("consumed = %d, want %d (whole response)", consumed, len(raw))
		}
	})

	t.Run("chunk extensions are ignored", func(t *testing.T) {
		raw := "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n" +
			"5;name=value\r\nhello\r\n0\r\n\r\n"

		resp, _, done, err := parseHTTPResponse([]byte(raw), max)
		if err != nil || !done {
			t.Fatalf("done=%v err=%v", done, err)
		}
		if got := string(resp.Body); got != "hello" {
			t.Errorf("body = %q, want %q", got, "hello")
		}
	})

	t.Run("trailer fields are skipped", func(t *testing.T) {
		raw := "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n" +
			"5\r\nhello\r\n0\r\nX-Checksum: abc\r\n\r\n"

		resp, consumed, done, err := parseHTTPResponse([]byte(raw), max)
		if err != nil || !done {
			t.Fatalf("done=%v err=%v", done, err)
		}
		if got := string(resp.Body); got != "hello" {
			t.Errorf("body = %q, want %q", got, "hello")
		}
		if consumed != len(raw) {
			t.Errorf("consumed = %d, want %d; trailers must be consumed too, "+
				"or they corrupt the next response on this connection",
				consumed, len(raw))
		}
	})
}

// A response larger than the cap must fail with ErrResponseTooLarge as soon as
// the declared length is known, rather than being buffered and then rejected.
func TestParseHTTPResponseTooLarge(t *testing.T) {
	t.Run("content length", func(t *testing.T) {
		raw := "HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\n"
		_, _, done, err := parseHTTPResponse([]byte(raw), 10)
		if !done {
			t.Fatal("want done=true (fatal), got false")
		}
		if !errors.Is(err, ErrResponseTooLarge) {
			t.Fatalf("err = %v, want ErrResponseTooLarge", err)
		}
	})

	t.Run("chunked", func(t *testing.T) {
		raw := "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n" +
			"14\r\n" + strings.Repeat("x", 20) + "\r\n0\r\n\r\n"
		_, _, done, err := parseHTTPResponse([]byte(raw), 10)
		if !done {
			t.Fatal("want done=true (fatal), got false")
		}
		if !errors.Is(err, ErrResponseTooLarge) {
			t.Fatalf("err = %v, want ErrResponseTooLarge", err)
		}
	})
}

// Feeding the parser one byte at a time is how it actually gets called: gnet
// delivers whatever TCP happened to coalesce. The parser must report
// incomplete for every prefix and produce exactly one correct answer at the
// end, never a partial body.
func TestParseHTTPResponseIncrementalDelivery(t *testing.T) {
	full := "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 11\r\n\r\nhello world"

	for i := 0; i < len(full); i++ {
		_, _, done, err := parseHTTPResponse([]byte(full[:i]), 1<<20)
		if err != nil {
			t.Fatalf("prefix of %d bytes: unexpected error %v", i, err)
		}
		if done {
			t.Fatalf("prefix of %d bytes reported complete; only the full "+
				"%d bytes should", i, len(full))
		}
	}

	resp, consumed, done, err := parseHTTPResponse([]byte(full), 1<<20)
	if err != nil || !done {
		t.Fatalf("full response: done=%v err=%v", done, err)
	}
	if string(resp.Body) != "hello world" {
		t.Errorf("body = %q", resp.Body)
	}
	if consumed != len(full) {
		t.Errorf("consumed = %d, want %d", consumed, len(full))
	}
}

// Two pipelined responses in one buffer: the parser must consume exactly the
// first and leave the second intact. If consumed were wrong, the leftover
// bytes would be misparsed as the next request's response.
func TestParseHTTPResponseLeavesPipelinedBytes(t *testing.T) {
	first := "HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nfirst"
	second := "HTTP/1.1 404 Not Found\r\nContent-Length: 6\r\n\r\nsecond"

	resp, consumed, done, err := parseHTTPResponse([]byte(first+second), 1<<20)
	if err != nil || !done {
		t.Fatalf("done=%v err=%v", done, err)
	}
	if string(resp.Body) != "first" {
		t.Errorf("body = %q, want %q", resp.Body, "first")
	}
	if consumed != len(first) {
		t.Fatalf("consumed = %d, want %d", consumed, len(first))
	}

	// The remainder must parse cleanly as the second response.
	resp2, _, done2, err2 := parseHTTPResponse([]byte(first + second)[consumed:], 1<<20)
	if err2 != nil || !done2 {
		t.Fatalf("second response: done=%v err=%v", done2, err2)
	}
	if resp2.Status != 404 || string(resp2.Body) != "second" {
		t.Errorf("second = %d %q, want 404 %q", resp2.Status, resp2.Body, "second")
	}
}

// ── buildHTTPRequest ───────────────────────────────────────────────────────

func TestBuildHTTPRequest(t *testing.T) {
	t.Run("get includes host and defaults", func(t *testing.T) {
		req := NewRequest("GET", "http://example.com/foo?a=1", nil)
		raw := string(mustBuild(t, req))

		wantPrefix := "GET /foo?a=1 HTTP/1.1\r\nHost: example.com\r\n"
		if !strings.HasPrefix(raw, wantPrefix) {
			t.Errorf("request line/Host wrong:\n%q", raw)
		}
		for _, want := range []string{
			"User-Agent: " + DefaultUserAgent + "\r\n",
			"Accept: */*\r\n",
			"Connection: keep-alive\r\n",
		} {
			if !strings.Contains(raw, want) {
				t.Errorf("missing %q in:\n%q", want, raw)
			}
		}
		if strings.Contains(raw, "Content-Length") {
			t.Error("bodyless request must not send Content-Length")
		}
	})

	t.Run("post sends content length and body", func(t *testing.T) {
		body := []byte(`{"k":"v"}`)
		req := NewRequest("POST", "http://example.com/x", body).
			SetHeader("Content-Type", "application/json")
		raw := string(mustBuild(t, req))

		if !strings.Contains(raw, fmt.Sprintf("Content-Length: %d\r\n", len(body))) {
			t.Errorf("missing/incorrect Content-Length:\n%q", raw)
		}
		if !strings.HasSuffix(raw, "\r\n\r\n"+string(body)) {
			t.Errorf("body must follow the blank line exactly:\n%q", raw)
		}
	})

	t.Run("caller headers win over defaults", func(t *testing.T) {
		req := NewRequest("GET", "http://example.com/", nil).
			SetHeader("User-Agent", "custom-agent")
		raw := string(mustBuild(t, req))

		if !strings.Contains(raw, "User-Agent: custom-agent\r\n") {
			t.Errorf("caller User-Agent missing:\n%q", raw)
		}
		if strings.Contains(raw, DefaultUserAgent) {
			t.Error("default User-Agent must not also be sent")
		}
	})

	t.Run("empty path becomes slash", func(t *testing.T) {
		req := NewRequest("GET", "http://example.com", nil)
		raw := string(mustBuild(t, req))
		if !strings.HasPrefix(raw, "GET / HTTP/1.1\r\n") {
			t.Errorf("want request-target \"/\", got:\n%q", raw)
		}
	})

	t.Run("tracing header survives serialisation", func(t *testing.T) {
		const tp = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
		req := NewRequest("GET", "http://example.com/", nil).
			SetHeader("traceparent", tp)
		raw := string(mustBuild(t, req))
		if !strings.Contains(raw, "Traceparent: "+tp+"\r\n") {
			t.Errorf("traceparent not serialised:\n%q", raw)
		}
	})
}

// ── ClientRequest ──────────────────────────────────────────────────────────

func TestClientRequestHeaders(t *testing.T) {
	t.Run("set is case insensitive for get", func(t *testing.T) {
		req := NewRequest("GET", "http://x/", nil).SetHeader("traceparent", "abc")
		if v, ok := req.GetHeader("Traceparent"); !ok || v != "abc" {
			t.Errorf("GetHeader(canonical) = %q,%v; a Carrier writing lowercase "+
				"and a reader asking canonical must agree", v, ok)
		}
	})

	t.Run("set replaces add appends", func(t *testing.T) {
		req := NewRequest("GET", "http://x/", nil)
		req.SetHeader("X-K", "one").SetHeader("X-K", "two")
		if got := req.Header().Values("X-K"); len(got) != 1 || got[0] != "two" {
			t.Errorf("Set must replace, got %v", got)
		}
		req.AddHeader("X-K", "three")
		if got := req.Header().Values("X-K"); len(got) != 2 {
			t.Errorf("Add must append, got %v", got)
		}
	})

	t.Run("missing header reports absent", func(t *testing.T) {
		req := NewRequest("GET", "http://x/", nil)
		if v, ok := req.GetHeader("Nope"); ok || v != "" {
			t.Errorf("got %q,%v; want \"\",false", v, ok)
		}
	})

	// Every mutator is nil-safe so an injection helper can operate on a
	// request it did not create without a nil check at each call site.
	t.Run("nil receiver does not panic", func(t *testing.T) {
		var req *ClientRequest
		req.SetHeader("a", "b")
		req.AddHeader("a", "b")
		if _, ok := req.GetHeader("a"); ok {
			t.Error("nil request reported a header")
		}
		if req.Header() != nil {
			t.Error("nil request returned non-nil Header")
		}
		if req.Context() == nil {
			t.Error("nil request must still yield a usable context")
		}
	})
}

func TestResponseHelpers(t *testing.T) {
	for _, tc := range []struct {
		status int
		wantOK bool
	}{{199, false}, {200, true}, {204, true}, {299, true}, {300, false}, {500, false}} {
		if got := (&Response{Status: tc.status}).OK(); got != tc.wantOK {
			t.Errorf("Status %d: OK() = %v, want %v", tc.status, got, tc.wantOK)
		}
	}
	var nilResp *Response
	if nilResp.OK() || nilResp.String() != "" {
		t.Error("nil Response must be safe to inspect")
	}
	if got := (&Response{Body: []byte("hi")}).String(); got != "hi" {
		t.Errorf("String() = %q", got)
	}
}

func TestConfigDefaults(t *testing.T) {
	got := Config{}.withDefaults()
	want := DefaultConfig()
	if got != want {
		t.Errorf("zero Config did not fill in defaults:\ngot  %+v\nwant %+v", got, want)
	}

	// One explicit field must not reset the others.
	partial := Config{Timeout: time.Second}.withDefaults()
	if partial.Timeout != time.Second {
		t.Errorf("explicit Timeout overwritten: %v", partial.Timeout)
	}
	if partial.MaxIdleConnsPerHost != DefaultMaxIdleConnsPerHost {
		t.Errorf("unset field not defaulted: %d", partial.MaxIdleConnsPerHost)
	}
}

func TestDoInputValidation(t *testing.T) {
	c := New()
	defer c.Close()

	if _, err := c.Do(nil); !errors.Is(err, ErrNilRequest) {
		t.Errorf("Do(nil) = %v, want ErrNilRequest", err)
	}
	if _, err := c.Do(NewRequest("GET", "   ", nil)); !errors.Is(err, ErrNoURL) {
		t.Errorf("blank URL = %v, want ErrNoURL", err)
	}
	// Malformed URLs must be reported, not dialed.
	if _, err := c.Do(NewRequest("GET", "http://[::1]:namedport/", nil)); err == nil {
		t.Error("malformed URL: want error, got nil")
	}
}

// ── Live round trips over the real gnet event loop ─────────────────────────

// These are the tests that matter most: they run the actual gnet client
// against a real HTTP server, so serialisation, the event loop, the response
// parser, and connection pooling are all exercised together. A unit test of
// the parser alone would not catch a mistake in how bytes reach it.

func TestClientLiveRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/echo":
			w.Header().Set("X-Method", r.Method)
			w.Header().Set("Content-Type", "text/plain")
			body := make([]byte, r.ContentLength)
			if r.ContentLength > 0 {
				_, _ = r.Body.Read(body)
			}
			w.WriteHeader(200)
			_, _ = w.Write(body)
		case "/empty":
			w.WriteHeader(204)
		case "/teapot":
			w.WriteHeader(418)
			_, _ = w.Write([]byte("short and stout"))
		case "/chunked":
			// No Content-Length set and a flush mid-write forces Go's
			// server to use chunked encoding.
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("chunk-one "))
			w.(http.Flusher).Flush()
			_, _ = w.Write([]byte("chunk-two"))
		case "/headers":
			w.Header().Set("X-Traceparent-Seen", r.Header.Get("traceparent"))
			w.WriteHeader(200)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	c := New(Config{Timeout: 5 * time.Second})
	defer c.Close()

	t.Run("get with body", func(t *testing.T) {
		resp, err := c.Get(srv.URL + "/teapot")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if resp.Status != 418 {
			t.Errorf("status = %d, want 418", resp.Status)
		}
		if string(resp.Body) != "short and stout" {
			t.Errorf("body = %q", resp.Body)
		}
		if resp.OK() {
			t.Error("418 must not report OK")
		}
	})

	t.Run("post round trips body", func(t *testing.T) {
		payload := []byte(`{"hello":"world"}`)
		resp, err := c.PostJSON(srv.URL+"/echo", payload)
		if err != nil {
			t.Fatalf("PostJSON: %v", err)
		}
		if !resp.OK() {
			t.Fatalf("status = %d", resp.Status)
		}
		if string(resp.Body) != string(payload) {
			t.Errorf("echoed body = %q, want %q", resp.Body, payload)
		}
		if got := resp.Header.Get("X-Method"); got != "POST" {
			t.Errorf("server saw method %q, want POST", got)
		}
	})

	t.Run("204 completes without hanging", func(t *testing.T) {
		// If the bodiless-status rule were missing, this would block until
		// Timeout instead of returning.
		done := make(chan struct{})
		go func() {
			defer close(done)
			resp, err := c.Get(srv.URL + "/empty")
			if err != nil {
				t.Errorf("Get: %v", err)
				return
			}
			if resp.Status != 204 || len(resp.Body) != 0 {
				t.Errorf("got %d with %d body bytes, want 204 and none",
					resp.Status, len(resp.Body))
			}
		}()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("204 response never completed")
		}
	})

	t.Run("chunked response decodes", func(t *testing.T) {
		resp, err := c.Get(srv.URL + "/chunked")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got := string(resp.Body); got != "chunk-one chunk-two" {
			t.Errorf("body = %q, want %q", got, "chunk-one chunk-two")
		}
	})

	t.Run("headers reach the server", func(t *testing.T) {
		const tp = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
		req := NewRequest("GET", srv.URL+"/headers", nil).SetHeader("traceparent", tp)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		if got := resp.Header.Get("X-Traceparent-Seen"); got != tp {
			t.Errorf("server saw traceparent %q, want %q", got, tp)
		}
	})
}

// Sequential requests must reuse pooled connections, and concurrent ones must
// not cross responses. Both are properties of the pool rather than the parser,
// so they need a live server to test.
func TestClientConnectionReuseAndConcurrency(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Echo back the caller's id so a crossed response is detectable.
		id := r.Header.Get("X-Id")
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(id))
	}))
	defer srv.Close()

	c := New(Config{Timeout: 5 * time.Second})
	defer c.Close()

	t.Run("sequential requests reuse one connection", func(t *testing.T) {
		for i := 0; i < 5; i++ {
			req := NewRequest("GET", srv.URL, nil).
				SetHeader("X-Id", fmt.Sprintf("seq-%d", i))
			resp, err := c.Do(req)
			if err != nil {
				t.Fatalf("request %d: %v", i, err)
			}
			if got, want := string(resp.Body), fmt.Sprintf("seq-%d", i); got != want {
				t.Fatalf("request %d returned %q, want %q", i, got, want)
			}
		}
	})

	t.Run("concurrent requests do not cross responses", func(t *testing.T) {
		const n = 24
		var wg sync.WaitGroup
		errs := make(chan error, n)

		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				id := fmt.Sprintf("cc-%d", i)
				req := NewRequest("GET", srv.URL, nil).SetHeader("X-Id", id)
				resp, err := c.Do(req)
				if err != nil {
					errs <- fmt.Errorf("request %d: %w", i, err)
					return
				}
				if got := string(resp.Body); got != id {
					errs <- fmt.Errorf("request %d got response for %q: "+
						"responses crossed connections", i, got)
				}
			}(i)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
	})
}

func TestClientErrorPaths(t *testing.T) {
	c := New(Config{Timeout: 2 * time.Second, DialTimeout: time.Second})
	defer c.Close()

	t.Run("connection refused", func(t *testing.T) {
		// Port 1 on loopback is reliably closed.
		if _, err := c.Get("http://127.0.0.1:1/"); err == nil {
			t.Error("want error connecting to a closed port")
		}
	})

	t.Run("oversized response is rejected", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(make([]byte, 4096))
		}))
		defer srv.Close()

		small := New(Config{Timeout: 3 * time.Second, MaxResponseBytes: 128})
		defer small.Close()

		_, err := small.Get(srv.URL)
		if !errors.Is(err, ErrResponseTooLarge) {
			t.Errorf("err = %v, want ErrResponseTooLarge", err)
		}
	})

	t.Run("server that never responds times out", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(5 * time.Second) // outlives the client timeout
		}))
		defer srv.Close()

		slow := New(Config{Timeout: 500 * time.Millisecond})
		defer slow.Close()

		start := time.Now()
		if _, err := slow.Get(srv.URL); err == nil {
			t.Fatal("want timeout error")
		}
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Errorf("took %v; Timeout was not honoured", elapsed)
		}
	})
}

// ── helpers ────────────────────────────────────────────────────────────────

// mustBuild serialises req the way Do would, so the assertions above test the
// exact bytes that go on the wire.
func mustBuild(t *testing.T, req *ClientRequest) []byte {
	t.Helper()
	u, err := url.Parse(req.URL)
	if err != nil {
		t.Fatalf("parse %q: %v", req.URL, err)
	}
	return buildHTTPRequest(req, u, DefaultUserAgent)
}
