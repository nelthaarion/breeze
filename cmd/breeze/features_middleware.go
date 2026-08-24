package main

// Middleware features. Every one of these ends in a router.Use call, so their
// priorities are the actual nesting order of the request pipeline: recovery at
// 10 is outermost and sees a panic from anything below it, etag at 110 sits
// closest to the handler.
//
// The ordering is not cosmetic. cors before ratelimit means a rejected client
// still gets its preflight answered. jwt after ratelimit means an unauthorized
// flood is dropped before it reaches signature verification. etag last means a
// cached body is never served to a caller auth would have turned away.

import (
	"flag"
	"fmt"
	"strings"
)

func registerMiddlewareFeatures() {
	registerRecovery()
	registerLogging()
	registerSecurity()
	registerCORS()
	registerCompression()
	registerRateLimit()
	registerI18n()
	registerJWT()
	registerOAuth2()
	registerETag()
}

func registerRecovery() {
	register(&feature{
		Name:     "recovery",
		Summary:  "convert handler panics into 500s instead of losing the connection",
		Priority: 10,
		Imports:  []string{middlewareImport},
		Build: func(fs *flag.FlagSet) func(featureCtx) (featureOutput, error) {
			return func(ctx featureCtx) (featureOutput, error) {
				return featureOutput{Body: `func setupRecovery(app *breeze.Breeze, router *breeze.Router) {
	// First in the chain, so it wraps every other middleware as well as the
	// handler. A panic in an inner middleware is just as fatal to the
	// connection as one in your code.
	router.Use(middleware.RecoveryMiddleware())
}`}, nil
			}
		},
	})
}

func registerLogging() {
	register(&feature{
		Name:     "logging",
		Summary:  "per-request method, path, status and duration line",
		Priority: 20,
		Imports:  []string{middlewareImport},
		Build: func(fs *flag.FlagSet) func(featureCtx) (featureOutput, error) {
			return func(ctx featureCtx) (featureOutput, error) {
				return featureOutput{Body: `func setupLogging(app *breeze.Breeze, router *breeze.Router) {
	// Just inside recovery: a request rejected by rate limiting or auth still
	// gets logged, which is exactly when you want the line.
	router.Use(middleware.LoggingMiddleware())
}`}, nil
			}
		},
	})
}

func registerSecurity() {
	register(&feature{
		Name:     "security",
		Summary:  "security response headers (CSP, X-Frame-Options, HSTS, referrer policy)",
		Priority: 30,
		Imports:  []string{middlewareImport},
		Build: func(fs *flag.FlagSet) func(featureCtx) (featureOutput, error) {
			csp := fs.String("csp", "", "Content-Security-Policy value (default: the package default)")
			frame := fs.String("frame-options", "", "X-Frame-Options value, e.g. DENY or SAMEORIGIN")
			referrer := fs.String("referrer-policy", "", "Referrer-Policy value")

			return func(ctx featureCtx) (featureOutput, error) {
				var builders []string
				if *csp != "" {
					builders = append(builders, fmt.Sprintf("middleware.WithContentSecurityPolicy(%q)", *csp))
				}
				if *frame != "" {
					builders = append(builders, fmt.Sprintf("middleware.WithXFrameOptions(%q)", *frame))
				}
				if *referrer != "" {
					builders = append(builders, fmt.Sprintf("middleware.WithReferrerPolicy(%q)", *referrer))
				}

				var body string
				if len(builders) == 0 {
					body = `func setupSecurity(app *breeze.Breeze, router *breeze.Router) {
	// The defaults are conservative and framework-agnostic. If your app loads
	// scripts or styles from a CDN, the default CSP will block them — re-run
	// with --csp to set a policy that matches what you actually serve.
	router.Use(middleware.DefaultSecurityMiddleware())
}`
				} else {
					body = fmt.Sprintf(`func setupSecurity(app *breeze.Breeze, router *breeze.Router) {
	router.Use(middleware.SecurityMiddleware(
		%s,
	))
}`, strings.Join(builders, ",\n\t\t"))
				}

				notes := []string{}
				if len(builders) == 0 {
					notes = append(notes, "Using the default header set — check the CSP against any CDN or inline script your pages rely on.")
				}
				return featureOutput{Body: body, Notes: notes}, nil
			}
		},
	})
}

