# Breeze Observability

A runtime observability layer for the Breeze Framework.

It records what actually happened inside a running application — which
events fired, which listeners ran, how long they took, what failed — and
exposes it for live streaming, querying and dashboard display.

**Nothing is recorded unless you attach it.** A detached application pays
one atomic pointer load per dispatch and nothing more.

---

## Contents

- [Quick start](#quick-start)
- [Why a separate package](#why-a-separate-package)
- [The Signal model](#the-signal-model)
- [Cost](#cost)
- [Collector](#collector)
- [Querying](#querying)
- [Live streaming](#live-streaming)
- [Metrics](#metrics)
- [The execution graph](#the-execution-graph)
- [Payload capture and masking](#payload-capture-and-masking)
- [Configuration](#configuration)
- [Extending to other subsystems](#extending-to-other-subsystems)
- [Design decisions](#design-decisions)
- [API reference](#api-reference)

---

## Quick start

```go
import (
    "github.com/nelthaarion/breeze/v2/events"
    "github.com/nelthaarion/breeze/v2/observability"
)

col := observability.NewCollector(observability.Config{
    Capacity: 1000,
    Metrics:  true,
})
defer col.Close()

detach := observability.AttachEvents(events.Default, col)
defer detach()

// ... application runs, events fire ...

for _, sig := range col.Recent(10) {
    fmt.Printf("%s took %v (%d listeners)\n",
        sig.Name, sig.Duration, sig.Executed)
}
```

That is the whole integration. `AttachEvents` returns the function that
detaches it again, so observability can be switched on and off at runtime
with no residual cost.

---

## Why a separate package

The event bus could have recorded its own history. It deliberately does
not, for three reasons.

**The bus must stay dependency-free.** `events` imports nothing outside
the standard library, and that is a property worth protecting. If the bus
knew about collectors, ring buffers and masking rules, every application
using the bus would carry them.

**Observability outlives any one subsystem.** The router, scheduler and
database layers need the same treatment. Putting the model in a shared
package means they publish `Signal` values into the same collector and
appear on the same timeline, rather than each growing its own parallel
history.

**Recording is a policy decision.** How much to keep, whether to capture
payloads, what counts as sensitive — these are deployment questions. They
belong outside the dispatch path.

The coupling surface between the two packages is one file
(`events/observer.go`) and one interface with four methods.

---

## The Signal model

Everything the layer records is a `Signal`: one flat, self-contained
record of something that happened.

```go
type Signal struct {
    ID       uint64        // assigned by the collector
    SourceID uint64        // the producer's own id (e.g. dispatch id)
    ParentID uint64        // links a child signal to its parent
    Source   Source        // "events", "router", "scheduler", ...
    Kind     Kind          // "dispatch", "listener", "request", ...
    Name     string        // event name, route pattern, job name
    Time     time.Time
    Duration time.Duration
    Executed int           // units of work that ran
    Children int           // units of work considered
    Failed    bool
    Cancelled bool
    Async     bool
    Err       string
    CorrelationID string
    RequestID     string
    Spans []Span            // per-unit detail
    Attrs map[string]string // arbitrary annotations
}
```

A `Span` is one unit of work inside a signal — for an event dispatch, one
listener:

```go
type Span struct {
    Name     string
    Duration time.Duration
    Priority int
    Phase    string  // "before", "normal", "after"
    Index    int     // position in the execution order
    Failed   bool
    Skipped  bool
    Stopped  bool
    Panicked bool
    Err      string
}
```

The model is intentionally flat rather than a tree. A dispatch and its
listeners fit in one record, which means one lock acquisition to store it
and one JSON object to send. Deep nesting would buy expressiveness the
dashboard does not need.

### Skipped, stopped, failed

These three are distinct, and conflating them is the most common way to
make a dashboard lie:

| State | Meaning | Counts as failure |
|---|---|---|
| `Skipped` | A filter rejected the event, or a once-listener was spent | No |
| `Stopped` | The listener returned `events.Stop` | No |
| `Failed` | The listener returned an error, or panicked | Yes |

A guard clause that stops propagation is doing its job. If it showed up
red, every healthy request would look broken.

---

## Cost

The claim is that observability is free when detached. Measured on a
12-core machine, one listener, `-benchtime=300ms`:

| Benchmark | ns/op | B/op | allocs/op |
|---|---|---|---|
| `Dispatch_NoObserver` | 280 | 168 | 3 |
| `Dispatch_AfterDetach` | 349 | 168 | **3** |
| `Dispatch_Observer` | 1077 | 424 | 7 |
| `Dispatch_ObserverNoMetrics` | 1029 | 424 | 7 |
| `Dispatch_ObserverWithPayload` | 2938 | 1192 | 16 |

The line that matters is the second: **after detaching, the allocation
count returns exactly to baseline**. The hook leaves nothing behind. The
remaining ns/op difference is measurement noise, not retained work.

Attached, a dispatch costs roughly 800ns and 4 extra allocations. Payload
capture roughly triples that again, which is why it is off by default.

Reproduce with:

```sh
go test -run "^$" -bench "Dispatch_" ./observability
```

### Where the attached cost goes

`Dispatch_Observer` and `Dispatch_ObserverNoMetrics` are within noise of
each other, so metrics aggregation is not the expense — assembling and
storing the signal is. If that ever needs reducing, the target is the
in-flight map, not the metrics.

---

## Collector

The collector owns a ring buffer of recent signals, lifetime metrics, and
the subscriber set.

```go
col := observability.NewCollector(observability.Config{
    Capacity: 1000,
    Metrics:  true,
})
```

Reading:

```go
col.Snapshot()        // every retained signal, oldest first
col.Recent(20)        // the 20 newest, newest first
col.ByID(id)          // one signal by collector id
col.Len()             // signals currently retained
col.Stats()           // lifetime totals
col.Dropped()         // signals dropped by slow subscribers
```

Clearing:

```go
col.Clear()   // drop retained signals, keep metrics
col.Reset()   // drop everything
```

`Clear` keeps metrics deliberately: an operator clearing the view to
watch fresh traffic rarely wants to lose the lifetime counters at the
same time.

There is also a process-wide `observability.Default()` for applications
that want one collector without threading it through.

---

## Querying

```go
col.Find(observability.Query{
    Source:       observability.SourceEvents,
    NameContains: "user",
    FailedOnly:   true,
    SlowerThan:   10 * time.Millisecond,
    Since:        time.Now().Add(-time.Hour),
    Limit:        50,
    Newest:       true,
})
```

Every field is optional; a zero `Query` matches everything. `Name` is an
exact match, `NameContains` is case-insensitive. `Newest` takes the most
recent matches rather than the first ones found, which is almost always
what a dashboard wants when combined with `Limit`.

Convenience wrappers:

```go
col.Slowest(10)        // []Signal, by duration descending
col.TopNames(10)       // []Metric, by count; ties broken alphabetically
col.Names()            // every distinct name, sorted
col.Rate(time.Minute)  // signals per second over a window
```

`TopNames` breaks ties alphabetically rather than leaving them to map
iteration order, so a dashboard table does not reshuffle between polls.

---

## Live streaming

```go
ch, unsubscribe := col.Stream()
defer unsubscribe()

for sig := range ch {
    fmt.Println(sig.Name, sig.Duration)
}
```

Each subscriber gets a buffered channel. **A subscriber that stops
reading is dropped from, never blocks, the publisher** — the drop is
counted in `col.Dropped()`. A stalled dashboard tab must not be able to
slow down the application it is observing.

`unsubscribe` closes the channel and is safe to call more than once.
`col.Close()` closes every subscriber channel, so `range` loops terminate
on their own.

Signals delivered to subscribers are independent copies: mutating one
cannot corrupt what other subscribers or the ring buffer see.

---

## Metrics

With `Metrics: true`, the collector aggregates per name:

```go
m := col.MetricFor("user.created")

m.Count      // dispatches
m.Executed   // listener executions
m.Failed     // failed dispatches
m.Total      // cumulative duration
m.Min, m.Max, m.Avg
m.Last       // time of the most recent dispatch
m.FailureRate()
```

Metrics are lifetime totals and survive ring-buffer eviction — a signal
from an hour ago is long gone, but it is still counted. `MetricFor`
returns a copy, so a caller cannot mutate collector state by accident.

Metrics are keyed by `(Source, Name)`, not by name alone. Names are only
unique within a subsystem: a route and an event may both be called
`user.created`, and their statistics must not merge. `MetricFor` scans by
name and returns the first match, which is ambiguous once more than one
subsystem publishes; `MetricForSource` is the exact lookup.

```go
m := col.MetricForSource(observability.SourceRouter, "user.created")
```


---

## The execution graph

```go
for _, node := range col.Graph() {
    fmt.Println(node.Name, node.Count)
    for _, edge := range node.Edges {
        fmt.Printf("  → %s (%d calls, %d failed)\n",
            edge.Target, edge.Count, edge.Failed)
    }
}
```

The graph is built from **observed execution**, not from the registry.
The registry knows what is registered; the graph knows what actually ran,
in what order, and how often it failed. Edges are ordered by observed
execution order, so reading a node top to bottom shows the real sequence.

---

## Payload capture and masking

Off by default. To enable:

```go
detach := observability.AttachEventsWithPayload(bus, col)
```

The payload is rendered to a short string immediately and stored as text,
never as a live reference — a captured event cannot pin application
objects in memory or expose them to later mutation.

Field names that look sensitive are masked before storage:

```go
observability.IsSensitive("password")      // true
observability.IsSensitive("api_key")       // true
observability.IsSensitive("Authorization") // true
observability.IsSensitive("user_id")       // false
```

The check covers passwords, tokens, keys, secrets, credentials, session
identifiers, authorization headers, CSRF tokens and PINs, in any casing
or separator style.

```go
observability.MaskAttrs(map[string]string{
    "user_id":  "42",        // kept
    "password": "hunter2",   // → "[MASKED]"
})
```

Masking runs at capture time, so a secret never reaches the ring buffer,
the stream or the dashboard. It is a safety net, not a licence: prefer
not to put secrets in event payloads at all.

---

## Configuration

```go
type Config struct {
    Capacity int              // retained signals; default 1000
    Metrics  bool             // aggregate per-name metrics
    ErrSink  func(error)      // internal error reporting
}
```

`Capacity` bounds memory: the ring buffer never grows past it, and the
oldest signal is evicted to make room. `ErrSink` is optional; without it
internal errors are silently dropped rather than written to stderr, on
the principle that an observability layer must not become a source of
noise in production logs.

---

## Extending to other subsystems

`Signal` is generic on purpose. A new producer publishes directly:

```go
col.Publish(observability.Signal{
    Source:    observability.SourceRouter,
    Kind:      observability.KindDispatch,
    Name:      "GET /users/:id",
    Time:      start,
    Duration:  time.Since(start),
    RequestID: reqID,
})
```

`Kind` currently has two values, `KindDispatch` for a unit of work and
`KindListener` for a child reported after its parent. New producers reuse
them rather than inventing a kind per subsystem — `Source` is what
distinguishes a route from an event.

Predefined sources: `SourceEvents`, `SourceRouter`, `SourceHTTP`,
`SourceCache`, `SourceDatabase`, `SourceScheduler`, `SourceWebSocket`,
`SourceOAuth2`, `SourcePlugin`.

Because everything lands in one collector, a `RequestID` shared between a
router signal and the event dispatches it triggered reconstructs the full
causal chain across subsystems.

---

## Design decisions

**One observer per bus.** Fanning out to multiple consumers is the
collector's job. Keeping it out of the bus is what makes the hot path a
single pointer load rather than a slice walk.

**The bus never imports this package.** The dependency runs one way. The
bus defines an interface; this package implements it.

**Observer panics are contained.** A panicking observer is recovered and
reported through the bus's panic handler, attributed to `<observer>`. It
does not abort the dispatch, and it is not charged to the listener's
panic counter — otherwise a bug in the dashboard would make every
dispatch look broken.

**The context is never retained.** `*events.Context` is pooled and reused
after a dispatch ends. The observer copies what it needs and keeps no
reference.

**In-flight state is bounded and sharded.** Dispatches are assembled in a
16-way sharded map so concurrent dispatches do not serialise on one lock.
Each shard is capped; a dispatch that somehow never ends is evicted
rather than retained forever. Listener spans per dispatch are capped at
256, with the count staying accurate even when detail is truncated.

**Async listeners publish their own signals.** An async listener finishes
after its dispatch has already been recorded. Rather than discard the
result or hold the dispatch open indefinitely, the late listener is
published as a child signal linked by `ParentID`.

---

## API reference

### Attachment

| Function | Purpose |
|---|---|
| `AttachEvents(bus, col) func()` | Attach; returns detach |
| `AttachEventsWithPayload(bus, col) func()` | Attach with payload capture |
| `NewEventObserver(col) *EventObserver` | Build an observer directly |

### Collector

| Method | Purpose |
|---|---|
| `NewCollector(Config) *Collector` | Construct |
| `Default() *Collector` | Process-wide collector |
| `Publish(Signal)` | Record a signal |
| `Close()` | Stop recording, close subscribers |

### Reading

| Method | Purpose |
|---|---|
| `Snapshot() []Signal` | All retained, oldest first |
| `Recent(n) []Signal` | Newest first |
| `ByID(id) (Signal, bool)` | Lookup by id |
| `Find(Query) []Signal` | Filtered search |
| `Slowest(n) []Signal` | By duration |
| `TopNames(n) []Metric` | By frequency |
| `Names() []string` | Distinct names, sorted |
| `Graph() []GraphNode` | Observed execution graph |
| `Metrics() []Metric` | All metrics |
| `MetricFor(name) *Metric` | First metric with that name, or nil |
| `MetricForSource(src, name) *Metric` | Exact metric, or nil |
| `Stats() Stats` | Lifetime totals |
| `Rate(window) float64` | Signals per second |
| `Len() / Dropped()` | Retained count, dropped count |

### Streaming


| Method | Purpose |
|---|---|
| `Stream() (<-chan Signal, func())` | Subscribe; returns unsubscribe |
| `Subscribers() int` | Active subscriber count |

### Masking

| Function | Purpose |
|---|---|
| `IsSensitive(name) bool` | Whether a field name looks sensitive |
| `MaskAttrs(map) map` | Copy with sensitive values masked |

---

## Testing

```sh
go test ./observability                        # 98.2% coverage
go test -race ./observability                  # race detector
go test -run "^$" -bench . ./observability     # benchmarks
```

The suite covers signal capture, priority ordering, failure and panic
recording, stop-versus-fail semantics, filtered listeners, async orphan
signals, metrics, every query filter, the graph, streaming and
backpressure, masking, observer resilience, and concurrent
attach/detach under dispatch.
