package breeze

import (
	"github.com/goccy/go-json"
	"github.com/nelthaarion/breeze/binding"
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

// statusOrDefault returns the status code already set on ctx.Res, if any,
// so that calling Status() before a body method (WriteString/JSON/HTML)
// is not silently discarded. Falls back to def when no status was set yet.
func (ctx *Context) statusOrDefault(def int) int {
	if ctx.Res != nil && ctx.Res.Status != 0 {
		return ctx.Res.Status
	}
	return def
}

func (ctx *Context) WriteString(s string) {
	r := ctx.ensureResponse()
	r.Status = ctx.statusOrDefault(200)
	r.Headers = hdrsText
	r.Body = []byte(s)
	r.headersShared = true
	r.rawHeaders = rawText
}

func (ctx *Context) JSON(data any) {
	d, err := json.Marshal(data)
	r := ctx.ensureResponse()
	if err != nil {
		r.Status = ctx.statusOrDefault(400)
		r.Headers = hdrsJSON
		r.Body = []byte(`{"message":"error parsing json"}`)
		r.headersShared = true
		r.rawHeaders = rawJSON
		return
	}
	r.Status = ctx.statusOrDefault(200)
	r.Headers = hdrsJSON
	r.Body = d
	r.headersShared = true
	r.rawHeaders = rawJSON
}

func (ctx *Context) HTML(data []byte) {
	r := ctx.ensureResponse()
	r.Status = ctx.statusOrDefault(200)
	r.Headers = hdrsHTML
	r.Body = data
	r.headersShared = true
	r.rawHeaders = rawHTML
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

func (ctx *Context) Next() {
	ctx.index++
	if ctx.index >= len(ctx.middlewares) {
		return
	}
	fn := ctx.middlewares[ctx.index]
	if fn != nil {
		fn(ctx)
	}
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
