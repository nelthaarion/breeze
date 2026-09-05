package middleware

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/nelthaarion/breeze"
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
}

// staleAfter is how long a client entry may sit idle before prune() evicts it.
const staleAfter = 10 * time.Minute

// pruneInterval is how often prune() sweeps the clients map.
const pruneInterval = time.Minute

// RateLimiter holds the per-client counters and the pre-formatted limit
// message so the hot path never calls fmt.Sprintf.
type RateLimiter struct {
	options  RateLimiterOptions
	clients  map[string]*clientData
	mu       sync.Mutex
	limitMsg string // FIX: pre-computed to avoid fmt.Sprintf on every 429
}

// prune periodically evicts clients that haven't made a request in a while,
// so the map doesn't grow without bound over the life of the process.
func (rl *RateLimiter) prune() {
	ticker := time.NewTicker(pruneInterval)
	defer ticker.Stop()
	for range ticker.C {
		cutoff := time.Now().Add(-staleAfter)
		rl.mu.Lock()
		for key, data := range rl.clients {
			if data.lastRequest.Before(cutoff) {
				delete(rl.clients, key)
			}
		}
		rl.mu.Unlock()
	}
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
	rl := &RateLimiter{
		options: opts,
		clients: make(map[string]*clientData),
	}

	// Pre-compute the 429 message once.
	if opts.Message == "" {
		rl.limitMsg = fmt.Sprintf("Rate limit exceeded: max %d requests per %s",
			opts.Requests, opts.Per)
	} else {
		rl.limitMsg = opts.Message
	}

	go rl.prune()

	// Recorded for the probe: the limit and window it will report, and the
	// instance whose client map it will size.
	rateLimitInstalled.Store(true)
	opts.Message = rl.limitMsg
	rateLimitConfig.Store(&opts)
	rateLimiterHandle.Store(rl)

	return func(ctx *breeze.Context) error {
		// Use IP as key (port stripped so reconnects share a counter).
		clientIP := clientKey(ctx.Conn.RemoteAddr().String())

		// ── Critical section: map lookup + counter update only ──────────
		// The lock is held for microseconds, never across ctx.Next().
		rl.mu.Lock()
		now := time.Now()
		data, exists := rl.clients[clientIP]
		if !exists {
			data = &clientData{lastRequest: now, requests: 1}
			rl.clients[clientIP] = data
		} else {
			if now.Sub(data.lastRequest) > rl.options.Per {
				data.requests = 1
				data.lastRequest = now
			} else {
				data.requests++
			}
		}
		exceeded := data.requests > rl.options.Requests
		rl.mu.Unlock()
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