func registerCORS() {
	register(&feature{
		Name:     "cors",
		Summary:  "cross-origin request headers and preflight handling",
		Priority: 40,
		Imports:  []string{middlewareImport},
		Build: func(fs *flag.FlagSet) func(featureCtx) (featureOutput, error) {
			origins := fs.String("origins", "*", "Access-Control-Allow-Origin value")
			methods := fs.String("methods", "GET,POST,PUT,PATCH,DELETE,OPTIONS", "allowed methods")
			headers := fs.String("headers", "Content-Type,Authorization", "allowed request headers")
			expose := fs.String("expose", "", "headers exposed to the browser")
			credentials := fs.Bool("credentials", false, "allow cookies and credentials")
			maxAge := fs.Int("max-age", 86400, "preflight cache lifetime in seconds")

			return func(ctx featureCtx) (featureOutput, error) {
				// A wildcard origin with credentials is rejected by every
				// browser, so generating that pair would produce code that
				// silently never works. Fail at generation instead.
				if *credentials && *origins == "*" {
					return featureOutput{}, fmt.Errorf(
						"--credentials with --origins=* is refused by browsers: name the origins explicitly, e.g. --origins=https://app.example.com")
				}

				var extra strings.Builder
				if *expose != "" {
					fmt.Fprintf(&extra, "\t\tExposeHeaders:    %q,\n", *expose)
				}
				if *credentials {
					extra.WriteString("\t\tAllowCredentials: \"true\",\n")
				}

				body := fmt.Sprintf(`func setupCors(app *breeze.Breeze, router *breeze.Router) {
	// Ahead of rate limiting and auth: a preflight carries no credentials and
	// is not the request being authorized, so anything that rejects it turns a
	// legitimate cross-origin call into an opaque browser error.
	router.Use(middleware.CORSMiddleware(middleware.CORSOptions{
		AllowOrigins:     %q,
		AllowMethods:     %q,
		AllowHeaders:     %q,
%s		MaxAge:           %q,
	}))
}`, *origins, *methods, *headers, extra.String(), fmt.Sprint(*maxAge))

				notes := []string{}
				if *origins == "*" {
					notes = append(notes, "Origins is \"*\" — any site can call this API. Narrow it before production.")
				}
				return featureOutput{Body: body, Notes: notes}, nil
			}
		},
	})
}

func registerCompression() {
	register(&feature{
		Name:     "compression",
		Summary:  "gzip response bodies for clients that accept it",
		Priority: 50,
		Imports:  []string{middlewareImport},
		Build: func(fs *flag.FlagSet) func(featureCtx) (featureOutput, error) {
			return func(ctx featureCtx) (featureOutput, error) {
				return featureOutput{Body: `func setupCompression(app *breeze.Breeze, router *breeze.Router) {
	// Outside etag, so the ETag is computed over the uncompressed body and
	// stays stable whether or not a given client accepts gzip.
	router.Use(middleware.CompressionMiddleware())
}`}, nil
			}
		},
	})
}

