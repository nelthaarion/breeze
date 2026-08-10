# Breeze — Changelog

All changes made to the Breeze framework.

---

## [v1.6.0] — Event System & Workflow Orchestration

### New Features

#### Event System (`events`)

A new internal communication layer for the framework: a typed,
allocation-conscious publish/subscribe system that lets any subsystem
(router, middleware, OAuth2, plugins, dashboard, scheduler, websocket)
observe and extend behaviour without importing each other. Zero external
dependencies.

- **Events are plain structs** — no interface, no embedding, no
  registration step. `type UserCreated struct { UserID uint64 }` is a
  complete event.
- **Generic, reflection-free dispatch**: `On`, `Once`, `Off`, `Before`,
  `After`, `Emit`. `reflect.Type` is used only as a map key to identify
  an event type — never to read fields, build values, or call handlers.
  A dispatch resolves to a strongly typed slice via one type assertion
  and calls each handler through a direct func value.
- **Two forms of every operation**: a package-level function on the
  `Default` bus, plus an explicit-bus form (`events.Emit(e)` /
  `events.EmitBus(myBus, e)`) for applications that want isolation. Go
  forbids generic methods, which is why the typed operations are
  functions taking a `*Bus`. Type-explicit variants (`OnType[T]`,
  `OnceTypeBus[T]`, …) are available where inference is unwanted.
- **Lock-free dispatch**: registration takes a mutex and publishes an
  immutable snapshot via `atomic.Pointer`; dispatch does a single atomic
  load and takes no lock. A listener added mid-dispatch does not affect
  the dispatch already in flight.
- **Ordering computed at registration, never during `Emit`**:
  before-hooks, then listeners by descending priority, then after-hooks.
  `Subscription.Priority` / `.Where` / `.Unsubscribe` configure a
  listener through a chainable handle.
- **Flow control**: returning `events.Stop` halts propagation;
  `ctx.Cancel()` stops the remaining listeners; `Config.ContinueOnError`
  chooses between failing fast and aggregating into a `*MultiError`.
- **Async emission** — `EmitAsync`, `EmitAsyncWait`, `EmitAsyncCtx` —
  with a selectable `AsyncMode` (`AsyncGoroutine` or `AsyncWorkerPool`),
  configurable `Workers` / `QueueSize`, and an `AsyncOverflow` policy
  (`OverflowBlock`, `OverflowSpawn`, `OverflowDrop`) for a full queue.
- **Panic recovery** with a configurable `PanicMode` and `PanicHandler`,
  so a panicking listener cannot take down the application.
- **Middleware** around every dispatch via `Use(...Middleware)`.
- **Introspection for the dashboard**: `List`, `CountEvents`,
  `CountListeners`, `HasEvent`, `SetName`/`GetName`, `Inspect` /
  `InspectAll` (registered listeners, priorities, execution order) and
  per-event `Metrics` (dispatch, listener, failure, panic, stopped and
  filtered counts; total/average/min/max duration; last dispatch time),
  per event type or aggregated for the bus via `TotalMetrics`.
- **Optional ring-buffer recorder** (`EnableRecorder`,
  `EnableRecorderWithPayload`, `History`, `DisableRecorder`) for
  debugging recent dispatches, off by default.
- **Built-in framework events** covering application lifecycle, router,
  HTTP, authentication, OAuth, plugins, websocket, scheduler and
  configuration reload, so modules can hook framework internals without
  patching them.
- **Tested**: unit, concurrency and race coverage for concurrent
  registration and dispatch, priorities, filters, once listeners,
  cancellation, panic recovery, middleware and the recorder. Passes
  `go test -race`; `go vet` clean; benchmarks included. Coverage 97.1%.

See `events/README.md` for full documentation.

#### Workflow Orchestration (`workflow`)

Durable, in-process orchestration for multi-step business processes,
with no broker and no required database.

- **Declarative steps with a validated DAG** — sequential by default;
  `WithDependsOn` opts into parallelism. Cycles, duplicate names,
  unknown dependencies and nil handlers are rejected at registration,
  and the graph is topologically sorted once rather than per execution.
