# events

**events** is the internal event bus powering the Breeze framework. It provides a production-grade, type-safe communication layer for framework subsystems and application code.

## Features

- **Type-safe**: Go generics eliminate runtime type assertions
- **Zero-reflection dispatch**: event types are resolved at compile time
- **Lock-free reads**: copy-on-write snapshots scale with cores
- **Async-ready**: goroutine or worker-pool modes
- **Priority & phases**: deterministic execution order
- **Middleware**: wrap every dispatch
- **Panic recovery**: one broken plugin cannot crash the bus
- **Filters**: skip listeners before they run
- **Once listeners**: automatic cleanup
- **Context propagation**: `context.Context` + metadata
- **Inspection**: runtime introspection for dashboards
- **Metrics**: per-event statistics
- **Event recorder**: ring-buffer history for debugging
- **Framework events**: built-in events for HTTP, WebSocket, OAuth2, routing, scheduling, and plugins

## Installation

```bash
go get github.com/nelthaarion/breeze/v2/events
```

## Quick Start

```go
package main

import (
	"fmt"
	"github.com/nelthaarion/breeze/v2/events"
)

type UserCreated struct {
	UserID uint64
	Email  string
}

func main() {
	// Register a listener
	sub := events.On(UserCreated{}, func(ctx *events.Context, e UserCreated) error {
		fmt.Printf("Welcome, user %d!\n", e.UserID)
		return nil
	})
	defer sub.Unsubscribe()

	// Emit an event
	events.Emit(UserCreated{UserID: 42, Email: "user@example.com"})
}
```

## Core Concepts

### Events

Events are plain Go structs. No interfaces, no embedding, no tags:

```go
type OrderPlaced struct {
	OrderID   uint64
	Total     float64
	Timestamp time.Time
}
```

### Listeners

Register a listener for an event type:

```go
events.On(OrderPlaced{}, func(ctx *events.Context, e OrderPlaced) error {
	// Handle the event
	return nil
})
```

The handler receives:
- `*events.Context`: per-dispatch metadata and control
- The event value

### Emit

Dispatch an event to its listeners:

```go
err := events.Emit(OrderPlaced{OrderID: 123, Total: 99.99})
```

Listeners run synchronously in priority order. The first error stops propagation and is returned to the caller.

### Async Emit

Dispatch without waiting:

```go
events.EmitAsync(OrderPlaced{OrderID: 123, Total: 99.99})
```

Errors go to `Config.OnError`; the call returns once listeners are scheduled.

## Priorities

Control execution order:

```go
events.On(UserCreated{}, Validate).Priority(events.PriorityHighest)
events.On(UserCreated{}, Save).Priority(events.PriorityNormal)
events.On(UserCreated{}, SendEmail).Priority(events.PriorityLow)
```

Higher priorities run first. Priorities within a lifecycle phase:

- `PriorityHighest` (1000): validation, security checks
- `PriorityHigh` (100): normalization, enrichment
- `PriorityNormal` (0): default
- `PriorityLow` (-100): persistence
- `PriorityLowest` (-1000): auditing, metrics

## Lifecycle Phases

Listeners are grouped into three phases, which always execute in this order regardless of priority:

```go
events.Before(RequestStarted{}, Logger)   // phase 1
events.On(RequestStarted{}, Handler)      // phase 2
events.After(RequestStarted{}, Metrics)   // phase 3
```

**Before hooks** prepare state that ordinary listeners depend on.  
**After hooks** run cleanup, metrics, and auditing.

Priority controls the order *within* each phase.

## Filters

Skip a listener when the event doesn't match:

```go
events.On(UserCreated{}, SendPromoEmail).
	Where(func(e UserCreated) bool { return e.Age >= 18 })
```

The filter is evaluated before the handler runs, so a rejected event never enters the listener.

## Once Listeners

Run at most once, then remove automatically:

```go
events.Once(AppStarted{}, InitializeCache)
```

Concurrent emits are safe: exactly one invocation happens.

## Stop Propagation

A listener may halt the dispatch:

```go
events.On(RequestStarted{}, CheckAuth).Priority(events.PriorityHighest)

func CheckAuth(ctx *events.Context, e RequestStarted) error {
	if !authorized(e.UserID) {
		return events.Stop
	}
	return nil
}
```

`Stop` is not returned to the caller — it signals normal control flow, not failure. The dispatcher consumes it and returns `nil`.

## Context

Every listener receives `*events.Context`:

```go
func Handler(ctx *events.Context, e MyEvent) error {
	// Immutable fields
	_ = ctx.Time       // when the dispatch started
	_ = ctx.EventID    // unique ID for this emit
	_ = ctx.EventName  // registered name, or Go type

	// Standard context
	_ = ctx.Ctx // context.Context, never nil

	// Metadata: share state across listeners
	ctx.Set("user", user)
	if u, ok := ctx.Get("user"); ok { /* ... */ }

	// Cancel remaining listeners
	ctx.Cancel()

	return nil
}
```

Context is **pooled** for sync dispatch and must not be retained past the handler's return. Async listeners receive a non-pooled Context.

## Metadata

Listeners coordinate through the shared Context:

```go
events.On(OrderPlaced{}, ParseOrder).Priority(100)
events.On(OrderPlaced{}, SaveOrder).Priority(50)

func ParseOrder(ctx *events.Context, e OrderPlaced) error {
	order := parseRawOrder(e)
	ctx.Set("parsed", order)
	return nil
}

func SaveOrder(ctx *events.Context, e OrderPlaced) error {
	order, _ := events.GetMeta[*Order](ctx, "parsed")
	return db.Save(order)
}
```

`GetMeta[T]` is type-safe: it returns `(T, bool)`.

## Middleware

Wrap every dispatch:

```go
func Logger(ctx *events.Context, next events.Next) error {
	log.Printf("dispatching %s", ctx.EventName)
	err := next()
	log.Printf("finished %s: %v", ctx.EventName, err)
	return err
}

events.Default.Use(Logger)
```

Middleware runs even when an event has no listeners, which makes it suitable for tracing and observability.

## Error Handling

By default, the first error stops the dispatch and is returned:

```go
err := events.Emit(MyEvent{})
if err != nil {
	// one listener failed
}
```

Enable `ContinueOnError` to run every listener and aggregate failures:

```go
bus := events.New(events.Config{ContinueOnError: true})
// ...
err := events.EmitBus(bus, MyEvent{})
if merr, ok := err.(*events.MultiError); ok {
	for _, e := range merr.Errors {
		// handle each
	}
}
```

## Panic Recovery

Panics are recovered by default:

```go
events.On(MyEvent{}, func(*events.Context, MyEvent) error {
	panic("oops")
	return nil
})

events.Emit(MyEvent{}) // does not crash; remaining listeners still run
```

Configure the behavior:

```go
bus := events.New(events.Config{
	PanicMode: events.PanicRecoverAndContinue, // default
	OnPanic: func(pe *events.PanicError) {
		log.Printf("panic: %v\n%s", pe.Value, pe.Stack)
	},
})
```

Modes:
- `PanicRecoverAndContinue`: recover, report, continue
- `PanicRecoverAndFail`: recover, report, stop dispatch, return `*PanicError`
- `PanicPropagate`: re-panic (for tests)

## Async Dispatch

### Goroutine Mode

One goroutine per listener:

```go
bus := events.New() // default: AsyncGoroutine
events.EmitAsyncBus(bus, MyEvent{})
```

Lowest latency, no queueing, unbounded concurrency.

### Worker Pool Mode

Bounded concurrency:

```go
bus := events.New(events.Config{
	Async:      events.AsyncWorkerPool,
	Workers:    8,
	QueueSize:  512,
})
defer bus.Close()

events.EmitAsyncBus(bus, MyEvent{})
```

Caps goroutine usage. When the queue is full, `AsyncOverflow` selects the policy:
- `OverflowSpawn` (default): spawn a goroutine for the rejected task
- `OverflowDrop`: discard the task

### Wait for Completion

Block until all listeners finish:

```go
events.EmitAsyncWaitBus(bus, MyEvent{})
```

Combines parallel execution with a synchronous completion point.

## Named Events

Assign a stable name for dashboards and logs:

```go
events.Name[UserCreated](bus, "user.created")
```

The name is used by the inspector, recorder, and metrics.

## Inspection

Query the bus at runtime:

```go
info := events.Inspect[UserCreated](bus)
fmt.Printf("Event: %s\n", info.Name)
fmt.Printf("Listeners: %d\n", info.ListenerCount)
for _, l := range info.Listeners {
	fmt.Printf("  %s [priority=%d, phase=%s]\n", l.Name, l.Priority, l.Phase)
}
fmt.Printf("Dispatches: %d\n", info.Metrics.Dispatches)
```

List all events:

```go
for _, name := range bus.EventNames() {
	fmt.Println(name)
}
```

## Metrics

Per-event statistics:

```go
m := events.MetricsFor[UserCreated](bus)
fmt.Printf("Dispatches: %d\n", m.Dispatches)
fmt.Printf("Failures: %d\n", m.Failures)
fmt.Printf("Avg duration: %v\n", m.AvgDuration)
```

Aggregate across all events:

```go
total := bus.TotalMetrics()
```

Disable metrics:

```go
bus := events.New(events.Config{DisableMetrics: true})
```

## Event Recorder

Capture dispatch history:

```go
bus.EnableRecorder()

// ... emit events ...

history := bus.RecorderHistory()
for _, rec := range history {
	fmt.Printf("%s at %s: %d listeners, %v\n",
		rec.Name, rec.Time, rec.Listeners, rec.Duration)
}
```

With payload capture:

```go
bus.EnableRecorderWithPayload()
// ...
rec := bus.RecorderHistory()[0]
if ev, ok := rec.Payload.(UserCreated); ok {
	fmt.Println(ev.UserID)
}
```

The recorder is a ring buffer; old events are evicted. Configure capacity:

```go
bus := events.New(events.Config{RecorderSize: 1024})
```

## Bus Isolation

Multiple independent buses:

```go
appBus := events.New()
testBus := events.New()
```

The package-level functions (`events.Emit`, `events.On`) operate on `events.Default`.

## Framework Events

Breeze emits built-in events:

```go
// Application lifecycle
events.On(events.ApplicationStarted{}, func(ctx *events.Context, e events.ApplicationStarted) error {
	log.Println("app started")
	return nil
})

// HTTP
events.On(events.RequestFinished{}, func(ctx *events.Context, e events.RequestFinished) error {
	log.Printf("%s %s -> %d (%v)", e.Method, e.Route, e.Status, e.Duration)
	return nil
})

// OAuth2
events.On(events.TokenRefreshed{}, ...)

// WebSocket
events.On(events.ClientConnected{}, ...)

// Scheduler
events.On(events.JobFailed{}, ...)

// Plugins
events.On(events.PluginInstalled{}, ...)
```

See `framework.go` for the full list.

## Configuration

```go
bus := events.New(events.Config{
	// Stop on first error, or aggregate all?
	ContinueOnError: false,

	// Panic recovery strategy
	PanicMode: events.PanicRecoverAndContinue,
	OnPanic:   func(*events.PanicError) { /* log it */ },

	// Error callback (for async or ContinueOnError)
	OnError: func(ctx *events.Context, listener string, err error) {
		log.Printf("listener %s failed: %v", listener, err)
	},

	// Async mode
	Async:         events.AsyncWorkerPool,
	Workers:       runtime.NumCPU(),
	QueueSize:     512,
	AsyncOverflow: events.OverflowSpawn,

	// Metrics and recorder
	Metrics:      true,  // default: on
	Recorder:     false, // default: off
	RecorderSize: 256,
})
```

## Performance

Benchmarked on `11th Gen Intel Core i5-11400F @ 2.60GHz`:

| Scenario | ns/op | B/op | allocs/op |
|----------|-------|------|-----------|
| Emit (1 listener) | 598 | 152 | 3 |
| Emit (10 listeners) | 1,032 | 152 | 3 |
| Emit (100 listeners) | 6,519 | 152 | 3 |
| Emit (1000 listeners) | 50,968 | 152 | 3 |
| Emit (no listeners) | **31.8** | 0 | 0 |
| Emit parallel (1 listener) | 408 | 152 | 3 |
| Emit with priorities (100) | 7,070 | 152 | 3 |
| Emit filtered (none match) | 7,222 | 152 | 3 |
| Recorder disabled | 882 | 152 | 3 |
| Recorder enabled | 935 | 152 | 3 |
| Recorder enabled (payload) | 1,387 | 152 | 3 |
| Middleware x3 | 2,914 | 248 | 7 |
| Async goroutine (1 listener) | 2,407 | 249 | 3 |
| Async worker pool (1 listener) | 2,555 | 248 | 3 |
| Context metadata (unused) | 1,065 | 152 | 3 |
| Context metadata (used) | 2,022 | 488 | 5 |

Reproduce with `go test -run=^$ -bench=. -benchmem ./events`. Absolute numbers move with hardware and load; the ratios are the useful part.

Emitting an event nobody listens to costs **31.8 ns** with zero allocations — a map lookup and an atomic load.

Allocation is flat at **152 bytes / 3 allocs** from 1 to 1000 listeners, because the Context is pooled and listeners execute against a pre-sorted snapshot. Per-listener cost grows linearly while the allocation profile stays constant.

## Thread Safety

All operations are safe for concurrent use:
- Emitting from multiple goroutines
- Registering listeners while emitting
- Inspecting while emitting
- Middleware registration
- Recorder access

The copy-on-write snapshot ensures dispatchers never see a torn slice, and the atomic pointer to the snapshot means no locks on the read path.

## Best Practices

