// Package client provides Breeze's outbound HTTP client, built on gnet for
// connection-pool management and I/O.
//
// # Why gnet, not net/http
//
// The server side of Breeze is built on gnet. Using the same event-loop engine
// for outbound calls means both directions share one non-blocking I/O model and
// one connection-pooling strategy, rather than the server being event-loop-based
// while every outgoing call spins up net/http's goroutine-per-connection
// machinery beside it.
//
// The cost of that choice is this file: gnet is a raw TCP byte-stream engine
// with no notion of HTTP, so request serialisation and response parsing are
// implemented here ([buildHTTPRequest], [parseHTTPResponse]) rather than
// inherited from the standard library. That is a deliberate trade, and it is
// the reason for the limitations below.
//
// # Limitations compared to net/http
//
//   - HTTP/1.1 only. No HTTP/2: that would mean implementing HPACK and stream
//     multiplexing here, which is far past the point of diminishing returns for
//     service-to-service JSON traffic.
//   - TLS works, but the handshake is done by crypto/tls (via [tls.Dial]) and
//     the resulting connection is handed to gnet with Enroll. gnet itself has
//     no TLS support.
//   - Chunked responses are decoded; chunked *request* bodies are not sent —
//     requests always carry a Content-Length.
//   - Redirects are not followed. A 3xx is returned as-is.
//   - Response bodies are fully buffered, so this is the wrong tool for
//     streaming (SSE, large downloads). Use net/http directly for those.
//   - One in-flight request per connection. Responses are correlated to
//     requests by connection, not by a pipelining sequence number, so
//     HTTP pipelining is not supported.
//
// # Usage
//
//	c := client.New()
//	resp, err := c.Get("http://auth-service/verify")
//	if err != nil { return err }
//	if resp.OK() { ... }
//
// With headers (what tracing uses to inject context):
//
//	req := client.NewRequest("POST", url, body).
//	    SetHeader("Content-Type", "application/json").
//	    SetHeader("Traceparent", tc.String())
//	resp, err := c.Do(req)
package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	gnet "github.com/panjf2000/gnet/v2"
)

// ── Sentinel errors ────────────────────────────────────────────────────────

var (
	// ErrNilRequest is returned by [Client.Do] when handed a nil request.
	ErrNilRequest = errors.New("client: nil request")

	// ErrNoURL is returned when a request has no URL.
	ErrNoURL = errors.New("client: request has no URL")

	// ErrResponseTooLarge is returned when a response body exceeds
	// [Config.MaxResponseBytes].
	ErrResponseTooLarge = errors.New("client: response body exceeds limit")
)

// ── ClientRequest ──────────────────────────────────────────────────────────

// ClientRequest describes an outbound request.
//
// It is a mutable value rather than the immutable *http.Request because
// trace context must be injected into a request by code that did not create
// it. A struct with Set/Get header methods is what lets a generic Carrier
// write to it without knowing what protocol is underneath.
type ClientRequest struct {
	// Method is the HTTP method. Empty means GET.
	Method string

	// URL is the absolute target URL.
	URL string

	// Body is the request body, or nil.
	Body []byte

	header http.Header
	ctx    context.Context
}

// NewRequest returns a request. A nil body is fine.
func NewRequest(method, rawURL string, body []byte) *ClientRequest {
	return &ClientRequest{
		Method: method,
		URL:    rawURL,
		Body:   body,
		header: make(http.Header, 4),
	}
}

// SetHeader sets a header, replacing any existing value. Returns the request
// for chaining. Keys are canonicalised via http.Header.
func (r *ClientRequest) SetHeader(key, value string) *ClientRequest {
	if r == nil {
		return r
	}
	if r.header == nil {
		r.header = make(http.Header, 4)
	}
	r.header.Set(key, value)
	return r
}

// AddHeader appends a value to a header rather than replacing it.
func (r *ClientRequest) AddHeader(key, value string) *ClientRequest {
	if r == nil {
		return r
	}
	if r.header == nil {
		r.header = make(http.Header, 4)
	}
	r.header.Add(key, value)
	return r
}

