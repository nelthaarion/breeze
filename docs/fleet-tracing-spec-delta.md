# Fleet Tracing — Spec/Repo Delta

**Status:** awaiting owner approval. No implementation code written yet.
**Scope:** every point in the Fleet Tracing spec whose premise does not hold against
the code at `317b7c0`, plus answers to §17's open questions that the repo itself
settles.

Each item is: **(1)** the spec text that is wrong, **(2)** what the code actually
does (verified by reading it, not inferred), **(3)** the fix with the smallest
blast radius on the rest of the spec.

Deliberately untouched, because nothing below undermines them: §4 `TraceContext` /
`Baggage` (already implemented and passing), §6.2 `Span`'s wire shape, §7 sampling,
§8.2 storage bounds, §9B root-cause/blast-radius, §12's benchmark *targets*, and
the whole of `httptransport`. Those are all sound as written.

---

## Severity summary

| # | Spec section | Premise | Severity | Blast radius if unfixed |
|---|---|---|---|---|
| 1 | §5A.5, §5A.0, §10, §11.8 | Events bus has `Publish`/`Subscribe`/topics/`Meta`/`Backend` | **Blocker** | The *default* transport cannot be written at all |
| 2 | §5.2, §5A.3, §14.14 | `breeze.ClientRequest` + outbound conn pool exist | **Blocker** | `gnettransport`, one carrier, one interop test |
| 3 | §9.1, §9C.5, §9.2 | SPA has hash routing + capabilities payload | **Blocker** | All Fleet View UI, deep links, nav gating |
| 4 | §9A | A JSON-Schema validator / OpenAPI parser is reusable | **Blocker** | The headline differentiator |
| 5 | §11.2, §11.3, §9A.3 | `auth`/`mask` helpers are reusable cross-package | **Major** | Security reqs say "reuse, don't reimplement" — impossible today |
| 6 | §8.1, §5.1, §6.1 | `breeze.App`, `breeze.Middleware`, `ctx.Locals`, `PushLog` arity | **Minor** | Mechanical signature fixes |
| 7 | §5A.0 vs §5A.2/§5A.6/§5A.7 | Priority order is self-consistent | **Major** | Build order is circular; "default" is ambiguous |
| 8 | §12.4, §14.4 | A 50k-rps harness and `breeze bench` exist | **Unverified** | Two acceptance numbers may be unmeasurable |

---

## 1. The events bus does not have the API the default transport is built on

**Blocker. This is the single most consequential item: §5A.0 makes this transport
priority 1 and the framework-wide default.**

### (1) What the spec says

> §5A.5.1: "the transport uses `events.Default` … exactly as-is:
> `bus.Publish("fleet.spans", ...)`, `bus.Subscribe("fleet.spans", ...)`"

> §5A.5.2: "Breeze already ships a general-purpose Event Bus … already meant to sit
> in front of pluggable network backends. `eventtransport`'s networked mode **reuses
> that existing backend-adapter abstraction directly** rather than inventing its own
> network protocol — this … **removes an entire custom protocol from the
> implementation surface**."

> §5.2: `fleet.EventMetaCarrier(m events.Meta) Carrier`
> §8.1: `EventsBus events.Bus`

### (2) What the code does

`events` is a **type-keyed, in-process, generics-based** bus. **The Go type is the
topic.** There are no topic strings anywhere in the package.

The entire dispatch surface (`events/api.go`) is package-level generic functions,
because Go forbids generic methods:

```go
func On[T any](sample T, fn Handler[T]) *Subscription[T]   // + OnBus(bus, …)
func Emit[T any](event T) error                            // + EmitBus(bus, …)
func EmitAsync[T any](event T) error                        // + EmitAsyncBus(bus, …)
func Off[T any](id uint64) bool                             // + OffBus[T](bus, …)
var Default = New()
func New(cfg ...Config) *Bus
```

Verified absent, by grep across `events/`:

- **No `Publish`. No `Subscribe`.** No topic strings at all.
- **No `events.Meta`** — so `EventMetaCarrier` has nothing to wrap. There is no
  envelope type carrying arbitrary key/values; the payload *is* the user's struct.
