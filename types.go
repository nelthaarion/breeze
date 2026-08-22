package breeze

import "net/url"

// Method defines the HTTP method type.
type Method string

const (
        GET     Method = "GET"
        PUT     Method = "PUT"
        PATCH   Method = "PATCH"
        POST    Method = "POST"
        DELETE  Method = "DELETE"
        OPTIONS Method = "OPTIONS" // FIX: was "OPTION" — RFC 9110 defines "OPTIONS"
)

// HTTPRequest holds a fully parsed HTTP request.
//
// # Lifetime of the strings on this struct
//
// req.Path and every req.Header key and value are unsafe string views (b2s
// slices) into a block of header bytes. Which block depends on the server's
// SetZeroCopyHeaders setting, and that is the only thing that changes how long
// they stay valid:
//
//   - Default (zero-copy off): the block is req.owned, a private copy the parser
//     made. The GC keeps it alive as long as any string viewing it is reachable,
//     even after the *HTTPRequest is collected — so a handler may stash header
//     strings in globals or caches without copying them.
//
//   - SetZeroCopyHeaders(true): the block is the connection's read buffer, which
//     gnet reuses for the next read. The strings are valid for the duration of
//     the handler and no longer. Stashing one past the handler's return requires
//     strings.Clone; the bytes stay readable, so the failure mode is silent
//     mutation into a later request rather than a crash. Requests handed to a
//     worker goroutine are re-parsed into owned memory first, so a handler
//     always sees a fully valid request regardless of the setting.
//
// req.Body is separate and its guarantee does not vary: it is always backed by
// Go memory — either OnTraffic's reassembly buffer or a copy made for it — and
// the GC keeps that array alive as long as req.Body is reachable. A handler may
// hold req.Body for as long as it likes, on any goroutine.
//
// The per-connection leftover slice in s.bufs backs neither of these once a
// request has been parsed out of it, so OnTraffic may compact or discard it
// without affecting an in-flight handler.
//
// req.Method is either a package-level constant (no allocation) or a
// freshly copied string for unknown methods — it never points into the header
// block.
type HTTPRequest struct {
        Method Method
        Path   string
        Query  url.Values
        Header map[string]string
        Body   []byte
        // owned holds the header bytes that req.Path and req.Header strings
        // point into, when the parser was asked to make its own copy of them.
        // It is nil for a zero-copy parse, where those strings view the caller's
        // buffer instead.
        //
        // Unexported so callers cannot mutate it; its presence here ensures the
        // GC can trace the pointer chain from any escaped header string back to
        // this backing array.
        owned []byte
}

// HTTPResponse represents an HTTP response.
type HTTPResponse struct {
        Status  int
        Headers map[string]string
        Body    []byte
        // headersShared is true when Headers points to one of the package-level
        // shared maps (hdrsJSON / hdrsText / hdrsHTML). SetHeader must copy-on-write
        // before mutating. Go does not allow map == map comparisons, so we use this
        // flag as the sentinel instead.
        headersShared bool
        // rawHeaders is the pre-rendered "Key: Value\r\n" block corresponding to
        // Headers, set only when Headers is one of the shared maps above. When it
        // is non-nil, Bytes copies it verbatim instead of iterating the map —
        // which is the whole point, since a map range costs more than the copy.
        //
        // SetHeader clears it as part of its copy-on-write, so a mutated response
        // always falls back to serializing from the map and the two can never
        // disagree about what the response actually contains.
        rawHeaders []byte
}
