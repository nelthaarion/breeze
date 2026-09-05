package middleware

// diag_probes.go — the per-middleware probes and their installation flags.
//
// Split from diag.go so that file stays readable as the explanation of the
// mechanism, and this one is the list. Each probe is a few lines because the
// shared countedReport does the work; what differs per middleware is the two
// labels and the configuration facts worth reporting alongside the counts.

import (
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nelthaarion/breeze/diag"
)

// Installation flags. Each constructor sets its own, so a probe can distinguish
// "not installed" from "installed and quiet" — which are the two readings of a
// zero count and want opposite next actions.
//
// atomic.Bool because a constructor may run on any goroutine and a probe reads
// from whichever one is serving the diagnostics request.
var (
	compressionInstalled atomic.Bool
	etagInstalled        atomic.Bool
	rateLimitInstalled   atomic.Bool
	corsInstalled        atomic.Bool
	securityInstalled    atomic.Bool
	jwtInstalled         atomic.Bool
	localeInstalled      atomic.Bool
	loggingInstalled     atomic.Bool
	recoveryInstalled    atomic.Bool
)

// Panic facts for the recovery probe.
//
// Ungated, unlike the counters: see RecoveryMiddleware for why. recoveredPanics
// is the count, lastPanicNanos when the most recent one happened, and lastPanic
// its value — the three things someone asking "did anything panic" wants, and
// none of them recoverable after the fact from anywhere else in the process.
var (
	recoveredPanics atomic.Uint64
	lastPanicNanos  atomic.Int64
	lastPanic       atomic.Pointer[string]
)

// storeLastPanic records the most recent panic value.
//
// Truncated because a panic value can be an arbitrary struct whose %v is
// kilobytes, and a diagnostics endpoint should not become a way to move one.
func storeLastPanic(text string) {
	const max = 300
	if len(text) > max {
		text = text[:max] + "…"
	}
	lastPanic.Store(&text)
}

// Configuration recorded at construction, for the probes that have some worth
// reporting. Written once by a constructor, read by a probe.
var (
	rateLimitConfig atomic.Pointer[RateLimiterOptions]
	corsConfig      atomic.Pointer[CORSOptions]
	securityConfig  atomic.Pointer[SecurityOptions]
	jwtConfig       atomic.Pointer[jwtFacts]
)

// jwtFacts is what the JWT probe reports about how verification is configured.
//
// A distinct struct rather than a *JWTOptions pointer, and this is the point of
// the file: JWTOptions holds the signing secrets. Publishing a pointer to it would
// put them one field access away from a probe, and this probe's output is served
// by GET /dashboard/api/diagnostics. Copying out the facts and deliberately not
// the keys makes the leak impossible rather than merely absent — there is no path
// from a Report back to the secret.
//
// SecretBytes is the length, which is a fact about strength and reveals nothing
// about content. It is here because it is the one remaining way an HMAC secret can
// be too weak now that empty is refused: RFC 7518 §3.2 requires a key at least as
// long as the hash output, and a short passphrase is offline-brute-forceable from
// a single captured token.
type jwtFacts struct {
	Algorithm       string
	SecretBytes     int
	RefreshEnabled  bool
	RefreshBytes    int
	RequiredRoles   []string
	ContextKey      string
	CustomLookup    bool
	CustomValidator bool
	CustomUnauth    bool
}

// minHMACSecretBytes is the shortest HMAC key that is not a finding.
//
// 32 bytes, the output size of SHA-256 and the floor RFC 7518 §3.2 sets for HS256.
// Applied to HS384 and HS512 too, which want more rather than less; reporting the
// weaker bound for them would understate the problem, and this probe's job is to
// say "this is short", not to grade it.
const minHMACSecretBytes = 32

// Handles to the most recently constructed stateful middlewares, so their probes
// can report live sizes rather than a number copied at construction.
var (
	etagCacheHandle   atomic.Pointer[ETagCache]
	rateLimiterHandle atomic.Pointer[RateLimiter]
)

