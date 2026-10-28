# Breeze performance review: requests per second

**Scope.** The HTTP hot path of `breeze.zip`: gnet event loop → request parser → router → middleware → response writer. The code was read end to end, profiled under `wrk` load, changed where the profiles and the code pointed, and measured before and after.

**Read this first.** Every number below comes from one 1‑vCPU VM (kernel 6.18.44-fc-v180) where `wrk` shares the only core with the server (server GOMAXPROCS=2, 64 connections, medians of interleaved runs). Run‑to‑run spread is ±5–15%, p99 is unusable (20–28 ms outliers appear in original and modified runs alike), and a single event loop cannot show lock contention. Treat the percentages as direction and rough size, and validate on your own multi‑core Linux hardware with `perf/run_ab.sh`.

## 1. What was found and fixed

The core path was already well optimised (inline execution, pooled request/context/response, batched pipelined writes, exact‑path route map, pre‑built header blocks), so what remains is specific rather than a rewrite.

| # | Bottleneck | Fix | Default |
|---|---|---|---|
| 1 | Routes with `:param` or `*wildcard` were resolved by an ordered scan over every route of the method (path split, per‑segment compares), so cost grew linearly with the route table | Segment trie that returns the lowest‑registration‑index match, so "first registered wins" is unchanged | on |
| 2 | Two static‑header middlewares (CORS + Security) rebuilt and re‑copied the merged header block and a 12‑entry map on every request | The merge is computed once per pair of blocks and shared copy‑on‑write | on |
| 3 | `LoggingMiddleware`: `fmt.Printf` per request (allocations plus one `write(2)` on the event‑loop thread) | Allocation‑free formatting with a per‑second cached timestamp, byte‑identical output; optional buffered writer that batches the syscalls | on / opt‑in |
| 4 | `CompressionMiddleware` re‑split `Accept-Encoding` up to three times per request | Single‑pass, allocation‑free parse with identical results | on |
| 5 | `ETagMiddleware`: one global lock taken for writing on every response, and an unbounded map that kept the full body of every distinct URL forever | Sharded, bounded store that holds only the ETag and writes nothing when it is unchanged | on |
| 6 | JWT: HMAC, base64 and JSON decoding on every request, for the same token | Optional verified‑token cache (honours `exp`; role checks and `ClaimsValidator` still run, on a private copy of the claims) | opt‑in |
| 7 | Compressing tiny bodies pays the whole encoder setup | `CompressionMiddlewareWith(CompressionOptions{MinSize: n})` | opt‑in |
| 8 | gnet's pre‑block `runtime.Gosched` costs scheduler round trips and futex wake‑ups on a server that only handles requests inline | `SetPollYield`; see section 4 for why it is not the default | opt‑in |
| 9 | Header‑name validation fell through to `strings.ContainsRune` for every `-` | 256‑entry lookup table | on |

## 2. End‑to‑end results

Each scenario: `wrk` 1 thread, 64 connections, 4 interleaved runs of 3 s per configuration, so every configuration saw the same machine state. "Server CPU µs/req" is read from `/proc` for the server process alone and is the steadier of the two figures. "new (default)" is the shipped default; the other "new" rows switch on the opt‑ins named in them.

**recover + CORS + security headers + rate limiter, small JSON handler**

| config | req/s (median) | min – max | server CPU µs/req | RPS vs original | CPU/req vs original |
|---|---:|---:|---:|---:|---:|
| orig | 84,310 | 73k – 85k | 6.98 | — | — |
| new (default) | 93,041 | 91k – 95k | 5.73 | +10.4% | -17.9% |
| new + PollYieldAuto | 103,165 | 102k – 109k | 4.77 | +22.4% | -31.7% |

**the same stack plus request logging to a file**

| config | req/s (median) | min – max | server CPU µs/req | RPS vs original | CPU/req vs original |
|---|---:|---:|---:|---:|---:|
| orig | 67,384 | 61k – 74k | 9.50 | — | — |
| new (default) | 79,667 | 72k – 88k | 7.30 | +18.2% | -23.2% |
| new + buffered log | 81,714 | 79k – 91k | 6.99 | +21.3% | -26.4% |
| new + buffered log + PollYieldAuto | 93,566 | 91k – 99k | 5.70 | +38.9% | -40.0% |

