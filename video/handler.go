package video

import (
	"errors"
	"fmt"
	"strings"
	"time"

	breeze "github.com/nelthaarion/breeze/v2"
	"github.com/nelthaarion/breeze/v2/events"
	"github.com/nelthaarion/breeze/v2/observability"
)

// Mount registers a video streaming handler on r.
//
// It returns an error rather than panicking, because a bad media root is a
// deployment mistake — a path that exists on a developer's laptop and not
// in a container — and the caller deserves the chance to report it well:
//
//	if err := video.Mount(router, video.Config{Root: "./media"}); err != nil {
//	    log.Fatalf("video: %v", err)
//	}
//
// Three routes are registered under Prefix + "/*filepath": GET, HEAD and
// OPTIONS. HEAD is registered explicitly because the framework's Method
// constants do not include it, and a player that cannot ask for a file's
// length without downloading it will download it just to find out. OPTIONS
// answers media CORS preflights without touching the filesystem.
func Mount(r *breeze.Router, cfg Config) error {
	if r == nil {
		return fmt.Errorf("%w: router is nil", ErrInvalidConfig)
	}
	m, err := newMount(cfg)
	if err != nil {
		return err
	}

	pattern := m.prefix + "/*filepath"
	h := m.handler()
	// Registered as blocking. Serving a range stats the file, opens it, and
	// copies chunks out of it — file I/O from start to finish. Running that on
	// a gnet event-loop goroutine would stall every other connection pinned to
	// the same reactor for the length of the read, which for video is exactly
	// the workload that makes it unbearable.
	r.HandleBlocking(breeze.GET, pattern, h)
	r.HandleBlocking(breeze.Method("HEAD"), pattern, h)
	r.Handle(breeze.OPTIONS, pattern, h)
	return nil
}

// Handler returns a handler for a configured mount without registering it,
// for callers who want to attach it to their own route or wrap it.
func Handler(cfg Config) (breeze.HandlerFunc, error) {
	m, err := newMount(cfg)
	if err != nil {
		return nil, err
	}
	return m.handler(), nil
}

// handler builds the request handler for this mount.
func (m *mount) handler() breeze.HandlerFunc {
	return func(ctx *breeze.Context) error {
		started := time.Now()
		out := &connSink{conn: ctx.Conn, bufs: m.bufs}

		// Taking over the connection. Every byte from here on is written
		// by this package, so the framework must not append a response of
		// its own — that would put a second set of headers on the wire
		// after a body the client is still reading.
		ctx.Res = nil

		if ctx.Req.Method == breeze.OPTIONS {
			// Preflight is deliberately not reported as a media stream:
			// it transfers no file bytes and must not create a phantom
			// "(unresolved)" row in the dashboard. A refused preflight is
			// still a failed request, so surface it through OnError without
			// pretending it was a file failure.
			if err := m.serveOptions(ctx, out); err != nil && m.onError != nil {
				m.onError(ctx, "", err)
			}
			return nil
		}

		name, status, sent, err := m.serve(ctx, out)

		m.report(ctx, name, status, sent, time.Since(started), err)

		return nil
	}
}

// serveOptions answers a CORS preflight without normalizing or stat'ing the
// requested file. The browser is asking whether a method/header combination is
// allowed, not asking for a representation.
func (m *mount) serveOptions(ctx *breeze.Context, out sink) error {
	origin := ctx.Req.Header["origin"]
	requestedMethod := strings.TrimSpace(ctx.Req.Header["access-control-request-method"])
	status := 204
	var reason error

	if requestedMethod != "" && !strings.EqualFold(requestedMethod, "GET") &&
		!strings.EqualFold(requestedMethod, "HEAD") {
		status = 405
		reason = fmt.Errorf("video: preflight method %q is not allowed", requestedMethod)
	}
	if origin != "" && (m.anyOrigin || len(m.origins) > 0) {
		if !m.originAllowed(origin) {
			status = 403
			reason = fmt.Errorf("video: origin %q is not allowed", origin)
		}
	}
	if status != 204 {
		return m.writeErrorResponse(out, origin, status, 0, reason, false)
	}

	h := newHead(204).
		set("Cache-Control", "no-store")
	if m.anyOrigin || len(m.origins) > 0 {
		h.set("Vary", "Origin, Access-Control-Request-Method, Access-Control-Request-Headers")
		if origin != "" && m.originAllowed(origin) {
			if m.anyOrigin {
				h.set("Access-Control-Allow-Origin", "*")
			} else {
				h.set("Access-Control-Allow-Origin", origin)
			}
			h.set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
			if headers := corsRequestHeaders(ctx.Req.Header["access-control-request-headers"]); headers != "" {
				h.set("Access-Control-Allow-Headers", headers)
			}
			h.set("Access-Control-Max-Age", "600")
		}
	}
	return out.write(h.bytes())
}

