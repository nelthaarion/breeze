package breeze

// Regression tests for the audit findings in the HTTP-core category:
//
//   - pipelined responses were reordered when a blocking route was followed by an
//     inline one;
//   - a request the worker pool refused was dropped with no response at all;
//   - "Connection: close" and HTTP/1.0 were ignored, so such clients waited for a
//     close that never came;
//   - a malformed request got a 400 but the connection stayed open;
//   - there was no idle or header-read timeout, so slowloris held descriptors.

import (
	"bufio"
	"context"
	"io"
	"net"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func startAuditServer(t *testing.T, app *Breeze) int {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	go func() { _ = app.Run(port, false) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 200*time.Millisecond); err == nil {
			_ = c.Close()
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				_ = app.Stop(ctx)
			})
			return port
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("server did not start")
	return 0
}

func auditApp() *Breeze {
	router := NewRouter()
	app := New(router, NewEventLoopWorkerPool(runtime.NumCPU()))
	router.Handle(GET, "/fast", func(c *Context) error { return c.WriteString("fast") })
	router.HandleBlocking(GET, "/slow", func(c *Context) error {
		time.Sleep(150 * time.Millisecond)
		return c.WriteString("slow")
	})
	return app
}

func dial(t *testing.T, port int) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// readResponse reads one Content-Length-framed response and returns head and body.
func readResponse(t *testing.T, r *bufio.Reader) (string, string) {
	t.Helper()
	var head strings.Builder
	n := 0
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("reading response head: %v (got %q)", err, head.String())
		}
		head.WriteString(line)
		if k, v, ok := strings.Cut(strings.TrimSpace(line), ":"); ok && strings.EqualFold(k, "content-length") {
			n, _ = strconv.Atoi(strings.TrimSpace(v))
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		t.Fatalf("reading body: %v", err)
	}
	return head.String(), string(body)
}

func expectEOF(t *testing.T, c net.Conn, within time.Duration, why string) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(within))
	if _, err := io.Copy(io.Discard, c); err != nil {
		t.Fatalf("%s: connection was not closed within %v (%v)", why, within, err)
	}
}

func TestPipelinedResponsesStayInRequestOrder(t *testing.T) {
	port := startAuditServer(t, auditApp())
	c := dial(t, port)
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))

	// slow (worker pool), fast (inline), slow, fast — all in one write.
	req := "GET /slow HTTP/1.1\r\nHost: x\r\n\r\nGET /fast HTTP/1.1\r\nHost: x\r\n\r\n"
	if _, err := c.Write([]byte(req + req)); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(c)
	for i, want := range []string{"slow", "fast", "slow", "fast"} {
		_, body := readResponse(t, r)
		if body != want {
			t.Fatalf("response %d = %q, want %q: pipelined responses are out of order", i, body, want)
		}
	}
}

