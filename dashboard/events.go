package dashboard

import (
	"strconv"
	"time"

	"github.com/nelthaarion/breeze/v2"
	"github.com/nelthaarion/breeze/v2/events"
	"github.com/nelthaarion/breeze/v2/observability"
)

// This file is the dashboard's bridge to the observability layer.
//
// It is deliberately the only place in the dashboard that imports
// observability. The Collector holds a nilable pointer, every read path
// tolerates nil, and nothing here runs unless the application opted in by
// calling AttachEvents. A dashboard without the event system attached
// behaves exactly as it did before.

// eventStreamBuffer bounds the observability signals the dashboard keeps
// for its own page. It is independent of the collector's own capacity:
// the dashboard only ever renders the most recent page of activity.
const eventStreamDefaultCapacity = 500

// AttachEvents connects a Breeze event bus to the dashboard.
//
// It creates an observability collector, attaches it to the bus, and wires
// its live stream into the dashboard's WebSocket hub so the Events page
// updates in real time. The returned function detaches everything and
// restores the bus's untouched fast path.
//
// Call it after Install:
//
//	coll := dashboard.Install(app, router, dashboard.DefaultConfig())
//	detach := coll.AttachEvents(events.Default)
//	defer detach()
//
// Until this is called the Events page reports that the event system is
// not attached, rather than showing an empty timeline that looks like a
// silent application.
//
// Payload capture is off. Use [Collector.AttachEventsWithPayload] to
// record event field values, which costs an extra allocation per dispatch
// and stores masked payload text.
func (c *Collector) AttachEvents(bus *events.Bus) (detach func()) {
	return c.attachEvents(bus, false)
}

// AttachEventsWithPayload is [Collector.AttachEvents] with event payload
// capture enabled.
//
// Payloads are rendered to short masked strings at capture time; fields
// whose names look sensitive (password, token, secret, key, ...) are
// replaced before storage. Even so, prefer plain AttachEvents unless the
// payload detail is genuinely needed for debugging.
func (c *Collector) AttachEventsWithPayload(bus *events.Bus) (detach func()) {
	return c.attachEvents(bus, true)
}

// attachEvents is the shared implementation.
func (c *Collector) attachEvents(bus *events.Bus, payload bool) func() {
	if bus == nil {
		return func() {}
	}

	capacity := c.cfg.EventCapacity
	if capacity <= 0 {
		capacity = eventStreamDefaultCapacity
	}

	col := observability.NewCollector(observability.Config{
		Capacity: capacity,
		Metrics:  true,
	})

	var detachBus func()
	if payload {
		detachBus = observability.AttachEventsWithPayload(bus, col)
	} else {
		detachBus = observability.AttachEvents(bus, col)
	}

	// Live workflow progress is accumulated from step events on the bus,
	// because the ring buffer only ever receives an execution once it has
	// already finished.
	live := newWorkflowLive()
	detachLive := live.attach(bus)

	c.eventsMu.Lock()
	c.eventCol = col
	c.wfLive = live
	c.eventsMu.Unlock()

	// Bridge the collector's live stream onto the dashboard hub. The
	// forwarding goroutine is what makes the Events page live rather than
	// poll-only; it exits when the stream closes.
	stopStream := c.forwardEventSignals(col)

	return func() {
		detachBus()
		detachLive()
		stopStream()
		col.Close()

		c.eventsMu.Lock()
		c.eventCol = nil
		c.wfLive = nil

		c.eventsMu.Unlock()
	}
}

// forwardEventSignals pumps signals from the observability stream into the
// WebSocket hub, and returns the function that stops it.
//
// The hub's pushEvent is a queue append, not a write, so this goroutine
// never blocks on a slow browser. If it falls behind anyway, the
// observability stream drops rather than blocking the emitting
// application — the dashboard is a consumer, never a brake.
func (c *Collector) forwardEventSignals(col *observability.Collector) func() {
	ch, unsub := col.Stream()
	done := make(chan struct{})

	go func() {
		for {
			select {
			case <-done:
				return
			case sig, ok := <-ch:
				if !ok {
					return
				}
				if c.hub != nil {
					pushEvent(c.hub, "event", eventRowFrom(sig))
				}
			}
		}
	}()

	var stopped bool
	return func() {
		if stopped {
			return
		}
		stopped = true
		close(done)
		unsub()
	}
}

// eventsCollector returns the attached observability collector, or nil.
func (c *Collector) eventsCollector() *observability.Collector {
	c.eventsMu.RLock()
	defer c.eventsMu.RUnlock()
	return c.eventCol
}

