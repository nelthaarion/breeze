package breeze

import (
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
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
	var rootCache atomic.Pointer[string]
	r.HandleBlocking(GET, pattern, func(ctx *Context) error {
		fp := ctx.Param("filepath")
		// Request paths reach the router raw, so "/static/my%20file.png" arrives
		// with the %20 still in it and used to 404 against a file that exists.
		// Decoded here, before cleaning, so an encoded "%2e%2e" is a ".." that the
		// cleaning and the symlink-aware containment check below both see.
		if strings.IndexByte(fp, '%') >= 0 {
			decoded, derr := url.PathUnescape(fp)
			if derr != nil || strings.IndexByte(decoded, 0) >= 0 {
				staticCounter.Miss()
				ctx.Status(404)
				return ctx.WriteString("File not found")
			}
			fp = decoded
		}
		// if client requested exactly '/static' (no trailing slash) treat as root index
		if fp == "" || fp == "/" {
			fp = "index.html"
		}
		// Sanitize path and resolve the final target through symlinks before opening.
		// filepath.Join alone prevents textual ../ traversal but still permits a
		// symlink inside the static tree to point outside it.
		fp = filepath.Clean("/" + fp)[1:]
		// The mount root's own symlinks are resolved once and remembered: it is
		// the same answer for every request, and resolving it walked the path
		// with an lstat per component each time. Only a success is cached, so a
		// root that does not exist yet is retried.
		var rootResolved string
		if cached := rootCache.Load(); cached != nil {
			rootResolved = *cached
		} else {
			resolvedRoot, rerr := filepath.EvalSymlinks(root)
			if rerr != nil {
				staticCounter.Miss()
				ctx.Status(404)
				return ctx.WriteString("File not found")
			}
			rootCache.Store(&resolvedRoot)
			rootResolved = resolvedRoot
		}
		full := filepath.Join(rootResolved, fp)
		resolved, err := filepath.EvalSymlinks(full)
		if err != nil {
			staticCounter.Miss()
			ctx.Status(404)
			return ctx.WriteString("File not found")
		}
		rel, err := filepath.Rel(rootResolved, resolved)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			staticCounter.Miss()
			ctx.Status(404)
			return ctx.WriteString("File not found")
		}

		// open and serve file
		f, err := os.Open(resolved)
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

		// Conditional GET: a browser that already holds this exact file gets a
		// bodyless 304 instead of the whole thing again.
		etag := staticETag(info)
		if inm := ctx.Req.Header["if-none-match"]; inm != "" && etagMatches(inm, etag) {
			res := ctx.ensureResponse()
			res.Status = 304
			res.Headers = map[string]string{"ETag": etag}
			res.headersShared = false
			res.rawHeaders = nil
			res.Body = nil
			staticCounter.HitBytes(0, 0)
			return nil
		}

		// Served from memory, so the size has to be bounded: an unbounded read
		// lets one large file in the tree cost that much heap per concurrent
		// request. Larger files belong on ctx.StreamFile.
		if info.Size() > maxStaticFileBytes {
			staticCounter.Error()
			ctx.Status(413)
			return ctx.WriteString("File too large to serve from memory")
		}
		// Sized up front: io.ReadAll grows by doubling, which for a large file
		// means copying it several times and holding about twice its size.
		data := make([]byte, info.Size())
		if _, err = io.ReadFull(f, data); err != nil && err != io.ErrUnexpectedEOF {
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
		res.Headers = map[string]string{
			"Content-Type":  ctype,
			"ETag":          etag,
			"Last-Modified": info.ModTime().UTC().Format(http.TimeFormat),
		}
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

// maxStaticFileBytes bounds what the static handler reads into memory.
const maxStaticFileBytes = 64 << 20

// staticETag is a weak validator built from size and modification time: enough
// to tell "unchanged" from "changed" without hashing the file.
func staticETag(fi os.FileInfo) string {
	return `W/"` + strconv.FormatInt(fi.Size(), 16) + "-" + strconv.FormatInt(fi.ModTime().UnixNano(), 16) + `"`
}

// etagMatches implements If-None-Match's weak comparison against one validator.
func etagMatches(header, etag string) bool {
	if strings.TrimSpace(header) == "*" {
		return true
	}
	want := strings.TrimPrefix(etag, "W/")
	for _, cand := range strings.Split(header, ",") {
		if strings.TrimPrefix(strings.TrimSpace(cand), "W/") == want {
			return true
		}
	}
	return false
}
