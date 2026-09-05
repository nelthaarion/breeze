# Repository structure and conventions

The rules this repository follows. Written down because the audit in
`repository-audit.md` found seven kinds of drift that all had the same cause:
nobody had ever stated the rule, so each new feature picked a reasonable answer
and the answers disagreed.

Every rule below is followed by *why*, because a rule without a reason is a rule
the next contributor is entitled to ignore.

---

## 1. Where code goes

### Repo root — `package breeze`

The HTTP core and nothing else: router, `Context`, request/response,
`Breeze` server, worker pool, WebSocket engine, template engine, i18n,
error type, Auto-MCP route registration.

A file belongs at root only if it is part of serving an HTTP request or is
required to construct the server. Everything else is a subpackage, even when
that means a package with three files.

*Why:* the root package is what `import "github.com/nelthaarion/breeze/v2"` gets,
and every symbol in it is in every user's namespace. Adding to it costs
everyone; adding a subpackage costs only its importers.

### Subdirectories of root — public subsystems

One directory per subsystem, importable by user code:
`binding`, `client`, `dashboard`, `diag`, `events`, `fleet`, `mcp`,
`middlewares`, `migrate`, `observability`, `rpc`, `scalar`, `video`, `workflow`.

A subsystem gets its own package when it has a distinct API surface a user
imports directly, and can be left out of an application entirely.

Nesting is allowed one level deeper when the nested packages are *alternatives*
rather than *layers* — `fleet/transport/{httptransport,wstransport,eventtransport,gnettransport}`
are four implementations of one interface, chosen by configuration. Do not nest
to express "part of": that is what files in a package are for.

### `internal/` — machinery with no public API

`internal/generator` (the CLI's implementation), `internal/mcp` (the MCP server's
implementation), `internal/mcpcmd` (flag parsing shared by two binaries).

Go enforces this: nothing outside the module can import them. That is exactly
the point — these packages change shape whenever a tool grows a feature, and no
user should be able to depend on that shape.

The public façade for anything in `internal/` is a thin wrapper: `mcp/` exports
`ServeInProcess` over `internal/mcp`, and `cmd/breeze` exports nothing at all.

### `cmd/` — one directory per binary

```
cmd/<name>/main.go
```

Never a loose `.go` file directly in `cmd/`. Three categories live here and the
name says which:

| Pattern | Meaning | Current members |
|---|---|---|
| `cmd/<tool>` | A shipped binary a user installs | `breeze`, `breeze-mcp` |
| `cmd/<subsystem>-example` | A runnable demonstration of one subsystem | `dashboard-example`, `workflow-example`, `video-example`, `fleet-example`, `templates-example`, `events-example`, `automcp-example` |
| `cmd/<name>` (neither) | A deployable that is not the CLI | `fleet-aggregator` |

*Why the `-example` suffix is load-bearing:* it is how a reader scanning `cmd/`
tells "this is how you use the framework" from "this is the framework". Before
this rule, `cmd/event-validator` and `cmd/fleet-aggregator` were
indistinguishable from examples by name alone.

### Nested modules

A package may carry its own `go.mod` **only** to keep a heavy third-party
dependency out of the framework's dependency graph. Currently one does:
`fleet/transport/eventtransport/backends/kafka`, so that `kafka-go` is opt-in.

The cost is that `go build ./...` and CI do not see it. Any nested module must
therefore be built explicitly by CI (see §6), or it rots invisibly.


---

## 2. File naming within a package

| File | Holds | Required when |
|---|---|---|
| `doc.go` | The package doc comment, nothing else | The package is public and has more than a trivial API |
| `config.go` | The `Config` struct, its defaults, its validation | The package has a `Config` |
| `types.go` | Wire/DTO types with no behaviour | There are enough to make a home worthwhile |
| `errors.go` | Sentinel errors and error types | The package exports either |
| `diag.go` | The `diag` probe and its registration | The package registers a probe |
| `<concern>.go` | One concern | always |

Two file-name styles exist in the repo and both stay:

- `internal/generator` and `internal/mcp` use `snake_case` with a shared prefix
  grouping related files (`tools_*.go`, `generate_*.go`, `features_*.go`,
  `provision_*.go`, `docker_*.go`). *Why:* both packages have 40+ files, and the
  prefix is what makes `ls` readable.
- Everything else uses single flat words (`dispatch.go`, `tracer.go`,
  `middleware.go`). *Why:* under 20 files, a prefix is noise.

Do not mix within one package.

### Test files

- `<file>_test.go` next to the code it tests.
- `bench_test.go` for a package's benchmarks. Not `zzperf_bench_test.go`, not
  `<subsystem>_bench_test.go` — one name, so `go test -bench` is predictable.
  Applied: the root package's `router_bench_test.go`, `zzperf_bench_test.go` and
  `zzrender_bench_test.go` are now one `bench_test.go`; `dashboard/` and
  `middlewares/` renamed theirs. The `Benchmark**ZZ**…` *function* names stay —
  they are the identifiers every recorded baseline in `CHANGELOG.md` is keyed to,
  and renaming them would make a `benchstat` comparison report "no benchmarks"
  rather than a regression.
