// Package wstransport implements the WebSocket fleet export transport.
//
// Spans are published over a persistent connection to the aggregator's
// /ws endpoint using the same envelope its hub already speaks: an "auth" frame
// with the ingest role, then one "publish" frame per batch. The aggregator side
// of this has existed since the hub was written; this package is the client
// half that dials it.
//
// The connection is established lazily on first export and reused. Export is
// called only from the Tracer's single background goroutine, so the common path
// takes an uncontended mutex and writes one frame.
//
// When AggregatorWSURL is empty the transport falls back to HTTP export. That
// keeps `fleet.transport: ws` working for a service that has not been given a
// WebSocket endpoint, which is how this package behaved when it was an HTTP
// facade — existing configurations keep working unchanged.
//
// Propagation (Inject/Extract) is always the W3C header encoding, identical to
// every other transport: trace context travels on the traced request, not on
// the export connection.
package wstransport

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nelthaarion/breeze/client"
	"github.com/nelthaarion/breeze/fleet"
	"github.com/nelthaarion/breeze/fleet/transport/eventtransport"
	"github.com/nelthaarion/breeze/fleet/transport/httptransport"
)

// DefaultTimeout bounds a dial, a write and a reply read independently.
const DefaultTimeout = 10 * time.Second

type Config struct {
	Client      *client.Client
	Timeout     time.Duration
	IngestToken string
	ServiceName string

	// AggregatorWSURL is the ws:// or wss:// URL of the aggregator's WebSocket
	// endpoint, typically "ws://host:port/fleet/ws". Empty means fall back to
	// HTTP export.
	AggregatorWSURL string

	// TLSConfig is used for wss:// only. nil means a default config verifying
	// the server against the system roots.
	TLSConfig *tls.Config
}

// Transport is a fleet.Transport that exports over WebSocket.
type Transport struct {
	// http is retained for propagation encoding and for the fallback path when
	// no WebSocket URL is configured.
	http *httptransport.Transport

	wsURL   string
	token   string
	service string
	timeout time.Duration
	tlsCfg  *tls.Config

	mu     sync.Mutex
	conn   *wsConn
	closed bool
}

func New(cfg Config) *Transport {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Transport{
		http: httptransport.New(httptransport.Config{
			Client: cfg.Client, Timeout: cfg.Timeout, IngestToken: cfg.IngestToken,
			ServiceName: cfg.ServiceName, Gzip: true,
		}),
		wsURL:   strings.TrimSpace(cfg.AggregatorWSURL),
		token:   cfg.IngestToken,
		service: cfg.ServiceName,
		timeout: timeout,
		tlsCfg:  cfg.TLSConfig,
	}
}

func (*Transport) Name() string { return "ws" }

func (t *Transport) Inject(tc fleet.TraceContext, b fleet.Baggage, c fleet.Carrier) {
	t.http.Inject(tc, b, c)
}

func (t *Transport) Extract(c fleet.Carrier) (fleet.TraceContext, fleet.Baggage, bool) {
	return t.http.Extract(c)
}

// ExportSpans publishes a batch on the spans topic.
func (t *Transport) ExportSpans(ctx context.Context, addr string, spans []fleet.Span) error {
	if len(spans) == 0 {
		return nil
	}
	if t.wsURL == "" {
		return t.http.ExportSpans(ctx, addr, spans)
	}
	return t.publish(ctx, eventtransport.DefaultSpansTopic, spans)
}

// ExportHeartbeat publishes one heartbeat on the heartbeat topic.
func (t *Transport) ExportHeartbeat(ctx context.Context, addr string, hb fleet.Heartbeat) error {
	if t.wsURL == "" {
		return t.http.ExportHeartbeat(ctx, addr, hb)
	}
	return t.publish(ctx, eventtransport.DefaultHeartbeatTopic, hb)
}

