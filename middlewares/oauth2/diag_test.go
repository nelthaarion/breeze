package oauth2

// diag_test.go — the OAuth2 probe.
//
// The probe's whole job is to make an invisible failure visible. OAuth2's failure
// loop runs through a third party and a browser, so a misconfiguration produces no
// error anywhere in the process — the login handler ran correctly, the user left,
// and nothing came back. The signature is logins started with zero callbacks, and
// that is the assertion this file exists for.
//
// The counters are per-provider and ungated, so these tests do not touch
// diag.EnableCounters: an OAuth2 count that depended on it would be useless for the
// question it answers.

import (
	"strings"
	"testing"

	"github.com/nelthaarion/breeze/diag"
)

// resetFlows clears the per-provider registry so each test starts from nothing.
//
// The registry is process-wide by design — a probe has to report what the process
// is configured to do, not what one test set up — so a test that did not reset
// would inherit the previous one's counts.
func resetFlows(t *testing.T) {
	t.Helper()
	clear := func() {
		flowsMu.Lock()
		flows = map[string]*flow{}
		flowsMu.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

func TestProbeReportsOffWithNoProviderConfigured(t *testing.T) {
	resetFlows(t)

	report := probe()
	if report.Status != diag.StatusOff {
		t.Fatalf("status = %q with nothing configured, want %q", report.Status, diag.StatusOff)
	}
	// The note has to say that a Config value alone is not enough, because that is
	// the mistake: the routes exist only once a constructor has run.
	if !strings.Contains(strings.Join(report.Notes, " "), "not enough on its own") {
		t.Errorf("no note explains that Register must be called: %q", report.Notes)
	}
}

func TestProbeReportsTheResolvedRedirectURL(t *testing.T) {
	resetFlows(t)
	mp := newMockProvider(t)
	defer mp.server.Close()
	t.Cleanup(restoreGoogle)

	cfg := testConfig(t, mp, SessionModeCookie)
	Login(cfg)

	report := probe()
	if report.Status != diag.StatusOK {
		t.Fatalf("status = %q, want %q: %s", report.Status, diag.StatusOK, report.Summary)
	}

	flowDetails, ok := report.Detail["flows"].([]map[string]any)
	if !ok || len(flowDetails) != 1 {
		t.Fatalf("flows = %#v, want one entry", report.Detail["flows"])
	}

	// The redirect URL is the fact nobody can otherwise see, and the one that has
	// to match what was registered with the provider byte for byte.
	want := "https://app.test/auth/google/callback"
	if got := flowDetails[0]["redirect_url"]; got != want {
		t.Errorf("redirect_url = %v, want %v", got, want)
	}
	if flowDetails[0]["pkce"] != true {
		t.Errorf("pkce = %v, want true by default", flowDetails[0]["pkce"])
	}
	if flowDetails[0]["secure_cookies"] != true {
		t.Errorf("secure_cookies = %v for an https BaseURL", flowDetails[0]["secure_cookies"])
	}
}

// TestProbeIsDegradedWhenLoginsStartAndNoCallbackArrives is the signature failure.
//
// Every redirect-URL mismatch looks exactly like this from inside the process, and
// nothing else does: the browser was sent to the provider and never came back.
func TestProbeIsDegradedWhenLoginsStartAndNoCallbackArrives(t *testing.T) {
	resetFlows(t)
	mp := newMockProvider(t)
	defer mp.server.Close()
	t.Cleanup(restoreGoogle)

	cfg := testConfig(t, mp, SessionModeCookie)
	login := Login(cfg)

	for i := 0; i < 3; i++ {
		ctx := newCtx("", nil)
		if err := login(ctx); err != nil {
			t.Fatalf("login: %v", err)
		}
	}

	report := probe()
	if report.Status != diag.StatusDegraded {
		t.Fatalf("status = %q after logins with no callbacks, want %q: %s",
			report.Status, diag.StatusDegraded, report.Summary)
	}

	joined := strings.Join(report.Notes, " ")
	if !strings.Contains(joined, "did not come back") {
		t.Errorf("the note does not describe the failure: %q", report.Notes)
	}
	// It must name the redirect URI as the likely cause, since that is the fix.
	if !strings.Contains(joined, "redirect_uri") {
		t.Errorf("the note does not name redirect_uri as the cause: %q", report.Notes)
	}
	if got, _ := report.Detail["logins_started"].(uint64); got != 3 {
		t.Errorf("logins_started = %v, want 3", report.Detail["logins_started"])
	}
}

// TestProbeCountsAFailedCallbackSeparatelyFromOneThatNeverArrived is the
// distinction that changes what a reader should do next.
//
// A callback that arrives and fails means the provider is redirecting correctly and
// something after that is rejecting — a different fault, in a different place, from
// a callback that never arrives at all.
func TestProbeCountsAFailedCallbackSeparatelyFromOneThatNeverArrived(t *testing.T) {
	resetFlows(t)
	mp := newMockProvider(t)
	defer mp.server.Close()
	t.Cleanup(restoreGoogle)

	cfg := testConfig(t, mp, SessionModeCookie)
	callback := Callback(cfg)

	// No state cookie and no code: the callback fails at its first check, which is
	// what a bookmarked or replayed callback URL produces.
	for i := 0; i < 3; i++ {
		if err := callback(newCtx("", nil)); err != nil {
			t.Fatalf("callback: %v", err)
		}
	}

	report := probe()
	if got, _ := report.Detail["callbacks_failed"].(uint64); got != 3 {
		t.Fatalf("callbacks_failed = %v, want 3", report.Detail["callbacks_failed"])
	}
	if report.Status != diag.StatusDegraded {
		t.Errorf("status = %q with every callback failing, want %q", report.Status, diag.StatusDegraded)
	}
	if !strings.Contains(strings.Join(report.Notes, " "), "arrive and fail") {
		t.Errorf("the note does not distinguish the two failure shapes: %q", report.Notes)
	}

	flowDetails, _ := report.Detail["flows"].([]map[string]any)
	if len(flowDetails) != 1 {
		t.Fatalf("flows = %#v, want one entry", report.Detail["flows"])
	}
	if flowDetails[0]["last_failure"] == nil {
		t.Error("last_failure is absent after three failed callbacks")
	}
	if flowDetails[0]["last_failure_at"] == nil {
		t.Error("last_failure_at is absent after three failed callbacks")
	}
}

// TestProbeWarnsAboutAGeneratedCookieSecret catches the configuration that works
// perfectly in development and logs everyone out on every deploy.
func TestProbeWarnsAboutAGeneratedCookieSecret(t *testing.T) {
	resetFlows(t)
	mp := newMockProvider(t)
	defer mp.server.Close()
	t.Cleanup(restoreGoogle)

	// testConfig supplies a secret, so this builds one without.
	RegisterDriver(&stubDriver{tokenURL: mp.tokenURL, userURL: mp.userURL})
	Login(Config{
		Provider:     testProvider,
		ClientID:     "cid",
		ClientSecret: "secret",
		BaseURL:      "https://app.test",
	})

	joined := strings.Join(probe().Notes, " ")
	if !strings.Contains(joined, "CookieSecret") {
		t.Errorf("no note about the generated cookie secret: %q", probe().Notes)
	}
	if !strings.Contains(joined, "restart") {
		t.Errorf("the note does not name the consequence: %q", probe().Notes)
	}
}

// TestProbeWarnsAboutInsecureCookiesOverPlainHTTP covers the local-development
// setting that is a real exposure once it reaches a server.
func TestProbeWarnsAboutInsecureCookiesOverPlainHTTP(t *testing.T) {
	resetFlows(t)
	mp := newMockProvider(t)
	defer mp.server.Close()
	t.Cleanup(restoreGoogle)

	RegisterDriver(&stubDriver{tokenURL: mp.tokenURL, userURL: mp.userURL})
	Login(Config{
		Provider:     testProvider,
		ClientID:     "cid",
		ClientSecret: "secret",
		BaseURL:      "http://localhost:3000",
		CookieSecret: "test-cookie-secret-0000000000000000",
	})

	if !strings.Contains(strings.Join(probe().Notes, " "), "not marked Secure") {
		t.Errorf("no note about insecure cookies over http: %q", probe().Notes)
	}
}

// TestTwoConstructorsForOneProviderShareOneEntry is why noteFlow keys on the
// provider slug rather than appending.
//
// An application calls Register plus Auth plus Refresh for the same provider, and
// four entries for one provider would make every count look a quarter of its size.
func TestTwoConstructorsForOneProviderShareOneEntry(t *testing.T) {
	resetFlows(t)
	mp := newMockProvider(t)
	defer mp.server.Close()
	t.Cleanup(restoreGoogle)

	cfg := testConfig(t, mp, SessionModeCookie)
	login := Login(cfg)
	Callback(cfg)
	Auth(cfg)
	Logout(cfg)

	if err := login(newCtx("", nil)); err != nil {
		t.Fatalf("login: %v", err)
	}

	report := probe()
	flowDetails, _ := report.Detail["flows"].([]map[string]any)
	if len(flowDetails) != 1 {
		t.Fatalf("four constructors produced %d flow entries, want 1", len(flowDetails))
	}
	if got, _ := report.Detail["logins_started"].(uint64); got != 1 {
		t.Errorf("logins_started = %v, want 1 — the constructors must share a counter", got)
	}
}

func TestProbeIsRegisteredFromInit(t *testing.T) {
	if _, found := diag.Get("oauth2"); !found {
		t.Errorf("no \"oauth2\" probe; registered: %v", diag.Registered())
	}
}