- Fixtures inline with `t.TempDir()`. A `testdata/` directory only for assets a
  Go test cannot construct (currently: JavaScript harnesses fed to Node).

### Known deviation, kept deliberately

`middlewares/` declares `package middleware` (singular). Every import site
aliases it:

```go
middleware "github.com/nelthaarion/breeze/v2/middlewares"
```

Renaming the directory or the package breaks every existing user's import line.
It stays. Do not "fix" it.

---

## 3. Naming inside code

- **Receivers:** one name per type, everywhere. `*Breeze` is `s`, `*Router` is
  `r`, `*Collector` is `c`, `*Tracer` is `t`. If a file disagrees with its type's
  other files, the file is wrong.
- **Config variables:** `cfg`. Not `config` (shadows the package name in
  `internal/generator`), not `c` (collides with the collector and the context
  receiver).
- **Abbreviations:** `ctx`, `cfg`, `req`, `res`, `err`, `buf`, `n`, `i`. Spell
  out anything not on that list.
- **Sentinel errors:** `ErrFoo`. One exception, `events.Stop`, kept because it is
  exported API a user returns from a listener and its call sites read as English:
  `return events.Stop`. Suppressed in the linter with that reason attached.
- **Do not shadow a package name with a local.** `config`, `template`, `url`,
  `path`, `time` as variable names are all bugs waiting to happen.

---

## 4. Error handling

**Wrap with `%w` when returning an error caused by another error:**

```go
if err := os.WriteFile(path, body, 0o644); err != nil {
    return fmt.Errorf("write %s: %w", path, err)
}
```

*Why:* `errors.Is` and `errors.As` are the only way a caller can react to a
specific failure, and both walk the `%w` chain. `fmt.Errorf("...: %v", err)`
produces the same string and silently deletes that ability.

**Use a bare sentinel when the error is the whole fact:**

```go
var ErrBusClosed = errors.New("events: bus is closed")
```

**Do not wrap when the error text is the product**, not a diagnosis of a cause.
`binding`'s validation errors are shown to an API client as an RFC 9457 body;
there is no underlying `error` and nothing to unwrap. Packages in this category:
`binding`, `middlewares`. Both are documented as such here so the next
contributor does not "fix" them into inconsistency with themselves.

**Prefix with the package name** in errors that escape the package:
`"events: bus is closed"`, `"video: malformed range header"`.

**Never swallow.** If an error genuinely cannot be handled, assign it to `_` with
a comment saying why, so the decision is visible:

```go
_ = _ = conn.Close() // response already written; a close error changes nothing
```

Current state per package, so drift is measurable rather than argued about:
`video` 39/40 wrapped, `workflow` 28/28, `oauth2` 9/9, `migrate` 18/25,
`client` 12/15 — those are the standard. `internal/generator` (29/147) and
`internal/mcp` (46/114) are below it and are being brought up incrementally, not
in one sweep.

---

## 5. Example projects

Every example is a directory under `cmd/` named `<subsystem>-example`, and
contains:

```
cmd/<subsystem>-example/
├── main.go             # package main, with a package doc comment
├── README.md           # required
├── <name>_test.go      # if the example demonstrates a claim, assert it
├── docker-compose.yml  # only if it needs more than one process
└── views/ etc.         # only if it needs assets
```

`main.go`'s doc comment states what the example demonstrates and how to run it,
because that is what a reader sees on pkg.go.dev without opening a file.

`README.md` has three sections, in this order:

1. **What this demonstrates** — the subsystem features exercised, and which
   framework APIs to look at.
2. **How to run it** — the exact command, copy-pasteable, plus any prerequisite.
3. **What to look for** — the URLs to open, the `curl` calls to make, and what
   the output should show. An example whose output nobody described is an example
   nobody can tell is broken.

`cmd/fleet-example/README.md` is the reference: it is the most complex example
(three services, a cross-compile script, a prebuilt-image Dockerfile, a compose
file), so if the shape works there it works everywhere.

**An example that is not compiled is an example that rots.** All of `cmd/...` is
covered by `go build ./...` and `go vet ./...` in CI. Do not add an example
outside `cmd/`, and do not give one its own `go.mod`.

**An example that makes a claim should test it.** Compiling proves the API still
exists; it does not prove the behaviour the README describes. Where an example's
whole point is a guarantee — "an untagged route is never exposed", "the same
middleware runs" — a `_test.go` beside `main.go` turns each claim into an
assertion, and `go test ./...` already covers `cmd/...`.

