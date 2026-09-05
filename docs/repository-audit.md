# Repository audit — structure, naming, dead code, docs

Findings only. Every row cites a path. Nothing in this document is a change; the
changes it justifies are listed in `docs/repository-structure.md` and the
CHANGELOG.

Method: `go list` for the real package graph (not directory guessing), an
import-edge sweep over `{{.Imports}} {{.TestImports}} {{.XTestImports}}` for all
38 packages, `staticcheck 2025.1.1` for reachability, a receiver-name census, a
function-length pass, and a fence-aware Markdown link check.

> **Status: every finding below has been fixed.** The paths and line numbers are as
> they were at audit time, so several no longer resolve — `cmd/main.go` is
> `cmd/api-example/main.go`, `example_template/` is `cmd/templates-example/`, and the
> line numbers of anything in a file that had a deletion above it have moved. That is
> deliberate: this document is the evidence for the rules in
> `repository-structure.md`, and rewriting it to match the current tree would destroy
> the thing it is useful for. The CHANGELOG entry *Repository structure and code
> cleanliness* records what was done about each one.

---

## 1. Top-level layout

`go.mod` is `github.com/nelthaarion/breeze/v2`, so the repo root **is** the
`breeze` package. Go convention: root and subdirectories = public API,
`internal/` = unimportable outside the module, `cmd/` = binaries.

### 1.1 Justified as placed

| Path | Kind | Why the placement is right |
|---|---|---|
| `.` (19 files) | public API | `breeze.Router`, `Context`, `Breeze`. The framework itself. |
| `binding/` `client/` `dashboard/` `diag/` `events/` `fleet/` `mcp/` `middlewares/` `migrate/` `observability/` `rpc/` `scalar/` `video/` `workflow/` | public API | All imported by user code in the README or a generator template. Public placement is correct. |
| `internal/generator` (42 files) | internal | The CLI's implementation. Imported only by `cmd/breeze` and `internal/mcp`. Verified: no external importer. |
| `internal/mcp` (42 files) | internal | Imported only by `cmd/breeze-mcp` and `mcp/`. Correct — `mcp/` is the public façade. |
| `internal/mcpcmd` | internal | Flag parsing shared by `cmd/breeze-mcp` and `cmd/breeze start mcp-server`. |
| `cmd/breeze`, `cmd/breeze-mcp` | binaries | Real shipped binaries; `.goreleaser.yaml` builds them. |
| `fleet/transport/{httptransport,wstransport,eventtransport,gnettransport}` | public API | One package per wire protocol, selected by config. |
| `fleet/transport/eventtransport/backends/kafka` | public API, **separate module** | Own `go.mod` so `kafka-go` stays out of the framework's dependency graph. Deliberate and documented in its package comment. **But** see 3.6 — nothing ever compiles it. |


### 1.2 Placed inconsistently with its own purpose

| Path | Problem | Evidence |
|---|---|---|
| `cmd/main.go` | A `package main` sitting **directly in `cmd/`**, not in `cmd/<name>/`. It is the demo server (`CreateUserRequest`, a chat WebSocket hub, Scalar docs) and is the default Docker target. Every other example is `cmd/<name>-example/`; this one is `./cmd`. | `Dockerfile:16` `ARG BREEZE_TARGET=./cmd`; `docker-compose.yml:7`; `README.md:122` calls it "the example server in `./cmd`" |
| `example_template/` | A `package main` example at the **repo root**, outside `cmd/`. Ships `views/`, `components/`, `locales/` and is picked up by `go build ./...`. Excluded from CodeQL by name, which is itself a sign it does not belong where it is. | `.github/codeql/codeql-config.yml:7` `- example_template/**`; `go list ./...` includes `./example_template` |
| `events/events-app/` | A `package main` example nested **inside the library package** `events/`. Three files (`main.go`, `events.go`, `service.go`). Nothing else in the repo nests a binary under a library. | `go list` shows `./events/events-app`; 0 importers |
| `cmd/event-validator/` | Not a binary a user runs and not an example of an API — it is a **concurrency stress harness** for the event bus (`sync/atomic` counters, 12 `fmt.Print` calls). This is a test, living in `cmd/` because there was no other home. | `cmd/event-validator/main.go:16` `func main` is 109 lines of assertions |
| `cmd/fleet-aggregator/` | Correctly a binary (it is a real deployable), but it is the **only** `cmd/` entry that is neither `-example` nor a shipped CLI, and `.goreleaser.yaml` does not build it. Its placement is right; its discoverability is not. | `.goreleaser.yaml` |