func registerRateLimit() {
	register(&feature{
		Name:     "ratelimit",
		Summary:  "per-client request rate limiting",
		Priority: 60,
		Imports:  []string{middlewareImport, timeImport},
		Build: func(fs *flag.FlagSet) func(featureCtx) (featureOutput, error) {
			requests := fs.Int("requests", 100, "requests allowed per window")
			per := fs.Duration("per", 0, "window length (default 1m)")
			message := fs.String("message", "", "429 response body (default: the package default)")

			return func(ctx featureCtx) (featureOutput, error) {
				if *requests < 1 {
					return featureOutput{}, fmt.Errorf("--requests must be at least 1, got %d", *requests)
				}

				window := "time.Minute"
				if *per > 0 {
					window = fmt.Sprintf("%d * time.Millisecond", per.Milliseconds())
				}

				msgLine := ""
				if *message != "" {
					msgLine = fmt.Sprintf("\t\tMessage:  %q,\n", *message)
				}

				body := fmt.Sprintf(`func setupRatelimit(app *breeze.Breeze, router *breeze.Router) {
	// After cors so preflights are never counted or rejected, before auth so a
	// flood of bad tokens is dropped without paying for signature
	// verification.
	router.Use(middleware.NewRateLimiter(middleware.RateLimiterOptions{
		Requests: %d,
		Per:      %s,
%s	}))
}`, *requests, window, msgLine)

				return featureOutput{Body: body, Notes: []string{
					fmt.Sprintf("Limiting to %d requests per %s.", *requests, strings.TrimPrefix(window, "time.")),
					"The limiter keys on client address, so behind a proxy every request looks like one client — set the proxy to pass a real client IP.",
				}}, nil
			}
		},
	})
}

func registerI18n() {
	register(&feature{
		Name:     "i18n",
		Summary:  "locale detection and message catalogs",
		Priority: 90,
		Imports:  []string{middlewareImport, logImport},
		Build: func(fs *flag.FlagSet) func(featureCtx) (featureOutput, error) {
			locales := fs.String("locales", "en", "comma-separated locales to scaffold")
			dir := fs.String("dir", "./locales", "directory holding the locale JSON files")
			fallback := fs.Bool("fallback", true, "fall back to the default locale for a missing key")
			devMode := fs.Bool("dev", true, "reload catalogs on change")

			return func(ctx featureCtx) (featureOutput, error) {
				list := splitList(*locales)
				if len(list) == 0 {
					return featureOutput{}, fmt.Errorf("--locales cannot be empty")
				}
				def := list[0]

				body := fmt.Sprintf(`// I18n holds the loaded message catalogs. Look up a translation with the
// request's locale, which the middleware below resolves from Accept-Language.
var I18n *breeze.I18n

func setupI18n(app *breeze.Breeze, router *breeze.Router) {
	var err error
	I18n, err = breeze.NewI18n(breeze.I18nConfig{
		Dir:           %q,
		DefaultLocale: %q,
		Fallback:      %t,
		DevMode:       %t,
	})
	if err != nil {
		// NewI18n fails when the directory has no loadable catalogs. That is
		// worth stopping for: continuing would serve every string untranslated
		// with no indication anything is wrong.
		log.Fatalf("i18n: %%v", err)
	}

	router.Use(middleware.LocaleMiddleware(I18n))
}`, *dir, def, *fallback, *devMode)

				// NewI18n returns an error when the locale directory contains
				// no files, so a catalog per locale has to ship with the
				// block — otherwise the generated app fails at boot on the
				// log.Fatalf above.
				files := make(map[string]string, len(list))
				for _, loc := range list {
					files[strings.TrimPrefix(strings.TrimPrefix(*dir, "./"), "/")+"/"+loc+".json"] =
						fmt.Sprintf("{\n  \"greeting\": \"Hello\",\n  \"locale_name\": %q\n}\n", loc)
				}

				return featureOutput{
					Body:  body,
					Files: files,
					Notes: []string{
						fmt.Sprintf("Scaffolded catalogs for %s in %s (default locale: %s).", strings.Join(list, ", "), *dir, def),
						"Catalogs must exist before the app boots — NewI18n treats an empty locale directory as an error.",
					},
				}, nil
			}
		},
	})
}

