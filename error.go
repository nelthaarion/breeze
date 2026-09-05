package breeze

// error.go — what happens when a handler returns an error.
//
// # Why this exists
//
// HandlerFunc returning an error is only an improvement if the error reliably
// becomes a response. An error that is returned and then dropped is worse than no
// error return at all: the handler believes it reported a failure, the client sees
// a 200 with an empty body or a hung connection, and nothing in between says so.
//
// So there is exactly one function that turns an error into a response, it is
// called from exactly one place in each chain runner, and it cannot decline to
// write. A caller may replace it; it may not remove it.
//
// # Why not reuse Bind's problem+json path
//
// Bind already writes RFC 9457 problem+json for a validation failure, and this
// deliberately matches that shape rather than inventing a second error format — a
// client parsing one should not need a second parser for the other. What it does
// not do is reuse Bind's *code*: Bind writes a 422 or 400 for a known, structured
// cause it holds in hand, while this handles an arbitrary error from anywhere in
// the chain. Sharing the implementation would mean one of the two callers getting
// behaviour tuned for the other.
//
// # Why the default hides the message
//
// A returned error is frequently a database driver's, or a file path, or a
// third-party client's — none of which a remote caller should read. The default
// therefore logs the detail and returns a generic body. An application that wants
// to expose more replaces ErrorHandler, which is a decision it makes explicitly
// rather than one this framework makes on its behalf.

import (
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/nelthaarion/breeze/binding"
)

// ErrorHandler turns an error returned by a handler or middleware into a
// response.
//
// It is called with the same Context the failing handler had, so it can read
// anything the chain already set — a status, a header, a partially built body —
// and it runs after the chain has stopped.
//
// An implementation must leave ctx.Res non-nil. One that does not is corrected by
// the caller rather than trusted, because the alternative is a connection that
// receives nothing at all.
type ErrorHandler func(ctx *Context, err error)

// HTTPError is an error carrying the status code it should produce.
//
// This is how a handler says "404" rather than "something went wrong": the
// framework has no way to infer intent from an arbitrary error value, and guessing
// would turn a deliberate 404 into a 500 or the reverse.
//
//	return breeze.NewHTTPError(404, "no such order")
//
// Message is sent to the client, so it must not carry anything internal. Err is the
// underlying cause: it is logged, wrapped for errors.Is/As, and never transmitted.
type HTTPError struct {
	Status  int
	Message string
	Err     error
}

// NewHTTPError builds an HTTPError with no wrapped cause.
func NewHTTPError(status int, message string) *HTTPError {
	return &HTTPError{Status: status, Message: message}
}

// WrapHTTPError builds an HTTPError around an existing error.
//
// The cause is preserved for errors.Is/As and for the log, and is not sent to the
// client — which is the entire reason the two are separate fields.
func WrapHTTPError(status int, message string, err error) *HTTPError {
	return &HTTPError{Status: status, Message: message, Err: err}
}

func (e *HTTPError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%d %s: %v", e.Status, e.Message, e.Err)
	}
	return fmt.Sprintf("%d %s", e.Status, e.Message)
}

// Unwrap exposes the cause, so errors.Is and errors.As see through an HTTPError.
func (e *HTTPError) Unwrap() error { return e.Err }

// defaultErrorHandler is the ErrorHandler used when an application sets none.
//
// # What it discloses, and what it does not
//
// An *HTTPError is a status and message the handler chose deliberately, so both are
// sent. Anything else is a failure the handler did not describe — a driver error, an
// I/O error, a wrapped third-party error — and its text is logged but not
// transmitted: those strings routinely name hosts, file paths and query fragments,
// and a framework that forwarded them by default would be leaking by default.
//
// A *binding.ValidationError is recognised so that a handler which returns Bind's
// error rather than handling it still produces the 422 problem+json Bind itself
// would have written. Without this, the idiomatic-looking
//
//	if err := ctx.Bind(&in); err != nil { return err }
//
// would downgrade a precise field-level 422 into a generic 500.
func defaultErrorHandler(ctx *Context, err error) {
	if err == nil {
		return
	}

	// Bind already wrote a problem+json body before returning; re-writing it would
	// replace a field-level explanation with a generic one.
	var verr *binding.ValidationError
	if errors.As(err, &verr) {
		if ctx.Res != nil && len(ctx.Res.Body) > 0 {
			return
		}
		ctx.Status(http.StatusUnprocessableEntity)
		_ = ctx.JSON(verr.ToProblemJSON())
		return
	}

	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		status := httpErr.Status
		if status < 100 || status > 599 {
			// A status outside the HTTP range would produce an unparseable status
			// line, so the response would fail rather than be wrong legibly.
			status = http.StatusInternalServerError
		}
		if httpErr.Err != nil {
			logHandlerError(ctx, httpErr.Err)
		}
		ctx.Status(status)
		_ = ctx.JSON(problemBody(status, httpErr.Message))
		return
	}

	logHandlerError(ctx, err)
	ctx.Status(http.StatusInternalServerError)
	_ = ctx.JSON(problemBody(http.StatusInternalServerError, "Internal Server Error"))
}

// problemBody is the RFC 9457 shape Bind already uses, so a client has one error
// format to parse rather than two.
func problemBody(status int, detail string) map[string]any {
	title := http.StatusText(status)
	if title == "" {
		title = "Error"
	}
	body := map[string]any{
		"type":   "about:blank",
		"title":  title,
		"status": status,
	}
	// Omitted when it would only repeat the title: that tells a reader nothing, and
	// makes every error response look like it carries detail when it does not.
	if detail != "" && detail != title {
		body["detail"] = detail
	}
	return body
}

// logHandlerError writes the cause where an operator can find it.
//
// stderr, matching the panic path in breeze.go, and with the method and path
// attached: an error message alone rarely identifies which request produced it.
func logHandlerError(ctx *Context, err error) {
	method, path := "?", "?"
	if ctx != nil && ctx.Req != nil {
		if ctx.Req.Method != "" {
			method = string(ctx.Req.Method)
		}
		if ctx.Req.Path != "" {
			path = ctx.Req.Path
		}
	}
	fmt.Fprintf(os.Stderr, "[Breeze][ERROR] %s %s: %v\n", method, path, err)
}

// handleChainError resolves a chain error into a response.
//
// Every path that runs a chain goes through this, so an error cannot reach the wire
// unanswered. The two guards below are why it is a function rather than an inline
// call:
//
//   - a nil ErrorHandler falls back to the default rather than dropping the error,
//     because a zero-valued Breeze is constructible and a test that built one by
//     hand should not silently lose responses;
//   - a handler that returns without writing anything is corrected to a 500,
//     because the contract this file exists to enforce is that a non-nil error
//     always produces a response.
func (s *Breeze) handleChainError(ctx *Context, err error) {
	if err == nil {
		return
	}

	handler := s.ErrorHandler
	if handler == nil {
		handler = defaultErrorHandler
	}
	handler(ctx, err)

	if ctx.Res == nil || (ctx.Res.Status == 0 && len(ctx.Res.Body) == 0) {
		ctx.Status(http.StatusInternalServerError)
		_ = ctx.JSON(problemBody(http.StatusInternalServerError, "Internal Server Error"))
	}
}
