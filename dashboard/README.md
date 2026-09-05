# Breeze Developer Dashboard

A native, production-grade developer dashboard for the Breeze framework — inspired by Laravel Telescope, Horizon, and Grafana, but designed specifically for Breeze.

The dashboard ships as a self-contained module under ` github.com/nelthaarion/breeze/v2/dashboard`. It adds **zero runtime overhead when disabled** and exposes a single function (`dashboard.Install`) for wiring.

---

## Quick Start

```go
package main

import (
    "runtime"
    "github.com/nelthaarion/breeze/v2"
    "github.com/nelthaarion/breeze/v2/dashboard"
)

func main() {
    router := breeze.NewRouter()
    pool   := breeze.NewWorkerPool(runtime.NumCPU())
    app    := breeze.New(router, pool)

    // 1. Install the dashboard (registers /dashboard, /dashboard/api/*, /dashboard/ws)
    coll := dashboard.Install(app, router, dashboard.DefaultConfig())

    // 2. Install the instrumentation middleware (must come before your routes)
    router.Use(coll.Middleware())

    // 3. Register application routes as usual
    router.Handle(breeze.GET, "/api/users", listUsers)

    app.Run(3000, true)
}
```

Visit `http://localhost:3000/dashboard` (default credentials `admin` / `admin`).

A fully runnable example is at `cmd/dashboard-example/main.go`.

---

## Features

The dashboard provides **14 pages** grouped into Monitor / Develop / System sections. Thirteen are always present; the Video page appears once you attach a video bus.


### Monitor

#### 1. Overview
Real-time cards for: Requests Today, Requests/sec, Avg Response Time, Error Rate, Active Sessions, DB Connections, Cache Hit Ratio, Queue Jobs, Goroutines, Heap Allocation, Memory System, CPU Usage. Plus four live charts (RPS, Latency, Memory, Goroutines) updated over WebSocket at 1 Hz.

#### 2. Live Requests
Every incoming request is captured and displayed with: Time, Method, Path, Status, Duration, IP, User, Response Size, and a link to the per-request timeline. Filters by Method / Status / Route / User. Slow requests (>500ms) are highlighted in red.

#### 3. Developer Timeline (the headline feature)
Each request generates a hierarchical timeline showing every phase: Request Received → Middleware → Authentication → Cache → ORM Query → Controller → JSON Serialization → Response. Each step shows start/end timestamps, duration, and arbitrary metadata. Slow steps (>100ms) are flagged.

#### 4. Performance
Detailed Go runtime metrics: Goroutines, Heap Alloc, Heap Sys, Stack In Use, GC Count, GC Pause, Mallocs, Frees, CPU Usage, Mem Sys, Mem Usage %, CGO Calls. Four charts (Heap, Goroutines, CPU, GC Pauses) update every second.

### Develop

#### 5. Routes Explorer
Every registered route with Method, Path, Requests, Avg Latency, Max Latency, Last Request, and Errors. Search by path or method.

Routes wrapped in `scalar.Doc` also carry their own summary, description and tags, joined from the OpenAPI registry when the page is read. A route with no documentation is marked `documented: false` — which matters beyond presentation, because a route absent from the registry is also absent from the generated OpenAPI document, and therefore invisible to every client generator and every agent reading the spec.

#### 6. API Explorer
A native API client built into the dashboard (no Scalar redirect). Select any registered endpoint, set headers/body, and execute. The response is pretty-printed and one-click copy buttons generate ready-to-run snippets in **curl, Go, JavaScript, Python, C#, and PHP**.

Documented routes get real request forms: body, query, path-parameter and header groups are derived from the route's `RouteDoc`, with each field's type and its `description:"…"` struct tag. Undocumented routes fall back to path parameters inferred from the pattern, typed as string.

**The target is this service only.** The explorer sends its request to `127.0.0.1` on the port the application is listening on; a relative path is resolved against it, and an absolute URL is accepted only when it names that same origin. Redirects are returned rather than followed.

This is a hard constraint rather than a convenience. A request the server makes comes from inside the deployment, so an unrestricted URL field would let anyone who reaches this endpoint read the cloud metadata service, any internal hostname the container can resolve, and every other port on the host — none of which they could reach directly. The dashboard's Basic Auth is not a sufficient gate for that, because it is disabled entirely when `Username` or `Password` is empty.