// Per-algorithm compression counters. Three counters rather than a map, because a
// map write on a response path needs a lock and this is exactly the kind of place
// a lock does not belong. Incremented only when counting is on, under the same
// gate read the shared Counter does.
var (
	brotliCount  atomic.Uint64
	gzipCount    atomic.Uint64
	deflateCount atomic.Uint64
)

func init() {
	// Registered from init so that every middleware is diagnosable whether or not
	// it was installed — a probe reporting "compression is not installed" is the
	// answer to "why are my responses not compressed", and it is only available
	// if something registered a probe before the answer was needed.
	//
	// Nine registry appends at process start, each one pointer.
	diag.Register(diagCompression, compressionProbe)
	diag.Register(diagETag, etagProbe)
	diag.Register(diagRateLimit, rateLimitProbe)
	diag.Register(diagCORS, corsProbe)
	diag.Register(diagSecurity, securityProbe)
	diag.Register(diagJWT, jwtProbe)
	diag.Register(diagLocale, localeProbe)
	diag.Register(diagLogging, loggingProbe)
	diag.Register(diagRecovery, recoveryProbe)
}

// compressionProbe reports the compression middleware.
//
// bytes_saved is the number that answers the real question. A high compressed
// count with negligible savings means the bodies are already compact — images, or
// already-gzipped payloads — and the CPU spent on them is waste.
func compressionProbe() diag.Report {
	if !compressionInstalled.Load() {
		return diag.Off("the compression middleware is not installed; add " +
			"middleware.CompressionMiddleware() with router.Use")
	}

	snap := compressionCounter.Snapshot()
	detail := counterDetail(snap, "compressed", "passed_through")
	detail["encodings"] = map[string]uint64{
		"br":      brotliCount.Load(),
		"gzip":    gzipCount.Load(),
		"deflate": deflateCount.Load(),
	}

	summary := fmt.Sprintf("installed; %d response(s) compressed, %d passed through",
		snap.Hits, snap.Misses)
	if snap.BytesSaved > 0 {
		summary += fmt.Sprintf(", %s saved", diag.HumanBytes(snap.BytesSaved))
	}

	report := diag.OK(summary, detail)
	if !snap.Counting {
		return report.WithNotes(notCountingNote)
	}
	if snap.Hits > 0 && snap.BytesSaved*20 < snap.Bytes {
		return report.WithNotes("Compression is saving under 5% of the bytes it processes, which " +
			"means the bodies are already compact — images, or payloads another layer compressed. " +
			"The CPU spent on them is not buying anything.")
	}
	return report
}

// etagProbe reports the ETag middleware.
func etagProbe() diag.Report {
	if !etagInstalled.Load() {
		return diag.Off("the ETag middleware is not installed; construct a " +
			"middleware.NewETagCache() and add its ETagMiddleware() with router.Use")
	}

	snap := etagCounter.Snapshot()
	detail := counterDetail(snap, "not_modified_304", "full_body")
	if cache := etagCacheHandle.Load(); cache != nil {
		cache.mu.RLock()
		detail["stored_entries"] = len(cache.store)
		cache.mu.RUnlock()
	}

	report := diag.OK(fmt.Sprintf("installed; %d 304(s) served, %d full body/bodies",
		snap.Hits, snap.Misses), detail)

	var notes []string
	if !snap.Counting {
		notes = append(notes, notCountingNote)
	}
	notes = append(notes, "The ETag store grows without bound: it has no TTL or LRU eviction, so "+
		"stored_entries rises with the number of distinct URLs ever served. For a path with an "+
		"unbounded parameter that memory cost is unbounded too.")
	return report.WithNotes(notes...)
}