func registerJWT() {
	register(&feature{
		Name:     "jwt",
		Summary:  "JWT bearer authentication helper for protected routes",
		Priority: 100,
		Imports:  []string{middlewareImport, osImport, logImport},
		Build: func(fs *flag.FlagSet) func(featureCtx) (featureOutput, error) {
			secretEnv := fs.String("secret-env", "JWT_ACCESS_SECRET", "environment variable holding the signing secret")
			refresh := fs.Bool("refresh", false, "enable refresh-token support")
			contextKey := fs.String("context-key", "user", "context key the claims are stored under")

			return func(ctx featureCtx) (featureOutput, error) {
				var extra strings.Builder
				if *refresh {
					fmt.Fprintf(&extra, "\t\tRefreshSecret:      os.Getenv(%q),\n", *secretEnv+"_REFRESH")
					extra.WriteString("\t\tEnableRefreshToken: true,\n")
				}

				body := fmt.Sprintf(`// JWTAuth returns the bearer-token middleware for routes that require a
// signed caller.
//
// It is deliberately not registered globally. router.Use would apply it to
// every route including the one that issues tokens, so login would require the
// token it exists to hand out. Attach it per route instead — Handle takes
// middleware after the handler:
//
//	router.Handle(breeze.GET, "/me", handlers.Me, JWTAuth())
//
// SigningMethod defaults to HS256 and the token is read from the
// Authorization header, so neither needs stating here.
func JWTAuth() breeze.HandlerFunc {
	return middleware.JWTAuthMiddleware(middleware.JWTOptions{
		AccessSecret:   os.Getenv(%[1]q),
%[2]s		UserContextKey: %[3]q,
	})
}

func setupJwt(app *breeze.Breeze, router *breeze.Router) {
	// Nothing global — see JWTAuth above. An empty secret is not a
	// configuration detail: HS256 verification against "" fails for every
	// token, so the routes you meant to protect become permanently
	// unreachable. Say so at boot rather than leaving it to be diagnosed from
	// a wall of 401s.
	if os.Getenv(%[1]q) == "" {
		log.Println("warning: %[1]s is not set — JWTAuth will reject every token")
	}
}`, *secretEnv, extra.String(), *contextKey)

				notes := []string{
					fmt.Sprintf("Set %s before starting the app.", *secretEnv),
					"Apply per route: router.Handle(breeze.GET, \"/me\", handlers.Me, JWTAuth()).",
				}
				if *refresh {
					notes = append(notes, fmt.Sprintf("Refresh tokens are on — also set %s_REFRESH.", *secretEnv))
				}
				return featureOutput{Body: body, Notes: notes}, nil
			}
		},
	})
}

// oauthProviders maps the --provider flag to its Provider constant. Provider
// is an int enum in the oauth2 package, so the generated code has to name the
// constant rather than pass a string.
var oauthProviders = map[string]string{
	"google":    "oauth2.Google",
	"github":    "oauth2.GitHub",
	"microsoft": "oauth2.Microsoft",
	"discord":   "oauth2.Discord",
}