- **Retries** with `BackoffExponential` and jitter, plus `NonRetryable`
  to end a doomed attempt sequence early while preserving the original
  error's identity under `errors.Is`.
- **Compensation (Saga)**: rollback runs in reverse over the steps that
  actually succeeded. A failing compensation handler does not stop the
  others — the remaining side effects still need undoing — and emits
  `WorkflowCompensationFailed`.
- **Timeouts and cancellation** per attempt and per execution, including
  cancellable retry backoff so shutdown never waits out a long delay.
- **Event triggers**: `workflow.OnType[UserRegistered](def)` starts an
  execution from a bus event, asynchronously, without blocking the
  emitter.
- **Durability and resume**: a nine-method `Store` interface with no
  driver or ORM behind it (`MemoryStore` by default). `Resume` continues
  interrupted executions and does not re-run completed steps.
- **Idempotency keys**, which is what makes at-least-once event delivery
  safe to trigger workflows with.
- **Observability**: every execution publishes framework events and one
  observability signal carrying each step as a span, so workflows render
  through the same dashboard pipeline as HTTP requests with no
  workflow-specific code.
- **Tested**: `go test -race` clean, `go vet` clean, benchmarks
  included. Coverage 95.3%.

See `workflow/README.md` for full documentation.

#### Live workflow view (dashboard)

The observability signal for an execution is written when it *ends*, so
it can only describe history — a workflow that is retrying with backoff
or waiting on a slow dependency is exactly the one worth watching, and
the signal cannot show it.

- The dashboard now assembles an in-flight view from step events as they
  arrive, exposed as `live` in the `/events` API payload. No
  configuration: attaching the dashboard to a bus is enough.
- Every step of the plan is present from the first frame, so the whole
  chain is visible immediately instead of materialising node by node.
  A step is `pending`, `running`, `done`, `failed`, `retrying` or
  `compensated`.
- A retryable failure reads as `retrying`, not `failed` — a step has no
  verdict until its final attempt. After a successful rollback,
  completed steps become `compensated` rather than staying `done`, so
  the UI never claims an effect that has since been undone.
- In-flight state is kept out of the observability ring buffer, where a
  half-finished execution would masquerade as history. Tracking is
  bounded (64 executions, finished ones evicted first) and finished
  executions are swept ~30s after they end.

---

## [v1.5.0] — OAuth2 / Social Login

### New Features

#### OAuth2 Authentication (`middlewares/oauth2`)

A new, zero-config OAuth2 / OpenID Connect middleware package that adds
social login with only `ClientID` and `ClientSecret` required — every other
setting has a secure default.

- **Four providers out of the box**: Google, GitHub, Microsoft, and Discord.
  Each is a self-registering `ProviderDriver` (via `init()`) that normalizes
  the provider's profile into a common `User` (stable `ID`, `Email`, `Name`,
  `Username`, `Avatar`, `Provider`) and handles its own quirks — Google
  offline-access consent, GitHub private-email fallback via `/user/emails` +
  numeric id, Microsoft `common` tenant with scope echo on the token request,
  and Discord avatar-hash → CDN URL expansion.
- **Six composable middlewares**: `Login`, `Callback`, `Auth`, `Optional`,
  `Refresh`, and `Logout`, plus context accessors `CurrentUser`,
  `CurrentToken`, `IsAuthenticated` (with `UserFrom` / `TokenFrom` aliases).
- **Secure by default**:
  - **PKCE** (S256) enabled for every provider.
  - **CSRF protection** — the `state` is stored in a signed, short-lived,
    single-use cookie and compared in constant time; the flow cookie is
    cleared on the callback so a state can never be replayed.
  - **Signed, HttpOnly, `SameSite=Lax` cookies**; `Secure` auto-enables for
    `https://` origins.
  - **Open-redirect guard** — `?redirect=` overrides accept same-site paths
    only (absolute and protocol-relative URLs are rejected).
  - **Algorithm-pinned HS256 JWTs** to prevent `alg=none` / algorithm-confusion
    attacks.
  - **Bounded provider calls** — every outbound token/userinfo request runs
    under a timeout so a slow provider can't pin a worker goroutine.
