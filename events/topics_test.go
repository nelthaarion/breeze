package events

// Tests for the topic layer in topics.go.
//
// Two very different things are verified here. Most tests cover the layer's own
// behaviour — routing, payload ownership, metadata, the Backend seam. But the
// first group is about the *rest of the package*: this layer was added to an
// existing bus with an existing test suite, and the requirement was that it be
// strictly additive. Those tests fail if this file's mere existence changes what
// a bus reports about itself.
//
// Every test builds its own bus with New(). Nothing touches Default, because the
// existing suite asserts deltas against Default's shared state and a stray
// subscription here would corrupt an unrelated test's arithmetic.

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// waitFor polls until cond holds, failing after a bounded wait.
//
// Async publishes complete on the bus's worker pool, so the alternative is a
// fixed sleep — which is either slower than necessary or flaky under load, and
// usually both.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out after 2s waiting for %s", what)
}

// --- Additivity: the layer must be invisible until used --------------------

// TestTopicLayerRegistersNothingOnAFreshBus is the guard that keeps this layer
// honest. The existing suite asserts a fresh bus with one registered type has
// exactly one entry in EventNames — so if RawMessage were registered eagerly, in
// New or an init, that unrelated test would start failing. Registration must
// therefore be lazy, and this pins it directly rather than relying on the other
// test to notice.
func TestTopicLayerRegistersNothingOnAFreshBus(t *testing.T) {
	b := New()

	if got := b.EventCount(); got != 0 {
		t.Errorf("EventCount on a fresh bus = %d, want 0", got)
	}
	if got := b.EventNames(); len(got) != 0 {
		t.Errorf("EventNames on a fresh bus = %v, want empty", got)
	}
	if got := b.ListenerCount(); got != 0 {
		t.Errorf("ListenerCount on a fresh bus = %d, want 0", got)
	}
}

// TestPublishAloneRegistersNothing — a bus that only publishes must stay clean
// too. Emitting a type with no listeners does not create a registry entry, and
// that is what lets a publish-only service leave no trace in the inspector.
func TestPublishAloneRegistersNothing(t *testing.T) {
	b := New()

	if err := PublishBus(b, "fleet.spans", []byte("[]"), nil); err != nil {
		t.Fatalf("PublishBus: %v", err)
	}

	if got := b.EventCount(); got != 0 {
		t.Errorf("EventCount after a publish with no subscribers = %d, want 0", got)
	}
}

// TestSubscribeRegistersExactlyOneType — once the layer is used it becomes
// visible, and it must account for itself as a single type no matter how many
// topics exist. If each topic registered its own type, the inspector would fill
// with per-topic noise and EventCount would track topic count instead.
func TestSubscribeRegistersExactlyOneType(t *testing.T) {
	b := New()

	SubscribeBus(b, "a", func(*Context, RawMessage) error { return nil })
	SubscribeBus(b, "b", func(*Context, RawMessage) error { return nil })
	SubscribeBus(b, "c", func(*Context, RawMessage) error { return nil })

	if got := b.EventCount(); got != 1 {
		t.Errorf("EventCount with three topics = %d, want 1 — topics share one event type", got)
	}
	if got := b.ListenerCount(); got != 3 {
		t.Errorf("ListenerCount = %d, want 3", got)
	}
}

// TestSubscriptionsAreNamedByTopic — with every subscriber on one type, the
// listener name is the only thing that tells them apart in the inspector. An
// operator looking at a bus should see which topics are subscribed, not three
// anonymous closures.
func TestSubscriptionsAreNamedByTopic(t *testing.T) {
	b := New()
	SubscribeBus(b, "fleet.spans", func(*Context, RawMessage) error { return nil })

	info := Inspect[RawMessage](b)
	if len(info.Listeners) != 1 {
		t.Fatalf("got %d listeners, want 1", len(info.Listeners))
	}
	if got := info.Listeners[0].Name; got != "topic:fleet.spans" {
		t.Errorf("listener name = %q, want topic:fleet.spans", got)
	}
}

// --- Routing ---------------------------------------------------------------

func TestPublishReachesTheRightSubscriber(t *testing.T) {
	b := New()
	var got []byte
	SubscribeBus(b, "fleet.spans", func(_ *Context, msg RawMessage) error {
		got = msg.Payload
		return nil
	})

	if err := PublishBus(b, "fleet.spans", []byte("payload"), nil); err != nil {
		t.Fatalf("PublishBus: %v", err)
	}
	if string(got) != "payload" {
		t.Errorf("payload = %q, want payload", got)
	}
}

