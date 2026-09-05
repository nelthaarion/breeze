package breeze

import (
	"strings"

	"github.com/goccy/go-json"
	"github.com/nelthaarion/breeze/v2/binding"
	"github.com/panjf2000/gnet/v2"
)

// Pre-built common response headers. These are reused across all responses
// so we never allocate a new map for the standard content types.
// They are marked read-only by convention — never write to them directly.
// SetHeader uses the headersShared flag for copy-on-write protection.
var (
	hdrsJSON = map[string]string{"Content-Type": "application/json"}
	hdrsText = map[string]string{"Content-Type": "text/plain"}
	hdrsHTML = map[string]string{"Content-Type": "text/html; charset=utf-8"}
)

// Pre-rendered wire form of each shared header map above, so serializing a
// standard response never has to range over a map. Keep each entry in sync
// with its map — they describe the same headers in two representations.
var (
	rawJSON = []byte("Content-Type: application/json\r\n")
	rawText = []byte("Content-Type: text/plain\r\n")
	rawHTML = []byte("Content-Type: text/html; charset=utf-8\r\n")
)

type Context struct {
	Conn        gnet.Conn
	Req         *HTTPRequest
	Res         *HTTPResponse
	params      map[string]string
	middlewares []HandlerFunc
	index       int

	// reqPooled records whether Req came from requestPool, so releaseContext
	// recycles only requests the pool actually issued. A Context built by
	// hand (NewContext, tests, the middleware integration suite) owns a
	// request that the caller may still hold, and must not be recycled.
	reqPooled bool

	// store is a lazy-initialized typed key-value store for middleware that
	// need to attach structured data (e.g. JWT claims, user objects) to the
	// request context. It is nil until the first Set call, so requests that
	// don't use it pay zero allocation cost.
	store map[string]any
}

// setContentType installs a body method's content type without discarding
// headers a middleware already set.
//
// # The bug this exists to prevent
//
// The three body methods used to assign `r.Headers = hdrsJSON` outright. Every
// header set before them was therefore dropped, and the middlewares that set
// headers all do so *before* ctx.Next() — because that is the only place they can:
// after Next returns, the handler has already written the response. So
// CORSMiddleware and SecurityMiddleware were computing a dozen headers per request
// and having every one of them silently discarded by the handler's ctx.JSON call.
// Nothing errored. A browser simply never saw Access-Control-Allow-Origin.
//
// # Why the fast path survives
//
// headersShared is the discriminator, and it answers exactly the right question:
// it is true when Headers is one of the package-level shared maps, which is only
// the case when nothing has called SetHeader. So an untouched response still gets
// the shared map and the precomputed wire block — zero allocations, no map range
// at serialization time — and only a response someone has actually written a
// header to takes the slower branch.
//
// A shared map is also replaced rather than merged, which covers
// `ctx.WriteString(...); ctx.JSON(...)`: the second call's content type is the
// correct one and there is nothing of the caller's to preserve.
//
// # Why an existing Content-Type wins
//
// A caller who set one chose it deliberately — `application/problem+json` for an
// RFC 9457 body is the case in this repository — and a body method overriding it
// would make ctx.JSON unusable for anything but the default type. Emitting both
// would be worse still: RFC 9110 §5.3 lets a recipient treat a duplicated
// Content-Type as malformed.
//
// "Set by a caller" is tracked with a flag rather than inferred from the map's
// contents, because the map cannot answer the question. A body method leaves a
// Content-Type behind too, so `ctx.WriteString(...)` followed by `ctx.JSON(...)` on a
// response some middleware had already touched would find "text/plain" present and
// keep it — sending a JSON body labelled as text. The flag distinguishes the caller's
// choice from a previous body method's default, and only the former is preserved.
func (r *HTTPResponse) setContentType(shared map[string]string, raw []byte, ctype string) {
	if r.Headers == nil || r.headersShared {
		r.Headers = shared
		r.headersShared = true
		r.rawHeaders = raw
		return
	}

	// A private map: SetHeader has been called, so its contents are the
	// caller's and must be kept. rawHeaders no longer describes them.
	r.rawHeaders = nil
	if r.ctypePinned {
		return
	}
	// Delete before assigning, so a previous body method's differently-cased key
	// cannot survive alongside this one. Both would go on the wire.
	deleteContentType(r.Headers)
	r.Headers["Content-Type"] = ctype
}

// deleteContentType removes any Content-Type key regardless of capitalisation.
//
// Case-insensitive because the map is written to the wire verbatim, so a caller's
// "content-type" and a body method's "Content-Type" would both be sent. It is a scan
// of a map with at most a handful of entries, on a path that is already off the fast
// branch.
func deleteContentType(h map[string]string) {
	for k := range h {
		if isContentType(k) {
			delete(h, k)
		}
	}
}

