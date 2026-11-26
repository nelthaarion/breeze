// Package kafka adapts Kafka to Breeze's generic events.Backend seam.
//
// The base Breeze module deliberately does not import kafka-go. Applications
// that need a broker opt into this nested module and pass the returned backend
// to eventtransport.New. The wire envelope is intentionally small and stable:
// topic selection remains Kafka's topic, while the payload and events.Meta stay
// together so eventtransport's signing and trace metadata survive the hop.
//
// # Delivery
//
// Delivery is at-least-once within a consumer group. A message's offset is
// committed only after its handler has returned, so a crash or a failed commit
// redelivers the message instead of losing it. A handler that returns an error
// does not stop the subscription and does not hold the partition back: the
// error is reported through [Config.OnError] and the message is committed, so
// a message that can never succeed cannot wedge a partition. Handlers should
// therefore be idempotent, and a handler that needs retries or a dead-letter
// topic implements them itself (Publish is available for the latter).
//
// A message that cannot be decoded is reported and skipped for the same reason.
//
// # Feeding the fleet aggregator
//
// The aggregator subscribes to its topics on a local bus
// (aggregator.Config.EventsBus) and knows nothing about brokers. Give that same
// bus to this backend and start one reader per topic. Messages a reader consumes
// are dispatched on the bus, the aggregator's own subscription handles them, and
// the offset is committed once it has; a handler error such as a failed
// signature check reaches Config.OnError instead of being lost.
//
//	bus := events.New()
//	be, _ := kafka.New(kafka.Config{Brokers: brokers, GroupID: "fleet-aggregator", Bus: bus})
//	noop := func(*events.Context, events.RawMessage) error { return nil }
//	be.Subscribe(eventtransport.DefaultSpansTopic, noop)
//	be.Subscribe(eventtransport.DefaultHeartbeatTopic, noop)
//	aggregator.InstallAggregator(app, router, aggregator.Config{EventsBus: bus})
//
// On the producing side, pass the backend to eventtransport.New as Config.Backend.
package kafka

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/nelthaarion/go-json-plus"

	"github.com/nelthaarion/breeze/v2/events"
	kafka "github.com/segmentio/kafka-go"
)

// Config configures a [Backend].
type Config struct {
	// Brokers is the bootstrap broker list. At least one is required.
	Brokers []string

	// GroupID is the consumer group every subscription joins. Required.
	GroupID string

	// DialTimeout bounds connecting to a broker. Defaults to 5s. Ignored
	// for readers when Dialer is set (the Dialer carries its own timeout).
	DialTimeout time.Duration

	// WriteTimeout bounds one Publish, and the broker-side write inside it.
	// Defaults to 5s. A deadline on the context given to PublishContext
	// applies as well; the earlier of the two wins.
	WriteTimeout time.Duration

	// ReadTimeout bounds one offset commit. Defaults to 30s.
	//
	// It used to bound the whole read, fetch and commit together; a commit
	// that began just before that deadline was cancelled with its message
	// already consumed, which dropped the message.
	ReadTimeout time.Duration

	// Bus is the local bus messages are dispatched on. When nil the backend
	// creates a private one (and closes it with the backend). That private
	// bus does not register itself as the process's event-bus diagnostic.
	Bus *events.Bus

	// BatchTimeout is how long the writer waits for more messages before
	// flushing a partial batch. Defaults to 10ms.
	//
	// kafka-go's own default is one second, and a synchronous write waits
	// for its batch to flush, so with that default every lone Publish took
	// about a second. Raise it to trade latency for fuller batches.
	BatchTimeout time.Duration

	// BatchSize is the writer's target batch size in messages. Zero uses
	// kafka-go's default (100).
	BatchSize int

	// MaxWait is how long the broker may hold a fetch open waiting for data.
	// Defaults to 500ms.
	//
	// kafka-go's own default is ten seconds, and Reader.Close cannot return
	// until the fetch that is in flight does: measured against a broker, a
	// Close with the default took up to ten seconds per reader, against under
	// a millisecond with 500ms. The cost of the short wait is one empty fetch
	// round trip per reader every MaxWait on an idle topic.
	MaxWait time.Duration

	// CommitInterval, when positive, makes offset commits asynchronous and
	// batched at that interval instead of one round trip per message. A
	// crash can then redeliver up to one interval of already-handled
	// messages. Zero (the default) commits synchronously after each message.
	CommitInterval time.Duration

	// StartOffset is where a group with no committed offset starts:
	// kafka.FirstOffset (the default) or kafka.LastOffset.
	StartOffset int64

	// ShutdownTimeout is how long Close waits for handlers that are already
	// running before giving up on them and returning an error. Defaults to
	// 10s. Handlers that watch their context's Done channel are cancelled
	// as soon as Close begins.
	ShutdownTimeout time.Duration

	// Dialer, when set, is used for every reader. It is how TLS and SASL are
	// configured for consuming.
	Dialer *kafka.Dialer

	// Transport, when set, is used by the writer. It is how TLS and SASL are
	// configured for publishing.
	Transport kafka.RoundTripper

	// OnError, when set, receives errors that have no caller to return to:
	// handler failures, undecodable messages, failed commits and fetch
	// errors. It is called on the subscription's goroutine, must not block
	// for long, and a panic inside it is contained.
	OnError func(topic string, err error)
}

