package aggregator

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nelthaarion/breeze"
	"github.com/nelthaarion/breeze/events"
	"github.com/nelthaarion/breeze/fleet"
	"github.com/nelthaarion/breeze/fleet/transport/eventtransport"
)

func TestAggregatorConsumesSignedInProcessEvents(t *testing.T) {
	bus := events.New()
	router := breeze.NewRouter()
	cfg := DefaultConfig()
	cfg.EventsBus = bus
	cfg.IngestToken = "secret"
	a := InstallAggregator(nil, router, cfg)
	defer a.Close(context.Background())

	transport := eventtransport.New(eventtransport.Config{Bus: bus, IngestToken: "secret", ServiceName: "orders"})
	span := fleet.Span{TraceID: strings.Repeat("b", 32), SpanID: strings.Repeat("2", 16), Service: "orders", Status: 200}
	if err := transport.ExportSpans(context.Background(), "", []fleet.Span{span}); err != nil {
		t.Fatal(err)
	}
	if err := transport.ExportHeartbeat(context.Background(), "", fleet.Heartbeat{Service: "orders", InstanceID: "one"}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if a.Store().Stats().Spans == 1 && len(a.Registry().Snapshot(time.Now())) == 1 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("events not consumed: stats=%+v registry=%+v", a.Store().Stats(), a.Registry().Snapshot(time.Now()))
}

func TestAggregatorRejectsUnsignedEventsWhenTokenConfigured(t *testing.T) {
	bus := events.New()
	a := InstallAggregator(nil, breeze.NewRouter(), Config{EventsBus: bus, IngestToken: "secret"})
	defer a.Close(context.Background())
	transport := eventtransport.New(eventtransport.Config{Bus: bus})
	span := fleet.Span{TraceID: strings.Repeat("c", 32), SpanID: strings.Repeat("3", 16), Service: "orders"}
	_ = transport.ExportSpans(context.Background(), "", []fleet.Span{span})
	time.Sleep(20 * time.Millisecond)
	if got := a.Store().Stats().Spans; got != 0 {
		t.Fatalf("unsigned span accepted: %d", got)
	}
}
