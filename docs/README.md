# Breeze documentation

Every subsystem this framework has, and where to read about it. If something is
not in this table, it is not documented — that is the point of the table.

The root [`README.md`](../README.md) is the tour: what Breeze is, how to install
it, and a section per feature. This index is the reference: one row per
subsystem, pointing at the document that covers it in full.

## Core

| Subsystem | Doc | What it covers |
|---|---|---|
| HTTP core | [`../README.md`](../README.md) | Router, `Context`, handlers, inline execution, zero-copy headers, WebSocket engine, worker pool |
| Repository conventions | [`repository-structure.md`](./repository-structure.md) | Where code goes, naming, error handling, example shape, linting |
| Request binding | [`binding.md`](./binding.md) | `binding.Bind`, the four sources, validation rules, RFC 9457 error bodies |
| Middleware | [`middlewares.md`](./middlewares.md) | All 12 built-in middlewares, ordering rules, and why the order matters |
| Errors | [`../README.md#-errors-as-return-values`](../README.md#-errors-as-return-values) | `breeze.Error`, RFC 9457 problem details, handler error returns |

## Subsystems

| Subsystem | Doc | What it covers |
|---|---|---|
| Events | [`../events/README.md`](../events/README.md) | Typed bus, priorities, filters, middleware, async dispatch, recorder, metrics |
| Workflows | [`../workflow/README.md`](../workflow/README.md) | Durable steps, DAG dependencies, retries, compensation, resume |
| Dashboard | [`../dashboard/README.md`](../dashboard/README.md) | 13 pages, live WebSocket updates, timeline profiler, DBWriter, auth |
| Observability | [`../observability/README.md`](../observability/README.md) | Signal model, collector, querying, live streaming, payload masking |
| Diagnostics | [`diag.md`](./diag.md) | The `diag` registry, writing a probe, `GET /dashboard/api/diagnostics` |
| Fleet tracing | [`fleet-tracing.md`](./fleet-tracing.md) | Distributed tracing, trace context propagation, transports, aggregator, root cause |
| Video streaming | [`../video/README.md`](../video/README.md) | Range requests, signed URLs, caching, memory bounds |
| OAuth2 / social login | [`../middlewares/oauth2/README.md`](../middlewares/oauth2/README.md) | Google/GitHub/Microsoft/Discord, PKCE, sessions, refresh |
| JSON-RPC 2.0 | [`rpc.md`](./rpc.md) | Server on its own port, batching, notifications, blocking methods |
| OpenAPI / Scalar | [`scalar.md`](./scalar.md) | Spec generation from route tags, the Scalar UI, `llms.txt` |
| Migrations | [`migrate.md`](./migrate.md) | Migration pairs, the runner, `breeze migrate`, `breeze makemigration` |
| HTTP client | [`client.md`](./client.md) | The gnet-backed outbound client, connection reuse, timeouts |
| MCP | [`mcp-walkthrough.md`](./mcp-walkthrough.md) | Modes, scopes, workspaces, stdio vs network, in-process, provisioning |

## Tooling

| Topic | Doc |
|---|---|
| CLI: `generate`, `add`, `routes`, `migrate` | [`../README.md#-cli--scaffolding--code-generation`](../README.md#-cli--scaffolding--code-generation) |
| MCP client configuration example | [`breeze-mcp-config-example.json`](./breeze-mcp-config-example.json) |
| Contributing | [`../CONTRIBUTING.md`](../CONTRIBUTING.md) |
| Security policy | [`../SECURITY.md`](../SECURITY.md) |
| Change history | [`../CHANGELOG.md`](../CHANGELOG.md) |

## Examples

Each is a runnable `main` package under `cmd/`, with its own README covering what
it demonstrates, how to run it, and what to look for.

| Example | Demonstrates |
|---|---|
| [`../cmd/api-example`](../cmd/api-example) | REST API, OpenAPI docs, WebSocket chat, static files |
| [`../cmd/dashboard-example`](../cmd/dashboard-example) | The developer dashboard against real traffic |
| [`../cmd/templates-example`](../cmd/templates-example) | Server-rendered views, components, i18n, SPA re-render |
| [`../cmd/workflow-example`](../cmd/workflow-example) | Retries, compensation, live workflow visualisation |
| [`../cmd/video-example`](../cmd/video-example) | Range-request video streaming |
| [`../cmd/events-example`](../cmd/events-example) | The event bus: listeners, priorities, async |
| [`../cmd/fleet-example`](../cmd/fleet-example) | Three services, distributed tracing, Docker Compose |
| [`../cmd/automcp-example`](../cmd/automcp-example) | Auto-MCP: tagged routes as agent-callable tools, and the untagged one that never is |

## Historical

| Doc | Status |
|---|---|
| [`fleet-tracing-spec-delta.md`](./fleet-tracing-spec-delta.md) | A pre-implementation design review of the Fleet spec against the code as it stood at `317b7c0`. Fleet has since shipped; kept as a record of which spec premises were wrong and why. Not a guide — read [`fleet-tracing.md`](./fleet-tracing.md) instead. |
| [`repository-audit.md`](./repository-audit.md) | The structural audit that produced [`repository-structure.md`](./repository-structure.md). Kept because its findings cite paths and line numbers, so a future reader can check whether a rule is still earning its place. |
