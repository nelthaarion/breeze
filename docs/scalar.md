# OpenAPI / Scalar

An OpenAPI 3 spec generated from route declarations, the Scalar UI to browse it,
and an `llms.txt` for models.

```go
import (
    middleware "github.com/nelthaarion/breeze/middlewares"
    "github.com/nelthaarion/breeze/scalar"
)

router.Use(middleware.ScalarMiddleware(router, middleware.ScalarOptions{
    Title:   "My API",
    Version: "2.0.0",
}))

router.Handle(breeze.POST, "/users", createUser,
    middleware.DocPOST("/users", scalar.RouteDoc{
        Title: "Create user",
        Tags:  []string{"users"},
        Input: []scalar.InputGroup{
            {Type: scalar.InputBody, Fields: CreateUserRequest{}, Required: true},
        },
        Output:       UserResponse{},
        OutputStatus: 201,
    }),
)
```

- `GET /openapi.json` — the spec
- `GET /scalar` — the UI
- `GET /llms.txt`, `GET /llms-full.txt` — the model-readable index

## Declared, not sniffed

Every other approach to this problem infers the contract: parse comments, sniff
live traffic, or reflect over the handler signature. Breeze asks the route to
declare it, as a `scalar.RouteDoc` passed to `Doc` at registration.

The reason is that the other three are all wrong at the moment it matters. A
comment drifts from the code and nothing checks it. Traffic sniffing documents
what clients happen to send, so a field no caller uses yet does not exist and a
malformed request becomes part of the spec. Reflection over `func(*Context) error`
sees nothing at all — the request and response types are inside the function body.

A declaration is checked by the compiler: `Fields: CreateUserRequest{}` stops
compiling the day that struct is renamed.

## Config

| Field | Default | Meaning |
|---|---|---|
| `Title` | `"Breeze API"` | API name in the UI |
| `Version` | `"1.0.0"` | version string |
| `Description` | `""` | long description |
| `JSONPath` | `"/openapi.json"` | where the spec is served |
| `UIPath` | `"/scalar"` | where the UI is served; `""` disables it and serves JSON only |

Both endpoints are registered with `HandleBlocking`. They regenerate the whole
document on every request — milliseconds of work for a route hit by hand a few
times a day, which is the opposite of what belongs on an event loop.

`SwaggerOptions` and `SwaggerMiddleware` are deprecated aliases. They serve
Scalar, not Swagger UI; the names predate the move and are kept only so existing
code compiles. If a project needs Swagger UI specifically, it is not available
here.

## `RouteDoc`

| Field | Type | Meaning |
|---|---|---|
| `Title` | `string` | the endpoint's summary line in the UI |
| `Tags` | `[]string` | groups the endpoint in the sidebar |
| `Description` | `string` | longer explanation |
| `Input` | `[]InputGroup` | the input contract, one group per source |
| `Output` | `any` | a zero-value struct or typed nil whose shape describes success |
| `OutputStatus` | `int` | success status (default 200) |
| `OutputDescription` | `string` | response description (default "OK") |

`Output` takes a value rather than a type so that a typed nil works:
`(*UserResponse)(nil)` documents the shape without allocating one.

### `InputGroup`

```go
Input: []scalar.InputGroup{
    {Type: scalar.InputBody, Fields: CreateUserRequest{}, Required: true},
    {Type: scalar.InputQuery, Fields: struct {
        Page int `json:"page"`
    }{}},
    {Type: scalar.InputParams, Fields: struct {
        ID string `json:"id"`
    }{}},
}
```

Four sources, matching the four the `binding` package reads:

| `InputType` | Where the fields come from |
|---|---|
| `InputBody` | the JSON request body |
| `InputQuery` | URL query parameters |
| `InputParams` | path parameters (`:id`) |
| `InputHeader` | request headers |

`Required` is meaningful for `InputBody` and marks the whole body required.

### The `Doc` helpers

`Doc(method, path, doc)` registers the documentation and returns a pass-through
middleware. The registration happens **when `Doc` is called** — at startup, as the
route is declared — not per request. The returned handler only calls `ctx.Next()`.

`DocGET`, `DocPOST`, `DocPUT`, `DocPATCH` and `DocDELETE` drop the method argument
when it is known statically. `Tag("Users", doc)` prepends a tag for inline
construction.

