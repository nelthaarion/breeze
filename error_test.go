package breeze

// error_test.go — Part 1: a handler's returned error becomes a real response.
//
// The signature change is only worth anything if the error reliably reaches the wire.
// These tests are the gate on that: each one asserts the *response*, not that some
// error-handling function was called.

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// runChainFor builds a one-route app, dispatches a request through the real chain
// runner, and returns the response the client would receive.
//
// It goes through Breeze.handleChainError rather than calling the handler directly:
// the thing under test is the wiring between a returned error and a response, and
// calling the handler would test neither half of that.
func runChainFor(t *testing.T, app *Breeze, handler HandlerFunc, mws ...HandlerFunc) *HTTPResponse {
	t.Helper()

	ctx := NewContext(GET, "/test")
	ctx.SetMiddlewareChain(mws, handler)

	if err := ctx.Next(); err != nil {
		app.handleChainError(ctx, err)
	}
	return ctx.Res
}

// decodeProblem reads an RFC 9457 body, failing the test if it is not one.
func decodeProblem(t *testing.T, res *HTTPResponse) map[string]any {
	t.Helper()

	if res == nil {
		t.Fatal("no response was written at all")
	}
	var body map[string]any
	if err := json.Unmarshal(res.Body, &body); err != nil {
		t.Fatalf("the error body is not JSON: %v\n%s", err, res.Body)
	}
	return body
}

// TestReturnedErrorProducesA500 is the base case: an undescribed failure is a 500, and
// the handler's message is not sent to the client.
func TestReturnedErrorProducesA500(t *testing.T) {
	app := &Breeze{}

	res := runChainFor(t, app, func(ctx *Context) error {
		return errors.New("the database at db-01.internal refused the connection")
	})

	if res.Status != 500 {
		t.Errorf("status = %d, want 500", res.Status)
	}
	// The point of the default handler: an arbitrary error's text routinely names
	// hosts, paths and queries, so it is logged rather than transmitted.
	if strings.Contains(string(res.Body), "db-01.internal") {
		t.Errorf("the error's internal detail was sent to the client:\n%s", res.Body)
	}

	body := decodeProblem(t, res)
	if body["status"] != float64(500) {
		t.Errorf("body status = %v, want 500", body["status"])
	}
	if body["title"] != "Internal Server Error" {
		t.Errorf("body title = %v", body["title"])
	}
}

// TestHTTPErrorControlsTheStatus — a handler cannot say "404" with a bare error, and
// the framework must not guess. HTTPError is how it says so.
func TestHTTPErrorControlsTheStatus(t *testing.T) {
	app := &Breeze{}

	res := runChainFor(t, app, func(ctx *Context) error {
		return NewHTTPError(404, "no such order")
	})

	if res.Status != 404 {
		t.Errorf("status = %d, want 404", res.Status)
	}
	body := decodeProblem(t, res)
	// A deliberate message is sent: the handler chose it for a client to read.
	if body["detail"] != "no such order" {
		t.Errorf("detail = %v, want the handler's message", body["detail"])
	}
}

// TestWrappedHTTPErrorHidesTheCause — the two fields exist so a handler can report a
// status to the client and a cause to the log without conflating them.
func TestWrappedHTTPErrorHidesTheCause(t *testing.T) {
	app := &Breeze{}

	cause := errors.New("pq: relation \"orders\" does not exist")
	res := runChainFor(t, app, func(ctx *Context) error {
		return WrapHTTPError(502, "the order service is unavailable", cause)
	})

	if res.Status != 502 {
		t.Errorf("status = %d, want 502", res.Status)
	}
	if strings.Contains(string(res.Body), "relation") {
		t.Errorf("the wrapped cause reached the client:\n%s", res.Body)
	}
	body := decodeProblem(t, res)
	if body["detail"] != "the order service is unavailable" {
		t.Errorf("detail = %v", body["detail"])
	}
}

