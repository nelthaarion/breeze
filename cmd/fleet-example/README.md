# `cmd/fleet-example`

## What this demonstrates

Distributed tracing across three services with no Jaeger, no Zipkin, no
OpenTelemetry Collector: a W3C `traceparent` propagated hop to hop, spans exported
to a bounded in-memory aggregator, and the result assembled into a topology graph
and merged timeline on the dashboard you already have.

```
client → gateway :3000 → auth :3001 → orders :3002
                    ↘        ↘         ↘
                     aggregator :9000  (spans + heartbeats)

gateway :2100  — MCP introspection of the running gateway, read-only
```

Specifically:

- `fleet.New(fleet.TracerConfig{...})` and `router.Use(fleet.Middleware(tracer))`
- `fleet.PropagateFromHTTP(ctx, req)` — opt-in propagation per downstream call
- `httptransport` — one of four transports, chosen by config
- `dashboard.Config.FleetAggregatorURL` — turns on Fleet View in an existing
  dashboard
- Separate **ingest** and **read** credentials, and a per-service token for log
  stitching
- `mcp` in app-runtime mode on a second port, scoped to `runtime`
- A `/healthz` route per service, deliberately excluded from tracing

## How to run it

Nothing is compiled inside the images — the build script cross-compiles the working
tree first, so a code change is one script away from running:

```powershell
powershell -File cmd/fleet-example/build.ps1   # or: pwsh -File ...
docker compose -f cmd/fleet-example/docker-compose.yml up
```

```bash
curl http://localhost:3000/api/orders/123
```

Open <http://localhost:3000/dashboard> (`admin` / `admin`) and pick **Fleet View**.

`docker compose build` will **not** recompile Go code. Re-run the build script.

## What to look for

**The gateway is where a trace is born.** It is the only service a client talks to
directly, so there is no inbound `traceparent` and `fleet.Middleware` starts a root
trace. Every hop below inherits it. That is the whole propagation story — there is
no agent and no sidecar.

**Propagation is opt-in per call.** `fleet.PropagateFromHTTP(ctx, req)` before each
downstream request. Omitting it is safe: the downstream service starts a new root
trace rather than losing the request. Implicit propagation would mean a context
leaking into a call that should not carry it.

**Two credentials, opposite directions.** `FLEET_INGEST_TOKEN` is the write
credential services use to post spans; the aggregator's Basic Auth is what a human
uses to read. `FLEET_SERVICE_TOKEN` is a third: it lets the *aggregator* fetch a
service's own logs when stitching the merged log panel. Collapsing them would mean a
service that can report spans can also read every other service's logs.

**`ORDERS_OPENAPI_URL` is a Compose DNS name, not localhost.** It has to be
resolvable *from the aggregator*, which is a different container. This is the class
of mistake that only appears once something is actually distributed.

**`depends_on: condition: service_healthy`, not merely `started`.** The first request
otherwise races container startup and produces a broken-looking trace as the
example's first impression.

**Every `/healthz` is excluded from tracing.** Probe traffic every five seconds
across four containers would otherwise dominate the trace list and bury the request
you came to look at.

**The MCP port is not the application port and not Auto-MCP.** Port 2100 serves
framework introspection — live routes, recent errors, logs, performance — of the
running gateway. It runs in app-runtime mode unconditionally, so the generating and
provisioning tools are not registered at all: there is no source tree in that
container for them to operate on, and nothing to reach even with a valid token.
`BREEZE_MCP_SCOPE=runtime` is the second layer — mode decided what the server
offers, scope decides what the credential reaches.

**All trace state is in memory.** Stopping the aggregator discards it. Fleet is a
live debugging tool, not a durable tracing warehouse, and a single aggregator process
is the v1 scaling boundary.

Next: [`../../docs/fleet-tracing.md`](../../docs/fleet-tracing.md) for the full
config reference, the four transports and the root-cause analysis, and
[`../fleet-aggregator`](../fleet-aggregator) for running the aggregator on its own.
