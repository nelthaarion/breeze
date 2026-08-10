# Breeze Workflow

Durable orchestration for the Breeze Framework: multi-step business
processes with retries, timeouts, parallelism, rollback and crash
recovery — in-process, with no broker and no required database.

```go
def := workflow.New("order-processing").
    Step("validate", ValidateOrder).
    Step("charge", ChargeCard, workflow.WithCompensation(RefundCard)).
    Step("ship", CreateShipment)

engine := workflow.NewEngine()
engine.Register(def)

res, err := engine.Run(ctx, "order-processing", order)
```

If `ship` fails, `RefundCard` runs automatically. That is the whole
idea: the failure path is declared next to the work, not scattered
through error handling.

---

## Why this exists

A step that charges a card and a step that ships a parcel are not the
same kind of failure. One must be undone, the other retried. Written by
hand, that logic becomes nested `if err != nil` blocks with rollback
code that is only exercised in production, at the worst possible moment.

This package makes the failure path declarative, testable and observable.

---

## Core concepts

### Steps and ordering

A step with no declared dependency runs after the one before it, so the
common case is sequential with no ceremony. Declaring dependencies opts
into a DAG:

```go
workflow.New("checkout").
    Step("validate", Validate).
    Step("charge", Charge,  workflow.WithDependsOn("validate")).
    Step("reserve", Reserve, workflow.WithDependsOn("validate")).
    Step("ship", Ship,       workflow.WithDependsOn("charge", "reserve"))
```

`charge` and `reserve` run concurrently; `ship` waits for both. The
graph is validated at registration: cycles, duplicate names, unknown
dependencies and nil handlers are rejected before anything runs.

### Passing data between steps

The payload is typed, and metadata is shared for everything computed
along the way:

```go
func Charge(ctx *workflow.Context) error {
    order, ok := workflow.Payload[Order](ctx)
    if !ok {
        return workflow.NonRetryable(errors.New("bad payload"))
    }
    id, err := billing.Charge(ctx.Ctx, order.Total)
    if err != nil {
        return err
    }
    ctx.Set("charge_id", id) // visible to every later step
    return nil
}
```

### Retries

```go
workflow.WithRetry(workflow.RetryPolicy{
    MaxAttempts:  5,
    Backoff:      workflow.BackoffExponential,
    InitialDelay: 500 * time.Millisecond,
    MaxDelay:     30 * time.Second,
    Jitter:       0.2,
})
```

Jitter is not decoration. When a dependency fails, every in-flight
execution fails at nearly the same instant; without jitter they all
retry at the same instant too, reproducing the spike that caused the
outage. Retries stop early for errors wrapped in `NonRetryable`, which
preserves the original error's identity:

```go
err := workflow.NonRetryable(ErrCardDeclined)
errors.Is(err, ErrCardDeclined)          // true
errors.Is(err, workflow.ErrNonRetryable) // true
```

### Compensation (Saga)

Rollback runs in reverse order over the steps that actually succeeded:

```go
Step("reserve", Reserve, workflow.WithCompensation(Release)).
Step("charge",  Charge,  workflow.WithCompensation(Refund)).
Step("ship",    Ship)
```

`ship` fails → `Refund`, then `Release`. A compensation handler that
itself fails does not stop the others: the remaining side effects still
need undoing, and stopping would leave strictly more damage behind. It
emits `WorkflowCompensationFailed`, which is a page-someone event.

### Timeouts and cancellation

`WithTimeout` bounds one attempt; `Definition.Timeout` bounds the whole
execution. Both cancel through `ctx.Ctx`, so a step that respects its
context stops promptly. Retry backoff is cancellable too — shutdown
never waits out a 30-second delay.

### Event triggers

```go
def := workflow.New("welcome").Step("email", SendWelcomeEmail)
workflow.OnType[UserRegistered](def)
engine.Register(def)
```

Emitting `UserRegistered` now starts the workflow, asynchronously, with
the event as the payload. The emitter never blocks on it.

### Durability

```go
engine := workflow.NewEngine(workflow.Config{Store: myStore})
n, err := engine.Resume(ctx) // continue what a crash interrupted
```

