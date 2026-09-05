# `cmd/events-example`

## What this demonstrates

The typed event bus with no HTTP anywhere: declaring an event as a struct,
registering two listeners for it, emitting once, and both running.

It is deliberately not a server. `events` is not an HTTP feature — `workflow` and
`observability` both use it without importing `breeze` — and an example that
started a listener on port 3000 would suggest otherwise.

Specifically:

- `events.On(UserRegistered{}, fn)` — registration keyed by type, with the sample
  value used only for inference and never read
- Two listeners for one event, both invoked in registration order
- `events.Emit(UserRegistered{...})` — one call, no topic string, no `interface{}`
- The global bus, which is what an application that never constructs one gets

## How to run it

```bash
cd cmd/events-example
go run .
```

Output:

```
🚀 Breeze Event Demo
Creating user...
📧 Sending welcome email: test@example.com
📝 Audit: 1
Request finished
```

## What to look for

**No topic strings.** `Emit(UserRegistered{...})` finds its listeners by the Go
type. A renamed event is a compile error at every registration and every emit, not
a listener that silently stops firing — which is the failure mode of every
string-keyed bus.

**The event type is a plain struct** in `events.go` with no interface to satisfy, no
embedded base type and no registration call. That is what makes the bus usable for
types you did not write.

**`Emit` returns an error**, and this example ignores it. That is fine here because
neither listener can fail; in real code the return is how a listener's failure
reaches the caller, and `events.Stop` is how a listener halts the chain
deliberately.

**The `time.Sleep`** is what makes async output visible. In a real application the
process outlives the dispatch; here it would not, and an example whose output
depended on scheduling luck would teach the wrong thing.

Next: [`../../events/README.md`](../../events/README.md) for priorities, filters,
dispatch middleware, once-listeners, isolated buses (`OnBus`/`EmitBus`) and the
recorder.
