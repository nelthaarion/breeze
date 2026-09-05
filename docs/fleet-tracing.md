# Breeze Fleet Tracing

Fleet Tracing adds bounded, live distributed observability to Breeze services
without requiring Jaeger, Zipkin, Tempo, or an OpenTelemetry Collector. Services
propagate the standard W3C `traceparent` header, export spans asynchronously, and
the Fleet Aggregator assembles them into a topology and merged timelines consumed
by the existing dashboard.

> **Retention is intentionally in memory.** Fleet is a live debugging tool, not a
> durable tracing warehouse. Traces disappear when their TTL expires, their ring
> buffer is overwritten, or the aggregator restarts. A single aggregator process
> is the v1 scaling boundary.

## Quick start

```go
tracer := fleet.New(fleet.TracerConfig{Enabled: true, ServiceName: "orders", AggregatorURL: "http://fleet:9000/fleet"})
router.Use(fleet.Middleware(tracer)) // before dashboard middleware
defer tracer.Close(context.Background())
```

Propagate explicitly on every downstream call:

```go
req, _ := http.NewRequest(http.MethodPost, ordersURL, body)
fleet.PropagateFromHTTP(ctx, req)
resp, err := http.DefaultClient.Do(req)
```

This is opt-in per call by design. If propagation is omitted, the downstream
service safely starts a new root trace.

Run the aggregator:

```bash
go run ./cmd/fleet-aggregator
```

Then activate Fleet View in an existing dashboard:

```go
cfg := dashboard.DefaultConfig()
cfg.FleetAggregatorURL = "http://127.0.0.1:9000/fleet"
dashboard.Install(app, router, cfg)
```

## Migration: single-service dashboard to Fleet

The application-side change is deliberately three lines:

```diff
+ tracer := fleet.New(fleet.TracerConfig{Enabled: true, ServiceName: "orders", AggregatorURL: os.Getenv("FLEET_URL")})
+ router.Use(fleet.Middleware(tracer))
  router.Use(coll.Middleware())
+ defer tracer.Close(context.Background())
```

Set `dashboard.Config.FleetAggregatorURL` to expose the 15th dashboard page. When
it is empty the capability and navigation entry are absent, preserving the old
dashboard exactly.

## Features

- W3C Trace Context parsing and propagation; malformed headers safely become new
  roots and increment a counter.
- Trace-wide fixed-rate sampling plus lightweight always-on error spans.
- Custom `fleet.Tag(ctx, key, value)` attributes propagated as bounded baggage.
- Lock-bounded local buffering and asynchronous batch export with capped retry.
- Bounded aggregator storage, service heartbeats, trace assembly, orphan spans,
  skew flags, topology percentiles, deterministic root cause, and blast radius.
- Asynchronous contract checks against heartbeat-advertised OpenAPI schemas.
- A dashboard Fleet page with topology, trace filters, merged waterfalls,
  violations, catalog, and incident state.
- Trace-correlated log stitching: every log line emitted inside a request is
  stamped with its trace id automatically, and the merged view fans out to each
  service's own log store rather than duplicating log storage in the aggregator.

### Reading the topology graph

The graph is a mesh, not a tree. Every observed caller-to-callee pair is drawn as
**two** arcs — the request leaving the caller and the response coming back —
bowed to opposite sides so they never overlap. One arrow per pair would tell you
that A calls B but never whether B answered, which is precisely the thing you
need to know during an incident.

Each arc carries its timing inline:

- With no trace open, edges show the aggregate over the retention window
  (`p50 5.0ms · p95 9.0ms`) computed by `GET /fleet/api/topology`.
- With a trace open, they switch to *that trace's* measured hop durations, so the
  numbers on screen are the request you are actually looking at. A hop called
  more than once in one trace shows its total and a `×n` call count.

Timings are printed on the graph rather than hidden behind a hover, so the graph
still carries its data in a screenshot pasted into an incident channel.

While stepping through a trace with the playback controls, arc styling tracks
where the cursor is: the request leg lights up while a hop is in flight, and the
response leg lights up once the cursor passes the hop's last span — red if that
hop's worst status was a 5xx. The user's own call and its answer are drawn too,
so the request visibly enters and the response visibly leaves.

## Configuration


### Tracer

