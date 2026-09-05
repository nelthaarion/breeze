package breeze

import "sync"

// pool.go — sync.Pool definitions for per-request objects.
//
// Pooling strategy:
//
//   - *Context: pooled. Acquired in OnTraffic, released after the response is
//     written — by runInline's deferred cleanup on the inline path, or by the
//     dispatch closure's on the pooled path. Reset clears all fields including
//     the lazy store map (required by
//     TestContextStoreNotRetainedAcrossRequests).
//
//   - *HTTPResponse: pooled. Acquired lazily by ensureResponse (called from
//     WriteString/JSON/HTML/Status/SetHeader). Released via releaseContext,
//     which checks whether ctx.Res is non-nil and returns it to the pool.
//
//   - *HTTPRequest: pooled, together with its header map — see requestPool
//     below for the lifetime argument that makes it safe.
//
//   - map[string]string route params: pooled — see paramsPool.
//
//   - []byte response wire buffers: pooled in response.go (wireBufPool), and
//     usable ONLY on the synchronous write path. See below.
//
// Lifecycle safety:
//
//   - Whichever path runs the chain registers its release defer FIRST so it
//     runs LAST, after the recover defer has had its chance to write a 500
//     from ctx.Res. The Context is therefore never recycled before its
//     response has been handed to the connection.
//   - Conn.Write is synchronous and has finished with the caller's slice by
//     the time it returns: it either completes the write syscall or copies the
//     remainder into the connection's outbound buffer. That is what licenses
//     wireBufPool on the inline path.
//   - Conn.AsyncWrite does NOT copy. It hands the slice to the poller and
//     returns before anything is written, so the pooled dispatch path
//     serializes into a freshly allocated slice (HTTPResponse.Bytes) instead.
//     An earlier version of this comment claimed AsyncWrite copies into gnet's
//     out-ring; it does not, and pooling those bytes would have produced
//     torn responses under load.
//   - sync.Pool is safe for concurrent use; a worker goroutine can release
//     while an event loop acquires for a different request.

var contextPool = sync.Pool{
	New: func() any { return &Context{} },
}

var responsePool = sync.Pool{
	New: func() any { return &HTTPResponse{} },
}

// requestPool stores *HTTPRequest together with its header map.
//
// Parsing used to allocate both on every request: the struct itself plus a
// make(map[string]string, 8) that had to grow buckets from scratch each time.
// Pooling them keeps the map's buckets alive across requests, so a reused
// request only pays the cost of inserting the keys.
//
// Lifecycle safety:
//
//   - The request is released from releaseContext, so its lifetime is exactly
//     the Context's. Context has been pooled since Phase 1.3.4, so anything
//     that would make pooling the request unsafe (a handler stashing ctx.Req
//     past the response) was already unsafe for ctx itself.
//   - req.owned is NOT reused. Header keys/values are b2s views into it, and
//     types.go documents that with zero-copy headers off, handlers may stash
//     those strings without copying. Dropping the reference on release lets each
//     escaped string keep its own backing array alive via the GC, so the
//     contract still holds. (With zero-copy headers on, owned is nil and the
//     strings view the connection buffer instead — a narrower guarantee, spelled
//     out on HTTPRequest.)
//   - req.Header is cleared with clear() rather than reallocated, which is
//     what makes the pooling worthwhile.
var requestPool = sync.Pool{
	New: func() any {
		return &HTTPRequest{Header: make(map[string]string, 8)}
	},
}

// paramsPool stores route parameter maps (map[string]string) for reuse
// across requests. The profiling report identified Router.Find's params
// map allocation as 34% of JSON-path bytes — pooling eliminates it.
//
// Lifecycle safety (analyzed before implementation):
//
//   - findRoute acquires a map from the pool when a parametric route
//     matches. The map is pre-sized by the pool's New func (capacity 4,
//     enough for typical 1-2 param routes without growth).
//   - OnTraffic sets ctx.params = params.
//   - Handlers read via ctx.Param(key) / ctx.GetParam(key). Returning a string
//     out of the map copies the string header, not the bytes: param values are
//     substrings of req.Path, so they view the request's header block and
//     inherit its lifetime exactly. Clearing the map on release therefore does
//     not disturb a value a handler is still holding — but with zero-copy
//     headers on, that value is only good until the handler returns. See
//     HTTPRequest in types.go. Param keys come from the registered route
//     pattern, which the router owns, so they are always safe to keep.
//   - ctx.GetParams() copies the map, so the returned map itself is safe to
//     stash; the strings inside it carry the lifetime described above.
//   - releaseContext clears all keys (delete loop) and returns the map
//     to the pool.
//
// SetParams(p) takes ownership of p. Callers must NOT pass a pooled map
// (which they cannot obtain — the pool is unexported). All current callers
// pass freshly-created maps, so this is safe.
//
// SetParam(key, value) when ctx.params == nil creates a new map via
// make(map[string]string) — NOT from the pool. This is correct because
// SetParam is a user-initiated write (not a route match), and the map
// will be returned to the pool by releaseContext. This means a request
// that calls SetParam but doesn't match a parametric route will allocate
// one map (same as before) — no regression.
var paramsPool = sync.Pool{
	New: func() any { return make(map[string]string, 4) },
}