`cmd/automcp-example` is the reference for this: four claims, four separately
named tests, so a failure says which guarantee broke rather than "the example is
broken". Not every example needs this — `events-example` prints to stdout and
demonstrates nothing a test could usefully hold — but an example that exists to
document a security property does.



---

## 6. Documentation

Every public subsystem has a doc, and every doc is reachable from
[`docs/README.md`](./README.md). The index is the contract: if a subsystem is not
in it, the subsystem is undocumented regardless of what files exist.

Where a doc lives:

- `docs/<topic>.md` for anything spanning packages or describing an operational
  workflow — `fleet-tracing.md`, `mcp-walkthrough.md`.
- `<package>/README.md` for a single package's guide — `events/`, `video/`,
  `workflow/`, `dashboard/`, `observability/`, `middlewares/oauth2/`.
- `<package>/doc.go` for the package comment, always, in addition to the above.

Minimum content for a subsystem doc:

1. What it does, and what problem it solves that the standard library does not.
2. A quick-start code example that compiles.
3. A configuration reference — every field, its default, and what it affects.
4. How it is exposed through the CLI (`breeze add <feature>`) and MCP, or an
   explicit statement that it is not.

`docs/fleet-tracing.md` and `docs/mcp-walkthrough.md` are the quality bar.

---

## 7. Linting

`.golangci.yml` at the repo root, run by `.github/workflows/ci.yml` on every push
and pull request, alongside `go vet ./...` and `go test ./...`.

Enabled beyond the defaults: `staticcheck` (the `SA`/`S`/`ST`/`QF` families) and
`unused`. Those two are what found every finding in §3 of the audit, all of which
had been in the tree for months while CI reported green.

Suppressions carry a reason, and they go at the site rather than in the config.
`//lint:ignore <Check> <why>` — staticcheck's own form, which golangci-lint honours —
or `//nolint:staticcheck // <why>`. A bare `nolint` is not acceptable: the point of a
suppression is to record a decision, and a decision recorded in a config file is
attached to a path rather than to the line that made it.

**`.golangci.yml` currently has no exclusions at all**, which is the target state and
not an oversight. It shipped with two, and both were removed by fixing the code: a
`//lint:ignore` moved the `events.Stop` reason to its declaration, and the one
generator message that actually tripped ST1005 lost a trailing period it did not
need. A blanket `text: ST1005` over a 40-file package would have suppressed every
future instance too — the difference between an exception and a blind spot.

A per-path exclusion is still the right tool for a whole-package property (`-ST1000`
is one, in `settings`), but reach for a site-level ignore first.

Locally:

```bash
gofmt -l .
go vet ./...
golangci-lint run
staticcheck ./...   # the bare binary does not read .golangci.yml
go test ./...
```

`staticcheck ./...` should be clean too. It reads `//lint:ignore` directives but not
`.golangci.yml`, so any `settings`-level exclusion will show up here — currently only
ST1000, the missing package comments being written subsystem by subsystem.

---

## 8. When these rules and the code disagree

The code is wrong, unless the deviation is listed here as deliberate. Add to that
list by editing this file in the same change that introduces the deviation, with the
reason. A deviation nobody wrote down is indistinguishable from a mistake, which is
how this repository accumulated the drift that produced this document.

### The deliberate list

| Deviation | Reason |
|---|---|
| `middlewares/` declares `package middleware` | Renaming either breaks every existing user's import line. See §2. |
| `events.Stop` is not `ErrStop` (ST1012) | It is exported API a listener returns: `return events.Stop` reads as English in a position where `ErrStop` would claim something failed, and it is documented that way in `events/README.md` and every generated listener stub. Suppressed by a `//lint:ignore` at the declaration, not by a config exclusion — the reason belongs where the decision is. |
| The `kafka` transport is a nested module | It pulls a heavyweight client that must not appear in the root `go.sum` for users who do not want it. CI builds and vets it separately. |
| `breeze.go:OnTraffic` (184 lines) and `video/handler.go:serve` (189 lines) stay long | Measured hot paths. Splitting risks the inlining and allocation behaviour the CHANGELOG's benchmark numbers verify. Length here is a consequence of a decision that was tested, not of neglect. |
| `Benchmark**ZZ**…` function names keep the `zz` prefix | Every recorded baseline in `CHANGELOG.md` is keyed to these identifiers. Renaming them would make a `benchstat` comparison against a stored baseline report "no benchmarks" instead of a regression. The `zz` in the old *file* names had no such excuse and is gone. |
| `env`/`envInt` duplicated across the four `cmd/fleet-example` and `cmd/fleet-aggregator` mains | They are separate `package main` programs by design. Sharing them would mean a library package existing only for example glue. |
| `docs/repository-audit.md` cites paths and line numbers that have since moved | It is the evidence for these rules. Rewriting it to match the current tree would destroy what it is for. Its status note says so. |
