package middleware

import (
	"bytes"
	"compress/gzip"
	"io"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/nelthaarion/breeze/v2"
)

// header_case_integration_test.go — regression tests that verify middlewares
// correctly read request headers produced by breeze.ParseHTTPRequest.
//
// These tests exist because of a HIGH-severity bug discovered in Phase 1.2
// profiling: middlewares read headers with mixed-case keys (e.g.
// ctx.Req.Header["Authorization"]) but ParseHTTPRequest lowercases ALL
// header keys (request.go, lowerHeaderKey). In production, every
// middleware header lookup missed — JWT always returned 401, compression
// never compressed, ETag never hit, CORS never saw the Origin.
//
// The existing unit tests (locale_test.go) passed because they set headers
// with the exact case the middleware expected, masking the bug. These
// integration tests build raw HTTP request bytes, parse them through
// ParseHTTPRequest, and run the middleware chain — exactly what production
// does. They would have caught the bug at submission time.
//
// Each test follows the same pattern:
//   1. Build a raw HTTP/1.1 request as []byte (what the kernel delivers).
//   2. Parse it with breeze.ParseHTTPRequest (lowercases header keys).
//   3. Build a Context from the parsed request.
//   4. Install the middleware under test + a stub handler.
//   5. Run ctx.Next().
//   6. Assert the middleware's observable behavior (status, body, headers).

// parseRequest is a helper that parses a raw HTTP request and returns a
// ready-to-use Context. It mirrors what OnTraffic does after receiving bytes
// from gnet. Fails the test if parsing fails.
//
// We use breeze.NewContext to get a Context with the right internal state
// (index = -1, non-nil Req.Header), then replace its Req with the parsed
// request. SetMiddlewareChain (called later by runChain) resets index = -1.
func parseRequest(t *testing.T, raw []byte) *breeze.Context {
	t.Helper()
	req, _, err := breeze.ParseHTTPRequest(raw)
	if err != nil {
		t.Fatalf("ParseHTTPRequest failed: %v\nraw:\n%s", err, raw)
	}
	if req == nil {
		t.Fatalf("ParseHTTPRequest returned nil request (incomplete?)\nraw:\n%s", raw)
	}
	ctx := breeze.NewContext(req.Method, req.Path)
	ctx.Req = req
	return ctx
}

// runChain installs middlewares + handler on ctx and runs the chain.
func runChain(ctx *breeze.Context, middlewares []breeze.HandlerFunc, handler breeze.HandlerFunc) {
	ctx.SetMiddlewareChain(middlewares, handler)
	ctx.Next()
}

// ─── JWT middleware ───────────────────────────────────────────────────────────

func TestJWT_HappyPathViaParseHTTPRequest(t *testing.T) {
	secret := "test-secret-key-32-bytes-long!!"
	// Build a valid HS256 token.
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"user_id": "alice",
		"role":    "admin",
	})
	tokenStr, err := token.SignedString([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}

	// Raw HTTP request with Authorization header using the canonical
	// mixed-case spelling that real HTTP clients send.
	raw := []byte("GET /api/protected HTTP/1.1\r\n" +
		"Host: example.com\r\n" +
		"Authorization: Bearer " + tokenStr + "\r\n" +
		"\r\n")

	ctx := parseRequest(t, raw)

	var handlerCalled bool
	var storedClaims any
	mw := JWTAuthMiddleware(JWTOptions{
		AccessSecret:  secret,
		SigningMethod: jwt.SigningMethodHS256,
	})
	runChain(ctx, []breeze.HandlerFunc{mw}, func(ctx *breeze.Context) error {
		handlerCalled = true
		storedClaims, _ = ctx.Get("user")
		return ctx.WriteString("ok")
	})

	if !handlerCalled {
		t.Fatalf(
			"handler was not called — JWT middleware rejected a valid token. status=%d body=%q",
			ctx.Res.Status,
			string(ctx.Res.Body),
		)
	}
	if ctx.Res.Status != 200 {
		t.Errorf("status = %d, want 200", ctx.Res.Status)
	}
	claims, ok := storedClaims.(jwt.MapClaims)
	if !ok {
		t.Fatalf("stored claims are not jwt.MapClaims: got %T", storedClaims)
	}
	if claims["user_id"] != "alice" {
		t.Errorf("claims[user_id] = %v, want alice", claims["user_id"])
	}
	if claims["role"] != "admin" {
		t.Errorf("claims[role] = %v, want admin", claims["role"])
	}
}