// writeErrorResponse preserves a transport failure alongside the internal
// request error. The wire status is already chosen, but report needs to know
// when the peer disappeared while the error response was being written; it
// must not count that ordinary disconnect as a server failure.
func (m *mount) writeErrorResponse(out sink, origin string, status int, size int64, cause error, noBody bool) error {
	if writeErr := m.writeError(out, origin, status, size, noBody); writeErr != nil {
		return errors.Join(cause, writeErr)
	}
	return cause
}

// originAllowed is the shared allow-list check for ordinary responses and
// preflight responses.
func (m *mount) originAllowed(origin string) bool {
	if m.anyOrigin {
		return true
	}
	_, ok := m.origins[origin]
	return ok
}

// corsRequestHeaders echoes only syntactically valid header names. It never
// reflects commas, CR, or LF from a request into a response header.
func corsRequestHeaders(raw string) string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" || !validHeaderToken(part) {
			continue
		}
		out = append(out, part)
	}
	return strings.Join(out, ", ")
}

func validHeaderToken(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c)) {
			continue
		}
		return false
	}
	return s != ""
}

// serve carries out one request and reports what happened.
//
// It returns the resolved name, the status put on the wire, the number of
// body bytes sent, and the internal error if any. Reporting is left to the
// caller so that every exit path is instrumented identically — an early
// 404 and a completed 206 travel through the same telemetry.
func (m *mount) serve(ctx *breeze.Context, out sink) (name string, status int, sent int64, err error) {
	origin := ctx.Req.Header["origin"]

	// Header keys are lowercased by the request parser, so they are read
	// in that form throughout. Reading "Range" here would silently never
	// match and quietly disable seeking.
	rawRange := ctx.Req.Header["range"]

	head := ctx.Req.Method == breeze.Method("HEAD")

	raw := ctx.Param("filepath")

	if m.opaque {
		// The path is a token, not a name. Opening it both authenticates
		// the request and yields the file, so there is nothing to
		// normalize first and no signature to check afterwards: a
		// forged, edited or expired token never gets this far.
		//
		// The decrypted name still goes through normalize, because the
		// token proves who issued the link and says nothing about
		// whether the name inside it is safe to resolve.
		tok, derr := m.decodePath(strings.TrimPrefix(raw, "/"))
		if derr != nil {
			status = statusFor(derr)
			return "", status, 0, m.writeErrorResponse(out, origin, status, 0, derr, head)
		}
		decoded, terr := m.parseToken(tok)
		if terr != nil {
			status = statusFor(terr)
			return "", status, 0, m.writeErrorResponse(out, origin, status, 0, terr, head)
		}
		name, err = m.normalize(decoded)
		if err != nil {
			status = statusFor(err)
			return name, status, 0, m.writeErrorResponse(out, origin, status, 0, err, head)
		}
	} else {
		name, err = m.normalize(raw)
		if err != nil {
			status = statusFor(err)
			return name, status, 0, m.writeErrorResponse(out, origin, status, 0, err, head)
		}

		// Signature and authorisation both run on the normalized name
		// and before any stat, so an unauthorised flood costs no disk
		// I/O.
		if err = m.verifySignature(name, ctx.Req.Query.Get("exp"), ctx.Req.Query.Get("sig")); err != nil {
			status = statusFor(err)
			return name, status, 0, m.writeErrorResponse(out, origin, status, 0, err, head)
		}
	}

	if m.authorize != nil {
		if aerr := m.authorize(ctx, name); aerr != nil {
			// An unrecognised error from user code becomes 403 rather
			// than 500: the callback refused the request, which is a
			// decision, not a malfunction.
			status = statusFor(aerr)
			if status == 500 {
				status = 403
			}
			return name, status, 0, m.writeErrorResponse(out, origin, status, 0, aerr, head)
		}
	}

	res, err := m.stat(name)
	if err != nil {
		status = statusFor(err)
		return name, status, 0, m.writeErrorResponse(out, origin, status, 0, err, head)
	}
	defer res.close()

	tag := etagFor(res.Size, res.ModTime)

	// A conditional hit ends the request before any body bytes are read.
	// Resolution already opened this descriptor so metadata and streaming
	// use the same race-free object; revalidation still avoids a transfer.
	if notModified(ctx.Req.Header["if-none-match"], ctx.Req.Header["if-modified-since"], tag, res.ModTime) {
		h := newHead(304).
			set("ETag", tag).
			set("Last-Modified", httpTime(res.ModTime)).
			set("Cache-Control", m.cache).
			set("Accept-Ranges", "bytes")
		m.applyCORS(h, origin)
		// A 304 carries no body and no Content-Length; adding either
		// would make the client wait for bytes that never arrive.
		if werr := out.write(h.bytes()); werr != nil {
			return name, 304, 0, werr
		}
		return name, 304, 0, nil
	}

	// If-Range guards a resumed transfer: the client is saying "send me
	// this range only if the file has not changed since I started". A
	// mismatch means the stored prefix is stale, so the whole file must
	// be sent rather than a slice that would corrupt the client's copy.
	if ir := ctx.Req.Header["if-range"]; ir != "" && rawRange != "" {
		if !ifRangeMatches(ir, tag, res.ModTime) {
			rawRange = ""
		}
	}

	rng, rerr := parseRange(rawRange, res.Size)
	switch {
	case errors.Is(rerr, ErrRangeNotSatisfiable):
		status = 416
		return name, status, 0, m.writeErrorResponse(out, origin, status, res.Size, rerr, head)

	case rng == nil && res.Size == 0:
		// An empty file is legitimate and must produce a valid 200 with
		// Content-Length: 0, not a range of -1.
		status = 200
		rng = &byteRange{Start: 0, End: -1}

	case rng == nil:
		// No Range is a deliberate first-chunk response rather than a
		// full download. Players learn the total size from Content-Range
		// and immediately follow up with explicit ranges, while a client
		// that did not ask for a range never gets an entire movie by
		// accident. The response is still streamed in ChunkSize writes.
		status = 206
		first, _ := byteRange{Start: 0, End: res.Size - 1}.clamp(int64(m.chunk))
		rng = &first

	default:
		status = 206
		clamped, _ := rng.clamp(m.maxChunk)
		rng = &clamped
	}

	h := newHead(status).
		set("Content-Type", contentTypeFor(res.Name)).
		set("Accept-Ranges", "bytes").
		set("ETag", tag).
		set("Last-Modified", httpTime(res.ModTime)).
		set("Cache-Control", m.cache)

	length := rng.Length()
	if length < 0 {
		length = 0
	}
	h.setInt("Content-Length", length)

	if status == 206 {
		h.set("Content-Range", rng.contentRange(res.Size))
	}

	// Videos are opaque containers, but a mount can be pointed at a
	// directory that also holds manifests, and an .m3u8 is text a browser
	// could be talked into rendering. nosniff removes the ambiguity.
	h.set("X-Content-Type-Options", "nosniff")
	m.applyCORS(h, origin)

	if werr := out.write(h.bytes()); werr != nil {
		return name, status, 0, werr
	}

	// A HEAD response must carry the same headers as the GET and no body,
	// which is what lets a player learn Content-Length and range support
	// in one cheap round trip.
	if head || length == 0 {
		return name, status, 0, nil
	}

	sent, werr := m.writeBody(out, res, *rng)
	if werr != nil {
		return name, status, sent, werr
	}
	if sent != length {
		// The head promised more than the file delivered, so the client
		// will hang waiting. Nothing can fix the response now; what
		// matters is that the operator finds out.
		return name, status, sent, fmt.Errorf("short read: sent %d of %d bytes of %q", sent, length, name)
	}
	return name, status, sent, nil
}

