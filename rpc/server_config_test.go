package rpc

import (
	"testing"

	"github.com/nelthaarion/gnetplus/v2"
)

func TestServerListenerConfiguration(t *testing.T) {
	s := NewServer(NewRegistry())
	if got := s.ListenHost(); got != "" {
		t.Fatalf("default ListenHost() = %q, want empty", got)
	}
	if got := s.ListenPort(); got != 0 {
		t.Fatalf("default ListenPort() = %d, want 0", got)
	}

	s.listenHost.Store(func() *string { h := "127.0.0.1"; return &h }())
	s.listenPort.Store(3001)
	if got := s.ListenHost(); got != "127.0.0.1" {
		t.Fatalf("ListenHost() = %q, want 127.0.0.1", got)
	}
	if got := s.ListenPort(); got != 3001 {
		t.Fatalf("ListenPort() = %d, want 3001", got)
	}
}

func TestServerMaxRequestBody(t *testing.T) {
	s := NewServer(NewRegistry())
	if got := s.MaxRequestBody(); got != 0 {
		t.Fatalf("default MaxRequestBody() = %d, want 0", got)
	}
	s.SetMaxRequestBody(32)
	if got := s.MaxRequestBody(); got != 32 {
		t.Fatalf("MaxRequestBody() = %d, want 32", got)
	}
	s.SetMaxRequestBody(-1)
	if got := s.MaxRequestBody(); got != 0 {
		t.Fatalf("negative SetMaxRequestBody = %d, want 0", got)
	}
}

func TestServerMaxRequestBodyRejectsCompleteMessage(t *testing.T) {
	calls := 0
	r := NewRegistry()
	r.Register("echo", func(ctx *Context) {
		calls++
		ctx.Result("ok")
	})
	s := NewServer(r)
	s.SetMaxRequestBody(16)
	c := &fakeConn{}

	message := `{"jsonrpc":"2.0","method":"echo","id":1}`
	if action := feed(s, c, message); action != gnetplus.Close {
		t.Fatalf("action = %v, want Close", action)
	}
	if calls != 0 {
		t.Fatalf("handler calls = %d, want 0", calls)
	}
	assertErrorCode(t, decode(t, c.written.Bytes()), CodeInvalidRequest)
}

func TestServerMaxRequestBodyAllowsExactLimit(t *testing.T) {
	message := `{"jsonrpc":"2.0","method":"sum","params":[1],"id":1}`
	s := testServer(t)
	s.SetMaxRequestBody(int64(len(message)))
	c := &fakeConn{}

	if action := feed(s, c, message); action != gnetplus.None {
		t.Fatalf("action = %v, want None", action)
	}
	if got := assertResult(t, decode(t, c.written.Bytes())); got != float64(1) {
		t.Fatalf("result = %v, want 1", got)
	}
}