#### 7. Database Browser
Browse every table with pagination, search, and column metadata (type, nullable, primary key, index, defaults, foreign-key references). Read-only unless both `Config.AllowWrites` is `true` and the application has called `Collector.SetDBWriter` with a `DBWriter` implementation — in which case rows can be created, edited inline, and deleted directly from the browser. See [DBWriter](#dbwriter-optional-crud) below.

#### 8. ORM Query Monitor
Every SQL statement executed by the ORM is captured with: SQL, Args, Duration, Rows Returned, File:Line, Error. Slow queries (>100ms configurable) are highlighted. Click a row to expand the full SQL and arguments.

### System

#### 9. Cache Monitor
Driver, Keys, Hits, Misses, Memory, Hit Rate (with chart). Buttons to Clear Cache or Clear by Prefix.

#### 10. Queue Monitor
Pending / Running / Completed / Failed jobs. Failed jobs have a Retry button.

#### 11. Scheduler
Every scheduled task with Name, Cron, Last Run, Duration, Next Run, Status, Run Count, Failure Count.

#### 12. Logs
Five tabs (Application / HTTP / Errors / Panics / Warnings), each with full-text search.

#### 13. Health
Configurable probes for Database, Redis, Cache, Storage, Queue, Mail — anything you register. Green/Yellow/Red indicators with latency.

#### 14. Video (opt-in)
Live streams grouped **by file** — throughput, seeks, disconnects and errors per title, with totals for bandwidth and bytes served. Requires one call, `coll.AttachVideo(bus)`, so a dashboard with no media allocates nothing.

Streaming does not fit the Live Requests feed: one viewer emits hundreds of range requests for a single file, so that page shows the same filename over and over, interleaved with unrelated traffic — and it cannot report throughput at all, because bandwidth belongs to a stream rather than to any one request.

---


## Configuration

All configuration is in YAML or via Go struct:

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
  max_requests: 1000       # rolling window
  max_queries: 500
  max_logs: 1000
  max_timeline_entries: 256
  slow_query_ms: 100
  slow_request_ms: 500
  masked_headers:
    - authorization
    - cookie
    - x-api-key
```

| Field | Default | Description |
|-------|---------|-------------|
| `enabled` | `true` | Master switch. When `false`, the middleware short-circuits before any work — zero overhead. |
| `timeline` | `true` | Per-request timeline profiler. |
| `queries` | `true` | ORM/SQL query capture. |
| `metrics` | `true` | Go runtime metrics (GC, heap, goroutines). |
| `requests` | `true` | Live request tracking. |
| `base_path` | `/dashboard` | URL prefix for the SPA and API. |
| `username` / `password` | `admin` / `admin` | HTTP Basic Auth credentials. Both must be non-empty for auth to be enforced. |
| `max_requests` | `1000` | Rolling window size for the live requests buffer. |
| `max_queries` | `500` | Rolling window size for the query monitor. |
| `max_logs` | `1000` | Per-tab log buffer size. |
| `max_timeline_entries` | `256` | Cap on timeline steps per request. |
| `slow_query_ms` | `100` | Threshold for highlighting slow queries. |
| `slow_request_ms` | `500` | Threshold for highlighting slow requests. |
| `masked_headers` | sensible defaults | Header names (case-insensitive) whose values are redacted in the inspector. |

---

## DBWriter (optional CRUD)

By default the Database Browser is read-only. To enable Create/Update/Delete:

```go
coll.SetDBInspector(store) // existing read-only interface
coll.SetDBWriter(store)    // NEW — enables writes
cfg.AllowWrites = true     // NEW — must also be explicitly enabled
```

`DBWriter` is a separate interface from `DBInspector` so existing read-only
integrations are unaffected:

```go
type DBWriter interface {
    InsertRow(table string, values map[string]any) (map[string]any, error)
    UpdateRow(table string, pk map[string]any, values map[string]any) error
    DeleteRow(table string, pk map[string]any) error
}
```

Both `Config.AllowWrites` and a configured `DBWriter` are required — either
one alone leaves the browser read-only. See `cmd/dashboard-example/main.go`
for a full `UserStore` implementation of both interfaces.

---

## Real-Time Updates

The dashboard uses a single WebSocket connection at `/dashboard/ws` for all live updates. The server pushes two kinds of messages:

- **`snapshot`** — full overview metrics, sent every second.
- **`event`** — a single live record (request / query / timeline) pushed the moment it is recorded.

No polling. The SPA reconnects automatically on disconnect with a 2-second backoff.

---

## Security

- **Authentication**: HTTP Basic Auth with constant-time password comparison (SHA-256 + `subtle.ConstantTimeCompare`).
- **Secret Masking**: Authorization, Cookie, API-Key, Token, and Password headers are masked in the request inspector. The `maskLine` helper also redacts `key=value`-style secrets in log messages.
- **Zero Overhead When Disabled**: When `enabled: false`, the middleware returns immediately after `ctx.Next()` — no allocations, no locks.

---

## Pushing Application Data

The Collector is your handle for pushing data from anywhere in your application:

```go
// Record an ORM query (call from your database adapter)
coll.PushQuery("SELECT * FROM users WHERE id = $1", []any{42}, 850, 1, "models/user.go", 42, nil)

// Record a log entry
coll.PushLog("app", "user 42 logged in", "auth/handler.go:88")

// Register a queue job
coll.PushQueueJob(dashboard.QueueJob{
    ID: "job-1", Queue: "emails", State: "pending", Payload: "...",
})

// Register a scheduled task
coll.PushTask(dashboard.SchedulerTask{
    Name: "cleanup-sessions", Cron: "0 */5 * * * *", NextRun: time.Now().Add(5*time.Minute),
})

// Register a health check
coll.RegisterHealthCheck("database", func() (string, string) {
    if err := db.Ping(); err != nil {
        return "red", err.Error()
    }
    return "green", "reachable"
})

// Register a database inspector for the DB Browser page
coll.SetDBInspector(myORMAdapter)

// Populate the Video page from the bus video.Mount publishes on.
// Returns a detach func; skip this call and the page stays hidden.
defer coll.AttachVideo(events.Default)()
```


### Implementing a DBInspector

To enable the Database Browser page, implement the `dashboard.DBInspector` interface:

```go
type DBInspector interface {
    Tables() ([]TableInfo, error)
    TableData(name string, page, pageSize int, search string) (TableData, error)
}
```

The adapter typically introspects `information_schema.columns` (for Postgres/MySQL) or `sqlite_master` + `PRAGMA table_info` (for SQLite).

---

## Architecture

```
dashboard/
  config.go        — Config + DefaultConfig()
  types.go         — All data model types (RequestRecord, QueryRecord, etc.)
  ringbuffer.go    — Thread-safe ring buffer used by all collectors
  collector.go     — Central aggregation point
  sampler.go       — 1Hz metrics sampler (GC, heap, goroutines, CPU)
  cpu.go           — CPU usage helpers (with /proc/self/stat on Linux)
  cpu_linux.go     — Linux implementation
  cpu_other.go     — Non-Linux stub
  timeline.go      — TimelineRecorder (per-request step profiler)
  middleware.go    — Breeze middleware that instruments every request
  mask.go          — Header / log secret masking
  auth.go          — HTTP Basic Auth (constant-time)
  wshub.go         — WebSocket hub for real-time updates
  api.go           — REST API handlers (all 14 pages)
  video_live.go    — Per-file live stream tracker (opt-in via AttachVideo)
  api_explorer.go  — API Explorer + multi-language code generation
  explorer_target.go — Confines an explorer request to this service (SSRF guard)
  install.go       — Install() entry point + push API
  attach.go        — Attach() one-liner convenience
  spa.go           — SPA() returns the dashboard HTML shell
  spa_css.go       — Inlined CSS
  spajavascript.go — Inlined JavaScript SPA runtime
```

### Design Decisions

1. **Zero-overhead fast path**: When `enabled: false`, the middleware returns after `ctx.Next()` — no allocations, no locks, no map writes.
2. **Ring buffers, not channels**: Each collector uses a fixed-capacity ring buffer. This bounds memory usage and avoids backpressure on the hot path.
3. **Single WebSocket**: All live updates multiplex over one connection. The hub drops messages for slow clients rather than blocking the request path.
4. **Self-contained SPA**: The dashboard ships as a single HTML response with inlined CSS and JS — no external dependencies, no asset pipeline, no CDN.
5. **Canvas charts**: Custom canvas-based line charts. No Chart.js, no D3, no npm install.
6. **Virtual scrolling**: The Live Requests table renders the latest 200 rows by default. Larger windows are handled by the same renderer without DOM thrashing.

---

## Running the Example

```bash
go run ./cmd/dashboard-example
```

Then open:
- Application: http://localhost:3000/
- Dashboard:   http://localhost:3000/dashboard (admin / admin)

The example simulates ORM queries, log entries, scheduled tasks, and health checks so you can see every page populated with realistic data.
