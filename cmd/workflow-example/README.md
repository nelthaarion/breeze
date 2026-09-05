# `cmd/workflow-example`

## What this demonstrates

The workflow engine with live dashboard visualisation: three workflows that fail on
purpose, so retry and compensation are visible rather than described.

Specifically:

- `workflow.New("name").Step(...).Step(...)` — a DAG built by chaining
- `workflow.WithRetry(workflow.RetryPolicy{...})` on a step that fails twice
- A compensating workflow, where a later failure rolls back earlier steps
- `coll.AttachEvents(events.Default)` — the one line that makes executions appear
  live on the dashboard
- `engine.Run(ctx, name, payload)` from an HTTP handler

## How to run it

```bash
go run ./cmd/workflow-example
```

Open <http://localhost:3000/dashboard> (`admin` / `admin`) and go to the **Events**
page. Then trigger each workflow and watch it draw:

```bash
curl -X POST localhost:3000/demo/workflow              # happy path
curl -X POST localhost:3000/demo/workflow/retry        # fails twice, then succeeds
curl -X POST localhost:3000/demo/workflow/compensation # fails late, rolls back
curl -X POST localhost:3000/demo/workflow/event        # a bare event, no workflow
curl localhost:3000/demo/workflows                     # the registered definitions
```

## What to look for

**`coll.AttachEvents(events.Default)` is the whole integration.** The engine
publishes events; the dashboard subscribes. Neither imports the other's
visualisation code, and a workflow run with no dashboard installed costs the same as
one with. `AttachEvents` returns a detach function — the example defers it, which is
what a test or a short-lived process should do.

**The retry workflow fails twice deliberately.** `WithRetry` bounds the attempts and
the backoff, and the Events page shows each attempt as a separate span rather than
one long step. That distinction is the reason retry is engine-level and not a loop
inside the step: a loop would report one step that took a while.

**The compensation workflow rolls backwards.** When a late step fails, the engine
runs the compensations of the steps that already succeeded, in reverse order. Watch
the page redraw right to left.

**All the sleeps are simulated work.** Every step in this example sleeps, and the
package comment says so twice. They exist only so the dashboard has something long
enough to look at — real steps would call a payment gateway or a database. Nothing
in the engine requires or assumes a delay, and copying a `time.Sleep` into your own
step is copying scaffolding.

**Executions survive the handler.** `engine.Run` is called synchronously here so the
HTTP response can report the result, but the state is in the store — a `MemoryStore`
by default, which is why restarting the process loses the history.

Next: [`../../workflow/README.md`](../../workflow/README.md) for durable stores, DAG
dependencies and resume, and [`../../docs/diag.md`](../../docs/diag.md) for the
`workflow` probe.
