package rpc

import (
	"sync"

	json "github.com/goccy/go-json"
	"github.com/panjf2000/gnet/v2"
)

// Context carries one call through its middleware chain and collects the reply.
//
// It mirrors breeze.Context: the connection is exposed for handlers that need
// it, Next drives the chain, Set/Get carry per-call values, and the reply is
// recorded on the context rather than returned. A handler that sets neither a
// result nor an error produces a null result, which is a valid response — the
// specification requires the member to be present, not to be non-null.
type Context struct {
	// Conn is the gnet connection the call arrived on. It is exposed for the
	// same reason breeze.Context exposes it: a handler may want the peer
	// address, or to push an out-of-band notification later. Writing the reply
	// through it directly is not supported — the server owns the write, because
	// in a batch the replies have to be collected into one array.
	Conn gnet.Conn

	// Method is the name the call was dispatched under.
	Method string

	// Params is the raw "params" member, or nil when the member was absent.
	//
	// It is not decoded before the handler runs, because only the handler knows
	// what shape it should be. Decoding to map[string]any first would allocate
	// a map and a boxed value per member and then throw them away when the
	// handler unmarshalled into its own struct.
	//
	// These bytes may point into gnet's read buffer — see the lifetime note in
	// the package doc. Bind copies as it decodes; holding Params itself past
	// the handler's return requires a copy.
	Params json.RawMessage

	// ID is the raw "id" member, nil for a notification.
	ID json.RawMessage

	// result and err hold the reply. Exactly one is written to the wire, and
	// err wins if a handler somehow sets both — reporting a failure that also
	// produced a partial result as success would be the more damaging of the
	// two mistakes.
	result json.RawMessage
	err    *Error

	// resultValue holds a value set by Result before it is encoded. Encoding is
	// deferred to the serializer so that a batch encodes straight into the
	// outbound buffer instead of allocating a []byte per element.
	resultValue any
	hasResult   bool

	middlewares []HandlerFunc
	index       int

	// store backs Set/Get. Lazily created, and cleared on release so a value
	// cannot leak into an unrelated later call.
	store map[string]any
}

// Next runs the remaining handlers in the chain.
//
// Middleware calls it to hand control down and regain it afterwards; a handler
// that does not call it stops the chain, which is how an auth middleware
// rejects a call by setting an error and returning.
func (ctx *Context) Next() {
	ctx.index++
	for ctx.index < len(ctx.middlewares) {
		ctx.middlewares[ctx.index](ctx)
		ctx.index++
	}
}

// Abort stops the chain from advancing any further.
//
// It is what a middleware calls after setting an error, when the handlers below
// it must not run. Set the error first: Abort does not supply one, and a call
// aborted with no error replies with a null result.
func (ctx *Context) Abort() {
	ctx.index = len(ctx.middlewares)
}

// Bind decodes Params into v.
//
// A decode failure is reported as -32602 Invalid params, which is what §5.1
// specifies for parameters the server cannot use — the JSON itself parsed, so
// -32700 would be wrong. Bind returns the error as well so a handler can add
// context to it, but a handler that ignores the return value still produces a
// correct response.
//
// Absent params bind as a no-op rather than an error. A method whose parameters
// are all optional is legitimate, and a method with required parameters will
// find them zero-valued and can say so itself with a more useful message than a
// generic one from here.
func (ctx *Context) Bind(v any) error {
	if len(ctx.Params) == 0 {
		return nil
	}
	if err := json.Unmarshal(ctx.Params, v); err != nil {
		ctx.err = NewErrorData(CodeInvalidParams, msgInvalidParams, err.Error())
		return ctx.err
	}
	return nil
}

// Result sets the response's result member.
//
// The value is encoded when the response is serialized, not here, so a batch
// encodes every element straight into one outbound buffer.
func (ctx *Context) Result(v any) {
	ctx.resultValue = v
	ctx.hasResult = true
	ctx.result = nil
}

// ResultRaw sets an already-encoded result, which is copied verbatim into the
// response.
//
// This is the path for a proxy or cache: bytes that are already valid JSON go
// out without a decode and re-encode. The caller is responsible for the bytes
// being well-formed JSON; invalid bytes here produce a malformed response, and
// this package deliberately does not re-validate them, since doing so would
// defeat the purpose.
func (ctx *Context) ResultRaw(raw json.RawMessage) {
	ctx.result = raw
	ctx.resultValue = nil
	ctx.hasResult = true
}

// Error sets the response's error member from an *Error.
func (ctx *Context) Error(err *Error) {
	ctx.err = err
}

// Errorf sets the response's error member from a code and message.
func (ctx *Context) Errorf(code int, message string) {
	ctx.err = NewError(code, message)
}

// ErrorData sets the response's error member with an additional data member.
func (ctx *Context) ErrorData(code int, message string, data any) {
	ctx.err = NewErrorData(code, message, data)
}

// IsNotification reports whether the call is a notification, and therefore that
// whatever this handler sets will be discarded rather than sent (spec §4.1).
//
// A handler can use it to skip building an expensive result it knows will be
// thrown away.
func (ctx *Context) IsNotification() bool { return len(ctx.ID) == 0 }

// Set stores a per-call value, mirroring breeze.Context.Set.
func (ctx *Context) Set(key string, val any) {
	if ctx.store == nil {
		ctx.store = make(map[string]any, 4)
	}
	ctx.store[key] = val
}

// Get retrieves a value stored by Set, mirroring breeze.Context.Get.
func (ctx *Context) Get(key string) (any, bool) {
	if ctx.store == nil {
		return nil, false
	}
	v, ok := ctx.store[key]
	return v, ok
}

// Err returns the error currently set on the context, if any.
//
// Middleware that runs code after Next uses it to observe the outcome — a
// metrics or logging middleware needs to know whether the call failed and with
// which code.
func (ctx *Context) Err() *Error { return ctx.err }

// ─── Pooling ─────────────────────────────────────────────────────────────────

// contextPool recycles per-call contexts.
//
// The lifetime argument is the same one that licenses pooling in the root
// package: the context is released only after its reply has been handed to the
// connection, by a defer registered before the recover so it runs after it. A
// handler that stashes its *Context past its own return was already misusing it.
var contextPool = sync.Pool{
	New: func() any { return &Context{} },
}

func acquireContext() *Context {
	return contextPool.Get().(*Context)
}

// releaseContext clears every field and returns ctx to the pool.
//
// Everything is zeroed, not just what looks live: a field left set would be
// visible to an unrelated later call on a different connection, which is a data
// leak across clients rather than a mere stale read.
func releaseContext(ctx *Context) {
	if ctx == nil {
		return
	}
	ctx.Conn = nil
	ctx.Method = ""
	ctx.Params = nil
	ctx.ID = nil
	ctx.result = nil
	ctx.err = nil
	ctx.resultValue = nil
	ctx.hasResult = false
	ctx.middlewares = nil
	ctx.index = -1
	ctx.store = nil
	contextPool.Put(ctx)
}
