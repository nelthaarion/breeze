package middleware

import (
	"github.com/nelthaarion/breeze/v2"
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

	// The security header set is fixed at registration, so its wire block and
	// lookup map are rendered once and replayed on every request. That keeps a
	// security-instrumented response on the serializer's allocation-free fast
	// path instead of building and ranging a twelve-entry map per request (see
	// Context.SetHeaderStatic for the measurement).
	var kv []string
	addKV := func(name, value string) {
		if value != "" {
			kv = append(kv, name, value)
		}
	}
	addKV("Content-Security-Policy", opts.ContentSecurityPolicy)
	addKV("X-Frame-Options", opts.XFrameOptions)
	addKV("X-Content-Type-Options", opts.XContentTypeOptions)
	addKV("Referrer-Policy", opts.ReferrerPolicy)
	addKV("Strict-Transport-Security", opts.StrictTransportSecurity)
	addKV("Permissions-Policy", opts.FeaturePolicy)
	addKV("X-XSS-Protection", opts.XXSSProtection)
	addKV("Expect-CT", opts.ExpectCT)
	addKV("Cross-Origin-Embedder-Policy", opts.CrossOriginEmbedderPolicy)
	addKV("Cross-Origin-Opener-Policy", opts.CrossOriginOpenerPolicy)
	addKV("Cross-Origin-Resource-Policy", opts.CrossOriginResourcePolicy)
	addKV("Cache-Control", opts.CacheControl)
	block, lookup := breeze.RenderStaticHeaders(kv...)

	return func(ctx *breeze.Context) error {
		ctx.SetHeaderStatic(block, lookup)
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