### 1.3 Verified *not* a problem

- **No package is orphaned at the package level.** All 24 library packages have
  at least one in-module importer. The 14 zero-importer entries are `package main`
  (expected) plus `fleet/transport/gnettransport`, which is reachable by
  configuration rather than by import — its `Name() == "gnet"` is how a user
  selects it. Deliberate, not dead.
- `mcp/` (public) vs `internal/mcp` (private) vs root `mcp_server.go` /
  `mcp_route.go` (Auto-MCP) look like three copies of one thing. They are three
  different things and `mcp/inprocess.go:15-30` already explains the distinction
  in its package comment. No change warranted.
- Root `diag.go` (the core's own probes, `package breeze`) vs `diag/` (the
  registry) is a legitimate split, not duplication.

---

## 2. Naming

### 2.1 Real inconsistencies

| Concern | Finding |
|---|---|
| **Directory ≠ package name** | `middlewares/` declares `package middleware` (singular). Every import in the repo needs an alias: `middleware "github.com/nelthaarion/breeze/v2/middlewares"` — see `cmd/main.go:8`, `example_template/main.go:12`, `README.md:88`. This is the only such mismatch in 38 packages. Renaming either side breaks every user's import line, so it stays; it needs to be *stated* as a known wart rather than rediscovered. |
| **`config.go` placement** | 6 of 24 library packages isolate config in `config.go` (`dashboard`, `events`, `fleet/aggregator`, `internal/generator`, `middlewares/oauth2`, `video`). The rest bury it in the main file: `fleet.TracerConfig` is in `fleet/tracer.go`, `client.Config` in `client/client.go`, `rpc` has `types.go` but its `Server` options are in `server.go`. |
| **`doc.go` placement** | Only 4 packages have one (`events`, `rpc`, `video`, `workflow`). 9 packages have **no package doc comment at all**: root `.`, `binding`, `dashboard`, `middlewares`, `migrate`, `observability`, `scalar`, `internal/generator`, `fleet/contracts`. The root package — the framework's front door — is among them. |
| **File-name case style** | `internal/mcp` (24 snake_case / 18 flat) and `internal/generator` (23/19) use `tools_provision.go`, `generate_grpc_files.go`. `events` (0/19), `fleet` (0/11), `video` (0/11), `rpc` (0/10) use `dispatch.go`, `tracer.go`. Both are idiomatic Go; the repo simply never picked one. |
| **Benchmark file names** | Four conventions coexist: `bench_test.go` (`events`, `fleet`, `rpc`, `video`, `workflow`, `observability`), `router_bench_test.go` (root), `bench_limits_test.go` (`internal/mcp`), and `zzperf_bench_test.go` / `zzrender_bench_test.go` (root, `dashboard`, `middlewares`). The `zz` prefix was a sort-order hack from the perf pass and has no meaning. |
| **`testdata/`** | Two exist (`testdata/`, `dashboard/testdata/`), both holding JS harnesses. Every other package builds fixtures inline with `t.TempDir()`. Not wrong, just unexplained. |
| **Receiver names** | Only two types disagree with themselves across files: `*Breeze` is `s` in `breeze.go`/`diag.go`/`mcp_server.go`/`error.go` but `b` in all 10 methods in `websocket_engine.go`. `*route` is `r` in `router.go:229-234` but `rt` in `router.go:441`. Everything else in the repo is consistent. |
| **Sentinel error name** | `events.Stop` does not match `ErrFoo` (ST1012). Flagged by staticcheck. It is exported API returned by user listeners (`return events.Stop`) and documented in `events/README.md`; renaming is a breaking change for zero benefit. Needs a suppression with a reason, not a rename. |

### 2.2 Cross-package duplicate helper names

Same name, different package, **different behaviour** — the dangerous kind:

| Name | Copies | Divergence |
|---|---|---|
| `humanBytes` | `internal/mcp/tools_live.go`, `middlewares/diag_probes.go`, `video/diag.go` | The MCP copy prints `KiB/MiB/GiB`, the other two print `KB/MB/GB`. Identical arithmetic, different units, all three feeding the same dashboard diagnostics page. |
| `toSlug` | `internal/generator/migrate.go`, `migrate/parse.go` | The generator copy handles acronym runs (`HTTPServer` → `http_server`); the `migrate` copy inserts `_` before every capital (`HTTPServer` → `h_t_t_p_server`). Both name migration files. |
| `msOf` | `events/diag.go`, `workflow/engine.go` | `float64(d.Microseconds())/1000` vs `float64(d)/float64(time.Millisecond)`. Same result, two roundings. |
| `firstLine` | `internal/mcp/live.go`, `scalar/llms.go` | Truncates to 160 chars with an ellipsis vs no truncation. |
| `itoa` | `dashboard/session.go`, `middlewares/oauth2/cookie.go` | The oauth2 copy **does not handle negatives**. Used for cookie `Max-Age`, which is never negative in that call site — correct by accident. |

---

## 3. Dead code and orphans

### 3.1 Unreachable identifiers (staticcheck U1000, full-module sweep)

| Path | Symbol | Verdict |
|---|---|---|
| `dashboard/api.go:787` | `jsonStringField` | Dead. Its own comment claims it avoids `encoding/json` "on the hot path" while calling `strings.Index(string(body), …)`, which allocates a copy of the body. Superseded. |
| `dashboard/cpu.go:31` | `currentMaxProcs` | Dead, **and its comment describes a different function**: "numCPU is captured at package init" — there is no `numCPU`. |
| `dashboard/embed.go:26` | `templatesDir` | Write-only. `dashboard/api.go:79` assigns it; nothing reads it. |
| `dashboard/sampler.go:199` | `procPid` | Dead. Comment: "pid is cached so we don't re-read on every sample" — nothing samples it. |
| `dashboard/collector.go:75` | `requestsPerSec` field | Dead field. Comment says "updated by sampler"; `sampler.go:131` computes `rps` into the *snapshot struct* and never touches the field. |
| `fleet/tags.go:102` | `recorder` field | Dead field with a 4-line comment explaining a design that `fleet/middleware.go:333` implements a different way (`timelineStepsFrom`). |
| `internal/generator/fields.go:283` | `zeroLiteral` | Dead. |
| `internal/generator/registry.go:35` | `blockMarkers` | Dead one-line wrapper over `markersFor`. |
| `internal/generator/generate_grpc_files.go:592,608,631,639` | `protoFieldGoType`, `protoMapToGoMap`, `protoFieldElemGoType`, `protoGoPrimitive` | Four dead proto→Go type mappers. |
| `internal/mcp/changeset.go:233` | `(*changeSetStore).open` | Dead. |
| `internal/mcp/ports.go:185` | `(*portAllocator).inUse` | Dead — comment says "for diagnostics and for the allocator's own tests"; no test calls it. |
| `video/resolve.go:36` | `(*mount).resolve` | Dead. Documented as "for callers with no authorisation step in between" — there are none. |
| `observability/observability_test.go:752` | `panickyObserver.calls` | Dead test field. |

### 3.2 Bugs found by the same sweep

| Path | Finding |
|---|---|
| `fleet/aggregator/blastradius.go:142` | **SA4006: `cfg = cfg.withDefaults()` is never read.** `ComputeBlastRadius` takes `cfg Config`, normalises it, and then uses **no field of it** — `Incidents` (line 115) applies the only threshold before calling. The parameter is dead weight and the normalisation is a no-op that reads as if it mattered. |
| `observability/observability_test.go:1059` | **SA4000: `Default() != Default()`** — identical expressions on both sides. The test is named `TestDefaultCollectorIsStable` and asserts a singleton; comparing a call to itself passes whether or not the singleton works. |
| `example_template/main.go:63` | **SA1019: calls deprecated `breeze.NewWorkerPool`.** Every other example, both generator templates, and every test use `NewEventLoopWorkerPool`. This is the only remaining caller outside `workerpool.go` and `context_lifecycle_test.go`. |
| `binding/bind_test.go:207,481,527` | S1021 ×3: `var f Form` then `f = tt.input`. |
| `internal/generator/generate_view.go:54` | ST1005: multi-line error string with trailing punctuation. It is deliberately a multi-line user-facing CLI message; needs a suppression, not a rewrite. |

