package observability

import (
	"sync"
	"time"

	"github.com/nelthaarion/breeze/events"
)

// EventObserver adapts the Breeze event bus to the observability layer.
//
// It is the first producer built on the generic Signal model; the router,
// scheduler and database layers will publish through the same Collector
// with a different [Source]. Nothing in this type is specific to the
// dashboard — it only turns dispatch callbacks into Signals.
//
// # Assembling a dispatch
//
// The bus reports a dispatch in pieces: a start, a listener start and end
// for each listener, then an end. A Signal is a single flat record, so
// the observer buffers the in-flight pieces in a small map keyed by
// dispatch id and publishes once, when the dispatch ends. The map is
// sharded to keep concurrent dispatches from serialising on one lock.
//
// # Bounded memory
//
// The in-flight map is bounded by maxInFlight. A dispatch that somehow
// never ends — impossible through the public API, but a panic in an
// exotic middleware could do it — is evicted rather than retained
// forever. Listener spans per dispatch are capped by maxSpans.
type EventObserver struct {
	col *Collector

	// shards partition in-flight dispatches by id. Sizing this to a power
	// of two lets the index be a mask rather than a modulo.
	shards [inFlightShards]inFlightShard
}

const (
	// inFlightShards is the number of independently locked partitions.
	inFlightShards = 16

	// inFlightMask turns a dispatch id into a shard index.
	inFlightMask = inFlightShards - 1

	// maxInFlight caps the dispatches tracked per shard.
	maxInFlight = 4096

	// maxSpans caps the listener spans recorded for one dispatch. A
	// dispatch with more listeners than this is still measured correctly;
	// only the per-listener detail is truncated.
	maxSpans = 256
)

type inFlightShard struct {
	mu sync.Mutex
	m  map[uint64]*inFlight
}

// inFlight is the accumulator for one dispatch that has started but not
// yet finished.
type inFlight struct {
	spans     []Span
	truncated int
}

// NewEventObserver builds an observer that publishes to col.
func NewEventObserver(col *Collector) *EventObserver {
	if col == nil {
		col = Default()
	}
	o := &EventObserver{col: col}
	for i := range o.shards {
		o.shards[i].m = make(map[uint64]*inFlight)
	}
	return o
}

// shardFor returns the shard owning a dispatch id.
func (o *EventObserver) shardFor(id uint64) *inFlightShard {
	return &o.shards[id&inFlightMask]
}

// OnEventStart begins accumulating a dispatch.
func (o *EventObserver) OnEventStart(d events.DispatchInfo) {
	sh := o.shardFor(d.EventID)

	sh.mu.Lock()
	// Evicting the whole shard on overflow is crude but predictable: it
	// bounds memory in one step and cannot livelock. It only triggers if
	// dispatches are leaking, which is already a bug elsewhere.
	if len(sh.m) >= maxInFlight {
		sh.m = make(map[uint64]*inFlight)
	}
	sh.m[d.EventID] = &inFlight{}
	sh.mu.Unlock()
}

// OnListenerStart is a no-op.
//
// Everything the layer needs is present in the matching
// [events.ListenerOutcome], and recording the start separately would
// double the map traffic on the dispatch path for no gain.
func (o *EventObserver) OnListenerStart(events.ListenerCall) {}

// OnListenerEnd appends a span to the in-flight dispatch.
func (o *EventObserver) OnListenerEnd(l events.ListenerOutcome) {
	sh := o.shardFor(l.EventID)

	sh.mu.Lock()
	f := sh.m[l.EventID]
	if f == nil {
		// The dispatch already ended, which happens for async listeners:
		// they finish after the emit returned. Publish the span as its
		// own child signal so the work is not lost.
		sh.mu.Unlock()
		o.publishOrphan(l)
		return
	}
	if len(f.spans) < maxSpans {
		f.spans = append(f.spans, spanFrom(l))
	} else {
		f.truncated++
	}
	sh.mu.Unlock()
}

