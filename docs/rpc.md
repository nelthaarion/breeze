# JSON-RPC 2.0

A JSON-RPC 2.0 server on its own port, running directly on gnet's event loop.

```go
reg := rpc.NewRegistry()
reg.Use(logging)                        // mirrors Router.Use
reg.Register("sum", sum)                // mirrors Router.Handle
reg.RegisterBlocking("db.query", query) // mirrors Router.HandleBlocking

srv := rpc.NewServer(reg)
srv.SetPool(breeze.NewEventLoopWorkerPool(runtime.NumCPU()))
log.Fatal(srv.Run(9000, true))          // port, multiCore
```

```go
func sum(ctx *rpc.Context) {
    var in struct{ A, B int }
    if err := ctx.Bind(&in); err != nil {
        ctx.Errorf(rpc.CodeInvalidParams, "a and b must be numbers")
        return
    }
    ctx.Result(in.A + in.B)
}
```

```bash
curl -s --data '{"jsonrpc":"2.0","id":1,"method":"sum","params":{"a":2,"b":3}}' \
  --output - localhost:9000
# {"jsonrpc":"2.0","id":1,"result":5}
```

## Why it is a peer of HTTP, not a passenger

This is a second protocol on a second port, not a route. Requests are framed out
of the raw connection buffer, dispatched, and answered with gnet's own
`Conn.Write` / `Conn.AsyncWrite`. There is no `net/http` server, no `net/rpc`, no
`http.Handler` adapter and no reflection-driven method discovery anywhere in the
path — the same reasons the HTTP layer avoids them apply here unchanged.

The API is deliberately the router's API with different nouns. `NewRegistry`,
`Use`, `Register`, `RegisterBlocking`, a `HandlerFunc` taking a `*Context`,
middleware composed through `Context.Next`, and the `[global..., method...,
handler]` chain flattened once at registration rather than rebuilt per call.
Nothing new to learn if you already registered a route.

## Framing

JSON-RPC 2.0 specifies a message format and says nothing about how messages are
delimited on a byte stream. This package frames by **structural completeness**: it
reads one complete JSON value at a time, tracking bracket depth while respecting
string literals and escapes.

That accepts every framing a client is likely to use — newline-delimited,
whitespace-separated, or values packed back to back with no separator at all —
without the client having to declare which one it picked, and without a
length-prefix header that is not part of the specification.

A partially-received value is left in the connection's gnet context and completed
by a later read, exactly as the HTTP layer reassembles a split request.

### Bounds

| Setting | Default | Why |
|---|---|---|
| `SetMaxMessageBytes(n)` | 4 MiB | A client that opens a brace and goes quiet would otherwise pin memory indefinitely. Exceeding it drops the connection. |
| `compactThreshold` (internal) | 512 B | Below this, the leftover partial message is copied to the front of the buffer rather than leaving the buffer grown. |


## The `Context`

| Method | Purpose |
|---|---|
| `Bind(v any) error` | decode `Params` into `v` |
| `Result(v any)` | set the result member |
| `ResultRaw(json.RawMessage)` | set an already-encoded result, copied verbatim |
| `Error(*rpc.Error)` | set a fully built error |
| `Errorf(code int, msg string)` | set code + message |
| `ErrorData(code int, msg string, data any)` | set code + message + `data` |
| `IsNotification() bool` | true when the request had no `id` |
| `Set` / `Get` | per-call values, for middleware to pass to handlers |
| `Next()` / `Abort()` | chain control |
| `Err() *rpc.Error` | what has been set so far |

`Bind` reports a decode failure as `-32602 Invalid params`, which is what §5.1
specifies for parameters the server cannot use — the JSON itself parsed, so
`-32700` would be wrong. It returns the error too, so a handler can add context,
but a handler that ignores the return still produces a correct response.

Absent params bind as a no-op rather than an error: a method whose parameters are
all optional is legitimate, and a method with required ones will find them
zero-valued and can say so with a better message than a generic one from here.

`Abort` does not supply an error. Set one first — a call aborted with no error
replies with a null result.

Contexts are pooled, and the `store` map is cleared on release so a value set by
middleware cannot leak into an unrelated later call.

## Notifications

A request with no `id` is a notification: the handler runs and **nothing is
written**, per §4.1. `ctx.IsNotification()` reports it, and a handler that sets a
result or an error on one has it silently dropped rather than sent — the spec does
not permit a reply.

