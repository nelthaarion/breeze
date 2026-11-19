package events

// Tests for the API added alongside the audit fixes: Config.DisableDiagnostics,
// the Backend sentinel errors, and ContextPublisher / PublishVia.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/nelthaarion/breeze/v2/diag"
)

// auditReportedListeners returns the "listeners" figure of the registered
// event-bus diagnostic, which identifies which bus the probe is reading.
func auditReportedListeners(t *testing.T) int {
	t.Helper()
	r, ok := diag.Get(diagName)
	if !ok {
		t.Fatal("no event-bus diagnostic is registered")
	}
	n, _ := r.Detail["listeners"].(int)
	return n
}

// New replaces the registered diagnostic by design (diag.Register: "last
// registration wins"). That is right for the application's bus and wrong for a
// private one — the Kafka backend builds one when it is not handed a bus — so
// there has to be a way to opt out.
func TestAuditDisableDiagnosticsKeepsTheRegisteredBus(t *testing.T) {
	t.Cleanup(func() { RegisterDiagnostics(Default) })

	app := New(quiet())
	OnBus(app, auditEvent{}, auditNoop)
	if got := auditReportedListeners(t); got != 1 {
		t.Fatalf("probe reports %d listeners, want 1 (the application's bus)", got)
	}

	private := New(Config{DisableDiagnostics: true, OnPanic: func(*PanicError) {}})
	OnBus(private, auditEvent{}, auditNoop)
	OnBus(private, auditEvent{}, auditNoop)
	if got := auditReportedListeners(t); got != 1 {
		t.Fatalf("a bus built with DisableDiagnostics took over the probe: it now reports %d listeners, want 1", got)
	}

	// The default is unchanged: an ordinary new bus still replaces the probe.
	New(quiet())
	if got := auditReportedListeners(t); got != 0 {
		t.Fatalf("an ordinary New no longer registers its bus: probe reports %d listeners, want 0", got)
	}

	// And an opted-out bus can still be published explicitly.
	RegisterDiagnostics(private)
	if got := auditReportedListeners(t); got != 2 {
		t.Fatalf("RegisterDiagnostics(private) reports %d listeners, want 2", got)
	}
}

func TestAuditBackendSentinelErrors(t *testing.T) {
	bus := New(quiet())
	defer bus.Close()
	be := NewInMemoryBackend(bus)
	h := func(*Context, RawMessage) error { return nil }

	if _, err := be.Subscribe("", h); !errors.Is(err, ErrEmptyTopic) {
		t.Errorf("empty topic: err=%v, want ErrEmptyTopic", err)
	}
	if _, err := be.Subscribe("t", nil); !errors.Is(err, ErrNilHandler) {
		t.Errorf("nil handler: err=%v, want ErrNilHandler", err)
	}

	if err := be.Close(); err != nil {
		t.Fatal(err)
	}
	if err := be.Publish("t", nil, nil); !errors.Is(err, ErrBackendClosed) {
		t.Errorf("Publish after Close: err=%v, want ErrBackendClosed", err)
	}
	if _, err := be.Subscribe("t", h); !errors.Is(err, ErrBackendClosed) {
		t.Errorf("Subscribe after Close: err=%v, want ErrBackendClosed", err)
	}
	if err := PublishVia(context.Background(), be, "t", nil, nil); !errors.Is(err, ErrBackendClosed) {
		t.Errorf("PublishVia after Close: err=%v, want ErrBackendClosed", err)
	}
}

// A subscription that was cancelled must not stay in the backend until Close:
// the Kafka backend leaked exactly this way (b.cancels only ever grew).
func TestAuditInMemoryBackendForgetsCancelledSubscriptions(t *testing.T) {
	bus := New(quiet())
	defer bus.Close()
	be := NewInMemoryBackend(bus).(*inMemoryBackend)

	for i := 0; i < 100; i++ {
		cancel, err := be.Subscribe("t", func(*Context, RawMessage) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		cancel() // must be safe to call twice
	}
	be.mu.Lock()
	n := len(be.subs)
	be.mu.Unlock()
	if n != 0 {
		t.Fatalf("backend still tracks %d cancelled subscriptions", n)
	}
	if got := Count[RawMessage](bus); got != 0 {
		t.Fatalf("%d listeners left on the bus after cancelling every subscription", got)
	}
}

type auditPlainBackend struct{ published atomic.Int64 }

func (b *auditPlainBackend) Publish(string, []byte, Meta) error { b.published.Add(1); return nil }
func (*auditPlainBackend) Subscribe(string, MessageHandler) (func(), error) {
	return func() {}, nil
}
func (*auditPlainBackend) Close() error { return nil }

type auditCtxBackend struct {
	auditPlainBackend
	got context.Context
}

func (b *auditCtxBackend) PublishContext(ctx context.Context, _ string, _ []byte, _ Meta) error {
	b.got = ctx
	return nil
}

func TestAuditPublishViaPassesTheContextOn(t *testing.T) {
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "v")

	cb := &auditCtxBackend{}
	if err := PublishVia(ctx, cb, "t", nil, nil); err != nil {
		t.Fatal(err)
	}
	if cb.got != ctx {
		t.Fatal("the context did not reach a ContextPublisher")
	}
	if cb.published.Load() != 0 {
		t.Fatal("PublishVia used Publish although the backend implements ContextPublisher")
	}
}

func TestAuditPublishViaChecksTheContextForPlainBackends(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	pb := &auditPlainBackend{}
	if err := PublishVia(cancelled, pb, "t", nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
	if pb.published.Load() != 0 {
		t.Fatal("a cancelled context still reached Publish")
	}
	if err := PublishVia(context.Background(), pb, "t", nil, nil); err != nil || pb.published.Load() != 1 {
		t.Fatalf("live context: err=%v published=%d, want nil and 1", err, pb.published.Load())
	}
	if err := PublishVia(nil, pb, "t", nil, nil); err != nil { //nolint:staticcheck // a nil context is tolerated by design
		t.Fatalf("nil context: %v", err)
	}

	// The in-memory backend honours it too.
	bus := New(quiet())
	defer bus.Close()
	be := NewInMemoryBackend(bus)
	var got atomic.Int64
	if _, err := be.Subscribe("t", func(*Context, RawMessage) error { got.Add(1); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := PublishVia(cancelled, be, "t", nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("in-memory: err=%v, want context.Canceled", err)
	}
	if got.Load() != 0 {
		t.Fatal("the in-memory backend delivered a message published under a cancelled context")
	}
}
