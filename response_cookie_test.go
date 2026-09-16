package breeze

// response_cookie_test.go — one response may carry several Set-Cookie headers,
// and the only thing allowed to put a second one there is the framework.
//
// # Why these read the bytes
//
// The header map holds every cookie on a single value, separated by CRLF, so the
// map cannot express the difference between a correctly written second cookie and
// a malformed line — both are just characters in a string. Every failure this file
// guards against was invisible at the map level:
//
//   - a handler writing two cookies and getting one, because the second replaced
//     the first;
//   - a caller smuggling a header line in through a cookie value;
//   - a producer that pre-joined its own "Set-Cookie: " prefix, so the wire read
//     "Set-Cookie: Set-Cookie: <name>=..." and browsers dropped the cookie while
//     ctx.GetHeader still reported it present.
//
// The last one is not hypothetical: it is what middlewares/oauth2 did, and it made
// login silently produce no session. Assert the wire, because that is what the
// browser parses.

import (
	"strings"
	"testing"
)

// newCookieCtx returns a context with a response already allocated, so a write the
// framework refuses can still be inspected. SetHeader validates before it creates
// the response, so without this ctx.Res is nil on the refusal path.
func newCookieCtx() *Context {
	ctx := NewContext(GET, "/")
	ctx.Status(200)
	return ctx
}

// setCookieLines returns the Set-Cookie lines of the serialized response.
func setCookieLines(ctx *Context) []string {
	if ctx.Res == nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(ctx.Res.Bytes()), "\r\n") {
		if strings.HasPrefix(strings.ToLower(line), "set-cookie:") {
			out = append(out, line)
		}
	}
	return out
}

// TestTwoCookiesBecomeTwoHeaderLines is why SetHeader appends for this key rather
// than replacing.
//
// Replacing keeps only the last cookie, so a callback that clears a single-use
// flow cookie and then writes a session cookie sends one of the two, and which one
// survives depends on call order — the kind of bug that shows up as "logging in
// works but the session is gone" and points at the wrong file.
func TestTwoCookiesBecomeTwoHeaderLines(t *testing.T) {
	ctx := newCookieCtx()
	ctx.SetHeader("Set-Cookie", "flow=abc; Path=/; Max-Age=0")
	ctx.SetHeader("Set-Cookie", "session=xyz; Path=/; HttpOnly")

	lines := setCookieLines(ctx)
	if len(lines) != 2 {
		t.Fatalf("got %d Set-Cookie lines, want 2:\n%s", len(lines), ctx.Res.Bytes())
	}
	if lines[0] != "Set-Cookie: flow=abc; Path=/; Max-Age=0" {
		t.Errorf("first line = %q", lines[0])
	}
	if lines[1] != "Set-Cookie: session=xyz; Path=/; HttpOnly" {
		t.Errorf("second line = %q", lines[1])
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "Set-Cookie: Set-Cookie:") {
			t.Errorf("the header name was emitted twice; browsers discard this line: %q", line)
		}
	}
}

// TestASetCookieValueContainingCRLFIsRefused keeps the multi-cookie separator the
// framework's own.
//
// The serializer splits the stored value on CRLF, so a value that can contain a
// CRLF can add header lines and name them. Refusing the write outright is what the
// contract above SetHeader promises, and it costs nothing: the way to send two
// cookies is two calls, not a pre-joined string.
func TestASetCookieValueContainingCRLFIsRefused(t *testing.T) {
	ctx := newCookieCtx()
	ctx.SetHeader("Set-Cookie", "a=1\r\nSet-Cookie: b=2")
	ctx.SetHeader("Set-Cookie", "c=3\r\nLocation: https://evil.example")

	if lines := setCookieLines(ctx); len(lines) != 0 {
		t.Errorf("a CRLF-bearing cookie value was written as %d lines:\n%s", len(lines), ctx.Res.Bytes())
	}
	if wire := string(ctx.Res.Bytes()); strings.Contains(wire, "evil.example") {
		t.Errorf("attacker-chosen bytes reached the wire:\n%s", wire)
	}
}

// TestABareLFInASetCookieValueIsRefused covers the byte a "\r\n"-only check would
// let through.
//
// A lone LF does not split the value, so it cannot add a cookie line on its own —
// the serializer rejects the segment and drops it. That is why this asserts the map
// and not just the wire: a wire-only assertion passes whether or not SetHeader
// refused the write, because two independent layers would each suppress it. The map
// is the layer the contract above SetHeader names, and it is where the difference
// shows.
func TestABareLFInASetCookieValueIsRefused(t *testing.T) {
	ctx := newCookieCtx()
	ctx.SetHeader("Set-Cookie", "a=1\nSet-Cookie: b=2")

	if got := ctx.Res.Headers["Set-Cookie"]; got != "" {
		t.Errorf("a value containing a bare LF entered the response map: %q", got)
	}
	if lines := setCookieLines(ctx); len(lines) != 0 {
		t.Errorf("got %d cookie lines, want none:\n%s", len(lines), ctx.Res.Bytes())
	}
}

// TestASetCookieIsStillRefusedWhenTheNameIsInvalid keeps the refusal from becoming
// an exemption for the key as a whole: the append path is reached only after the
// name check, so an invalid name is refused on every header, this one included.
func TestASetCookieIsStillRefusedWhenTheNameIsInvalid(t *testing.T) {
	ctx := newCookieCtx()
	ctx.SetHeader("Set-Cookie\r\nX: y", "a=1")

	if lines := setCookieLines(ctx); len(lines) != 0 {
		t.Errorf("a header with an invalid name was written:\n%s", ctx.Res.Bytes())
	}
}

// TestALaterCookieAppendsToAnEarlierOneAcrossBodyMethods pins that the append
// survives the copy-on-write upgrade.
//
// A body method installs a shared package-level header map; SetHeader copies it
// before mutating. A cookie written before the body method and one written after
// it must still land on two lines, which is the case a copy that dropped the
// accumulated value would break.
func TestALaterCookieAppendsToAnEarlierOneAcrossBodyMethods(t *testing.T) {
	ctx := NewContext(GET, "/")
	ctx.SetHeader("Set-Cookie", "first=1; Path=/")
	_ = ctx.JSON(struct{ N int }{1})
	ctx.SetHeader("Set-Cookie", "second=2; Path=/")

	lines := setCookieLines(ctx)
	if len(lines) != 2 {
		t.Fatalf("got %d Set-Cookie lines, want 2:\n%s", len(lines), ctx.Res.Bytes())
	}
	if !strings.Contains(lines[0], "first=1") || !strings.Contains(lines[1], "second=2") {
		t.Errorf("cookies lost across the copy-on-write: %q", lines)
	}
}
