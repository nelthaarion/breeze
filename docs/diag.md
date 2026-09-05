# Diagnostics

One endpoint that answers "what is every subsystem of this process actually doing
right now?"

```bash
curl localhost:3000/dashboard/api/diagnostics
curl localhost:3000/dashboard/api/diagnostics?subsystem=events
```

```json
{
  "total": 14,
  "subsystems": [
    {
      "subsystem": "events",
      "status": "ok",
      "summary": "3 event types, 7 listeners, 1204 dispatches, avg 0.12 ms",
      "detail": {"event_types": 3, "listeners": 7, "avg_ms": 0.12}
    },
    {
      "subsystem": "compression",
      "status": "off",
      "summary": "not installed",
      "detail": {"reason": "call router.Use(middleware.CompressionMiddleware())"}
    }
  ]
}
```

## Why the package exists

Breeze has a dozen subsystems that each know something an operator or an agent
needs — the event bus knows its listener counts, the fleet tracer knows how many
spans it failed to export, the template engine knows whether it is re-parsing on
every render. Before `diag`, each of those facts was reachable only by holding a
typed handle to that subsystem, which means a diagnostic tool had to be handed
every handle the application constructed. Nothing had the whole picture, so
nothing could report it.

### Why it is a leaf package

`diag` imports nothing but the standard library, and that is a requirement rather
than a virtue. The import graph is:

```
breeze      → binding, rpc, scalar, internal/mcp
dashboard   → breeze, events, observability, video
fleet       → dashboard
workflow    → events, observability          (not breeze)
```

A registry in the root `breeze` package could not be used by `events`, `workflow`,
`observability` or `scalar`, because `breeze` imports `scalar` and those packages
sit deliberately below it. A registry in `dashboard` could not be used by anything,
since almost everything is below `dashboard`. So the registry is a leaf: importable
from every layer including the lowest.

This is also why the shared formatting helpers ([`HumanBytes`](../diag/format.go),
`Milliseconds`) live here. Three packages had independently grown a private
`humanBytes`, each with a comment explaining it could not be shared — and each was
right about the import and wrong about the conclusion. Two of the three printed
`KB/MB/GB` while dividing by 1024, and all three fed the same dashboard page.

## Zero cost

Nothing here runs on a request path. Registration happens once while an application
is being wired and costs one slice append. Reading happens only when someone asks,
and that is the only time a probe function is invoked.

The registry is copy-on-write behind an atomic pointer, so `Snapshot` is a single
atomic load with no lock. That is not chosen for throughput — registration is rare
and reads are rarer — it is chosen so that reading diagnostics can never contend
with anything, including another reader, on a process that is already in trouble.


## The four statuses

| Status | Means | A reader should |
|---|---|---|
| `ok` | Running and healthy. | Nothing. |
| `degraded` | Running and unhappy. | Read the summary — it names what is wrong. |
| `off` | Not installed, or installed and deliberately disabled. | Nothing. **Not a fault.** |
| `unknown` | Registered but unable to answer; a probe that panicked. | Suspect the probe, not the subsystem. |

`off` is separate from `degraded` because a dashboard that rendered "compression is
not installed" in red would train its reader to ignore red. `unknown` is separate
because a broken probe says nothing about the health of what it was probing.

An empty status is normalised to `unknown` rather than trusted, so a probe that
forgets to set one cannot be misread as healthy.

## The registry keys

Fourteen subsystems register, and the key is the same string the feature is called
on the command line wherever one exists — an agent that read `breeze add ratelimit`
can ask for `?subsystem=ratelimit` without a translation table.

| Key | Registered by |
|---|---|
| `router`, `workerpool`, `templates`, `i18n`, `websocket`, `auto-mcp`, `static` | [`../diag.go`](../diag.go) — the framework core |
| `events` | `events.New` |
| `workflow` | `workflow.New` |
| `observability` | the `observability` collector |
| `dashboard` | `dashboard.Install` |
| `fleet` | `fleet.New` |
| `video`, `video:<prefix>` | each `video` mount |
| `docs` | `scalar` — "docs", matching the feature name |
| `jsonrpc` | `rpc.Server` |
| `migrate` | the migration runner |
| `mcp` | `mcp.StartInProcess` — the embedded endpoint |
| `oauth2` | `middlewares/oauth2` |
| `compression`, `etag`, `ratelimit`, `cors`, `security`, `jwt`, `locale`, `logging`, `recovery` | each middleware constructor |

`video` registers twice — once under the shared key and once under
`video:<prefix>` — because a multi-mount process needs the per-mount answer and a
single-mount process should not have to know that.

`mcp` and `auto-mcp` are the two MCP endpoints, and they answer different questions:
`auto-mcp` is the application's own tagged routes exposed as tools, `mcp` is the
embedded read-only introspection endpoint. Neither is a `breeze add` feature, which
is why they appear in `internal/mcp/diag_completeness_test.go`'s list of framework
subsystems that exist without being added.

### The endpoint that reports on itself

The `mcp` probe is the one an agent needs most and could not previously get, because
`breeze_diagnose_service` reads this registry — so the endpoint serving the call was
the one subsystem missing from its own report.

Three states it surfaces, none of which produces an error anywhere else:

- **A scope withheld a tool.** A client calling one gets the same "no such tool" as
  for a tool that does not exist. `reachable_tools` versus `tools`, plus
  `withheld_by_scope`, is the only place the distinction is visible — mode decides
  what is registered, scope decides what the credential reaches.
- **Generator mode in a container with no source tree.** It registers the full
  toolchain and every mutating tool fails at its first file operation. Nothing at
  startup knows whether the source is there, so nothing warns.
- **`AllowWorkspaceTools` left on in production.** The process will `chdir` into and
  rewrite its own tree while serving requests.


## Writing a probe

Three constructors, and a `Report` should be built with one of them so that "off"
means the same thing in every subsystem's answer:

```go
func (t *Tracer) probe() diag.Report {
    if !t.cfg.Enabled {
        return diag.Off("tracing is disabled; set TracerConfig.Enabled")
    }

    exported, failed := t.exported.Load(), t.failed.Load()
    detail := map[string]any{
        "service":  t.cfg.ServiceName,
        "exported": exported,
        "failures": failed,
    }

    if failed > 0 {
        return diag.Degraded(
            fmt.Sprintf("%d spans exported, %d export failures", exported, failed),
            detail,
        ).WithNotes("the aggregator was unreachable at " + t.cfg.AggregatorURL)
    }
    return diag.OK(fmt.Sprintf("%d spans exported", exported), detail)
}

func (t *Tracer) registerDiagnostics() {
    diag.Register("fleet", t.probe)
}
```

Rules the existing probes hold to:

- **The summary names numbers, not the status.** `"412 spans exported, 3 export
  failures"` beats `"tracer is degraded"` — the second tells a reader only that
  they now have to go and look somewhere else.
- **`Off` names the call that would turn it on.** `"no event bus is attached; call
  coll.AttachEvents(events.Default)"` is useful; `"events are off"` is not.
- **Notes record what the report could not determine.** A reader who sees no note
  is entitled to treat the numbers as complete.
- **`Detail` holds JSON-encodable scalars, slices and maps only.** A probe
  returning a live handle would leak it through the endpoint.
- **Answer from state you already hold.** A probe must be cheap and non-blocking;
  it should not dial, read a file, or take a lock that a request path uses.

`Register` replaces rather than appends, so an application that installs the
dashboard twice — or a test that builds three buses — has one report and not three.
Last registration wins, matching the wiring order. Registering a nil probe
unregisters, so a subsystem that is torn down stops claiming to exist.

`Snapshot` is sorted by subsystem name, which is what makes two reads of an
unchanged process comparable.

### A panicking probe is contained

`Snapshot` recovers each probe individually and reports it as `unknown` with the
panic value in a note. One broken probe cannot hide the other thirteen, which is
the whole reason this endpoint is worth trusting during an incident.

The panic value is rendered with `%v` rather than `%#v`: a panic value is usually
an error or a string, and the Go-syntax representation of a large struct would bury
the message in a field dump.

## Counted diagnostics

Most subsystems can be diagnosed for free because they already hold the answer:
the event bus already counts dispatches, the fleet tracer already counts export
failures, the template engine already knows its cache size. A probe over those
costs nothing because it only reads what exists.

A few hold nothing. Compression does not know how many responses it compressed;
the ETag middleware does not know its own hit rate; the rate limiter knows its
client map but not how many requests it rejected. Those facts are exactly the ones
an operator asks for, and none can be recovered after the fact — they have to be
counted as they happen, on the response path.

### Why there is a gate

A counter on a response path is not free. One atomic increment is a handful of
nanoseconds in isolation, but a *shared* counter incremented by every core moves a
cache line between cores on every request, and under real concurrency that
coherence traffic is the cost, not the instruction.

So counting is off by default and the hot path reads a gate first:

```go
if !counting.Load() { return }
```

That load reads a global written approximately never. It sits in every core's cache
in shared state, costs no coherence traffic, and the branch predicts perfectly — as
close to zero as a runtime check gets, and unlike the increment it protects, it does
not get worse as cores are added.

### Turning it on

```go
diag.EnableCounters()   // idempotent, safe at any time, no restart needed
diag.DisableCounters()  // keeps what was counted
since, on := diag.CountersSince()
```

`dashboard.Install` and `mcp.ServeInProcess` both call `EnableCounters` — their
presence already means the process has accepted observability cost. A bare
application that installed neither is not paying for counters it has nothing to
read them with.

Enabling does not retroactively invent numbers, and disabling does not discard
them: a caller who disabled counting for a benchmark would otherwise also destroy
the evidence it had collected, and re-enabling would silently restart from zero with
nothing saying so.

`CountersSince` exists because a caller rendering a rate needs the window, and
"since process start" is the one answer it must never give — counting can be
enabled at any moment, and a rate against the wrong denominator is worse than no
rate.

### Every counted number says whether it was counted

`CounterSnapshot.Counting` travels with the numbers deliberately. Zeroes with
`counting: false` mean "not measured", and a reader who cannot tell that apart from
"did not happen" will draw the wrong conclusion from a healthy service.

## See also

- [`../diag/`](../diag/) — the package: `diag.go`, `report.go`, `counter.go`, `counters.go`, `format.go`
- [`../dashboard/README.md`](../dashboard/README.md) — the page that renders this
- [`middlewares.md`](./middlewares.md) — the nine middleware probes
- [`mcp-walkthrough.md`](./mcp-walkthrough.md) — the MCP tool that reads diagnostics