- **No `events.Backend`, no adapter interface, and zero NATS/Kafka/RabbitMQ code.**
- `Bus` is a **struct, not an interface** — `EventsBus events.Bus` (§8.1) will not
  compile as written; it must be `*events.Bus`.

The load-bearing sentence in §5A.5.2 is therefore **exactly inverted**: there is no
abstraction to delegate to, so choosing events *adds* a custom protocol plus three
broker adapters rather than removing them. §5A.5.2's own `memory` tier concedes this
("the only tier that ships a bespoke wire protocol") — in reality that is the *only*
networked tier that exists, and it must be built from scratch.

This answers **§17.7** definitively: (a) no adapter interface exists under any name;
(b) no broker adapters ship today — all three are new work; (c) `events.Default` is
in-process-only, and there is no broker-backed bus constructor to name.

### (3) Proposed fix

Keep events as the idiomatic **in-process** path, where it genuinely is cheapest and
genuinely idiomatic, and stop treating "networked events" as a distinct transport:

- **In-proc mode (§5A.5.1):** implement against the real API. Declare exported
  payload types and let the type be the topic — no topic strings, so `spans_topic` /
  `heartbeat_topic` in §10 become no-ops for this mode and should be dropped from it:
  ```go
  events.EmitBus(bus, fleet.SpanBatch{Spans: spans})
  events.OnBus(bus, fleet.SpanBatch{}, func(ctx *events.Context, b fleet.SpanBatch) error { … })
  ```
  In-proc propagation needs **no `Carrier` at all** — `TraceContext` rides as a typed
  field on the emitted struct. `EventMetaCarrier` should be **deleted from §5.2** and
  replaced with a generic `fleet.MapCarrier map[string]string`, which covers every
  "some protocol with a string bag" case including any future broker.
- **Networked mode (§5A.5.2a):** note that "JSON-over-WS to the aggregator's
  `/fleet/ws`" is *identical in substance* to `wstransport` (§5A.4). Implement that
  export path **once**, in `wstransport`, and have networked-events delegate to it
  rather than shipping a second copy of the same protocol under a different envelope.
- **Broker tiers (b/c/d):** these are three new adapters against libraries the repo
  has never depended on. They are independent of everything else in the spec and
  should be split into a follow-up PR, not gate v1.
- **Default transport:** `httptransport` for v1, because it is the only transport
  that works with zero new protocol *and* zero new dependencies. Revisit once the
  WS export path is proven by the conformance suite.
- **§11.8's events clause** resolves cleanly: in-proc has no network boundary, so
  token auth is N/A; networked is WS, so it uses §5A.4's handshake token. There is no
  "signed field in the event metadata" to build, because there is no metadata.

---

## 2. There is no Breeze HTTP client, and no outbound connection pool

**Blocker for `gnettransport` (priority 3) and §14.14's interop test.**

### (1) What the spec says

> §5.2: `fleet.BreezeRequestCarrier(r *breeze.ClientRequest) Carrier`
> §5A.3: "a client built on the same connection-pooling and buffer-reuse primitives
> gnet/Breeze already use server-side… Requires: a client-mode wrapper around
> whatever connection-pool type Breeze already exposes internally (check
> `breeze.go`/`router.go` for an existing primitive before writing a new pool from
> scratch)."

### (2) What the code does

I checked as instructed. **`breeze.ClientRequest` does not exist.** There is no HTTP
client type, no dialer, and no outbound connection pool anywhere in the root package.
gnet is used **server-side only** — it accepts connections, it does not make them.
`pool.go` pools server-side request/response objects, not connections.

So §5A.3 is not "wrap an existing primitive"; it is "write an HTTP/1.1 client and a
connection pool from scratch" — a substantial, high-risk component (keep-alive,
timeouts, chunked encoding, connection reuse under concurrency), and the thing most
likely to produce subtle production bugs of anything in this spec.

### (3) Proposed fix

- Drop `BreezeRequestCarrier` from §5.2. `fleet.MapCarrier` + `HTTPHeaderCarrier`
  cover every case it was for.
