package middleware

// diag_jwt_test.go — the JWT probe reports configuration without reporting keys.
//
// # The two things under test
//
// 1. No secret material reaches the report. GET /dashboard/api/diagnostics serves
//    this, and the dashboard's auth is a no-op when Username or Password is empty,
//    so the probe must not be holding anything worth stealing. The assertion is
//    exhaustive rather than field-by-field: every value in the report is flattened
//    to text and searched for the secrets that were configured.
//
// 2. A short HMAC key is degraded. It is offline-recoverable from one captured
//    token, and a recovered key mints tokens with any claims — the same end state as
//    the empty secret the constructor refuses.
//
// jwtConfig is process-wide and last-writer-wins, so these tests construct the
// middleware themselves and do not depend on ordering against other files.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/nelthaarion/breeze/v2/diag"
)

// longSecret is 32 bytes — the minimum the probe accepts without comment.
const longSecret = "0123456789abcdef0123456789abcdef"

func TestJWTProbeNeverReportsSecretMaterial(t *testing.T) {
	const (
		access  = "the-access-secret-which-is-long-enough"
		refresh = "the-refresh-secret-which-is-also-long"
	)

	JWTAuthMiddleware(JWTOptions{
		AccessSecret:       access,
		RefreshSecret:      refresh,
		EnableRefreshToken: true,
	})

	report := jwtProbe()

	// Flattened rather than checked key by key: a future field that carried the
	// secret would pass a per-key assertion simply by not being in its list.
	haystack := report.Summary + " " + strings.Join(report.Notes, " ") +
		" " + fmt.Sprint(report.Detail)

	for name, secret := range map[string]string{
		"AccessSecret":  access,
		"RefreshSecret": refresh,
	} {
		if strings.Contains(haystack, secret) {
			t.Errorf("the %s appears in the diagnostics report:\n%s", name, haystack)
		}
	}

	// Length is reported, because it is the remaining way to be wrong.
	if got := report.Detail["secret_bytes"]; got != len(access) {
		t.Errorf("secret_bytes = %v, want %d", got, len(access))
	}
	if got := report.Detail["refresh_secret_bytes"]; got != len(refresh) {
		t.Errorf("refresh_secret_bytes = %v, want %d", got, len(refresh))
	}
}

func TestJWTProbeReportsAShortSecretAsDegraded(t *testing.T) {
	JWTAuthMiddleware(JWTOptions{AccessSecret: "short"})

	report := jwtProbe()
	if report.Status != diag.StatusDegraded {
		t.Fatalf("status = %q for a 5-byte HS256 key, want %q: %s",
			report.Status, diag.StatusDegraded, report.Summary)
	}

	// The note has to say what an attacker gets, not merely that the key is short:
	// "use a longer secret" reads as tidiness, and this is a full bypass.
	joined := strings.Join(report.Notes, " ")
	if !strings.Contains(joined, "offline") {
		t.Errorf("the note does not explain the consequence: %q", report.Notes)
	}
}

func TestJWTProbeAcceptsALongSecret(t *testing.T) {
	JWTAuthMiddleware(JWTOptions{AccessSecret: longSecret})

	report := jwtProbe()
	if report.Status != diag.StatusOK {
		t.Fatalf("status = %q for a 32-byte key, want %q: %s\nnotes: %q",
			report.Status, diag.StatusOK, report.Summary, report.Notes)
	}
	if got := report.Detail["algorithm"]; got != "HS256" {
		t.Errorf("algorithm = %v, want HS256", got)
	}
}

// TestJWTProbeFlagsAShortRefreshSecret covers the half a single check would miss.
//
// The access key is fine here, so a probe that only looked at AccessSecret would
// report OK on a configuration whose refresh path is forgeable — and a forged
// refresh token is exchanged for an access token the middleware signs itself.
func TestJWTProbeFlagsAShortRefreshSecret(t *testing.T) {
	JWTAuthMiddleware(JWTOptions{
		AccessSecret:       longSecret,
		RefreshSecret:      "short",
		EnableRefreshToken: true,
	})

	report := jwtProbe()
	if report.Status != diag.StatusDegraded {
		t.Fatalf("status = %q with a 5-byte refresh key, want %q", report.Status, diag.StatusDegraded)
	}
	if !strings.Contains(strings.Join(report.Notes, " "), "RefreshSecret") {
		t.Errorf("no note names RefreshSecret: %q", report.Notes)
	}
}

// TestJWTProbeReportsTheDefaultsItApplied keeps the report honest about what the
// constructor filled in: reading opts after the defaults were applied would report
// a custom lookup and a custom 401 handler for a configuration that supplied
// neither.
func TestJWTProbeReportsTheDefaultsItApplied(t *testing.T) {
	JWTAuthMiddleware(JWTOptions{AccessSecret: longSecret})

	report := jwtProbe()
	for _, key := range []string{
		"custom_token_lookup", "custom_claims_validator", "custom_unauthorized_handler",
	} {
		if got := report.Detail[key]; got != false {
			t.Errorf("%s = %v, want false — nothing custom was supplied", key, got)
		}
	}
	if got := report.Detail["context_key"]; got != "user" {
		t.Errorf("context_key = %v, want the applied default \"user\"", got)
	}
}