func TestJWT_MissingHeaderViaParseHTTPRequest(t *testing.T) {
	secret := "test-secret-key-32-bytes-long!!"
	raw := []byte("GET /api/protected HTTP/1.1\r\n" +
		"Host: example.com\r\n" +
		"\r\n")

	ctx := parseRequest(t, raw)

	var handlerCalled bool
	mw := JWTAuthMiddleware(JWTOptions{
		AccessSecret:  secret,
		SigningMethod: jwt.SigningMethodHS256,
	})
	runChain(ctx, []breeze.HandlerFunc{mw}, func(ctx *breeze.Context) error {
		handlerCalled = true

		return nil
	})

	if handlerCalled {
		t.Error("handler was called despite missing Authorization header")
	}
	if ctx.Res.Status != 401 {
		t.Errorf("status = %d, want 401", ctx.Res.Status)
	}
	if string(ctx.Res.Body) == "" {
		t.Error("expected non-empty 401 body")
	}
}

func TestJWT_InvalidTokenViaParseHTTPRequest(t *testing.T) {
	secret := "test-secret-key-32-bytes-long!!"
	raw := []byte("GET /api/protected HTTP/1.1\r\n" +
		"Host: example.com\r\n" +
		"Authorization: Bearer not.a.valid.token\r\n" +
		"\r\n")

	ctx := parseRequest(t, raw)

	var handlerCalled bool
	mw := JWTAuthMiddleware(JWTOptions{
		AccessSecret:  secret,
		SigningMethod: jwt.SigningMethodHS256,
	})
	runChain(ctx, []breeze.HandlerFunc{mw}, func(ctx *breeze.Context) error {
		handlerCalled = true

		return nil
	})

	if handlerCalled {
		t.Error("handler was called despite invalid token")
	}
	if ctx.Res.Status != 401 {
		t.Errorf("status = %d, want 401", ctx.Res.Status)
	}
}

func TestJWT_LowercaseHeaderKeyViaParseHTTPRequest(t *testing.T) {
	// Some clients send lowercase "authorization" directly. This should
	// also work (ParseHTTPRequest is idempotent on already-lowercase keys).
	secret := "test-secret-key-32-bytes-long!!"
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"user_id": "bob",
	})
	tokenStr, _ := token.SignedString([]byte(secret))

	raw := []byte("GET /api/protected HTTP/1.1\r\n" +
		"Host: example.com\r\n" +
		"authorization: Bearer " + tokenStr + "\r\n" +
		"\r\n")

	ctx := parseRequest(t, raw)

	var handlerCalled bool
	mw := JWTAuthMiddleware(JWTOptions{
		AccessSecret:  secret,
		SigningMethod: jwt.SigningMethodHS256,
	})
	runChain(ctx, []breeze.HandlerFunc{mw}, func(ctx *breeze.Context) error {
		handlerCalled = true
		return ctx.WriteString("ok")
	})

	if !handlerCalled {
		t.Fatalf("handler was not called despite valid lowercase authorization header")
	}
}

// ─── Compression middleware ───────────────────────────────────────────────────

func TestCompression_GzipViaParseHTTPRequest(t *testing.T) {
	originalBody := bytes.Repeat([]byte("Hello, World! "), 64) // ~960 bytes

	raw := []byte("GET /api/data HTTP/1.1\r\n" +
		"Host: example.com\r\n" +
		"Accept-Encoding: gzip\r\n" +
		"\r\n")

	ctx := parseRequest(t, raw)

	mw := CompressionMiddleware()
	runChain(ctx, []breeze.HandlerFunc{mw}, func(ctx *breeze.Context) error {
		return ctx.HTML(originalBody)
	})

	if ctx.Res == nil {
		t.Fatal("no response")
	}
	if enc := ctx.GetHeader("Content-Encoding"); enc != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip", enc)
	}
	if len(ctx.Res.Body) >= len(originalBody) {
		t.Errorf("compressed body (%d B) should be smaller than original (%d B)",
			len(ctx.Res.Body), len(originalBody))
	}
	// Verify the body is valid gzip by decompressing.
	gr, err := gzip.NewReader(bytes.NewReader(ctx.Res.Body))
	if err != nil {
		t.Fatalf("gzip.NewReader failed: %v", err)
	}
	decompressed, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("gzip read failed: %v", err)
	}
	if !bytes.Equal(decompressed, originalBody) {
		t.Errorf("decompressed body does not match original")
	}
}

func TestCompression_NoAcceptEncodingViaParseHTTPRequest(t *testing.T) {
	originalBody := []byte("Hello, World!")

	raw := []byte("GET /api/data HTTP/1.1\r\n" +
		"Host: example.com\r\n" +
		"\r\n")

	ctx := parseRequest(t, raw)

	mw := CompressionMiddleware()
	runChain(ctx, []breeze.HandlerFunc{mw}, func(ctx *breeze.Context) error {
		return ctx.HTML(originalBody)
	})

	if ctx.Res == nil {
		t.Fatal("no response")
	}
	if enc := ctx.GetHeader("Content-Encoding"); enc != "" {
		t.Errorf("Content-Encoding = %q, expected no compression without Accept-Encoding", enc)
	}
	if !bytes.Equal(ctx.Res.Body, originalBody) {
		t.Errorf("body was modified despite no Accept-Encoding")
	}
}

