//go:build newapi

package main

import (
	"flag"

	breeze "github.com/nelthaarion/breeze/v2"
	middleware "github.com/nelthaarion/breeze/v2/middlewares"
)

var pollYield = flag.String("pollyield", "default", "default|auto|never (new tree only)")

func applyNewAPI(app *breeze.Breeze) {
	switch *pollYield {
	case "auto":
		app.SetPollYield(breeze.PollYieldAuto)
	case "never":
		app.SetPollYield(breeze.PollYieldNever)
	}
}

func newMW(name string) breeze.HandlerFunc {
	switch name {
	case "logbuf":
		return middleware.LoggingMiddlewareWith(middleware.LoggingOptions{Buffered: true})
	case "jwtcache":
		return middleware.JWTAuthMiddleware(middleware.JWTOptions{AccessSecret: jwtSecret, CacheSize: 1024})
	case "compressmin":
		return middleware.CompressionMiddlewareWith(middleware.CompressionOptions{MinSize: 1024})
	}
	return nil
}