// acquireContext returns a *Context from the pool. The caller MUST call
// releaseContext when done (typically in a deferred cleanup). Fields are
// zero-valued on first use and cleared by Reset on subsequent uses.
func acquireContext() *Context {
	return contextPool.Get().(*Context)
}

// releaseContext resets all fields on ctx and returns it to the pool.
// If ctx.Res is non-nil, it is also reset and returned to the response pool.
// If ctx.params is non-nil, it is cleared and returned to the params pool.
// If ctx.Req is non-nil, it is cleared and returned to the request pool.
//
// Safe to call on a nil ctx (no-op).
func releaseContext(ctx *Context) {
	if ctx == nil {
		return
	}
	// Release the response to its pool if one was built.
	if ctx.Res != nil {
		releaseResponse(ctx.Res)
		ctx.Res = nil
	}
	// Release the params map to its pool if one was set (route match or
	// user SetParam/SetParams). Clear all keys first so the next request
	// starts with an empty map.
	if ctx.params != nil {
		releaseParams(ctx.params)
		ctx.params = nil
	}
	// Release the request and its header map. Only requests that came from
	// the pool are returned to it — a hand-built Context (NewContext, tests)
	// owns a request the pool never issued, and recycling it would hand a
	// caller-visible struct to an unrelated connection.
	if ctx.Req != nil {
		if ctx.reqPooled {
			releaseRequest(ctx.Req)
		}
		ctx.Req = nil
	}
	// Clear all fields. Order doesn't matter, but be thorough — any
	// field left populated would leak data into the next request that
	// acquires this Context from the pool.
	ctx.Conn = nil
	ctx.reqPooled = false
	ctx.middlewares = nil
	ctx.index = -1
	ctx.store = nil // MUST be nil — TestContextStoreNotRetainedAcrossRequests
	contextPool.Put(ctx)
}

// acquireParams returns a map[string]string from the params pool. The map
// is pre-cleared (empty) — the caller populates it with route parameters.
func acquireParams() map[string]string {
	return paramsPool.Get().(map[string]string)
}

// acquireRequest returns an *HTTPRequest from the pool with an empty header
// map ready to populate. ParseHTTPRequest fills in the rest.
func acquireRequest() *HTTPRequest {
	return requestPool.Get().(*HTTPRequest)
}

// releaseRequest clears req and returns it to the pool.
//
// owned and Query are dropped rather than reused: string views into owned may
// have escaped to the caller (see requestPool), and url.ParseQuery always
// builds a fresh map anyway. Header is cleared in place so its buckets survive.
func releaseRequest(req *HTTPRequest) {
	if req == nil {
		return
	}
	clear(req.Header)
	req.Method = ""
	req.Path = ""
	req.Query = nil
	req.Body = nil
	req.owned = nil
	requestPool.Put(req)
}

// releaseParams clears all keys from m and returns it to the params pool.
// The map MUST not be referenced by the caller after this call — it will
// be reused by a future request.
func releaseParams(m map[string]string) {
	for k := range m {
		delete(m, k)
	}
	paramsPool.Put(m)
}

// acquireResponse returns a *HTTPResponse from the pool. The caller MUST
// ensure the response is eventually returned via releaseResponse (either
// directly or via releaseContext).
func acquireResponse() *HTTPResponse {
	return responsePool.Get().(*HTTPResponse)
}

// releaseResponse resets all fields on r and returns it to the pool.
//
// The Headers map is set to nil (not cleared in-place) so the GC can
// collect it. Pooling the map separately is a future optimization.
// The shared maps (hdrsJSON/hdrsText/hdrsHTML) are package-level vars
// and are not affected by nil-ing r.Headers.
func releaseResponse(r *HTTPResponse) {
	r.Status = 0
	r.Headers = nil
	r.headersShared = false
	r.rawHeaders = nil
	r.ctypePinned = false
	r.Body = nil
	responsePool.Put(r)
}

// ensureResponse returns a *HTTPResponse for ctx, acquiring one from the
// pool if ctx.Res is nil. If ctx.Res already exists (e.g., Status was
// called first), it is reused — no allocation.
//
// All body methods (WriteString/JSON/HTML) and SetHeader call this to
// ensure responses come from the pool rather than via &HTTPResponse{...}
// literals.
func (ctx *Context) ensureResponse() *HTTPResponse {
	if ctx.Res == nil {
		ctx.Res = acquireResponse()
	}
	return ctx.Res
}
