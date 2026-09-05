package oauth2

import (
	"context"
	"net/url"
)

// microsoftDriver implements ProviderDriver for the Microsoft identity platform
// (Azure AD v2.0, "common" tenant so both work and personal accounts sign in).
type microsoftDriver struct{ ep endpoints }

func init() {
	RegisterDriver(&microsoftDriver{ep: endpoints{
		auth:     "https://login.microsoftonline.com/common/oauth2/v2.0/authorize",
		token:    "https://login.microsoftonline.com/common/oauth2/v2.0/token",
		userInfo: "https://graph.microsoft.com/oidc/userinfo",
	}})
}

func (d *microsoftDriver) Provider() Provider { return Microsoft }

func (d *microsoftDriver) DefaultScopes() []string {
	// offline_access is required for a refresh token.
	return []string{"openid", "profile", "email", "offline_access"}
}

func (d *microsoftDriver) AuthURL(cfg *Config, state, nonce, challenge string) string {
	extra := url.Values{}
	extra.Set("response_mode", "query")
	return buildAuthURL(d.ep.auth, cfg, state, nonce, challenge, extra)
}

func (d *microsoftDriver) Exchange(ctx context.Context, cfg *Config, code, verifier string) (*Token, error) {
	form := exchangeForm(cfg, code, verifier)
	// The scope must be echoed on the token request for v2.0.
	if len(cfg.Scopes) > 0 {
		form.Set("scope", joinScopes(cfg.Scopes))
	}
	return postForm(ctx, cfg, d.ep.token, form, ErrTokenExchange)
}

func (d *microsoftDriver) Refresh(ctx context.Context, cfg *Config, refreshToken string) (*Token, error) {
	form := refreshForm(cfg, refreshToken)
	if len(cfg.Scopes) > 0 {
		form.Set("scope", joinScopes(cfg.Scopes))
	}
	return postForm(ctx, cfg, d.ep.token, form, ErrTokenExchange)
}

// microsoftUser maps the Graph OIDC userinfo response. Microsoft's stable
// per-app object id is "oid" (falling back to "sub" when oid is absent).
type microsoftUser struct {
	OID     string `json:"oid"`
	Sub     string `json:"sub"`
	Email   string `json:"email"`
	Name    string `json:"name"`
	Picture string `json:"picture"`
}

func (d *microsoftDriver) UserInfo(ctx context.Context, cfg *Config, tok *Token) (*User, error) {
	var mu microsoftUser
	if err := getJSON(ctx, cfg, d.ep.userInfo, tok.AccessToken, &mu, nil); err != nil {
		return nil, err
	}
	id := mu.OID
	if id == "" {
		id = mu.Sub
	}
	return &User{
		ID:       id,
		Email:    mu.Email,
		Name:     mu.Name,
		Username: mu.Email,
		Avatar:   mu.Picture,
		Provider: Microsoft,
	}, nil
}

// joinScopes space-joins scopes (Microsoft token endpoint expects the same
// space-delimited scope string as the auth request).
func joinScopes(scopes []string) string {
	out := ""
	for i, s := range scopes {
		if i > 0 {
			out += " "
		}
		out += s
	}
	return out
}