`Resume` replays interrupted executions and **does not re-run steps
that already completed**. `Store` is a nine-method interface with no
driver or ORM behind it; `MemoryStore` is the default.

### Idempotency

```go
engine.Run(ctx, "order-processing", order,
    workflow.WithIdempotencyKey(order.ID))
```

The second call with the same key returns the first execution's outcome
instead of running again — which is what makes at-least-once event
delivery safe.

---

## Performance

`go test -bench=Run`, Intel i5-11400F, Go 1.25, `MemoryStore`,
observability disabled:

| Benchmark | ns/op | B/op | allocs/op |
|---|---|---|---|
| 1 step | 5,477 | 1,752 | 24 |
| 10 steps | 41,992 | 10,846 | 89 |
| 50 steps | 164,597 | 46,661 | 337 |
| 10 parallel steps | 35,636 | 11,188 | 86 |
| concurrent (4 cores) | 4,475 | 2,491 | 32 |

Roughly 3–4 µs per step, dominated by persistence calls and event
publication rather than orchestration. Throughput improves under
concurrency because executions overlap on the bounded worker pool.
For context, a step that does anything real — a query, an HTTP call —
costs hundreds of microseconds to milliseconds, so orchestration
overhead is not the thing to optimise.

Design choices behind those numbers:

- The DAG is topologically sorted **once**, at registration. Dispatch
  walks a precomputed plan.
- Definitions are immutable once registered, so execution reads them
  without a lock.
- No lock is ever held while user code runs.
- `MaxWorkers` bounds step concurrency across all executions, so a
  fan-out cannot spawn unbounded goroutines.

---

## Observability

Every execution publishes framework events (`workflow.started`,
`workflow.step.failed`, `workflow.compensation.started`, …) and one
observability signal carrying each step as a span. The dashboard renders
workflows through the same pipeline as HTTP requests and event
dispatches, with no workflow-specific code.

```go
events.On(events.WorkflowFailed{}, func(_ *events.Context, e events.WorkflowFailed) error {
    alert.Page("workflow %s failed at step %s: %s", e.Workflow, e.Step, e.Err)
    return nil
})
```

---

## Best practices

**Make steps idempotent.** A step can run twice: retries, resumes and
at-least-once triggers all cause it. Key external calls on
`ctx.ExecutionID()` plus the step name.

**Mark permanent failures.** A declined card will be declined five
times. `NonRetryable` turns five doomed attempts into one.

**Compensate anything with an external side effect.** If a step charges
money, sends mail or provisions a resource, give it a compensation
handler. If it only writes to your own database inside a transaction,
you may not need one.

**Respect the context.** A step that ignores `ctx.Ctx` cannot be
cancelled or timed out; the engine can only stop waiting for it.

**Keep payloads small.** The payload is carried for the whole
execution and persisted. Pass an ID, not a 2 MB struct.

---

## Error reference

| Error | Meaning |
|---|---|
| `ErrWorkflowNotFound` | No workflow registered under that name |
| `ErrDuplicateWorkflow` | Name already registered |
| `ErrInvalidWorkflow` | Definition rejected by validation |
| `ErrWorkflowCycle` | Dependencies form a cycle |
| `ErrDuplicateStep` / `ErrUnknownDependency` | Bad step graph |
| `ErrStepTimeout` / `ErrWorkflowTimeout` | Deadline exceeded |
| `ErrWorkflowCancelled` | Context cancelled |
| `ErrStepPanicked` | Step panicked; recovered and converted |
| `ErrNonRetryable` | Failure marked final |
| `ErrEngineClosed` | Engine is shut down |
| `ErrPersistenceFailure` | Store returned an error |

Failures are wrapped in `*StepError` (workflow, step, attempt) and
validation problems in `*ValidationError`, both reachable with
`errors.As`.

---

## Testing workflows

Steps are plain functions — test them directly. For the orchestration
itself, run the engine with a `MemoryStore` and assert on `Result`:

```go
res, err := engine.Run(context.Background(), "order-processing", order)
if res.State != workflow.StateCompensated {
    t.Fatalf("state = %v", res.State)
}
```

`Result.Steps` reports each step's state, attempt count, duration and
whether it was skipped.