// isContentType reports whether k names the Content-Type header.
func isContentType(k string) bool {
	return len(k) == len("Content-Type") && strings.EqualFold(k, "Content-Type")
}

// statusOrDefault returns the status code already set on ctx.Res, if any,
// so that calling Status() before a body method (WriteString/JSON/HTML)
// is not silently discarded. Falls back to def when no status was set yet.
func (ctx *Context) statusOrDefault(def int) int {
	if ctx.Res != nil && ctx.Res.Status != 0 {
		return ctx.Res.Status
	}
	return def
}

// WriteString writes a plain-text body.
//
// It returns an error for signature consistency with JSON, not because it can
// fail: there is nothing here to go wrong, and it always returns nil. Having the
// three body methods differ in signature would mean a handler's shape depended on
// which one it happened to call, and switching a response from text to JSON would
// then be a two-line change instead of a one-line one.
func (ctx *Context) WriteString(s string) error {
	r := ctx.ensureResponse()
	r.Status = ctx.statusOrDefault(200)
	r.Body = []byte(s)
	r.setContentType(hdrsText, rawText, "text/plain")
	return nil
}

// JSON marshals data and writes it as the response body.
//
// # Why the error is returned as well as written
//
// A value that will not marshal is a bug in the handler, not a bad request, and it
// used to be reported as a 400 with `{"message":"error parsing json"}` — a response
// that blames the client for the server's mistake and gives nobody the actual
// marshalling error.
//
// It now returns the error so the caller can propagate it to the framework's error
// path, and a 500 with the cause logged is what a caller who does propagate gets.
// The 400 body is still written first, so a handler that ignores the return value
// behaves exactly as it did before rather than sending nothing at all.
func (ctx *Context) JSON(data any) error {
	d, err := json.Marshal(data)
	r := ctx.ensureResponse()
	if err != nil {
		r.Status = ctx.statusOrDefault(400)
		r.Body = []byte(`{"message":"error parsing json"}`)
		r.setContentType(hdrsJSON, rawJSON, "application/json")
		return err
	}
	r.Status = ctx.statusOrDefault(200)
	r.Body = d
	r.setContentType(hdrsJSON, rawJSON, "application/json")
	return nil
}

// HTML writes an HTML body. Returns nil always; see WriteString for why it returns
// anything at all.
func (ctx *Context) HTML(data []byte) error {
	r := ctx.ensureResponse()
	r.Status = ctx.statusOrDefault(200)
	r.Body = data
	r.setContentType(hdrsHTML, rawHTML, "text/html; charset=utf-8")
	return nil
}

// Status sets (or overrides) the response status code.
//
// Order-independent: Status may be called before or after the body
// methods (JSON/WriteString/HTML). Those methods replace ctx.Res but
// preserve any status code already set via Status, so both of these
// work identically:
//
//	ctx.Status(401); ctx.WriteString("nope")
//	ctx.WriteString("nope"); ctx.Status(401)
//
// For bodyless responses (204, 304) call this alone.
func (ctx *Context) Status(code int) {
	r := ctx.ensureResponse()
	r.Status = code
}

// SetHeader adds or replaces a single response header.
//
// When the response was built via JSON/WriteString/HTML, its Headers field
// points to a shared package-level map. SetHeader detects this via the
// headersShared flag and performs a copy-on-write before mutating, so the
// shared maps are never clobbered. Subsequent SetHeader calls on the same
// response are direct writes into the private copy.
//
// Optimization (Phase 1.3.3): the copy-on-write now allocates with tight
// capacity (len(orig)+1 instead of len(orig)+4), reducing over-allocation
// for the common case of adding 1 header to a 1-entry shared map.
// Status() when ctx.Res == nil no longer allocates an empty map — it
// creates a bare HTTPResponse and lets SetHeader allocate the map lazily
// if needed.
func (ctx *Context) SetHeader(key, value string) {
	r := ctx.ensureResponse()
	// Any mutation invalidates the pre-rendered header block: from here on
	// the response must be serialized from the map, which is now the only
	// representation that reflects this write.
	r.rawHeaders = nil
	// Copy-on-write: upgrade shared map to a private one.
	if r.headersShared {
		orig := r.Headers
		priv := make(map[string]string, len(orig)+1)
		for k, v := range orig {
			priv[k] = v
		}
		r.Headers = priv
		r.headersShared = false
	}
	if r.Headers == nil {
		r.Headers = make(map[string]string, 2)
	}
	// A caller naming Content-Type is choosing it, and setContentType must not
	// override that with a body method's default. Recorded as a flag because the
	// map cannot distinguish this write from the one a body method makes.
	if isContentType(key) {
		deleteContentType(r.Headers)
		r.ctypePinned = true
	}
	r.Headers[key] = value
}

// GetHeader returns the value of a response header, or "" if not set.
//
// This is the preferred way to read response headers in middleware — it
// is safe to call even when ctx.Res is nil.
func (ctx *Context) GetHeader(key string) string {
	if ctx.Res == nil {
		return ""
	}
	return ctx.Res.Headers[key]
}