- Defer `gnettransport` out of v1 and mark it "planned" in the docs, exactly as §5A.6
  already permits for gRPC. Its stated value is *"lower per-call allocation
  overhead"* — an optimization, and §5A.3 itself says not to recommend it unless the
  benchmark shows a win. Writing a bespoke HTTP client to *maybe* win an allocation
  benchmark is the worst risk/reward ratio in the spec.
- §14.14 (gateway-on-http calling auth-on-gnet) is then unrunnable as written.
  Smallest substitution that still proves the wire-compat claim: run the interop test
  http↔ws, which exercises two genuinely different transports against one aggregator.

---

## 3. The dashboard SPA conventions §9 builds on do not exist

**Blocker for all Fleet View UI (§9.1, §9.2, §9C.1's search box, §9C.5's deep links).**

### (1) What the spec says

> §9.1: "Page is hidden from the SPA nav exactly like Video is hidden … same
> convention, same code path style (`spajavascript.go` conditionally renders nav items
> based on a capabilities payload the backend sends on initial load; add a
> `fleet_enabled: bool` field to that payload)."

> §9C.5: "Implement via the existing SPA's client-side routing convention (inspect
> `spajavascript.go` for how the other 14 pages already do hash-based routing; extend
> the same router table rather than introducing a new routing mechanism)."

### (2) What the code does

`dashboard/spa.go`, `spajavascript.go` and `spa_css.go` are **dead code**.
`dashboard.SPA()` is defined at `dashboard/spa.go:16` and has **zero callers**
repo-wide; `spaJS`/`spaCSS` are referenced only from inside `SPA()` itself.

The live UI is **server-rendered Go templates** plus
`dashboard/templates/public/dashboard.js` (~1600 lines).

Consequently:
- There is **no hash-based router table** to extend. `grep -i capabilit` across
  `dashboard/` returns **0 results** — there is **no capabilities payload** to add
  `fleet_enabled` to.
- The Video page is therefore *not* hidden by the mechanism §9.1 describes. §9.1 and
  §9C.5 both instruct me to edit files that are not on the execution path, which
  would produce a Fleet View that compiles, ships, and is invisible at runtime.

### (3) Proposed fix

Needs an owner decision, because the cheap options differ in taste and I should not
pick unilaterally:

- **(a) Recommended** — follow the *live* convention: add Fleet View as a
  server-rendered template page + a section in `templates/public/dashboard.js`, gate
  the nav item on a `FleetEnabled` field in the existing template view-model, and use
  real paths (`/dashboard/fleet/trace/<id>`) instead of §9C.5's `#/fleet/...` hashes.
  Smallest diff; matches what actually runs; deep links still work.
- **(b)** Build the hash router and capabilities payload the spec assumes, in
  `dashboard.js`. Honors §9C.5's URL shapes literally, but it is new UI
  infrastructure the other 14 pages don't use — inconsistent by construction.
- **(c)** Revive the dead `spa*.go` files. Not recommended: it means shipping a
  second, competing UI.

I also recommend the `dashboard → fleet` seam stay **string-keyed** (§9C.2 reads the
trace id out of `ctx.Get("...")` rather than importing `fleet`). `fleet` must import
`dashboard` for `dashboard.TimelineStep` (§6.2), so any import back from `dashboard`
into `fleet` is an **import cycle**. Worth stating explicitly in the spec.

---

## 4. There is no JSON-Schema validation, and nothing that parses a fetched OpenAPI doc

**Blocker for §9A, the feature the spec calls "THE DIFFERENTIATOR".**

### (1) What the spec says

> §9A.3: "Validate using a JSON-Schema validator against the relevant operation's
> request/response schema, resolved from the callee's OpenAPI document by matching
> `Route`+`Method` to the corresponding `paths` entry."

### (2) What the code does

Grepped repo-wide for `jsonschema|json-schema|validate schema|\$ref|additionalProperties|\$schema`:
**zero hits in Go source.** No third-party validator is vendored, and there is no
`vendor/` directory.

The critical distinction: `swagger/` **generates** a document —
`func Generate() []byte` — from registered routes. Generation is not validation, and
there is **no parser** that turns a *fetched* `openapi.json` back into anything a
validator could walk. §9A needs both halves, and the repo has neither.

