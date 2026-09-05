package middleware

import (
	"github.com/nelthaarion/breeze"
)

// SecurityOptions defines configurable HTTP security headers
type SecurityOptions struct {
	ContentSecurityPolicy     string
	XFrameOptions             string
	XContentTypeOptions       string
	ReferrerPolicy            string
	StrictTransportSecurity   string
	FeaturePolicy             string
	XXSSProtection            string
	ExpectCT                  string
	CrossOriginEmbedderPolicy string
	CrossOriginOpenerPolicy   string
	CrossOriginResourcePolicy string
	CacheControl              string
}

// SecurityMiddleware returns a Breeze HandlerFunc that applies security headers
func SecurityMiddleware(opts SecurityOptions) breeze.HandlerFunc {
	securityInstalled.Store(true)
	securityConfig.Store(&opts)
	return func(ctx *breeze.Context) error {
		if opts.ContentSecurityPolicy != "" {
			ctx.SetHeader("Content-Security-Policy", opts.ContentSecurityPolicy)
		}
		if opts.XFrameOptions != "" {
			ctx.SetHeader("X-Frame-Options", opts.XFrameOptions)
		}
		if opts.XContentTypeOptions != "" {
			ctx.SetHeader("X-Content-Type-Options", opts.XContentTypeOptions)
		}
		if opts.ReferrerPolicy != "" {
			ctx.SetHeader("Referrer-Policy", opts.ReferrerPolicy)
		}
		if opts.StrictTransportSecurity != "" {
			ctx.SetHeader("Strict-Transport-Security", opts.StrictTransportSecurity)
		}
		if opts.FeaturePolicy != "" {
			ctx.SetHeader("Permissions-Policy", opts.FeaturePolicy)
		}
		if opts.XXSSProtection != "" {
			ctx.SetHeader("X-XSS-Protection", opts.XXSSProtection)
		}
		if opts.ExpectCT != "" {
			ctx.SetHeader("Expect-CT", opts.ExpectCT)
		}
		if opts.CrossOriginEmbedderPolicy != "" {
			ctx.SetHeader("Cross-Origin-Embedder-Policy", opts.CrossOriginEmbedderPolicy)
		}
		if opts.CrossOriginOpenerPolicy != "" {
			ctx.SetHeader("Cross-Origin-Opener-Policy", opts.CrossOriginOpenerPolicy)
		}
		if opts.CrossOriginResourcePolicy != "" {
			ctx.SetHeader("Cross-Origin-Resource-Policy", opts.CrossOriginResourcePolicy)
		}
		if opts.CacheControl != "" {
			ctx.SetHeader("Cache-Control", opts.CacheControl)
		}
		securityCounter.Hit()
		return ctx.Next()
	}
}

// DefaultSecurityMiddleware returns a middleware with safe default headers
func DefaultSecurityMiddleware() breeze.HandlerFunc {
	return SecurityMiddleware(SecurityOptions{
		ContentSecurityPolicy:     "default-src 'self'",
		XFrameOptions:             "DENY",
		XContentTypeOptions:       "nosniff",
		ReferrerPolicy:            "no-referrer",
		StrictTransportSecurity:   "max-age=63072000; includeSubDomains; preload",
		FeaturePolicy:             "geolocation 'none'; microphone 'none'; camera 'none'",
		XXSSProtection:            "1; mode=block",
		ExpectCT:                  "max-age=86400, enforce",
		CrossOriginEmbedderPolicy: "require-corp",
		CrossOriginOpenerPolicy:   "same-origin",
		CrossOriginResourcePolicy: "same-origin",
		CacheControl:              "no-store, no-cache, must-revalidate",
	})
}

// Modifiable headers helper functions.

// WithContentSecurityPolicy returns a SecurityOptions with only the
// Content-Security-Policy header set, for merging with the defaults.
func WithContentSecurityPolicy(csp string) SecurityOptions {
	return SecurityOptions{ContentSecurityPolicy: csp}
}

func WithXFrameOptions(option string) SecurityOptions {
	return SecurityOptions{XFrameOptions: option}
}

func WithReferrerPolicy(policy string) SecurityOptions {
	return SecurityOptions{ReferrerPolicy: policy}
}