// report publishes the outcome of a request to every configured observer.
//
// All telemetry is funnelled through here so that a new exit path cannot
// be added without instrumentation, and so a client disconnect is recorded
// as what it is rather than as a server error.
func (m *mount) report(ctx *breeze.Context, name string, status int, sent int64, took time.Duration, err error) {
	gone := isPeerGone(err)

	// A disconnect is not a failure of the server. Counting it as one
	// would make a normal seek-heavy session look like an outage.
	failed := err != nil && !gone

	// Counted here rather than in serve, for the same reason the telemetry is:
	// this is the one place every exit path passes through, so a new early
	// return cannot escape the count.
	m.served.Add(1)
	if status == 206 {
		m.partial.Add(1)
	}
	if failed {
		m.failedReqs.Add(1)
	}
	if gone {
		m.disconnects.Add(1)
	}
	if sent > 0 {
		m.bytesSent.Add(uint64(sent))
	}
	m.lastServedNs.Store(time.Now().UnixNano())

	if m.onError != nil && err != nil {
		m.onError(ctx, name, err)
	}

	if m.col != nil {
		attrs := map[string]string{
			"file":   name,
			"status": fmt.Sprint(status),
			"bytes":  fmt.Sprint(sent),
		}
		if gone {
			attrs["disconnected"] = "true"
		}
		sig := observability.Signal{
			Source:   observability.SourceHTTP,
			Kind:     observability.KindDispatch,
			Name:     "video.stream",
			Time:     time.Now().Add(-took),
			Duration: took,
			Failed:   failed,
			Attrs:    attrs,
		}
		if err != nil {
			sig.Err = err.Error()
		}
		if gone {
			// Cancelled, not failed: the work stopped because the other
			// end went away.
			sig.Cancelled = true
		}
		m.col.Publish(sig)
	}

	if m.bus != nil {
		// Async because a listener may write to a database or push to a
		// socket, and a video response must not wait on that. The
		// framework's own guidance is to use the async form for anything
		// that performs I/O.
		if emitErr := events.EmitAsyncBus(m.bus, StreamServed{
			Mount:        m.prefix,
			File:         name,
			Status:       status,
			Bytes:        sent,
			Duration:     took,
			Partial:      status == 206,
			Disconnected: gone,
			Err:          errString(err),
		}); emitErr != nil {
			// The response has already been sent, so an event-bus shutdown
			// cannot be returned to the player. Retain the scheduling failure
			// in diagnostics rather than silently losing lifecycle telemetry.
			// It is deliberately not sent to OnError: that hook describes
			// request failures, and this request succeeded independently of
			// its optional telemetry publication.
			m.eventErrors.Add(1)
		}
	}
}

// errString renders an error for transport in an event, tolerating nil.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// StreamServed is emitted once per completed video request.
//
// It is a package-level event type rather than a reuse of the framework's
// RequestFinished because the interesting fields here — whether the reply
// was partial, how many bytes actually left, whether the viewer walked
// away mid-stream — have no equivalent in a general HTTP event.
type StreamServed struct {
	// Mount is the URL prefix of the mount that served the request. It
	// distinguishes equal relative names when an application has several
	// media roots.
	Mount string

	// File is the path relative to the mount root.
	File string

	// Status is the status code written.
	Status int

	// Bytes is the number of body bytes actually sent, which is less than
	// Content-Length when the viewer disconnected.
	Bytes int64

	// Duration is how long the request took end to end.
	Duration time.Duration

	// Partial reports whether this was a 206.
	Partial bool

	// Disconnected reports that the viewer went away mid-transfer. This
	// is ordinary behaviour for video, not an error.
	Disconnected bool

	// Err is the internal error message, empty on success. It is safe to
	// log but was never sent to the client.
	Err string
}