// GetHeader returns the first value for key and whether it was present.
func (r *ClientRequest) GetHeader(key string) (string, bool) {
	if r == nil || r.header == nil {
		return "", false
	}
	vs := r.header.Values(key)
	if len(vs) == 0 {
		return "", false
	}
	return vs[0], true
}

// Header returns the underlying header map directly (not a copy) so a Carrier
// adapter can wrap it without allocation. Do not mutate it concurrently with
// an in-flight call.
func (r *ClientRequest) Header() http.Header {
	if r == nil {
		return nil
	}
	if r.header == nil {
		r.header = make(http.Header, 4)
	}
	return r.header
}

// WithContext attaches a context. Cancellation and deadline from ctx compose
// with [Config.Timeout]: whichever fires first ends the call.
func (r *ClientRequest) WithContext(ctx context.Context) *ClientRequest {
	if r == nil {
		return r
	}
	r.ctx = ctx
	return r
}

// Context returns the request context, or context.Background if none was set.
func (r *ClientRequest) Context() context.Context {
	if r == nil || r.ctx == nil {
		return context.Background()
	}
	return r.ctx
}

// ── Response ───────────────────────────────────────────────────────────────

// Response is a completed response with its body already read into memory.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// OK reports whether Status is in the 2xx range.
func (r *Response) OK() bool {
	return r != nil && r.Status >= 200 && r.Status < 300
}

// String returns the body as a string.
func (r *Response) String() string {
	if r == nil {
		return ""
	}
	return string(r.Body)
}

// ── Config ─────────────────────────────────────────────────────────────────

// Config tunes a [Client]. The zero value is valid: every field falls back
// to the corresponding [DefaultConfig] value.
type Config struct {
	// Timeout bounds the whole call (connect + write + read). Default 30s.
	Timeout time.Duration

	// MaxIdleConnsPerHost is the idle-connection budget per upstream host.
	// Default 64. Unlike net/http's default of 2, 64 is sized for a service
	// that calls the same few upstreams on every request.
	MaxIdleConnsPerHost int

	// DialTimeout bounds establishing the TCP (or TLS) connection. Default 5s.
	DialTimeout time.Duration

	// MaxResponseBytes caps the response body. Default 32 MiB.
	MaxResponseBytes int64

	// UserAgent is sent on every request unless the request sets its own.
	// Default "breeze-client/1".
	UserAgent string

	// TLSConfig, if non-nil, is used for HTTPS connections instead of the
	// default tls.Config. Set this to supply custom certificates or to
	// disable hostname verification in a trusted internal network.
	TLSConfig *tls.Config
}

const (
	DefaultTimeout             = 30 * time.Second
	DefaultMaxIdleConnsPerHost = 64
	DefaultDialTimeout         = 5 * time.Second
	DefaultMaxResponseBytes    = 32 << 20 // 32 MiB
	DefaultUserAgent           = "breeze-client/1"
)

// DefaultConfig returns the configuration [New] uses when given none.
func DefaultConfig() Config {
	return Config{
		Timeout:             DefaultTimeout,
		MaxIdleConnsPerHost: DefaultMaxIdleConnsPerHost,
		DialTimeout:         DefaultDialTimeout,
		MaxResponseBytes:    DefaultMaxResponseBytes,
		UserAgent:           DefaultUserAgent,
	}
}

func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.Timeout <= 0 {
		c.Timeout = d.Timeout
	}
	if c.MaxIdleConnsPerHost <= 0 {
		c.MaxIdleConnsPerHost = d.MaxIdleConnsPerHost
	}
	if c.DialTimeout <= 0 {
		c.DialTimeout = d.DialTimeout
	}
	if c.MaxResponseBytes <= 0 {
		c.MaxResponseBytes = d.MaxResponseBytes
	}
	if c.UserAgent == "" {
		c.UserAgent = d.UserAgent
	}
	return c
}

// ── Connection pool ────────────────────────────────────────────────────────

// hostPool holds idle gnet connections for one host:port.
type hostPool struct {
	idle chan gnet.Conn
}

