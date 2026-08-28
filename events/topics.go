package events

// Topic-based publish/subscribe, layered on top of the typed API.
//
// The typed API in api.go — On/Emit with a Go type as the event — remains the
// primary and recommended way to use this package. It is type-safe, needs no
// serialization, and the compiler catches a mismatch between publisher and
// subscriber. Prefer it whenever both ends are Go code in the same binary.
//
// This file adds a second, narrower entry point for the cases the typed API
// structurally cannot serve:
//
//   - The topic is not known at compile time (it arrives from config, or is
//     derived from data at runtime).
//   - The payload is already bytes and its Go type is not available here —
//     it came off a network, or is destined for one.
//   - A message needs to carry string metadata alongside its payload, for
//     things like trace context, which a plain struct field cannot express
//     generically.
//
// # This is a wrapper, not a second bus
//
// Everything here funnels into one internal event type, [RawMessage], and is
// dispatched by the same Emit/On machinery as any other event. That is
// deliberate: middleware, the observer, the recorder, metrics, priorities,
// cancellation and the async worker pool all apply to published messages
// automatically, with no parallel implementation to keep in sync. A published
// message is an ordinary event that happens to have a string topic.
//
// One consequence is worth knowing: every subscriber on a bus is a listener on
// the same RawMessage type, each with a filter that accepts only its own topic.
// A publish therefore costs one dispatch plus a string comparison per
// subscriber. For the dozens-of-topics scale this layer is meant for that is
// cheaper than a second registry; if you ever have thousands of subscribers on
// one bus, the typed API is the better tool and always was.
//
// # Nothing is registered until it is used
//
// RawMessage is registered on a bus only when Subscribe is first called on it.
// A bus that never publishes or subscribes is byte-for-byte unaffected by this
// file — it does not appear in EventNames, EventCount, or the inspector. This
// is a property the tests pin, not an accident.

import (
	"errors"
)

// ErrEmptyTopic is returned by [Publish] and its variants when the topic is
// empty.
//
// An unset topic is nearly always a bug — a missing config value or a typo in a
// constant — and silently publishing to a topic nobody can subscribe to would
// hide it. Failing loudly costs one comparison.
var ErrEmptyTopic = errors.New("events: topic is empty")

// Meta carries string key/value metadata alongside a published payload.
//
// It exists for context that travels *with* a message but is not *part* of it:
// a trace id, a tenant, a schema version, a signature. Keeping it separate from
// the payload means a subscriber can route or authorize a message without
// deserializing it, and a publisher can add context without changing the
// payload's schema.
//
// A nil Meta is valid and behaves as empty.
type Meta map[string]string

// Get returns the value for key, or "" if absent.
//
// Reading a missing key from a nil map is already safe in Go; this exists so
// call sites that only need a value can skip the two-result form.
func (m Meta) Get(key string) string { return m[key] }

// Has reports whether key is present, distinguishing a stored empty string
// from an absent key — which [Meta.Get] cannot.
func (m Meta) Has(key string) bool {
	_, ok := m[key]
	return ok
}

