# `cmd/dashboard-example`

## What this demonstrates

The developer dashboard against a real application — no fake data, no mock
generators, no hardcoded metrics. Every number on every page comes from something
that actually happened in this process.

Specifically:

- `dashboard.Install(app, router, cfg)` and `router.Use(coll.Middleware())`
- `DBInspector` / `DBWriter` implemented over an in-memory store, which is what
  makes the Database Browser page live and editable
- `RegisterConnection` for the Architecture page
- `RegisterHealthCheck` with two checks that verify something
- `AllowWrites: true`, and why that is a demo-only setting

## How to run it

```bash
go run ./cmd/dashboard-example
```

Open <http://localhost:3000/dashboard> — credentials `admin` / `admin`.

The dashboard is empty until traffic exists. Generate some:

```bash
curl localhost:3000/api/users
curl -X POST localhost:3000/api/users -d '{"name":"Alice","email":"alice@example.com"}'
curl localhost:3000/api/users/1
curl -X DELETE localhost:3000/api/users/1
curl localhost:3000/api/health
```

Then watch the Live Requests page while a loop runs:

```bash
while true; do curl -s localhost:3000/api/users > /dev/null; sleep 0.2; done
```

## What to look for

**`router.Use(coll.Middleware())` before the application routes.** The middleware
captures what it wraps; installed after the routes, it wraps nothing. This is the
one ordering mistake that produces a dashboard which looks installed and reports
nothing.

**The two-tier capture in the middleware.** When nobody is watching, a request costs
an atomic increment and two `time.Now()` calls. When the dashboard is open, the full
record is captured — IP, headers, route pattern, timeline. That is why this is
usable in production and not only in development.

**`DBInspector` and `DBWriter` are interfaces you implement.** `UserStore` satisfies
them over a map. A real application implements them over its ORM, and the same page
works — the dashboard has no opinion about the database because it never talks to
one.

**`AllowWrites: true` is set here deliberately** and commented as demo-only. It
enables row insert, update and delete from the browser. The guard behind it
(`writableGuard`) also checks that the target table is one the inspector reports, so
a write cannot reach a hand-crafted table name — but the config flag is still the
thing standing between a dashboard user and your data.

**The health checks verify something.** `runtime` reads the real goroutine count and
returns yellow above 1000, red above 10000. `data-store` reads the real record
count. A health check that returns green unconditionally is worse than no health
check, because it is trusted.

**`GOMEMLIMIT` and `GOGC` in `DefaultConfig`.** The dashboard sets a 512 MB soft
limit and `GOGC=50` because a process holding gigabytes of idle heap makes every
memory number on the Performance page meaningless. Tune them for your RAM; the
comment above `cfg` shows how.

**`coll.PushQuery(...)`** is how SQL reaches the ORM Query Monitor. This example
notes the hook rather than using it, since there is no driver to hook.

Next: [`../../dashboard/README.md`](../../dashboard/README.md) for all 13 pages, and
[`../fleet-example`](../fleet-example) for the same dashboard across three services.