- **Two session modes** (`Config.SessionMode`): `SessionModeCookie` (default,
  signed HMAC cookie) and `SessionModeJWT`. Every write **rotates** the
  session so a pre-login cookie can't be reused post-login.
- **Transparent token refresh** — the `Refresh` middleware renews an expiring
  access token using the stored refresh token and rotates the session cookie,
  layered in front of `Auth`.
- **Shared pooled HTTP client** across all provider calls for connection reuse,
  overridable via `Config.HTTPClient` (injected in tests).
- **Tested**: the suite drives the full `Login → Callback → Auth` flow against
  an in-process mock provider, plus PKCE, cookie signing, JWT tamper-rejection,
  state expiry, config defaults, open-redirect protection, and concurrent
  registry access. Passes `go test -race`; `go vet` clean; benchmarks included.

See `middlewares/oauth2/README.md` for full documentation.

---

## [v.1.4.1] — Performance Improvements & gRPC Codegen


### New Features

#### gRPC Code Generation (`breeze generate grpc`)

A new CLI generator scans the project for `_grpc.go` files, treats every
interface declared in them as a gRPC service, and generates the
corresponding server/client scaffolding, adapters, and boilerplate:

- Each method's call style is driven by a `grpc_type` comment annotation
  (`Unary`, `ServerSideStreaming`, `ClientSideStreaming`, `Bidirectional`)
  rather than a naming convention.
- New CLI usage: `breeze generate grpc <InterfaceName> [--force]`.
- Implemented across five new files: `generate_grpc.go` (detection +
  parsing), `generate_grpc_adapters.go`, `generate_grpc_files.go`,
  `generate_grpc_tags.go`, and a matching test suite
  (`generate_grpc_test.go`).

#### Editable Database Browser (dashboard)

The dashboard's Database Browser can now perform writes, not just reads:

