package oauth2

// diag.go — the OAuth2 flow's diagnostic probe.
//
// # The failure this exists for
//
// OAuth2 is the framework's longest failure loop. A misconfiguration does not
// produce an error in the service: it produces a redirect to a provider, which
// redirects back, which fails — and the only party that sees why is the provider's
// error page, in a browser, on someone else's screen. A mismatched RedirectURL, a
// client secret from the wrong environment, a cookie secret that changed when the
// process restarted: from inside the process all three look identical, because all
// three are a login handler that ran correctly.
//
// This probe reports what the flow is configured to do, which is the half nobody
// can see: which providers are wired, the exact redirect URL each one will send,
// whether a real cookie secret was supplied or a process-lifetime random one was
// generated, and logins started against callbacks completed.
//
// That last pair is the diagnostic. Logins started with zero callbacks completed is
// the signature of every redirect-URL mismatch there is, and it is unmistakable —
// the user leaves and never comes back.
//
// # Cost
//
// Every counted event here involves a network round trip to a third party or a
// cookie signature. Nothing is gated: the atomics are unmeasurable against the work
// they accompany, and a login count that depended on whether someone remembered to
// enable counting would be useless for the one question it answers.
//
// The counters are reached through a pointer captured at construction, so no lookup
// happens per request.

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nelthaarion/breeze"
	"github.com/nelthaarion/breeze/diag"
)

// diagName is the registry key, matching the `breeze add oauth2` feature name.
const diagName = "oauth2"

// flowCounts are one provider's ungated counters.
//
// Per provider rather than per process, because an application with Google and
// GitHub configured and one of them broken needs to know which — and an aggregate
// would hide exactly that.
type flowCounts struct {
	loginsStarted     atomic.Uint64
	callbacksOK       atomic.Uint64
	callbacksFailed   atomic.Uint64
	logouts           atomic.Uint64
	sessionsRead      atomic.Uint64
	sessionsRejected  atomic.Uint64
	lastFailureNanos  atomic.Int64
	lastFailureReason atomic.Pointer[string]
}

// flow is one provider's wiring as normalize resolved it, plus its counters.
type flow struct {
	provider     string
	redirectURL  string
	baseURL      string
	sessionMode  string
	cookieName   string
	scopes       []string
	pkce         bool
	secure       bool
	generatedKey bool
	sessionTTL   time.Duration

	counts flowCounts
}

// flows holds one entry per provider, keyed by slug.
//
// A mutex rather than a copy-on-write pointer: registration happens once per
// provider at construction, the probe is the only reader, and the counters live
// behind stable pointers so nothing on a request path touches this map.
var (
	flowsMu sync.Mutex
	flows   = map[string]*flow{}
)

func init() {
	// Registered from init rather than from a constructor, so a project that
	// configured OAuth2 and never called any of the handler constructors still
	// gets an answer — and that project is exactly the one whose login route
	// silently does not exist.
	diag.Register(diagName, probe)
}

// noteFlow records a provider's resolved configuration and returns its counters.
//
// Called from prepareConfig, so every constructor registers, and calling two of
// them for the same provider updates one entry rather than creating two. The
// returned pointer is captured by the handler closure.
func noteFlow(c *Config, generatedKey bool) *flowCounts {
	slug := c.Provider.String()

	flowsMu.Lock()
	defer flowsMu.Unlock()

	f, ok := flows[slug]
	if !ok {
		f = &flow{provider: slug}
		flows[slug] = f
	}
	f.redirectURL = c.RedirectURL
	f.baseURL = c.BaseURL
	f.cookieName = c.CookieName
	f.scopes = c.Scopes
	f.pkce = !c.DisablePKCE
	f.secure = c.Secure
	f.sessionTTL = c.SessionTTL
	f.sessionMode = "cookie"
	if c.SessionMode == SessionModeJWT {
		f.sessionMode = "jwt"
	}
	// Sticky: one constructor given an explicit secret does not clear the warning
	// earned by another that was not.
	f.generatedKey = f.generatedKey || generatedKey

	return &f.counts
}

// failCallback counts a failed callback and then fails the request as usual.
//
// Separate from fail because the same fail is used by Auth and Optional, where a
// missing session is an anonymous visitor rather than a broken flow.
func (fc *flowCounts) failCallback(ctx *breeze.Context, cfg *Config, status int, err error) {
	fc.callbacksFailed.Add(1)
	fc.lastFailureNanos.Store(time.Now().UnixNano())
	if err != nil {
		reason := err.Error()
		fc.lastFailureReason.Store(&reason)
	}
	fail(ctx, cfg, status, err)
}