// EventsAttached reports whether an event bus is attached to the
// dashboard. The Events page uses it to distinguish "not wired up" from
// "wired up but idle", which are very different things to be looking at.
func (c *Collector) EventsAttached() bool { return c.eventsCollector() != nil }

// Observability returns the collector backing the Events page, or nil
// when no bus has been attached yet.
//
// Most subsystems reach the dashboard by emitting on the event bus. A
// few produce observability signals directly instead, because one
// signal describes a whole execution with its steps as spans — the
// workflow engine is the current example. Those subsystems must publish
// into *this* collector, not a private one, or their signals will never
// reach the page:
//
//	coll := dashboard.Install(app, router, cfg)
//	detach := coll.AttachEvents(events.Default)
//	engine := workflow.NewEngine(workflow.Config{
//	    Collector: coll.Observability(),
//	})
//
// Call it after AttachEvents; before that there is no collector to
// return and the result is nil, which every consumer treats as "do not
// publish".
func (c *Collector) Observability() *observability.Collector {
	return c.eventsCollector()
}

// ─── Wire format ──────────────────────────────────────────────────────────

// eventRow is one dispatch as the Events page consumes it.
//
// It is a flattened projection of observability.Signal rather than the
// Signal itself: the dashboard sends this over WebSocket at up to 10
// frames a second, so the payload is trimmed to what the table renders.
type eventRow struct {
	ID         uint64  `json:"id"`
	Name       string  `json:"name"`
	Source     string  `json:"source"`
	Time       string  `json:"time"`
	DurationMS float64 `json:"duration_ms"`
	Listeners  int     `json:"listeners"`
	Failed     bool    `json:"failed"`
	Cancelled  bool    `json:"cancelled"`
	Async      bool    `json:"async"`
	Error      string  `json:"error,omitempty"`
	RequestID  string  `json:"request_id,omitempty"`
	Payload    string  `json:"payload,omitempty"`

	// ExecutionID and State are populated for sources that model a
	// long-running execution rather than a single dispatch, such as
	// workflows. They stay empty for plain events, so the table can
	// show a state column only where one exists.
	ExecutionID string `json:"execution_id,omitempty"`
	State       string `json:"state,omitempty"`
	Trigger     string `json:"trigger,omitempty"`

	Spans []eventSpan `json:"spans,omitempty"`
}

// eventSpan is one listener execution within a dispatch.
type eventSpan struct {
	Name       string  `json:"name"`
	DurationMS float64 `json:"duration_ms"`
	Priority   int     `json:"priority"`
	Phase      string  `json:"phase"`
	Failed     bool    `json:"failed"`
	Skipped    bool    `json:"skipped"`
	Stopped    bool    `json:"stopped"`
	Panicked   bool    `json:"panicked"`
	Error      string  `json:"error,omitempty"`
}

// eventRowFrom projects a Signal onto the wire format.
func eventRowFrom(s observability.Signal) eventRow {
	row := eventRow{
		ID:         s.ID,
		Name:       s.Name,
		Source:     string(s.Source),
		Time:       s.Time.UTC().Format(time.RFC3339Nano),
		DurationMS: s.DurationMS,
		Listeners:  s.Executed,
		Failed:     s.Failed,
		Cancelled:  s.Cancelled,
		Async:      s.Async,
		Error:      s.Err,
		RequestID:  s.RequestID,
	}
	if s.Attrs != nil {
		row.Payload = s.Attrs["payload"]
		// Workflow executions carry their identity in attrs. Reading
		// them here keeps the projection generic: any source that
		// adopts the same keys gets the same columns for free.
		row.ExecutionID = s.Attrs["execution_id"]
		row.State = s.Attrs["state"]
		row.Trigger = s.Attrs["trigger"]
	}
	if len(s.Spans) > 0 {
		row.Spans = make([]eventSpan, 0, len(s.Spans))
		for _, sp := range s.Spans {
			row.Spans = append(row.Spans, eventSpan{
				Name:       sp.Name,
				DurationMS: sp.DurationMS,
				Priority:   sp.Priority,
				Phase:      sp.Phase,
				Failed:     sp.Failed,
				Skipped:    sp.Skipped,
				Stopped:    sp.Stopped,
				Panicked:   sp.Panicked,
				Error:      sp.Err,
			})
		}
	}
	return row
}

