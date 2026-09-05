package middleware

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"strings"
	"sync"

	"github.com/andybalholm/brotli"
	"github.com/nelthaarion/breeze"
	"github.com/nelthaarion/breeze/diag"
)

// CompressionMiddleware compresses responses using supported algorithms.
//
// FIX: The original code inspected ctx.Res BEFORE calling ctx.Next(), so it
// always saw a nil response and short-circuited — compression never ran.
// The middleware now calls ctx.Next() first, then post-processes the response.
//
// Performance decisions:
//   - Early-return on empty Accept-Encoding to avoid any allocation for
//     clients that don't advertise compression (common for internal APIs).
//   - Early-return if the response already has Content-Encoding to prevent
//     double-compression when multiple compression layers are stacked.
//   - bytes.Buffer is stack-declared; its internal []byte is the only heap
//     allocation, and it grows once for typical response sizes.
//   - strings.Contains is used instead of full Accept-Encoding parsing
//     (with q-values) because it is ~10x faster and sufficient for the
//     common case where the client lists algorithms plainly.
//   - The switch respects the standard preference order: br > gzip > deflate.
//
// Phase 1.3.5 optimization: encoders (brotli/gzip/flate) and the output
// bytes.Buffer are pooled via sync.Pool. The pre-optimization code allocated
// a fresh encoder (4-8 KB for brotli, smaller for gzip/flate) on EVERY
// compressed response. Pooling eliminates that allocation — the encoder is
// acquired from the pool, Reset with the output buffer, used, and returned.
// The bytes.Buffer is also pooled to avoid its internal []byte allocation.
func CompressionMiddleware() breeze.HandlerFunc {
	compressionInstalled.Store(true)
	return func(ctx *breeze.Context) error {
		// Run the handler first — we need its response to compress. A failure is
		// returned straight away: the error path writes its own body, and compressing
		// a response that does not exist yet is not a thing to attempt.
		if err := ctx.Next(); err != nil {
			return err
		}

		// Nothing to compress.
		if ctx.Res == nil || len(ctx.Res.Body) == 0 {
			return nil
		}

		accept := ctx.Req.Header["accept-encoding"]
		if accept == "" {
			compressionCounter.Miss()
			return nil
		}

		// Don't double-compress if another layer already encoded the body.
		if ctx.GetHeader("Content-Encoding") != "" {
			compressionCounter.Miss()
			return nil
		}

		var encoding string
		var compressed []byte

		original := len(ctx.Res.Body)

		switch {
		case strings.Contains(accept, "br"):
			compressed, encoding = compressBrotli(ctx.Res.Body)
		case strings.Contains(accept, "gzip"):
			compressed, encoding = compressGzip(ctx.Res.Body)
		case strings.Contains(accept, "deflate"):
			compressed, encoding = compressDeflate(ctx.Res.Body)
		default:
			// No supported encoding — serve the raw response.
			compressionCounter.Miss()
			return nil
		}

		// Only swap the body if compression actually produced output.
		if encoding == "" {
			compressionCounter.Miss()
			return nil
		}

		ctx.Res.Body = compressed
		ctx.SetHeader("Content-Encoding", encoding)
		// Vary header tells caches that the response differs by Accept-Encoding.
		ctx.SetHeader("Vary", "Accept-Encoding")

		// One gate read for the hit, the bytes and the saving together; see
		// diag.Counter.HitBytes. The per-algorithm counter is incremented inside
		// the same gated call rather than unconditionally, so a process with
		// counting off does not touch it either.
		if diag.CountersEnabled() {
			compressionCounter.HitBytes(int64(len(compressed)), int64(original-len(compressed)))
			switch encoding {
			case "br":
				brotliCount.Add(1)
			case "gzip":
				gzipCount.Add(1)
			case "deflate":
				deflateCount.Add(1)
			}
		}

		return nil
	}
}

// ─── Pooled encoders ─────────────────────────────────────────────────────────
//
// Each encoder type has its own sync.Pool. The pattern is:
//   1. Get an encoder from the pool (pool's New creates one if empty).
//   2. Reset the encoder with a pooled bytes.Buffer as its output.
//   3. Write the body and Close (flush).
//   4. Extract the compressed bytes.
//   5. Return the encoder AND the buffer to their pools.
//
// The bytes.Buffer is pooled separately because its internal []byte can be
// large (the compressed output) and reusing it avoids growing a new slice
// each time.

var brotliPool = sync.Pool{
	New: func() any { return brotli.NewWriter(nil) },
}

var gzipPool = sync.Pool{
	New: func() any { return gzip.NewWriter(nil) },
}

var flatePool = sync.Pool{
	New: func() any {
		w, err := flate.NewWriter(nil, flate.DefaultCompression)
		if err != nil {
			// flate.NewWriter only errors on invalid level; DefaultCompression
			// is always valid. This should never happen.
			panic("middleware: flate.NewWriter(DefaultCompression) failed: " + err.Error())
		}
		return w
	},
}

var bufferPool = sync.Pool{
	New: func() any { return &bytes.Buffer{} },
}

// compressBrotli compresses data using a pooled brotli encoder.
// Returns (compressed, "br") on success, (nil, "") on failure.
func compressBrotli(data []byte) ([]byte, string) {
	buf := bufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufferPool.Put(buf)

	w := brotliPool.Get().(*brotli.Writer)
	defer brotliPool.Put(w)
	w.Reset(buf)

	if _, err := w.Write(data); err != nil {
		w.Close()
		return nil, ""
	}
	if err := w.Close(); err != nil {
		return nil, ""
	}

	// Copy the compressed bytes because buf will be reused by the next
	// caller. The pool's buffer's internal []byte cannot be retained.
	out := make([]byte, len(buf.Bytes()))
	copy(out, buf.Bytes())
	return out, "br"
}

// compressGzip compresses data using a pooled gzip encoder.
// Returns (compressed, "gzip") on success, (nil, "") on failure.
func compressGzip(data []byte) ([]byte, string) {
	buf := bufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufferPool.Put(buf)

	w := gzipPool.Get().(*gzip.Writer)
	defer gzipPool.Put(w)
	w.Reset(buf)

	if _, err := w.Write(data); err != nil {
		w.Close()
		return nil, ""
	}
	if err := w.Close(); err != nil {
		return nil, ""
	}

	out := make([]byte, len(buf.Bytes()))
	copy(out, buf.Bytes())
	return out, "gzip"
}

// compressDeflate compresses data using a pooled flate encoder.
// Returns (compressed, "deflate") on success, (nil, "") on failure.
func compressDeflate(data []byte) ([]byte, string) {
	buf := bufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufferPool.Put(buf)

	w := flatePool.Get().(*flate.Writer)
	defer flatePool.Put(w)
	w.Reset(buf)

	if _, err := w.Write(data); err != nil {
		w.Close()
		return nil, ""
	}
	if err := w.Close(); err != nil {
		return nil, ""
	}

	out := make([]byte, len(buf.Bytes()))
	copy(out, buf.Bytes())
	return out, "deflate"
}