**plaintext GET on a static route**

| config | req/s (median) | min – max | server CPU µs/req | RPS vs original | CPU/req vs original |
|---|---:|---:|---:|---:|---:|
| orig | 125,487 | 107k – 129k | 4.28 | — | — |
| new (default) | 118,056 | 107k – 128k | 4.56 | -5.9% | +6.5% |
| new + PollYieldAuto | 131,474 | 114k – 151k | 3.73 | +4.8% | -12.7% |

**small JSON response**

| config | req/s (median) | min – max | server CPU µs/req | RPS vs original | CPU/req vs original |
|---|---:|---:|---:|---:|---:|
| orig | 105,755 | 103k – 119k | 5.30 | — | — |
| new (default) | 112,990 | 107k – 119k | 4.99 | +6.8% | -5.8% |
| new + PollYieldAuto | 134,712 | 127k – 140k | 3.90 | +27.4% | -26.4% |

**244‑route REST table, 10 paths spread across it**

| config | req/s (median) | min – max | server CPU µs/req | RPS vs original | CPU/req vs original |
|---|---:|---:|---:|---:|---:|
| orig | 105,430 | 87k – 116k | 5.51 | — | — |
| new (default) | 106,536 | 94k – 112k | 5.38 | +1.0% | -2.3% |
| new + PollYieldAuto | 110,883 | 107k – 120k | 4.91 | +5.2% | -11.0% |

**JWT (HS256) with the same valid token on every request**

| config | req/s (median) | min – max | server CPU µs/req | RPS vs original | CPU/req vs original |
|---|---:|---:|---:|---:|---:|
| orig | 58,787 | 55k – 63k | 11.57 | — | — |
| new (default) | 58,581 | 57k – 61k | 11.52 | -0.3% | -0.4% |
| new + CacheSize | 106,892 | 97k – 108k | 5.46 | +81.8% | -52.8% |

**compression, `Accept-Encoding: gzip`, 730 B JSON body**

| config | req/s (median) | min – max | server CPU µs/req | RPS vs original | CPU/req vs original |
|---|---:|---:|---:|---:|---:|
| orig | 43,060 | 41k – 44k | 17.02 | — | — |
| new (default) | 44,424 | 43k – 47k | 16.40 | +3.2% | -3.7% |
| new + MinSize 1024 | 103,661 | 103k – 108k | 5.53 | +140.7% | -67.5% |

**ETag middleware, no `If-None-Match`**

| config | req/s (median) | min – max | server CPU µs/req | RPS vs original | CPU/req vs original |
|---|---:|---:|---:|---:|---:|
| orig | 93,148 | 90k – 100k | 6.66 | — | — |
| new (default) | 93,937 | 86k – 97k | 6.57 | +0.8% | -1.4% |

**Reading the results.** The default‑on changes move end‑to‑end throughput where the code they replaced was on the request path: the header‑middleware stack (+10.4% RPS, -17.9% CPU per request) and request logging (+18.2%, -23.2%). Plain static routes (-5.9%), small JSON (+6.8%) and the 244‑route table (+1.0%) are unchanged within noise. That is expected: on this VM roughly half of a request is kernel time (read, write, epoll), the router fix saves about 0.3 µs of about 5 µs at 244 routes, and the other fixes do not touch those paths. The router gain is real and grows with the table (section 3).

JWT (-0.3%), compression (+3.2%) and ETag (+0.8%) are unchanged by default, because their cost is the HMAC and JSON decode, the compressor, and MD5, not framework overhead. The opt‑ins are where the large numbers are: `JWTOptions.CacheSize` +81.8% RPS / -52.8% CPU per request, `CompressionOptions.MinSize` +140.7% / -67.5% (that one is a trade‑off, a 730 B body is simply not compressed, and it shows that the compressor costs about 11 µs per request), and the buffered logger a further +2.6% RPS over the synchronous one when writing to a file in the page cache (a pipe or container log driver makes the per‑request `write(2)` more expensive, so the gap should be wider there; I did not measure it).

## 3. Micro‑benchmarks

