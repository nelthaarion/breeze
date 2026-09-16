package breeze

import (
	"testing"
)

func TestParseHTTPRequestBasicGET(t *testing.T) {
	raw := []byte("GET /hello?x=1 HTTP/1.1\r\nHost: example.com\r\nUser-Agent: test\r\n\r\n")
	req, consumed, err := ParseHTTPRequest(raw)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if req == nil {
		t.Fatal("expected request")
	}
	if consumed != len(raw) {
		t.Fatalf("consumed=%d want=%d", consumed, len(raw))
	}
	if req.Method != GET {
		t.Fatalf("method=%v want GET", req.Method)
	}
	if req.Path != "/hello" {
		t.Fatalf("path=%q", req.Path)
	}
	if got := req.Header["host"]; got != "example.com" {
		t.Fatalf("host=%q", got)
	}
	if got := req.Header["user-agent"]; got != "test" {
		t.Fatalf("user-agent=%q", got)
	}
	if got := req.Query.Get("x"); got != "1" {
		t.Fatalf("query x=%q", got)
	}
}

func TestParseHTTPRequestPostBody(t *testing.T) {
	raw := []byte("POST / HTTP/1.1\r\nHost: example.com\r\nContent-Type: text/plain\r\nContent-Length: 5\r\n\r\nhello")
	req, consumed, err := ParseHTTPRequest(raw)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if req == nil {
		t.Fatal("expected request")
	}
	if consumed != len(raw) {
		t.Fatalf("consumed=%d want=%d", consumed, len(raw))
	}
	if string(req.Body) != "hello" {
		t.Fatalf("body=%q", req.Body)
	}
}