1. **Register at startup**: Listener registration sorts and republishes the snapshot, so do it from `init()` or `main()`, not inside a request handler.

2. **Use priorities sparingly**: Most listeners work at `PriorityNormal`. Reserve `PriorityHighest` for validation and security checks that gate later work.

3. **Prefer `Before`/`After` over extreme priorities**: Lifecycle phases are clearer than `Priority(9999)`.

4. **Filters beat no-op handlers**: A filter is evaluated before the handler runs, so a rejected event never enters the stack.

5. **Avoid heavy work in listeners**: Dispatch is synchronous by default. Offload heavy work to a queue, or use `EmitAsync`.

6. **Pool Contexts are transient**: Never retain a `*Context` past the handler's return. Copy the fields you need.

7. **Emit from the current event's context**: The `Context` can emit new events with inherited correlation IDs for tracing.

8. **Name your events**: Dashboards and logs use the registered name; the Go type name is a fallback.

9. **Enable the recorder in development**: History is invaluable for debugging. Disable it in production unless you need it.

10. **Use `events.Stop`, not sentinel errors**: Stopping propagation is normal control flow, not failure. `Stop` is consumed by the dispatcher and reported as `nil`.

## Examples

### Validation Pipeline

```go
events.On(OrderPlaced{}, ValidateOrder).Priority(events.PriorityHighest)
events.On(OrderPlaced{}, CheckInventory).Priority(events.PriorityHigh)
events.On(OrderPlaced{}, SaveOrder).Priority(events.PriorityNormal)
events.On(OrderPlaced{}, SendConfirmation).Priority(events.PriorityLow)
```

### Conditional Email

```go
events.On(UserCreated{}, SendWelcomeEmail).
	Where(func(e UserCreated) bool { return e.EmailVerified })
```

### Audit Trail

```go
events.After(OrderPlaced{}, func(ctx *events.Context, e OrderPlaced) error {
	return audit.Log("order.placed", e.OrderID, e.UserID)
})
```

### Plugin Hook

```go
// Plugins register listeners without touching framework internals
func (p *MyPlugin) Install() {
	events.On(events.RequestFinished{}, p.RecordMetrics)
}
```

### Tracing Middleware

```go
func Tracing(ctx *events.Context, next events.Next) error {
	span := tracer.Start(ctx.Ctx, ctx.EventName)
	defer span.End()
	return next()
}

events.Default.Use(Tracing)
```

## Architecture

The bus is built around three core ideas:

1. **Type-keyed registry**: Events are indexed by `reflect.Type`, so `UserCreated` and `OrderPlaced` are separate channels. This avoids runtime type assertions in the hot path.

2. **Copy-on-write snapshots**: Listeners are stored in a sorted slice behind an `atomic.Pointer`. Registration writes a new slice; dispatch reads the pointer. No locks on the read path.

3. **Pooled Contexts**: Synchronous dispatch reuses Context instances via `sync.Pool`, which keeps allocation constant regardless of listener count.

### Package Structure

```
events/
├── api.go          # package-level functions (Emit, On, etc.)
├── bus.go          # Bus type, registry, configuration
├── channel.go      # per-event listener storage + snapshot
├── config.go       # Config type + normalization
├── context.go      # Context type + pooling
├── dispatch.go     # sync/async emit implementations
├── errors.go       # Stop, MultiError, PanicError, ListenerError
├── framework.go    # built-in event types (RequestFinished, etc.)
├── inspector.go    # runtime introspection API
├── listener.go     # listener type + invocation
├── metrics.go      # per-event statistics
├── middleware.go   # middleware chain
├── pool.go         # worker pool for AsyncWorkerPool
├── recorder.go     # ring-buffer history
├── subscription.go # Subscription type + registration API
└── doc.go          # package documentation
```

## Testing

Run the full suite:

```bash
go test ./events/
```

Race detector:

```bash
go test -race ./events/
```

Benchmarks:

```bash
go test -bench=. -benchmem ./events/
```

Coverage:

```bash
go test -cover ./events/
```

The test suite covers:
- Synchronous and asynchronous dispatch
- Priorities and phases
- Filters and once-listeners
- Error aggregation and panic recovery
- Middleware and cancellation
- Metadata and context propagation
- Concurrent registration and dispatch
- Worker-pool overflow policies (block, drop, spawn)
- Recorder and metrics
- Framework events

Coverage: **97.1%** of statements, clean under `-race`.


## License

MIT

## Contributing

Breeze is an open-source project. Contributions are welcome. See [CONTRIBUTING.md](../CONTRIBUTING.md) for guidelines.

## Changelog

See [CHANGELOG.md](../CHANGELOG.md) for the version history.