**Router lookup as the route table grows** (`go test -bench RouterLookupScaling`; the paths are the last‑registered resource, one in the middle and the first; zero allocations before and after):

| routes in the table | before (ns/lookup) | after (ns/lookup) | change |
|---:|---:|---:|---:|
| 100 | 325 | 106 | -68% |
| 400 | 1,021 | 117 | -89% |
| 1,000 | 2,105 | 109 | -95% |
| 4,000 | 8,502 | 112 | -99% |
| 244 (the REST table above, mixed paths) | 320 | 101 | -69% |

The old cost is linear in the number of routes, the new one is flat (depth of the path). Small tables, a handful of routes, are unchanged within measurement noise.

**Other micro‑benchmarks that moved by more than the noise floor:**

| benchmark | before | after |
|---|---|---|
| CORS + Security middleware pair | 1,481 ns, 2,384 B, 12 allocs | 331 ns, 440 B, 7 allocs |
| `Accept-Encoding: gzip, deflate, br` selection | 127 ns, 4 allocs | 61 ns, 0 allocs |
| ETag middleware, steady state (allocations only; the time difference is inside the noise) | 10 allocs, 824 B | 8 allocs, 768 B |

**Noise floor.** Running the *unchanged original code* twice on this VM gave a median absolute difference of 10% per benchmark and up to 45% for the worst one (`ZZPipelineInlineZeroCopy`), so no other micro‑benchmark difference is claimed, in either direction. The existing benchmarks for the parser, serializer and pipeline paths show differences of that size and are not evidence of a change.

## 4. `SetPollYield`: why it is opt‑in

Breeze never passes `gnet.WithNoPollYield` to the gnet fork, although inline execution is the default and the fork's own documentation recommends it for servers that handle traffic inline. I first made it automatic, then measured it, and the picture is mixed, so the default stays exactly as before and skipping the yield is an explicit choice: `app.SetPollYield(breeze.PollYieldAuto)` (skip it only when inline execution is on, no route is registered with `HandleBlocking`, and GOMAXPROCS > 1) or `PollYieldNever`. When the yield is skipped, a loop that does hand a request to a worker yields right after the hand‑off, so workers are not starved.

Effect of skipping the yield, same binary, same session (RPS change / CPU‑per‑request change):

| workload | earlier session | final session |
|---|---:|---:|
| plaintext | +7% / -14% | +11% / -18% |
| small JSON | +13% / -19% | +19% / -22% |
| 244‑route REST | +10% / -13% | +4% / -9% |
| header‑middleware stack | +11% / -17% | +11% / -17% |
| stack + file logging (final session: buffered logger) | +6% / -12% | +15% / -18% |
| JWT handler (6 runs) | +2% / -4% | – |
| ETag / MD5 handler (8 runs) | -10% / +6% | – |
| plaintext, **GOMAXPROCS=1** | -21% / +30% | – |

Light handlers gain roughly 4–19% RPS (CPU per request down 9–22%) depending on the session, a JWT handler is neutral, an MD5‑hashing handler loses about 10%, and with a single P the loop loses about 20%. At 8 connections plaintext went 139,943 → 157,027 req/s (+12%) with p99 212 → 177 µs (original vs the build that skipped the yield). I could not explain the ETag result beyond measuring it, and I could not test more than two Ps, so please measure this on the hardware you deploy to. To make it the default after doing so, make `PollYieldDefault` behave like `PollYieldAuto` in `decideNoPollYield` (`pollyield.go`).

## 5. Behaviour differences

Everything is output‑identical by default except these three:

* **ETag store is bounded.** It holds at most 16,384 URLs (32 shards × 512 entries) and keeps only the ETag, not the response body. An evicted URL costs one handler run on its next conditional request, which is still answered 304 if the ETag matches. Before, it grew without limit and retained every body.
* **Wildcard parameters are a substring of the request path** instead of a freshly joined string (same value). With `SetZeroCopyHeaders(true)` they now have the same "valid only during the request" lifetime as the other path parameters; before, a wildcard that spanned two or more segments happened to be a copy.
* **The diagnostics probe** reports the ETag entry count from the sharded store.

