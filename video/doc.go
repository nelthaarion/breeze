// Package video serves video files over HTTP with byte-range support, so
// that a browser can seek without downloading the whole file.
//
// # Why this exists
//
// A video element does not fetch a file the way a download does. It asks
// for a few hundred kilobytes to read the container header, then jumps to
// wherever the viewer clicked, then reads forward. Each of those is a
// separate request carrying a Range header, and the server must answer
// with 206 Partial Content, the exact slice requested, and a Content-Range
// describing it. A server that ignores Range and returns 200 with the
// whole body will appear to work — the video plays — but the scrubber will
// be dead, because the browser has no way to ask for the middle of a file
// it is being handed sequentially.
//
// Doing that on top of a gnet-based framework needs care, because the
// framework's response type describes a complete in-memory body: it always
// writes a Content-Length equal to the buffer it holds. That is the right
// model for JSON, and the wrong one for a four-gigabyte file. So this
// package writes to the connection itself and builds its own response head.
//
// # Usage
//
//	router := breeze.NewRouter()
//	err := video.Mount(router, video.Config{Root: "./media"})
//	if err != nil {
//	    log.Fatal(err)
//	}
//
// That registers GET and HEAD on "/videos/*filepath". A player pointed at
// /videos/movie.mp4 will then seek correctly.
//
// # Memory
//
// A response never holds more than one chunk of the file, [DefaultChunkSize]
// by default, taken from a pool and returned when the write completes. Ten
// thousand concurrent viewers cost ten thousand chunks of transient buffer,
// not ten thousand copies of the file. An open-ended request — "bytes=0-",
// which players send routinely — is capped at [DefaultMaxChunkSize] rather
// than being answered with the entire file, which a server is explicitly
// permitted to do so long as Content-Range reports what it actually sent.
//
// # Security
//
// The path a client supplies is treated as hostile. In order:
// percent-decoding happens first, so an encoded traversal cannot slip past
// a check that only understands literal dots; NUL bytes and backslashes are
// refused; any dot-dot segment is refused outright rather than collapsed,
// so an attempt stays visible in the logs instead of being silently
// rewritten into a valid-looking request; dotfiles are hidden by default;
// the extension must be on an allow-list; and only then is the disk
// touched. Containment is finally proven against the symlink-resolved real
// path, which catches the case where every segment looks innocent but one
// is a link out of the tree.
//
// Every rejection that concerns a file's existence reports 404, never 403,
// so that a client cannot map the filesystem by watching which refusals
// differ. The internal reason goes to [Config.OnError] and to the
// observability collector; it is never written to the wire.
//
// Signed URLs ([Sign], [SignAt]) are available when a mount must not be
// world-readable. Verification happens before any filesystem access, so an
// unauthenticated flood costs no disk I/O.
//
// # Caching
//
// Responses carry an ETag derived from size and mtime, plus Last-Modified.
// Conditional requests are answered before the file is opened, so a
// revalidation costs a stat rather than a transfer. If-Range is honoured,
// which is what makes a resumed download safe: if the file changed while
// the client was away, the whole file is sent instead of a slice that would
// corrupt the client's copy.
//
// # Observability
//
// Each request publishes an [observability.Signal] and a [StreamServed]
// event. A viewer who closes the tab mid-stream is recorded as cancelled
// rather than failed — in video that is the single most common way a
// request ends, and counting it as an error would make a healthy server
// look broken.
package video
