# `cmd/api-example`

## What this demonstrates

The default Breeze application: a JSON REST API with generated OpenAPI docs, two
WebSocket endpoints, and static file serving — in one `main.go`.

This is the example the `Dockerfile` builds by default
(`BREEZE_TARGET=./cmd/api-example`).

Specifically:

- `breeze.New(router, pool)` with an event-loop worker pool sized to `NumCPU`
- `SetZeroCopyHeaders(true)` and the reasoning that makes it safe here
- Five routes with `:id` params, each documented via `middleware.DocGET`/`DocPOST`
  and friends
- `ScalarMiddleware` → `/openapi.json` and `/scalar`
- `app.WebSocket("/ws", handler)` for a chat hub, and `WSHandlerFunc` for an inline
  echo endpoint
- `router.ServeStatic("/files", "./files/")`

## How to run it

```bash
go run ./cmd/api-example
```

```bash
curl localhost:3000/users
curl -X POST localhost:3000/users -d '{"name":"Ada","email":"ada@example.com","age":36}'
curl localhost:3000/users/1
curl localhost:3000/ws/stats
open http://localhost:3000/scalar
```

WebSocket:

```bash
websocat ws://localhost:3000/ws/echo
```

Or in a container:

```bash
docker build -t breeze-example . && docker run --rm -p 3000:3000 breeze-example
```

`ServeStatic` points at `./files/`, which is relative to the working directory —
run from the repository root, or create the directory first.

## What to look for

**`SetZeroCopyHeaders(true)` and the comment above it.** This is the one setting in
Breeze that trades safety for an allocation, and the comment states the whole
precondition: no handler keeps a string off `ctx.Req` past its own return, the
blocking routes promote their requests to owned memory before a worker sees them,
and no middleware caches by path. Copy the setting only when you can make the same
three statements.

**`middleware.DocGET(...)` as a trailing argument to `Handle`.** The documentation
is declared where the route is, and it is Go — `Fields: CreateUserRequest{}` stops
compiling the day that struct is renamed. Compare with a comment-based generator,
where nothing checks the comment.

**The chat hub is injected, not looked up.** `app.WebSocket` returns the hub
immediately, so `chat.hub = app.WebSocket("/ws", chat)` gives the handler its
broadcast handle at registration. There is no service locator and no `nil` window.

**`wsStats` is a struct, not a `map[string]int64`.** The comment at its use site
explains why: a map allocates and sorts its keys on every marshal, and this is on a
route a monitoring system polls.

**`WSHandlerFunc`** is the closure form for an endpoint that does not need state.
The chat handler is a struct because it has a hub; the echo endpoint is not.

Next: [`../dashboard-example`](../dashboard-example) to see this traffic on the
dashboard, and [`../../docs/scalar.md`](../../docs/scalar.md) for what else
`RouteDoc` can declare.
