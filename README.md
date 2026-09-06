<div align="center">

# 🌬️ Breeze

**A ridiculously fast, event‑driven Go web framework** — built for maximum
throughput, minimal allocations, native WebSockets, and a batteries‑included
path to production.

[![Documentation](https://img.shields.io/badge/Documentation-Latest-blue?style=for-the-badge)](https://nelthaarion.github.io/breeze)
[![GitHub](https://img.shields.io/badge/GitHub-nelthaarion%2Fbreeze-181717?style=for-the-badge&logo=github)](https:// github.com/nelthaarion/breeze/v2)
[![Go Version](https://img.shields.io/badge/Go-1.25%2B-00ADD8?style=for-the-badge&logo=go&logoColor=white)](https://go.dev)
[![License](https://img.shields.io/badge/License-MIT-green?style=for-the-badge)](./LICENSE)
[![PRs Welcome](https://img.shields.io/badge/PRs-welcome-brightgreen?style=for-the-badge)](./CONTRIBUTING.md)

*Built on [`gnet`](https://github.com/panjf2000/gnet) · One event loop per core · Zero‑copy where it counts*

</div>

---

Breeze is a modern, high‑performance Go web framework for people who want
speed **and** a real toolbox: a router, WebSockets, request binding, an
OpenAPI generator, a live developer dashboard, an event bus, durable
workflows, distributed tracing across services, a JSON‑RPC server, an
AI‑agent control plane — all first‑party, all documented, none of it bolted
on as an afterthought.

> 📖 Looking for the full reference instead of the tour? [`docs/README.md`](./docs/README.md)
> is the index — one row per subsystem, pointing at the document that covers it in full.

## 📑 Table of Contents

<table>
<tr>
<td valign="top" width="33%">

**Get started**
- [Installation](#-installation)
- [Quick Start](#-quick-start)
- [Graceful Shutdown](#-graceful-shutdown)
- [Docker](#-docker)
- [CLI](#-cli)
- [Packages at a Glance](#-packages-at-a-glance)

**Core HTTP**
- [Performance Core](#-performance-core)
- [Errors as Values](#-errors-as-values)
- [WebSocket](#-websocket)
- [Request Validation](#-request-validation)
- [Middleware](#-middleware)
- [OAuth2 Login](#-oauth2-login)

</td>
<td valign="top" width="33%">

**Data & workflow**
- [Events](#-events)
- [Workflows](#-workflows)
- [Migrations](#-migrations)
- [HTTP Client](#-http-client)
-
-
**See inside your app**
- [Dashboard](#-dashboard)
- [Observability](#-observability)
- [Diagnostics](#-diagnostics)
- [Fleet Tracing](#-fleet-tracing)

</td>
<td valign="top" width="33%">

**More protocols**
- [JSON‑RPC 2.0](#-json-rpc-20)
- [OpenAPI / Scalar](#-openapi--scalar)
- [Video Streaming](#-video-streaming)
- [Templates, SPA & i18n](#-templates-spa--i18n)
- [MCP for AI Agents](#-mcp-for-ai-agents)

**Wrap‑up**
- [Examples](#-examples)
- [Contributing](#-contributing)
- [Security](#-security-scanning)
- [License](#-license)

</td>
</tr>
</table>

---

## 📦 Installation

Requires **Go 1.25.13** or later.

```bash
go get  github.com/nelthaarion/breeze/v2
```

Pulls in **gnet v2** for the event loop, **go‑json** for fast marshaling,
**brotli** for compression, and **golang‑jwt** for authentication. Nothing
else — every other subsystem below is an opt‑in subpackage.

## 🚀 Quick Start

A complete server in under 20 lines:

```go
package main

import (
	"runtime"

	"github.com/nelthaarion/breeze/v2"
	middleware "github.com/nelthaarion/breeze/v2/middlewares"
)

func main() {
	router := breeze.NewRouter()
	router.Use(middleware.RecoveryMiddleware())
	router.Use(middleware.LoggingMiddleware())

	router.Handle(breeze.GET, "/", func(ctx *breeze.Context) error {
		return ctx.JSON(map[string]string{"status": "ok"})
	})
	router.Handle(breeze.GET, "/users/:id", func(ctx *breeze.Context) error {
		return ctx.JSON(map[string]string{"id": ctx.Param("id")})
	})

	pool := breeze.NewEventLoopWorkerPool(runtime.NumCPU())
	app := breeze.New(router, pool)
	app.Run(3000, true) // port, multiCore
}
```

```bash
go run main.go
# curl http://localhost:3000/        → {"status":"ok"}
# curl http://localhost:3000/users/42 → {"id":"42"}
```

## 🛑 Graceful Shutdown

`Run` blocks until the server is stopped; `Stop` stops it. The contract is
`net/http.Server.Shutdown`'s — a context bounds how long in-flight work is given
to finish, and whatever is left is closed:

```go
app := breeze.New(router, pool)

go func() {
	if err := app.Run(3000, true); err != nil {
		log.Printf("breeze: %v", err)
	}
}()

// SIGINT / SIGTERM
sig := make(chan os.Signal, 1)
signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
<-sig

ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
defer cancel()
if err := app.Stop(ctx); err != nil {
	log.Printf("breeze: unclean shutdown: %v", err)
}
pool.Shutdown(ctx) // Stop does not touch a pool it did not create
```

What `Stop` does, in order:

| Step | Behaviour |
|------|-----------|
| 1 | New connections are refused immediately |
| 2 | Every WebSocket connection gets a Close frame with **1001 going away**, and its handler's `OnClose` runs through the connection's ordered queue |
| 3 | Work already dispatched to the pool — blocking routes, WebSocket callbacks — is given until `ctx` is done |
| 4 | The listener closes, anything still connected is force-closed, and `Run` returns |

`Stop` returns `nil` on a clean stop, `ctx.Err()` when step 3 ran out of time (the
teardown still happens), and `breeze.ErrNotRunning` when there was nothing to stop.
It is idempotent, and when it returns, `Run`'s goroutine has exited. Each `*Breeze`
holds its own engine, so two servers in one process stop independently; a stopped
one is not reusable (`Run` then returns `breeze.ErrServerStopped`).

## 🐳 Docker

```bash
docker build -t breeze-example .
docker run --rm -p 3000:3000 breeze-example
# or
docker compose up --build
```

Breeze itself is a library — point `BREEZE_TARGET` at any `main` package in
your module to containerize *your* app:

```bash
docker build --build-arg BREEZE_TARGET=./cmd/dashboard-example -t my-app .
```

## 🧰 CLI

A `rails`‑style CLI: **`new`** scaffolds a project, **`generate`** writes code
you edit, **`add`** wires in a framework feature that already exists.

```bash
go install  github.com/nelthaarion/breeze/v2/cmd/breeze@latest

breeze new myapp                       # minimal REST API
breeze new myapp --template=views      # + templates, layouts, components

breeze generate resource User \
  name:string:required,min=2,max=40 \
  role:string:required,oneof=admin editor viewer

breeze add dashboard --allow-writes
breeze add events --async
breeze add --list                      # all 23 features, in wiring order

breeze routes                          # list routes without booting the app
breeze migrate up | down [n] | status
breeze makemigration <name>
```

<details>
<summary><b>🧪 Every <code>generate</code> subcommand</b></summary>

| Command | Writes |
|---|---|
| `generate resource <n> field:type…` | struct, handlers, in‑memory store, OpenAPI docs, validation — fully wired |
| `generate handler <n>` | route group with CRUD stubs |
| `generate model <n> field:type…` | `models/` struct + a paired SQL migration |
| `generate event <n> [field:type…]` | event type with emit/subscribe helpers |
| `generate listener <Event>` | subscriber for an existing event |
| `generate workflow <n> --steps=a,b,c` | workflow definition with retries + compensation |
| `generate middleware <n>` | `breeze.HandlerFunc` stub |
| `generate ws <n>` | WebSocket handler with connect/message/close hooks |
| `generate view <n>` | HTML view + the route that renders it |
| `generate job <n> [--every=1h]` | background job registered with the dashboard |
| `generate grpc <n>` | gRPC server/client scaffolding from a `*_grpc.go` interface |

Supported field types: `string`, `int`, `int64`, `uint`, `uint64`, `float32`,
`float64`, `bool`, `[]string`, `time.Time`, `time.Duration`.
</details>

<details>
<summary><b>🧩 All 23 <code>breeze add</code> features</b></summary>

`events` `observability` `dashboard` `workflow` `tuning` `migrator` `fleet`
`recovery` `logging` `security` `cors` `compression` `ratelimit` `i18n` `jwt`
`oauth2` `etag` `docs` `static` `video` `websocket` `templates` `jsonrpc`

Each is idempotent and written as a replaceable block in
`features_generated.go`; middlewares are emitted in an order that matters —
`recovery` outermost, `cors` before `ratelimit`, `etag` innermost.
</details>

## 🗂 Packages at a Glance

Breeze is one module. Each row is a package you can import directly:

| Package | Import path | Doc |
|---|---|---|
| Request binding | `breeze/binding` | [`docs/binding.md`](./docs/binding.md) |
| Middleware | `breeze/middlewares` | [`docs/middlewares.md`](./docs/middlewares.md) |
| OAuth2 | `breeze/middlewares/oauth2` | [`middlewares/oauth2/README.md`](./middlewares/oauth2/README.md) |
| Diagnostics | `breeze/diag` | [`docs/diag.md`](./docs/diag.md) |
| Events | `breeze/events` | [`events/README.md`](./events/README.md) |
| Workflows | `breeze/workflow` | [`workflow/README.md`](./workflow/README.md) |
| Dashboard | `breeze/dashboard` | [`dashboard/README.md`](./dashboard/README.md) |
| Observability | `breeze/observability` | [`observability/README.md`](./observability/README.md) |
| Fleet tracing | `breeze/fleet` | [`docs/fleet-tracing.md`](./docs/fleet-tracing.md) |
| Video streaming | `breeze/video` | [`video/README.md`](./video/README.md) |
| JSON‑RPC 2.0 | `breeze/rpc` | [`docs/rpc.md`](./docs/rpc.md) |
| OpenAPI / Scalar | `breeze/scalar` | [`docs/scalar.md`](./docs/scalar.md) |
| Migrations | `breeze/migrate` | [`docs/migrate.md`](./docs/migrate.md) |
| HTTP client | `breeze/client` | [`docs/client.md`](./docs/client.md) |
| MCP (in‑process) | `breeze/mcp` | [`docs/mcp-walkthrough.md`](./docs/mcp-walkthrough.md) |

`events`, `workflow` and `observability` never import the root package — they
work in a program that serves no HTTP at all.

---

# 🧬 Core HTTP Framework

## ⚡ Performance Core

- 🏎 **Inline execution** — a non‑blocking handler runs *directly on the
  `gnet` event‑loop goroutine* that read the request: no worker‑pool hop, no
  channel handoff, no `AsyncWrite` poller wakeup. This is the single largest
  throughput win in the framework.
- 🧠 **Zero‑copy headers (opt‑in)** — parse a request, route it, handle it,
  and answer it with **zero heap allocations**.
- 🗂 O(1) exact‑path routing via per‑method buckets, dynamic `:params` and
  wildcard segments.
- 📦 Everything hot is pooled: `Context`, `HTTPRequest`, `HTTPResponse`, wire
  buffers, route‑param maps.
- 🧵 A configurable worker pool for anything that blocks, with three
  backpressure policies (`Block` / `Reject` / `Spawn`).

```go
// In-memory, returns immediately → inline (fastest path).
router.Handle(breeze.GET, "/users/:id", getUser)

// Touches a database, disk, or network → the worker pool.
router.HandleBlocking(breeze.POST, "/orders", createOrder)

// Remove the last allocation on the fast path (careful — see the docs on lifetime):
app.SetZeroCopyHeaders(true)
```

## 🧯 Errors as Values

A handler — and a middleware — returns an `error`. One function decides what
that becomes on the wire, and it can never leave a connection with no
response at all.

```go
router.Handle(breeze.GET, "/orders/:id", func(ctx *breeze.Context) error {
	order, err := store.Find(ctx.Param("id"))
	if errors.Is(err, sql.ErrNoRows) {
		return breeze.NewHTTPError(404, "no such order")
	}
	if err != nil {
		// client sees "unavailable"; the driver's message goes to the log only
		return breeze.WrapHTTPError(502, "the order service is unavailable", err)
	}
	return ctx.JSON(order)
})
```

| Returned | Response |
|---|---|
| `*binding.ValidationError` | `422` with field‑level RFC 9457 detail |
| `*breeze.HTTPError` | its status, with `Message` as the detail |
| anything else | logged to stderr, generic `500` |

## 🔌 WebSocket

A dedicated fast path — an upgraded connection carries **zero** HTTP
overhead, and an HTTP‑only server never even probes for one.

```go
hub := app.WebSocket("/ws", &breeze.WSHandlerFunc{
	Connect: func(c *breeze.WSConn) { c.SendText("welcome!") },
	Message: func(c *breeze.WSConn, op byte, payload []byte) {
		hub.BroadcastExcept(op, payload, c) // fan out to everyone else
	},
})
```

Binary & text frames, ping/pong, fragmentation, graceful close — all RFC 6455.

A connection's messages reach its handler **in the order the peer sent them**, one
at a time. Delivery is per-connection FIFO, so a handler parsing a stream can rely
on it; two different connections are still handled concurrently. `OnClose` runs
after every message already delivered, so per-connection state torn down there
cannot be used afterwards.

### Dialling out

`DialWS` is the symmetric other half: a Breeze process connecting *to* a WebSocket
server rather than accepting one. It returns the same `*WSConn`, so a peer table
holds one type for connections it accepted and connections it dialled.

```go
conn, err := breeze.DialWS("ws://peer:9000/p2p", breeze.WSClientConfig{
	HandshakeTimeout: 5 * time.Second,
	Header:           map[string]string{"Authorization": "Bearer " + token},
})
if err != nil {
	return err
}
conn.OnMessage(func(op byte, payload []byte) { peer.handle(op, payload) })
conn.OnClose(func(code uint16, reason string) { peers.drop(conn) })
conn.SendBinary(hello)
```

Full client handshake with `Sec-WebSocket-Accept` verification, mandatory frame
masking, `wss://` via `crypto/tls`, and `Ping` for liveness. One goroutine per
dialled connection — sized for tens to low hundreds of peers, not tens of
thousands. Reconnection is deliberately yours: `OnClose` is where a redial goes.

## ✅ Request Validation

One call: decode JSON body + query + path params, validate, and — on
failure — write an RFC 9457 `422` for you.

```go
type CreateUser struct {
	Name  string `json:"name"  validate:"required,min=2,max=50"`
	Email string `json:"email" validate:"required,email"`
	Role  string `json:"role"  validate:"oneof=admin user viewer"`
	ID    string `param:"id"`
}

router.Handle(breeze.POST, "/users/:id", func(ctx *breeze.Context) error {
	var in CreateUser
	if err := ctx.Bind(&in); err != nil {
		return nil // the 422 problem+json response is already written
	}
	return ctx.JSON(in)
})
```

## 🛡 Middleware

Twelve built‑ins, composed once at registration — a route with no `:params`
costs **zero** allocations to dispatch.

```go
import middleware "github.com/nelthaarion/breeze/v2/middlewares"

router.Use(middleware.RecoveryMiddleware())   // outermost
router.Use(middleware.LoggingMiddleware())
router.Use(middleware.NewRateLimiter(middleware.RateLimiterOptions{
	Requests: 100, Per: time.Minute,
}))
router.Use(middleware.CORSMiddleware(middleware.CORSOptions{AllowOrigins: "*"}))
router.Use(middleware.DefaultSecurityMiddleware())
router.Use(middleware.CompressionMiddleware()) // innermost
```

🚦 Rate Limiter · 🗜 Compression · 💾 ETag Cache · 🔑 JWT Auth · 🌍 CORS ·
🪖 Security Headers · 📝 Logger · 💥 Panic Recovery · 🌐 Locale ·
📚 Scalar (OpenAPI route) — see [`docs/middlewares.md`](./docs/middlewares.md)
for the install order and why it matters.

## 🔐 OAuth2 Login

Zero‑config social login under `middlewares/oauth2` — only `ClientID` and
`ClientSecret` are required.

```go
import "github.com/nelthaarion/breeze/v2/middlewares/oauth2"

cfg := oauth2.Config{
	Provider:     oauth2.Google,
	ClientID:     "your-client-id",
	ClientSecret: "your-client-secret",
	BaseURL:      "https://app.example.com",
	CookieSecret: "a-long-random-secret",
}

router.Handle(breeze.GET, "/auth/google", oauth2.Login(cfg))
router.Handle(breeze.GET, "/auth/google/callback", oauth2.Callback(cfg))
router.Handle(breeze.GET, "/dashboard", dashboard, oauth2.Auth(cfg))

func dashboard(ctx *breeze.Context) error {
	return ctx.JSON(oauth2.CurrentUser(ctx)) // *oauth2.User
}
```

🌐 **Google · GitHub · Microsoft · Discord** out of the box · 🔑 PKCE (S256) by
default · 🛡 CSRF‑protected, single‑use `state` · 🍪 signed HttpOnly cookies
or HS256 JWT sessions · 🔄 transparent token refresh · 🚫 open‑redirect guard.
Full reference: [`middlewares/oauth2/README.md`](./middlewares/oauth2/README.md).

---

# 🗃 Data, Events & Workflow

## 🔔 Events

The framework's internal nervous system — a typed, reflection‑free
publish/subscribe bus every subsystem uses to talk to your code.

```go
import "github.com/nelthaarion/breeze/v2/events"

type UserCreated struct{ UserID uint64 }

events.On(UserCreated{}, func(ctx *events.Context, e UserCreated) error {
	return sendWelcomeEmail(e.UserID)
}).Priority(events.PriorityHigh)

events.Emit(UserCreated{UserID: 42})
```

🧩 plain structs, no interfaces · ⚡ zero reflection on the dispatch path ·
🔒 lock‑free reads via `atomic.Pointer` snapshots · 🎯 before/normal/after
phases + priority · 🛑 `events.Stop`, filters, once‑listeners · 🔁 async
(goroutine or bounded worker pool) · 🛡 panic recovery · 📈 per‑event metrics
+ a ring‑buffer recorder. Full reference: [`events/README.md`](./events/README.md).

## 🧬 Workflows

Durable, in‑process orchestration — retries, timeouts, parallelism, Saga
rollback and crash recovery, with **no broker and no required database**.

```go
import "github.com/nelthaarion/breeze/v2/workflow"

def := workflow.New("order-processing").
	Step("validate", ValidateOrder).
	Step("charge", ChargeCard, workflow.WithCompensation(RefundCard)).
	Step("ship", CreateShipment)

engine := workflow.NewEngine()
engine.Register(def)

res, err := engine.Run(ctx, "order-processing", order)
```

If `ship` fails, `RefundCard` runs automatically — the failure path lives
next to the work, not scattered through error handling. `WithDependsOn`
opts a step into a validated, parallel DAG; `WithRetry` adds exponential
backoff with jitter; `Resume` replays what a crash interrupted **without**
re‑running completed steps. Full reference: [`workflow/README.md`](./workflow/README.md).

## 🗄 Migrations

Numbered `.up.sql` / `.down.sql` pairs, applied one transaction at a time,
with a checksummed ledger.

```go
//go:embed migrations/*.sql
var migrationsFS embed.FS

sub, _ := fs.Sub(migrationsFS, "migrations")
runner := migrate.New(db, sub)
runner.Up(context.Background())
```

```bash
breeze makemigration create_users   # writes the .up.sql / .down.sql pair
breeze migrate up                   # apply everything pending
breeze migrate status               # what's applied, and what drifted
```

## 🌐 HTTP Client

An outbound HTTP client built on the **same `gnet` engine** as the server —
one non‑blocking I/O model in both directions.

```go
c := client.New()
defer c.Close()

resp, err := c.Get("http://auth-service/verify")
if err == nil && resp.OK() {
	fmt.Println(resp.String())
}
```

Sized for service‑to‑service JSON traffic: HTTP/1.1, TLS via `crypto/tls`,
a per‑host connection pool (64 idle conns by default, vs. `net/http`'s 2).
Full reference: [`docs/client.md`](./docs/client.md).

---

# 👁 See Inside Your Running App

## 📊 Dashboard

A native developer dashboard — 14 pages, live over one WebSocket, **zero
overhead when disabled**.

```go
coll := dashboard.Install(app, router, dashboard.DefaultConfig())
router.Use(coll.Middleware())
// → http://localhost:3000/dashboard  (default: admin / admin)
```

📈 Overview (RPS, latency, memory, goroutines) · 📡 Live Requests ·
🕒 Developer Timeline (per‑request profiler) · 🛣 Routes Explorer ·
🧪 API Explorer with curl/Go/JS/Python/C#/PHP snippets · 🗄 Database Browser
(optional inline CRUD) · 🔍 ORM Query Monitor · 💾 Cache / 🧾 Queue /
⏰ Scheduler monitors · 📝 Logs (5 tabs) · ❤️ Health checks · 🎬 Video monitor ·
🕸 Fleet page (once an aggregator is attached). Full reference:
[`dashboard/README.md`](./dashboard/README.md).

## 👁 Observability

Records what actually happened — which events fired, which listeners ran,
what failed — and it costs **nothing** until you attach it.

```go
col := observability.NewCollector(observability.Config{Capacity: 1000, Metrics: true})
defer col.Close()

detach := observability.AttachEvents(events.Default, col)
defer detach() // switch it back off at runtime, with zero residual cost

for _, sig := range col.Recent(10) {
	fmt.Printf("%s took %v (%d listeners)\n", sig.Name, sig.Duration, sig.Executed)
}
```

🔍 filterable queries · 🌊 live streaming subscribers (never block the app) ·
📊 per‑name metrics · 🕸 an *observed* execution graph · 🎭 automatic
secret masking on captured payloads. Full reference:
[`observability/README.md`](./observability/README.md).

## 🩺 Diagnostics

One endpoint that answers **"what is every subsystem of this process doing
right now?"** — zero cost until read.

```go
import "github.com/nelthaarion/breeze/v2/diag"

diag.Register("billing", func() diag.Report {
	if !gateway.Configured() {
		return diag.Off("no payment gateway configured; call billing.Configure(key)")
	}
	return diag.OK(fmt.Sprintf("%d charge(s) settled", settled.Load()), nil)
})
```

```bash
curl -u admin:pass http://127.0.0.1:8080/dashboard/api/diagnostics
```

Four honest states — `ok` / `degraded` / `off` / `unknown` — so "never wired
up" is never confused with "wired up and unhappy". Full reference:
[`docs/diag.md`](./docs/diag.md).

## 🕸 Fleet Tracing

Distributed tracing across your services — **no Jaeger, Zipkin, or OTel
Collector required.**

```go
tracer := fleet.New(fleet.TracerConfig{
	ServiceName:   "orders",
	AggregatorURL: "http://fleet-aggregator:9000/fleet",
})
router.Use(fleet.Middleware(tracer)) // before the dashboard middleware
defer tracer.Close(context.Background())
```

```bash
go run ./cmd/fleet-aggregator   # run once, shared by every service
```

🔗 W3C `traceparent` propagation · 🌪 async span export, never blocks a
handler · 🗺 topology graph with live p50/p95 per hop · 🎯 deterministic
root‑cause & blast‑radius highlighting (graph math, not a black box) ·
📐 live OpenAPI contract validation · 📜 trace‑correlated log stitching.
Full reference: [`docs/fleet-tracing.md`](./docs/fleet-tracing.md).

---

# 🔗 More Protocols

## 🔗 JSON‑RPC 2.0

A complete JSON‑RPC 2.0 server, on **its own port**, running directly on
`gnet`'s event loop — a peer of the HTTP layer, not a route on it.

```go
reg := rpc.NewRegistry()
reg.Use(logging)                        // mirrors Router.Use
reg.Register("sum", func(ctx *rpc.Context) {
	var in struct{ A, B int }
	if err := ctx.Bind(&in); err != nil {
		ctx.Errorf(rpc.CodeInvalidParams, "a and b must be numbers")
		return
	}
	ctx.Result(in.A + in.B)
})

srv := rpc.NewServer(reg)
srv.SetPool(breeze.NewEventLoopWorkerPool(runtime.NumCPU()))
log.Fatal(srv.Run(9000, true))
```

```bash
curl -s --data '{"jsonrpc":"2.0","id":1,"method":"sum","params":{"a":2,"b":3}}' \
  --output - localhost:9000
# {"jsonrpc":"2.0","id":1,"result":5}
```

Single requests, notifications, batches, all five standard error codes, and
a **stdio transport** for peers that talk over pipes (this is what powers
MCP below). Full reference: [`docs/rpc.md`](./docs/rpc.md).

## 📚 OpenAPI / Scalar

A spec generated from **declared** route docs (checked by the compiler, not
sniffed from traffic or parsed from a comment) — plus a Scalar UI and an
`llms.txt` for models.

```go
router.Use(middleware.ScalarMiddleware(router, middleware.ScalarOptions{
	Title: "My API", Version: "2.0.0",
}))

router.Handle(breeze.POST, "/users", createUser,
	middleware.DocPOST("/users", scalar.RouteDoc{
		Title: "Create user",
		Tags:  []string{"users"},
		Input: []scalar.InputGroup{
			{Type: scalar.InputBody, Fields: CreateUserRequest{}, Required: true},
		},
		Output:       UserResponse{},
		OutputStatus: 201,
	}),
)
```

- `GET /openapi.json` — the spec
- `GET /scalar` — the interactive UI
- `GET /llms.txt`, `GET /llms-full.txt` — the model‑readable index

Full reference: [`docs/scalar.md`](./docs/scalar.md).

## 🎬 Video Streaming

Byte‑range streaming so browsers can actually **seek** — the one thing a
plain static file handler silently gets wrong.

```go
video.Mount(router, video.Config{Root: "./media"})
// registers GET + HEAD on /videos/*filepath
```

```go
// optional: signed, expiring URLs — verified before any disk access
video.Mount(router, video.Config{Root: "./media", Secret: []byte(secret)})
url := "/videos/movie.mp4?" + video.Sign(secret, "movie.mp4", 10*time.Minute)
```

🎯 correct `206 Partial Content` / `416` handling · 🧠 one pooled 256 KiB
chunk per response (10,000 viewers ≠ 10,000 copies) · 🛡 layered traversal
defense · 🕵️ 404‑never‑403 so the filesystem can't be mapped · ⚡ conditional
requests answered before the file is even opened. Full reference:
[`video/README.md`](./video/README.md).

## 🖼 Templates, SPA & i18n

Server‑rendered views with a built‑in SPA runtime — link clicks are
intercepted, partials are swapped in, and the URL updates — with zero
client‑side framework.

```go
te := breeze.NewTemplateEngine(breeze.TemplateConfig{ViewsDir: "views"})

router.Handle(breeze.GET, "/", func(ctx *breeze.Context) error {
	return te.RenderView(ctx, "home", map[string]any{"Name": "World"})
})
```

```go
// locales/en.json: {"home": {"greeting": "Hello, %{name}!"}}
i18n, _ := breeze.NewI18n(breeze.I18nConfig{Dir: "locales", DefaultLocale: "en"})
router.Use(middleware.LocaleMiddleware(i18n))
// in a template: {{t "home.greeting" "name" .User.Name}}
```

📄 layouts + reusable components · 🔁 partial re‑renders with no full reload ·
🌍 per‑locale translation with pluralization · 🔥 hot‑reload in dev mode.

## 🤖 MCP for AI Agents

Breeze exposes itself to AI agents over the **Model Context Protocol**, in
three different shapes depending on what the agent needs to do.

### 1️⃣ `breeze-mcp` — the generator (build & change a project)

```bash
go build -o breeze-mcp ./cmd/breeze-mcp
```

```json
{
  "mcpServers": {
    "breeze": { "command": "/path/to/breeze-mcp", "args": ["--mode=generator"] }
  }
}
```

Or served over the network, for a container an agent reaches remotely:

```bash
breeze-mcp --mode generator --port 2000 --scope fleet,runtime
# loopback by default; a bearer token is mandatory on every request
```

~40 tools: scaffold a project, generate a resource, wire in a feature, plan
change sets, run the Go toolchain, and inspect a live service — routes,
errors, logs, performance, traces, contract violations,
`breeze_diagnose_service`. Docker‑aware fleet provisioning is included.

### 2️⃣ `--mode app-runtime` — read‑only introspection of a deployed instance

```bash
breeze-mcp --mode app-runtime --port 2000 --scope fleet
```

Structurally different, not just filtered: the mutating tools are **never
registered** — no token scope or misconfiguration can reach what was never
built. Exactly what a deployed container should expose.

### 3️⃣ In‑process — the app serves its own control plane

No separate binary; the application answers MCP itself, beside its own
traffic:

```go
scope, _ := mcp.NewScope(mcp.CapFleet) // narrow what this token reaches
server, token, err := mcp.StartInProcess(app, mcp.InProcessConfig{
	Mode:  mcp.ModeAppRuntime, // required, no default
	Port:  2000,
	Token: os.Getenv("BREEZE_MCP_TOKEN"),
	Scope: scope,
})
if err != nil {
	log.Fatal(err)
}
go server.Serve()
app.Run(3000, true)
```

### 🏷 Bonus: make your own routes agent‑callable (Auto‑MCP)

Tag a route once — the tag is stripped at registration, so it costs nothing
per request, and calls run through the exact same middleware chain as HTTP:

```go
router.Handle(breeze.POST, "/orders", createOrder,
	auth.Require(),
	breeze.MCPTool("create_order", "Places an order for a customer."))
```

An untagged route is *never* exposed — that's the whole opt‑in surface.
`app.EnableMCP(addr)` serves every tagged route as a callable tool, separate
from (and complementary to) the read‑only in‑process endpoint above. Full
reference: [`docs/mcp-walkthrough.md`](./docs/mcp-walkthrough.md).

---

## 🧪 Examples

Every subsystem has a runnable example under `cmd/`, each with its own README:

| Example | Demonstrates |
|---|---|
| [`cmd/api-example`](./cmd/api-example) | REST API, OpenAPI docs, WebSocket chat, static files |
| [`cmd/dashboard-example`](./cmd/dashboard-example) | The dashboard against real traffic |
| [`cmd/templates-example`](./cmd/templates-example) | Server‑rendered views, components, i18n, SPA re‑render |
| [`cmd/workflow-example`](./cmd/workflow-example) | Retries, compensation, live workflow visualisation |
| [`cmd/video-example`](./cmd/video-example) | Range‑request video streaming |
| [`cmd/events-example`](./cmd/events-example) | The event bus: listeners, priorities, async |
| [`cmd/fleet-example`](./cmd/fleet-example) | Three services, distributed tracing, Docker Compose |
| [`cmd/automcp-example`](./cmd/automcp-example) | Auto‑MCP: a tagged route, one behind auth, and one that's never a tool |

## 🤝 Contributing

We welcome contributions of all sizes — bug fixes, docs, performance work,
new features.

1. **Fork** the repository
2. **Create** a feature branch (`git checkout -b feat/my-thing`)
3. **Commit** your changes with a clear message
4. **Open** a pull request describing what and why

Please open an issue first for non‑trivial changes so we can align on the
approach before you spend time on code.

## 🔐 Security Scanning

- **CodeQL** static analysis (`.github/workflows/codeql.yml`)
- **govulncheck** vulnerability scanning (`.github/workflows/govulncheck.yml`)
- **Gitleaks** secret scanning (`.github/workflows/secret-scan.yml`)
- **Dependabot** weekly updates for Go modules and GitHub Actions

Repository admins: enable GitHub Advanced Security secret scanning + push
protection, require the security workflows on branch protection, and triage
alerts via [`.github/SECURITY_TRIAGE.md`](./.github/SECURITY_TRIAGE.md).

## 📄 License

Breeze is released under the [MIT License](./LICENSE).

<div align="center">

© Nelthaarion — made with 🌬️ and a healthy dislike of unnecessary allocations

</div>
