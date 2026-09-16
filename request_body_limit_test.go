package breeze

import (
	"bytes"
	"testing"

	"github.com/nelthaarion/gnet/v2"
)

type bodyLimitConn struct {
	gnet.Conn
	inbound []byte
	written bytes.Buffer
	ctx     any
}

func (c *bodyLimitConn) Next(n int) ([]byte, error) {
	if n < 0 || n > len(c.inbound) {
		n = len(c.inbound)
	}
	b := c.inbound[:n]
	c.inbound = c.inbound[n:]
	return b, nil
}

func (c *bodyLimitConn) Write(b []byte) (int, error) { return c.written.Write(b) }
func (c *bodyLimitConn) Context() any                { return c.ctx }
func (c *bodyLimitConn) SetContext(v any)            { c.ctx = v }

func TestMaxRequestBodyConfiguration(t *testing.T) {
	app := New(NewRouter(), NewEventLoopWorkerPool(1))
	if got := app.MaxRequestBody(); got != 8<<20 {
		t.Fatalf("default MaxRequestBody() = %d, want 8 MiB", got)
	}

	app.SetMaxRequestBody(1024)
	if got := app.MaxRequestBody(); got != 1024 {
		t.Fatalf("MaxRequestBody() = %d, want 1024", got)
	}

	app.SetMaxRequestBody(-1)
	if got := app.MaxRequestBody(); got != 0 {
		t.Fatalf("negative SetMaxRequestBody left %d, want 0", got)
	}
}

func TestRequestBodyLimitDeclaredLength(t *testing.T) {
	app := New(NewRouter(), NewEventLoopWorkerPool(1))
	app.SetMaxRequestBody(4)

	called := false
	app.Router.Handle(POST, "/", func(*Context) error {
		called = true
		return nil
	})

	c := &bodyLimitConn{inbound: []byte("POST / HTTP/1.1\r\nHost: test\r\nContent-Length: 5\r\n\r\nhello")}
	if action := app.OnTraffic(c); action != gnet.Close {
		t.Fatalf("action = %v, want Close", action)
	}
	if called {
		t.Fatal("handler was invoked for an oversized declared body")
	}
	if got := c.written.String(); !bytes.HasPrefix([]byte(got), []byte("HTTP/1.1 413 Request Entity Too Large")) {
		t.Fatalf("response = %q, want 413", got)
	}
}

func TestRequestBodyLimitExactLengthPasses(t *testing.T) {
	app := New(NewRouter(), NewEventLoopWorkerPool(1))
	app.SetMaxRequestBody(5)

	called := false
	app.Router.Handle(POST, "/", func(c *Context) error {
		called = true
		if string(c.Req.Body) != "hello" {
			t.Errorf("body = %q, want hello", c.Req.Body)
		}
		return nil
	})

	c := &bodyLimitConn{inbound: []byte("POST / HTTP/1.1\r\nHost: test\r\nContent-Length: 5\r\n\r\nhello")}
	if action := app.OnTraffic(c); action != gnet.None {
		t.Fatalf("action = %v, want None", action)
	}
	if !called {
		t.Fatal("handler was not invoked for a body exactly at the configured limit")
	}
}

func TestParseHTTPRequestBodyLimit(t *testing.T) {
	raw := []byte("POST / HTTP/1.1\r\nHost: test\r\nContent-Length: 6\r\n\r\n123456")

	if _, _, err := parsePooledRequest(raw, true, 5); err != ErrBodyTooLarge {
		t.Fatalf("err = %v, want ErrBodyTooLarge", err)
	}

	req, _, err := parsePooledRequest(raw, true, 6)
	if err != nil {
		t.Fatalf("exact-limit parse failed: %v", err)
	}
	if string(req.Body) != "123456" {
		t.Fatalf("body = %q, want 123456", req.Body)
	}
	releaseRequest(req)
}