### 3.3 Committed junk in `HEAD`

Thirteen files, all tracked, all build/debug residue — including one named
`$($f.FullName)`, which is an unexpanded PowerShell interpolation that got
committed as a filename:

```
$($f.FullName)   build.log   t.log   t3.log   vet.log
c_inc.json  c_list.json  c_list2.json  c_logs.json
c_svc.json  c_topo.json  c_tr.json  c_viol.json
```

The `c_*.json` files are captured Fleet aggregator API responses from a manual
debugging session (`c_topo.json` = topology, `c_tr.json` = traces).

### 3.4 Untracked binaries and archives in the working tree


### 3.5 Superseded-design remnants — swept, and mostly clean

- **Pre-Scalar Swagger:** `middlewares/swagger.go` is the only Swagger-named
  file. It contains `SwaggerOptions = ScalarOptions` and `SwaggerMiddleware` →
  `ScalarMiddleware`, both marked `// Deprecated:` with the reason and a
  CHANGELOG pointer, and `internal/mcp/idioms.go:120` actively lints projects
  that use them. This is a correctly maintained compatibility shim, not a
  remnant. **Keep.** The stale bit is the *filename*: `swagger.go` holds
  `ScalarOptions`/`ScalarMiddleware`, and `files/index.html:575` still renders a
  "Swagger" nav entry — but `files/` is gitignored demo content.
- **Pre-error-return `HandlerFunc`:** zero remnants in `breeze.HandlerFunc`
  form. The only `func(ctx *Context)` signatures left are `rpc`'s, where that
  *is* the current signature (`rpc.Handler` has no error return by design), plus
  `pool.go:132`/`rpc/context.go:200` `releaseContext` helpers. Clean.
- `.migration/handlerfix/` — 6 files, own `go.mod`, the one-shot rewriter that
  performed the error-return migration. Gitignored with a 5-line comment already
  explaining why it is kept locally and never committed. Correctly handled.
- `docs/fleet-tracing-spec-delta.md` (24 KB) — a design-review document whose
  own header says "**Status:** awaiting owner approval. No implementation code
  written yet." Fleet has since shipped. It is a historical record of a review,
  not documentation, and it sits in `docs/` where a reader looking for Fleet
  docs will find it next to the real guide.

### 3.6 Stale relative to source / never verified

| Path | Finding |
|---|---|
| `fleet/transport/eventtransport/backends/kafka` | Own `go.mod`, therefore invisible to `go build ./...`, `go vet ./...`, `go test ./...` and every CI job. Nothing has compiled it since it was written. |
| `.github/workflows/ci.yml:35-40` | The golangci-lint step is gated on `hashFiles('.golangci.yml', …) != ''`. No such file exists, so **the lint step has never run**. CI is `go vet` + `go test` only — which is why every finding in 3.1 and 3.2 survived. |
| `spa.min.js`, `dashboard/templates/public/dashboard.min.{js,css}` | Generated by `gen_assets.go`'s `go:generate` directives, committed, and **verified against source** by `TestSPAMinifiedMatchesSource` (skips without Node). Correctly guarded — not stale. |

`1.zip` (1.7 MB), `1 (2).zip`, `breeze-mcp.exe` (20 MB), `media/sample.mp4`,

---

## 4. Example projects

Nine `main` packages. No two share a shape.

| Example | Layout | Doc comment | README | Compose | Assets | Run instruction given |
|---|---|---|---|---|---|---|
| `cmd/main.go` | **loose file in `cmd/`** | none | no | root-level | serves `./files` | `README.md:127` (as a Docker target) |
| `cmd/dashboard-example/` | `main.go` | 17 lines, incl. curl commands | no | no | no | `go run ./cmd/dashboard-example` |
| `cmd/workflow-example/` | `main.go` | 22 lines, incl. curl commands | no | no | no | `go run ./cmd/workflow-example` |
| `cmd/video-example/` | `main.go` + `views/` | 10 lines | no | no | `views/`, creates `./media` | `cd cmd/video-example && go run .` — **the only one requiring a `cd`** |
| `cmd/fleet-example/` | `auth/ gateway/ orders/` + `build.ps1` + `Dockerfile.prebuilt` + `docker-compose.yml` | per-service, 2 lines each | no | **yes** | no | in `build.ps1:8` and `docker-compose.yml:4`, not in the README |
| `cmd/fleet-aggregator/` | `main.go` | 3 lines + env table | no | no | no | `CHANGELOG.md:656` only |
| `cmd/event-validator/` | `main.go` | **none** | no | no | no | **nowhere** |
| `events/events-app/` | `main.go` + `events.go` + `service.go` | **none** | no | no | no | **nowhere** |
| `example_template/` | `main.go` + `views/ components/ locales/` | **none** | no | no | 11 asset files | **nowhere** |

