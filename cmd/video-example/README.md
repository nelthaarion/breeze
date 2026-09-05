# `cmd/video-example`

## What this demonstrates

Range-request video streaming: a directory of files served with `Accept-Ranges`,
seekable in a browser scrubber, optionally behind expiring signed URLs, with the
dashboard's video page watching.

Specifically:

- `video.Mount(router, video.Config{...})` — one call registers the streaming route
- `ChunkSize` and `MaxChunkSize` — how much is written per syscall, and the cap on
  what a client can ask for
- `video.Sign(secret, name, ttl)` — a URL that is a capability with an expiry
- `events.OnType(func(_ *events.Context, e video.StreamServed) error {...})` —
  per-stream telemetry with nothing added to the streaming path
- `coll.AttachVideo(events.Default)` — the dashboard's video page
- `OnError` — the internal error, which is never sent to the client

## How to run it

Run from **this directory** so the template engine finds `./views`:

```bash
cd cmd/video-example
go run .
```

It creates `./media` on first run. Drop an `.mp4` in there, open
<http://localhost:3000>, and press play. **Drag the scrubber** — that is what issues
the range requests this package exists to serve.

With signed URLs:

```bash
VIDEO_SECRET=some-long-random-string go run .
```

Unsigned requests are then refused, and the page's links carry a signature valid for
ten minutes.

The dashboard is at <http://localhost:3000/dashboard> — the Video page shows
throughput per stream.

## What to look for

**The log line per request.** `ChunkSize` is set to 128 KiB here, half the package
default, deliberately so a single playback produces several writes and the log shows
what a range request actually does. 256 KiB is the right production value — large
enough that syscall overhead disappears.

**`MaxChunkSize` is separate from `ChunkSize`.** A client controls the range it
requests; `MaxChunkSize` is the cap on what the server will honour. Without it, a
`Range: bytes=0-` on a 4 GB file is a request to buffer 4 GB.

**Signed URLs are off unless `VIDEO_SECRET` is set.** Hardcoding a secret in an
example would work, but leaving it in the environment means the demo runs out of the
box and only enforces signatures when you ask — and nobody copies a hardcoded key
into production.

**Ten minutes is the signature TTL, and that is the point.** A link that leaks is
only useful for as long as it lives. A signed URL with no expiry is a permanent
grant with extra steps.

**`StreamServed` carries `Disconnected`.** A viewer who closes the tab mid-stream is
not an error, and the listener distinguishes it from a completed stream. Telemetry
that logged both the same way would report a healthy service as failing constantly.

**`OnError` receives the real error; the client does not.** A path that failed to
open, a signature that did not verify — the client gets a status code, and the
operator gets the reason. That split is the reason the callback exists.

**Nothing in the listener is on the streaming path.** The event is published after
the response completes, so per-stream accounting costs nothing per byte.

Next: [`../../video/README.md`](../../video/README.md) for the full config reference
and the traversal rules, and [`../../video/doc.go`](../../video/doc.go) for why the
error semantics are what they are.
