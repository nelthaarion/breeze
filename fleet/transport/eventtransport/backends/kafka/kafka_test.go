package kafka

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nelthaarion/go-json-plus"

	"github.com/nelthaarion/breeze/v2/diag"
	"github.com/nelthaarion/breeze/v2/events"
	kafka "github.com/segmentio/kafka-go"
)

// These tests run the backend against fakes of the two kafka-go types it
// uses, so delivery, commit ordering, error handling and shutdown are checked
// without a broker. The fakes model only what the backend relies on: a reader
// returns io.EOF once closed and honours its context; a writer honours its.

type fakeWriter struct {
	mu     sync.Mutex
	msgs   []kafka.Message
	closes int
	block  bool // WriteMessages waits for its context instead of returning
}

func (w *fakeWriter) WriteMessages(ctx context.Context, msgs ...kafka.Message) error {
	if w.block {
		<-ctx.Done()
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	w.mu.Lock()
	w.msgs = append(w.msgs, msgs...)
	w.mu.Unlock()
	return nil
}

func (w *fakeWriter) Close() error {
	w.mu.Lock()
	w.closes++
	w.mu.Unlock()
	return nil
}

func (w *fakeWriter) written() []kafka.Message {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]kafka.Message(nil), w.msgs...)
}

type fakeReader struct {
	in        chan kafka.Message
	closedCh  chan struct{}
	closeOnce sync.Once
	closes    atomic.Int64
	fetches   atomic.Int64

	mu        sync.Mutex
	committed []kafka.Message
	commitErr error
	fetchErr  error
}

func newFakeReader() *fakeReader {
	return &fakeReader{in: make(chan kafka.Message, 16), closedCh: make(chan struct{})}
}

func (r *fakeReader) FetchMessage(ctx context.Context) (kafka.Message, error) {
	r.fetches.Add(1)
	r.mu.Lock()
	ferr := r.fetchErr
	r.mu.Unlock()
	if ferr != nil {
		return kafka.Message{}, ferr
	}
	select {
	case m := <-r.in:
		return m, nil
	case <-ctx.Done():
		return kafka.Message{}, ctx.Err()
	case <-r.closedCh:
		return kafka.Message{}, io.EOF
	}
}

func (r *fakeReader) CommitMessages(_ context.Context, msgs ...kafka.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.commitErr != nil {
		err := r.commitErr
		r.commitErr = nil // fail once
		return err
	}
	r.committed = append(r.committed, msgs...)
	return nil
}

func (r *fakeReader) Close() error {
	r.closes.Add(1)
	r.closeOnce.Do(func() { close(r.closedCh) })
	return nil
}

func (r *fakeReader) commits() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]int64, 0, len(r.committed))
	for _, m := range r.committed {
		out = append(out, m.Offset)
	}
	return out
}

// harness is a Backend wired to fakes.
type harness struct {
	b  *Backend
	w  *fakeWriter
	mu sync.Mutex

	readers map[string]*fakeReader
	// fetchErr, when set before Subscribe, is what every new reader's fetch
	// returns. It has to be in place before the consuming goroutine starts: a
	// fetch already blocked waiting for a message never looks at it again.
	fetchErr error
}

func (h *harness) reader(topic string) *fakeReader {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.readers[topic]
}

func newHarness(t *testing.T, cfg Config) *harness {
	t.Helper()
	cfg.Brokers = []string{"unused:9092"}
	cfg.GroupID = "test-group"
	b, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{b: b, w: &fakeWriter{}, readers: make(map[string]*fakeReader)}
	b.writer = h.w
	b.newReader = func(topic string) messageReader {
		r := newFakeReader()
		h.mu.Lock()
		r.fetchErr = h.fetchErr
		h.readers[topic] = r
		h.mu.Unlock()
		return r
	}
	t.Cleanup(func() { _ = b.Close() })
	return h
}

