package oauth2

import (
	"encoding/json"
	"time"

	"github.com/nelthaarion/breeze"
)

// stateCookieSuffix is appended to the session cookie name to form the transient
// login-flow cookie name (distinct so it can be deleted independently).
const stateCookieSuffix = "_state"

// flowState is the transient data a login flow must remember between the
// authorization redirect and the callback. It is stored client-side in a
// signed, HttpOnly cookie so the server stays stateless (no server-side store
// to scale or expire).
type flowState struct {
	State     string `json:"s"`           // CSRF state echoed by the provider
	Nonce     string `json:"n,omitempty"` // OIDC nonce (replay protection)
	Verifier  string `json:"v,omitempty"` // PKCE code verifier
	IssuedAt  int64  `json:"iat"`         // unix seconds, for expiry
	Redirect  string `json:"r,omitempty"` // optional post-login redirect override
	SecureSet bool   `json:"sec"`         // whether the flow was initiated over https
}

// stateCookieName returns the transient cookie name for a config.
func stateCookieName(cfg *Config) string { return cfg.CookieName + stateCookieSuffix }

// newFlowState mints a fresh flow with random state + nonce and (optionally) a
// PKCE verifier.
func newFlowState(cfg *Config, verifier string) flowState {
	return flowState{
		State:     randomString(24),
		Nonce:     randomString(16),
		Verifier:  verifier,
		IssuedAt:  time.Now().Unix(),
		SecureSet: cfg.Secure,
	}
}

// save serializes, signs and writes the flow state cookie. It is scoped to
// Path=/ so the callback (on a different path) can read it, and short-lived via
// StateTTL.
func (fs flowState) save(ctx *breeze.Context, cfg *Config) {
	raw, _ := json.Marshal(fs)
	setCookie(ctx, cookieOptions{
		Name:     stateCookieName(cfg),
		Value:    signedValue(cfg.CookieSecret, string(raw)),
		Path:     "/",
		MaxAge:   int(cfg.StateTTL.Seconds()),
		Secure:   cfg.Secure,
		HTTPOnly: true,
		// SameSite=Lax lets the cookie ride along on the top-level GET redirect
		// back from the provider while still blocking cross-site POST CSRF.
		SameSite: "Lax",
	})
}

// loadFlowState reads, verifies and decodes the flow state cookie, enforcing
// the StateTTL expiry (replay/staleness protection).
func loadFlowState(ctx *breeze.Context, cfg *Config) (flowState, error) {
	var fs flowState
	raw, ok := readCookie(ctx, stateCookieName(cfg))
	if !ok {
		return fs, ErrMissingState
	}
	payload, ok := unsignValue(cfg.CookieSecret, raw)
	if !ok {
		return fs, ErrStateMismatch
	}
	if err := json.Unmarshal([]byte(payload), &fs); err != nil {
		return fs, ErrStateMismatch
	}
	age := time.Now().Unix() - fs.IssuedAt
	if age > int64(cfg.StateTTL.Seconds())+int64(cfg.ClockSkew.Seconds()) {
		return fs, ErrExpiredState
	}
	return fs, nil
}

// clear deletes the flow state cookie. Called once the callback consumes it so
// a state can never be replayed (single-use).
func clearFlowState(ctx *breeze.Context, cfg *Config) {
	deleteCookie(ctx, stateCookieName(cfg), "/", cfg.Secure)
}