type envelope struct {
	Payload []byte      `json:"payload,omitempty"`
	Meta    events.Meta `json:"meta,omitempty"`
}

// messageWriter and messageReader are the parts of kafka-go this package uses.
// *kafka.Writer and *kafka.Reader satisfy them; the tests substitute fakes so
// delivery, commit and shutdown behaviour can be checked without a broker.
type messageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
	Close() error
}

type messageReader interface {
	FetchMessage(ctx context.Context) (kafka.Message, error)
	CommitMessages(ctx context.Context, msgs ...kafka.Message) error
	Close() error
}

// Backend implements [events.Backend] and [events.ContextPublisher] over Kafka.
type Backend struct {
	cfg     Config
	writer  messageWriter
	bus     *events.Bus
	ownsBus bool

	newReader func(topic string) messageReader

	mu     sync.Mutex
	subs   map[*subscription]struct{}
	closed bool
}

// New creates a Kafka-backed events backend. It only validates local config;
// Kafka connectivity is established lazily by WriteMessages/FetchMessage.
func New(cfg Config) (*Backend, error) {
	if len(cfg.Brokers) == 0 {
		return nil, errors.New("kafka backend: at least one broker is required")
	}
	if cfg.GroupID == "" {
		return nil, errors.New("kafka backend: group id is required")
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 5 * time.Second
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = 5 * time.Second
	}
	if cfg.ReadTimeout <= 0 {
		cfg.ReadTimeout = 30 * time.Second
	}
	if cfg.BatchTimeout <= 0 {
		cfg.BatchTimeout = 10 * time.Millisecond
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = 10 * time.Second
	}
	if cfg.MaxWait <= 0 {
		cfg.MaxWait = 500 * time.Millisecond
	}
	// kafka.NewReader panics on an invalid configuration, and it runs inside
	// Subscribe, long after the mistake was made. Reject it here instead.
	if cfg.CommitInterval < 0 {
		return nil, errors.New("kafka backend: commit interval must not be negative")
	}
	if so := cfg.StartOffset; so != 0 && so != kafka.FirstOffset && so != kafka.LastOffset {
		return nil, errors.New("kafka backend: start offset must be kafka.FirstOffset or kafka.LastOffset")
	}

	b := &Backend{
		cfg: cfg,
		writer: &kafka.Writer{
			Addr:         kafka.TCP(cfg.Brokers...),
			Balancer:     &kafka.Hash{},
			WriteTimeout: cfg.WriteTimeout,
			RequiredAcks: kafka.RequireOne,
			BatchTimeout: cfg.BatchTimeout,
			BatchSize:    cfg.BatchSize,
			Transport:    cfg.Transport,
		},
		bus:  cfg.Bus,
		subs: make(map[*subscription]struct{}),
	}
	if b.bus == nil {
		// A private bus must not take over the process's event-bus
		// diagnostic from the bus the application actually runs on.
		b.bus = events.New(events.Config{DisableDiagnostics: true})
		b.ownsBus = true
	}
	b.newReader = b.defaultReader
	return b, nil
}

