package middleware

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/nelthaarion/breeze/v2"
)

// clientKey returns the request's client IP, stripping the ephemeral source
// port so repeated requests from the same client on new connections share a
// counter instead of each getting a fresh one.
func clientKey(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

type clientData struct {
	lastRequest time.Time
	requests    int
}

// RateLimiterOptions defines the configuration for the middleware.
type RateLimiterOptions struct {
	Requests int           // allowed requests
	Per      time.Duration // per duration
	Message  string        // optional message on limit

	// KeyFunc overrides how a client is identified. The default is the peer's IP
	// (IPv6 by /64). Behind a reverse proxy every request arrives from the
	// proxy's address and shares one bucket, so supply a key derived from a
	// header your proxy sets and clients cannot forge, or from an API key.
	KeyFunc func(*breeze.Context) string
}

// staleAfter is how long a client entry may sit idle before prune() evicts it.
const staleAfter = 10 * time.Minute

// pruneInterval is how often prune() sweeps the clients map.
const pruneInterval = time.Minute

const maxRateLimiterClients = 100000

// rlShards splits the client table so concurrent event loops rarely meet on the
// same lock. With one global mutex every request on every loop serialised here,
// which at high request rates is the contention point, not the counter update.
const rlShards = 64

// rlKey identifies a client. The common case is an address (ip, with IPv6
// reduced to its /64 so a subscriber cannot rotate through 2^64 sources to dodge
// the limit) and building it allocates nothing — the previous key was the
// formatted "ip:port" string, one allocation per request. custom is set only
// when RateLimiterOptions.KeyFunc supplies its own identity.
type rlKey struct {
	ip     [16]byte
	custom string
}

func (k rlKey) shard() uint64 {
	h := uint64(14695981039346656037)
	for _, c := range k.ip {
		h = (h ^ uint64(c)) * 1099511628211
	}
	for i := 0; i < len(k.custom); i++ {
		h = (h ^ uint64(k.custom[i])) * 1099511628211
	}
	return h % rlShards
}

type rlShard struct {
	mu        sync.Mutex
	clients   map[rlKey]*clientData
	lastPrune time.Time
	_         [24]byte // keep neighbouring shards off one cache line
}

// RateLimiter holds the per-client counters and the pre-formatted limit
// message so the hot path never calls fmt.Sprintf.
type RateLimiter struct {
	options  RateLimiterOptions
	shards   [rlShards]rlShard
	limitMsg string // pre-computed to avoid fmt.Sprintf on every 429
}

// trackedClients is the number of live client entries, for the diagnostics probe.
func (rl *RateLimiter) trackedClients() int {
	n := 0
	for i := range rl.shards {
		sh := &rl.shards[i]
		sh.mu.Lock()
		n += len(sh.clients)
		sh.mu.Unlock()
	}
	return n
}

// pruneLocked evicts entries whose window has ended. It is called
// opportunistically from the request path rather than via a permanent goroutine,
// so constructing a rate limiter does not leak a process-lifetime goroutine.
//
// An entry is only safe to drop once its window is over: lastRequest is when the
// window *started*. The old fixed 10-minute cutoff deleted entries that were
// still inside their window whenever Per was longer than that, which reset the
// counter and let a client exceed the limit.
func (rl *RateLimiter) pruneLocked(sh *rlShard, now time.Time) {
	if !sh.lastPrune.IsZero() && now.Sub(sh.lastPrune) < pruneInterval {
		return
	}
	sh.lastPrune = now
	cutoff := now.Add(-rl.options.Per)
	for key, data := range sh.clients {
		if data.lastRequest.Before(cutoff) {
			delete(sh.clients, key)
		}
	}
}

// keyFor derives the client identity without allocating on the address path.
func (rl *RateLimiter) keyFor(ctx *breeze.Context) rlKey {
	if rl.options.KeyFunc != nil {
		return rlKey{custom: rl.options.KeyFunc(ctx)}
	}
	if ctx.Conn != nil {
		if addr := ctx.Conn.RemoteAddr(); addr != nil {
			if tcp, ok := addr.(*net.TCPAddr); ok {
				var k rlKey
				if ip4 := tcp.IP.To4(); ip4 != nil {
					copy(k.ip[:], ip4.To16())
				} else if ip6 := tcp.IP.To16(); ip6 != nil {
					copy(k.ip[:8], ip6[:8]) // /64
				}
				return k
			}
			return rlKey{custom: clientKey(addr.String())}
		}
	}
	// No socket (an Auto-MCP tool call): all such calls share one bucket, so the
	// route's limit still applies to the agent as a whole instead of the
	// middleware panicking.
	return rlKey{custom: "mcp-local"}
}

// NewRateLimiter returns a rate limiting middleware.
//
// FIX: The original code held mu.Lock() across ctx.Next(), serializing every
// request and completely defeating the WorkerPool. The lock is now released
// before ctx.Next() — it is held only for the map lookup and counter update
// (microseconds).
//
// FIX: The limit message is pre-computed at construction time so the 429
// path does not call fmt.Sprintf on every rejected request.
func NewRateLimiter(opts RateLimiterOptions) breeze.HandlerFunc {
	if opts.Requests <= 0 {
		opts.Requests = 1
	}
	if opts.Per <= 0 {
		opts.Per = time.Second
	}
	rl := &RateLimiter{options: opts}
	for i := range rl.shards {
		rl.shards[i].clients = make(map[rlKey]*clientData)
	}

	// Pre-compute the 429 message once.
	if opts.Message == "" {
		rl.limitMsg = fmt.Sprintf("Rate limit exceeded: max %d requests per %s",
			opts.Requests, opts.Per)
	} else {
		rl.limitMsg = opts.Message
	}

	// Recorded for the probe: the limit and window it will report, and the
	// instance whose client map it will size.
	rateLimitInstalled.Store(true)
	opts.Message = rl.limitMsg
	rateLimitConfig.Store(&opts)
	rateLimiterHandle.Store(rl)

	return func(ctx *breeze.Context) error {
		key := rl.keyFor(ctx)
		sh := &rl.shards[key.shard()]

		// ── Critical section: map lookup + counter update only ──────────
		// The lock is held for microseconds, never across ctx.Next().
		sh.mu.Lock()
		now := time.Now()
		rl.pruneLocked(sh, now)
		data, exists := sh.clients[key]
		if !exists {
			if len(sh.clients) >= maxRateLimiterClients/rlShards {
				sh.mu.Unlock()
				ctx.Status(429)
				rateLimitCounter.Miss()
				return ctx.WriteString(rl.limitMsg)
			}
			data = &clientData{lastRequest: now, requests: 1}
			sh.clients[key] = data
		} else {
			if now.Sub(data.lastRequest) > rl.options.Per {
				data.requests = 1
				data.lastRequest = now
			} else {
				data.requests++
			}
		}
		exceeded := data.requests > rl.options.Requests
		sh.mu.Unlock()
		// ── End critical section ─────────────────────────────────────────

		if exceeded {
			ctx.Status(429)
			rateLimitCounter.Miss()
			return ctx.WriteString(rl.limitMsg)
		}

		rateLimitCounter.Hit()

		// Handler runs lock-free — the WorkerPool can fully parallelize.
		return ctx.Next()
	}
}
