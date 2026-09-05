// Package gnettransport provides a Breeze-native HTTP/1.1 export transport.
//
// Breeze's public client is already the gnet-backed HTTP client used by the
// baseline HTTP transport. Keeping this adapter separate gives configuration a
// stable transport name without inventing a second wire protocol.
package gnettransport

import (
	"context"
	"time"

	"github.com/nelthaarion/breeze/v2/client"
	"github.com/nelthaarion/breeze/v2/fleet"
	"github.com/nelthaarion/breeze/v2/fleet/transport/httptransport"
)

type Config struct {
	Client       *client.Client
	Timeout      time.Duration
	IngestToken  string
	ServiceName  string
	MaxIdleConns int
}

type Transport struct{ http *httptransport.Transport }

func New(cfg Config) *Transport {
	return &Transport{http: httptransport.New(httptransport.Config{
		Client: cfg.Client, Timeout: cfg.Timeout, IngestToken: cfg.IngestToken,
		ServiceName: cfg.ServiceName, Gzip: true,
	})}
}

func (*Transport) Name() string { return "gnet" }
func (t *Transport) Inject(tc fleet.TraceContext, b fleet.Baggage, c fleet.Carrier) {
	t.http.Inject(tc, b, c)
}

func (t *Transport) Extract(c fleet.Carrier) (fleet.TraceContext, fleet.Baggage, bool) {
	return t.http.Extract(c)
}

func (t *Transport) ExportSpans(ctx context.Context, addr string, spans []fleet.Span) error {
	return t.http.ExportSpans(ctx, addr, spans)
}

func (t *Transport) ExportHeartbeat(ctx context.Context, addr string, hb fleet.Heartbeat) error {
	return t.http.ExportHeartbeat(ctx, addr, hb)
}