func TestCompression_BrotliViaParseHTTPRequest(t *testing.T) {
	originalBody := bytes.Repeat([]byte("compressible-content-"), 50)

	raw := []byte("GET /api/data HTTP/1.1\r\n" +
		"Host: example.com\r\n" +
		"Accept-Encoding: br\r\n" +
		"\r\n")

	ctx := parseRequest(t, raw)

	mw := CompressionMiddleware()
	runChain(ctx, []breeze.HandlerFunc{mw}, func(ctx *breeze.Context) error {
		return ctx.HTML(originalBody)
	})

	if ctx.Res == nil {
		t.Fatal("no response")
	}
	if enc := ctx.GetHeader("Content-Encoding"); enc != "br" {
		t.Errorf("Content-Encoding = %q, want br", enc)
	}
	if len(ctx.Res.Body) >= len(originalBody) {
		t.Errorf("compressed body (%d B) should be smaller than original (%d B)",
			len(ctx.Res.Body), len(originalBody))
	}
}

// ─── ETag cache middleware ────────────────────────────────────────────────────

func TestETag_FirstRequestSetsETagViaParseHTTPRequest(t *testing.T) {
	cache := NewETagCache()
	raw := []byte("GET /api/data HTTP/1.1\r\n" +
		"Host: example.com\r\n" +
		"\r\n")

	ctx := parseRequest(t, raw)

	mw := cache.ETagMiddleware()
	runChain(ctx, []breeze.HandlerFunc{mw}, func(ctx *breeze.Context) error {
		return ctx.WriteString("version-1-body")
	})

	if ctx.Res == nil {
		t.Fatal("no response")
	}
	etag := ctx.GetHeader("ETag")
	if etag == "" {
		t.Fatal("ETag header not set on first request")
	}
	if ctx.Res.Status != 200 {
		t.Errorf("status = %d, want 200 (first request should return full body)", ctx.Res.Status)
	}
	if string(ctx.Res.Body) != "version-1-body" {
		t.Errorf("body = %q, want version-1-body", string(ctx.Res.Body))
	}
}

func TestETag_IfNoneMatchReturns304ViaParseHTTPRequest(t *testing.T) {
	cache := NewETagCache()

	// First request: compute and store the ETag.
	raw1 := []byte("GET /api/data HTTP/1.1\r\n" +
		"Host: example.com\r\n" +
		"\r\n")
	ctx1 := parseRequest(t, raw1)
	mw := cache.ETagMiddleware()
	runChain(ctx1, []breeze.HandlerFunc{mw}, func(ctx *breeze.Context) error {
		return ctx.WriteString("version-1-body")
	})
	etag := ctx1.GetHeader("ETag")
	if etag == "" {
		t.Fatal("first request did not set ETag")
	}

	// Second request with If-None-Match matching the stored ETag.
	raw2 := []byte("GET /api/data HTTP/1.1\r\n" +
		"Host: example.com\r\n" +
		"If-None-Match: " + etag + "\r\n" +
		"\r\n")
	ctx2 := parseRequest(t, raw2)
	runChain(ctx2, []breeze.HandlerFunc{mw}, func(ctx *breeze.Context) error {
		return ctx.WriteString("version-1-body")
	})

	if ctx2.Res.Status != 304 {
		t.Errorf("status = %d, want 304 (If-None-Match matched)", ctx2.Res.Status)
	}
	if len(ctx2.Res.Body) != 0 {
		t.Errorf("304 response should have empty body, got %q", string(ctx2.Res.Body))
	}
}

func TestETag_IfNoneMatchMismatchReturns200ViaParseHTTPRequest(t *testing.T) {
	cache := NewETagCache()

	// First request stores ETag for body "v1".
	raw1 := []byte("GET /api/data HTTP/1.1\r\nHost: example.com\r\n\r\n")
	ctx1 := parseRequest(t, raw1)
	mw := cache.ETagMiddleware()
	runChain(ctx1, []breeze.HandlerFunc{mw}, func(ctx *breeze.Context) error {
		return ctx.WriteString("v1")
	})

	// Second request with non-matching If-None-Match.
	raw2 := []byte("GET /api/data HTTP/1.1\r\n" +
		"Host: example.com\r\n" +
		"If-None-Match: \"stale-etag-that-does-not-match\"\r\n" +
		"\r\n")
	ctx2 := parseRequest(t, raw2)
	runChain(ctx2, []breeze.HandlerFunc{mw}, func(ctx *breeze.Context) error {
		return ctx.WriteString("v1")
	})

	if ctx2.Res.Status != 200 {
		t.Errorf("status = %d, want 200 (If-None-Match did not match)", ctx2.Res.Status)
	}
}

