package middleware

import (
	"fmt"
	"runtime/debug"
	"time"

	"github.com/nelthaarion/breeze/v2"
)

// RecoveryMiddleware returns a HandlerFunc that catches panics and returns 500
func RecoveryMiddleware() breeze.HandlerFunc {
	recoveryInstalled.Store(true)
	return func(ctx *breeze.Context) error {
		defer func() {
			if r := recover(); r != nil {
				// Counted unconditionally, unlike every other middleware count in
				// this package. The gate exists to keep atomics off cheap hot
				// paths, and this path is neither: it has already formatted a
				// stack trace and written it to stdout, so an atomic add is
				// unmeasurable beside it. More to the point, "how many handlers
				// panicked" is the one number here that must not be lost because
				// nobody remembered to turn counting on — it is the reason this
				// middleware exists.
				recoveredPanics.Add(1)
				lastPanicNanos.Store(time.Now().UnixNano())
				storeLastPanic(fmt.Sprintf("%v", r))

				// Log panic and stack trace
				fmt.Printf("[Breeze][PANIC] %v\n%s\n", r, string(debug.Stack()))

				// Return 500 Internal Server Error
				if ctx.Res == nil {
					ctx.Res = &breeze.HTTPResponse{
						Status:  500,
						Headers: map[string]string{"Content-Type": "text/plain"},
						Body:    []byte("Internal Server Error"),
					}
				} else {
					ctx.Status(500)
					ctx.Res.Body = []byte("Internal Server Error")
					ctx.SetHeader("Content-Type", "text/plain")
				}

				// Stop middleware chain
				ctx.Abort()
			}
		}()

		// Continue normal chain
		err := ctx.Next()

		// Gated, because this one does run on every request and the request that
		// did not panic is the uninteresting case.
		recoveryCounter.Hit()
		return err
	}
}
