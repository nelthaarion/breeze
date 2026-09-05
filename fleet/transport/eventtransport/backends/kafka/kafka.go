// Package kafka adapts Kafka to Breeze's generic events.Backend seam.
//
// The base Breeze module deliberately does not import kafka-go. Applications
// that need a broker opt into this nested module and pass the returned backend
// to eventtransport.New. The wire envelope is intentionally small and stable:
// topic selection remains Kafka's topic, while the payload and events.Meta stay
// together so eventtransport's signing and trace metadata survive the hop.
package kafka

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/nelthaarion/breeze/v2/events"
	kafka "github.com/segmentio/kafka-go"
)

type Config struct {
	Brokers      []string
	GroupID      string
	DialTimeout  time.Duration
	WriteTimeout time.Duration
	ReadTimeout  time.Duration
	Bus          *events.Bus
}

type envelope struct {
	Payload []byte      `json:"payload,omitempty"`
	Meta    events.Meta `json:"meta,omitempty"`
}

type Backend struct {
	cfg    Config
	writer *kafka.Writer
	bus    *events.Bus

	mu      sync.Mutex
	readers map[*kafka.Reader]struct{}
	cancels []func()
	closed  bool
}

// New creates a Kafka-backed events backend. It only validates local config;
// Kafka connectivity is established lazily by WriteMessages/ReadMessage.
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
	bus := cfg.Bus
	if bus == nil {
		bus = events.New()
	}
	return &Backend{
		cfg: cfg,
		writer: &kafka.Writer{
			Addr:         kafka.TCP(cfg.Brokers...),
			Balancer:     &kafka.Hash{},
			WriteTimeout: cfg.WriteTimeout,
			RequiredAcks: kafka.RequireOne,
		},
		bus:     bus,
		readers: make(map[*kafka.Reader]struct{}),
	}, nil
}

var _ events.Backend = (*Backend)(nil)

func (b *Backend) Publish(topic string, payload []byte, meta events.Meta) error {
	if topic == "" {
		return events.ErrEmptyTopic
	}
	b.mu.Lock()
	closed := b.closed
	b.mu.Unlock()
	if closed {
		return errors.New("kafka backend: closed")
	}
	body, err := json.Marshal(envelope{Payload: payload, Meta: meta.Clone()})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), b.cfg.WriteTimeout)
	defer cancel()
	return b.writer.WriteMessages(ctx, kafka.Message{Topic: topic, Value: body})
}

// Subscribe starts one consumer-group reader for topic. The returned cancel
// function is safe to call repeatedly and closes only that subscription.
func (b *Backend) Subscribe(topic string, fn events.MessageHandler) (func(), error) {
	if topic == "" {
		return nil, events.ErrEmptyTopic
	}
	if fn == nil {
		return nil, errors.New("kafka backend: nil handler")
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, errors.New("kafka backend: closed")
	}
	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:        b.cfg.Brokers,
		Topic:          topic,
		GroupID:        b.cfg.GroupID,
		Dialer:         &kafka.Dialer{Timeout: b.cfg.DialTimeout},
		ReadBackoffMin: 100 * time.Millisecond,
		ReadBackoffMax: time.Second,
	})
	ctx, cancel := context.WithCancel(context.Background())
	localSub := events.SubscribeBus(b.bus, topic, fn)
	stop := func() {
		cancel()
		localSub.Unsubscribe()
		_ = reader.Close()
	}
	b.readers[reader] = struct{}{}
	b.cancels = append(b.cancels, stop)
	b.mu.Unlock()

	go b.consume(ctx, reader, topic, fn)
	return stop, nil
}

func (b *Backend) consume(
	ctx context.Context,
	reader *kafka.Reader,
	topic string,
	fn events.MessageHandler,
) {
	defer func() {
		_ = reader.Close()
		b.mu.Lock()
		delete(b.readers, reader)
		b.mu.Unlock()
	}()
	for {
		readCtx, cancel := context.WithTimeout(ctx, b.cfg.ReadTimeout)
		msg, err := reader.ReadMessage(readCtx)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		var item envelope
		if json.Unmarshal(msg.Value, &item) != nil {
			continue
		}
		// Publish through the local typed bus so the callback receives a
		// correctly initialized events.Context and follows normal dispatch
		// middleware/observer semantics.
		_ = events.PublishBus(b.bus, topic, item.Payload, item.Meta)
	}
}

// Close stops readers and releases the shared writer. It is idempotent.
func (b *Backend) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	readers := make([]*kafka.Reader, 0, len(b.readers))
	for reader := range b.readers {
		readers = append(readers, reader)
	}
	for _, stop := range b.cancels {
		stop()
	}
	b.mu.Unlock()
	for _, reader := range readers {
		_ = reader.Close()
	}
	return b.writer.Close()
}
