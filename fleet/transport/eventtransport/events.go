// Package eventtransport exports fleet telemetry through Breeze's topic-based
// events.Backend seam. With events.NewInMemoryBackend this is a pure in-process
// function-call path; broker and WebSocket backends can implement the same seam
// without changing Tracer or Aggregator.
package eventtransport

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/nelthaarion/breeze/v2/events"
	"github.com/nelthaarion/breeze/v2/fleet"
)

const (
	DefaultSpansTopic     = "fleet.spans"
	DefaultHeartbeatTopic = "fleet.heartbeat"
	MetaSignature         = "x-fleet-signature"
)

type Config struct {
	Backend        events.Backend
	Bus            *events.Bus
	SpansTopic     string
	HeartbeatTopic string
	IngestToken    string
	ServiceName    string
}

type Transport struct {
	backend        events.Backend
	spansTopic     string
	heartbeatTopic string
	token          string
	service        string
}

func New(cfg Config) *Transport {
	backend := cfg.Backend
	if backend == nil {
		backend = events.NewInMemoryBackend(cfg.Bus)
	}
	if cfg.SpansTopic == "" {
		cfg.SpansTopic = DefaultSpansTopic
	}
	if cfg.HeartbeatTopic == "" {
		cfg.HeartbeatTopic = DefaultHeartbeatTopic
	}
	return &Transport{
		backend:        backend,
		spansTopic:     cfg.SpansTopic,
		heartbeatTopic: cfg.HeartbeatTopic,
		token:          cfg.IngestToken,
		service:        cfg.ServiceName,
	}
}

func (*Transport) Name() string { return "events" }
func (t *Transport) Inject(tc fleet.TraceContext, bag fleet.Baggage, c fleet.Carrier) {
	fleet.InjectInto(tc, bag, t.service, c)
}

func (*Transport) Extract(c fleet.Carrier) (fleet.TraceContext, fleet.Baggage, bool) {
	return fleet.ExtractFrom(c)
}

func (t *Transport) ExportSpans(ctx context.Context, _ string, spans []fleet.Span) error {
	if len(spans) == 0 {
		return nil
	}
	return t.publish(ctx, t.spansTopic, spans)
}

func (t *Transport) ExportHeartbeat(ctx context.Context, _ string, hb fleet.Heartbeat) error {
	return t.publish(ctx, t.heartbeatTopic, hb)
}

func (t *Transport) publish(ctx context.Context, topic string, value any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	meta := events.Meta{fleet.HeaderService: t.service}
	if t.token != "" {
		meta[MetaSignature] = Sign(t.token, payload)
	}
	return t.backend.Publish(topic, payload, meta)
}

func Sign(token string, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(token))
	_, _ = mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

func Authorized(token string, payload []byte, meta events.Meta) bool {
	if token == "" {
		return true
	}
	want := Sign(token, payload)
	got := meta[MetaSignature]
	return got != "" && hmac.Equal([]byte(got), []byte(want))
}

func (t *Transport) Close() error {
	if t == nil || t.backend == nil {
		return nil
	}
	return t.backend.Close()
}

var ErrUnauthorized = errors.New("eventtransport: invalid fleet signature")
