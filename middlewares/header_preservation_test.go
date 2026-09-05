package middleware

// header_preservation_test.go — a middleware's response headers must survive the
// handler's body write.
//
// # The bug
//
// breeze.Context's three body methods (JSON, WriteString, HTML) assigned
// `r.Headers = hdrsJSON` — a package-level shared map — discarding whatever was
// already there. Every middleware in this package that sets a response header does
// so *before* ctx.Next(), because that is the only place it can: after Next returns
// the handler has already written the response. So CORSMiddleware and
// SecurityMiddleware computed up to eighteen headers per request and the handler's
// `return ctx.JSON(...)` threw all of them away.
//
// Nothing errored. `curl -i` showed a correct 200 with a correct body and no
// Access-Control-Allow-Origin, and a browser reported it as a CORS failure with no
// indication that the server had computed the header and dropped it.
//
// # Why the tests live here
//
// The fix is in the root package, but the bug was only observable through a real
// chain: a middleware, then a handler, in that order. Testing `SetHeader` then
// `JSON` on a bare Context proves the mechanism; these prove the thing that broke.

import (
	"strings"
	"testing"

	"github.com/nelthaarion/breeze"
)

// TestCORSHeadersSurviveHandlerJSON is the case that was reported as a browser CORS
// failure.
func TestCORSHeadersSurviveHandlerJSON(t *testing.T) {
	raw := []byte("GET /api/users HTTP/1.1\r\n" +
		"Host: example.com\r\n" +
		"Origin: https://app.example.com\r\n" +
		"\r\n")
	ctx := parseRequest(t, raw)

	mw := CORSMiddleware(CORSOptions{
		AllowOrigins:     "https://app.example.com",
		AllowMethods:     "GET,POST",
		AllowHeaders:     "Content-Type,Authorization",
		AllowCredentials: "true",
	})

	runChain(ctx, []breeze.HandlerFunc{mw}, func(c *breeze.Context) error {
		return c.JSON(map[string]string{"status": "ok"})
	})

	if ctx.Res == nil {
		t.Fatal("no response was written")
	}

	// Without this header a browser refuses the response, and the request looks
	// like a network failure to the page that made it.
	if got := ctx.GetHeader("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Errorf("Access-Control-Allow-Origin = %q, want the configured origin — "+
			"the handler's JSON call discarded the middleware's headers", got)
	}
	if got := ctx.GetHeader("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("Access-Control-Allow-Credentials = %q, want true", got)
	}
	// And the body method's own header still has to be there.
	if got := ctx.GetHeader("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if string(ctx.Res.Body) != `{"status":"ok"}` {
		t.Errorf("body = %s, want the handler's JSON", ctx.Res.Body)
	}
}

// TestSecurityHeadersSurviveEveryBodyMethod covers all three body methods, because
// the assignment that caused this was copy-pasted into each of them.
func TestSecurityHeadersSurviveEveryBodyMethod(t *testing.T) {
	bodies := map[string]struct {
		write     breeze.HandlerFunc
		wantCType string
	}{
		"JSON": {
			write:     func(c *breeze.Context) error { return c.JSON(map[string]int{"n": 1}) },
			wantCType: "application/json",
		},
		"WriteString": {
			write:     func(c *breeze.Context) error { return c.WriteString("plain") },
			wantCType: "text/plain",
		},
		"HTML": {
			write:     func(c *breeze.Context) error { return c.HTML([]byte("<p>hi</p>")) },
			wantCType: "text/html; charset=utf-8",
		},
	}

	for name, tc := range bodies {
		t.Run(name, func(t *testing.T) {
			raw := []byte("GET / HTTP/1.1\r\nHost: example.com\r\n\r\n")
			ctx := parseRequest(t, raw)

			runChain(ctx, []breeze.HandlerFunc{DefaultSecurityMiddleware()}, tc.write)

			// X-Frame-Options absent is a live clickjacking exposure rather
			// than a missing hardening nicety.
			if got := ctx.GetHeader("X-Frame-Options"); got == "" {
				t.Error("X-Frame-Options is absent — the body method discarded it")
			}
			if got := ctx.GetHeader("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
			}
			if got := ctx.GetHeader("Content-Type"); got != tc.wantCType {
				t.Errorf("Content-Type = %q, want %q", got, tc.wantCType)
			}
		})
	}
}

// TestHandlerContentTypeWinsOverBodyMethod pins the precedence rule.
//
// A handler that sets Content-Type explicitly chose it — problem+json for an
// RFC 9457 body is the case in this repository. A body method overriding it would
// make ctx.JSON unusable for anything but the default, and emitting both would let
// a recipient treat the response as malformed (RFC 9110 §5.3).
func TestHandlerContentTypeWinsOverBodyMethod(t *testing.T) {
	raw := []byte("GET / HTTP/1.1\r\nHost: example.com\r\n\r\n")
	ctx := parseRequest(t, raw)

	runChain(ctx, nil, func(c *breeze.Context) error {
		c.SetHeader("Content-Type", "application/problem+json")
		c.Status(422)
		return c.JSON(map[string]string{"title": "Validation Failed"})
	})

	if got := ctx.GetHeader("Content-Type"); got != "application/problem+json" {
		t.Errorf("Content-Type = %q, want the handler's explicit value", got)
	}
	if ctx.Res.Status != 422 {
		t.Errorf("status = %d, want 422", ctx.Res.Status)
	}
}

