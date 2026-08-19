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

Breeze ships a `rails`-style CLI for scaffolding projects and generating
CRUD boilerplate.

```bash
go install github.com/nelthaarion/breeze/cmd/breeze@latest
```

**Start a new project:**

```bash
breeze new myapp                    # minimal REST API layout (default)
breeze new myapp --template=views   # + views/components/template engine
```

**Generate a full CRUD resource** — structs, handlers, an in-memory store,
and OpenAPI docs, wired into the router automatically:

```bash
breeze generate resource User name:string email:string age:int
```

**Generate a bare handler stub** (no structs, no docs):

```bash
breeze generate handler Session --methods=get,create
```

**Generate gRPC server/client scaffolding** from an interface declared in
any `*_grpc.go` file — no naming convention on methods, just a
`grpc_type` comment (`Unary`, `ServerSideStreaming`, `ClientSideStreaming`,
or `Bidirectional`) above each method:

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

Supported field types: `string`, `int`, `int64`, `float64`, `bool`, `time.Time`.

## Features

### 🚀 Built for Extreme Performance

- ⚡ Event-driven architecture powered by `gnet`
- 🧠 Zero-copy HTTP request parsing where possible
- 📦 Minimal allocations via pooled `Context`, `HTTPResponse`, and route
  parameter maps (`sync.Pool`)
- 🔥 Optimized response serialization (precomputed status-line table,
  preallocated buffer, no `fmt.Sprintf`)
- 🧵 Configurable Worker Pool backpressure (`OverflowBlock` /
  `OverflowReject` / `OverflowSpawn`)
- 🌲 Single-allocation middleware chain construction in the router
- 💨 Lock-free fast paths for critical operations
- 🎯 Preallocated buffers & cached status codes
- 📈 Worker Pool for scalable request processing

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
  unsatisfiable ranges, `200` for malformed ones (as RFC 9110 requires)
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

- Zero-copy body handling
- Header reuse
- Copy-on-write headers
- Cached HTTP status text
- Unsafe string conversions
- Compact receive buffers
- Optimized HTTP parser
- Single-pass header parsing
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