// ─── CORS middleware ──────────────────────────────────────────────────────────

func TestCORS_OptionsPreflightViaParseHTTPRequest(t *testing.T) {
	opts := CORSOptions{
		AllowOrigins:     "*",
		AllowMethods:     "GET, POST, OPTIONS",
		AllowHeaders:     "Content-Type, Authorization",
		AllowCredentials: "true",
		MaxAge:           "3600",
	}
	raw := []byte("OPTIONS /api/data HTTP/1.1\r\n" +
		"Host: example.com\r\n" +
		"Origin: https://example.com\r\n" +
		"\r\n")

	ctx := parseRequest(t, raw)

	var handlerCalled bool
	mw := CORSMiddleware(opts)
	runChain(ctx, []breeze.HandlerFunc{mw}, func(ctx *breeze.Context) error {
		handlerCalled = true

		return nil
	})

	// CORS should short-circuit OPTIONS with 204.
	if handlerCalled {
		t.Error("handler was called for OPTIONS preflight — CORS should have short-circuited")
	}
	if ctx.Res.Status != 204 {
		t.Errorf("status = %d, want 204", ctx.Res.Status)
	}
	if got := ctx.GetHeader("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
	}
	if got := ctx.GetHeader("Access-Control-Allow-Methods"); got != "GET, POST, OPTIONS" {
		t.Errorf("Access-Control-Allow-Methods = %q, want 'GET, POST, OPTIONS'", got)
	}
	if got := ctx.GetHeader("Access-Control-Allow-Headers"); got != "Content-Type, Authorization" {
		t.Errorf("Access-Control-Allow-Headers = %q, want 'Content-Type, Authorization'", got)
	}
}

func TestCORS_GetRequestSetsHeadersViaParseHTTPRequest(t *testing.T) {
	// NOTE: The CORS middleware sets headers BEFORE ctx.Next(). If the
	// handler calls ctx.WriteString/JSON/HTML, those methods replace
	// ctx.Res entirely, discarding the CORS headers. This is a pre-existing
	// CORS ordering bug (separate from the header-case fix) — CORS should
	// set headers AFTER ctx.Next() to survive handler response replacement.
	//
	// For this regression test, we verify the header-case fix by using a
	// handler that does NOT replace ctx.Res. The CORS headers should then
	// be visible on the response.
	opts := CORSOptions{
		AllowOrigins: "https://example.com",
		AllowMethods: "GET, POST",
	}
	raw := []byte("GET /api/data HTTP/1.1\r\n" +
		"Host: example.com\r\n" +
		"Origin: https://example.com\r\n" +
		"\r\n")

	ctx := parseRequest(t, raw)

	var handlerCalled bool
	mw := CORSMiddleware(opts)
	// Handler does NOT call WriteString/JSON/HTML — it leaves ctx.Res
	// alone so the CORS headers survive.
	runChain(ctx, []breeze.HandlerFunc{mw}, func(ctx *breeze.Context) error {
		handlerCalled = true

		return nil
	})

	if !handlerCalled {
		t.Error("handler was not called for non-OPTIONS request")
	}
	if ctx.Res == nil {
		t.Fatal("no response — CORS middleware should have created one via SetHeader")
	}
	if got := ctx.GetHeader("Access-Control-Allow-Origin"); got != "https://example.com" {
		t.Errorf("Access-Control-Allow-Origin = %q, want https://example.com", got)
	}
}

// ─── Expired token (regression for exp claim validation) ──────────────────────

func TestJWT_ExpiredTokenViaParseHTTPRequest(t *testing.T) {
	secret := "test-secret-key-32-bytes-long!!"
	// Build a token that expired 1 hour ago.
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"user_id": "alice",
		"exp":     time.Now().Add(-1 * time.Hour).Unix(),
	})
	tokenStr, _ := token.SignedString([]byte(secret))

	raw := []byte("GET /api/protected HTTP/1.1\r\n" +
		"Host: example.com\r\n" +
		"Authorization: Bearer " + tokenStr + "\r\n" +
		"\r\n")

	ctx := parseRequest(t, raw)

	var handlerCalled bool
	mw := JWTAuthMiddleware(JWTOptions{
		AccessSecret:  secret,
		SigningMethod: jwt.SigningMethodHS256,
	})
	runChain(ctx, []breeze.HandlerFunc{mw}, func(ctx *breeze.Context) error {
		handlerCalled = true

		return nil
	})

	if handlerCalled {
		t.Error("handler was called despite expired token")
	}
	if ctx.Res.Status != 401 {
		t.Errorf("status = %d, want 401 for expired token", ctx.Res.Status)
	}
}