Consistency findings:

1. **Zero of the nine has a `README.md`.** `cmd/fleet-example` is the most
   elaborate (three services, cross-compile script, prebuilt-image Dockerfile,
   compose file) and documents itself in a PowerShell comment header.
2. **Naming splits three ways**: `<subsystem>-example` (dashboard, workflow,
   video, fleet), no suffix at all (`cmd/main.go`, `example_template`,
   `events/events-app`), and `<noun>-<noun>` for things that are not examples
   (`event-validator`, `fleet-aggregator`).
3. **Discovery is impossible for three of them.** `event-validator`,
   `events/events-app` and `example_template` are referenced by no README, no
   doc comment, and no CHANGELOG entry. `example_template` is only mentioned in
   a CodeQL exclusion path.
4. **CI does compile all nine** — `go vet ./...` and `go build ./...` cover
   every package in `go list ./...`, and both are green. So they compile; none
   is *executed* or asserted on. The nested `kafka` module is the sole exception
   and is covered by neither (3.6).
5. `example_template/` is the **only** example calling a deprecated
   constructor (3.2). Being undiscoverable and being stale are the same problem
   twice.

### `example_template/` — what it is

A full server-rendered demo: 4 views with a layout, 5 components, 2 locale
files, live WebSocket log, `runtime` stats. It is a *demo of the template
engine*, functionally overlapping `cmd/video-example`'s `views/` and the
`breeze new --template=views` scaffold. It is current in the sense that it
compiles and its API usage is valid apart from the deprecated pool
constructor — but it is at the repo root, undocumented, unreferenced, and

---

## 5. `docs/` coverage per subsystem

Quality bar, as specified: `docs/fleet-tracing.md` (301 lines — quick start,
migration, features, configuration, transport status, security, architecture,
competitive positioning, performance/limits, running the example) and
`docs/mcp-walkthrough.md` (623 lines — every address kind, every mode, every
scope, with real terminal output).

`docs/` contains exactly four files: the two above, `fleet-tracing-spec-delta.md`
(historical, see 3.5) and `breeze-mcp-config-example.json`. **There is no
`docs/README.md` and no index anywhere.**

| Subsystem | Doc | Lines | Package doc | At the bar? | Linked from root README |
|---|---|---|---|---|---|
| Fleet | `docs/fleet-tracing.md` | 301 | 91 ch | **reference** | yes, `README.md:764` |
| MCP | `docs/mcp-walkthrough.md` | 623 | 80 ch | **reference** | yes (path only, not a link) `:854,940,979,1013` |
| events | `events/README.md` + `doc.go` | 689 | 75 ch | **yes** — exceeds it | yes, `:552` |
| workflow | `workflow/README.md` + `doc.go` | 306 | 76 ch | **yes** | yes, `:641` |
| dashboard | `dashboard/README.md` | 303 | **none** | **yes** | yes, `:669` |
| observability | `observability/README.md` | 525 | **none** | **yes** | **no — unlinked from anywhere** |
| video | `video/README.md` + `doc.go` | 218 | 130 ch | close; no quick-start section | yes, `:604` |
| oauth2 | `middlewares/oauth2/README.md` | 178 | 101 ch | close; thinner config reference | yes, `:517` |
| RPC | `rpc/doc.go` only | 62 | 66 ch | **no** — a doc comment, not a guide | no |
| client | package comment only | — | 108 ch | **no** | no |
| diag | package comment only | — | 149 ch | **no** | no |
| scalar | **nothing** | — | **none** | **no** | no |
| migrate | **nothing** | — | **none** | **no** | no |
| binding | **nothing** | — | **none** | **no** | no |
| middlewares | root README section | — | **none** | **no** — 12 middlewares, no reference | section only |
| HTTP core (`.`) | root README | — | **none** | n/a | n/a |