A batch consisting entirely of notifications produces no response at all, not an
empty array.

## Error codes

The reserved set from the specification, as constants:

| Constant | Code | Meaning |
|---|---|---|
| `CodeParseError` | −32700 | invalid JSON received |
| `CodeInvalidRequest` | −32600 | the JSON is not a valid Request object |
| `CodeMethodNotFound` | −32601 | the method does not exist |
| `CodeInvalidParams` | −32602 | invalid method parameters |
| `CodeInternalError` | −32603 | internal error |

Ranges, and the check that uses them:

```go
rpc.CodeServerErrorMin // -32099 ─ implementation-defined server errors
rpc.CodeServerErrorMax // -32000 ┘
rpc.CodeReservedMin    // -32768 ─ reserved by the spec
rpc.CodeReservedMax    // -32000 ┘

rpc.ErrorCodeReserved(code) // true when code is inside the reserved range
```

Application errors belong **outside** −32768…−32000. `ErrorCodeReserved` exists so
a server can assert that in a test rather than discovering a collision from a
confused client.

Constructors for the five standard errors — `ErrParseError()`,
`ErrInvalidRequest()`, `ErrMethodNotFound()`, `ErrInvalidParams()`,
`ErrInternalError()` — plus `NewError(code, msg)` and

## Blocking work

A handler that blocks must not run on an event loop — it stalls every connection
pinned to that loop. Register those with `RegisterBlocking` and give the server a
pool:

```go
srv.SetPool(breeze.NewEventLoopWorkerPool(runtime.NumCPU()))
reg.RegisterBlocking("db.query", query)
```

Everything else runs inline on the loop that read the bytes and is answered with a
direct write. Same trade-off, same defaults, as `Breeze.SetInlineExecution`.

`RefreshBlocking()` re-reads which methods are blocking, for a server that
registered more methods after starting.

## Lifetime of `Context.Params`

`Params` is raw JSON pointing into the connection's read buffer, which gnet reuses
for the next read. **It is valid for the duration of the handler and no longer.**

A batch containing any blocking method is copied into owned memory before it leaves
the event loop, so a handler always sees valid bytes. But anything kept past the
handler's return must be copied first — the same rule and the same failure mode as
`breeze.SetZeroCopyHeaders`.

`ctx.Bind` copies as part of decoding, so bound structs are always safe to keep.

## stdio transport

Some peers do not speak TCP. The Model Context Protocol runs a server as a child
process and talks to it over the child's stdin and stdout, so there is a second
transport:

```go
stdio := rpc.NewStdioServerOS(srv)   // or NewStdioServer(srv, in, out)
log.Fatal(stdio.Serve())
```

Both transports go through `Server.Handle`, so dispatch, middleware, batching and
the error codes are shared. Only framing differs — one JSON value per line, because
that is what the peer writes.

Newline framing is safe here because `encoding/json` escapes literal newlines inside
strings as `\n`, so a marshalled JSON value never contains a bare newline. A line is
therefore always a whole message.

| Setting | Default | Why |
|---|---|---|
| `SetMaxLine(n)` | 8 MiB | A stdio peer is usually a local process, but "usually" is not a security model: without a cap, a peer that never sends a newline grows the scanner buffer until the process dies, and the failure looks like a memory leak rather than a bad message. |

`Serve` returns nil on clean end of input — a peer closing its side is how a stdio
session normally finishes, not a failure. A malformed message does not stop the loop
either: `Handle` answers it with a parse error and reading continues, because the
peer is still there and still able to send something valid.

**Log to stderr.** Anything a handler prints to stdout lands in the middle of the
protocol stream and corrupts it.

## `Server.Handle` for tests

```go
out := srv.Handle([]byte(`{"jsonrpc":"2.0","id":1,"method":"sum","params":{"a":2,"b":3}}`))
```

Bytes in, bytes out, no listener and no connection. This is the same entry point
both transports use, so a test against it is testing the real dispatch path.

## See also

- [`../rpc/doc.go`](../rpc/doc.go) — the package documentation
- [`mcp-walkthrough.md`](./mcp-walkthrough.md) — the largest consumer of the stdio transport
- [`diag.md`](./diag.md) — the `jsonrpc` probe: method count, blocking count, unknown-method notes
- [`../README.md`](../README.md) — the tour's JSON-RPC section

`NewErrorData(code, msg, data)` for your own.