// eventsPayload is the Events page's API response.
type eventsPayload struct {
	// Attached reports whether a bus is wired up at all.
	Attached bool `json:"attached"`
	// Recent holds the newest dispatches, newest first.
	Recent []eventRow `json:"recent"`
	// Metrics holds per-event aggregates, busiest first.
	Metrics []eventMetric `json:"metrics"`
	// Graph is the observed execution graph.
	Graph []observability.GraphNode `json:"graph"`
	// Live holds workflow executions that have not finished yet, newest
	// first. Recent only ever contains completed executions, so this is
	// the only place an in-progress step can be seen.
	Live []liveExecution `json:"live,omitempty"`
	// Totals summarises lifetime activity.
	Totals eventTotals `json:"totals"`
}

// liveWorkflows returns the in-flight executions, or nil when no bus is
// attached.
func (c *Collector) liveWorkflows() []liveExecution {
	c.eventsMu.RLock()
	live := c.wfLive
	c.eventsMu.RUnlock()
	return live.Snapshot()
}

// eventMetric is one event's aggregate row.
type eventMetric struct {
	Name        string  `json:"name"`
	Count       uint64  `json:"count"`
	Executed    uint64  `json:"executed"`
	Failed      uint64  `json:"failed"`
	AvgMS       float64 `json:"avg_ms"`
	MaxMS       float64 `json:"max_ms"`
	FailureRate float64 `json:"failure_rate"`
	Last        string  `json:"last"`
}

// eventTotals summarises the whole event system.
type eventTotals struct {
	Signals        uint64  `json:"signals"`
	Failed         uint64  `json:"failed"`
	Listeners      uint64  `json:"listeners"`
	Dropped        uint64  `json:"dropped"`
	DistinctEvents int     `json:"distinct_events"`
	RatePerSec     float64 `json:"rate_per_sec"`
}

// ─── Handler ──────────────────────────────────────────────────────────────

// handleEvents serves the Events page data.
//
// Query parameters:
//
//	limit=200        how many recent dispatches to return
//	name=user.created  exact event name filter
//	q=user           case-insensitive substring filter
//	failed=1         only failed dispatches
//	source=workflow  restrict to one producing subsystem
//
// An unattached dashboard returns Attached:false with empty collections
// rather than an error, so the page can render a clear explanation
// instead of a failed request.
func (c *Collector) handleEvents(ctx *breeze.Context) error {
	col := c.eventsCollector()
	if col == nil {
		return ctx.JSON(eventsPayload{
			Attached: false,
			Recent:   []eventRow{},
			Metrics:  []eventMetric{},
			Graph:    []observability.GraphNode{},
		})
	}

	limit := 200
	if v := ctx.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			// Capped so a hand-edited URL cannot ask for an unbounded
			// response.
			if n > 1000 {
				n = 1000
			}
			limit = n
		}
	}

	q := observability.Query{
		Name:         ctx.Query("name"),
		NameContains: ctx.Query("q"),
		FailedOnly:   ctx.Query("failed") == "1" || ctx.Query("failed") == "true",
		Limit:        limit,
		Newest:       true,
	}
	// An empty or "all" source means no restriction, so the filter
	// degrades to the previous behaviour rather than matching nothing.
	if src := ctx.Query("source"); src != "" && src != "all" {
		q.Source = observability.Source(src)
	}

	// Find with Newest already returns newest-first, which is the order the
	// table renders in, so no reordering is needed here.
	sigs := col.Find(q)
	rows := make([]eventRow, 0, len(sigs))
	for _, s := range sigs {
		rows = append(rows, eventRowFrom(s))
	}

	metrics := col.TopNames(50)
	mrows := make([]eventMetric, 0, len(metrics))
	for _, m := range metrics {
		row := eventMetric{
			Name:        m.Name,
			Count:       m.Count,
			Executed:    m.Executed,
			Failed:      m.Failed,
			AvgMS:       m.AvgMS,
			MaxMS:       float64(m.Max.Microseconds()) / 1000.0,
			FailureRate: m.FailureRate(),
		}
		if !m.Last.IsZero() {
			row.Last = m.Last.UTC().Format(time.RFC3339)
		}
		mrows = append(mrows, row)
	}

	st := col.Stats()
	graph := col.Graph()
	if graph == nil {
		graph = []observability.GraphNode{}
	}

	return ctx.JSON(eventsPayload{
		Attached: true,
		Recent:   rows,
		Metrics:  mrows,
		Graph:    graph,
		Live:     c.liveWorkflows(),

		Totals: eventTotals{
			Signals:        st.Signals,
			Failed:         st.Failed,
			Listeners:      st.Executed,
			Dropped:        col.Dropped(),
			DistinctEvents: len(col.Names()),
			RatePerSec:     col.Rate(time.Minute),
		},
	})
}
