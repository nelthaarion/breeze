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

	// The five settings below never vary per request, so their wire block and
	// lookup map are rendered once and replayed on every request. Only the
	// allow-origin value is per-request, and only in the multi-origin (echo)
	// case — which is therefore left entirely on the ordinary map path.
	var kv []string
	addKV := func(name, value string) {
		if value != "" {
			kv = append(kv, name, value)
		}
	}
	addKV("Access-Control-Allow-Methods", opts.AllowMethods)
	addKV("Access-Control-Allow-Headers", opts.AllowHeaders)
	addKV("Access-Control-Expose-Headers", opts.ExposeHeaders)
	addKV("Access-Control-Allow-Credentials", opts.AllowCredentials)
	addKV("Access-Control-Max-Age", opts.MaxAge)
	baseBlock, baseLookup := breeze.RenderStaticHeaders(kv...)

	// A single fixed origin (the common "*" case) has a fully static header set,
	// so the whole thing is precomputed as one block.
	hasSingle := !multi && opts.AllowOrigins != ""
	var singleBlock []byte
	var singleLookup map[string]string
	if hasSingle {
		singleKV := append([]string{"Access-Control-Allow-Origin", opts.AllowOrigins}, kv...)
		singleBlock, singleLookup = breeze.RenderStaticHeaders(singleKV...)
	}

	return func(ctx *breeze.Context) error {
		switch {
		case multi:
			// Genuinely per-request: the origin is echoed, so this response is
			// not a single precomputed block. Write every header through the
			// ordinary map path, exactly as before.
			ctx.SetHeader("Vary", "Origin")
			if origin := ctx.Req.Header["origin"]; origin != "" {
				for _, allowed := range origins {
					if allowed == origin {
						ctx.SetHeader("Access-Control-Allow-Origin", origin)
						break
					}
				}
			}
			for i := 0; i+1 < len(kv); i += 2 {
				ctx.SetHeader(kv[i], kv[i+1])
			}
		case hasSingle:
			ctx.SetHeaderStatic(singleBlock, singleLookup)
		default:
			ctx.SetHeaderStatic(baseBlock, baseLookup)
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