`binding/validate.go` is struct-tag request binding for known Go types — it cannot
validate an arbitrary payload against a schema fetched at runtime.

### (3) Proposed fix

§16 already carves out this exact exception ("A JSON-Schema validator for §9A is the
one likely exception … propose a single minimal, well-maintained dependency … before
adding it"). I am invoking it and **asking before adding**, per §17.4.

- **Recommended:** `santhosh-tekuri/jsonschema/v6` — the most standards-complete Go
  validator (full 2020-12 support, which OpenAPI 3.1 requires), no transitive deps,
  actively maintained.
- Alternative if you want zero new deps: restrict v1 to a **hand-rolled subset**
  (`required`, `type`, `enum`, `additionalProperties` — exactly the four `Violation.Rule`
  values §9A.3 lists). Feasible, but §14.11 makes false positives a correctness gate,
  and hand-rolled `$ref` resolution is where that will break.
- Either way I still need a small OpenAPI **doc parser** (paths → operation →
  request/response schema). Reusing `swagger/types.go`'s structs for *decoding* is
  the cheapest route; I'll confirm they round-trip before relying on it.

**Also unspecified, and needed before §9A can be built:** §9A.1 says spans carry
"a reference to the captured request/response shape", but §6.2's `Span` has **no
payload field**, and §5.1 never captures a body. Payload capture — bounded size, and
redacted at the source per §11.6 — has to be added to §6.2 and §5.1 for §9A to have
any input at all.

---

## 5. The auth and mask helpers §11 says to reuse are unexported

**Major: §11.2/§11.3 mandate reuse, and reuse is currently impossible.**

### (1) What the spec says

> §11.2: "require the same constant-time Basic Auth pattern already implemented in
> `dashboard/auth.go` — **reuse that code, don't reimplement**."
> §11.3: "same masking philosophy as `dashboard/mask.go`… reuse existing regex/logic,
> **don't duplicate**."
> §9A.3: "reuse `dashboard/mask.go`'s field-name matching, extended from headers to
> JSON body field names".

### (2) What the code does

The only exported auth entry point is:

```go
func AuthMiddleware(cfg Config, sessions *sessionStore) breeze.HandlerFunc
```

It takes an **unexported** `*sessionStore`, so no external package can call it — and
it is session-cookie-first with Basic Auth only as a fallback, not the standalone
constant-time Basic Auth §11.2 describes.

Masking is likewise unexported: `maskHeaders(cfg Config, …)` and
`maskLine(cfg Config, …)`. Both are also keyed on `dashboard.Config`, which
`fleet/aggregator` has no reason to construct.

Also, §11.4 asks me to mirror the dashboard's startup warning for empty credentials
"matching how the dashboard already warns". It does **not** warn — the
"both must be non-empty" rule exists only as a doc comment, silently applied in the
`cfg.Username == "" || cfg.Password == ""` branch above. There is no wording to mirror.

### (3) Proposed fix

Smallest change that satisfies the spec's intent without touching dashboard behavior:

- Extract the constant-time Basic Auth comparison into a small exported helper
  (e.g. `dashboard.BasicAuthCheck(user, pass, gotUser, gotPass) bool`) and have both
  the existing middleware and `fleet/aggregator/auth.go` call it. Pure refactor, no
  behavior change, keeps §14.7 (dashboard tests pass unmodified) green.
- Export a config-free field-name matcher (e.g. `dashboard.IsSensitiveFieldName(string) bool`)
  over the existing sensitive-name list, so both `Span.Error` scrubbing (§11.3) and
  §9A's JSON body redaction share one source of truth.
- For §11.4, **write** the warning and treat this doc as the place its wording is
  defined, since there is no precedent to copy.

---

## 6. Mechanical signature corrections

**Minor — no design impact, but the spec's code blocks won't compile as written.**

| Spec | Actual |
|---|---|
| `breeze.App` (§8.1 `InstallAggregator`) | **`*breeze.Breeze`** — there is no `App` type |
| `breeze.Middleware` (§5.1) | **`breeze.HandlerFunc`** (`type HandlerFunc func(*Context) error`) |
| `ctx.Locals` (§5.1, §9C.2) | **`ctx.Set(key string, val any)` / `ctx.Get(key) (any, bool)`** — no `Locals`, no `Values` |
| `coll.PushLog("fleet", ...)` (§6.1) | **`PushLog(level, message, source string)`** — three args; "fleet" is the *source*, not the first arg |
| `events.Bus` as a field type (§8.1) | **`*events.Bus`** — `Bus` is a struct |
| "make [ringBuffer] generic over `T` if it isn't already" (§6.1) | Already `ringBuffer[T any]`, but **unexported** — cross-package reuse needs an export or a local copy |

On the ring buffer: I'd rather **copy** the ~70-line generic buffer into `fleet` than
export `dashboard`'s. Exporting it makes an internal storage detail of the dashboard
part of its public API forever, to save 70 lines. §6.1 permits the copy; I'll note it
in the PR.

Also worth fixing while there: §5.1 says to install `fleet.Middleware` **before**
`coll.Middleware()`, and §5.1.4 says to reuse the dashboard's timeline steps. But
`dashboard.Middleware` only attaches a `TimelineRecorder` when
`timelineEnabled && c.hub != nil && c.hub.clientCount() > 0` — i.e. **only while
someone has the dashboard open in a browser.** So "reuse the timeline the dashboard
already captured" yields `nil` in production whenever nobody is watching. §6.2's
`Timeline` field will be empty in exactly the case that matters. This needs a
decision: either fleet's sampling forces the recorder on, or §6.2's timeline is
documented as best-effort/dashboard-open-only.

---

## 7. §5A.0's priority order contradicts three other sections

**New in this revision of the spec. Major, because it makes the build order circular.**

1. **"Default" is now ambiguous.** §5A.0 ranks `http` **priority 5 (lowest)** and
   says to build it **last**. But §5A.2's own body still reads: *"**This remains the
   default** and the one the example app and all benchmarks in §12 are measured
   against."* Two sections, two different defaults.
2. **The build order is circular.** §5A.0 says build top-to-bottom (events first,
   http last). §5A.7 says the conformance suite validates every transport *"against
   behavior httptransport already defines in §4–§6"*, and §5A.2 calls it the
   "correctness baseline". A baseline that is built last cannot validate the four
   transports built before it.
3. **gRPC's priority is stated twice, differently.** §5A.0 puts gRPC at 4 and http at
   5 ("lowest"); §5A.6's body still says gRPC is *"**lowest priority** of the five"*.

### Proposed fix

Separate the two things "priority" is conflating — **build order** and
**recommendation order**:

- **Build order:** `httptransport` first, always. It is the baseline the conformance
  suite is defined against, it needs no new protocol and no new dependency, and
  §5A.0's own rationale (c) — "which transport gets engineering attention first if
  time is constrained" — is best served by first shipping the one transport that
  cannot fail to work.
- **Recommendation order:** keep §5A.0's list verbatim as *what docs steer users
  toward*, once each transport exists and passes conformance.
- Fix the three contradictory sentences in §5A.2 and §5A.6 so "default" and "lowest
  priority" each appear once, in one meaning.

This is the one item where I'm pushing back on an explicitly-decided instruction
("do not re-litigate"). I'm flagging it rather than silently reordering because
§5A.0's ordering is not just a preference here — following it literally means
building four transports against a baseline that doesn't exist yet, and one of those
four (events-networked) has no working API to build on at all (item 1).

---

## 8. Two acceptance numbers may have no harness (unverified)

- §12.4 targets "under 3% p99 latency overhead **at 50k rps** on the existing
  benchmark harness the repo's prior code-review work already established". I found
  only Go micro-benchmarks (`router_bench_test.go`, `zzperf_bench_test.go`). I have
  **not** confirmed a 50k-rps load harness exists. If it doesn't, that number needs
  either a new harness (non-trivial, and load-generator-dependent) or restating as a
  micro-benchmark delta.
- §14.4 references "the repo's existing `breeze bench` CLI capability". There is a
  `cmd/breeze/` tree, but I have **not** verified a `bench` subcommand. §14.4's own
  fallback ("or a standalone `go test -bench`") is what I'd use.

Neither blocks the spine; both need confirmation before they can be *acceptance*
criteria.

---

## §17 answers the repo settles

- **§17.2 (SpanStore seam):** agreed, and I'd do it regardless — define `SpanStore`,
  ship only the in-memory ring-buffer impl. Cheap now, expensive later.
- **§17.3 (sharding helper):** **does not exist.** `router.go` has
  `byMethod [nBuckets]methodIndex`, which is a per-HTTP-method route index, not a
  general sharded map. Proposal: a small local sharded map in
  `fleet/aggregator/store.go` behind `SpanStore`, ~40 lines, no new dependency.
- **§17.4 (JSON-Schema lib):** see item 4 — asking, per §16.
- **§17.5 (log fan-out auth):** taking the stated default — single shared secret,
  same trust model as `IngestToken`.
- **§17.6 (example inbound/outbound):** inbound on Breeze/gnet, outbound via
  `net/http`. Confirmed as the only option today, given item 2.
- **§17.7 (event bus adapters):** answered in full in item 1 — (a) no interface under
  any name, (b) no adapters exist, all new work, (c) `Default` is in-process-only and
  there is no broker-backed constructor.
- **§17.8 / §17.10 (example default + broker order):** contingent on item 1. If the
  events redesign is accepted, the example ships on `http` for v1 and gains `ws` once
  that lands; brokers move to a follow-up.
- **§17.9 (gRPC stubbed):** recommend yes — stub and document, per §5A.6's own escape
  hatch.

---

## Recommended v1 scope (the approved "option 1" spine)

Everything here is implementable today, on stdlib + verified APIs, with **no new
dependencies**:

1. `fleet/span.go` — `Span` (+ `Tags`), `Heartbeat`, `SpanBatch`
2. `fleet/tags.go` — `fleet.Tag(ctx, k, v)`, caps + truncation counters (§9C.1)
3. `fleet/transport.go` — `Transport`, `Carrier`, `MapCarrier`, `HTTPHeaderCarrier`, `Wrap` type switch
4. `fleet/sampling.go` — §7, incl. always-sample-errors
5. `fleet/tracer.go` — ring buffer, async flush, heartbeat tick, backoff, `Close`
6. `fleet/middleware.go` + `fleet/client.go` — §5.1, `Inject`/`Extract`, `PropagateFromHTTP`, `WrapClient`
7. `fleet/transport/httptransport` + `fleet/transport/conformance_test.go` (§5A.7)
8. `fleet/aggregator/` — `SpanStore` + sharded in-memory store, registry, assembly, skew flagging, topology, REST, WS hub, auth
9. `fleet/aggregator/rootcause.go` + `blastradius.go` (§9B — pure math, no blockers)
10. `fleet/bench_test.go` (§12.1–12.3, 12.5, 12.7) + `-race` + integration/chaos tests

**Deferred pending the decisions above:** eventtransport (item 1), wstransport
(sequenced after http), gnettransport (item 2), grpctransport (§5A.6), contracts
(item 4), all Fleet View UI (item 3), broker adapters, `cmd/fleet-example`.

---

## Decisions I need from you

1. **Events (item 1):** accept the in-proc-only redesign + `http` as v1 default, and
   move networked-events onto the single WS export path?
2. **UI (item 3):** option (a) live server-rendered convention, (b) build the hash
   router the spec assumes, or (c) revive `spa*.go`?
3. **JSON-Schema (item 4):** approve `santhosh-tekuri/jsonschema/v6`, or hand-roll the
   four-rule subset? And confirm payload capture gets added to §6.2/§5.1.
4. **Exports (item 5):** OK to add the two small exported helpers to `dashboard`
   (`BasicAuthCheck`, `IsSensitiveFieldName`) as pure refactors?
5. **Build order (item 7):** OK to build `httptransport` first as the conformance
   baseline, keeping §5A.0 as the *recommendation* order?
6. **Timeline reuse (item 6, last para):** should fleet force the timeline recorder on
   for sampled requests, or is an empty `Span.Timeline` when nobody's watching
   acceptable and documented?
