package middleware

import (
	"strings"

	"github.com/nelthaarion/breeze/v2"
)

// CORSOptions defines configuration for CORS.
type CORSOptions struct {
	// AllowOrigins is "*", one origin, or a comma-separated allowlist such as
	// "https://app.example.com, https://admin.example.com". With a list, the
	// request's Origin is echoed back only when it is on the list, and the
	// response carries "Vary: Origin" so a shared cache cannot serve one
	// origin's answer to another.
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

	// A list is split once, here, not on every request.
	var origins []string
	multi := strings.Contains(opts.AllowOrigins, ",")
	if multi {
		for _, o := range strings.Split(opts.AllowOrigins, ",") {
			if o = strings.TrimSpace(o); o != "" {
				origins = append(origins, o)
			}
		}
	}

	return func(ctx *breeze.Context) error {
		switch {
		case multi:
			ctx.SetHeader("Vary", "Origin")
			if origin := ctx.Req.Header["origin"]; origin != "" {
				for _, allowed := range origins {
					if allowed == origin {
						ctx.SetHeader("Access-Control-Allow-Origin", origin)
						break
					}
				}
			}
		case opts.AllowOrigins != "":
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