// With returns a copy of m with key set to value.
//
// The receiver is not modified, so metadata can be extended while a publish is
// in flight without a data race, and a shared base Meta can be safely used as a
// template. Calling With on a nil Meta returns a new one-entry Meta.
func (m Meta) With(key, value string) Meta {
	out := make(Meta, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	out[key] = value
	return out
}

// Clone returns an independent copy of m, or nil if m is nil.
//
// Preserving nil matters: a cloned message's Meta should be indistinguishable
// from the original's, including its absence.
func (m Meta) Clone() Meta {
	if m == nil {
		return nil
	}
	out := make(Meta, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// RawMessage is the event type every published message is dispatched as.
//
// It is exported because it appears in the inspector and the recorder once a
// bus has subscribers, and because a middleware or observer may legitimately
// want to inspect published traffic. Application code normally uses [Publish]
// and [Subscribe] rather than emitting this type directly — though doing so is
// harmless, since it is an ordinary event.
type RawMessage struct {
	// Topic is the subscription key. Never empty for a message that went
	// through Publish.
	Topic string

	// Payload is the message body, untouched by this package. Its meaning
	// and encoding are entirely the publisher's and subscriber's agreement.
	//
	// Ownership differs by dispatch mode, and the difference matters:
	// [Publish] hands the caller's slice straight to subscribers, which is
	// safe because they all run before Publish returns. [PublishAsync]
	// copies it, because subscribers run after the call returns and the
	// caller may reuse the buffer by then.
	Payload []byte

	// Meta is optional metadata; nil is valid.
	Meta Meta
}

// MessageHandler handles a published message.
//
// The Context is the same one typed listeners receive, so cancellation,
// per-dispatch metadata and the deadline all behave identically.
type MessageHandler func(ctx *Context, msg RawMessage) error

// Publish dispatches a message on the default bus synchronously.
func Publish(topic string, payload []byte, meta Meta) error {
	return PublishBus(Default, topic, payload, meta)
}

// PublishBus dispatches a message on b synchronously, returning once every
// subscriber to topic has run.
//
// The payload is not copied. Subscribers all run before this returns, so they
// observe the slice as it is at the moment of publication — but a subscriber
// that retains the payload beyond its handler must copy it, exactly as it would
// for any other borrowed slice. Use [PublishAsyncBus] when the caller intends to
// reuse the buffer immediately.
//
// Errors from subscribers are reported the same way [EmitBus] reports listener
// errors. Publishing to a topic with no subscribers is not an error.
func PublishBus(b *Bus, topic string, payload []byte, meta Meta) error {
	if topic == "" {
		return ErrEmptyTopic
	}
	return EmitBus(b, RawMessage{Topic: topic, Payload: payload, Meta: meta})
}

// PublishAsync dispatches a message on the default bus without waiting for its
// subscribers.
func PublishAsync(topic string, payload []byte, meta Meta) error {
	return PublishAsyncBus(Default, topic, payload, meta)
}

// PublishAsyncBus dispatches a message on b without waiting for its
// subscribers.
//
// Unlike [PublishBus], the payload and metadata are copied. Subscribers run
// after this call returns, so a caller that reuses its buffer — the normal
// pattern for anything reading from a pool or a network connection — would
// otherwise mutate a message already in flight. That class of bug is silent and
// data-dependent, so the copy is not optional.
func PublishAsyncBus(b *Bus, topic string, payload []byte, meta Meta) error {
	if topic == "" {
		return ErrEmptyTopic
	}
	var buf []byte
	if payload != nil {
		buf = make([]byte, len(payload))
		copy(buf, payload)
	}
	return EmitAsyncBus(b, RawMessage{Topic: topic, Payload: buf, Meta: meta.Clone()})
}

// Subscribe registers fn for topic on the default bus.
func Subscribe(topic string, fn MessageHandler) *Subscription[RawMessage] {
	return SubscribeBus(Default, topic, fn)
}

// SubscribeBus registers fn for topic on b.
//
// The returned [Subscription] is an ordinary subscription: Unsubscribe,
// Priority and Named all work as documented there. Its Where is already set to
// match this topic — calling Where again would replace that filter and make the
// subscriber receive every topic, so use the returned value for lifecycle
// control rather than filtering.
//
// Since [Publish] rejects an empty topic, subscribing to "" yields a
// subscription that can never fire. That is intentional: a missing topic
// silently receiving everything would be far worse than receiving nothing.
func SubscribeBus(b *Bus, topic string, fn MessageHandler) *Subscription[RawMessage] {
	return OnBus(b, RawMessage{}, func(ctx *Context, msg RawMessage) error {
		return fn(ctx, msg)
	}).Where(func(msg RawMessage) bool {
		return msg.Topic == topic
	}).Named("topic:" + topic)
}

// Backend is the seam between topic-based publishing and a transport.
//
// The in-process implementation returned by [NewInMemoryBackend] is the only one
// this package ships, and it is the one to use when publisher and subscriber
// share a binary. The interface exists so that a networked implementation —
// NATS, Kafka, RabbitMQ, or something bespoke — can be substituted without the
// code that publishes and subscribes changing at all.
//
// An implementation must be safe for concurrent use by multiple goroutines.
type Backend interface {
	// Publish sends payload to topic. Whether it blocks until delivery is
	// implementation-defined; the in-process backend does not.
	Publish(topic string, payload []byte, meta Meta) error

	// Subscribe registers fn for topic and returns a function that cancels
	// the subscription. The cancel function must be safe to call more than
	// once.
	Subscribe(topic string, fn MessageHandler) (cancel func(), err error)

	// Close releases the backend's resources. Subscriptions made through it
	// stop receiving messages. Close must be idempotent.
	Close() error
}

// inMemoryBackend implements [Backend] over a *Bus.
type inMemoryBackend struct {
	bus *Bus
}

// NewInMemoryBackend returns a [Backend] that delivers messages in-process via
// bus. A nil bus uses [Default].
//
// This is the cheapest possible backend: delivery is a function call, with no
// serialization and no network. It is also the only one that cannot cross a
// process boundary, which is precisely the trade being made.
func NewInMemoryBackend(bus *Bus) Backend {
	if bus == nil {
		bus = Default
	}
	return &inMemoryBackend{bus: bus}
}

// Publish delivers asynchronously, so that a slow subscriber cannot block the
// publisher. This is what makes the in-process backend behave like a network
// one from the caller's point of view, and it means the payload is copied — see
// [PublishAsyncBus].
func (m *inMemoryBackend) Publish(topic string, payload []byte, meta Meta) error {
	return PublishAsyncBus(m.bus, topic, payload, meta)
}

func (m *inMemoryBackend) Subscribe(topic string, fn MessageHandler) (func(), error) {
	if topic == "" {
		return nil, ErrEmptyTopic
	}
	sub := SubscribeBus(m.bus, topic, fn)
	// Subscription.Unsubscribe is already idempotent — it guards on a
	// compare-and-swap — so it satisfies Backend's contract directly.
	return sub.Unsubscribe, nil
}

// Close is a no-op beyond satisfying the interface: this backend owns no
// resources of its own, and closing the bus is the caller's decision, not
// something a backend handed a shared bus should do on its behalf.
func (m *inMemoryBackend) Close() error { return nil }