// rateLimitProbe reports the rate limiter.
func rateLimitProbe() diag.Report {
	if !rateLimitInstalled.Load() {
		return diag.Off("the rate limiter is not installed; add " +
			"middleware.NewRateLimiter(opts) with router.Use")
	}

	snap := rateLimitCounter.Snapshot()
	detail := counterDetail(snap, "allowed", "rejected_429")
	if cfg := rateLimitConfig.Load(); cfg != nil {
		detail["limit"] = cfg.Requests
		detail["per"] = cfg.Per.String()
	}
	if rl := rateLimiterHandle.Load(); rl != nil {
		rl.mu.Lock()
		detail["tracked_clients"] = len(rl.clients)
		rl.mu.Unlock()
	}

	summary := fmt.Sprintf("installed; %d request(s) allowed, %d rejected", snap.Hits, snap.Misses)

	if !snap.Counting {
		return diag.OK(summary, detail).WithNotes(notCountingNote)
	}
	// A rejection rate above a tenth means the limit is shaping real traffic
	// rather than catching abuse, which is usually not what was intended.
	if total := snap.Total(); total > 0 && snap.Misses*10 > total {
		return diag.Degraded(summary+" — over 10% of requests are being rejected", detail).
			WithNotes("A rejection rate this high usually means the limit is too low for normal " +
				"traffic rather than that an abuser is being caught. Check limit and per against " +
				"the request rate a legitimate client produces.")
	}
	return diag.OK(summary, detail)
}

// corsProbe reports the CORS middleware.
func corsProbe() diag.Report {
	if !corsInstalled.Load() {
		return diag.Off("the CORS middleware is not installed; add " +
			"middleware.CORSMiddleware(opts) with router.Use")
	}

	snap := corsCounter.Snapshot()
	detail := counterDetail(snap, "preflight_answered", "simple_requests")

	var notes []string
	if !snap.Counting {
		notes = append(notes, notCountingNote)
	}

	if cfg := corsConfig.Load(); cfg != nil {
		detail["allow_origins"] = cfg.AllowOrigins
		detail["allow_methods"] = cfg.AllowMethods
		detail["allow_headers"] = cfg.AllowHeaders
		detail["allow_credentials"] = cfg.AllowCredentials

		if cfg.AllowOrigins == "*" {
			if cfg.AllowCredentials == "true" {
				// Refused by every browser, silently: the response is sent and
				// the browser discards it, so no server-side error ever appears.
				return diag.Degraded("installed with AllowOrigins \"*\" and AllowCredentials \"true\"",
					detail).WithNotes("A wildcard origin with credentials is refused by every " +
					"browser — the request is made, the response is returned, and the browser " +
					"discards it without an error a server can see. Name the origin explicitly.")
			}
			notes = append(notes, "AllowOrigins is \"*\", so any site can read responses from "+
				"this service.")
		}
		if cfg.AllowOrigins == "" {
			notes = append(notes, "AllowOrigins is empty, so no Access-Control-Allow-Origin header "+
				"is sent and every cross-origin read is blocked by the browser. The middleware is "+
				"installed but has nothing to permit.")
		}
	}

	return diag.OK(fmt.Sprintf("installed; %d preflight(s) answered", snap.Hits), detail).
		WithNotes(notes...)
}