// TestPublishDoesNotCrossTopics is the core routing property: a subscriber must
// never see another topic's traffic. Without it, a service subscribing to
// fleet.spans would also receive heartbeats and try to decode them as spans.
func TestPublishDoesNotCrossTopics(t *testing.T) {
	b := New()
	var spans, beats int

	SubscribeBus(b, "fleet.spans", func(*Context, RawMessage) error { spans++; return nil })
	SubscribeBus(b, "fleet.heartbeat", func(*Context, RawMessage) error { beats++; return nil })

	if err := PublishBus(b, "fleet.spans", nil, nil); err != nil {
		t.Fatalf("PublishBus: %v", err)
	}

	if spans != 1 {
		t.Errorf("the subscribed topic ran %d times, want 1", spans)
	}
	if beats != 0 {
		t.Errorf("an unrelated topic ran %d times, want 0 — topic filtering is not isolating subscribers", beats)
	}
}

// TestPublishFanOut — several subscribers on one topic all run, which is what
// makes a topic a topic rather than a queue.
func TestPublishFanOut(t *testing.T) {
	b := New()
	var n int
	for i := 0; i < 3; i++ {
		SubscribeBus(b, "topic", func(*Context, RawMessage) error { n++; return nil })
	}

	if err := PublishBus(b, "topic", nil, nil); err != nil {
		t.Fatalf("PublishBus: %v", err)
	}
	if n != 3 {
		t.Errorf("%d of 3 subscribers ran", n)
	}
}

// TestPublishWithNoSubscribersIsNotAnError — the aggregator may start after the
// services that publish to it, and a publisher must not treat "nobody is
// listening yet" as a failure worth retrying or logging.
func TestPublishWithNoSubscribersIsNotAnError(t *testing.T) {
	b := New()
	if err := PublishBus(b, "nobody.listening", []byte("x"), nil); err != nil {
		t.Errorf("PublishBus with no subscribers = %v, want nil", err)
	}
}

// TestPublishRejectsAnEmptyTopic — an unset topic is a config bug, and
// delivering it silently to nothing would hide it indefinitely.
func TestPublishRejectsAnEmptyTopic(t *testing.T) {
	b := New()
	if err := PublishBus(b, "", []byte("x"), nil); !errors.Is(err, ErrEmptyTopic) {
		t.Errorf("PublishBus with an empty topic = %v, want ErrEmptyTopic", err)
	}
	if err := PublishAsyncBus(b, "", []byte("x"), nil); !errors.Is(err, ErrEmptyTopic) {
		t.Errorf("PublishAsyncBus with an empty topic = %v, want ErrEmptyTopic", err)
	}
}

// TestSubscribeToEmptyTopicNeverFires — the counterpart. Since publishing to ""
// is rejected, a subscriber on "" cannot receive anything; what must not happen
// is it acting as a catch-all for every topic.
func TestSubscribeToEmptyTopicNeverFires(t *testing.T) {
	b := New()
	var n int
	SubscribeBus(b, "", func(*Context, RawMessage) error { n++; return nil })

	if err := PublishBus(b, "real.topic", nil, nil); err != nil {
		t.Fatalf("PublishBus: %v", err)
	}
	if n != 0 {
		t.Errorf("an empty-topic subscriber ran %d times — it is acting as a catch-all", n)
	}
}

// --- Errors and cancellation ----------------------------------------------

// TestPublishReportsSubscriberErrors — a publisher needs to know its message was
// not handled, exactly as an emitter does.
func TestPublishReportsSubscriberErrors(t *testing.T) {
	b := New()
	want := errors.New("handler failed")
	SubscribeBus(b, "topic", func(*Context, RawMessage) error { return want })

	if err := PublishBus(b, "topic", nil, nil); !errors.Is(err, want) {
		t.Errorf("PublishBus = %v, want the subscriber's error", err)
	}
}

// TestSubscriberSeesTheDispatchContext — the Context handed to a subscriber must
// be the real one, since that is how the trace-context metadata this layer
// exists to carry gets read back out.
func TestSubscriberSeesTheDispatchContext(t *testing.T) {
	b := New()
	var seen bool
	SubscribeBus(b, "topic", func(ctx *Context, _ RawMessage) error {
		seen = ctx != nil && !ctx.Time.IsZero()
		return nil
	})

	if err := PublishBus(b, "topic", nil, nil); err != nil {
		t.Fatalf("PublishBus: %v", err)
	}
	if !seen {
		t.Error("the subscriber received no usable dispatch context")
	}
}

