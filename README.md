<div align="center">

# Breeze

**A ridiculously fast, event-driven Go web framework built for maximum
throughput, minimal allocations, native WebSockets, and
production-ready APIs.**

[![Documentation](https://img.shields.io/badge/Documentation-Latest-blue?style=for-the-badge)](https://nelthaarion.github.io/breeze)
[![GitHub](https://img.shields.io/badge/GitHub-nelthaarion%2Fbreeze-181717?style=for-the-badge&logo=github)](https://github.com/nelthaarion/breeze)
[![License](https://img.shields.io/badge/License-MIT-green?style=for-the-badge)](./LICENSE)

</div>

---

Breeze is a modern, high-performance Go web framework engineered for
developers who demand speed without sacrificing developer experience.
Built on an event-driven architecture, Breeze minimizes allocations,
optimizes every request path, and provides first-class support for REST
APIs, WebSockets, middleware, and automatic OpenAPI documentation.

Whether you're building microservices, real-time applications, or
high-throughput APIs, Breeze is designed to handle millions of requests
efficiently while keeping your code clean and maintainable.

## Table of Contents

- [Installation](#installation)
- [Quick Start](#quick-start)
- [Docker](#docker)
- [CLI — Scaffolding & Code Generation](#-cli--scaffolding--code-generation)
- [Features](#features)
  - [Built for Extreme Performance](#-built-for-extreme-performance)
  - [Inline Execution](#-inline-execution)
  - [Zero-Copy Headers](#-zero-copy-headers)
  - [High-Performance Routing](#-high-performance-routing)
  - [Native WebSocket Engine](#-native-websocket-engine)
    - [Built-in OpenAPI / Scalar](#-built-in-openapi--scalar)
    - [gRPC Code Generation](#-grpc-code-generation)
  - [Production Middleware](#-production-middleware)
  - [OAuth2 / Social Login](#-oauth2--social-login)
  - [Event System](#-event-system)
  - [Video Streaming](#-video-streaming)
  - [Durable Workflows](#-durable-workflows)

  - [Built-in Developer Dashboard](#-built-in-developer-dashboard)

  - [Developer Experience](#-developer-experience)
  - [Performance Optimizations](#-performance-optimizations)
- [Support the Project](#-support-the-project)

- [Contributing](#-contributing)
- [Security Scanning](#-security-scanning)
- [License](#license)

## Installation

Requires **Go 1.25.13** or later.


```bash
go get github.com/nelthaarion/breeze
```

The module pulls in **gnet v2** for the event loop, **go-json** for fast
JSON marshaling, **brotli** for compression, and **golang-jwt/jwt** for
authentication.

📖 **Full documentation:** <https://nelthaarion.github.io/breeze>

## Quick Start

A complete working server in under 20 lines:

```go
package main

import (
    "runtime"

    "github.com/nelthaarion/breeze"
    middleware "github.com/nelthaarion/breeze/middlewares"
)

func main() {
    router := breeze.NewRouter()

    router.Use(middleware.RecoveryMiddleware())
    router.Use(middleware.LoggingMiddleware())

    router.Handle(breeze.GET, "/", func(ctx *breeze.Context) {
        ctx.JSON(map[string]string{"status": "ok"})
    })

    router.Handle(breeze.GET, "/users/:id", func(ctx *breeze.Context) {
        ctx.JSON(map[string]string{"id": ctx.Param("id")})
    })

    pool := breeze.NewWorkerPool(runtime.NumCPU())
    app  := breeze.New(router, pool)
    app.Run(3000, true) // port, multiCore
}
```

Run it:

```bash
go run main.go
# → curl http://localhost:3000/        → {"status":"ok"}
# → curl http://localhost:3000/users/42 → {"id":"42"}
```

## Docker

The repository ships a multi-stage `Dockerfile` and `docker-compose.yml`
that containerize the example server in `./cmd` (~25 MB image, static
binary, non-root user, built-in healthcheck):

```bash
# Plain Docker
docker build -t breeze-example .
docker run --rm -p 3000:3000 breeze-example

# Or with Compose
docker compose up --build
```

Breeze itself is a library — to containerize your own application, point
the `BREEZE_TARGET` build argument at any main package in the module:

```bash
docker build --build-arg BREEZE_TARGET=./cmd/dashboard-example -t my-app .
```

## 🧰 CLI — Scaffolding & Code Generation

Breeze ships a `rails`-style CLI. It has two verbs: **`generate`** writes code
you then edit, **`add`** wires a framework feature that already exists into your
project.

```bash
go install github.com/nelthaarion/breeze/cmd/breeze@latest
```

**Start a new project:**

```bash
breeze new myapp                    # minimal REST API layout (default)
breeze new myapp --template=views   # + views/components/template engine
```

**Generate a full CRUD resource** — request/response structs, handlers, an
in-memory store, OpenAPI docs and request validation, wired into the router
automatically:

```bash
breeze generate resource User name:string email:string age:int
```

A third segment on a field becomes a `validate:"..."` tag, which the generated
handler enforces through `binding.Bind` — a request that breaks a rule gets a
`422` with an RFC 9457 body rather than being stored:

```bash
breeze generate resource User \
  name:string:required,min=2,max=40 \
  role:string:required,oneof=admin editor viewer
```

String fields given no rules are inferred as `required` (and `required,email`
when named like an email address); numbers and bools are left alone, since
`required` means *non-zero* and would otherwise reject `age: 0`. Pass
`--no-validate` to turn inference off. Rules: `required`, `email`, `min=N`,
`max=N`, `oneof=a b c`.

**The other generators:**

| Command | Writes |
|---|---|
| `generate handler <Name>` | route group with CRUD stubs, no structs or docs |
| `generate model <Name> field:type…` | `models/` struct plus a paired SQL migration |
| `generate event <Name> [field:type…]` | event type with emit and subscribe helpers |
| `generate listener <Event>` | subscriber for an existing event |
| `generate workflow <Name> --steps=a,b,c` | workflow definition with retries and compensation |
| `generate middleware <Name>` | `breeze.HandlerFunc` stub |
| `generate ws <Name>` | WebSocket handler with connect/message/close hooks |
| `generate view <Name>` | HTML view and the route that renders it |
| `generate job <Name> [--every=1h]` | background job, registered with the dashboard |
| `generate grpc <Name>` | gRPC server/client scaffolding (see below) |

**Wire in a framework feature** — 21 of them, each idempotent, each written as a
replaceable block in `features_generated.go`:

```bash
breeze add dashboard --allow-writes
breeze add events --async
breeze add video --root=./media
breeze add --list                   # all 21, in the order they are wired
```

Middlewares are emitted in an order that matters: `recovery` outermost so it
catches panics from everything below, `cors` before `ratelimit` so a preflight is
never rate-limited, `etag` innermost so a cached body is never served to an
unauthenticated caller.

**And the rest:**

```bash
breeze routes                       # list generated routes without booting the app
breeze migrate up|down [n]|status   # via the runner from `breeze add migrator`
breeze makemigration <Name>
breeze version
breeze help <command>
```

`breeze migrate` shells out to `cmd/migrate` in your project, which
`breeze add migrator --driver=postgres` writes with your SQL driver
blank-imported. That keeps `lib/pq`, `mysql` and `sqlite3` out of breeze's own
`go.mod` while still letting the CLI run your migrations.

**gRPC** scaffolding is generated from an interface declared in any `*_grpc.go`
file — no naming convention on methods, just a `grpc_type` comment (`Unary`,
`ServerSideStreaming`, `ClientSideStreaming`, or `Bidirectional`) above each:

```bash
breeze generate grpc UserService
```

The `resource` and `handler` generators write to `handlers/<name>.go` and
register routes in a single `routes_generated.go` file — your hand-written
`main.go` is never touched. Re-running `generate` for the same resource
replaces its block, so it's safe to regenerate after adding fields (pass
`--force` to overwrite the handler file too). The `grpc` generator writes
its own server/client/adapter files alongside the `*_grpc.go` interface it
was generated from, and also supports `--force` to overwrite them.

Supported field types: `string`, `int`, `int64`, `uint`, `uint64`, `float32`,
`float64`, `bool`, `[]string`, `time.Time`, `time.Duration`.

## Features

### 🚀 Built for Extreme Performance

- ⚡ Event-driven architecture powered by `gnet`
- 🏎 **Inline execution**: non-blocking handlers run directly on the `gnet`
  event-loop goroutine — no worker-pool hop, no channel handoff, no
  `AsyncWrite` poller wakeup (see [Inline Execution](#-inline-execution))
- 🧠 Zero-copy HTTP request parsing, optionally all the way down to **zero
  allocations per request** (see [Zero-Copy Headers](#-zero-copy-headers))
- 📦 Minimal allocations via pooled `Context`, `HTTPRequest`,
  `HTTPResponse`, wire buffers, and route parameter maps (`sync.Pool`)
- 🔥 Optimized response serialization (precomputed status-line table,
  append-in-place into a pooled buffer, no `fmt.Sprintf`)
- 🗂 O(1) exact-path route lookup via per-method buckets
- 🧵 Configurable Worker Pool backpressure (`OverflowBlock` /
  `OverflowReject` / `OverflowSpawn`)
- 🌲 Single-allocation middleware chain construction in the router
- 💨 Lock-free fast paths for critical operations
- 🎯 Preallocated buffers & cached status codes
- 🧊 Cache-line-padded pool counters (no false sharing between reactors)

### 🏎 Inline Execution

By default, Breeze runs handlers **directly on the `gnet` event-loop
goroutine** that read the request, rather than passing every request through
the worker pool.

`gnet` already runs one event loop per core and pins each connection to one,
so the parallelism is there before the pool is involved. Funnelling every
request through a single channel re-serialises that work and adds two
goroutine handoffs plus a poller wakeup to each request. Removing the hop is
the single largest throughput win in the framework.

**This puts a requirement on your handlers.** A handler on the inline path
must not block, because the event-loop goroutine it occupies is also serving
every other connection pinned to that reactor. Anything that waits — a SQL
query, a file read, an outbound HTTP call, a lock held under I/O — stalls all
of them for its duration.

Register those with **`HandleBlocking`**, which routes the request through the
worker pool exactly as before:

```go
// In-memory, returns immediately → inline (fastest path).
router.Handle(breeze.GET, "/users/:id", getUser)

// Touches a database, the disk, or the network → worker pool.
router.HandleBlocking(breeze.POST, "/orders", createOrder)
```

The framework's own I/O-bound routes already do this: static files, video
streaming, template rendering, the dashboard, the OpenAPI/Scalar endpoints,
the OAuth2 flow, and the WebSocket upgrade are all registered as blocking.

To opt out globally and send every route through the pool — the pre-inline
behaviour — call:

```go
app.SetInlineExecution(false)
```

### 🧠 Zero-Copy Headers

Answering a request on the inline path allocates exactly once: the parser copies
the request's header block into memory the request owns, and points `Path` and
every header key and value at that copy. `SetZeroCopyHeaders(true)` removes that
copy too, so a request is parsed, routed, handled and answered **without a
single heap allocation by the framework**:

```go
app.SetZeroCopyHeaders(true)
```

A few hundred bytes per request does not sound like much. At high request rates
it is a continuous stream of short-lived garbage, and the mark work for it comes
back as GC assist on the very event-loop goroutines that are supposed to be
serving connections. Removing the allocation removes the assist.

**The trade-off is lifetime.** With this on, every string on `ctx.Req` — `Path`
included — is a view into the connection's read buffer, which is reused for the
next read. Inside your handler they are always valid; Breeze re-parses a request
into owned memory before it is ever handed to a worker goroutine. What is not
safe is keeping one *after* the handler returns:

```go
app.Handle(breeze.GET, "/x", func(c *breeze.Context) {
    seen[c.Req.Path] = true                  // ✗ mutates into a later request
    seen[strings.Clone(c.Req.Path)] = true   // ✓
})
```

The failure mode is not a crash. The bytes stay mapped and readable, so a stored
string silently turns into a fragment of some later request — and as a map key it
lands in the wrong bucket and corrupts every lookup after it. The rule for
anything that outlives the handler, including a package-level cache or a value
sent to another goroutine, is `strings.Clone`.

Breeze's own middlewares hold to that rule, so the dashboard, the ETag cache, and
the rest are safe with this enabled. Third-party middleware written against the
default contract may not be — which is why the flag is **off by default**. Turn
it on for a service whose handlers read the request, write a response and keep
nothing, which is most of them and is where the throughput is won. Leave it off
if you cannot audit what your handlers and middlewares retain.

### 🌐 High-Performance Routing

- ⚡ Fast HTTP router
- 🎯 Dynamic route parameters
- 🌲 Wildcard routing
- 📂 Static file serving
- 🧩 Global middleware pipeline
- 🔍 Optimized route matching

### 🔌 Native WebSocket Engine

- ⚡ Zero-overhead HTTP → WebSocket upgrade
- 🔥 Dedicated WebSocket fast path
- 📡 Binary & Text frames
- ❤️ Ping / Pong support
- 🔄 Fragmented frame handling
- 🚪 Graceful close frames
- 🧵 Concurrent connection management

### 📚 Built-in OpenAPI / Scalar

- 📖 Automatic OpenAPI 3.1 generation
- 📝 Route registration
- 🎯 Schema generation
- 🔍 Typed request & response definitions
- 🌍 Ready for Scalar API Reference

### 📡 gRPC Code Generation

- 🔎 Auto-detects gRPC services from any `*_grpc.go` interface file —
  no naming convention required
- 🏷 Call style (`Unary`, `ServerSideStreaming`, `ClientSideStreaming`,
  `Bidirectional`) set per-method via a `grpc_type` comment annotation
- 🧩 Generates server/client scaffolding and adapters with
  `breeze generate grpc <InterfaceName>`

### 🛡 Production Middleware

- 🚦 Rate Limiter
- 🗜 Compression
- 💾 Response Cache
- 🔑 JWT Authentication
- 🌍 CORS
- 🪖 Security Headers
- 📝 Request Logger
- 💥 Panic Recovery

### 🔐 OAuth2 / Social Login

Zero-config OAuth2 / OpenID Connect login under `middlewares/oauth2` — only
`ClientID` and `ClientSecret` are required, everything else has a secure default.


- 🌐 Four providers out of the box: **Google, GitHub, Microsoft, Discord**
- 🔑 PKCE (S256) enabled by default for every provider
- 🛡 CSRF-protected `state` — signed, short-lived, single-use, constant-time compared
- 🍪 Signed, HttpOnly, `SameSite=Lax` cookies (or algorithm-pinned HS256 JWT sessions)
- 🔄 Transparent access-token refresh with session rotation
- 👤 Normalized provider-independent `User` (id, email, name, username, avatar)
- 🧩 Six composable middlewares: `Login`, `Callback`, `Auth`, `Optional`, `Refresh`, `Logout`
- 🚫 Open-redirect guard on post-login redirects

```go
import "github.com/nelthaarion/breeze/middlewares/oauth2"

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

func dashboard(ctx *breeze.Context) {
    ctx.JSON(oauth2.CurrentUser(ctx)) // *oauth2.User
}
```

See [`middlewares/oauth2/README.md`](./middlewares/oauth2/README.md) for full documentation.

### 🔔 Event System

The framework's internal communication layer lives in `events` — a typed,
reflection-free publish/subscribe bus that every subsystem (router, OAuth2,
dashboard, scheduler, plugins, WebSocket) uses to observe and extend
behaviour without importing each other.

- 🧩 Events are **plain Go structs** — no interfaces, no registration ceremony
- ⚡ Generic `On` / `Once` / `Off` / `Emit` with **zero reflection during dispatch**
- 🔒 Lock-free reads: registration publishes an immutable snapshot via `atomic.Pointer`
- 🎯 Deterministic order: before-hooks → listeners by descending priority → after-hooks
- 🛑 Flow control via `events.Stop`, `ctx.Cancel()`, and `Config.ContinueOnError`
- 🔁 Async emission — goroutine or worker-pool modes with overflow policies
- 🛡 Panic recovery, dispatch middleware, filters, and once-listeners
- 📈 Per-event metrics + optional ring-buffer recorder for debugging
- 🏗 Built-in framework events: application lifecycle, HTTP, WebSocket, OAuth2,
  routing, plugins, scheduler, and configuration reload

```go
import "github.com/nelthaarion/breeze/events"

// Events are just structs.
type UserCreated struct{ UserID uint64 }

// Listen — plain function, compiler-typed, no reflection.
events.On(UserCreated{}, func(ctx *events.Context, e UserCreated) error {
    return sendWelcomeEmail(e.UserID)
})

// Emit — listeners run in priority order.
events.Emit(UserCreated{UserID: 10})
```

See [`events/README.md`](./events/README.md) for full documentation.

### 🎬 Video Streaming

`video` serves a directory of media files with byte-range support, so
browsers can **seek**. A static file handler cannot do this: ignore the
`Range` header and the video still plays, which is what makes the bug
expensive to find — the scrubber is simply dead, because the browser cannot
ask for the middle of a file it is being handed sequentially.

- 🎯 `206 Partial Content` with correct `Content-Range`; `416` for
  well-formed but unsatisfiable ranges; a malformed one is ignored (as RFC 9110
  requires) and answered like a request with no `Range` at all — the first
  chunk, still `206`, so the player learns the full size and can seek. `200`
  appears only for an empty file
- 🧠 One pooled chunk in flight per response — 256 KiB by default, so ten
  thousand viewers cost ten thousand chunks, not ten thousand copies
- 🚧 Open-ended `Range: bytes=0-` capped at `MaxChunkSize` (4 MiB), so a
  player's opening request cannot pin an entire movie in memory
- 🛡 Traversal defence in a deliberate order: percent-decode first, reject
  NUL/backslash and any `..`, hide dotfiles, allow-list extensions, then
  prove containment against the symlink-resolved path
- 🕵️ Existence questions always answer **404, never 403**, so the filesystem
  cannot be mapped by watching which refusals differ
- 🔐 Optional signed, expiring URLs — verified *before* any disk access, so
  an unauthenticated flood costs no I/O
- ⚡ `ETag`/`Last-Modified` answered **before the file is opened**;
  `If-Range` honoured so resumed downloads cannot be corrupted
- 👁 Publishes `StreamServed`; a viewer closing the tab is reported as
  **cancelled, not failed**, and the dashboard's Video tab groups live
  traffic **by file** with throughput per stream

```go
import "github.com/nelthaarion/breeze/video"

if err := video.Mount(router, video.Config{Root: "./media"}); err != nil {
    log.Fatal(err)
}
```

That registers `GET` and `HEAD` on `/videos/*filepath`. To make every URL a
capability that expires, add a secret:

```go
video.Mount(router, video.Config{
    Root:   "./media",
    Secret: []byte(os.Getenv("VIDEO_SECRET")),
})

url := "/videos/movie.mp4?" + video.Sign(secret, "movie.mp4", 10*time.Minute)
```

See [`video/README.md`](./video/README.md) for full documentation.

### 🧬 Durable Workflows


`workflow` brings durable, in-process orchestration to Breeze: multi-step
business processes with retries, timeouts, parallelism, rollback (Saga)
and crash recovery — no broker, and no required database.

- 🧩 Declarative steps; `WithDependsOn` opts into a validated DAG with parallelism
- 🔁 Retries with exponential backoff + jitter; `NonRetryable` marks permanent failures
- ↩️ Automatic compensation: rollback runs in reverse over completed steps
- ⏱ Per-attempt and per-execution timeouts, cancellable retry backoff
- 💾 Optional `Store` for durability; `Resume` continues interrupted executions
  **without re-running completed steps**
- 🔑 Idempotency keys — safe with at-least-once event delivery
- 📡 Triggered by bus events via `OnType[UserRegistered](def)`
- 👁 Publishes `WorkflowStarted`/`StepFailed`/`CompensationStarted`/… events, and the
  dashboard shows **in-flight executions live**, step by step

```go
import "github.com/nelthaarion/breeze/workflow"

def := workflow.New("order-processing").
    Step("validate", ValidateOrder).
    Step("charge", ChargeCard, workflow.WithCompensation(RefundCard)).
    Step("ship", CreateShipment)

engine := workflow.NewEngine()
engine.Register(def)

res, err := engine.Run(ctx, "order-processing", order)
```

If `ship` fails, `RefundCard` runs automatically — the failure path is
declared next to the work, not scattered through error handling.

See [`workflow/README.md`](./workflow/README.md) for full documentation.

### 📊 Built-in Developer Dashboard


- 🔧 Native module under `/dashboard` (zero-overhead when disabled)
- 📈 Real-time overview: RPS, latency, memory, goroutines, CPU

- 🛣 Routes Explorer with per-route latency stats
- 🧪 API Explorer with multi-language code generation (curl / Go / JS / Python / C# / PHP)
- 📡 Live Requests feed with WebSocket push
- 🎬 Video monitor — live streams grouped **by file**, with throughput,
  range/seek counts and abandoned transfers
- 🗄 Database Browser (paginated; optional inline Create/Update/Delete via `DBWriter`)

- 🔍 ORM Query Monitor with slow-query detection
- 💾 Cache, Queue, and Scheduler monitors
- 📝 Logs with five tabs (App / HTTP / Errors / Panics / Warnings)
- ❤️ Health checks with green / yellow / red indicators
- ⚡ Go runtime performance metrics with charts
- 🕒 Developer Timeline — per-request profiler with expandable steps
- 🔒 HTTP Basic Auth + secret masking (Authorization, Cookie, API keys…)
- 🌑 Modern dark mode, responsive, single-file SPA (no external deps)

See [`dashboard/README.md`](./dashboard/README.md) for full documentation.

### ⚙️ Developer Experience

- 📦 Lightweight architecture
- 🎨 JSON responses out of the box
- 📄 Template rendering
- 📁 Static assets
- 🔍 Request validation
- 🧩 Simple Context API

### 🧠 Performance Optimizations

- Inline handler execution on the event loop (no dispatch hop)
- Zero-copy body handling
- No per-event copy of the request bytes
- In-place header-key lowercasing (no allocation per header)
- Optional zero-copy headers — zero framework allocations per request
- WebSocket lookup gated on a counter, so HTTP requests never probe the map
- Header reuse
- Copy-on-write headers
- Cached HTTP status text and full status lines
- Unsafe string conversions
- Compact receive buffers
- Optimized HTTP parser
- Single-pass header parsing
- Pooled requests, responses, params, and wire buffers
- Exact-path route map per HTTP method
- Cache-line padding on hot atomic counters
- Reduced GC pressure

---


## 🤝 Contributing

We welcome contributions of all sizes.

Whether it's fixing bugs, improving documentation, optimizing
performance, or adding new features — every contribution helps make
Breeze better.

1. **Fork** the repository
2. **Create** a feature branch (`git checkout -b feat/my-thing`)
3. **Commit** your changes with a clear message
4. **Open** a pull request describing what and why

Please open an issue first for non-trivial changes so we can align on
the approach before you spend time on code.

## 🔐 Security Scanning

Breeze now includes automated security checks in GitHub Actions:

- **CodeQL** static analysis (`.github/workflows/codeql.yml`)
- **govulncheck** vulnerability scanning for Go packages and reachable code (`.github/workflows/govulncheck.yml`)
- **Gitleaks** secret scanning (`.github/workflows/secret-scan.yml`)
- **Dependabot** weekly updates for Go modules and GitHub Actions (`.github/dependabot.yml`)

For repository admins:

- Enable GitHub Advanced Security **secret scanning** and **push protection** in repository settings when available.
- Configure branch protection to require the three security workflow checks before merge.
- Use the triage process in `.github/SECURITY_TRIAGE.md` to classify and resolve alerts.

## License

Breeze is released under the [MIT License](./LICENSE).

© Nelthaarion