**Missing or below the bar: `scalar`, `migrate`, `binding`, `rpc`, `client`,
`diag`, `middlewares`.** Seven of sixteen. `scalar`, `migrate` and `binding`
have neither a doc file nor a single line of package documentation, and all
three are in the public API surface — `binding.Bind` is called by every
generated resource handler, `scalar.Generate` produces the OpenAPI document, and
`migrate` is what `breeze migrate` drives.

**`observability/README.md` is 525 lines at full quality and reachable only by
guessing the path.**


---

## 6. README / CHANGELOG accuracy

### 6.1 README defects (line-cited)

| Line | Claim | Reality |
|---|---|---|
| 197 | "**21 of them**, each idempotent" | `breeze add --list` prints "Features (**23**), in the order they are wired". Verified by running it. |
| 204 | `breeze add --list  # all 21` | Same, in a code block a reader will copy. |
| 56 | ToC entry `- [Support the Project](#-support-the-project)` | **No such heading exists.** Dead anchor. |
| 40–41 | `Built-in OpenAPI / Scalar` and `gRPC Code Generation` are indented as children of `Native WebSocket Engine` | All three are `###` siblings (`README.md:443`, `:453`, `:461`). Mis-nesting. |
| 122 | "the example server in `./cmd`" | Accurate but describes the layout wart in 1.2. |
| 123 | "~25 MB image" | Not verifiable without a Docker build; flagged as unverified, not wrong. |
| 854, 940, 979, 1013 | `docs/mcp-walkthrough.md` written as bare text | Every other doc reference is a real Markdown link (`:517`, `:552`, `:604`, `:641`, `:669`, `:764`). Four references a reader cannot click. |
| — | No mention of `binding`, `client`, `scalar`, `migrate`, `rpc`, `observability`, or `fleet` as importable packages | Confirmed by grep: `breeze/binding`, `breeze/client`, `breeze/scalar`, `breeze/migrate`, `breeze/rpc`, `breeze/observability`, `breeze/fleet`, `breeze/dashboard`, `breeze/mcp` appear **0 times** in the README. |
| — | `observability/README.md` never linked | Section 5. |

Link check over all 18 Markdown files: **10 internal links, 0 broken** (after
excluding fenced code blocks — a naïve checker reports 4 false positives from Go
generics like `events.Inspect[UserCreated](bus)`, which is why the checker being
added is fence-aware).

Verified correct: Go version (`README.md:64` = `go.mod` `1.25.13`), the
dependency list (`:71-73` matches `go.mod`), all 11 generator rows against
`breeze help`, and every one of the 5 subsystem-README links. Note `:105` in the
quick start uses `breeze.NewWorkerPool`, which is deprecated — it still behaves
identically, but the README teaches the deprecated spelling.

### 6.2 CHANGELOG

Structure: 14 `##` sections, 6 of them `[Unreleased]`, then `[v1.8.0]` back to
`[v1.3.1]`, then two headings that are not versions at all.

| Finding | Evidence |
|---|---|
| **Six concurrent `[Unreleased]` sections.** Allocation Pass, Security Hardening, Route Descriptions, Handler Error Returns, Subsystem Diagnostics, Native Fleet Tracing. A reader cannot tell what is in a release. | lines 7, 110, 370, 403, 506, 638 |
| **`## Bug Fixes` (line 1724) and `## File Inventory` (line 1952) are `##`-level siblings of version headings.** `Bug Fixes` is a `###`-level concern under v1.3.1; it reads as a release. | |
| **`## File Inventory` is factually wrong now.** It describes a distribution layout — "`breeze-final/`", "Replace your entire `breeze/` directory with these files", "**Original files (7)**: `breeze.go`, `response.go`, `router.go`…" — from when this was shipped as a zip. Every file it lists as "ORIGINAL (unchanged)" has changed many times. | lines 1952-1985 |
| Version typo | `## [v.1.4.1]` — stray dot. Line 1555. |
| **Coverage is genuinely good.** Every subsystem appears: fleet 31 mentions, dashboard 59, MCP 61, rpc 23, events 21, video 16, workflow 15, scalar 15, oauth2 11, observability 9. The thin ones are `binding` (2) and `i18n` (2) — both shipped before the CHANGELOG became detailed. | grep counts |