// securityProbe reports the security-headers middleware.
//
// The counts matter less here than the configuration: this middleware's whole
// output is headers, so what a reader needs is which ones are actually being set.
func securityProbe() diag.Report {
	if !securityInstalled.Load() {
		return diag.Off("the security-headers middleware is not installed; add " +
			"middleware.SecurityMiddleware(opts) or DefaultSecurityMiddleware() with router.Use")
	}

	snap := securityCounter.Snapshot()
	detail := counterDetail(snap, "responses_secured", "skipped")

	var missing []string
	if cfg := securityConfig.Load(); cfg != nil {
		detail["content_security_policy"] = cfg.ContentSecurityPolicy
		detail["x_frame_options"] = cfg.XFrameOptions
		detail["referrer_policy"] = cfg.ReferrerPolicy
		detail["hsts"] = cfg.StrictTransportSecurity

		if cfg.ContentSecurityPolicy == "" {
			missing = append(missing, "Content-Security-Policy")
		}
		if cfg.XFrameOptions == "" {
			missing = append(missing, "X-Frame-Options")
		}
		if cfg.StrictTransportSecurity == "" {
			missing = append(missing, "Strict-Transport-Security")
		}
	}

	var notes []string
	if !snap.Counting {
		notes = append(notes, notCountingNote)
	}
	if len(missing) > 0 {
		notes = append(notes, fmt.Sprintf("These headers are unset and therefore not sent: %v. "+
			"The middleware is installed, so their absence is a configuration choice rather than "+
			"a missing middleware.", missing))
	}
	return diag.OK(fmt.Sprintf("installed; %d response(s) secured", snap.Hits), detail).
		WithNotes(notes...)
}

// jwtProbe reports the JWT middleware.
//
// # What is deliberately absent
//
// No secret, no prefix of a secret, no hash of one. The endpoint that serves this
// is authenticated by the dashboard's basic auth, which is a no-op when Username or
// Password is empty — so "the reader is already trusted" is not an assumption this
// probe is entitled to make. The signing key is the entire security of the scheme;
// a diagnostics view has no reason to hold it and every reason not to.
//
// Secret *length* is reported instead. It is what distinguishes a real key from a
// passphrase somebody typed, and it says nothing about the key itself.
//
// # Why a short secret is degraded and not a note
//
// HS256 with a key shorter than its 32-byte hash output is brute-forceable offline
// from one captured token — no interaction with the service, no rate limit, no log
// entry. The result is the ability to mint tokens with any claims. That is the same
// outcome as the empty secret the constructor now refuses, reached a few CPU-hours
// later, so it belongs at the same severity the rest of this file reserves for a
// thing that is actively wrong.
func jwtProbe() diag.Report {
	if !jwtInstalled.Load() {
		return diag.Off("the JWT middleware is not installed; add " +
			"middleware.JWTAuthMiddleware(opts) to the routes that need it")
	}

	snap := jwtCounter.Snapshot()
	detail := counterDetail(snap, "accepted", "rejected")

	var notes []string
	if !snap.Counting {
		notes = append(notes, notCountingNote)
	}

	degraded := false
	if facts := jwtConfig.Load(); facts != nil {
		detail["algorithm"] = facts.Algorithm
		detail["secret_bytes"] = facts.SecretBytes
		detail["context_key"] = facts.ContextKey
		detail["refresh_enabled"] = facts.RefreshEnabled
		if facts.RefreshEnabled {
			detail["refresh_secret_bytes"] = facts.RefreshBytes
		}
		if len(facts.RequiredRoles) > 0 {
			detail["required_roles"] = facts.RequiredRoles
		}
		detail["custom_token_lookup"] = facts.CustomLookup
		detail["custom_claims_validator"] = facts.CustomValidator
		detail["custom_unauthorized_handler"] = facts.CustomUnauth

		if strings.HasPrefix(facts.Algorithm, "HS") && facts.SecretBytes < minHMACSecretBytes {
			degraded = true
			notes = append(notes, fmt.Sprintf("AccessSecret is %d bytes. %s needs at least %d "+
				"(RFC 7518 §3.2): a key shorter than the hash output can be recovered offline from "+
				"a single captured token, and whoever recovers it can sign tokens with any claims, "+
				"including any role. Use 32+ bytes of random data, not a passphrase.",
				facts.SecretBytes, facts.Algorithm, minHMACSecretBytes))
		}
		if facts.RefreshEnabled && strings.HasPrefix(facts.Algorithm, "HS") &&
			facts.RefreshBytes < minHMACSecretBytes {
			degraded = true
			notes = append(notes, fmt.Sprintf("RefreshSecret is %d bytes, under the %d-byte "+
				"minimum. A recovered refresh key is worse than a recovered access key: the "+
				"middleware exchanges a valid refresh token for an access token it signs itself.",
				facts.RefreshBytes, minHMACSecretBytes))
		}
	}

	summary := fmt.Sprintf("installed; %d token(s) accepted, %d rejected", snap.Hits, snap.Misses)
	if degraded {
		return diag.Degraded(summary, detail).WithNotes(notes...)
	}
	return diag.OK(summary, detail).WithNotes(notes...)
}