// publish sends one envelope, reconnecting once if the connection was already
// broken. The retry exists because a pooled connection that the aggregator
// closed while idle is indistinguishable from a live one until the write fails:
// without it, every aggregator restart would cost one dropped batch.
func (t *Transport) publish(ctx context.Context, topic string, value any) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("wstransport: marshal payload: %w", err)
	}
	frame, err := json.Marshal(wsEnvelope{Type: "publish", Topic: topic, Payload: payload})
	if err != nil {
		return fmt.Errorf("wstransport: marshal envelope: %w", err)
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return errors.New("wstransport: transport is closed")
	}

	if err := t.sendLocked(frame); err == nil {
		return nil
	} else if !t.retryable(err) {
		return err
	}

	// Drop the dead connection and try once on a fresh one.
	t.dropLocked()
	if err := ctx.Err(); err != nil {
		return err
	}
	return t.sendLocked(frame)
}

// sendLocked writes one frame on the live connection, dialing if needed.
// t.mu must be held.
func (t *Transport) sendLocked(frame []byte) error {
	if t.conn == nil {
		conn, err := t.connect()
		if err != nil {
			return err
		}
		t.conn = conn
	}
	if err := t.conn.writeText(frame, t.timeout); err != nil {
		t.dropLocked()
		return err
	}
	return nil
}

// retryable reports whether err is the kind of failure a reconnect can fix. A
// rejected handshake or a bad URL is not: retrying would fail identically and
// hide the real cause.
func (t *Transport) retryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errClosedByPeer) {
		return true
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "unsupported scheme"),
		strings.Contains(msg, "url has no host"),
		strings.Contains(msg, "handshake rejected"),
		strings.Contains(msg, "accept mismatch"),
		strings.Contains(msg, "authentication"),
		strings.Contains(msg, "marshal"):
		return false
	}
	return true
}

// connect dials and authenticates. The hub requires the ingest role before it
// will accept a publish, and replies auth_ok, so the reply is read and checked
// here rather than assumed: a rejected token must surface as an export error,
// not as spans that vanish.
func (t *Transport) connect() (*wsConn, error) {
	conn, err := dialWS(t.wsURL, t.timeout, t.tlsCfg)
	if err != nil {
		return nil, err
	}

	auth, err := json.Marshal(wsEnvelope{Type: "auth", Role: "ingest", Token: t.token})
	if err != nil {
		_ = conn.close()
		return nil, fmt.Errorf("wstransport: marshal auth: %w", err)
	}
	if err := conn.writeText(auth, t.timeout); err != nil {
		_ = conn.close()
		return nil, err
	}

	reply, err := conn.readText(t.timeout)
	if err != nil {
		_ = conn.close()
		// The hub closes with a policy-violation rather than replying when the
		// token is wrong, so a close here is most likely a rejected token.
		if errors.Is(err, errClosedByPeer) {
			return nil, ErrUnauthorized
		}
		return nil, err
	}

	var env wsEnvelope
	if err := json.Unmarshal(reply, &env); err != nil {
		_ = conn.close()
		return nil, fmt.Errorf("wstransport: malformed auth reply: %w", err)
	}
	if env.Type != "auth_ok" {
		_ = conn.close()
		if env.Error != "" {
			return nil, fmt.Errorf("wstransport: authentication refused: %s", env.Error)
		}
		return nil, ErrUnauthorized
	}
	return conn, nil
}

// dropLocked closes and forgets the current connection. t.mu must be held.
func (t *Transport) dropLocked() {
	if t.conn != nil {
		_ = t.conn.close()
		t.conn = nil
	}
}

// Close releases the connection. Safe to call more than once, and safe to call
// concurrently with an in-flight export.
func (t *Transport) Close() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	t.dropLocked()
	return nil
}

// ErrUnauthorized is returned when the aggregator refuses the ingest token.
var ErrUnauthorized = errors.New("wstransport: aggregator refused the ingest token")

// wsEnvelope is the client-side view of the hub's envelope. Only the fields
// this transport sends or reads are present; the tags match the hub's exactly.
type wsEnvelope struct {
	Type    string          `json:"type"`
	Role    string          `json:"role,omitempty"`
	Token   string          `json:"token,omitempty"`
	Topic   string          `json:"topic,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Error   string          `json:"error,omitempty"`
}
