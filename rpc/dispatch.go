package rpc

import (
	"fmt"
	"runtime/debug"

	json "github.com/goccy/go-json"
	"github.com/panjf2000/gnet/v2"
)

// dispatch.go — turning one framed JSON value into zero or more responses.
//
// The order in which failures are classified is fixed by spec §5.1 and is the
// part most easy to get subtly wrong:
//
//   - Bytes that are not valid JSON at all are -32700, and nothing else can be
//     said about them, so the id is null.
//   - Bytes that are valid JSON but not a valid Request object are -32600. That
//     covers a scalar where an object was expected, a missing or wrong
//     "jsonrpc" member, a missing or non-string "method", and a "params" member
//     that is not a structured value (§4.2).
//   - A well-formed request naming a method nobody registered is -32601.
//   - Parameters the handler cannot decode are -32602, raised by Context.Bind.
//   - A handler that panics is -32603, because the fault is the server's.
//
// Deciding between -32700 and -32600 needs to know whether the bytes were valid
// JSON, which is a second pass over them. That pass runs only after a decode has
// already failed, so a well-formed request never pays for it.

// methodProbe extracts just enough of a message to find out which methods it
// names, for the blocking pre-scan. See Server.messageNeedsWorker.
type methodProbe struct {
	Method string `json:"method"`
}

// idProbe recovers the id from a message whose full decode failed.
type idProbe struct {
	ID json.RawMessage `json:"id"`
}

// appendMessage appends the response to one framed JSON value onto buf.
//
// A notification, or a batch consisting only of notifications, appends nothing —
// §4.1 and §6 both require silence rather than an empty reply.
func (s *Server) appendMessage(buf []byte, msg []byte, conn gnet.Conn) []byte {
	v := trimLeadingSpace(msg)
	if len(v) == 0 {
		// The framer does not emit empty values, so this is unreachable from
		// the connection path. It is reachable from Handle, which accepts
		// caller-supplied bytes.
		return appendErrorResponse(buf, ErrInvalidRequest(), nullID)
	}
	if v[0] == '[' {
		return s.appendBatch(buf, v, conn)
	}
	out, _ := s.appendSingle(buf, v, conn)
	return out
}

// appendBatch handles a top-level array (spec §6).
func (s *Server) appendBatch(buf []byte, v []byte, conn gnet.Conn) []byte {
	var elems []json.RawMessage
	if err := json.Unmarshal(v, &elems); err != nil {
		if !json.Valid(v) {
			return appendErrorResponse(buf, ErrParseError(), nullID)
		}
		// Valid JSON, but not an array of values this server can iterate — for
		// instance a nested structure the decoder cannot map to raw elements.
		return appendErrorResponse(buf, ErrInvalidRequest(), nullID)
	}

	// "If the batch rpc call itself fails to be recognized [...] as an Array
	// with at least one value, the response from the Server MUST be a single
	// Response object" (§6). So an empty array is answered with a bare object,
	// not with an empty array.
	if len(elems) == 0 {
		return appendErrorResponse(buf, ErrInvalidRequest(), nullID)
	}

	// One grow for the whole batch. The estimate is deliberately rough — it
	// only has to avoid the repeated doubling that appending N responses into
	// a 2 KiB buffer would otherwise cause.
	buf = grow(buf, len(v)+len(elems)*64)

	start := len(buf)
	buf = append(buf, '[')
	written := 0

	for _, el := range elems {
		mark := len(buf)
		if written > 0 {
			buf = append(buf, ',')
		}
		var wrote bool
		buf, wrote = s.appendSingle(buf, el, conn)
		if !wrote {
			// A notification: roll back the separator as well as the (empty)
			// response, so the array does not grow a hole.
			buf = buf[:mark]
			continue
		}
		written++
	}

	if written == 0 {
		// Every element was a notification. §6: "If there are no Response
		// objects contained within the Response array as it is to be sent to
		// the client, the server MUST NOT return an empty Array and should
		// return nothing at all."
		return buf[:start]
	}
	return append(buf, ']')
}

// appendSingle handles one request object, whether standalone or a batch
// element, and reports whether it appended a response.
//
// A false return means the message was a valid notification and the caller must
// send nothing for it.
func (s *Server) appendSingle(buf []byte, v []byte, conn gnet.Conn) ([]byte, bool) {
	v = trimLeadingSpace(v)

	// A batch element may be any JSON value, including a scalar — `[1,2,3]` is
	// the specification's own example — so the object check happens here rather
	// than only at the top level.
	if len(v) == 0 || v[0] != '{' {
		if len(v) > 0 && json.Valid(v) {
			return appendErrorResponse(buf, ErrInvalidRequest(), nullID), true
		}
		return appendErrorResponse(buf, ErrParseError(), nullID), true
	}

	var req Request
	if err := json.Unmarshal(v, &req); err != nil {
		if !json.Valid(v) {
			return appendErrorResponse(buf, ErrParseError(), nullID), true
		}
		// Valid JSON whose members are the wrong types — the specification's
		// own invalid-request example, `{"jsonrpc":"2.0","method":1}`, lands
		// here because method is not a string.
		return appendErrorResponse(buf, ErrInvalidRequest(), recoverID(v)), true
	}

	// §4: the jsonrpc member MUST be exactly "2.0". A missing member decodes to
	// the empty string and fails the same check, which is the required outcome
	// for a version 1.0 request arriving at a 2.0 server.
	if req.JSONRPC != Version {
		return appendErrorResponse(buf, ErrInvalidRequest(), idOrNull(req.ID)), true
	}

	// §4: method is REQUIRED. An empty name is indistinguishable from an absent
	// member after decoding, and neither is a method anyone can register — the
	// registry rejects an empty name — so both are invalid requests.
	if req.Method == "" {
		return appendErrorResponse(buf, ErrInvalidRequest(), idOrNull(req.ID)), true
	}

	// §4.2: params, if present, MUST be a structured value — an Array or an
	// Object. A string or number here makes the object an invalid Request
	// rather than a request with bad parameters, so it is -32600 and not
	// -32602.
	if len(req.Params) > 0 && !isStructured(req.Params) {
		return appendErrorResponse(buf, ErrInvalidRequest(), idOrNull(req.ID)), true
	}

	notification := req.IsNotification()

	m, ok := s.reg.lookup(req.Method)
	if !ok {
		if notification {
			return buf, false
		}
		return appendErrorResponse(buf, ErrMethodNotFound(), req.ID), true
	}

	ctx := acquireContext()
	ctx.Conn = conn
	ctx.Method = req.Method
	ctx.Params = req.Params
	ctx.ID = req.ID
	ctx.middlewares = m.chain
	ctx.index = -1

	runChain(ctx)

	if notification {
		// The handler ran — a notification is a real call, it just has no
		// reply. Whatever it set on the context is discarded here.
		releaseContext(ctx)
		return buf, false
	}

	buf = appendResponse(buf, ctx)
	releaseContext(ctx)
	return buf, true
}

