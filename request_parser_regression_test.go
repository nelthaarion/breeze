package breeze

import (
	"strconv"
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

func TestParseHTTPRequestHugeContentLengthIsIncomplete(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	raw := []byte("POST / HTTP/1.1\r\nHost: example.com\r\nContent-Length: " + strconv.Itoa(maxInt) + "\r\n\r\n")

	req, consumed, err := ParseHTTPRequest(raw)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if req != nil || consumed != 0 {
		t.Fatalf("got req=%v consumed=%d, want incomplete request", req, consumed)
	}
}

func TestParseHTTPRequestOversizedUnterminatedHeaders(t *testing.T) {
	raw := make([]byte, maxRequestHeaderBytes+1)
	copy(raw, "GET / HTTP/1.1\r\nHost: example.com\r\n")

	_, _, err := ParseHTTPRequest(raw)
	if err != ErrHeadersTooLarge {
		t.Fatalf("err = %v, want ErrHeadersTooLarge", err)
	}
}