func registerOAuth2() {
	register(&feature{
		Name:     "oauth2",
		Summary:  "OAuth2 login, callback and session for Google/GitHub/Microsoft/Discord",
		Priority: 105,
		Imports:  []string{oauth2Import, osImport, timeImport},
		Build: func(fs *flag.FlagSet) func(featureCtx) (featureOutput, error) {
			provider := fs.String("provider", "google", "identity provider: google, github, microsoft, discord")
			sessionMode := fs.String("session", "cookie", "session persistence: cookie or jwt")
			success := fs.String("success-redirect", "/", "where to send the browser after a successful login")
			failure := fs.String("failure-redirect", "/login", "where to send the browser after a failed login")
			secure := fs.Bool("secure", false, "mark session cookies Secure (required in production)")

			return func(ctx featureCtx) (featureOutput, error) {
				constant, ok := oauthProviders[strings.ToLower(*provider)]
				if !ok {
					return featureOutput{}, fmt.Errorf("unknown provider %q — must be one of: discord, github, google, microsoft", *provider)
				}
				mode := strings.ToLower(*sessionMode)
				if mode != "cookie" && mode != "jwt" {
					return featureOutput{}, fmt.Errorf("unknown --session %q — must be cookie or jwt", *sessionMode)
				}

				secretField := "CookieSecret: os.Getenv(\"OAUTH_COOKIE_SECRET\"),"
				if mode == "jwt" {
					secretField = "JWTSecret:    os.Getenv(\"OAUTH_JWT_SECRET\"),"
				}

				body := fmt.Sprintf(`// OAuth2Config is the provider configuration, kept package-level so the
// per-route helpers below can reuse it.
//
// Credentials come from the environment rather than this file: it is generated
// code that belongs in version control, and a client secret does not.
var OAuth2Config oauth2.Config

func setupOauth2(app *breeze.Breeze, router *breeze.Router) {
	OAuth2Config = oauth2.Config{
		Provider:        %s,
		ClientID:        os.Getenv("OAUTH_CLIENT_ID"),
		ClientSecret:    os.Getenv("OAUTH_CLIENT_SECRET"),
		BaseURL:         os.Getenv("OAUTH_BASE_URL"),
		%s
		SuccessRedirect: %q,
		FailureRedirect: %q,
		Secure:          %t,
		SessionTTL:      24 * time.Hour,
	}

	// Register mounts the whole browser flow: the redirect to the provider,
	// the callback that exchanges the code, and logout. PKCE and state
	// checking are on by default.
	oauth2.Register(router, OAuth2Config)
}

// OAuth2Auth guards routes that require a logged-in user, redirecting to the
// provider when there is no valid session. Use OAuth2Optional instead where a
// page should render for anonymous visitors but knows the user when present.
func OAuth2Auth() breeze.HandlerFunc { return oauth2.Auth(OAuth2Config) }

// OAuth2Optional resolves the session when one exists and passes anonymous
// requests through untouched.
func OAuth2Optional() breeze.HandlerFunc { return oauth2.Optional(OAuth2Config) }`,
					constant, secretField, *success, *failure, *secure)

				envVars := "OAUTH_CLIENT_ID, OAUTH_CLIENT_SECRET, OAUTH_BASE_URL, OAUTH_COOKIE_SECRET"
				if mode == "jwt" {
					envVars = "OAUTH_CLIENT_ID, OAUTH_CLIENT_SECRET, OAUTH_BASE_URL, OAUTH_JWT_SECRET"
				}

				notes := []string{
					fmt.Sprintf("Set %s.", envVars),
					fmt.Sprintf("Register the callback URL with %s as $OAUTH_BASE_URL/auth/%s/callback.", *provider, strings.ToLower(*provider)),
					"Guard routes with OAuth2Auth(); use OAuth2Optional() where anonymous access is fine.",
				}
				if !*secure {
					notes = append(notes, "Session cookies are not marked Secure — fine over localhost, wrong over HTTPS. Re-run with --secure for production.")
				}
				return featureOutput{Body: body, Notes: notes}, nil
			}
		},
	})
}

func registerETag() {
	register(&feature{
		Name:     "etag",
		Summary:  "ETag generation and 304 responses for unchanged bodies",
		Priority: 110,
		Imports:  []string{middlewareImport},
		Build: func(fs *flag.FlagSet) func(featureCtx) (featureOutput, error) {
			return func(ctx featureCtx) (featureOutput, error) {
				body := `// ETagCache holds the hash of every response body it has seen, so a repeat
// request carrying If-None-Match can be answered with 304 instead of the
// body.
//
// Note the store has no eviction: no LRU, no TTL, no size bound. It grows with
// the number of distinct URLs served and never shrinks. That is fine for a
// bounded set of cacheable endpoints and a slow leak for anything with
// unbounded paths or query strings — check middlewares/cache.go before putting
// this in front of user-generated URLs.
var ETagCache = middleware.NewETagCache()

func setupEtag(app *breeze.Breeze, router *breeze.Router) {
	// Innermost of the middlewares, which is the point: auth and rate limiting
	// have already run, so a 304 is never served to a caller who would have
	// been turned away.
	router.Use(ETagCache.ETagMiddleware())
}`
				return featureOutput{Body: body, Notes: []string{
					"The ETag store is unbounded — no LRU or TTL. Keep it off routes with unbounded URLs.",
				}}, nil
			}
		},
	})
}
