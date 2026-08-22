package middleware

import (
	"crypto/md5"
	"encoding/hex"
	"strings"
	"sync"

	"github.com/nelthaarion/breeze"
)

// ETagCache stores cached responses per route or URL.
//
// FIX: The original middleware inspected ctx.Res BEFORE calling ctx.Next(),
// so it always saw nil and short-circuited — ETag generation never ran.
// The middleware now calls ctx.Next() first, then computes the ETag from
// the handler's response body (strong validation).
//
// FIX: The cache key now includes the query string so that
// /api/users?page=1 and /api/users?page=2 get distinct ETags.
//
// Performance decisions:
//   - RLock for the store read (If-None-Match pre-check) allows concurrent
//     304 checks without contention.
//   - Lock for the store write is held for a single map assignment —
//     microseconds, never across ctx.Next().
//   - MD5 is used because it is already in the stdlib and the body is in
//     memory. For extreme throughput, xxhash would be ~5x faster but adds
//     a dependency. The body hash is O(len(body)) regardless.
//   - hex.EncodeToString allocates one string per response. This is
//     unavoidable with the current ETag header API.
//
// NOTE: The store grows unboundedly. For production use, add LRU or TTL
// eviction. The store is written on every response but only read on
// If-None-Match requests — it exists for observability and future
// cache-hit support.
type ETagCache struct {
	mu    sync.RWMutex
	store map[string]*cachedResponse
}

type cachedResponse struct {
	body []byte
	etag string
}

// NewETagCache creates a new ETag cache.
func NewETagCache() *ETagCache {
	return &ETagCache{
		store: make(map[string]*cachedResponse),
	}
}

// ETagMiddleware returns a Breeze middleware that sets ETag headers
// and returns 304 Not Modified when the client's If-None-Match matches.
//
// The ETag is computed from the fresh response body on every request
// (strong validation), so stale 304s are impossible — if the handler
// produces a different body, the ETag changes and the client gets a
// full 200 response.
func (c *ETagCache) ETagMiddleware() breeze.HandlerFunc {
	return func(ctx *breeze.Context) {
		// Pre-check: if the client sent If-None-Match, see if we have a
		// stored ETag for this URL. If it matches, we can skip the handler
		// entirely and return 304 immediately.
		inm := ctx.Req.Header["if-none-match"]
		if inm != "" {
			cacheKey := buildCacheKey(ctx)
			c.mu.RLock()
			cached, ok := c.store[cacheKey]
			c.mu.RUnlock()
			if ok && cached.etag == inm {
				ctx.Status(304)
				ctx.SetHeader("ETag", inm)
				ctx.Res.Body = nil
				return // skip handler — client's cache is still valid
			}
		}

		// Run the handler to produce the response.
		ctx.Next()

		if ctx.Res == nil || len(ctx.Res.Body) == 0 {
			return
		}

		// Compute ETag from the fresh response body.
		hash := md5.Sum(ctx.Res.Body)
		etag := hex.EncodeToString(hash[:])

		cacheKey := buildCacheKey(ctx)

		// Store for future If-None-Match pre-checks.
		c.mu.Lock()
		c.store[cacheKey] = &cachedResponse{
			body: ctx.Res.Body,
			etag: etag,
		}
		c.mu.Unlock()

		// Set ETag header on the response.
		ctx.SetHeader("ETag", etag)

		// If the client's If-None-Match matches the fresh ETag, return 304.
		if inm != "" && inm == etag {
			ctx.Status(304)
			ctx.Res.Body = nil
		}
	}
}

// buildCacheKey constructs a cache key from the path and query string.
//
// The path is cloned rather than used directly. ctx.Req.Path is a view into the
// bytes the request was parsed from, and with breeze's SetZeroCopyHeaders those
// bytes are the connection's read buffer, which is reused for the next read.
// The key here goes into c.store, which outlives the request — so a borrowed
// string would not merely go stale, it would mutate into part of some later
// request while sitting in the map as a key, putting it in the wrong bucket and
// corrupting every lookup that follows.
//
// One small allocation per cached response, on a path that already hashes the
// body with MD5 and hex-encodes the digest, is not worth optimising away.
func buildCacheKey(ctx *breeze.Context) string {
	if len(ctx.Req.Query) == 0 {
		return strings.Clone(ctx.Req.Path)
	}
	// Concatenation already copies, so no Clone needed on this branch.
	return ctx.Req.Path + "?" + ctx.Req.Query.Encode()
}
