// Package httptransport exports fleet spans over plain HTTP (§5A.2).
//
// This is the correctness baseline for every other transport: the wire format
// here — gzip-or-plain JSON POSTed to /fleet/api/spans and /fleet/api/heartbeat,
// with the traceparent and x-breeze-service headers for propagation — is what the
// conformance suite (§5A.7) checks the others against, and what a non-Go service
// implements when it wants to join a fleet.
//
// It is also the transport with nothing to go wrong: no broker, no persistent
// connection, no schema. If tracing misbehaves under another transport, pointing
// a service at this one is the way to find out whether the problem is the
// transport or the tracing.
//
// # Which HTTP client
//
// Requests go through breeze/client, the framework's own gnet-based client,
// rather than net/http. A Breeze service already runs a gnet event loop for its
// inbound traffic; exporting spans through the same engine keeps outbound calls
// on that loop instead of starting net/http's separate goroutine-per-connection
// machinery alongside it just to POST a batch of spans every second.
//
// The name "http" refers to the wire protocol, which is ordinary HTTP/1.1 and
// interoperates with any server or client — the choice of engine underneath is
// not visible to the aggregator.
//
// # Isolation
//
// This package imports only the standard library, breeze/client, and fleet
// itself, so importing it adds no third-party dependency to a build (§16). The
// transports that *do* need one (gRPC, brokers) are separate packages precisely
// so this stays true.
package httptransport

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nelthaarion/breeze/v2/client"
	"github.com/nelthaarion/breeze/v2/fleet"
)

// Path suffixes appended to the configured aggregator URL. Kept as constants
// because the aggregator's own route registration (§8.1) must match them exactly,
// and a typo on either side produces a 404 that looks like an outage.
const (
	PathSpans     = "/api/spans"
	PathHeartbeat = "/api/heartbeat"
)

// gzipMinBytes is the payload size above which a batch is compressed.
//
// Below roughly a kilobyte, gzip's framing plus the CPU cost outweighs the
// bytes saved, and span batches are extremely compressible (repeated service
// names, routes, and hex ids) so the savings above that threshold are large.
// A single heartbeat is always below it and is therefore never compressed.
const gzipMinBytes = 1024

// Config configures the transport.
type Config struct {
	// Client performs the requests. Nil gets one from newDefaultClient,
	// sized for this workload.
	//
	// Supplying your own is how to point exports at a client with custom TLS
	// settings, or to share one client — and so one event loop — across
	// several transports in the same process.
	Client *client.Client

	// Timeout bounds a single request. Also applied to the default client.
	Timeout time.Duration

	// IngestToken is the write credential from §11.1, sent as X-Fleet-Token.
	// Empty sends no header, which an aggregator with no token configured
	// accepts — that combination is for local development, and the
	// aggregator warns about it at startup rather than failing.
	IngestToken string

	// ServiceName is written as x-breeze-service on exports so the
	// aggregator can attribute a batch even if it fails to parse (§4).
	ServiceName string

	// Gzip compresses span batches over gzipMinBytes. On by default via New;
	// set false only when something between service and aggregator mishandles
	// Content-Encoding.
	Gzip bool
}

// Transport implements fleet.Transport over HTTP.
type Transport struct {
	cfg    Config
	client *client.Client

	// bufPool reuses the marshal/compress scratch buffers. Export runs once
	// per flush interval on one goroutine, so this is not hot-path pooling;
	// it exists because span batches are large (up to MaxBatchSize spans with
	// nested timelines) and letting each flush allocate a fresh multi-hundred-
	// kilobyte buffer produces exactly the sawtooth GC pressure the framework
	// avoids elsewhere.
	bufPool sync.Pool
}

// New returns a Transport for cfg.
func New(cfg Config) *Transport {
	if cfg.Timeout <= 0 {
		cfg.Timeout = fleet.DefaultExportTimeout
	}
	cli := cfg.Client
	if cli == nil {
		cli = newDefaultClient(cfg.Timeout)
	}
	return &Transport{
		cfg:    cfg,
		client: cli,
		bufPool: sync.Pool{
			New: func() any { return new(bytes.Buffer) },
		},
	}
}

// NewWithGzip returns a Transport with compression enabled — the recommended
// construction, and what a config-driven setup uses.
func NewWithGzip(cfg Config) *Transport {
	cfg.Gzip = true
	return New(cfg)
}

// newDefaultClient builds a client suitable for talking to one aggregator.
//
// The idle-connection budget is deliberately tiny: a Tracer makes at most one
// export and one heartbeat request per flush interval, to a single host, so the
// package default of 64 would reserve capacity for a fanout this workload never
// has. Two is enough for an export and a heartbeat overlapping, and keep-alive
// means those sockets are reused instead of re-handshaken every second.
//
// MaxResponseBytes is small for the same reason: replies to these two endpoints
// are a status code and at most a short JSON error. A misconfigured URL pointing
// at something that returns megabytes should fail rather than have a tracing
// goroutine buffer it.
func newDefaultClient(timeout time.Duration) *client.Client {
	return client.New(client.Config{
		Timeout:             timeout,
		DialTimeout:         timeout,
		MaxIdleConnsPerHost: 2,
		MaxResponseBytes:    64 << 10,
		UserAgent:           "breeze-fleet/1",
	})
}

