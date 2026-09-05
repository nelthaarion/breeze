package rpc

import json "github.com/goccy/go-json"

// Version is the only value the "jsonrpc" member may take (spec §4).
const Version = "2.0"

// Standard error codes (spec §5.1).
//
// -32768..-32000 is reserved for pre-defined errors. Everything outside that
// window is available to applications; see ErrorCodeReserved.
const (
	CodeParseError     = -32700 // Invalid JSON was received by the server.
	CodeInvalidRequest = -32600 // The JSON sent is not a valid Request object.
	CodeMethodNotFound = -32601 // The method does not exist / is not available.
	CodeInvalidParams  = -32602 // Invalid method parameter(s).
	CodeInternalError  = -32603 // Internal JSON-RPC error.
)

// The reserved range for pre-defined errors (spec §5.1). -32099..-32000 within
// it is set aside for implementation-defined server errors, which is where a
// transport-level or framework-level condition belongs; application errors
// should sit outside the reserved range entirely.
const (
	CodeServerErrorMin = -32099
	CodeServerErrorMax = -32000
	CodeReservedMin    = -32768
	CodeReservedMax    = -32000
)

// ErrorCodeReserved reports whether code falls in the range the specification
// reserves for pre-defined errors.
//
// It is exported because the interesting question for an application author is
// the inverse: whether a code they picked is safe to use. A code that answers
// true here may collide with a future revision of the specification.
func ErrorCodeReserved(code int) bool {
	return code >= CodeReservedMin && code <= CodeReservedMax
}

// Request is a decoded JSON-RPC request or notification.
//
// ID is kept as raw JSON rather than a concrete type because the specification
// allows a String, a Number, or (discouraged) Null, and requires the response
// to echo the same value back. Decoding it into any would turn 1 into a float64
// and echo 1.0, which is a different member value than the client sent. Holding
// the original bytes makes the echo exact and costs no allocation.
//
// A nil ID means the "id" member was absent, which by §4.1 makes the message a
// notification. That is distinct from an ID of literal null, which is a request
// whose id is null — the bytes "null" are present in that case.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
}

// IsNotification reports whether r omits the "id" member and therefore must not
// be replied to (spec §4.1).
func (r *Request) IsNotification() bool { return len(r.ID) == 0 }

// Error is the error member of a failed response (spec §5).
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Error implements the error interface so a handler can return an *Error
// directly, and so errors.As can pull one out of a wrapped chain.
func (e *Error) Error() string { return e.Message }

// NewError builds an *Error with no data member.
func NewError(code int, message string) *Error {
	return &Error{Code: code, Message: message}
}

// NewErrorData builds an *Error carrying an additional data member, which the
// specification leaves entirely to the server to define.
func NewErrorData(code int, message string, data any) *Error {
	return &Error{Code: code, Message: message, Data: data}
}

// Canonical messages for the standard codes. The specification supplies these
// strings, so they are package-level rather than formatted per response — a
// method-not-found reply allocates nothing for its message.
const (
	msgParseError     = "Parse error"
	msgInvalidRequest = "Invalid Request"
	msgMethodNotFound = "Method not found"
	msgInvalidParams  = "Invalid params"
	msgInternalError  = "Internal error"
)

// Prebuilt errors for the standard codes. These are returned by value-copying
// constructors below rather than shared by pointer, because a caller may attach
// a Data member to what they were handed.
func errParseError() *Error { return &Error{Code: CodeParseError, Message: msgParseError} }

func errInvalidRequest() *Error { return &Error{Code: CodeInvalidRequest, Message: msgInvalidRequest} }

func errMethodNotFound() *Error { return &Error{Code: CodeMethodNotFound, Message: msgMethodNotFound} }

func errInvalidParams() *Error { return &Error{Code: CodeInvalidParams, Message: msgInvalidParams} }

func errInternalError() *Error { return &Error{Code: CodeInternalError, Message: msgInternalError} }

// ErrParseError returns a -32700 error.
func ErrParseError() *Error { return errParseError() }

// ErrInvalidRequest returns a -32600 error.
func ErrInvalidRequest() *Error { return errInvalidRequest() }

// ErrMethodNotFound returns a -32601 error.
func ErrMethodNotFound() *Error { return errMethodNotFound() }

// ErrInvalidParams returns a -32602 error.
func ErrInvalidParams() *Error { return errInvalidParams() }

// ErrInternalError returns a -32603 error.
func ErrInternalError() *Error { return errInternalError() }

// Response is a JSON-RPC response object (spec §5).
//
// Result and Error are mutually exclusive and exactly one must be present. That
// invariant is why neither is exported as a plain field on the hot path: the
// serializer in wire.go writes one member or the other, so a Response with both
// set cannot be produced by this package.
//
// Result is raw JSON so an already-encoded result can be forwarded without a
// decode/re-encode round trip.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
	ID      json.RawMessage `json:"id"`
}

// nullID is the id member used when a request could not be parsed well enough
// to recover its id (spec §5: "If there was an error in detecting the id in the
// Request object (e.g. Parse error/Invalid Request), it MUST be Null.").
var nullID = json.RawMessage("null")
