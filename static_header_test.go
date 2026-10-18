package breeze

import (
	"sort"
	"strings"
	"testing"
)

// static_header_test.go guards the SetHeaderStatic fast path (response.go /
// context.go). The optimisation lets a response whose headers came from static
// middleware serialize from a precomputed wire block instead of a per-request
// map; these tests pin the invariant that the block and the map produce the
// same bytes, and that a later dynamic write cannot lose a header.

func headerSetFromWire(wire []byte) (status string, headers []string, body string) {
	s := string(wire)
	if i := strings.Index(s, "\r\n\r\n"); i >= 0 {
		head, rest := s[:i], s[i+4:]
		lines := strings.Split(head, "\r\n")
		status = lines[0]
		headers = append(headers, lines[1:]...)
		body = rest
	}
	sort.Strings(headers)
	return
}

// A response built through SetHeaderStatic must be byte-equivalent, header-for-
// header and body-for-body, to the same response built through SetHeader.
func TestSetHeaderStaticSerializesIdenticallyToSetHeader(t *testing.T) {
	kvs := []string{
		"Access-Control-Allow-Origin", "*",
		"X-Frame-Options", "DENY",
		"X-Content-Type-Options", "nosniff",
	}
	block, lookup := RenderStaticHeaders(kvs...)

	cs := NewContext(GET, "/x")
	cs.SetHeaderStatic(block, lookup)
	if err := cs.JSON(map[string]string{"a": "b"}); err != nil {
		t.Fatal(err)
	}
	staticWire := cs.Res.AppendTo(nil)

	cm := NewContext(GET, "/x")
	for i := 0; i+1 < len(kvs); i += 2 {
		cm.SetHeader(kvs[i], kvs[i+1])
	}
	if err := cm.JSON(map[string]string{"a": "b"}); err != nil {
		t.Fatal(err)
	}
	mapWire := cm.Res.AppendTo(nil)

	stS, hS, bS := headerSetFromWire(staticWire)
	stM, hM, bM := headerSetFromWire(mapWire)

	if stS != stM {
		t.Fatalf("status line differs: %q vs %q", stS, stM)
	}
	if bS != bM {
		t.Fatalf("body differs: %q vs %q", bS, bM)
	}
	if strings.Join(hS, "|") != strings.Join(hM, "|") {
		t.Fatalf("header set differs:\n static=%v\n map   =%v", hS, hM)
	}
	if n := strings.Count(string(staticWire), "Content-Type:"); n != 1 {
		t.Fatalf("%d Content-Type lines on the static wire:\n%s", n, staticWire)
	}
	if cs.Res.preBuilt == nil {
		t.Fatal("expected preBuilt block on the static path")
	}
	if cm.Res.preBuilt != nil {
		t.Fatal("map path must not build a preBuilt block")
	}
}

// A static response must still answer GetHeader for its static headers and the
// body method's content type while it stays on the fast path.
func TestSetHeaderStaticGetHeader(t *testing.T) {
	block, lookup := RenderStaticHeaders("X-Frame-Options", "DENY")
	c := NewContext(GET, "/x")
	c.SetHeaderStatic(block, lookup)
	if err := c.JSON(map[string]string{"a": "b"}); err != nil {
		t.Fatal(err)
	}
	if c.GetHeader("X-Frame-Options") != "DENY" {
		t.Fatalf("GetHeader lost static header: %q", c.GetHeader("X-Frame-Options"))
	}
	if ct := c.GetHeader("Content-Type"); ct != "application/json" {
		t.Fatalf("GetHeader content type = %q", ct)
	}
}