// --- Payload ownership ----------------------------------------------------

// TestPublishDoesNotCopyPayload documents the synchronous contract explicitly.
// Subscribers run before Publish returns, so handing the caller's slice straight
// through is safe and avoids a copy on the hot path — but it is a real
// constraint on subscribers, so it is pinned rather than left implied.
func TestPublishDoesNotCopyPayload(t *testing.T) {
	b := New()
	payload := []byte("original")
	var same bool
	SubscribeBus(b, "topic", func(_ *Context, msg RawMessage) error {
		same = len(msg.Payload) > 0 && &msg.Payload[0] == &payload[0]
		return nil
	})

	if err := PublishBus(b, "topic", payload, nil); err != nil {
		t.Fatalf("PublishBus: %v", err)
	}
	if !same {
		t.Error("PublishBus copied the payload; the synchronous path should pass it through")
	}
}

// TestPublishAsyncCopiesPayload is the bug this layer must not have. An async
// subscriber runs after the call returns, so a caller reusing its buffer — the
// normal pattern when reading from a pool or a socket — would otherwise mutate a
// message already in flight. The failure is silent and data-dependent, which is
// exactly why it is tested rather than trusted.
func TestPublishAsyncCopiesPayload(t *testing.T) {
	b := New()
	var got atomic.Value
	done := make(chan struct{})
	SubscribeBus(b, "topic", func(_ *Context, msg RawMessage) error {
		got.Store(string(msg.Payload))
		close(done)
		return nil
	})

	payload := []byte("original")
	if err := PublishAsyncBus(b, "topic", payload, nil); err != nil {
		t.Fatalf("PublishAsyncBus: %v", err)
	}
	// Simulate the caller recycling its buffer immediately, before the
	// subscriber has had a chance to run.
	copy(payload, "OVERWRIT")

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the async subscriber never ran")
	}

	if s, _ := got.Load().(string); s != "original" {
		t.Errorf("subscriber saw %q, want original — the async path is sharing the caller's buffer", s)
	}
}

// TestPublishAsyncCopiesMeta — same reasoning for metadata, which is a map and
// therefore shared by reference by default.
func TestPublishAsyncCopiesMeta(t *testing.T) {
	b := New()
	var got atomic.Value
	done := make(chan struct{})
	SubscribeBus(b, "topic", func(_ *Context, msg RawMessage) error {
		got.Store(msg.Meta.Get("trace_id"))
		close(done)
		return nil
	})

	meta := Meta{"trace_id": "abc"}
	if err := PublishAsyncBus(b, "topic", nil, meta); err != nil {
		t.Fatalf("PublishAsyncBus: %v", err)
	}
	meta["trace_id"] = "MUTATED"

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the async subscriber never ran")
	}

	if s, _ := got.Load().(string); s != "abc" {
		t.Errorf("subscriber saw trace_id=%q, want abc — the async path is sharing the caller's map", s)
	}
}

// TestPublishAsyncNilPayloadStaysNil — a copy step must not turn nil into an
// empty slice, since a subscriber may distinguish them.
func TestPublishAsyncNilPayloadStaysNil(t *testing.T) {
	b := New()
	var wasNil atomic.Bool
	done := make(chan struct{})
	SubscribeBus(b, "topic", func(_ *Context, msg RawMessage) error {
		wasNil.Store(msg.Payload == nil)
		close(done)
		return nil
	})

	if err := PublishAsyncBus(b, "topic", nil, nil); err != nil {
		t.Fatalf("PublishAsyncBus: %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the async subscriber never ran")
	}
	if !wasNil.Load() {
		t.Error("a nil payload arrived as non-nil")
	}
}

// --- Meta ------------------------------------------------------------------

func TestMetaAccessors(t *testing.T) {
	var nilMeta Meta
	if got := nilMeta.Get("k"); got != "" {
		t.Errorf("Get on a nil Meta = %q, want empty", got)
	}
	if nilMeta.Has("k") {
		t.Error("Has on a nil Meta = true")
	}
	if got := nilMeta.Clone(); got != nil {
		t.Errorf("Clone of a nil Meta = %v, want nil", got)
	}

	m := Meta{"present": "value", "empty": ""}
	if got := m.Get("present"); got != "value" {
		t.Errorf("Get = %q, want value", got)
	}
	if got := m.Get("absent"); got != "" {
		t.Errorf("Get of an absent key = %q, want empty", got)
	}
	// The distinction Get cannot express: a stored empty string is present.
	if !m.Has("empty") {
		t.Error("Has of a stored empty value = false, want true")
	}
	if m.Has("absent") {
		t.Error("Has of an absent key = true")
	}
}

