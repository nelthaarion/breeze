# Middleware

Every middleware Breeze ships, what each one does, and the order to install them
in.

```go
import middleware "github.com/nelthaarion/breeze/v2/middlewares"

router.Use(middleware.RecoveryMiddleware())            // outermost
router.Use(middleware.LoggingMiddleware())
router.Use(middleware.NewRateLimiter(middleware.RateLimiterOptions{
    Requests: 100,
    Per:      time.Minute,
}))
router.Use(middleware.CORSMiddleware(middleware.CORSOptions{AllowOrigins: "*"}))
router.Use(middleware.DefaultSecurityMiddleware())
router.Use(middleware.CompressionMiddleware())         // innermost
```

The package is `middlewares` on disk and `middleware` in Go. Every import site
aliases it, as above — see
[`repository-structure.md`](./repository-structure.md#known-deviation-kept-deliberately)
for why that is not being fixed.

## The middleware contract

A middleware is a `breeze.HandlerFunc`. It runs before the handler, calls
`ctx.Next()` to continue, and returns an `error` — the same signature as a handler,
so there is one type to learn and a middleware can terminate a request by simply
not calling `Next`:

```go
func requireAPIKey(ctx *breeze.Context) error {
    if ctx.Req.Header["x-api-key"] != wantKey {
        ctx.Status(401)
        return ctx.JSON(map[string]string{"error": "api key required"})
    }
    return ctx.Next()
}
```

`router.Use` prepends to every route's chain, including routes registered before
the `Use` call — the chain is rebuilt, so installation order in the file does not
have to match registration order. Per-route middleware goes as trailing arguments
to `Handle`, and runs after all global middleware.

The chain is composed once at registration time, not per request. A route with no
`:params` costs zero allocations to dispatch.

### Capture before `Next`

Anything read from `ctx.Req` must be read **before** `ctx.Next()`. Strings on the
request point into a pooled read buffer that the next request on that connection
reuses, so a middleware that logs `ctx.Req.Path` after `Next` returns may log a
different request's path. Copy with `strings.Clone` if a value must outlive the
handler.

### Response headers set before `Next` survive the handler

Setting a response header is the other half, and it goes the same way round: a
middleware calls `ctx.SetHeader` **before** `Next`, because after `Next` returns the
handler has already written the response.

That works. `ctx.JSON`, `ctx.WriteString` and `ctx.HTML` merge their content type into
whatever headers are already on the response rather than replacing them, and a
`Content-Type` the caller set explicitly wins over the body method's default — which is
what makes `application/problem+json` possible from `ctx.JSON`.

A *previous body method's* content type does not win, which is the distinction worth
knowing: `ctx.WriteString(...)` followed by `ctx.JSON(...)` sends
`application/json`, not `text/plain`. Only a type set through `SetHeader` is treated as
a choice.

It did not always work. Until recently the body methods assigned a shared header map
outright, so every header CORS and the security middleware computed was silently
discarded by the handler's `ctx.JSON` call. Nothing errored; a browser simply reported
a CORS failure. See the CHANGELOG entry *Response headers set by middleware were
discarded by the handler* — the regression tests live in
`middlewares/header_preservation_test.go`, because the bug was only observable through
a real chain.

## Installation order

Order is not a preference. Each of these is a consequence of what the middleware
does to the request or the response:

| # | Middleware | Why here |
|---|---|---|
| 1 | `RecoveryMiddleware` | Outermost, or it cannot catch a panic raised by anything installed after it. |
| 2 | `LoggingMiddleware` | Outside the rate limiter, so a rejected request still appears in the log. |
| 3 | `NewRateLimiter` | Before auth and before any handler work, so a flood costs a map lookup rather than a token verification. |
| 4 | `LocaleMiddleware` | Before anything that renders text. |
| 5 | `CORSMiddleware` | Must answer the `OPTIONS` preflight before auth rejects it — a preflight carries no `Authorization` header. |
| 6 | `SecurityMiddleware` | After CORS: it only adds response headers, and putting it first means CORS can overwrite them. |
| 7 | `JWTAuthMiddleware` | After CORS, before the handler. |
| 8 | `ETagMiddleware` | Inside auth, so a 304 is only ever served to a caller who was allowed to see the body. |
| 9 | `CompressionMiddleware` | Innermost. It rewrites the finished body, so everything that inspects or sets the body must already have run. |

`ScalarMiddleware` is a route, not a chain member — install it once, not with
`Use`.

## The middlewares

### `RecoveryMiddleware()`

Recovers a panic, logs it with the stack, and returns a 500. Also feeds the
`recovery` diagnostic probe with the count, the time and the value of the most
recent panic — the three facts nothing else in the process retains.

Panic facts are counted **ungated**, unlike every other counter here: a panic is
rare by definition, so the atomic increment is not on a hot path, and a probe that
answered "counting was off" to "did anything panic?" would be useless.

### `LoggingMiddleware()`

One line per request: method, path, status, duration. Reads everything it needs
before `Next`.

### `NewRateLimiter(RateLimiterOptions)`

Fixed-window counting, keyed by client IP with the port stripped so a reconnect
shares its counter.

| Field | Type | Default | Meaning |
|---|---|---|---|
| `Requests` | `int` | 0 (blocks everything) | requests allowed per window |
| `Per` | `time.Duration` | 0 | the window |
| `Message` | `string` | `"Rate limit exceeded: max N requests per D"` | 429 body |

Set both `Requests` and `Per`. There is no default, deliberately: a rate limiter
that silently picked a limit would be worse than one that obviously blocks.

The lock is held for the map lookup and the counter update only, never across
`ctx.Next()`. An earlier version held it across the handler, which serialised
every request and defeated the worker pool entirely. The client map is swept once
a minute by a background goroutine.

### `CORSMiddleware(CORSOptions)`

All fields are `string`, matching the header values they become — no join step,
and what you configure is what goes on the wire.

| Field | Example |
|---|---|
| `AllowOrigins` | `"*"` or `"https://example.com"` |
| `AllowMethods` | `"GET,POST,PUT,DELETE"` |
| `AllowHeaders` | `"Content-Type,Authorization"` |
| `ExposeHeaders` | `"X-Request-Id"` |
| `AllowCredentials` | `"true"` |
| `MaxAge` | `"86400"` (seconds) |

`AllowOrigins: "*"` with `AllowCredentials: "true"` is rejected by browsers, not
by this middleware. Name the origin when you need credentials.

### `SecurityMiddleware(SecurityOptions)` / `DefaultSecurityMiddleware()`

Twelve response headers: CSP, `X-Frame-Options`, `X-Content-Type-Options`,
`Referrer-Policy`, HSTS, `Permissions-Policy`, `X-XSS-Protection`, `Expect-CT`,
the three `Cross-Origin-*-Policy` headers, and `Cache-Control`.

`DefaultSecurityMiddleware()` is the strict set. Override one field at a time with
the `With*` helpers:

```go
router.Use(middleware.SecurityMiddleware(
    middleware.WithContentSecurityPolicy("default-src 'self'; img-src *"),
))
```

### `CompressionMiddleware()`

Negotiates `Accept-Encoding` and compresses with brotli, gzip or deflate — in that
preference order, since brotli wins on ratio for text and every browser that sends
`br` supports it.

Encoders are pooled per algorithm. Allocating a fresh brotli encoder costs 4–8 KB,
and doing that per response put the allocation on the hot path for no benefit.

### `NewETagCache()` + `ETagMiddleware()`

MD5 of the response body as a strong ETag, and a 304 when `If-None-Match` matches.
The cache key includes the query string, so `?page=1` and `?page=2` get distinct
ETags.

MD5 rather than a faster hash because it is already in the standard library and
the body is already in memory; this is not a security boundary, it is a change
detector.

**The store is unbounded.** It is written on every response and read only on an
`If-None-Match` request, so it exists for observability rather than for serving
from cache. Do not install this on an endpoint with unbounded distinct URLs
without adding eviction.
### `JWTAuthMiddleware(JWTOptions)`

| Field | Type | Purpose |
|---|---|---|
| `AccessSecret` | `string` | HMAC key — **required** |
| `RefreshSecret` | `string` | key for refresh tokens |
| `SigningMethod` | `jwt.SigningMethod` | pinned; e.g. `jwt.SigningMethodHS256` |
| `TokenLookup` | `func(*Context) (string, string, error)` | defaults to `Authorization: Bearer` |
| `OnUnauthorized` | `func(*Context, error)` | defaults to a 401 problem+json |
| `UserContextKey` | `string` | where claims land in `ctx` |
| `RequiredRoles` | `[]string` | role gate |
| `ClaimsValidator` | `func(jwt.MapClaims) bool` | extra claim checks |
| `EnableRefreshToken` | `bool` | accept and rotate refresh tokens |

It **refuses to construct without `AccessSecret`**. An empty HMAC key verifies any
token an attacker signs with an empty key, so a missing secret is not a degraded
configuration — it is an open door, and failing at startup is the only safe
response.

`SigningMethod` is pinned rather than read from the token's own header, which is
what closes the `alg: none` and RS256→HS256 confusion attacks.

`GenerateJWT` and `GenerateRefreshToken` are exported for the login handler.

### `LocaleMiddleware(*breeze.I18n)`

Resolves the request locale from `?lang=`, then the `breeze_locale` cookie, then
`Accept-Language`, and puts it on the context for the template engine.

### `ScalarMiddleware(router, ScalarOptions)`

Not a chain middleware — it registers the OpenAPI JSON and the Scalar UI as routes.
See [`scalar.md`](./scalar.md).

## Diagnostics

Nine of these register a `diag` probe, so `GET /dashboard/api/diagnostics` reports
whether each is installed and what it has done. See [`diag.md`](./diag.md).

The distinction the probes are careful about: "not installed" and "installed and
quiet" are both a zero count, and they want opposite next actions. Each
constructor sets an installed flag so its probe can tell them apart.

Counted middlewares (compression, ETag, rate limit) count only once
`diag.EnableCounters()` has been called. A counter shared across cores moves a
cache line on every request, and that coherence traffic — not the increment
itself — is the cost.

## See also

- [`../README.md`](../README.md) — the tour's production-middleware list
- [`../middlewares/oauth2/README.md`](../middlewares/oauth2/README.md) — OAuth2 login, a separate package
- [`scalar.md`](./scalar.md) — `ScalarMiddleware` and route docs
- [`diag.md`](./diag.md) — the probe registry these report into