// probe reports every configured provider.
func probe() diag.Report {
	flowsMu.Lock()
	names := make([]string, 0, len(flows))
	entries := make([]*flow, 0, len(flows))
	for slug, f := range flows {
		names = append(names, slug)
		entries = append(entries, f)
	}
	flowsMu.Unlock()

	if len(entries) == 0 {
		return diag.Off("no OAuth2 provider is configured; call oauth2.Register(router, cfg) " +
			"(or `breeze add oauth2 --provider=…`)").
			WithNotes("Configuring a Config value is not enough on its own — the login, callback " +
				"and logout routes only exist once Register or the individual handler " +
				"constructors have been called.")
	}
	sort.Strings(names)
	sort.Slice(entries, func(i, j int) bool { return entries[i].provider < entries[j].provider })

	var totalLogins, totalOK, totalFailed uint64
	details := make([]map[string]any, 0, len(entries))
	var notes []string
	degraded := false

	for _, f := range entries {
		logins := f.counts.loginsStarted.Load()
		ok := f.counts.callbacksOK.Load()
		failed := f.counts.callbacksFailed.Load()
		totalLogins += logins
		totalOK += ok
		totalFailed += failed

		entry := map[string]any{
			"provider":          f.provider,
			"redirect_url":      f.redirectURL,
			"base_url":          f.baseURL,
			"session_mode":      f.sessionMode,
			"cookie_name":       f.cookieName,
			"scopes":            f.scopes,
			"pkce":              f.pkce,
			"secure_cookies":    f.secure,
			"session_ttl":       f.sessionTTL.String(),
			"logins_started":    logins,
			"callbacks_ok":      ok,
			"callbacks_failed":  failed,
			"logouts":           f.counts.logouts.Load(),
			"sessions_read":     f.counts.sessionsRead.Load(),
			"sessions_rejected": f.counts.sessionsRejected.Load(),
		}
		if nanos := f.counts.lastFailureNanos.Load(); nanos != 0 {
			entry["last_failure_at"] = time.Unix(0, nanos).UTC().Format(time.RFC3339Nano)
		}
		if reason := f.counts.lastFailureReason.Load(); reason != nil {
			entry["last_failure"] = *reason
		}
		details = append(details, entry)

		// The signature failure: the browser left and never came back. Every
		// redirect-URL mismatch looks exactly like this, and nothing else does.
		if logins > 0 && ok == 0 && failed == 0 {
			degraded = true
			notes = append(notes, fmt.Sprintf("%s: %d login(s) were started and no callback ever "+
				"arrived. The browser was redirected to the provider and did not come back, which "+
				"means the provider rejected the request before redirecting — almost always "+
				"because the redirect_uri registered with %s does not match %q exactly, or the "+
				"client ID is for a different application.",
				f.provider, logins, f.provider, f.redirectURL))
		}
		if ok+failed > 0 && failed*2 > ok+failed {
			degraded = true
			notes = append(notes, fmt.Sprintf("%s: over half of callbacks failed (%d of %d). "+
				"Callbacks that arrive and fail are a different fault from callbacks that never "+
				"arrive: the provider is redirecting correctly and something after that — state "+
				"cookie, code exchange, userinfo — is rejecting.", f.provider, failed, ok+failed))
		}
		if f.generatedKey {
			notes = append(notes, fmt.Sprintf("%s: no CookieSecret was set, so one was generated "+
				"for this process. Every session is invalidated by a restart, and two instances "+
				"behind a load balancer cannot read each other's sessions. Set CookieSecret from "+
				"the environment.", f.provider))
		}
		if !f.secure {
			notes = append(notes, fmt.Sprintf("%s: session cookies are not marked Secure, because "+
				"BaseURL (%s) is not https. Over a plain-HTTP origin the session cookie travels in "+
				"clear text.", f.provider, f.baseURL))
		}
		if !f.pkce {
			notes = append(notes, fmt.Sprintf("%s: PKCE is disabled. The authorization code is "+
				"then interceptable by anything that can observe the redirect.", f.provider))
		}
	}

	detail := map[string]any{
		"providers":        names,
		"flows":            details,
		"logins_started":   totalLogins,
		"callbacks_ok":     totalOK,
		"callbacks_failed": totalFailed,
	}

	summary := fmt.Sprintf("%s configured; %d login(s) started, %d completed, %d failed",
		strings.Join(names, ", "), totalLogins, totalOK, totalFailed)

	if degraded {
		return diag.Degraded(summary, detail).WithNotes(notes...)
	}
	return diag.OK(summary, detail).WithNotes(notes...)
}
