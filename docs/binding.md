# Request binding

`binding` turns an HTTP request into a validated Go struct. One call, four
sources, and a failure that is already an RFC 9457 response body.

```go
type CreateUser struct {
    Name  string `json:"name"  validate:"required,min=2,max=50"`
    Email string `json:"email" validate:"required,email"`
    Role  string `json:"role"  validate:"oneof=admin user viewer"`
    Page  int    `form:"page"`
    ID    string `param:"id"`
}

router.Handle(breeze.POST, "/users/:id", func(ctx *breeze.Context) error {
    var in CreateUser
    if err := ctx.Bind(&in); err != nil {
        return nil // the response is already written — see below
    }
    return ctx.JSON(in)
})
```

## Why this package exists

Three things had to happen on every request that accepts input: decode, validate,
and report. Doing them separately means every handler writes the same twenty
lines, and the twentieth handler writes them slightly differently — so a client
gets `{"error": "..."}` from one endpoint and `{"errors": [...]}` from the next.

`Bind` does all three and reports failures in one shape. That shape is
[RFC 9457](https://www.rfc-editor.org/rfc/rfc9457) problem details, because it is
the only format a client can be told about once.

## `ctx.Bind` vs `binding.Bind`

Two entry points, and the difference matters.

[`ctx.Bind(&dst)`](../context.go) is what a handler calls. It picks the sources
from the request, and **on failure it writes the response itself** — 422 with a
problem+json body for a validation error, 400 for anything else. A non-nil return
means "the response is already written", so the handler returns immediately and
does not write a second one.

`binding.Bind(&dst, sources...)` is the library call, with no `Context` and no
response. Use it when the input is not an HTTP request — a CLI flag set, a queue
message, a test — or when you want to choose the sources yourself:

```go
err := binding.Bind(&in,
    binding.JSONBody(body),
    binding.Query(values),
## The four sources

Sources are applied in the order given, and a later source overwrites a field an
earlier one set. `ctx.Bind` applies them in this order, skipping any that are
empty:

| Source | Struct tag | Reads from |
|---|---|---|
| `JSONBody(body []byte)` | `json` | the request body |
| `Query(url.Values)` | `form`, then `query` | the query string |
| `Form(url.Values)` | `form`, then `query` | a parsed form body |
| `Path(map[string]string)` | `param` | route parameters (`/users/:id`) |

`Query` and `Form` are the same function with two names. They read the same tags
because a field bound from `?page=2` and the same field bound from a form post are
the same field; giving them separate tags would mean tagging most structs twice.

The tag fallback (`form` first, then `query`) exists for the same reason: one tag
covers both, and `query` remains available when a field must differ between them.

Untagged fields are matched by lowercased field name, so `Page` binds `?page=2`
without a tag. That is a convenience for small structs, not a contract — add the
tag when the wire name matters.

### Supported field types

`string`, `bool`, all sizes of `int`/`uint`, `float32`/`float64`, and pointers to
any of those. A pointer distinguishes absent from zero: `?count=0` sets `*int` to
a pointer to 0, while omitting it leaves the pointer nil.

Anything else is skipped rather than rejected — a `[]string` or a nested struct is
left for `JSONBody` to fill, since the query and path sources have no syntax for
them.

## Validation rules

Rules go in a `validate:"..."` tag, comma-separated. All five:

| Rule | Argument | Passes when |
|---|---|---|
| `required` | — | the field is not its zero value |
| `min=N` | number | a number is `>= N`; a string, slice or map has `len >= N` |
| `max=N` | number | a number is `<= N`; a string, slice or map has `len <= N` |
| `email` | — | the value contains one `@` with a non-empty local part, and a `.` in the domain |
| `oneof=a b c` | space-separated | the value equals one of the listed values |

`min` and `max` are deliberately overloaded on kind: `min=2` on a string is a
length and on an `int` is a bound. This is what a reader expects from
`validate:"min=2"` on a name field, and splitting them into `minlen`/`min` would
mean every struct in a codebase uses whichever one its author remembered.

`email` is a shape check, not an RFC 5322 parser and not a deliverability check.
A regex that accepts every legal address accepts almost anything, and the only way
to know an address works is to send to it. This catches the typo — a missing `@`,
a bare hostname — and lets the rest through.

Every rule is checked against **every** field before an error is returned, so a
client fixing a form gets the whole list rather than one error per round trip.
The plan is compiled per struct type and cached, so tag parsing happens once per
type rather than once per request.

## The error shape

A validation failure is a `*binding.ValidationError` holding one `FieldError` per
failing rule:

```go
type FieldError struct {
    Field   string `json:"field"`   // the struct field name
    Rule    string `json:"rule"`    // "required", "min", "email", …
    Message string `json:"message"` // "Email must be a valid email"
}
```

`ToProblemJSON()` renders it as RFC 9457. This is what `ctx.Bind` writes with a
422:

```json
{
  "type": "about:blank",
  "status": 422,
  "title": "Validation Failed",
  "errors": [
    {"field": "Name",  "rule": "required", "message": "Name is required"},
    {"field": "Email", "rule": "email",    "message": "Email must be a valid email"}
  ]
}
```

422 rather than 400 because the request was syntactically valid and semantically
wrong — the distinction a client needs in order to decide whether to retry with
different input or fix its serialisation. Malformed JSON gets the 400.

`Error()` returns the single message when there is one failure and
`"validation error: multiple fields invalid"` when there are several, on the
grounds that a log line should not contain a whole form's worth of complaints.
Read `.Errors` for those.

### Returning it from a handler

`breeze.Error` recognises a `*binding.ValidationError`, so a handler that would
rather return than write can:

```go
func create(ctx *breeze.Context) error {
    var in CreateUser
    if err := binding.Bind(&in, binding.JSONBody(ctx.Req.Body)); err != nil {
        return err // the framework renders the 422 problem+json
    }
    return ctx.JSON(in)
}
```

## Zero-copy and the request body

`JSONBody` does not retain the slice it is given. `ctx.Req.Body` points into a
pooled read buffer that is reused by the next request on that connection, so a
`Source` that kept it would hand the next request's bytes to this one's struct.

The decoded struct is yours — `string` fields are copies. Only the input slice is
borrowed, and only for the duration of the `Bind` call.

## See also

- [`../README.md`](../README.md) — the tour's request-binding section
- [`../context.go`](../context.go) — `ctx.Bind`
- [`repository-structure.md`](./repository-structure.md) — where this package sits in the import graph

)
```
