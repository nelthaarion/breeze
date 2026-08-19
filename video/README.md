# video

Byte-range video streaming for Breeze. Serves a directory of media files so
that browsers can **seek** — which is the whole difficulty, and the reason a
plain static file handler is not enough.

```go
router := breeze.NewRouter()

if err := video.Mount(router, video.Config{Root: "./media"}); err != nil {
    log.Fatal(err)
}
```

That registers `GET` and `HEAD` on `/videos/*filepath`.

Try it:

```bash
go run ./cmd/video-example
```

## Why a static handler is not enough

A `<video>` element never downloads a file in one go. It reads the container
header, jumps to wherever the viewer clicked, and reads forward from there.
Each jump is a separate request carrying a `Range` header, and the server has
to reply with `206 Partial Content`, exactly the bytes asked for, and a
`Content-Range` that describes them.

Ignore `Range` and the video still plays — which is what makes the bug
expensive to find. The scrubber is simply dead: the browser cannot ask for
the middle of a file it is being handed sequentially.

There is also a structural reason this could not be a thin wrapper. Breeze's
`HTTPResponse.Bytes()` always emits `Content-Length: len(Body)` and always
writes `Body`, because it is built to describe a complete in-memory
response. A `206` whose length is one slice of a file, a `304` with no body,
and a multi-write stream whose head goes out before the bytes exist are all
outside what it can express. So this package sets `ctx.Res = nil`, takes over
the connection, and serialises its own head.

## Memory

One chunk in flight per response — 256 KiB by default, from a pool, returned
when the write completes. Ten thousand viewers cost ten thousand chunks, not
ten thousand copies of the file.

Players routinely open with `Range: bytes=0-`, meaning "the rest of the
file". Answering literally would pin an entire movie in memory and defeat
seeking, so open-ended requests are capped at `MaxChunkSize` (4 MiB default).
A server is explicitly allowed to return less than was asked for, provided
`Content-Range` says what it actually sent.

Measured on an i5-11400F:

```
BenchmarkStreamChunk-12     691416 ns/op   6066 MB/s   1655 B/op    4 allocs/op
BenchmarkStreamSeek-12       92186 ns/op   2844 MB/s   1655 B/op    4 allocs/op
BenchmarkNormalize-12          342 ns/op                128 B/op    3 allocs/op
BenchmarkParseRange-12          88 ns/op                 16 B/op    1 allocs/op
```

Allocations per request do not grow with file size.

## Security

The request path is treated as hostile, and the **order** of the checks is
the security property:

1. **Percent-decode first** — otherwise `%2e%2e%2f` walks straight past a
   check that only understands literal dots.
2. **Reject NUL and backslash** — `safe.mp4\x00../../etc/passwd` passes a
   naive suffix check and opens something else; `..\..\x` traverses on
   Windows but survives a slash-only cleaner.
3. **Reject any `..` segment outright.** Cleaning it away would be *safe* —
   `path.Clean("/"+"../x")` gives `/x`, inside the root — but it silently
   rewrites an attack into a legitimate-looking lookup, so neither the logs
   nor the `Authorize` callback ever learn an attack was attempted. No real
   client puts `..` in a media URL, so failing closed costs nothing.
4. **Hide dotfiles** by default, so a stray `.env` in the media root is
   unreachable.
5. **Extension allow-list**, so a new dangerous type cannot silently become
   servable.
6. **Only then touch the disk**, and prove containment against the
   symlink-resolved real path — which catches the subtle case where every
   segment is innocent but one is a link out of the tree.

Everything about a file's existence returns **404, never 403**, so the
filesystem cannot be mapped by watching which refusals differ. The real
reason goes to `OnError` and the collector, never to the wire. Header values
are stripped of CR/LF at the single point where bytes are serialised, so a
filename cannot inject a second response.

Optional signed URLs, verified *before* any filesystem access so an
unauthenticated flood costs no disk I/O:

```go
video.Mount(router, video.Config{
    Root:   "./media",
    Secret: []byte(os.Getenv("VIDEO_SECRET")),
})

url := "/videos/movie.mp4?" + video.Sign(secret, "movie.mp4", 10*time.Minute)
```

Comparison is constant-time, and the expiry is inside the signed payload so
it cannot be extended by editing the query string.

## Caching

`ETag` (size + mtime) and `Last-Modified` on every success. Conditional
requests are answered **before the file is opened**, so a revalidation costs
a stat instead of a transfer. `If-None-Match` takes precedence over
`If-Modified-Since`, because a date is only second-accurate and would keep
serving a stale body for up to a second after an edit.

`If-Range` is honoured, which is what makes a resumed download safe: if the
file changed while the client was away, the whole file is sent rather than a
slice that would corrupt the client's copy.

## Observability

Every request publishes an `observability.Signal` and a `StreamServed` event:

```go
events.OnType(func(_ *events.Context, e video.StreamServed) error {
    log.Printf("%s: %d bytes in %v", e.File, e.Bytes, e.Duration)
    return nil
})
```

A viewer who closes the tab mid-stream is reported as **cancelled, not
failed**. In video that is the most common way a request ends — every seek
and every closed tab aborts a transfer in flight — and counting those as
errors would make a healthy server look like an outage. `Bytes` reports what
actually left, so a transfer abandoned at 90% is distinguishable from one
that never started.

### The dashboard's Video tab

The dashboard can consume that event and show what is playing right now:

```go
coll := dashboard.Install(app, router, dashboard.Config{})
defer coll.AttachVideo(events.Default)()
```

This is a separate call from `AttachEvents` on purpose, so an application
with no media pays nothing: the tracker is only allocated when you attach
it.

The reason it is not just the Live Requests feed is that streaming breaks
the one-row-per-request model. A single viewer emits hundreds of range
requests for one file, so that feed shows the same filename repeatedly,
interleaved with unrelated traffic, with the oldest entries evicted first —
and it cannot report throughput at all, because bandwidth is a property of
a *stream*, not of any single request. The Video tab therefore aggregates
**by file**: bytes in flight, MB/s, seeks, disconnects and errors per title.

Rates are computed over a fixed 10-second window rather than the span
between samples, because two requests 3 ms apart would otherwise read as
tens of MB/s of "sustained" throughput. `304`s are excluded from that
window, so a well-cached file does not appear slow. The file table is
bounded, and idle files are evicted before active ones, so a client probing
random paths cannot push a live stream off the page.

Finished requests remain in the observability ring buffer either way — the
tab is a live view, not the historical record.


## Configuration

| Field | Default | Notes |
|---|---|---|
| `Root` | — | Required. Resolved absolutely, symlinks evaluated once at mount. |
| `Prefix` | `/videos` | Route is `Prefix + "/*filepath"`. |
| `Extensions` | common video + HLS/DASH | Allow-list, case-insensitive. |
| `AllowHidden` | `false` | Dotfiles stay unreachable. |
| `FollowSymlinks` | `false` | Target must still resolve inside `Root`. |
| `ChunkSize` | 256 KiB | Bytes per write. |
| `MaxChunkSize` | 4 MiB | Cap for open-ended ranges; negative disables. |
| `CacheControl` | `public, max-age=86400` | `"-"` omits the header. |
| `AllowedOrigins` | none | `"*"` allows any; `Vary: Origin` always sent. |
| `Authorize` | nil | Runs on the clean name, before any disk access. |
| `Secret` | nil | Enables signed URLs. |
| `OnError` | nil | Receives the internal error, never sent to clients. |

## Status codes

| Code | When |
|---|---|
| 200 | Whole file, or a malformed `Range` (which must be ignored per RFC 9110). |
| 206 | Valid range. |
| 304 | `If-None-Match` / `If-Modified-Since` hit. |
| 403 | Signature missing, invalid or expired; `Authorize` refused. |
| 404 | Missing, traversal, hidden, wrong type, directory, symlink. |
| 416 | Well-formed but unsatisfiable range; carries `Content-Range: bytes */size`. |