func envelopeMsg(t *testing.T, offset int64, payload string, meta events.Meta) kafka.Message {
	t.Helper()
	body, err := json.Marshal(envelope{Payload: []byte(payload), Meta: meta})
	if err != nil {
		t.Fatal(err)
	}
	return kafka.Message{Partition: 3, Offset: offset, Value: body}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

type errLog struct {
	mu   sync.Mutex
	errs []error
}

func (l *errLog) hook(_ string, err error) {
	l.mu.Lock()
	l.errs = append(l.errs, err)
	l.mu.Unlock()
}

func (l *errLog) all() []error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]error(nil), l.errs...)
}

// ─── publishing ───────────────────────────────────────────────────────────

func TestPublishWritesTheEnvelopeToTheTopic(t *testing.T) {
	h := newHarness(t, Config{})
	meta := events.Meta{"x-trace": "abc"}
	if err := h.b.Publish("fleet.spans", []byte("hello"), meta); err != nil {
		t.Fatal(err)
	}
	got := h.w.written()
	if len(got) != 1 || got[0].Topic != "fleet.spans" {
		t.Fatalf("written=%+v, want one message for fleet.spans", got)
	}
	var env envelope
	if err := json.Unmarshal(got[0].Value, &env); err != nil {
		t.Fatal(err)
	}
	if string(env.Payload) != "hello" || env.Meta.Get("x-trace") != "abc" {
		t.Fatalf("envelope=%+v", env)
	}
}

// Backend.Publish has no context, so the caller's deadline used to be dropped
// on its way to the broker: a stuck broker held the caller for the full
// WriteTimeout regardless.
func TestPublishContextPropagatesTheCallersDeadline(t *testing.T) {
	h := newHarness(t, Config{WriteTimeout: 5 * time.Second})
	h.w.block = true

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := h.b.PublishContext(ctx, "t", []byte("x"), nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want context.DeadlineExceeded", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("PublishContext took %v; the caller's 40ms deadline was not honoured", d)
	}

	// Through the events package's helper, which is what eventtransport uses.
	err = events.PublishVia(ctx, h.b, "t", []byte("x"), nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("PublishVia: err=%v, want context.DeadlineExceeded", err)
	}
}

func TestPublishRejectsBadInput(t *testing.T) {
	h := newHarness(t, Config{})
	if err := h.b.Publish("", []byte("x"), nil); !errors.Is(err, events.ErrEmptyTopic) {
		t.Errorf("empty topic: err=%v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.b.PublishContext(cancelled, "t", nil, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled ctx: err=%v", err)
	}
	if n := len(h.w.written()); n != 0 {
		t.Errorf("%d messages written for rejected publishes", n)
	}
}

// ─── consuming ────────────────────────────────────────────────────────────