| Field | Default | Meaning |
|---|---:|---|
| `ServiceName` | required | Stable service name shown in traces. |
| `AggregatorURL` | — | Aggregator base URL. Empty disables export. |
| `Transport` | HTTP baseline | Export/propagation transport. Events is available for in-process use. |
| `FlushInterval` | `1s` | Maximum batch age. |
| `MaxBatchSize` | `200` | Span export batch size. |
| `MaxBufferSpans` | `4096` | Drop-oldest local ring capacity. |
| `SampleRate` | `1.0` | Root sampling probability. |
| `ExportTimeout` | `2s` | Export timeout. |
| `IngestToken` | empty | Value sent as `X-Fleet-Token`. |
| `Enabled` | false unless set | False creates the no-op fast path. |

### Aggregator

| Field | Default | Meaning |
|---|---:|---|
| `BasePath` | `/fleet` | API and WebSocket prefix. |
| `MaxTraces` | `2000` | Trace ring capacity. |
| `MaxSpansPerTrace` | `512` | Per-trace drop-oldest cap. |
| `TraceTTL` | `5m` | Idle trace lifetime. |
| `ServiceTTL` | `15s` | Heartbeat deadline before a service is down. |
| `ContractValidation` | true | Enables asynchronous schema checks. |
| `MaxViolations` | `1000` | Violation ring capacity. |
| `Username`/`Password` | empty | Viewer/read Basic Auth. |
| `IngestToken` | empty | Separate service write credential. |

## Transport status

- **HTTP:** complete correctness baseline, JSON/gzip over the repository's native
  Breeze client.
- **Events:** complete in-process mode using `events.Bus`; network export safely
  falls back to the HTTP baseline when no local bus is supplied.
- **gnet:** uses the same interoperable HTTP/1.1 format through Breeze's native
  client package and has an isolated configuration name.
- **WebSocket:** propagation-compatible adapter currently exports via the HTTP
  fallback; a dedicated persistent export connection is planned.
- **gRPC:** planned and intentionally not advertised as implemented. It will live
  in an isolated submodule so the base Breeze module never pulls gRPC transitively.

### Broker backends for networked events

Networked event delivery is expressed through the generic `events.Backend` seam
(`Publish`/`Subscribe`), so the broker is a pluggable detail rather than a fork of
the transport:

- **memory (default):** no external infrastructure. Networked export uses the HTTP
  baseline, keeping zero-dependency deployments working unchanged.
- **Kafka:** shipped, and the durable/replayable option for high sustained
  throughput. It lives in its own nested Go module at
  `fleet/transport/eventtransport/backends/kafka` with a separate `go.mod`, so the
  `kafka-go` client is only compiled by applications that explicitly opt in:

  ```go
  backend, err := kafka.New(kafka.Config{
      Brokers: []string{"kafka-1:9092", "kafka-2:9092"},
      GroupID: "fleet-aggregator",
  })
  ```

- **NATS / RabbitMQ:** not yet implemented. Because each is just another
  `events.Backend`, adding one requires no change to the tracer, the aggregator, or
  any application call site.

The base module's `go.mod` and `go.sum` are unchanged by this feature: no broker,
WebSocket, or gRPC dependency is reachable from
`go get github.com/nelthaarion/breeze/v2`. Verify with `go list -deps ./...`, which
resolves no `kafka-go`, `nats`, `amqp091`, or `grpc` package.

## Security

Use both credential classes in production:

1. `IngestToken` protects writes to span/heartbeat ingestion and is compared in
   constant time.
2. `Username` + `Password` protect human read APIs. Both must be non-empty for
   Basic Auth to be enforced, matching the dashboard convention.

The aggregator logs a warning when credentials are absent. Keep it on a private
network, terminate TLS at the normal service edge, and never expose an unsecured
aggregator publicly. Spans contain header names, never header values. Error text
and captured JSON payloads are scrubbed at the originating service before export.

Dashboard proxy credentials remain server-side through
`FleetAggregatorUsername`/`FleetAggregatorPassword`; the browser talks only to its
normal dashboard origin. If the aggregator is unreachable, Fleet shows a clear
degraded state and the existing dashboard APIs remain independent.

## Architecture

| Path | Responsibility |
|---|---|
| `fleet/traceparent.go` | Allocation-free W3C parsing and baggage encoding. |
| `fleet/middleware.go` | Incoming extraction, sampling, span completion. |
| `fleet/tracer.go` | Local ring, background flush, heartbeats, retry. |
| `fleet/transport/` | HTTP/events and isolated compatibility adapters. |
| `fleet/aggregator/store.go` | Bounded sharded trace storage and TTL eviction. |
| `fleet/aggregator/assemble.go` | Trees, orphans, skew, root-cause summary. |
| `fleet/aggregator/topology.go` | Incremental graph statistics/blast radius. |
| `fleet/contracts/` | OpenAPI cache and lightweight async validation. |
| `dashboard/templates/views/fleet.html` | Capability-gated Fleet View. |

