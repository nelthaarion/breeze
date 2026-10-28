package middleware

import (
	"crypto/md5"
	"encoding/hex"
	"hash/maphash"
	"strings"
	"sync"

	"github.com/nelthaarion/breeze/v2"
)

// ETagCache remembers the ETag last computed for each request URL (path plus
// query) so that a conditional request can be answered 304 without running the
// handler.
//
// It is sharded, so concurrent event loops do not queue on one lock, and bounded:
// a full shard evicts an arbitrary entry, which at worst makes one conditional
// request run its handler again (it is still answered 304 afterwards when the
// ETag matches). It stores only the ETag, not the response body.
type ETagCache struct {
	shards [etagShards]etagShard
}

const (
	etagShards   = 32
	etagShardMax = 512 // entries per shard, 16384 URLs in all
)

type etagShard struct {
	mu sync.RWMutex
	m  map[string]string // URL -> ETag
	_  [32]byte          // keep neighbouring shards off one cache line
}

var etagSeed = maphash.MakeSeed()

func (c *ETagCache) shard(key string) *etagShard {
	return &c.shards[maphash.String(etagSeed, key)%etagShards]
}

func (c *ETagCache) get(key string) (string, bool) {
	sh := c.shard(key)
	sh.mu.RLock()
	etag, ok := sh.m[key]
	sh.mu.RUnlock()
	return etag, ok
}

// put records etag for key. The common case, the same content as last time, only
// takes the read lock and writes nothing.
func (c *ETagCache) put(key, etag string) {
	sh := c.shard(key)
	sh.mu.RLock()
	cur, ok := sh.m[key]
	sh.mu.RUnlock()
	if ok && cur == etag {
		return
	}
	sh.mu.Lock()
	if _, exists := sh.m[key]; !exists && len(sh.m) >= etagShardMax {
		for k := range sh.m {
			delete(sh.m, k)
			break
		}
	}
	sh.m[strings.Clone(key)] = etag // key may alias request memory
	sh.mu.Unlock()
}

func (c *ETagCache) entries() int {
	n := 0
	for i := range c.shards {
		c.shards[i].mu.RLock()
		n += len(c.shards[i].m)
		c.shards[i].mu.RUnlock()
	}
	return n
}

// NewETagCache returns an empty cache.
func NewETagCache() *ETagCache {
	c := &ETagCache{}
	for i := range c.shards {
		c.shards[i].m = make(map[string]string)
	}
	etagCacheHandle.Store(c)
	return c
}

// ETagMiddleware sets an ETag (the MD5 of the response body) on every response with
// a body and answers a matching If-None-Match with 304 Not Modified. When the
// cache already holds the matching ETag for the URL the handler is skipped.
func (c *ETagCache) ETagMiddleware() breeze.HandlerFunc {
	etagInstalled.Store(true)
	return func(ctx *breeze.Context) error {
		inm := ctx.Req.Header["if-none-match"]
		if inm != "" {
			if etag, ok := c.get(etagKey(ctx)); ok && etag == inm {
				ctx.Status(304)
				ctx.SetHeader("ETag", inm)
				ctx.Res.Body = nil
				etagCounter.Hit()
				return nil // skip handler: the client's copy is still valid
			}
		}

		if err := ctx.Next(); err != nil {
			return err
		}
		if ctx.Res == nil || len(ctx.Res.Body) == 0 {
			return nil
		}

		hash := md5.Sum(ctx.Res.Body)
		var hexETag [md5.Size * 2]byte
		hex.Encode(hexETag[:], hash[:])
		etag := string(hexETag[:])
		c.put(etagKey(ctx), etag)

		ctx.SetHeader("ETag", etag)
		if inm != "" && inm == etag {
			ctx.Status(304)
			ctx.Res.Body = nil
			etagCounter.Hit()
		} else {
			etagCounter.Miss()
		}
		return nil
	}
}

// etagKey is the cache key of the request: its path, plus the canonical query
// when there is one. Without a query it is the path itself, not a copy, so a
// lookup allocates nothing; put clones the key before storing it.
func etagKey(ctx *breeze.Context) string {
	if len(ctx.Req.Query) == 0 {
		return ctx.Req.Path
	}
	return buildCacheKey(ctx)
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