---

## 7. Code-level cleanliness (Part 3.1)

### 7.1 Duplicated logic

Beyond the seven cross-package helper triplets in 2.2:

| Where | Duplication |
|---|---|
| `cmd/fleet-aggregator/main.go`, `cmd/fleet-example/{auth,gateway,orders}/main.go` | `env` + `envInt`, four byte-identical copies. Cannot be shared without a library package for example glue — the four are separate `main` packages by design, so this is the one duplication that is arguably correct. |
| `dashboard/api_explorer.go:233,237,247,253,270` + `dashboard/api.go:579` | `ctx.JSON(map[string]string{"error": …})` six times, three distinct status-code conventions around it. No `writeError` helper, while `fleet/aggregator/api.go:316` has exactly that helper for the same job. |
| `internal/mcp/docker_names.go` | `validateContainerName`, `validateServiceName`, `validateImageTag` — already correctly factored through one `validateDockerIdentifier`. **Good example**, cited here as the pattern the rest should follow. |
| `internal/generator/generate_*.go` | 11 `generateX` entry points (`generate_model.go:17` 175 lines, `generate_job.go:17` 160, `generate_workflow.go:16` 134, `generate_view.go:19` 121, `generate_ws.go:16` 115) each repeat: resolve paths → check ownership → render template → `ensureImports` → write → print. The shared steps are already helpers; the *sequence* is copy-pasted. |

### 7.2 Oversized functions

48 functions exceed 100 lines. Worst, excluding table-driven tests:

| Path | Function | Lines |
|---|---|---|
| `dashboard/api.go:52` | `registerRoutes` | **196** |
| `video/handler.go:85` | `serve` | 189 |
| `breeze.go:191` | `OnTraffic` | 184 |
| `dashboard/middleware.go:38` | `Middleware` | 183 |
| `internal/generator/generate_model.go:17` | `generateModel` | 175 |
| `internal/generator/generate_job.go:17` | `generateJob` | 160 |
| `cmd/dashboard-example/main.go:239` | `main` | 160 |
| `internal/generator/features_core.go:403` | `migratorRunner` | 154 |
| `internal/mcp/tools_knowledge.go:970` | `suggestNextSteps` | 147 |
| `video/config.go:246` | `newMount` | 145 |
| `internal/mcp/tools_verify.go:226` | `verifyProject` | 142 |
| `fleet/middleware.go:70` | `Middleware` | 139 |

`breeze.go:OnTraffic` and `video/handler.go:serve` are the request hot paths,
where splitting risks inlining and allocation behaviour that was measured — they
are long *and* justified. `dashboard/api.go:registerRoutes` is 196 lines of
sequential route registration with no shared state between blocks: a pure
dumping ground, and the clearest split candidate in the repo.

Oversized files: `internal/mcp/tools_provision.go` (1419 lines, 56 KB, and
`tools=0` — it holds the *orchestrator*, not tool definitions, despite the
`tools_` prefix), `tools_fleet.go` (1141), `tools_plan.go` (1168),

### 7.3 Error-handling drift

Census of `fmt.Errorf` / `%w` / `errors.New` per package:

| Package | `fmt.Errorf` | of which `%w` | `errors.New` | Verdict |
|---|---|---|---|---|
| `video` | 40 | **39** | 10 | Consistent — wraps. |
| `workflow` | 28 | **28** | 16 | Consistent — wraps. |
| `migrate` | 25 | 18 | 0 | Mostly wraps. |
| `client` | 15 | 12 | 6 | Mostly wraps. |
| `fleet/transport/wstransport` | 24 | 17 | 5 | Mostly wraps. |
| `internal/mcp` | 114 | **46** | 10 | **40% wrapped.** Drifts inside one package. |
| `internal/generator` | 147 | **29** | 6 | **20% wrapped.** The worst in the repo, and the package whose errors users see. |
| `binding` | 7 | **0** | 0 | Never wraps — but its errors are user-facing validation messages, not causes. Defensible; undocumented. |
| `middlewares` | 6 | **0** | 0 | Never wraps. |
| `.` (root) | 30 | 16 | 7 | Mixed. |