## Why this is different from Jaeger, Zipkin, or Datadog

Fleet's first differentiator is **live contract validation against the schema that
the running Breeze service itself generates**. A generic tracing backend sees
spans but has no framework-owned, always-current OpenAPI model tied to each route.
Fleet can therefore report a real request missing a required field or a response
returning the wrong type without a separately maintained Pact fixture.

The second is **deterministic root-cause and blast-radius highlighting**. Fleet
walks the causal span tree to mark one earliest failure, labels later failures as
derived effects, and traverses its incrementally maintained service graph to show
which observed dependencies are impacted. This is graph math, not probabilistic AI,
so it is explainable and available offline.

## Performance and limits

Benchmarks live in `fleet/bench_test.go` and `fleet/aggregator/bench_test.go`.
Disabled `RecordSpan` and well-formed traceparent parsing are expected to remain at
zero allocations. Export never blocks a handler: overload drops the oldest local
span instead of growing memory or waiting on the network.

Known v1 limits: no durable storage, no multi-aggregator coordination, no OTLP,
and no completed gRPC transport. These are deliberate boundaries, not hidden
failure modes.

## Running the example

The complete three-service example is in `cmd/fleet-example`. The Compose file
there uses runtime-only images: `cmd/fleet-example/build.ps1` cross-compiles the
working tree on the host and `Dockerfile.prebuilt` copies the binaries in. Nothing
is compiled inside a container, so what runs is the code currently checked out
rather than whatever the module proxy serves for this import path — which matters
while `fleet/` is unreleased. Each container is gated on the previous one reporting
healthy, so the first request cannot race startup:

```bash
powershell -File cmd/fleet-example/build.ps1     # or: pwsh -File ...
docker compose -f cmd/fleet-example/docker-compose.yml up --build
curl http://localhost:3000/api/orders/123
```

Re-run the build script after changing Go code; `docker compose build` alone will
not recompile it.

The gateway additionally serves an in-process MCP control endpoint on `:2100` —
app-runtime mode, `runtime`-scoped token, `fleet-demo-mcp-token`:

```bash
curl -H "Authorization: Bearer fleet-demo-mcp-token" http://127.0.0.1:2100/mcp/features
```

Both permission layers are visible in that answer. App-runtime mode removed the
generating and provisioning tools from the registry — there is no source tree in the
container for them to act on — and the `runtime` scope then withheld the fleet tools
from this particular token, so `breeze_get_trace` returns a structured refusal naming
the `fleet` capability. `BREEZE_MCP_SCOPE` widens it; removing the line issues an
unscoped token; unsetting `BREEZE_MCP_PORT` disables the endpoint entirely.

Every service exposes `/healthz` for those probes and deliberately excludes it
from tracing, along with the orders service's `/openapi.json` and `/chaos/*`
routes. This is worth understanding before instrumenting your own app, because
the reason is not cosmetic: the aggregator fetches `/openapi.json` itself, and
that fetch carries no inbound `traceparent`, so tracing it would record the
orders service — the deepest hop in the fleet — as the *root* of a new trace and
render it as an entry point in the topology graph beside the gateway. Probe
traffic would likewise bury real requests in the trace list. Each service wraps
`fleet.Middleware` in a small `skipUntraced` predicate to exclude those paths;
copy that pattern for your own health and introspection endpoints.

Open `http://localhost:3000/dashboard` with `admin/admin`, then select Fleet
View. The gateway tags the request with `order_id=123`; the tag is propagated
through auth and orders. The orders response includes an intentionally additive
`debug_note` field for the contract-validation demonstration. To exercise
deterministic root-cause and blast-radius highlighting, run:

```bash
curl -X POST http://localhost:3002/chaos/fail
curl http://localhost:3000/api/orders/123
curl -X POST http://localhost:3002/chaos/recover
```

All three services install a dashboard and share one `FLEET_SERVICE_TOKEN`. Only
the gateway's is published, since it is the one a browser opens, but auth and
orders need theirs too: the aggregator stitches a trace's merged log panel by
fetching each involved service's own logs, so a service without a dashboard
contributes no log lines to that trace.

The example uses HTTP transport as the interoperability baseline. Replace each
service's transport construction with another available adapter when comparing
transport behavior. The aggregator and all trace state are memory-only; stopping
the aggregator discards retained traces.

