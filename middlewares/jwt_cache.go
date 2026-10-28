package middleware

import (
	"hash/maphash"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// jwtNow is the clock the verified-token cache checks expiry against; tests replace it.
var jwtNow = time.Now

// jwtCacheEntry is a token that passed full verification. claims is a private
// copy that is never handed out; every hit gets its own deep copy.
type jwtCacheEntry struct {
	claims jwt.MapClaims
	exp    time.Time
}

const jwtCacheShards = 16

type jwtCacheShard struct {
	mu  sync.RWMutex
	m   map[string]*jwtCacheEntry
	max int
	_   [24]byte // keep neighbouring shards off one cache line
}

// jwtCache remembers access tokens whose signature, expiry and type have already
// been verified, so a client that sends the same token on every request does not
// pay for the HMAC, three base64 decodes and two JSON decodes each time.
//
// An entry only exists for a token that verified, and it is trusted only until the
// token's own exp. Everything that depends on the request rather than the token
// (required roles, ClaimsValidator) still runs on every request against a fresh
// copy of the claims.
type jwtCache struct {
	seed   maphash.Seed
	shards [jwtCacheShards]jwtCacheShard
}

func newJWTCache(size int) *jwtCache {
	c := &jwtCache{seed: maphash.MakeSeed()}
	per := (size + jwtCacheShards - 1) / jwtCacheShards
	for i := range c.shards {
		c.shards[i].m = make(map[string]*jwtCacheEntry)
		c.shards[i].max = per
	}
	return c
}

func (c *jwtCache) shard(token string) *jwtCacheShard {
	return &c.shards[maphash.String(c.seed, token)%jwtCacheShards]
}

// lookup returns a private copy of the claims of a previously verified, still
// unexpired token.
func (c *jwtCache) lookup(token string) (jwt.MapClaims, bool) {
	sh := c.shard(token)
	sh.mu.RLock()
	e := sh.m[token]
	sh.mu.RUnlock()
	if e == nil || !jwtNow().Before(e.exp) {
		return nil, false // unknown, or expired: the full path decides (and rejects)
	}
	return cloneClaims(e.claims), true
}

// store records a token that has just passed verification.
func (c *jwtCache) store(token string, claims jwt.MapClaims) {
	exp, err := claims.GetExpirationTime()
	if err != nil || exp == nil {
		return
	}
	e := &jwtCacheEntry{claims: cloneClaims(claims), exp: exp.Time}
	sh := c.shard(token)
	sh.mu.Lock()
	if _, ok := sh.m[token]; !ok && len(sh.m) >= sh.max {
		for k := range sh.m { // bounded: make room by dropping an arbitrary entry
			delete(sh.m, k)
			break
		}
	}
	sh.m[strings.Clone(token)] = e
	sh.mu.Unlock()
}

func (c *jwtCache) entries() int {
	n := 0
	for i := range c.shards {
		c.shards[i].mu.RLock()
		n += len(c.shards[i].m)
		c.shards[i].mu.RUnlock()
	}
	return n
}

// cloneClaims deep-copies claims decoded from JSON, so a handler that edits the
// map it receives, or a slice or map inside it, cannot change what the next
// request with the same token sees.
func cloneClaims(c jwt.MapClaims) jwt.MapClaims {
	out := make(jwt.MapClaims, len(c))
	for k, v := range c {
		out[k] = cloneJSONValue(v)
	}
	return out
}

func cloneJSONValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, e := range x {
			m[k] = cloneJSONValue(e)
		}
		return m
	case []any:
		s := make([]any, len(x))
		for i, e := range x {
			s[i] = cloneJSONValue(e)
		}
		return s
	}
	return v // string, float64, bool, nil, json.Number: immutable
}

// verifyAccess validates an access token, through the cache when one is configured.
func verifyAccess(cache *jwtCache, token string, key []byte, method jwt.SigningMethod) (jwt.MapClaims, bool) {
	if cache != nil {
		if claims, ok := cache.lookup(token); ok {
			return claims, true
		}
	}
	claims, ok := validateJWTKey(token, key, method, "access")
	if ok && cache != nil {
		cache.store(token, claims)
	}
	return claims, ok
}