// connContext is the per-connection state, stored as the gnet connection's
// user context and living for as long as the connection itself — including
// across pooled reuse, which is what lets a partially-received response
// survive between requests.
type connContext struct {
	// buf accumulates bytes from the server until a complete response has
	// been parsed. Only ever touched from the event loop (OnTraffic and
	// OnClose run on it), so it needs no lock.
	//
	// It deliberately outlives a single request: any bytes beyond the
	// response just parsed belong to the connection, not to the request,
	// and discarding them would corrupt whatever is read next.
	buf []byte

	// respCh carries the result of the in-flight request. Capacity 1 so the
	// event loop can always deposit a result without blocking; a caller that
	// has already given up simply never reads it.
	//
	// One channel serves every request on the connection, which is safe
	// because only one request is in flight at a time: a connection is
	// removed from the idle pool for the duration of a request, and is
	// closed rather than returned if that request is abandoned.
	respCh chan connResult
}

type connResult struct {
	resp *Response
	err  error
}

// ── Event handler (shared across all connections from one Client) ──────────

type clientHandler struct {
	maxBody int64
}

func (h *clientHandler) OnBoot(gnet.Engine) gnet.Action { return gnet.None }
func (h *clientHandler) OnShutdown(gnet.Engine)         {}
func (h *clientHandler) OnTick() (time.Duration, gnet.Action) {
	// Keep-alive tick; idle connections rely on TCP keep-alive probes rather
	// than application-level pinging, so nothing to do here.
	return time.Minute, gnet.None
}

func (h *clientHandler) OnOpen(c gnet.Conn) ([]byte, gnet.Action) {
	// Context was already set by DialContext; nothing to do here.
	return nil, gnet.None
}

func (h *clientHandler) OnClose(c gnet.Conn, err error) gnet.Action {
	// If a request is in flight, unblock Do() with the connection error.
	ctx, ok := c.Context().(*connContext)
	if !ok || ctx == nil {
		return gnet.None
	}
	var ferr error
	if err != nil {
		ferr = fmt.Errorf("client: connection closed: %w", err)
	} else {
		ferr = errors.New("client: connection closed by server")
	}
	// Non-blocking send: if the channel is already full, the result of the
	// last request was already delivered (e.g. server closed after response).
	select {
	case ctx.respCh <- connResult{err: ferr}:
	default:
	}
	return gnet.None
}

func (h *clientHandler) OnTraffic(c gnet.Conn) gnet.Action {
	ctx, ok := c.Context().(*connContext)
	if !ok || ctx == nil {
		// Unexpected data on a connection with no context; discard.
		_, _ = c.Discard(c.InboundBuffered())
		return gnet.None
	}

	n := c.InboundBuffered()
	if n == 0 {
		return gnet.None
	}

	// Next advances the inbound buffer; gnet guarantees the returned slice
	// stays valid until the next event-loop callback on this connection.
	raw, err := c.Next(n)
	if err != nil {
		select {
		case ctx.respCh <- connResult{err: fmt.Errorf("client: read: %w", err)}:
		default:
		}
		return gnet.Close
	}

	// Append into ctx.buf (which persists across partial reads).
	ctx.buf = append(ctx.buf, raw...)

	resp, consumed, done, perr := parseHTTPResponse(ctx.buf, h.maxBody)
	if !done {
		// Response not yet complete; wait for more data.
		return gnet.None
	}

	// Advance past the consumed bytes; preserve any pipelined data.
	ctx.buf = ctx.buf[consumed:]

	select {
	case ctx.respCh <- connResult{resp: resp, err: perr}:
	default:
		// Do() already timed out and is no longer listening.
	}

	if perr != nil {
		// An oversized or malformed body leaves this connection at an
		// unknown offset in the stream, so it cannot be safely reused for
		// the next request. Close it rather than return it to the pool.
		return gnet.Close
	}
	return gnet.None
}

// ── Client ─────────────────────────────────────────────────────────────────

// Client is a pooled HTTP client built on gnet, safe for concurrent use.
// Create one per process (or per upstream if different timeouts are needed)
// and share it.
type Client struct {
	cfg     Config
	gcli    *gnet.Client
	handler *clientHandler
	pools   sync.Map // "host:port" → *hostPool
	started int32    // 0 = not started, 1 = started (atomic)
	startMu sync.Mutex
}

// Default is a ready-to-use Client with [DefaultConfig]. Safe to use as-is;
// the gnet event loop starts lazily on the first call to Do.
var Default = New()