`Doc` is a no-op when doc collection is not enabled, so it is safe to leave in
production code — a build that never calls `ScalarMiddleware` pays a slice append
per route at startup and nothing at request time.

## Schema inference

`Output` and `InputGroup.Fields` are reflected, not declared field by field.
`json` tags name the properties, `omitempty` marks a field optional, and `json:"-"`
excludes it.

Recursion is capped at three levels (`maxShapeDepth`). Past that the schema renders
as a type name rather than expanding — a self-referential model would otherwise
generate an infinite document, and three levels is enough for a reader to recognise
the shape.


## `llms.txt`

[llms.txt](https://llmstxt.org) is a convention for handing a language model the
same orientation a new contributor would get: what this service is, what it
exposes, and the house rules. Two files by convention:

| Path | Contents |
|---|---|
| `/llms.txt` | the index — what this is, and one line per route |
| `/llms-full.txt` | the full reference — payload shapes and model definitions |

The split is the point. The common question is "which endpoint do I want", and
answering it should cost a few hundred tokens rather than tens of thousands. A model
that needs the payload shape reads the full file.

### One source of facts

A route's method, path, summary and payload shape are already recorded once by
`RegisterRoute`. Rendering `llms.txt` from a second walk of the router would mean
two collections of the same facts, and they would disagree the first time a route
was documented in one place and not the other — which is exactly the failure
`llms.txt` is meant to prevent.

So `Routes()` reads the same slice `Generate()` reads, through the same lock, and
produces the same paths through the same normaliser. A service whose OpenAPI
document is right cannot have an `llms.txt` that is wrong.

Routes are sorted by path then method. Registration order is arrival order, which
depends on which file registered first — not something a diff should be sensitive
to.

### Two ways in, one renderer

`LLMSFromRegistry()` builds an `LLMSDoc` from the live registry: a running service
describing itself.

The renderer takes that document rather than reading globals, because the registry is
populated at startup by the process that owns the routes — and a tool generating
`llms.txt` for a project on disk is not that process and cannot be. Importing a
project to enumerate its routes would mean running its `main`. So the CLI fills an
`LLMSDoc` by parsing the project's generated registry file instead. One renderer, one
output format, two ways of learning the same facts.

### Freshness stamps

A generated file checked into a repository is a snapshot, and the useful question
about a snapshot is not "does it parse" but "is it still true". That can only be
answered by rebuilding and comparing, so generated files carry a stamp:

```
<!-- breeze-llms-stamp: sha256=… -->
```

```go
digest, ok := scalar.ReadStamp(fileContents)   // what it was built from
fresh := digest == scalar.BodyDigest(rebuilt)  // is it still true
```

`BodyDigest` ignores the stamp line and trailing whitespace on every line. Hashing
a document that contains its own hash is not something a checker could reproduce,
and editors and version control disagree about final newlines — a freshness check

## Diagnostics

The `docs` probe (not `scalar` — the key matches the `breeze add docs` feature
name) reports what the spec actually contains:

```bash
curl localhost:3000/dashboard/api/diagnostics?subsystem=docs
```

| Detail | Meaning |
|---|---|
| `routes_recorded` | how many routes reached the registry |
| `with_title` / `without_title` | how many have a summary |
| `by_method` | counts per HTTP method |
| `api_title`, `api_version`, `has_description` | what `SetInfo` was given |
| `undocumented_list` | which routes have no title (capped) |

Two states worth knowing about, because neither errors anywhere else:

**Collection off** reports `off` with a note. `Doc` wrappers are no-ops while
collection is off, so the OpenAPI document and the UI are both empty and nothing
complains. This probe is the only place that is reported.

**Collection on, zero routes** reports `degraded`, not `ok`. It means the wiring is
half done — `ScalarMiddleware` was installed but no route carries a `Doc` wrapper —
and the symptom is an empty document that looks like a working one.

The undocumented list is capped. A project with three hundred undocumented routes
needs to know that, not to receive three hundred strings through a diagnostics
endpoint.

## See also

- [`middlewares.md`](./middlewares.md) — `ScalarMiddleware` in the chain
- [`binding.md`](./binding.md) — the four input sources these `InputType`s mirror
- [`diag.md`](./diag.md) — the `docs` probe
- [`../README.md`](../README.md) — the tour's OpenAPI section

that failed because of one would be noise that trains a reader to ignore it.

A comment rather than a header so it does not read as content.