// Name identifies this transport in logs and metrics.
func (t *Transport) Name() string { return "http" }

// Inject writes trace context and baggage onto c.
//
// Delegates to fleet's shared header encoding rather than writing the headers
// here, so this transport cannot drift from the wire format the others follow.
func (t *Transport) Inject(tc fleet.TraceContext, baggage fleet.Baggage, c fleet.Carrier) {
	fleet.InjectInto(tc, baggage, t.cfg.ServiceName, c)
}

// Extract reads trace context and baggage back off c.
func (t *Transport) Extract(c fleet.Carrier) (fleet.TraceContext, fleet.Baggage, bool) {
	return fleet.ExtractFrom(c)
}

// ExportSpans POSTs a batch of spans to the aggregator.
func (t *Transport) ExportSpans(ctx context.Context, addr string, spans []fleet.Span) error {
	if len(spans) == 0 {
		return nil
	}
	// The HTTP wire format is the spec's plain JSON array. SpanBatch is only
	// the in-process events envelope; sending it here makes the aggregator's
	// transport-neutral ingestion endpoint reject every otherwise valid batch.
	return t.post(ctx, addr+PathSpans, spans, t.cfg.Gzip)
}

// ExportHeartbeat POSTs one heartbeat.
//
// Never compressed: a heartbeat is a few hundred bytes, so gzip would add
// framing and a CPU cost to a payload that cannot benefit.
func (t *Transport) ExportHeartbeat(ctx context.Context, addr string, hb fleet.Heartbeat) error {
	return t.post(ctx, addr+PathHeartbeat, hb, false)
}

// post marshals payload and sends it, returning an error for any non-2xx reply.
func (t *Transport) post(ctx context.Context, url string, payload any, allowGzip bool) error {
	buf := t.bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer func() {
		// Very large buffers are dropped rather than pooled: one
		// unusually big batch would otherwise pin its peak size for the
		// process's lifetime, which is the classic sync.Pool footgun.
		if buf.Cap() <= 1<<20 {
			t.bufPool.Put(buf)
		}
	}()

	if err := json.NewEncoder(buf).Encode(payload); err != nil {
		// Unreachable for these types, but returning the error rather
		// than ignoring it means a future field that cannot marshal
		// surfaces as an export failure instead of silently sending
		// nothing.
		return fmt.Errorf("fleet/httptransport: encode: %w", err)
	}

	body := buf.Bytes()
	compressed := false
	if allowGzip && len(body) >= gzipMinBytes {
		gz := t.bufPool.Get().(*bytes.Buffer)
		gz.Reset()
		zw := gzip.NewWriter(gz)
		if _, err := zw.Write(body); err == nil && zw.Close() == nil {
			body = gz.Bytes()
			compressed = true
			defer func() {
				if gz.Cap() <= 1<<20 {
					t.bufPool.Put(gz)
				}
			}()
		} else {
			// Compression failing is not a reason to lose the batch;
			// send it uncompressed.
			t.bufPool.Put(gz)
		}
	}

	// body aliases a pooled buffer, so the request must be fully written
	// before this function returns and that buffer is recycled. Do is
	// synchronous, which is what makes passing it directly safe.
	req := client.NewRequest(http.MethodPost, url, body).
		WithContext(ctx).
		SetHeader("Content-Type", "application/json")
	if compressed {
		req.SetHeader("Content-Encoding", "gzip")
	}
	if t.cfg.IngestToken != "" {
		req.SetHeader(fleet.HeaderIngestToken, t.cfg.IngestToken)
	}
	if t.cfg.ServiceName != "" {
		req.SetHeader(fleet.HeaderService, t.cfg.ServiceName)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("fleet/httptransport: post %s: %w", url, err)
	}

	// No draining or closing needed here: breeze/client reads the body to
	// completion before returning, so there is no open reader holding the
	// connection out of the pool.
	if !resp.OK() {
		return &ExportError{StatusCode: resp.Status, URL: url}
	}
	return nil
}

// ExportError reports a non-2xx reply from the aggregator.
//
// A typed error so the Tracer's retry logic can distinguish cases that retrying
// will never fix from ones it will — see Permanent.
type ExportError struct {
	StatusCode int
	URL        string
}

func (e *ExportError) Error() string {
	return fmt.Sprintf("fleet/httptransport: aggregator returned %d for %s", e.StatusCode, e.URL)
}

// Permanent reports whether retrying this request is pointless.
//
// A 401 (bad ingest token) or 400 (malformed payload) will fail identically
// forever, so the spans should be dropped and the operator told, rather than
// retried until they age out of the buffer. A 429 or 503 is the opposite:
// exactly what backoff exists for. 404 counts as permanent because it means the
// URL is wrong, not that the aggregator is busy.
func (e *ExportError) Permanent() bool {
	switch e.StatusCode {
	case http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusRequestEntityTooLarge:
		return true
	default:
		return false
	}
}

// NormalizeAddr trims a configured aggregator URL into the form the export paths
// are appended to.
//
// Accepts "http://host:9000", "http://host:9000/", and "http://host:9000/fleet"
// alike, and supplies the http:// scheme when a bare host:port is given — all
// four are what people actually write in config files, and the failure mode
// without this is a request to "host:9000/api/spans" that fails to parse.
func NormalizeAddr(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	return strings.TrimSuffix(addr, "/")
}