// TestMetaWithDoesNotMutateTheReceiver — With is the safe way to add context to
// a shared template while a publish may be in flight.
func TestMetaWithDoesNotMutateTheReceiver(t *testing.T) {
	base := Meta{"a": "1"}
	derived := base.With("b", "2")

	if base.Has("b") {
		t.Error("With mutated the receiver")
	}
	if got := derived.Get("a"); got != "1" {
		t.Errorf("derived lost the original key: %v", derived)
	}
	if got := derived.Get("b"); got != "2" {
		t.Errorf("derived.Get(b) = %q, want 2", got)
	}

	// Overwriting an existing key must not disturb the original either.
	over := base.With("a", "9")
	if got := base.Get("a"); got != "1" {
		t.Errorf("base.Get(a) = %q after an overwrite, want 1", got)
	}
	if got := over.Get("a"); got != "9" {
		t.Errorf("over.Get(a) = %q, want 9", got)
	}

	// With on a nil receiver is the common case for building metadata from
	// scratch and must not panic.
	var nilMeta Meta
	if got := nilMeta.With("k", "v").Get("k"); got != "v" {
		t.Errorf("With on a nil Meta = %q, want v", got)
	}
}

func TestMetaCloneIsIndependent(t *testing.T) {
	orig := Meta{"k": "v"}
	clone := orig.Clone()
	clone["k"] = "changed"
	clone["new"] = "x"

	if got := orig.Get("k"); got != "v" {
		t.Errorf("the original changed: %v", orig)
	}
	if orig.Has("new") {
		t.Error("a key added to the clone appeared in the original")
	}
}

// TestPublishCarriesMeta is the whole point of Meta existing: trace context has
// to survive the hop without being part of the payload's schema.
func TestPublishCarriesMeta(t *testing.T) {
	b := New()
	var got Meta
	SubscribeBus(b, "topic", func(_ *Context, msg RawMessage) error {
		got = msg.Meta
		return nil
	})

	if err := PublishBus(b, "topic", nil, Meta{"traceparent": "00-abc-def-01"}); err != nil {
		t.Fatalf("PublishBus: %v", err)
	}
	if got.Get("traceparent") != "00-abc-def-01" {
		t.Errorf("meta = %v, want the traceparent to survive the hop", got)
	}
}

// --- Subscription lifecycle ------------------------------------------------

func TestUnsubscribeStopsDelivery(t *testing.T) {
	b := New()
	var n int
	sub := SubscribeBus(b, "topic", func(*Context, RawMessage) error { n++; return nil })

	if err := PublishBus(b, "topic", nil, nil); err != nil {
		t.Fatalf("PublishBus: %v", err)
	}
	sub.Unsubscribe()
	if err := PublishBus(b, "topic", nil, nil); err != nil {
		t.Fatalf("PublishBus: %v", err)
	}

	if n != 1 {
		t.Errorf("the subscriber ran %d times, want 1 — Unsubscribe did not take effect", n)
	}
	if sub.Active() {
		t.Error("Active() = true after Unsubscribe")
	}
}

// TestSubscriptionPriorityIsHonoured — the returned value is an ordinary
// Subscription, so the typed API's ordering controls work unchanged.
func TestSubscriptionPriorityIsHonoured(t *testing.T) {
	b := New()
	var order []string

	SubscribeBus(b, "topic", func(*Context, RawMessage) error {
		order = append(order, "low")
		return nil
	}).Priority(1)
	SubscribeBus(b, "topic", func(*Context, RawMessage) error {
		order = append(order, "high")
		return nil
	}).Priority(100)

	if err := PublishBus(b, "topic", nil, nil); err != nil {
		t.Fatalf("PublishBus: %v", err)
	}
	if len(order) != 2 || order[0] != "high" {
		t.Errorf("order = %v, want the higher priority first", order)
	}
}

// --- Backend --------------------------------------------------------------

