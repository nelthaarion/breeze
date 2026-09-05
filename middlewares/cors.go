package middleware

import (
	"github.com/nelthaarion/breeze"
)

// CORSOptions defines configuration for CORS.
type CORSOptions struct {
	AllowOrigins     string // e.g., "*", or "https://example.com"
	AllowMethods     string // e.g., "GET,POST,PUT,DELETE"
	AllowHeaders     string // e.g., "Content-Type,Authorization"
	ExposeHeaders    string
	AllowCredentials string // "true" or "false"
	MaxAge           string // seconds
}

// CORSMiddleware returns a HandlerFunc to apply CORS headers.
//
// FIX: The original OPTIONS handler called `return` without `ctx.Abort()`,
// leaving ctx.index at its current position. If any code later called
// ctx.Next() on the same context (e.g. a deferred recovery middleware),
// the chain would resume past the CORS short-circuit. We now call
// ctx.Abort() to set index = len(middlewares), guaranteeing the chain
// cannot resume.
//
// Performance: ctx.Abort() is a single int assignment — zero cost.
func CORSMiddleware(opts CORSOptions) breeze.HandlerFunc {
	corsInstalled.Store(true)
	corsConfig.Store(&opts)
	return func(ctx *breeze.Context) error {
		if opts.AllowOrigins != "" {
			ctx.SetHeader("Access-Control-Allow-Origin", opts.AllowOrigins)
		}
		if opts.AllowMethods != "" {
			ctx.SetHeader("Access-Control-Allow-Methods", opts.AllowMethods)
		}
		if opts.AllowHeaders != "" {
			ctx.SetHeader("Access-Control-Allow-Headers", opts.AllowHeaders)
		}
		if opts.ExposeHeaders != "" {
			ctx.SetHeader("Access-Control-Expose-Headers", opts.ExposeHeaders)
		}
		if opts.AllowCredentials != "" {
			ctx.SetHeader("Access-Control-Allow-Credentials", opts.AllowCredentials)
		}
		if opts.MaxAge != "" {
			ctx.SetHeader("Access-Control-Max-Age", opts.MaxAge)
		}

		// Handle preflight OPTIONS request.
		if ctx.Req.Method == breeze.OPTIONS {
			ctx.Status(204)
			ctx.Abort() // FIX: guarantee the chain stops here
			corsCounter.Hit()
			return nil
		}

		corsCounter.Miss()
		return ctx.Next()
	}
}