// New returns a Client. The variadic config matches the convention used
// elsewhere in the framework: New() or New(cfg) are both valid.
func New(cfg ...Config) *Client {
	var c Config
	if len(cfg) > 0 {
		c = cfg[0]
	}
	c = c.withDefaults()

	h := &clientHandler{maxBody: c.MaxResponseBytes}
	gcli, err := gnet.NewClient(h)
	if err != nil {
		// NewClient only fails if the options are contradictory; with our
		// defaults that cannot happen at runtime.
		panic(fmt.Sprintf("client: gnet.NewClient: %v", err))
	}
	return &Client{cfg: c, gcli: gcli, handler: h}
}

// lazyStart starts the gnet event loop on the first call.
func (c *Client) lazyStart() error {
	if atomic.LoadInt32(&c.started) == 1 {
		return nil
	}
	c.startMu.Lock()
	defer c.startMu.Unlock()
	if atomic.LoadInt32(&c.started) == 1 {
		return nil
	}
	if err := c.gcli.Start(); err != nil {
		return fmt.Errorf("client: start event loop: %w", err)
	}
	atomic.StoreInt32(&c.started, 1)
	return nil
}

func (c *Client) pool(addr string) *hostPool {
	v, _ := c.pools.LoadOrStore(addr, &hostPool{
		idle: make(chan gnet.Conn, c.cfg.MaxIdleConnsPerHost),
	})
	return v.(*hostPool)
}

// getConn returns a connection for addr, preferring an idle pooled one. The
// third result reports whether the connection came from the pool, which
// [Client.Do] uses to decide whether a write failure is worth retrying.
//
// A pooled connection may have been closed by the peer since it was returned:
// the FIN is processed by the event loop, but the connection is still sitting
// in the idle channel. That race is unavoidable in any connection pool, and is
// why Do retries once on a pooled connection whose write fails.
func (c *Client) getConn(addr, hostname string, secure bool) (gnet.Conn, *connContext, bool, error) {
	p := c.pool(addr)

	// Try an idle connection first.
	select {
	case conn := <-p.idle:
		// Reuse the connection's existing state rather than replacing it, so
		// its read buffer survives. SafeContext is callable off the loop.
		state, ok := conn.SafeContext().(*connContext)
		if !ok || state == nil {
			// Should not happen; a connection always has its context set at
			// dial time. Rather than risk a nil dereference on the event
			// loop, drop this connection and dial a fresh one.
			conn.Close()
			break
		}
		// Discard any result left by a previous abandoned request so this
		// request cannot observe a stale response.
		select {
		case <-state.respCh:
		default:
		}
		return conn, state, true, nil
	default:
	}

	conn, state, err := c.dial(addr, hostname, secure)
	return conn, state, false, err
}

// dial opens a new connection to addr, wrapping it in TLS when secure.
func (c *Client) dial(addr, hostname string, secure bool) (gnet.Conn, *connContext, error) {
	state := &connContext{respCh: make(chan connResult, 1)}

	if secure {
		tlsCfg := c.cfg.TLSConfig
		if tlsCfg == nil {
			tlsCfg = &tls.Config{ServerName: hostname}
		} else {
			// Clone so callers can share a Config without races.
			tlsCfg = tlsCfg.Clone()
			if tlsCfg.ServerName == "" {
				tlsCfg.ServerName = hostname
			}
		}
		dialer := &net.Dialer{Timeout: c.cfg.DialTimeout}
		nc, err := tls.DialWithDialer(dialer, "tcp", addr, tlsCfg)
		if err != nil {
			return nil, nil, fmt.Errorf("client: tls dial %s: %w", addr, err)
		}
		conn, err := c.gcli.EnrollContext(nc, state)
		if err != nil {
			nc.Close()
			return nil, nil, fmt.Errorf("client: enroll tls conn %s: %w", addr, err)
		}
		return conn, state, nil
	}

	conn, err := c.gcli.DialContext("tcp", addr, state)
	if err != nil {
		return nil, nil, fmt.Errorf("client: dial %s: %w", addr, err)
	}
	return conn, state, nil
}

func (c *Client) putConn(addr string, conn gnet.Conn) {
	p := c.pool(addr)
	select {
	case p.idle <- conn:
	default:
		// Pool is full; close the excess connection.
		conn.Close()
	}
}