- New `DBWriter` interface (kept separate from `DBInspector` so existing
  read-only integrations don't break) with `InsertRow` / `UpdateRow` /
  `DeleteRow`, plus a sentinel `ErrRowNotFound` error.
- New `Config.AllowWrites` gate — defaults to `false`, so writes must be
  explicitly opted into.
- New HTTP endpoints: `POST /rows`, `PUT /rows/:pk`, `DELETE /rows/:pk`,
  with support for composite primary keys via a new `parsePK` helper.
- Table cache is invalidated automatically after any write.
- `TableData` now reports a `writable` flag so the UI can conditionally
  show edit controls.
- Inline edit / delete / new-row UI added to the Database Browser
  frontend.
- NULL cells are no longer overwritten with an empty string when a cell
  is blurred without being edited.

### Performance Improvements

- **Worker pool overhaul** (`workerpool.go`, new `pool.go`): the pool now
  supports a configurable `OverflowPolicy` (`OverflowBlock`,
  `OverflowReject`, `OverflowSpawn`) instead of always spawning a
  goroutine on backpressure, plus a two-phase shutdown to avoid
  send-on-closed-channel races. `*Context`, `*HTTPResponse`, and route
  parameter maps are now pooled via `sync.Pool` (see `pool.go`) to cut
  per-request allocations.
- **Router**: `Router.findRoute` builds the middleware chain (global +
  route middlewares + handler) in a single allocation instead of the
  previous double-build through a `finalHandler` closure, and route
  parameter maps are drawn from a pool instead of freshly allocated on
  every match.
- **Response serialization**: HTTP status lines are now generated from a
  precomputed lookup table instead of per-request string building, and
  the response buffer is preallocated using an estimated size instead of
  growing incrementally.
- **Context**: response header maps are now shared/copy-on-write
  (`headersShared`) to avoid an allocation when a handler never mutates
  headers, and header/body writes (`WriteString`, `JSON`, `HTML`,
  `Status`) route through a shared `ensureResponse` helper. Added
  `ctx.GetHeader`.
- **Compression middleware**: broader rework of the post-`Next()`
  pipeline (building on the ordering fix from the previous release) to
  reduce redundant work on the hot path.

### Bug Fixes

- **`dashboard/auth.go`** — Dashboard Basic Auth / login previously used a
  random per-call salt when hashing the password, so a PBKDF2 hash of a
  correct password could never match the stored hash and valid
  credentials were always rejected. Fixed by hashing against a fixed,
  package-level salt (fixes #27).
- **`dashboard/mask.go`** — Secret masking only matched plain
  `key=value` / `key:value` tokens, so JSON-style log lines such as
  `"authorization":"Bearer xyz"` were never redacted and could leak into
  the dashboard's log buffer. `maskLine` now recognizes quoted JSON keys
  and values and redacts them while preserving surrounding JSON
  punctuation (fixes #28).

### Documentation / Housekeeping

- Removed the "Support the Project" donation section from `README.md`.
- Added design spec and implementation plan docs for the editable
  Database Browser feature (later removed from the tree — see "Removed
  docs" commit).
- Documented `DBWriter` and the editable Database Browser in
  `dashboard/README.md`.

---

## [v1.3.1]

### New Features

### Developer Dashboard (`/dashboard`)

A production-grade, native developer dashboard inspired by Laravel Telescope,
Horizon, and Grafana. Designed specifically for Breeze.

**Pages (13 total):**

- Overview — real-time cards (RPS, latency, error rate, goroutines, heap,
  CPU, cache hit ratio, queue jobs) + 4 live charts
- Routes Explorer — every registered route with per-route latency stats
- API Explorer — native API client (no Scalar redirect) with one-click
  code generation in curl / Go / JavaScript / Python / C# / PHP
- Live Requests — every incoming request with method/status/route/user
  filters, slow-request highlighting
- Database Browser — paginated table browser with column metadata and
  foreign-key relationships (read-only)
- ORM Query Monitor — every SQL with args, duration, rows, file:line,
  slow-query highlighting, expandable rows
- Cache Monitor — driver, keys, hits/misses, hit rate, clear/cache-prefix
- Queue Monitor — pending/running/completed/failed with retry button
- Scheduler — task name, cron, last/next run, status, run/fail counts
- Logs — five tabs (App / HTTP / Errors / Panics / Warnings) with search
- Health — configurable probes with green/yellow/red indicators
- Performance — Go runtime metrics (goroutines, GC, heap, stack, CPU,
  network) with 4 live charts
- Developer Timeline — per-request hierarchical profiler with
  expandable steps and metadata (the headline feature)

**Architecture:**

- Single WebSocket connection multiplexes all live updates (no polling)
- 1Hz metrics sampler pushes runtime stats into a 10-minute ring buffer
- Self-contained SPA: HTML + CSS + JS inlined into a single response
  (zero external dependencies, zero asset pipeline)
- Custom canvas charts (no Chart.js / D3 / npm)
- HTTP Basic Auth with constant-time password comparison (SHA-256 +
  subtle.ConstantTimeCompare)
- Secret masking for Authorization, Cookie, API-Key, Token, Password headers
  and key=value patterns in log lines
- Zero-overhead fast path: when enabled: false, the middleware returns
  immediately after ctx.Next() — no allocations, no locks
- Ring buffers bound memory for every collector (requests, queries, logs,
  timelines, metrics)

**Configuration:**

```yaml
dashboard:
  enabled: true
  timeline: true
  queries: true
  metrics: true
  requests: true
  base_path: "/dashboard"
  username: "admin"
  password: "s3cret"
  max_requests: 1000
  max_queries: 500
  max_logs: 1000
  slow_query_ms: 100
  slow_request_ms: 500
```

**Installation:**

```go
coll := dashboard.Install(app, router, dashboard.DefaultConfig())
router.Use(coll.Middleware())
```

See `dashboard/README.md` for full documentation.

---

## Bug Fixes

### 1. `types.go` — HTTP Method Typo

**Bug:** `OPTION Method = "OPTION"` — missing the trailing `S`.
RFC 9110 defines the method as `OPTIONS` (7 characters).

**Impact:** All CORS preflight requests (`OPTIONS /path`) failed to match
the constant, causing 404s on every cross-origin browser request.

**Fix:** `OPTIONS Method = "OPTIONS"`

---

### 2. `request.go` — `internMethod` Never Matched OPTIONS

**Bug:** `internMethod` had a case-6 branch checking for `"OPTION"` (6 bytes),
which is not a real HTTP method. Real OPTIONS requests are 7 bytes
(`"OPTIONS"`), so they fell through to `Method(string(b))` — an allocation.
Worse, the returned `Method("OPTIONS")` never matched the `OPTION` constant.

**Fix:** Removed the 6-byte branch. Added a case-7 branch using the same
zero-alloc byte-comparison pattern:

```go
case 7:
    if b[0] == 'O' && b[1] == 'P' && b[2] == 'T' &&
       b[3] == 'I' && b[4] == 'O' && b[5] == 'N' && b[6] == 'S' {
        return OPTIONS
    }
```

---

### 3. `websocket_engine.go` — Use-After-Put in Close Frame

**Bug:** In the `wsOpClose` handler:

```go
code, reason := parseClosePayload(frame.payload)
wsFramePool.Put(frame)                                    // returned to pool
echo := buildWSFrame(wsOpClose, frame.payload)            // reads stale data!
```

After `wsFramePool.Put(frame)`, another goroutine calling `parseWSFrame`
could grab the same `*wsFrame` and overwrite `frame.payload`. The subsequent
`buildWSFrame` call would read corrupted data — a use-after-free in pooled
memory.

**Fix:** Reorder — use `frame.payload` first, then return to pool:

```go
code, reason := parseClosePayload(frame.payload)
echo := buildWSFrame(wsOpClose, frame.payload)
wsFramePool.Put(frame)
```

---

### 3b. `websocket.go` — RFC 6455 Control Frame Validation

**Bug:** `parseWSFrame` did not enforce RFC 6455 §5.5 requirements for
control frames (Close, Ping, Pong):

1. Control frames MUST have a payload ≤ 125 bytes (no extended length
   encoding allowed).
2. Control frames MUST NOT be fragmented (FIN must be 1).

A malicious client could send an oversized or fragmented control frame,
which the parser would accept — potentially causing excessive memory
allocation or confusing the defragmentation logic.

**Fix:** Added validation early in `parseWSFrame`, after the opcode and
initial payload length are parsed:

```go
isControl := opcode >= wsOpClose
if isControl {
    if payLen > wsMaxControlPayload {
        return nil, -1 // control frame payload exceeds 125 bytes
    }
    if !fin {
        return nil, -1 // control frames must not be fragmented
    }
}
```

Also added a defensive invariant check using `wsMaxFrameHeader` (14 bytes =
2 + 8 + 4) to validate the parsed header size never exceeds the maximum:

```go
if offset > wsMaxFrameHeader {
    return nil, -1
}
```

This also silences the `unusedfunc` warnings for `wsMaxControlPayload` and
`wsMaxFrameHeader` — both constants are now referenced in the parser.

---

### 4. `context.go` — Added Typed Store (Set/Get/MustGet)

**Why:** Needed for the JWT fix (#8 below). The existing `params` field is
`map[string]string`, which can't hold structured data like `jwt.MapClaims`.

**Added:**
```go
type Context struct {
    // ... existing fields ...
    store map[string]any  // lazy-initialized, nil until first Set
}

func (ctx *Context) Set(key string, val any)
func (ctx *Context) Get(key string) (any, bool)
func (ctx *Context) MustGet(key string) any
```

**Performance:** The `store` field is `nil` until the first `Set` call —
zero allocation for requests that don't use it. Same pattern as Gin/Echo/Fiber.

---

### 5. `middlewares/compression.go` — Pre-Next Ordering Bug

**Bug:** The middleware checked `ctx.Res` **before** calling `ctx.Next()`.
At that point `ctx.Res` is always `nil` (handler hasn't run), so the
middleware short-circuited and **compression never ran** — the entire
feature was dead code.

**Fix:** Call `ctx.Next()` first, then post-process the response.

**Additional improvements:**
- Early-return on empty `Accept-Encoding`
- Early-return if `Content-Encoding` is already set (prevent double-compress)
- Added `Vary: Accept-Encoding` header for proper cache behavior
- Properly check `Close()` return value

---

### 6. `middlewares/cache.go` — ETag Ordering + Query Key Collision

**Bug 1 (ordering):** Same as compression — checked `ctx.Res` before
`ctx.Next()`, so ETag generation never ran.

**Bug 2 (key collision):** The cache key was `ctx.Req.Path` only, so
`/api/users?page=1` and `/api/users?page=2` shared the same ETag entry,
causing false 304s.

**Fix:**
- Call `ctx.Next()` first, then compute ETag from the fresh response body
- Include query string in the cache key (only allocates when a query exists)
- Use `RLock` for the If-None-Match pre-check (concurrent 304 checks)
- Pre-check: skip the handler entirely on a known ETag match

---

### 7. `middlewares/cors.go` — Missing Abort() on OPTIONS

**Bug:** On OPTIONS preflight, the middleware called `return` without
`ctx.Abort()`, leaving `ctx.index` at its current position. If any code
later called `ctx.Next()` on the same context, the chain would resume past
the CORS short-circuit.

**Fix:**
```go
if ctx.Req.Method == breeze.OPTIONS {
    ctx.Status(204)
    ctx.Abort()
    return
}
```

---

### 8. `middlewares/rate_limiter.go` — Lock Held Across Next()

**Bug (critical performance):** The middleware held `mu.Lock()` with
`defer rl.mu.Unlock()` across `ctx.Next()`:

```go
rl.mu.Lock()
defer rl.mu.Unlock()
// ... counter update ...
ctx.Next()  // ← handler runs under lock!
```

This serialized **every request** through a single mutex, completely
defeating the WorkerPool's concurrency. A 16-core server would process
requests one at a time.

**Fix:**
- Do the map lookup + counter update under the lock
- Release the lock before `ctx.Next()`
- Pre-compute the 429 message at construction time (avoid `fmt.Sprintf`
  on every rejected request)

**Impact:** Before — lock held for entire handler duration (ms to seconds).
After — lock held for map ops only (microseconds).

---

### 9. `middlewares/jwt.go` — Claims Stored as Unparseable String

**Bug:** The middleware stored JWT claims via:

```go
ctx.SetParam(opts.UserContextKey, fmt.Sprintf("%v", claims))
```

`fmt.Sprintf("%v", map[string]any{...})` produces a Go-specific
representation like `map[exp:1234 role:admin user_id:42]`. Downstream
handlers could not parse this back into structured data.

**Fix:** Use the new typed store:

```go
ctx.Set(opts.UserContextKey, claims)
```

Handlers retrieve claims with a type assertion:

```go
claims, ok := ctx.Get("user").(jwt.MapClaims)
```

---

## File Inventory

This package contains the **complete** framework — every file is either the
original unchanged source, or a bug-fix version. Replace your entire `breeze/`
directory with these files to avoid any mixing of versions.

```
breeze-final/
├── CHANGELOG.md
├── breeze.go               ← ORIGINAL (unchanged)
├── types.go                ← BUG FIX: OPTIONS method
├── request.go              ← BUG FIX: internMethod case 7
├── context.go              ← BUG FIX: typed Set/Get store
├── response.go             ← ORIGINAL (unchanged)
├── router.go               ← ORIGINAL (unchanged)
├── router_static.go        ← ORIGINAL (unchanged)
├── workerpool.go           ← ORIGINAL (unchanged)
├── websocket.go            ← BUG FIX: RFC 6455 control frame validation
├── websocket_engine.go     ← BUG FIX: use-after-Put
├── file.go                 ← ORIGINAL (unchanged)
├── template.go             ← ORIGINAL (unchanged)
└── middlewares/
    ├── compression.go      ← BUG FIX: post-Next ordering
    ├── cache.go            ← BUG FIX: post-Next + query key + RLock
    ├── cors.go             ← BUG FIX: Abort() on OPTIONS
    ├── rate_limiter.go     ← BUG FIX: lock released before Next()
    └── jwt.go              ← BUG FIX: typed claims storage
```

**Bug-fixed files (10):** `types.go`, `request.go`, `context.go`,
`websocket.go`, `websocket_engine.go`, and all 5 middlewares.

**Original files (7):** `breeze.go`, `response.go`, `router.go`,
`router_static.go`, `workerpool.go`, `file.go`, `template.go`.
