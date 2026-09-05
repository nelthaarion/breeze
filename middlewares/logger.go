package middleware

import (
	"fmt"
	"time"

	"github.com/nelthaarion/breeze"
)

func LoggingMiddleware() breeze.HandlerFunc {
	loggingInstalled.Store(true)
	return func(ctx *breeze.Context) error {
		start := time.Now()
		method := ctx.Req.Method
		path := ctx.Req.Path

		// The chain's error is held rather than returned immediately: this
		// middleware's whole purpose is the line it prints afterwards, and returning
		// early would skip it for exactly the requests worth logging.
		err := ctx.Next()

		status := 0
		if ctx.Res != nil {
			status = ctx.Res.Status
		}
		fmt.Printf("[Breeze][%s] %s %s -> %d (%v)\n", start.Format(time.RFC3339), method, path, status, time.Since(start))

		// Counted after the line is printed, so the count is of lines actually
		// emitted rather than of requests that reached this middleware.
		if err != nil {
			loggingCounter.Error()
		}
		loggingCounter.Hit()

		return err
	}
}
