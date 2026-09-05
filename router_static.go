package breeze

import (
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// ServeStatic registers handlers to serve files under `root` at URL prefix `prefix`.
// Example: ServeStatic("/static", "./public") will serve ./public/* at /static/*
func (r *Router) ServeStatic(prefix, root string) {
	// ensure prefix has no trailing slash when registering pattern,
	// the pattern we register will be prefix + "/*filepath"
	cleanPrefix := strings.TrimSuffix(prefix, "/")

	// Recorded before the route so the probe can name the directory a 404 came
	// from. Registration-time only; nothing reads this on a request.
	r.staticMounts = append(r.staticMounts, staticMount{prefix: cleanPrefix, root: root})

	// handler for files: pattern: prefix + "/*filepath"
	pattern := cleanPrefix + "/*filepath"
	// Registered as blocking: the handler opens and reads a file from disk,
	// so it must run on a worker goroutine rather than inline on the gnet
	// event loop.
	r.HandleBlocking(GET, pattern, func(ctx *Context) error {
		fp := ctx.Param("filepath")
		// if client requested exactly '/static' (no trailing slash) treat as root index
		if fp == "" || fp == "/" {
			fp = "index.html"
		}
		// sanitize path to avoid directory traversal
		fp = filepath.Clean("/" + fp)[1:] // make it relative and cleaned

		full := filepath.Join(root, fp)

		// open and serve file
		f, err := os.Open(full)
		if err != nil {
			staticCounter.Miss()
			ctx.Status(404)
			return ctx.WriteString("File not found")
		}
		defer f.Close()

		info, err := f.Stat()
		if err != nil || info.IsDir() {
			staticCounter.Miss()
			ctx.Status(404)
			return ctx.WriteString("File not found")
		}

		// For small/medium files: read into memory (simple)
		// If you want streaming for big files, use ctx.StreamFile or implement chunked writes.
		data, err := io.ReadAll(f)
		if err != nil {
			staticCounter.Error()
			ctx.Status(500)
			return ctx.WriteString("Error reading file")
		}

		ctype := mime.TypeByExtension(filepath.Ext(full))
		if ctype == "" {
			ctype = http.DetectContentType(data)
		}

		// Take the response from the pool rather than allocating a
		// literal, so releaseContext recycles it like every other
		// response instead of dropping it on the GC.
		res := ctx.ensureResponse()
		res.Status = 200
		res.Headers = map[string]string{"Content-Type": ctype}
		res.headersShared = false
		res.rawHeaders = nil
		// Pinned: the type came from the file's extension, which is the most
		// specific answer available. Nothing downstream should replace it with a
		// body method's default.
		res.ctypePinned = true
		res.Body = data

		// Counted after the read, so bytes is what was actually produced. One
		// gate read for the hit and the byte total together, on a path that has
		// already done an open, a stat and a full file read.
		staticCounter.HitBytes(int64(len(data)), 0)
		return nil
	})
}