func TestInMemoryBackendRoundTrip(t *testing.T) {
	b := New()
	backend := NewInMemoryBackend(b)
	defer func() { _ = backend.Close() }()

	var got atomic.Value
	done := make(chan struct{})
	cancel, err := backend.Subscribe("fleet.spans", func(_ *Context, msg RawMessage) error {
		got.Store(string(msg.Payload))
		close(done)
		return nil
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	if err := backend.Publish("fleet.spans", []byte("[]"), nil); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the message never arrived")
	}
	if s, _ := got.Load().(string); s != "[]" {
		t.Errorf("payload = %q, want []", s)
	}
}

// TestInMemoryBackendCancelStopsDelivery — the cancel function is the only
// handle a Backend user has on a subscription's lifetime, so it has to work.
func TestInMemoryBackendCancelStopsDelivery(t *testing.T) {
	b := New()
	backend := NewInMemoryBackend(b)
	defer func() { _ = backend.Close() }()

	var n int64
	cancel, err := backend.Subscribe("topic", func(*Context, RawMessage) error {
		atomic.AddInt64(&n, 1)
		return nil
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	if err := backend.Publish("topic", nil, nil); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	waitFor(t, "the first message", func() bool { return atomic.LoadInt64(&n) == 1 })

	cancel()
	if err := backend.Publish("topic", nil, nil); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	// Nothing more should arrive. A brief wait is the only way to observe
	// the absence of an async delivery.
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt64(&n); got != 1 {
		t.Errorf("received %d messages, want 1 — cancel did not unsubscribe", got)
	}
}

// TestInMemoryBackendCancelIsIdempotent — Backend's contract requires it, and a
// double cancel is what happens when a defer and an explicit call coexist.
func TestInMemoryBackendCancelIsIdempotent(t *testing.T) {
	backend := NewInMemoryBackend(New())
	cancel, err := backend.Subscribe("topic", func(*Context, RawMessage) error { return nil })
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	cancel()
	cancel() // must not panic
}

func TestInMemoryBackendRejectsAnEmptyTopic(t *testing.T) {
	backend := NewInMemoryBackend(New())
	if _, err := backend.Subscribe("", nil); !errors.Is(err, ErrEmptyTopic) {
		t.Errorf("Subscribe(\"\") = %v, want ErrEmptyTopic", err)
	}
	if err := backend.Publish("", nil, nil); !errors.Is(err, ErrEmptyTopic) {
		t.Errorf("Publish(\"\") = %v, want ErrEmptyTopic", err)
	}
}

// TestInMemoryBackendCloseIsIdempotentAndHarmless — Close must not tear down a
// bus the backend does not own, since the caller may still be using it.
func TestInMemoryBackendCloseIsIdempotentAndHarmless(t *testing.T) {
	b := New()
	backend := NewInMemoryBackend(b)

	if err := backend.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := backend.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	// The bus must still work after its backend is closed.
	if err := PublishBus(b, "topic", nil, nil); err != nil {
		t.Errorf("the bus stopped working after its backend closed: %v", err)
	}
}

// TestNewInMemoryBackendNilBus — a nil bus falls back to Default rather than
// panicking on first publish, so a zero-value config is usable.
func TestNewInMemoryBackendNilBus(t *testing.T) {
	if NewInMemoryBackend(nil) == nil {
		t.Error("NewInMemoryBackend(nil) returned nil")
	}
}

// --- Concurrency ----------------------------------------------------------

// TestTopicConcurrentPublishAndSubscribe is the shape the fleet aggregator will
// actually produce: many goroutines publishing while subscriptions come and go.
// Under -race, this is what proves the layer adds no unsynchronized state of its
// own on top of the bus's.
func TestTopicConcurrentPublishAndSubscribe(t *testing.T) {
	b := New()
	var received int64
	SubscribeBus(b, "spans", func(*Context, RawMessage) error {
		atomic.AddInt64(&received, 1)
		return nil
	})

	const publishers = 8
	const perPublisher = 100

	var wg sync.WaitGroup
	for p := 0; p < publishers; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perPublisher; i++ {
				if err := PublishBus(b, "spans", []byte("x"), Meta{"i": "1"}); err != nil {
					t.Errorf("PublishBus: %v", err)
					return
				}
			}
		}()
	}
	// Churn subscriptions concurrently: registration and dispatch share the
	// bus's listener storage.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			SubscribeBus(b, "other", func(*Context, RawMessage) error { return nil }).Unsubscribe()
		}
	}()
	wg.Wait()

	if got := atomic.LoadInt64(&received); got != publishers*perPublisher {
		t.Errorf("received %d messages, want %d", got, publishers*perPublisher)
	}
}
