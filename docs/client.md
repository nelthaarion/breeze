# HTTP client

Breeze's outbound HTTP client, built on gnet — the same event-loop engine the
server runs on.

```go
c := client.New()
defer c.Close()

resp, err := c.Get("http://auth-service/verify")
if err != nil {
    return err
}
if resp.OK() {
    fmt.Println(resp.String())
}
```

With headers — what Fleet tracing uses to inject trace context:

```go
req := client.NewRequest("POST", url, body).
    SetHeader("Content-Type", "application/json").
    SetHeader("Traceparent", tc.String())

resp, err := c.Do(req)
```

## Why gnet, not `net/http`

The server side of Breeze is built on gnet. Using the same engine for outbound
calls means both directions share one non-blocking I/O model and one
connection-pooling strategy, rather than the server being event-loop-based while
every outgoing call spins up `net/http`'s goroutine-per-connection machinery
beside it.

The cost of that choice is that gnet is a raw TCP byte-stream engine with no notion
of HTTP, so request serialisation and response parsing are implemented in this
package rather than inherited from the standard library. That is a deliberate
trade, and it is where the limitations below come from.

## Limitations — read these first

This client exists for service-to-service JSON traffic. If your case is not that,
`net/http` is the right answer and there is no penalty for using it.

| Limitation | Detail |
|---|---|
| **HTTP/1.1 only** | No HTTP/2. That means HPACK and stream multiplexing here, far past diminishing returns for JSON between services. |
| **TLS via `crypto/tls`** | The handshake is done by `tls.Dial` and the connection handed to gnet with `Enroll`. gnet itself has no TLS support. |
| **No chunked requests** | Chunked *responses* are decoded. Requests always carry a `Content-Length`. |
| **No redirect following** | A 3xx is returned as-is. |
| **Bodies are fully buffered** | Wrong tool for SSE or large downloads. Use `net/http`. |
| **No pipelining** | One in-flight request per connection; responses are correlated by connection, not by sequence number. |

## Config

```go
c := client.New(client.Config{
    Timeout:             10 * time.Second,
    MaxIdleConnsPerHost: 128,
})
```

| Field | Default | Meaning |
|---|---|---|
| `Timeout` | 30s | bounds the whole call: connect + write + read |
| `MaxIdleConnsPerHost` | 64 | idle-connection budget per upstream host |
| `DialTimeout` | 5s | bounds establishing the TCP or TLS connection |
| `MaxResponseBytes` | 32 MiB | caps the response body |
| `UserAgent` | `"breeze-client/1"` | sent unless the request sets its own |
| `TLSConfig` | nil | replaces the default `tls.Config` |

The constants are exported: `DefaultTimeout`, `DefaultMaxIdleConnsPerHost`,
`DefaultDialTimeout`, `DefaultMaxResponseBytes`, `DefaultUserAgent`. `DefaultConfig()`
returns them as a `Config`, and `New()` with no argument uses it.

`MaxIdleConnsPerHost` defaults to 64 rather than `net/http`'s 2. Two is sized for a
browser-like workload calling many different hosts once; 64 is sized for a service
that calls the same few upstreams on every request, where a budget of 2 means
dialling on almost every call.

`Timeout` covers the *whole* call rather than each phase. A per-phase timeout lets
a slow upstream spend the budget three times over, which is how a 10-second timeout
turns into a 30-second stall.

## Requests and responses

```go
req := client.NewRequest("POST", "https://api.example.com/users", body)
req.SetHeader("Content-Type", "application/json")  // replaces
req.AddHeader("X-Tag", "b")                        // appends
val, ok := req.GetHeader("Content-Type")
hdr := req.Header()                                // the http.Header
req = req.WithContext(ctx)                         // cancellation
```

`SetHeader`, `AddHeader` and `WithContext` return the request, so they chain.

```go
type Response struct {
    Status int
    Header http.Header
    Body   []byte
}

resp.OK()      // 2xx — nil-safe
resp.String()  // body as a string — nil-safe
```

Both `Response` methods are nil-safe, so `resp.OK()` on an error path is false
rather than a panic. That matters because the natural shape — check the error, then
check the status — reads better when the second check cannot itself fail.

### Convenience methods

```go
c.Get(url)
c.Post(url, contentType, body)
c.PostJSON(url, body)   // Content-Type: application/json
c.Do(req)               // the general form
```

### Sentinel errors

| Error | When |
|---|---|
| `ErrNilRequest` | `Do(nil)` |
| `ErrNoURL` | the request's URL is empty or whitespace |
| `ErrResponseTooLarge` | the body exceeded `MaxResponseBytes` |

Also returned as plain errors: `"client: connection closed by server"` and
`"client: malformed status line"`.

`ErrResponseTooLarge` is a sentinel rather than a formatted string because a caller
that wants to retry against a streaming client needs to distinguish it from a
transport failure, and `errors.Is` is the only way to do that without matching on
message text.

## Lifecycle

```go
c := client.New()
defer c.Close()
```

The gnet engine is started **lazily**, on the first request, not by `New`. A
`Client` constructed and never used costs a struct — which is what makes it
reasonable for a library to hold one as a field rather than requiring the
application to inject it.

`Close` shuts the engine down and releases pooled connections. A `Client` is safe
for concurrent use; that is the point of the per-host pools.

`c.Config()` returns the effective configuration with defaults applied, which is
what a diagnostic or a test asserts against rather than re-deriving the defaults.

## Who uses it

- **Fleet tracing** — span export to the aggregator, with `Traceparent` injected via
  `SetHeader`. See [`fleet-tracing.md`](./fleet-tracing.md).
- **MCP live tools** — reading a running service's dashboard API.

Both are the shape this client is for: JSON, service-to-service, small bodies, the
same few upstreams on every call.

The dashboard's API explorer deliberately uses `net/http` instead, because it needs
per-request control over redirect behaviour — see the comment at
[`../dashboard/api_explorer.go`](../dashboard/api_explorer.go).

## See also

- [`../client/client.go`](../client/client.go) — the package, including its own doc comment
- [`fleet-tracing.md`](./fleet-tracing.md) — the largest consumer
- [`rpc.md`](./rpc.md) — the same gnet-native reasoning applied to a server

