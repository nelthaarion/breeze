package oauth2

import (
	"context"
	"net/url"
)

// testProvider is the provider slot the tests hijack with stubDriver. We reuse
// the Google constant so Config.normalize resolves a registered driver; each
// testConfig re-registers stubDriver for this provider, pointing at the mock
// server. Tests that need the real Google driver should not run in parallel
// with these (they don't — the suite is deterministic).
const testProvider = Google

// stubDriver is a ProviderDriver whose token/userinfo endpoints point at the
// in-process mock server. It exercises the real HTTP helpers (postForm,
// getJSON, exchangeForm, PKCE passthrough) without reaching the internet.
type stubDriver struct {
	tokenURL string
	userURL  string
}

func (d *stubDriver) Provider() Provider { return testProvider }

func (d *stubDriver) DefaultScopes() []string { return []string{"openid", "email"} }

func (d *stubDriver) AuthURL(cfg *Config, state, nonce, challenge string) string {
	return buildAuthURL("https://provider.test/authorize", cfg, state, nonce, challenge, url.Values{})
}

func (d *stubDriver) Exchange(ctx context.Context, cfg *Config, code, verifier string) (*Token, error) {
	return postForm(ctx, cfg, d.tokenURL, exchangeForm(cfg, code, verifier), ErrTokenExchange)
}

func (d *stubDriver) Refresh(ctx context.Context, cfg *Config, refreshToken string) (*Token, error) {
	return postForm(ctx, cfg, d.tokenURL, refreshForm(cfg, refreshToken), ErrTokenExchange)
}

func (d *stubDriver) UserInfo(ctx context.Context, cfg *Config, tok *Token) (*User, error) {
	var u struct {
		Sub   string `json:"sub"`
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	if err := getJSON(ctx, cfg, d.userURL, tok.AccessToken, &u, nil); err != nil {
		return nil, err
	}
	return &User{
		ID:       u.Sub,
		Email:    u.Email,
		Name:     u.Name,
		Username: u.Email,
		Provider: testProvider,
	}, nil
}

// restoreGoogle re-registers the real Google driver after a test that swapped
// it out, so provider-specific tests (TestGoogleDriver, etc.) see the genuine
// implementation regardless of execution order.
func restoreGoogle() {
	RegisterDriver(&googleDriver{ep: endpoints{
		auth:     "https://accounts.google.com/o/oauth2/v2/auth",
		token:    "https://oauth2.googleapis.com/token",
		userInfo: "https://openidconnect.googleapis.com/v1/userinfo",
	}})
}