// Do performs the request and returns the response. The response body is
// fully buffered; the connection is returned to the pool for reuse.
func (c *Client) Do(req *ClientRequest) (*Response, error) {
	if req == nil {
		return nil, ErrNilRequest
	}
	if strings.TrimSpace(req.URL) == "" {
		return nil, ErrNoURL
	}

	if err := c.lazyStart(); err != nil {
		return nil, err
	}

	u, err := url.Parse(req.URL)
	if err != nil {
		return nil, fmt.Errorf("client: parse URL %q: %w", req.URL, err)
	}

	host := u.Hostname()
	port := u.Port()
	secure := u.Scheme == "https"
	if port == "" {
		if secure {
			port = "443"
		} else {
			port = "80"
		}
	}
	addr := net.JoinHostPort(host, port)

	conn, state, fromPool, err := c.getConn(addr, host, secure)
	if err != nil {
		return nil, err
	}

	// Serialise the request as raw HTTP/1.1 bytes.
	reqBytes := buildHTTPRequest(req, u, c.cfg.UserAgent)

	// AsyncWrite is concurrency-safe and queues the bytes for the event loop
	// to flush. The nil callback means "fire-and-forget"; errors surface via
	// the response channel (connection close → OnClose → error in respCh).
	if err := conn.AsyncWrite(reqBytes, nil); err != nil {
		conn.Close()
		if !fromPool {
			return nil, fmt.Errorf("client: write to %s: %w", addr, err)
		}
		// The pooled connection was already dead. Nothing was written, so
		// retrying on a fresh connection cannot duplicate a side effect.
		conn, state, err = c.dial(addr, host, secure)
		if err != nil {
			return nil, err
		}
		if err := conn.AsyncWrite(reqBytes, nil); err != nil {
			conn.Close()
			return nil, fmt.Errorf("client: write to %s: %w", addr, err)
		}
	}

	// Wait for the event loop to deliver the response.
	ctx := req.Context()
	timer := time.NewTimer(c.cfg.Timeout)
	defer timer.Stop()

	select {
	case result := <-state.respCh:
		if result.err != nil {
			// Do not return to pool; connection is broken.
			conn.Close()
			return nil, result.err
		}
		c.putConn(addr, conn)
		return result.resp, nil
	case <-ctx.Done():
		conn.Close()
		return nil, fmt.Errorf("client: %w", ctx.Err())
	case <-timer.C:
		conn.Close()
		return nil, fmt.Errorf("client: timeout waiting for response from %s", addr)
	}
}

// Get performs a GET.
func (c *Client) Get(rawURL string) (*Response, error) {
	return c.Do(NewRequest(http.MethodGet, rawURL, nil))
}

// Post performs a POST with the given content type.
func (c *Client) Post(rawURL, contentType string, body []byte) (*Response, error) {
	req := NewRequest(http.MethodPost, rawURL, body)
	if contentType != "" {
		req.SetHeader("Content-Type", contentType)
	}
	return c.Do(req)
}

// PostJSON performs a POST with Content-Type: application/json.
func (c *Client) PostJSON(rawURL string, body []byte) (*Response, error) {
	return c.Post(rawURL, "application/json", body)
}

// Close stops the gnet event loop and closes all idle connections. In-flight
// requests are unaffected.
func (c *Client) Close() error {
	if c == nil {
		return nil
	}
	return c.gcli.Stop()
}

// Config returns the client's effective configuration, with defaults applied.
func (c *Client) Config() Config {
	if c == nil {
		return Config{}
	}
	return c.cfg
}

// ── HTTP wire-format helpers ───────────────────────────────────────────────

var crlfcrlf = []byte("\r\n\r\n")