func TestKeepAliveStillServesSequentialRequests(t *testing.T) {
	port := startAuditServer(t, auditApp())
	c := dial(t, port)
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(c)
	for _, p := range []string{"/fast", "/slow", "/fast", "/slow"} {
		if _, err := c.Write([]byte("GET " + p + " HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
			t.Fatal(err)
		}
		_, body := readResponse(t, r)
		if want := strings.TrimPrefix(p, "/"); body != want {
			t.Fatalf("%s answered %q", p, body)
		}
	}
}

func TestRejectedBlockingRequestGetsA503(t *testing.T) {
	app := auditApp()
	// A pool that has been shut down refuses every task, which is the same
	// refusal an OverflowReject pool gives when its queue is full.
	pool := NewWorkerPoolWithConfig(WorkerPoolConfig{Workers: 1, QueueSize: 1, Overflow: OverflowReject})
	app.Pool = pool
	pool.Shutdown(context.Background())

	port := startAuditServer(t, app)
	c := dial(t, port)
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte("GET /slow HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	head, _ := readResponse(t, bufio.NewReader(c))
	if !strings.HasPrefix(head, "HTTP/1.1 503") {
		t.Fatalf("a request the pool refused must be answered 503, got %q", head)
	}
}

func TestConnectionCloseIsHonored(t *testing.T) {
	port := startAuditServer(t, auditApp())
	for _, path := range []string{"/fast", "/slow", "/nope"} {
		c := dial(t, port)
		_, _ = c.Write([]byte("GET " + path + " HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"))
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		all, err := io.ReadAll(c)
		if err != nil {
			t.Fatalf("%s: server did not close after Connection: close (%v)", path, err)
		}
		if !strings.Contains(strings.ToLower(string(all)), "connection: close") {
			t.Fatalf("%s: response must announce the close: %q", path, all)
		}
	}
}

func TestHTTP10ClosesUnlessKeepAliveRequested(t *testing.T) {
	port := startAuditServer(t, auditApp())

	c := dial(t, port)
	_, _ = c.Write([]byte("GET /fast HTTP/1.0\r\n\r\n"))
	expectEOF(t, c, 3*time.Second, "HTTP/1.0 without keep-alive")

	// Explicit keep-alive: stays open and says so.
	k := dial(t, port)
	_ = k.SetDeadline(time.Now().Add(3 * time.Second))
	r := bufio.NewReader(k)
	for i := 0; i < 2; i++ {
		_, _ = k.Write([]byte("GET /fast HTTP/1.0\r\nConnection: keep-alive\r\n\r\n"))
		head, body := readResponse(t, r)
		if body != "fast" || !strings.Contains(strings.ToLower(head), "connection: keep-alive") {
			t.Fatalf("HTTP/1.0 keep-alive round %d: head=%q body=%q", i, head, body)
		}
	}
}

func TestConnectionModeTable(t *testing.T) {
	cases := []struct {
		http10 bool
		hdr    string
		want   uint8
	}{
		{false, "", connModeKeepAlive},
		{false, "keep-alive", connModeKeepAlive},
		{false, "close", connModeClose},
		{false, "Keep-Alive, Close", connModeClose},
		{false, " CLOSE ", connModeClose},
		{true, "", connModeClose},
		{true, "keep-alive", connModeKeepAlive10},
		{true, "keep-alive, close", connModeClose},
	}
	for _, tc := range cases {
		if got := connectionMode(tc.http10, tc.hdr); got != tc.want {
			t.Errorf("connectionMode(http10=%v, %q) = %d, want %d", tc.http10, tc.hdr, got, tc.want)
		}
	}
}

func TestMalformedRequestGets400AndClose(t *testing.T) {
	port := startAuditServer(t, auditApp())
	c := dial(t, port)
	_, _ = c.Write([]byte("GET /fast HTTP/1.1\r\nHost: x\r\nbad header line\r\n\r\n"))
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	all, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("connection stayed open after a malformed request: %v", err)
	}
	if !strings.HasPrefix(string(all), "HTTP/1.1 400") {
		t.Fatalf("want 400, got %q", all)
	}
}

func TestSlowHeadersAreCutOff(t *testing.T) {
	app := auditApp()
	app.SetReadHeaderTimeout(time.Second)
	port := startAuditServer(t, app)

	c := dial(t, port)
	_, _ = c.Write([]byte("GET /fast HTTP/1.1\r\nHost: x\r\n")) // headers never finish
	// Keep trickling so the idle timer alone would never fire.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(300 * time.Millisecond):
				_, _ = c.Write([]byte("X-A: b\r\n"))
			}
		}
	}()
	expectEOF(t, c, 6*time.Second, "slowloris header trickle")
}

func TestIdleConnectionsAreClosed(t *testing.T) {
	app := auditApp()
	app.SetIdleTimeout(time.Second)
	port := startAuditServer(t, app)
	c := dial(t, port)
	expectEOF(t, c, 6*time.Second, "idle connection")
}

func TestRunningHandlerIsNotIdle(t *testing.T) {
	router := NewRouter()
	app := New(router, NewEventLoopWorkerPool(runtime.NumCPU()))
	app.SetIdleTimeout(time.Second)
	router.HandleBlocking(GET, "/long", func(c *Context) error {
		time.Sleep(2500 * time.Millisecond)
		return c.WriteString("done")
	})
	port := startAuditServer(t, app)
	c := dial(t, port)
	_ = c.SetDeadline(time.Now().Add(8 * time.Second))
	_, _ = c.Write([]byte("GET /long HTTP/1.1\r\nHost: x\r\n\r\n"))
	if _, body := readResponse(t, bufio.NewReader(c)); body != "done" {
		t.Fatalf("a request in flight must not be reaped as idle, got %q", body)
	}
}