// runChain executes ctx's chain, converting a panic into -32603.
//
// A panicking handler must not take down the event loop it is running on, and it
// must not leave the client waiting: the specification has a code for exactly
// this situation, so the panic becomes an Internal error response. The stack is
// printed in the same format the root package uses for a panicking HTTP handler,
// because the operator needs it and the client must not receive it.
func runChain(ctx *Context) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("[Breeze][RPC][PANIC] %v\n%s\n", r, debug.Stack())
			// Overwrite whatever the handler managed to set: a result built
			// half-way is not a result, and reporting it as success would be
			// worse than reporting the failure.
			ctx.result = nil
			ctx.resultValue = nil
			ctx.hasResult = false
			ctx.err = errInternalError()
		}
	}()
	ctx.Next()
}

// messageNeedsWorker reports whether msg names any method registered as
// blocking.
//
// It costs a decode of the method names, so it is only called when the registry
// actually holds a blocking method — the counter check in Server.onMessage. That
// gate mirrors the wsCount check at the top of Breeze.OnTraffic: a server with
// no blocking methods must not pay to ask a question whose answer is always no.
//
// A batch is deferred whole if any element blocks. Splitting it would mean
// writing part of a response array from the event loop and the rest from a
// worker, and the two halves could interleave with another connection's write.
func (s *Server) messageNeedsWorker(msg []byte) bool {
	v := trimLeadingSpace(msg)
	if len(v) == 0 {
		return false
	}

	if v[0] == '[' {
		var probes []methodProbe
		if err := json.Unmarshal(v, &probes); err != nil {
			// Undecodable input cannot reach a handler at all, so it is
			// answered inline with an error and never needs a worker.
			return false
		}
		for i := range probes {
			if s.isBlocking(probes[i].Method) {
				return true
			}
		}
		return false
	}

	var probe methodProbe
	if err := json.Unmarshal(v, &probe); err != nil {
		return false
	}
	return s.isBlocking(probe.Method)
}

func (s *Server) isBlocking(name string) bool {
	if name == "" {
		return false
	}
	m, ok := s.reg.lookup(name)
	return ok && m.blocking
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

// trimLeadingSpace drops JSON insignificant whitespace from the front of b.
//
// Only the front matters: every caller needs the first structural byte to
// decide how to treat the value, and the decoder tolerates trailing space.
func trimLeadingSpace(b []byte) []byte {
	i := 0
	for i < len(b) && isSpace(b[i]) {
		i++
	}
	return b[i:]
}

// isStructured reports whether raw is a JSON Array or Object, which is what
// §4.2 requires of a params member.
func isStructured(raw []byte) bool {
	v := trimLeadingSpace(raw)
	if len(v) == 0 {
		return false
	}
	// A params member of literal null is treated as structured-enough to pass:
	// it carries no parameters, which is the same position as omitting the
	// member, and rejecting it would fail clients that serialize an absent
	// argument list as null.
	if v[0] == 'n' {
		return true
	}
	return v[0] == '{' || v[0] == '['
}

// idOrNull returns id, or the null literal when the id member was absent.
//
// The specification requires a null id when the server could not detect one. It
// does not require discarding an id that was detected, and echoing it lets a
// client correlate the failure with the call that caused it — which is the whole
// purpose of the member. So a request that is invalid for some other reason
// still gets its id back when the id itself was readable.
func idOrNull(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return nullID
	}
	return id
}

// recoverID pulls the id out of a message whose full decode failed.
//
// Returns null when the id is absent or itself undecodable, which is the
// fallback §5 mandates.
func recoverID(v []byte) json.RawMessage {
	var probe idProbe
	if err := json.Unmarshal(v, &probe); err != nil || len(probe.ID) == 0 {
		return nullID
	}
	// An id that is not a String, Number or Null is not a usable id (§4), so it
	// is not echoed.
	switch probe.ID[0] {
	case '"', '-', 'n':
		return probe.ID
	}
	if probe.ID[0] >= '0' && probe.ID[0] <= '9' {
		return probe.ID
	}
	return nullID
}