// localeProbe reports the locale middleware.
//
// Separate from the "i18n" probe in the root package, which reports the bundle.
// This one reports whether requests are having a locale resolved at all — a bundle
// can be perfectly loaded while no middleware is selecting from it.
func localeProbe() diag.Report {
	return countedReport(localeInstalled.Load(),
		"the locale middleware is not installed, so no request has a negotiated locale; add "+
			"middleware.LocaleMiddleware(i18n) with router.Use",
		"request(s) with a resolved locale", "resolved", "defaulted", &localeCounter)
}

// loggingProbe reports the request logger.
func loggingProbe() diag.Report {
	return countedReport(loggingInstalled.Load(),
		"the request logger is not installed; add middleware.LoggerMiddleware() with router.Use",
		"request(s) logged", "logged", "skipped", &loggingCounter)
}

// recoveryProbe reports the panic-recovery middleware.
//
// The one probe in this package whose headline is not a count of ordinary work.
// Recovery is only interesting in two states: not installed, which means a
// panicking handler takes the connection down with no response, and installed
// with a non-zero panic count, which means it has been doing that job and
// something in the application is broken.
//
// The panic count is ungated, so it is trustworthy whether or not counted
// diagnostics were ever enabled — see RecoveryMiddleware. The request count is
// gated and therefore carries the usual caveat.
func recoveryProbe() diag.Report {
	if !recoveryInstalled.Load() {
		return diag.Off("the panic-recovery middleware is not installed; add " +
			"middleware.RecoveryMiddleware() with router.Use").
			WithNotes("Without it a panic in a handler propagates out of the chain. On a blocking " +
				"route the worker recovers it and the connection gets no response at all; the " +
				"client sees a hang, then a closed connection, and nothing in the response says " +
				"what happened.")
	}

	panics := recoveredPanics.Load()
	snap := recoveryCounter.Snapshot()

	detail := counterDetail(snap, "requests_completed", "aborted")
	// Overwrites the gated hits reading with the ungated one, because this is the
	// number a reader came for and it means something even with counting off.
	detail["panics_recovered"] = panics
	if nanos := lastPanicNanos.Load(); nanos != 0 {
		detail["last_panic_at"] = time.Unix(0, nanos).UTC().Format(time.RFC3339Nano)
	}
	if last := lastPanic.Load(); last != nil {
		detail["last_panic"] = *last
	}

	if panics == 0 {
		report := diag.OK("installed; no handler has panicked", detail)
		if !snap.Counting {
			return report.WithNotes("The panic count above is exact regardless of whether counted " +
				"diagnostics are enabled — it is not gated. The request counts are.")
		}
		return report
	}

	// Any recovered panic is a degraded application. The middleware did its job,
	// which is precisely why nothing else in the process is going to report this.
	summary := fmt.Sprintf("installed; %d handler panic(s) recovered", panics)
	notes := []string{
		"Each recovered panic returned a bare 500 to a client and printed a stack trace to " +
			"stdout. The stack traces are the only record of where they happened — this probe " +
			"keeps the count and the most recent value, not the traces.",
	}
	if last := lastPanic.Load(); last != nil {
		notes = append(notes, "Most recent panic value: "+*last)
	}
	return diag.Degraded(summary, detail).WithNotes(notes...)
}