// buildHTTPRequest serialises req as an HTTP/1.1 request line + headers +
// body. Always sends Connection: keep-alive and a Content-Length when there
// is a body; never sends Transfer-Encoding: chunked (see package doc).
func buildHTTPRequest(req *ClientRequest, u *url.URL, userAgent string) []byte {
	method := req.Method
	if method == "" {
		method = "GET"
	}

	path := u.RequestURI()
	if path == "" {
		path = "/"
	}

	// Pre-size the buffer: request line (~40 B) + Host (~30 B) +
	// Content-Length (~25 B) + headers (variable) + CRLF + body.
	var b bytes.Buffer
	b.Grow(128 + len(req.Body))

	fmt.Fprintf(&b, "%s %s HTTP/1.1\r\nHost: %s\r\n", method, path, u.Host)

	if len(req.Body) > 0 {
		fmt.Fprintf(&b, "Content-Length: %d\r\n", len(req.Body))
	}

	// Caller-supplied headers (including tracing headers).
	for k, vs := range req.header {
		for _, v := range vs {
			fmt.Fprintf(&b, "%s: %s\r\n", k, v)
		}
	}

	// Default headers if not already set by the caller.
	if req.header.Get("User-Agent") == "" {
		fmt.Fprintf(&b, "User-Agent: %s\r\n", userAgent)
	}
	if req.header.Get("Accept") == "" {
		b.WriteString("Accept: */*\r\n")
	}

	b.WriteString("Connection: keep-alive\r\n\r\n")

	if len(req.Body) > 0 {
		b.Write(req.Body)
	}
	return b.Bytes()
}

// parseHTTPResponse attempts to parse one complete HTTP/1.1 response from buf.
//
// It returns the parsed response, how many bytes of buf it consumed, whether a
// complete response was available at all, and any error that makes the response
// unusable. The four results separate the two distinct "no response yet"
// cases that a stream parser has to tell apart: done=false means *keep
// reading, nothing is wrong*, while a non-nil error means *stop, this response
// can never be valid*. Collapsing those into one signal is how stream parsers
// end up either hanging on a malformed response or discarding a partial one.
//
// buf is never mutated, so the caller can safely retain it across calls.
func parseHTTPResponse(buf []byte, maxBody int64) (resp *Response, consumed int, done bool, err error) {
	// Wait until the whole header block has arrived.
	headerEnd := bytes.Index(buf, crlfcrlf)
	if headerEnd < 0 {
		return nil, 0, false, nil
	}

	// Include the final CRLF so every header line in headerBlock is
	// terminated. Slicing at headerEnd instead would leave the last header
	// without its "\r\n", the scan below would stop before reading it, and a
	// response whose Content-Length happened to be last would never be
	// recognised as complete.
	headerBlock := buf[:headerEnd+2]

	// ── Status line ──────────────────────────────────────────────────────
	lineEnd := bytes.IndexByte(headerBlock, '\r')
	if lineEnd < 0 {
		return nil, 0, true, errors.New("client: malformed status line")
	}
	statusLine := headerBlock[:lineEnd]

	// "HTTP/1.1 200 OK" — only the numeric code is of interest.
	sp1 := bytes.IndexByte(statusLine, ' ')
	if sp1 < 0 {
		return nil, 0, true, errors.New("client: malformed status line")
	}
	tail := statusLine[sp1+1:]
	codeSlice := tail
	if sp2 := bytes.IndexByte(tail, ' '); sp2 >= 0 {
		codeSlice = tail[:sp2]
	}
	statusCode, cerr := strconv.Atoi(b2s(codeSlice))
	if cerr != nil || statusCode < 100 || statusCode > 599 {
		return nil, 0, true, fmt.Errorf("client: bad status code %q", codeSlice)
	}

	// ── Header scan (one pass) ────────────────────────────────────────────
	hdr := make(http.Header, 8)
	contentLength := int64(-1)
	chunked := false

	pos := lineEnd + 2 // skip status line CRLF
	for pos < len(headerBlock) {
		end := bytes.IndexByte(headerBlock[pos:], '\r')
		if end < 0 {
			break
		}
		line := headerBlock[pos : pos+end]
		pos += end + 2
		if len(line) == 0 {
			break
		}
		colon := bytes.IndexByte(line, ':')
		if colon <= 0 {
			continue
		}
		key := string(line[:colon])
		val := strings.TrimSpace(string(line[colon+1:]))
		hdr.Add(key, val)

		lower := strings.ToLower(key)
		switch lower {
		case "content-length":
			if contentLength < 0 {
				cl, e := strconv.ParseInt(val, 10, 64)
				if e == nil && cl >= 0 {
					contentLength = cl
				}
			}
		case "transfer-encoding":
			if strings.Contains(strings.ToLower(val), "chunked") {
				chunked = true
			}
		}
	}

	bodyStart := headerEnd + 4 // skip \r\n\r\n

	// ── Body ──────────────────────────────────────────────────────────────

	// 1xx, 204 and 304 carry no body by definition, regardless of what the
	// headers claim. Without this case such a response would never be
	// considered complete, and the call would block until it timed out.
	if statusCode == 204 || statusCode == 304 || statusCode < 200 {
		return &Response{Status: statusCode, Header: hdr}, bodyStart, true, nil
	}

	if chunked {
		body, n, cdone, cerr := decodeChunked(buf[bodyStart:], maxBody)
		if cerr != nil {
			return nil, 0, true, cerr
		}
		if !cdone {
			return nil, 0, false, nil
		}
		return &Response{Status: statusCode, Header: hdr, Body: body},
			bodyStart + n, true, nil
	}

	if contentLength == 0 {
		return &Response{Status: statusCode, Header: hdr}, bodyStart, true, nil
	}

	if contentLength > 0 {
		if contentLength > maxBody {
			// Fail as soon as the declared length is known to be too large,
			// rather than buffering the whole body first only to reject it.
			return nil, 0, true, fmt.Errorf("%w: %d > %d", ErrResponseTooLarge, contentLength, maxBody)
		}
		if int64(len(buf)-bodyStart) < contentLength {
			return nil, 0, false, nil // partial body; keep reading
		}
		body := make([]byte, contentLength)
		copy(body, buf[bodyStart:])
		return &Response{Status: statusCode, Header: hdr, Body: body},
			bodyStart + int(contentLength), true, nil
	}

	// Neither Content-Length nor chunked, so the body is delimited by the
	// server closing the connection (RFC 9112 §6.3, the "read until EOF"
	// case). OnClose surfaces that as an error to the waiting caller; this
	// parser cannot know where the body ends before then.
	return nil, 0, false, nil
}