The framework's dominant style is `fmt.Errorf("context: %w", err)` — that is
what `video`, `workflow`, `migrate`, `client` and `oauth2` (9/9) do. The two
`internal/` packages are the outliers, at exactly the scale where it matters
most.

### 7.4 Under-commented complex logic

Checked the four algorithms named in the brief:

| Algorithm | Path | Verdict |
|---|---|---|
| Port-allocator collision avoidance | `internal/mcp/ports.go` | **Well commented.** |
| Blast-radius / root-cause | `fleet/aggregator/blastradius.go` | **Exemplary.** Every non-obvious step states *why*: BFS chosen over DFS because `Hops` feeds a credibility judgement (`:137-140`); origin pre-seeded as visited so retry cycles terminate (`:155-158`); a gracefully-degrading caller excluded so the banner does not overstate (`:172-176`); attribution capped at the caller's own rate with the failure mode spelled out (`:192-197`). This is the standard the rest of the repo should be held to. |
| Marker/checksum file ownership | `internal/generator/ownership.go`, `registry.go` | Commented, including *why* the anchor is the quoted breeze import. |
| Token generation / scoping | `internal/mcp/scope_token.go`, `token.go` | Commented. |

So the named algorithms are fine. The real gap is elsewhere: **six comments that
describe code that no longer exists** — `dashboard/cpu.go:29` documents a
`numCPU` that was deleted, `dashboard/sampler.go:198` and
`dashboard/embed.go:23` document dead variables, `dashboard/collector.go:75`
claims "updated by sampler" for a field the sampler ignores, `fleet/tags.go:99`
describes an abandoned design, and `dashboard/middleware.go:36` cites "the crash
analysis in PHASE 1-9 of the runtime audit" — a document that is not in this
repository. A comment that confidently describes something untrue is worse than
no comment.

### 7.5 Dead code inside live files

Covered by 3.1 and 3.2 — that sweep *is* this category, and it found no
commented-out code blocks and no unreachable branches anywhere in the module.

Debug printing: 130 `fmt.Print*` calls in non-test source. All accounted for —
`cmd/*` examples (66, intentional CLI output), `internal/generator` (34, CLI
progress), `rpc`/`breeze.go`/`workerpool.go`/`middlewares` panic and logging
paths (10, prefixed `[Breeze][PANIC]` etc.), and `dashboard/api_explorer.go:360-361`,
which are inside a `fmt.Fprintf` template that *generates* Go source for the API
Explorer's snippet feature — a false positive. **No leftover debug prints.**

---

## 8. Summary of what needs doing

Ordered by risk, lowest first.

1. Delete 13 committed junk files (3.3) — zero code impact.
2. Delete 15 unreachable symbols/fields (3.1); fix the 5 real defects (3.2).
3. Fix 6 comments that describe deleted code (7.4).
4. Correct the README's `21`→`23`, the dead ToC anchor, the mis-nesting, and
   make the 4 bare `docs/` references links (6.1).
5. Reconcile the 7 divergent duplicate helpers (2.2), unit-tested at their new
   home.
6. Write the 7 missing/thin subsystem docs and a `docs/README.md` index (5).
7. Give all 9 examples one shape and a README each (4).
8. Move `cmd/main.go`, `example_template/`, `events/events-app/` to conform (1.2)
   — the only changes that touch import paths or build targets.
9. Wire a linter into CI so 3.1/3.2 cannot recur (3.6) — the audit's single
   highest-leverage outcome, since the lint step has never once run.

`tools_knowledge.go` (1191). All four are internally sectioned with `─── ───`
banner comments, which is the seam a split would follow.


excluded from CodeQL. It is `cmd/templates-example` in everything but location.

`files/`. All already matched by `.gitignore`, so none is at risk of being
committed. Listed for completeness only.

| `contains` | `internal/generator/config_validate.go`, `internal/mcp/tools_provision.go` | Identical. Both predate `slices.Contains`. |
| `env` / `envInt` | `cmd/fleet-aggregator`, `cmd/fleet-example/{auth,gateway,orders}` | Four copies, byte-identical except a `v`/`n` variable name. |