func (b *Backend) defaultReader(topic string) messageReader {
	dialer := b.cfg.Dialer
	if dialer == nil {
		dialer = &kafka.Dialer{Timeout: b.cfg.DialTimeout}
	}
	return kafka.NewReader(kafka.ReaderConfig{
		Brokers:        b.cfg.Brokers,
		Topic:          topic,
		GroupID:        b.cfg.GroupID,
		Dialer:         dialer,
		StartOffset:    b.cfg.StartOffset,
		CommitInterval: b.cfg.CommitInterval,
		MaxWait:        b.cfg.MaxWait,
		ReadBackoffMin: 100 * time.Millisecond,
		ReadBackoffMax: time.Second,
	})
}

var (
	_ events.Backend          = (*Backend)(nil)
	_ events.ContextPublisher = (*Backend)(nil)
)

// Publish implements [events.Backend]. See [Backend.PublishContext].
func (b *Backend) Publish(topic string, payload []byte, meta events.Meta) error {
	return b.PublishContext(context.Background(), topic, payload, meta)
}

// PublishContext writes payload and meta to topic and returns once the broker
// has acknowledged them, ctx is done, or WriteTimeout passes, whichever is
// first.
func (b *Backend) PublishContext(ctx context.Context, topic string, payload []byte, meta events.Meta) error {
	if topic == "" {
		return events.ErrEmptyTopic
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if b.isClosed() {
		return events.ErrBackendClosed
	}
	// meta is only read by Marshal; no defensive copy is needed.
	body, err := json.Marshal(envelope{Payload: payload, Meta: meta})
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, b.cfg.WriteTimeout)
	defer cancel()
	if err := b.writer.WriteMessages(wctx, kafka.Message{Topic: topic, Value: body}); err != nil {
		if errors.Is(err, io.ErrClosedPipe) && b.isClosed() {
			return events.ErrBackendClosed // lost a race with Close
		}
		return err
	}
	return nil
}

func (b *Backend) isClosed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

// subscription is one consumer-group reader and the goroutine that drains it.
type subscription struct {
	b      *Backend
	topic  string
	reader messageReader
	local  *events.Subscription[events.RawMessage]

	cancel context.CancelFunc
	done   chan struct{} // closed when the consuming goroutine has returned
	once   sync.Once
}

// shutdown stops the subscription without waiting for it. It is idempotent and
// safe to call from inside the subscription's own handler.
func (s *subscription) shutdown() {
	s.once.Do(func() {
		s.cancel()
		s.local.Unsubscribe()
		_ = s.reader.Close()
	})
}

// Subscribe starts one consumer-group reader for topic. The returned cancel
// function is safe to call repeatedly and stops only that subscription.
func (b *Backend) Subscribe(topic string, fn events.MessageHandler) (func(), error) {
	if topic == "" {
		return nil, events.ErrEmptyTopic
	}
	if fn == nil {
		return nil, events.ErrNilHandler
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, events.ErrBackendClosed
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &subscription{
		b:      b,
		topic:  topic,
		reader: b.newReader(topic),
		cancel: cancel,
		done:   make(chan struct{}),
	}
	// Registered before the goroutine starts, so the handler is in place for
	// the first message.
	s.local = events.SubscribeBus(b.bus, topic, fn)
	b.subs[s] = struct{}{}
	b.mu.Unlock()

	go s.run(ctx)

	return func() {
		s.shutdown()
		b.mu.Lock()
		delete(b.subs, s) // a cancelled subscription must not be retained until Close
		b.mu.Unlock()
	}, nil
}

// Fetch errors are retried with a growing pause so a broker that is down, or a
// reader in a state that fails immediately, cannot turn the loop into a spin.
const (
	fetchBackoffMin = 100 * time.Millisecond
	fetchBackoffMax = 5 * time.Second
)

func (s *subscription) run(ctx context.Context) {
	defer close(s.done)

	backoff := fetchBackoffMin
	for {
		msg, err := s.reader.FetchMessage(ctx)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, io.EOF) {
				return // stopped, or the reader was closed under us
			}
			s.b.report(s.topic, fmt.Errorf("kafka backend: fetch: %w", err))
			if !sleep(ctx, backoff) {
				return
			}
			if backoff *= 2; backoff > fetchBackoffMax {
				backoff = fetchBackoffMax
			}
			continue
		}
		backoff = fetchBackoffMin

		if !s.handle(ctx, msg) {
			return
		}
	}
}

