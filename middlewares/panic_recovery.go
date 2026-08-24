package middleware

import (
	"fmt"
	"runtime/debug"

	"github.com/nelthaarion/breeze"
)

// RecoveryMiddleware returns a HandlerFunc that catches panics and returns 500
func RecoveryMiddleware() breeze.HandlerFunc {
	return func(ctx *breeze.Context) {
		defer func() {
			if r := recover(); r != nil {
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
		ctx.Next()
	}
}