Additions to the public API (all additive): `Breeze.SetPollYield`, `PollYieldDefault/Auto/Never`; `middleware.LoggingOptions`, `LoggingMiddlewareWith`, `FlushLogs`; `middleware.CompressionOptions`, `CompressionMiddlewareWith`; `JWTOptions.CacheSize`. Call `middleware.FlushLogs()` from your shutdown path if you enable buffered logging; lines still buffered when the process dies abruptly are lost.

## 6. Verification

* **Differential tests against the old behaviour**: the router trie against a verbatim copy of the old ordered scan on 1,500 random route tables × 250 lookups = 375,000 lookups (same route and same params every time, with overlapping routes, duplicate shapes, zero‑segment wildcards, empty segments and several methods); the `Accept-Encoding` parser against the old implementation on 300,000 random headers, including non‑ASCII; the log line against `fmt.Sprintf` on 30,000 random cases (time zones, durations from 0 ns to years, statuses); the token table against the old check on all 256 bytes plus 20,000 random strings.
* **Wire‑level differential test** (`perf/wirecheck.py`): the same raw requests sent to the original and the new server: routing edge cases (trailing and empty segments, percent‑encoding, query strings, 404s), `POST`/`PUT`/`DELETE`/`PATCH`/`HEAD`, CORS preflight and actual requests, the full `Accept-Encoding` matrix, ETag/304 flows, JWT valid and invalid. 192 comparisons, 0 differences, including an original‑versus‑original control (header order is compared as a set because the server randomises it per response: 30 identical requests to the original produced 30 different orderings).
* New unit tests for the static‑header cache, the poll‑yield decision, the buffered log under 8 concurrent writers, the ETag cache bounds and concurrency, and the JWT cache (expiry, claim isolation, rejection of bad tokens). The new concurrent middleware code passes under `-race`.
* `go vet ./...` is clean; the module builds for linux/amd64, windows/amd64, darwin/arm64 and linux/arm64. `go.mod` and `go.sum` are untouched.
* **Test failures that were already there** (they fail identically on the zip you uploaded): `TestSPAMinifiedMatchesSource` (root; `spa.min.js` is 19,870 bytes committed vs 19,868 regenerated), `TestNoToolReachesTheFilesystemOutsideResolvePath` (`internal/mcp`; a source check that flags `os.Chdir`/`os.WriteFile` without `resolvePath` in `capture.go` and `provision_image.go`) and `TestParallelStepsShareOneLevel` (`workflow`). No other test fails.

## 7. Not changed, and why

* The per‑request copy of the header block in the default mode, and `WriteString`'s `[]byte(s)` copy: they back the documented guarantee that retained strings stay valid (`SetZeroCopyHeaders(true)` already removes the first).
* `Header map[string]string` and `url.ParseQuery` in the parser: exported field types, so changing them is an API break.
* Merging the pooled request/context/response objects: `sync.Pool` is about 1–2% of CPU in the profiles.
* `WithReusePort` and edge‑triggered I/O in gnet: not measurable on one CPU.
* BreezeORM, WebSocket and the static file server were out of scope.

## 8. Worth knowing

* `internal/generator` emits `router.Use(middleware.LoggingMiddleware())` into new projects, so request logging sits on the hot path of generated apps. `LoggingMiddlewareWith(LoggingOptions{Buffered: true})` is the larger lever there.
* `NewRouter()` defaults `autoServeRoot: true`: `GET /` with no `/` route goes down the blocking auto‑index path (worker‑pool dispatch). An API‑only service that is probed on `/` should register an explicit `/` route so it stays inline.

## 9. Files

Modified: `router.go`, `breeze.go`, `context.go`, `request.go`, `middlewares/logger.go`, `compression.go`, `cache.go`, `jwt.go`, `diag_probes.go`.
New: `router_trie.go`, `pollyield.go`, `static_union.go`, `middlewares/logger_util.go`, `accept_encoding.go`, `jwt_cache.go`, their tests, `router_rest_bench_test.go`, `middlewares/etag_bench_test.go`, and `perf/` (this harness, a separate Go module, so `go build ./...` ignores it). Line endings are CRLF like the rest of the tree.

## 10. Reproducing

See `perf/README.md`. In short: `perf/run_ab.sh /path/to/original /path/to/this/checkout plain json rest`.
