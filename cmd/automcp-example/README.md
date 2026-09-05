# `cmd/automcp-example`

## What this demonstrates

Auto-MCP: taking routes an application already serves and publishing *some* of them as
MCP tools an agent can call — without writing a second description of them.

One order service, three routes, chosen so they can be compared:

| Route | Tagged? | What it shows |
|---|---|---|
| `POST /orders` | `create_order` | the basic case — a real handler behind a derived schema |
| `GET /orders/:id` | `get_order`, behind JWT | the route's own middleware still runs |
| `GET /internal/metrics` | **no tag** | documented, and still never a tool |

Specifically:

- `breeze.MCPTool("create_order", "…")` as a trailing argument to `router.Handle`
- `app.EnableMCP(addr)` on its own listener, separate from the HTTP port
- `scalar.RouteDoc` as the single source for both `/openapi.json` and the tool schema
- `middleware.JWTAuthMiddleware` on a tagged route, enforced identically over MCP
- an untagged route that is in the OpenAPI document and unreachable as a tool

## How to run it

```bash
go run ./cmd/automcp-example
```

It prints a one-hour token and two `curl` lines. HTTP is on `:3000`, Auto-MCP on
`127.0.0.1:2001`, docs at `/scalar`.

```bash
curl -X POST localhost:3000/orders \
  -d '{"sku":"BRZ-100","quantity":2,"customer":"ada@example.com"}'
curl localhost:3000/orders/ord-1 -H "Authorization: Bearer $TOKEN"
curl localhost:3000/internal/metrics
```

The MCP side is **raw JSON-RPC 2.0 over TCP** — not MCP Streamable HTTP, which is what
`breeze-mcp --port` serves. There is no HTTP framing and no session handshake, so `curl`
is not the tool for it; write a JSON value to the socket and read one back:

```bash
printf '{"jsonrpc":"2.0","id":1,"method":"tools/list"}\n' | nc 127.0.0.1 2001
```

`tools/list` returns two tools. `internal_metrics` is not one of them, and calling it by
name answers `no such tool: internal_metrics (available: create_order, get_order)`.

```bash
go test ./cmd/automcp-example
```

By default the JWT secret is random per run, so tokens do not survive a restart. Set
`BREEZE_EXAMPLE_JWT_SECRET` to keep them.

## The four assertions, in plain language

Each is its own test, so a failure names which guarantee broke rather than reporting
"MCP is broken".

**1. The tool schema says what the OpenAPI document says**
(`TestToolSchemaMatchesTheRoutesOpenAPIShape`)

An agent reads the tool schema; a human reads the OpenAPI document. If they disagree, the
agent's calls are wrong in a way nobody reviewing the docs would notice. The test
generates the real document and compares both directions — every declared field must be
an accepted argument, and every accepted argument must be declared. Field *descriptions*
are compared too: that sentence is what a model reads to decide what to put in a field,
so losing it degrades call quality without changing a single type.

**2. A real handler ran** (`TestCallingCreateOrderRunsTheRealHandler`)

A matching schema proves nothing if the handler is a stub. So the assertions are on three
things the handler computes and could not echo back: the generated `id`, `unit_cents`
looked up from the catalogue (the caller never sends it), and `total_cents` equal to
`unit_cents × quantity`. An unknown SKU comes back as a 404 and a `quantity` of 0 as a
422 from `ctx.Bind` — the same answers HTTP gives.

**3. The untagged route is not listed, and not callable either**
(`TestTheUntaggedRouteIsNeitherListedNorCallable`)

Two different claims. Absent from `tools/list` is what a well-behaved agent sees;
unreachable by name is what a badly-behaved one gets, and it is the one that matters.
The test also confirms the route works over HTTP and *is* in the OpenAPI document —
otherwise it would pass for the uninteresting reason that the route is broken. Being
discoverable is not being callable, and only the tag decides the second.

**4. Auth is enforced identically** (`TestGetOrderIsRefusedOverMCPExactlyAsOverHTTP`)

The claim is not "MCP returns 401" — a separate refusal path that happened to use the
same status code would pass that. The test runs the same request through the router's own
dispatch and requires the status *and body* to match, which is only true if the same
middleware ran. A token signed with the wrong key is rejected, a valid one gets through
with its claims visible in the response, and an undeclared header (`authorization`,
`cookie`, `x-forwarded-for`) on `create_order` is refused by name rather than silently
dropped.

## What to look for

**The tag is not a step in the chain.** `Handle` strips it at registration, so a tagged
route's chain is byte-for-byte what an untagged one would be. That is why the tag costs
nothing per request and why an MCP call and an HTTP call cannot diverge — they run the
same slice, not two copies of it.

**`orderAuthHeader` is why the JWT route is callable at all.** A tool may only send
arguments it advertises, and it advertises exactly what the route declared. Declaring the
header is what makes credentials possible; not declaring one is what makes it
unsmuggleable. Which headers a tool can influence is a decision written down at the route,
not a default.

**Two rule sets on `CreateOrderRequest`, written to agree.** `json` without `omitempty`
makes a field required in the schema; `validate` makes it required at request time. They
are enforced by different code, so a field optional in one and required in the other would
advertise a call the route then refuses.

**The MCP listener has no authentication of its own.** Auto-MCP is plain JSON-RPC — no
bearer token, no Origin check, no session. Everything between a caller and a route is the
route's own middleware, which is why `get_order` is behind JWT and why this binds
`127.0.0.1`. Exposing that port off-host exposes every tagged route to whatever can reach
it. (`breeze-mcp --port` is the other endpoint and *does* require a token; it serves
framework introspection, not this app's routes.)

**`EnableMCP` returns configuration errors synchronously.** A tag on an undocumented route
is a tool whose arguments are unknown, so it fails at startup rather than on an agent's
first call. `TestEnableMCPValidatesEveryTagBeforeListening` asserts that.

Next: [`../../docs/mcp-walkthrough.md`](../../docs/mcp-walkthrough.md) for how this
differs from `mcp.StartInProcess` — Auto-MCP lets an agent *call* the service, the
embedded endpoint lets it *understand* one — and
[`../../docs/scalar.md`](../../docs/scalar.md) for what else `RouteDoc` can declare.