// OnEventEnd completes the dispatch and publishes one Signal.
func (o *EventObserver) OnEventEnd(r events.DispatchResult) {
	sh := o.shardFor(r.EventID)

	sh.mu.Lock()
	f := sh.m[r.EventID]
	delete(sh.m, r.EventID)
	sh.mu.Unlock()

	sig := Signal{
		SourceID:      r.EventID,
		Source:        SourceEvents,
		Kind:          KindDispatch,
		Name:          r.EventName,
		Time:          r.Time,
		Duration:      r.Duration,
		DurationMS:    ms(r.Duration),
		Executed:      r.ListenersExecuted,
		Cancelled:     r.Cancelled,
		Async:         r.Async,
		CorrelationID: r.CorrelationID,
		RequestID:     r.RequestID,
	}
	if r.Err != nil {
		sig.Err = r.Err.Error()
		sig.Failed = true
	}
	if f != nil {
		sig.Spans = f.spans
		sig.Children = len(f.spans) + f.truncated
	} else {
		sig.Children = r.ListenersExecuted
	}
	if r.Payload != nil {
		sig.Attrs = map[string]string{"payload": describePayload(r.Payload)}
	}

	o.col.Publish(sig)
}

// publishOrphan records a listener that finished after its dispatch had
// already been published, which is the normal case for async listeners.
func (o *EventObserver) publishOrphan(l events.ListenerOutcome) {
	sig := Signal{
		SourceID:   l.EventID,
		ParentID:   l.EventID,
		Source:     SourceEvents,
		Kind:       KindListener,
		Name:       l.EventName,
		Time:       time.Now().Add(-l.Duration),
		Duration:   l.Duration,
		DurationMS: ms(l.Duration),
		Async:      true,
		Children:   1,
		Executed:   1,
		Spans:      []Span{spanFrom(l)},
	}
	if l.Err != nil {
		sig.Err = l.Err.Error()
		sig.Failed = !isStop(l.Err)
		sig.Cancelled = isStop(l.Err)
	}
	if l.Panicked {
		sig.Failed = true
	}
	o.col.Publish(sig)
}

// spanFrom converts a listener outcome into a Span.
func spanFrom(l events.ListenerOutcome) Span {
	sp := Span{
		Name:       l.ListenerName,
		Duration:   l.Duration,
		DurationMS: ms(l.Duration),
		Panicked:   l.Panicked,
		Skipped:    l.Skipped,
		Priority:   l.Priority,
		Phase:      l.Phase,
		Index:      l.Index,
	}
	if l.Err != nil {
		if isStop(l.Err) {
			// Stopping propagation is control flow, not failure. Marking
			// it as an error here would make every guard clause look like
			// a bug on the dashboard.
			sp.Stopped = true
		} else {
			sp.Err = l.Err.Error()
			sp.Failed = true
		}
	}
	if l.Panicked {
		sp.Failed = true
	}
	return sp
}

// isStop reports whether err is the bus's propagation-stop sentinel.
func isStop(err error) bool { return err == events.Stop }

// AttachEvents wires the given bus to the collector and returns the
// function that detaches it.
//
// Detaching restores the bus's untouched fast path, so observability can
// be switched off at runtime with no residual cost:
//
//	detach := observability.AttachEvents(events.Default, col)
//	defer detach()
func AttachEvents(bus *events.Bus, col *Collector) (detach func()) {
	obs := NewEventObserver(col)
	bus.SetObserver(obs)
	return func() { bus.SetObserver(nil) }
}

// AttachEventsWithPayload is [AttachEvents] plus event payload capture.
//
// The payload is rendered to a short string and masked, never stored as a
// live reference, so a captured event cannot pin application objects in
// memory. It costs one interface boxing per dispatch.
func AttachEventsWithPayload(bus *events.Bus, col *Collector) (detach func()) {
	obs := NewEventObserver(col)
	bus.SetObserverWithPayload(obs)
	return func() { bus.SetObserver(nil) }
}