// TestHTTPErrorUnwraps — errors.Is and errors.As have to see through an HTTPError, or
// wrapping a sentinel makes it undetectable.
func TestHTTPErrorUnwraps(t *testing.T) {
	sentinel := errors.New("not found in store")
	err := WrapHTTPError(404, "no such order", sentinel)

	if !errors.Is(err, sentinel) {
		t.Error("errors.Is cannot see the wrapped cause")
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != 404 {
		t.Error("errors.As did not recover the HTTPError")
	}
}

// TestBindErrorStaysA422 — the shape a handler is most likely to write is
//
//	if err := ctx.Bind(&in); err != nil { return err }
//
// and it must not downgrade Bind's field-level 422 into a generic 500. Without the
// ValidationError branch in defaultErrorHandler, that is exactly what would happen.
func TestBindErrorStaysA422(t *testing.T) {
	app := &Breeze{}

	res := runChainFor(t, app, func(ctx *Context) error {
		var in struct {
			Email string `json:"email" validate:"required,email"`
		}
		ctx.Req.Body = []byte(`{"email":"not-an-email"}`)
		if err := ctx.Bind(&in); err != nil {
			return err
		}
		return ctx.JSON(in)
	})

	if res.Status != 422 {
		t.Fatalf("status = %d, want 422 — a validation failure became something else", res.Status)
	}
	// Bind's own body survives: it names the failing field, which a generic 422 does
	// not, and that is the whole value of the branch.
	if !strings.Contains(string(res.Body), "email") {
		t.Errorf("the field-level detail was replaced:\n%s", res.Body)
	}
}

// TestMiddlewareErrorStopsTheChain — a middleware returning an error must prevent the
// handler running at all, and must still produce a response.
func TestMiddlewareErrorStopsTheChain(t *testing.T) {
	app := &Breeze{}
	handlerRan := false

	res := runChainFor(t, app,
		func(ctx *Context) error {
			handlerRan = true
			return ctx.WriteString("this must never be sent")
		},
		func(ctx *Context) error {
			return NewHTTPError(401, "unauthorized")
		},
	)

	if handlerRan {
		t.Error("the handler ran even though a middleware failed before it")
	}
	if res.Status != 401 {
		t.Errorf("status = %d, want 401", res.Status)
	}
	if strings.Contains(string(res.Body), "must never be sent") {
		t.Errorf("the handler's body reached the client:\n%s", res.Body)
	}
}

// TestMiddlewarePropagatesAHandlerError — the other direction: a middleware that passes
// Next's error through must not turn a failure into a success.
func TestMiddlewarePropagatesAHandlerError(t *testing.T) {
	app := &Breeze{}
	after := false

	res := runChainFor(t, app,
		func(ctx *Context) error {
			return NewHTTPError(503, "downstream is down")
		},
		func(ctx *Context) error {
			err := ctx.Next()
			after = true // the middleware still gets to run its tail
			return err
		},
	)

	if !after {
		t.Error("the middleware's post-Next code did not run")
	}
	if res.Status != 503 {
		t.Errorf("status = %d, want 503 — the middleware swallowed the handler's error", res.Status)
	}
}

// TestCustomErrorHandlerReplacesTheDefault — an application that wants its own error
// format sets one field, and nothing else in the framework needs to know.
func TestCustomErrorHandlerReplacesTheDefault(t *testing.T) {
	app := &Breeze{
		ErrorHandler: func(ctx *Context, err error) {
			ctx.Status(418)
			_ = ctx.JSON(map[string]string{"custom": err.Error()})
		},
	}

	res := runChainFor(t, app, func(ctx *Context) error {
		return errors.New("boom")
	})

	if res.Status != 418 {
		t.Errorf("status = %d, want the custom handler's 418", res.Status)
	}
	if !strings.Contains(string(res.Body), `"custom"`) {
		t.Errorf("the custom body was not used:\n%s", res.Body)
	}
}

// TestErrorHandlerThatWritesNothingStillProducesAResponse is the guard that makes the
// whole design honest.
//
// A replaceable hook that can decline to write would mean an error could still reach
// the wire as nothing at all — a hung request with no explanation, which is precisely
// the failure Part 1 exists to remove. handleChainError corrects it rather than
// trusting the hook.
func TestErrorHandlerThatWritesNothingStillProducesAResponse(t *testing.T) {
	app := &Breeze{
		ErrorHandler: func(ctx *Context, err error) {
			// Deliberately writes nothing.
		},
	}

	res := runChainFor(t, app, func(ctx *Context) error {
		return errors.New("boom")
	})

	if res == nil {
		t.Fatal("an ErrorHandler that wrote nothing produced no response at all")
	}
	if res.Status != 500 {
		t.Errorf("status = %d, want the corrected 500", res.Status)
	}
}

// TestNilErrorHandlerUsesTheDefault — a zero-valued Breeze is constructible, and a test
// or embedding that builds one must not silently lose responses.
func TestNilErrorHandlerUsesTheDefault(t *testing.T) {
	app := &Breeze{} // ErrorHandler is nil

	res := runChainFor(t, app, func(ctx *Context) error {
		return NewHTTPError(403, "forbidden")
	})

	if res == nil || res.Status != 403 {
		t.Fatalf("a nil ErrorHandler did not fall back to the default: %+v", res)
	}
}

// TestSuccessfulHandlerIsUnaffected — the signature change must not alter the ordinary
// path. A handler returning nil after writing behaves exactly as it did before.
func TestSuccessfulHandlerIsUnaffected(t *testing.T) {
	app := &Breeze{}

	res := runChainFor(t, app, func(ctx *Context) error {
		return ctx.JSON(map[string]string{"ok": "yes"})
	})

	if res.Status != 200 {
		t.Errorf("status = %d, want 200", res.Status)
	}
	if !strings.Contains(string(res.Body), `"ok"`) {
		t.Errorf("body = %s", res.Body)
	}
}

// TestJSONReturnsItsMarshalError — the old behaviour was a 400 blaming the client for
// the server's unmarshallable value, with the real cause discarded. The 400 is kept for
// a caller that ignores the return, and the error is now available to one that does not.
func TestJSONReturnsItsMarshalError(t *testing.T) {
	ctx := NewContext(GET, "/")

	// A channel cannot be marshalled.
	err := ctx.JSON(map[string]any{"bad": make(chan int)})
	if err == nil {
		t.Fatal("JSON returned nil for an unmarshallable value")
	}
	// And it still wrote something, so an ignoring caller is no worse off.
	if ctx.Res == nil || ctx.Res.Status != 400 {
		t.Errorf("the fallback body was not written: %+v", ctx.Res)
	}
}