// --- Typed store (Set/Get) ---

func (ctx *Context) Set(key string, val any) {
	if ctx.store == nil {
		ctx.store = make(map[string]any, 4)
	}
	ctx.store[key] = val
}

func (ctx *Context) Get(key string) (any, bool) {
	if ctx.store == nil {
		return nil, false
	}
	v, ok := ctx.store[key]
	return v, ok
}

func (ctx *Context) MustGet(key string) any {
	v, ok := ctx.Get(key)
	if !ok {
		panic("breeze: context key not found: " + key)
	}
	return v
}

// --- Params helpers ---

func (ctx *Context) Param(key string) string {
	if ctx.params == nil {
		return ""
	}
	return ctx.params[key]
}

func (ctx *Context) GetParam(key string) string {
	if ctx.params == nil {
		return ""
	}
	return ctx.params[key]
}

func (ctx *Context) SetParam(key, value string) {
	if ctx.params == nil {
		ctx.params = make(map[string]string)
	}
	ctx.params[key] = value
}

func (ctx *Context) SetParams(p map[string]string) {
	if p == nil {
		ctx.params = make(map[string]string)
	} else {
		ctx.params = p
	}
}

func (ctx *Context) GetParams() map[string]string {
	if ctx.params == nil {
		return map[string]string{}
	}
	cpy := make(map[string]string, len(ctx.params))
	for k, v := range ctx.params {
		cpy[k] = v
	}
	return cpy
}

func (ctx *Context) Query(key string) string {
	if ctx.Req == nil || ctx.Req.Query == nil {
		return ""
	}
	return ctx.Req.Query.Get(key)
}

// --- Middleware chain control ---

func NewContext(method Method, path string) *Context {
	return &Context{
		Req: &HTTPRequest{
			Method: method,
			Path:   path,
			Header: make(map[string]string),
		},
		index: -1,
	}
}

func (ctx *Context) SetMiddlewareChain(middlewares []HandlerFunc, handler HandlerFunc) {
	ctx.middlewares = append(middlewares, handler)
	ctx.index = -1
}

// Next runs the remaining chain and returns the first error any link produced.
//
// # Why the error comes back rather than being handled here
//
// A middleware's own return value is what the framework acts on, and a middleware
// that calls Next is responsible for what it does with Next's result. The common
// shape is to pass it straight through:
//
//	func Logging(ctx *breeze.Context) error {
//	    start := time.Now()
//	    err := ctx.Next()
//	    log.Printf("%s %v", ctx.Req.Path, time.Since(start))
//	    return err
//	}
//
// A middleware that swallows it — `_ = ctx.Next()` — is choosing to treat the
// failure as handled, which is legitimate for something like a fallback but must be
// deliberate. Handling the error inside Next would remove that choice, and would
// also mean a middleware could not run cleanup code around a failing handler.
//
// Returning at the first error is what stops the chain: the links after the failing
// one never run, and neither does the rest of any middleware that propagates.
func (ctx *Context) Next() error {
	ctx.index++
	if ctx.index >= len(ctx.middlewares) {
		return nil
	}
	fn := ctx.middlewares[ctx.index]
	if fn == nil {
		return nil
	}
	return fn(ctx)
}

// Bind decodes and validates an incoming request into dst using the JSON
// body, query parameters, and path parameters available on the request, in
// that order. dst must be a pointer to a struct; see package binding for the
// supported "json", "form", "param", and "validate" struct tags.
//
// On a validation failure, Bind writes a 422 response with an RFC 9457
// problem+json body describing each failing field and returns the
// *binding.ValidationError. On any other failure (e.g. malformed JSON body),
// it writes a 400 problem+json response and returns the underlying error.
// Callers should treat a non-nil return as "response already written" and
// return early.
func (ctx *Context) Bind(dst any) error {
	sources := []binding.Source{}

	if ctx.Req != nil && len(ctx.Req.Body) > 0 {
		sources = append(sources, binding.JSONBody(ctx.Req.Body))
	}

	if ctx.Req != nil && len(ctx.Req.Query) > 0 {
		sources = append(sources, binding.Query(ctx.Req.Query))
	}

	if len(ctx.params) > 0 {
		sources = append(sources, binding.Path(ctx.params))
	}

	err := binding.Bind(dst, sources...)
	if err == nil {
		return nil
	}

	if verr, ok := err.(*binding.ValidationError); ok {
		ctx.Status(422)
		ctx.JSON(verr.ToProblemJSON())
		return err
	}

	ctx.Status(400)
	ctx.JSON(map[string]any{
		"type":   "about:blank",
		"title":  "Bad Request",
		"status": 400,
		"detail": err.Error(),
	})
	return err
}

func (ctx *Context) Abort() {
	ctx.index = len(ctx.middlewares)
}