func TestSubscribeDeliversPayloadAndMeta(t *testing.T) {
	h := newHarness(t, Config{})
	got := make(chan events.RawMessage, 1)
	if _, err := h.b.Subscribe("t", func(_ *events.Context, m events.RawMessage) error {
		got <- m
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h.reader("t").in <- envelopeMsg(t, 1, "body", events.Meta{"k": "v"})

	select {
	case m := <-got:
		if m.Topic != "t" || string(m.Payload) != "body" || m.Meta.Get("k") != "v" {
			t.Fatalf("message=%+v", m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("message not delivered")
	}
}

// ReadMessage commits before returning, so the old consume loop acknowledged a
// message before its handler ran: a crash, or a handler that never finished,
// lost it. The offset must only move once the handler has returned.
func TestMessageIsCommittedOnlyAfterItsHandlerReturns(t *testing.T) {
	h := newHarness(t, Config{})
	entered, release := make(chan struct{}), make(chan struct{})
	if _, err := h.b.Subscribe("t", func(*events.Context, events.RawMessage) error {
		close(entered)
		<-release
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r := h.reader("t")
	r.in <- envelopeMsg(t, 7, "x", nil)

	<-entered
	time.Sleep(40 * time.Millisecond)
	if c := r.commits(); len(c) != 0 {
		t.Fatalf("offsets %v committed while the handler was still running", c)
	}
	close(release)
	waitFor(t, "the commit", func() bool { c := r.commits(); return len(c) == 1 && c[0] == 7 })
}

// A failing handler used to lose its error (the result of PublishBus was
// discarded) and, because of the commit-first order, the message with it.
func TestHandlerErrorIsReportedAndTheMessageStillCommitted(t *testing.T) {
	var log errLog
	h := newHarness(t, Config{OnError: log.hook})
	boom := errors.New("boom")
	var calls atomic.Int64
	if _, err := h.b.Subscribe("t", func(*events.Context, events.RawMessage) error {
		if calls.Add(1) == 1 {
			return boom
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r := h.reader("t")
	r.in <- envelopeMsg(t, 1, "a", nil)
	r.in <- envelopeMsg(t, 2, "b", nil)

	waitFor(t, "both commits", func() bool { return len(r.commits()) == 2 })
	errs := log.all()
	if len(errs) != 1 || !errors.Is(errs[0], boom) {
		t.Fatalf("OnError got %v, want exactly the handler's error", errs)
	}
	if calls.Load() != 2 {
		t.Fatalf("handler ran %d times, want 2: a failed message must not stop the subscription", calls.Load())
	}
}

func TestUndecodableMessageIsReportedAndSkipped(t *testing.T) {
	var log errLog
	h := newHarness(t, Config{OnError: log.hook})
	var handled atomic.Int64
	if _, err := h.b.Subscribe("t", func(*events.Context, events.RawMessage) error { handled.Add(1); return nil }); err != nil {
		t.Fatal(err)
	}
	r := h.reader("t")
	r.in <- kafka.Message{Offset: 1, Value: []byte("this is not json")}
	r.in <- envelopeMsg(t, 2, "ok", nil)

	waitFor(t, "both commits", func() bool { return len(r.commits()) == 2 })
	if handled.Load() != 1 {
		t.Fatalf("handler ran %d times, want 1 (only the decodable message)", handled.Load())
	}
	if errs := log.all(); len(errs) != 1 {
		t.Fatalf("OnError got %d errors, want 1: %v", len(errs), errs)
	}
}

func TestCommitFailureIsReportedAndDoesNotStopTheSubscription(t *testing.T) {
	var log errLog
	h := newHarness(t, Config{OnError: log.hook})
	var handled atomic.Int64
	if _, err := h.b.Subscribe("t", func(*events.Context, events.RawMessage) error { handled.Add(1); return nil }); err != nil {
		t.Fatal(err)
	}
	r := h.reader("t")
	r.mu.Lock()
	r.commitErr = errors.New("rebalance in progress")
	r.mu.Unlock()
	r.in <- envelopeMsg(t, 1, "a", nil)
	r.in <- envelopeMsg(t, 2, "b", nil)

	waitFor(t, "second message handled", func() bool { return handled.Load() == 2 })
	waitFor(t, "second commit", func() bool { c := r.commits(); return len(c) == 1 && c[0] == 2 })
	if len(log.all()) != 1 {
		t.Fatalf("OnError got %v, want exactly the commit failure", log.all())
	}
}

// Fetch errors used to be retried with no pause at all.
func TestFetchErrorsBackOffInsteadOfSpinning(t *testing.T) {
	var log errLog
	h := newHarness(t, Config{OnError: log.hook})
	h.fetchErr = errors.New("broker unreachable")
	if _, err := h.b.Subscribe("t", func(*events.Context, events.RawMessage) error { return nil }); err != nil {
		t.Fatal(err)
	}
	r := h.reader("t")

	time.Sleep(350 * time.Millisecond)
	if n := r.fetches.Load(); n > 8 {
		t.Fatalf("%d fetch attempts in 350ms: the loop is spinning instead of backing off", n)
	}
	if len(log.all()) == 0 {
		t.Fatal("fetch errors were not reported")
	}
}

// A panic in the user's OnError hook must not kill the consumer goroutine.
func TestPanickingOnErrorHookIsContained(t *testing.T) {
	var calls atomic.Int64
	h := newHarness(t, Config{OnError: func(string, error) { calls.Add(1); panic("hook bug") }})
	var handled atomic.Int64
	if _, err := h.b.Subscribe("t", func(*events.Context, events.RawMessage) error { handled.Add(1); return nil }); err != nil {
		t.Fatal(err)
	}
	r := h.reader("t")
	r.in <- kafka.Message{Offset: 1, Value: []byte("garbage")}
	r.in <- envelopeMsg(t, 2, "ok", nil)
	waitFor(t, "the valid message after the hook panicked", func() bool { return handled.Load() == 1 })
	if calls.Load() == 0 {
		t.Fatal("the hook was never called")
	}
}

// The documented (and previously implicit) property: messages are dispatched on
// the local bus by topic, so a subscriber registered directly on a shared bus
// sees what Kafka delivers.
func TestSharedBusSubscribersSeeKafkaMessages(t *testing.T) {
	bus := events.New(events.Config{DisableDiagnostics: true})
	defer bus.Close()
	h := newHarness(t, Config{Bus: bus})

	direct := make(chan events.RawMessage, 1)
	events.SubscribeBus(bus, "t", func(_ *events.Context, m events.RawMessage) error { direct <- m; return nil })
	if _, err := h.b.Subscribe("t", func(*events.Context, events.RawMessage) error { return nil }); err != nil {
		t.Fatal(err)
	}
	h.reader("t").in <- envelopeMsg(t, 1, "shared", nil)

	select {
	case m := <-direct:
		if string(m.Payload) != "shared" {
			t.Fatalf("payload=%q", m.Payload)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a subscriber on the shared bus did not see the Kafka message")
	}
}

// ─── lifecycle ────────────────────────────────────────────────────────────

func TestSubscribeValidatesItsArguments(t *testing.T) {
	h := newHarness(t, Config{})
	if _, err := h.b.Subscribe("", func(*events.Context, events.RawMessage) error { return nil }); !errors.Is(err, events.ErrEmptyTopic) {
		t.Errorf("empty topic: err=%v", err)
	}
	if _, err := h.b.Subscribe("t", nil); !errors.Is(err, events.ErrNilHandler) {
		t.Errorf("nil handler: err=%v", err)
	}
	if h.reader("t") != nil {
		t.Error("a reader was created for a rejected subscription")
	}
}

func TestCancelStopsOnlyThatSubscriptionAndIsIdempotent(t *testing.T) {
	h := newHarness(t, Config{})
	var a, b atomic.Int64
	cancelA, err := h.b.Subscribe("a", func(*events.Context, events.RawMessage) error { a.Add(1); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.b.Subscribe("b", func(*events.Context, events.RawMessage) error { b.Add(1); return nil }); err != nil {
		t.Fatal(err)
	}

	cancelA()
	cancelA()
	cancelA()

	// The cancelled subscription is forgotten at once, not at Close: b.cancels
	// only ever grew, so an application that churned subscriptions leaked.
	h.b.mu.Lock()
	n := len(h.b.subs)
	h.b.mu.Unlock()
	if n != 1 {
		t.Fatalf("backend tracks %d subscriptions after one was cancelled, want 1", n)
	}
	if c := h.reader("a").closes.Load(); c != 1 {
		t.Fatalf("the cancelled reader was closed %d times, want exactly 1", c)
	}

	h.reader("a").in <- envelopeMsg(t, 1, "x", nil)
	h.reader("b").in <- envelopeMsg(t, 1, "y", nil)
	waitFor(t, "the surviving subscription", func() bool { return b.Load() == 1 })
	time.Sleep(30 * time.Millisecond)
	if a.Load() != 0 {
		t.Fatal("a cancelled subscription still received a message")
	}
}

// A handler may stop its own subscription; that must not deadlock.
func TestHandlerCanCancelItsOwnSubscription(t *testing.T) {
	h := newHarness(t, Config{})
	var cancel func()
	var once sync.Once
	done := make(chan struct{})
	c, err := h.b.Subscribe("t", func(*events.Context, events.RawMessage) error {
		once.Do(func() { cancel(); close(done) })
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	cancel = c
	h.reader("t").in <- envelopeMsg(t, 1, "x", nil)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("cancel from inside the handler deadlocked")
	}
}

// Close used to hold the lock while closing readers and returned without
// waiting for the consuming goroutines, so handlers could still be running
// after it returned.
func TestCloseWaitsForARunningHandlerWhichSeesItsContextCancelled(t *testing.T) {
	h := newHarness(t, Config{})
	entered := make(chan struct{})
	var returned atomic.Bool
	if _, err := h.b.Subscribe("t", func(ctx *events.Context, _ events.RawMessage) error {
		close(entered)
		<-ctx.Ctx.Done() // a well-behaved handler stops when the subscription does
		time.Sleep(30 * time.Millisecond)
		returned.Store(true)
		return ctx.Ctx.Err()
	}); err != nil {
		t.Fatal(err)
	}
	r := h.reader("t")
	r.in <- envelopeMsg(t, 5, "x", nil)
	<-entered

	if err := h.b.Close(); err != nil {
		t.Fatal(err)
	}
	if !returned.Load() {
		t.Fatal("Close returned while a handler was still running")
	}
	// The handler was interrupted, not finished: the message must stay
	// uncommitted so it is redelivered.
	if c := r.commits(); len(c) != 0 {
		t.Fatalf("offsets %v committed for a message whose handler was interrupted by shutdown", c)
	}
}

func TestCloseGivesUpOnAHandlerThatNeverReturns(t *testing.T) {
	h := newHarness(t, Config{ShutdownTimeout: 100 * time.Millisecond})
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	if _, err := h.b.Subscribe("t", func(*events.Context, events.RawMessage) error {
		close(entered)
		<-release // ignores its context
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h.reader("t").in <- envelopeMsg(t, 1, "x", nil)
	<-entered

	start := time.Now()
	err := h.b.Close()
	if err == nil {
		t.Fatal("Close reported success although a handler is still running")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Close took %v with a 100ms ShutdownTimeout", d)
	}
	if h.w.closes != 1 {
		t.Fatalf("writer closed %d times, want 1 (even when a handler is stuck)", h.w.closes)
	}
}

func TestCloseIsIdempotentAndRejectsFurtherUse(t *testing.T) {
	h := newHarness(t, Config{})
	if _, err := h.b.Subscribe("t", func(*events.Context, events.RawMessage) error { return nil }); err != nil {
		t.Fatal(err)
	}
	r := h.reader("t")

	if err := h.b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.b.Close(); err != nil {
		t.Fatal(err)
	}
	if h.w.closes != 1 {
		t.Fatalf("writer closed %d times, want 1", h.w.closes)
	}
	if c := r.closes.Load(); c != 1 {
		t.Fatalf("reader closed %d times, want 1", c)
	}
	if err := h.b.Publish("t", nil, nil); !errors.Is(err, events.ErrBackendClosed) {
		t.Errorf("Publish after Close: err=%v", err)
	}
	if _, err := h.b.Subscribe("t", func(*events.Context, events.RawMessage) error { return nil }); !errors.Is(err, events.ErrBackendClosed) {
		t.Errorf("Subscribe after Close: err=%v", err)
	}
	if !h.b.bus.Closed() {
		t.Error("the private bus was not closed with its backend")
	}
}

func TestCloseLeavesASharedBusOpen(t *testing.T) {
	bus := events.New(events.Config{DisableDiagnostics: true})
	defer bus.Close()
	h := newHarness(t, Config{Bus: bus})
	if err := h.b.Close(); err != nil {
		t.Fatal(err)
	}
	if bus.Closed() {
		t.Fatal("Close closed a bus the caller supplied")
	}
}

// A closed local bus means nothing can handle messages; acknowledging them
// anyway would lose them.
func TestMessagesAreNotCommittedWhenTheLocalBusIsClosed(t *testing.T) {
	bus := events.New(events.Config{DisableDiagnostics: true})
	h := newHarness(t, Config{Bus: bus})
	if _, err := h.b.Subscribe("t", func(*events.Context, events.RawMessage) error { return nil }); err != nil {
		t.Fatal(err)
	}
	bus.Close()
	r := h.reader("t")
	r.in <- envelopeMsg(t, 1, "x", nil)

	time.Sleep(60 * time.Millisecond)
	if c := r.commits(); len(c) != 0 {
		t.Fatalf("offsets %v committed although nothing could handle the message", c)
	}
}

// ─── construction ─────────────────────────────────────────────────────────

func TestNewValidatesItsConfig(t *testing.T) {
	good := Config{Brokers: []string{"b:9092"}, GroupID: "g"}
	cases := map[string]func(*Config){
		"no brokers":            func(c *Config) { c.Brokers = nil },
		"no group":              func(c *Config) { c.GroupID = "" },
		"negative commit":       func(c *Config) { c.CommitInterval = -1 },
		"bogus start offset":    func(c *Config) { c.StartOffset = 42 },
		"bogus negative offset": func(c *Config) { c.StartOffset = -7 },
	}
	for name, mutate := range cases {
		cfg := good
		mutate(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: New accepted the config", name)
		}
	}
	for _, so := range []int64{0, kafka.FirstOffset, kafka.LastOffset} {
		cfg := good
		cfg.StartOffset = so
		b, err := New(cfg)
		if err != nil {
			t.Errorf("StartOffset %d rejected: %v", so, err)
			continue
		}
		_ = b.Close()
	}
}

// kafka-go flushes a partial batch only when BatchTimeout expires, and its
// default is one second, so every synchronous Publish of a single message took
// about a second.
func TestWriterIsConfiguredForLowLatency(t *testing.T) {
	b, err := New(Config{Brokers: []string{"b:9092"}, GroupID: "g"})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	w := b.writer.(*kafka.Writer)
	if w.BatchTimeout != 10*time.Millisecond {
		t.Fatalf("BatchTimeout=%v, want 10ms (kafka-go's own default is 1s)", w.BatchTimeout)
	}

	b2, err := New(Config{Brokers: []string{"b:9092"}, GroupID: "g", BatchTimeout: 250 * time.Millisecond, BatchSize: 7})
	if err != nil {
		t.Fatal(err)
	}
	defer b2.Close()
	w2 := b2.writer.(*kafka.Writer)
	if w2.BatchTimeout != 250*time.Millisecond || w2.BatchSize != 7 {
		t.Fatalf("BatchTimeout=%v BatchSize=%d, want the configured 250ms and 7", w2.BatchTimeout, w2.BatchSize)
	}
}

// Reader.Close blocks until the in-flight long-poll fetch returns, and
// kafka-go's default MaxWait is ten seconds, so closing a backend with live
// readers could take that long.
func TestReadersUseAShortFetchWait(t *testing.T) {
	b, err := New(Config{Brokers: []string{"unused.invalid:9092"}, GroupID: "g"})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	r := b.defaultReader("t").(*kafka.Reader)
	defer r.Close()
	if got := r.Config().MaxWait; got != 500*time.Millisecond {
		t.Fatalf("MaxWait=%v, want 500ms (kafka-go's default of 10s makes Close take that long)", got)
	}

	b2, err := New(Config{Brokers: []string{"unused.invalid:9092"}, GroupID: "g", MaxWait: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer b2.Close()
	r2 := b2.defaultReader("t").(*kafka.Reader)
	defer r2.Close()
	if got := r2.Config().MaxWait; got != 2*time.Second {
		t.Fatalf("MaxWait=%v, want the configured 2s", got)
	}
}

type diagProbeEvent struct{}

func reportedListeners(t *testing.T) int {
	t.Helper()
	r, ok := diag.Get("events")
	if !ok {
		t.Fatal("no events diagnostic registered")
	}
	n, _ := r.Detail["listeners"].(int)
	return n
}

// New used events.New() for its private bus, which registers that bus as the
// process's event-bus diagnostic — silently replacing the application's.
func TestPrivateBusDoesNotReplaceTheProcessDiagnostic(t *testing.T) {
	t.Cleanup(func() { events.RegisterDiagnostics(events.Default) })

	app := events.New(events.Config{OnPanic: func(*events.PanicError) {}})
	events.OnBus(app, diagProbeEvent{}, func(*events.Context, diagProbeEvent) error { return nil })
	if got := reportedListeners(t); got != 1 {
		t.Fatalf("setup: probe reports %d listeners, want 1", got)
	}

	b, err := New(Config{Brokers: []string{"b:9092"}, GroupID: "g"})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if got := reportedListeners(t); got != 1 {
		t.Fatalf("creating a backend replaced the application's diagnostic: probe reports %d listeners, want 1", got)
	}
}
