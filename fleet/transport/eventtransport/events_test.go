package eventtransport

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nelthaarion/go-json-plus"

	"github.com/nelthaarion/breeze/v2/events"
	"github.com/nelthaarion/breeze/v2/fleet"
)

func TestInProcessExportAndSignature(t *testing.T) {
	bus := events.New()
	backend := events.NewInMemoryBackend(bus)
	got := make(chan events.RawMessage, 1)
	cancel, err := backend.Subscribe(DefaultSpansTopic, func(_ *events.Context, msg events.RawMessage) error {
		got <- msg
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	tr := New(Config{Bus: bus, IngestToken: "secret", ServiceName: "orders"})
	spans := []fleet.Span{{TraceID: strings.Repeat("a", 32), SpanID: strings.Repeat("1", 16), Service: "orders"}}
	if err := tr.ExportSpans(context.Background(), "ignored-inproc", spans); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-got:
		if !Authorized("secret", msg.Payload, msg.Meta) || Authorized("wrong", msg.Payload, msg.Meta) {
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
	if !ok || gotTC != tc || gotBag["order_id"] != "123" || carrier[fleet.HeaderService] != "gateway" {
		t.Fatalf("round trip tc=%+v bag=%+v ok=%v carrier=%+v", gotTC, gotBag, ok, carrier)
	}
}

// ctxBackend is a Backend that, like a network broker, can take the caller's
// context. It records which of its two publish methods was used.
type ctxBackend struct {
	gotCtx context.Context
	topic  string
	plain  atomic.Int64
}

func (b *ctxBackend) Publish(string, []byte, events.Meta) error { b.plain.Add(1); return nil }
func (b *ctxBackend) PublishContext(ctx context.Context, topic string, _ []byte, _ events.Meta) error {
	b.gotCtx, b.topic = ctx, topic
	return nil
}
func (*ctxBackend) Subscribe(string, events.MessageHandler) (func(), error) { return func() {}, nil }
func (*ctxBackend) Close() error                                            { return nil }

// An exporter's context carries its deadline and cancellation. The transport
// used to call Backend.Publish, which has no context parameter, so a broker
// backend never saw either.
func TestExporterContextReachesAContextAwareBackend(t *testing.T) {
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "marker")
	b := &ctxBackend{}
	tr := New(Config{Backend: b, ServiceName: "orders"})

	span := fleet.Span{TraceID: strings.Repeat("a", 32), SpanID: strings.Repeat("1", 16), Service: "orders"}
	if err := tr.ExportSpans(ctx, "", []fleet.Span{span}); err != nil {
		t.Fatal(err)
	}
	if b.gotCtx == nil || b.gotCtx.Value(key{}) != "marker" {
		t.Fatal("the exporter's context did not reach the backend")
	}
	if b.topic != DefaultSpansTopic {
		t.Fatalf("topic=%q, want %q", b.topic, DefaultSpansTopic)
	}
	if b.plain.Load() != 0 {
		t.Fatal("Backend.Publish was used although the backend implements ContextPublisher")
	}
}

func TestExportAfterTransportCloseFails(t *testing.T) {
	tr := New(Config{Bus: events.New(events.Config{DisableDiagnostics: true}), ServiceName: "orders"})
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	span := fleet.Span{TraceID: strings.Repeat("a", 32), SpanID: strings.Repeat("1", 16), Service: "orders"}
	if err := tr.ExportSpans(context.Background(), "", []fleet.Span{span}); !errors.Is(err, events.ErrBackendClosed) {
		t.Fatalf("export after Close: err=%v, want events.ErrBackendClosed", err)
	}
}