// handle dispatches one message and then commits it. It reports false when the
// subscription cannot continue.
func (s *subscription) handle(ctx context.Context, msg kafka.Message) bool {
	var item envelope
	if err := json.Unmarshal(msg.Value, &item); err != nil {
		// A message that cannot be decoded never will be; skip it, loudly.
		s.b.report(s.topic, fmt.Errorf("kafka backend: undecodable message at partition %d offset %d: %w",
			msg.Partition, msg.Offset, err))
		s.commit(ctx, msg)
		return true
	}

	// Dispatch on the local bus so the callback receives a correctly
	// initialised events.Context and follows normal dispatch middleware and
	// observer semantics. The subscription's context is the dispatch's, so a
	// handler that watches ctx.Ctx sees the subscription being stopped.
	err := events.EmitCtxBus(s.b.bus, ctx, events.RawMessage{Topic: s.topic, Payload: item.Payload, Meta: item.Meta})
	if err != nil && ctx.Err() != nil {
		// The subscription was stopped while the handler ran, so the failure
		// is most likely the handler giving up on its cancelled context, not
		// a verdict on the message. Leave it uncommitted: it is redelivered.
		return false
	}
	if errors.Is(err, events.ErrBusClosed) {
		// Nothing can handle the message. Leave it uncommitted (it will be
		// redelivered) rather than acknowledge something nobody saw.
		s.b.report(s.topic, err)
		return false
	}
	if err != nil {
		s.b.report(s.topic, fmt.Errorf("kafka backend: handler failed at partition %d offset %d: %w",
			msg.Partition, msg.Offset, err))
	}
	s.commit(ctx, msg)
	return true
}

// commit acknowledges msg. It deliberately does not use the subscription's
// context: a message handled just before the subscription is stopped should
// still be acknowledged, and a cancelled context would turn that into a
// redelivery.
func (s *subscription) commit(ctx context.Context, msg kafka.Message) {
	cctx, cancel := context.WithTimeout(context.Background(), s.b.cfg.ReadTimeout)
	defer cancel()
	if err := s.reader.CommitMessages(cctx, msg); err != nil && ctx.Err() == nil {
		s.b.report(s.topic, fmt.Errorf("kafka backend: commit partition %d offset %d: %w",
			msg.Partition, msg.Offset, err))
	}
}

// report hands err to the configured hook. The hook is user code running on
// the subscription's goroutine, so a panic inside it is contained: it must not
// be able to take the consumer — or the process — down.
func (b *Backend) report(topic string, err error) {
	if b.cfg.OnError == nil {
		return
	}
	defer func() { _ = recover() }()
	b.cfg.OnError(topic, err)
}

// sleep waits for d or for ctx, and reports whether the full time elapsed.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// Close stops every subscription, waits (up to [Config.ShutdownTimeout]) for
// handlers that are already running to return, and releases the writer. It is
// idempotent.
//
// Close must not be called from inside a handler: it waits for handlers, so it
// would wait for itself until the timeout. A handler that wants to stop its own
// subscription uses the cancel function returned by Subscribe, which does not
// wait.
func (b *Backend) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	subs := make([]*subscription, 0, len(b.subs))
	for s := range b.subs {
		subs = append(subs, s)
	}
	b.subs = make(map[*subscription]struct{})
	b.mu.Unlock()

	// Outside the lock: closing a reader talks to the broker, and a handler
	// that publishes through this backend takes b.mu to check closed.
	for _, s := range subs {
		s.shutdown()
	}

	wait, cancel := context.WithTimeout(context.Background(), b.cfg.ShutdownTimeout)
	defer cancel()
	stuck := 0
	for _, s := range subs {
		select {
		case <-s.done:
		case <-wait.Done():
			stuck++
		}
	}

	err := b.writer.Close()
	if stuck > 0 {
		// A handler that outlives the timeout is reported rather than waited
		// on for ever: shutdown that hangs is worse than a goroutine that
		// finishes on its own.
		err = errors.Join(fmt.Errorf("kafka backend: %d handler(s) still running after %s",
			stuck, b.cfg.ShutdownTimeout), err)
	}
	if b.ownsBus && stuck == 0 {
		b.bus.Close()
	}
	return err
}