// decodeChunked decodes a chunked transfer-encoded body from buf.
//
// It mirrors [parseHTTPResponse]'s four-result contract for the same reason: a
// truncated chunk stream (done=false) and an invalid one (err != nil) require
// opposite responses from the caller — wait, versus give up.
func decodeChunked(buf []byte, maxBody int64) (body []byte, consumed int, done bool, err error) {
	var result []byte
	pos := 0

	for {
		// Chunk header: hex size, optional ";extension", CRLF.
		nl := bytes.IndexByte(buf[pos:], '\n')
		if nl < 0 {
			return nil, 0, false, nil // header not fully arrived
		}
		sizeLine := b2s(buf[pos : pos+nl])
		if i := strings.IndexByte(sizeLine, ';'); i >= 0 {
			sizeLine = sizeLine[:i]
		}
		chunkSize, perr := strconv.ParseInt(strings.TrimSpace(sizeLine), 16, 64)
		if perr != nil || chunkSize < 0 {
			return nil, 0, true, fmt.Errorf("client: bad chunk size %q", sizeLine)
		}
		pos += nl + 1

		if chunkSize == 0 {
			// Terminal chunk, optionally followed by trailer fields and
			// then the final CRLF.
			for {
				nl := bytes.IndexByte(buf[pos:], '\n')
				if nl < 0 {
					return nil, 0, false, nil
				}
				line := bytes.TrimRight(buf[pos:pos+nl], "\r")
				pos += nl + 1
				if len(line) == 0 {
					return result, pos, true, nil
				}
				// Non-empty line is a trailer field; skip it.
			}
		}

		if int64(len(result))+chunkSize > maxBody {
			return nil, 0, true, fmt.Errorf("%w: chunked body exceeds %d", ErrResponseTooLarge, maxBody)
		}

		// Chunk data plus its trailing CRLF.
		if int64(len(buf)-pos) < chunkSize+2 {
			return nil, 0, false, nil
		}
		result = append(result, buf[pos:pos+int(chunkSize)]...)
		pos += int(chunkSize) + 2
	}
}

// b2s converts a byte slice to a string without allocation, using the same
// unsafe idiom as request.go.
func b2s(b []byte) string {
	return unsafe.String(unsafe.SliceData(b), len(b))
}
