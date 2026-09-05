package oauth2

import (
	"context"
	"time"

	"github.com/nelthaarion/breeze/v2"
)

// requestTimeout bounds every outbound provider call made while handling a
// request so a slow provider cannot pin a worker goroutine indefinitely.
const requestTimeout = 20 * time.Second

// reqContext returns a context.Context bounded by requestTimeout. The caller
// must invoke the returned cancel func (defer) to release resources.
func reqContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), requestTimeout)
}

// nowPlus returns the current time offset by d. Extracted so token/expiry
// comparisons have a single, testable time source.
func nowPlus(d time.Duration) time.Time { return time.Now().Add(d) }

// redirect issues a 302 redirect to url and stops the middleware chain. Using
// 302 (Found) preserves the GET method on the follow-up request, which is what
// every step of the OAuth dance expects.
func redirect(ctx *breeze.Context, url string) {
	ctx.SetHeader("Location", url)
	ctx.Status(302)
	ctx.Abort()
}

// fail handles an authentication failure uniformly: redirect to FailureRedirect
// when configured, otherwise write a 401. It always stops the chain so a
// failed request never reaches the protected handler.
func fail(ctx *breeze.Context, cfg *Config, status int, err error) {
	if cfg.FailureRedirect != "" {
		redirect(ctx, cfg.FailureRedirect)
		return
	}
	ctx.Status(status)
	ctx.WriteString(err.Error())
	ctx.Abort()
}

// prepareConfig normalizes a copy of the config once, at construction time, and
// returns a pointer suitable for sharing across all requests to the handler.
// Normalizing per-handler (not per-request) keeps the hot path allocation-free:
// no defaults are recomputed on each request.
//
// It also returns the provider's diagnostic counters, resolved here because this is
// the one place every constructor passes through and the only place that can still
// see whether CookieSecret was supplied — normalize() fills it with a random value,
// after which the two cases are indistinguishable.
func prepareConfig(cfg Config) (*Config, *flowCounts) {
	generatedKey := cfg.CookieSecret == ""
	cfg.mustNormalize()
	return &cfg, noteFlow(&cfg, generatedKey)
}