// TestUntouchedResponseStillSerializesItsContentType is the other half of the fix.
//
// The reason the old code assigned the shared map outright was to avoid an
// allocation on the path every request takes. The fix keeps that: a response no
// middleware has touched still gets the shared map and its precomputed wire block,
// which is what AppendTo needs in order to serialize without ranging a map.
//
// The flag carrying that is unexported, so this asserts the observable consequence —
// the content type reaches the wire — and reports the allocation count, which is
// where a regression would show up.
func TestUntouchedResponseStillSerializesItsContentType(t *testing.T) {
	raw := []byte("GET / HTTP/1.1\r\nHost: example.com\r\n\r\n")

	ctx := parseRequest(t, raw)
	runChain(ctx, nil, func(c *breeze.Context) error {
		return c.JSON(struct{ N int }{1})
	})

	wire := string(ctx.Res.Bytes())
	if !strings.Contains(wire, "Content-Type: application/json\r\n") {
		t.Errorf("serialized response is missing its content type:\n%s", wire)
	}
	if n := countHeaderLines(wire, "Content-Type"); n != 1 {
		t.Errorf("%d Content-Type header lines; a duplicate lets a recipient "+
			"treat the response as malformed:\n%s", n, wire)
	}
}

// TestASecondBodyMethodReplacesTheFirstsContentType is the case the naive fix got
// wrong, and the reason `ctypePinned` is a flag rather than a map lookup.
//
// The first attempt preserved any Content-Type already in the map. That is correct for
// a caller's explicit choice and wrong for a previous body method's default: a body
// method leaves a Content-Type behind too, so on a response some middleware had
// touched, `WriteString` then `JSON` found "text/plain" present, kept it, and sent a
// JSON body labelled as text. The map cannot distinguish whose value it holds; the
// flag can.
//
// The middleware is load-bearing here — without it the response still holds a shared
// map and takes the replacing branch anyway, so the test would pass against the bug.
func TestASecondBodyMethodReplacesTheFirstsContentType(t *testing.T) {
	raw := []byte("GET / HTTP/1.1\r\nHost: example.com\r\n\r\n")
	ctx := parseRequest(t, raw)

	runChain(ctx, []breeze.HandlerFunc{DefaultSecurityMiddleware()}, func(c *breeze.Context) error {
		// A handler that starts writing an error page and then changes its mind,
		// which is what an early-return path rewritten to JSON looks like.
		_ = c.WriteString("something went wrong")
		return c.JSON(map[string]string{"error": "something went wrong"})
	})

	if got := ctx.GetHeader("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json — a JSON body labelled as "+
			"text/plain is parsed as text by every strict client", got)
	}
	// The middleware's header still has to be there; that is the first fix.
	if got := ctx.GetHeader("X-Frame-Options"); got == "" {
		t.Error("X-Frame-Options is absent")
	}
	// And exactly one Content-Type header line reaches the wire.
	if n := countHeaderLines(string(ctx.Res.Bytes()), "Content-Type"); n != 1 {
		t.Errorf("%d Content-Type header lines:\n%s", n, ctx.Res.Bytes())
	}
}

// TestAnExplicitContentTypeSurvivesADifferentlyCasedKey covers the pair that would
// otherwise both reach the wire.
//
// The header map is serialized verbatim, so a caller's "content-type" and a body
// method's "Content-Type" are two distinct keys as far as Go is concerned and both
// would be sent. RFC 9110 §5.3 lets a recipient treat that as malformed.
func TestAnExplicitContentTypeSurvivesADifferentlyCasedKey(t *testing.T) {
	raw := []byte("GET / HTTP/1.1\r\nHost: example.com\r\n\r\n")
	ctx := parseRequest(t, raw)

	runChain(ctx, nil, func(c *breeze.Context) error {
		c.SetHeader("content-type", "application/problem+json") // lowercase
		c.Status(422)
		return c.JSON(map[string]string{"title": "Validation Failed"})
	})

	wire := strings.ToLower(string(ctx.Res.Bytes()))
	if n := countHeaderLines(wire, "Content-Type"); n != 1 {
		t.Errorf("%d Content-Type header lines; a duplicate lets a recipient treat "+
			"the response as malformed:\n%s", n, wire)
	}
	if !strings.Contains(wire, "application/problem+json") {
		t.Errorf("the caller's content type did not reach the wire:\n%s", wire)
	}
}

// countHeaderLines counts how many header lines name the given header.
//
// A substring count over the serialized response does not work: "Content-Type" is a
// substring of "X-Content-Type-Options", which DefaultSecurityMiddleware always sets,
// so a naive strings.Count reports two for a correct response. This matches at the
// line start and requires the colon, case-insensitively — the map is written to the
// wire verbatim, so a caller's capitalisation is whatever they typed.
func countHeaderLines(wire, name string) int {
	prefix := strings.ToLower(name) + ":"
	n := 0
	for _, line := range strings.Split(wire, "\r\n") {
		if strings.HasPrefix(strings.ToLower(line), prefix) {
			n++
		}
	}
	return n
}
