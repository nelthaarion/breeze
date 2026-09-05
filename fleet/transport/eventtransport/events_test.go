package eventtransport

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nelthaarion/breeze/v2/events"
	"github.com/nelthaarion/breeze/v2/fleet"
)

func TestInProcessExportAndSignature(t *testing.T) {
	bus := events.New()
	backend := events.NewInMemoryBackend(bus)
	got := make(chan events.RawMessage, 1)
	cancel, err := backend.Subscribe(
		DefaultSpansTopic,
		func(_ *events.Context, msg events.RawMessage) error {
			got <- msg
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	tr := New(Config{Bus: bus, IngestToken: "secret", ServiceName: "orders"})
	spans := []fleet.Span{
		{TraceID: strings.Repeat("a", 32), SpanID: strings.Repeat("1", 16), Service: "orders"},
	}
	if err := tr.ExportSpans(context.Background(), "ignored-inproc", spans); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-got:
		if !Authorized("secret", msg.Payload, msg.Meta) ||
			Authorized("wrong", msg.Payload, msg.Meta) {
			t.Fatalf("signature did not authenticate correctly: %+v", msg.Meta)
		}
		var decoded []fleet.Span
		if err := json.Unmarshal(msg.Payload, &decoded); err != nil || len(decoded) != 1 {
			t.Fatalf("decoded=%+v err=%v", decoded, err)
		}
	case <-time.After(time.Second):
		t.Fatal("span event not delivered")
	}
}

func TestPropagationRoundTrip(t *testing.T) {
	tr := New(Config{ServiceName: "gateway"})
	tc := fleet.NewTraceContext()
	tc.ParentSpanID = tc.NewChildSpanID()
	bag := fleet.Baggage{"order_id": "123"}
	carrier := fleet.MapCarrier{}
	tr.Inject(tc, bag, carrier)
	gotTC, gotBag, ok := tr.Extract(carrier)
	if !ok || gotTC != tc || gotBag["order_id"] != "123" ||
		carrier[fleet.HeaderService] != "gateway" {
		t.Fatalf("round trip tc=%+v bag=%+v ok=%v carrier=%+v", gotTC, gotBag, ok, carrier)
	}
}
