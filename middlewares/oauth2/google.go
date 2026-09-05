package oauth2

import (
	"context"
	"net/url"
)

// googleDriver implements ProviderDriver for Google (OpenID Connect).
type googleDriver struct{ ep endpoints }

// init registers the Google driver so it is available with zero configuration
// the moment the package is imported.
func init() {
	RegisterDriver(&googleDriver{ep: endpoints{
		auth:     "https://accounts.google.com/o/oauth2/v2/auth",
		token:    "https://oauth2.googleapis.com/token",
		userInfo: "https://openidconnect.googleapis.com/v1/userinfo",
	}})
}

func (d *googleDriver) Provider() Provider { return Google }

func (d *googleDriver) DefaultScopes() []string {
	return []string{"openid", "profile", "email"}
}

// AuthURL requests offline access + consent so a refresh token is returned on
// first authorization (Google only returns refresh tokens with these params).
func (d *googleDriver) AuthURL(cfg *Config, state, nonce, challenge string) string {
	extra := url.Values{}
	extra.Set("access_type", "offline")
	extra.Set("prompt", "consent")
	return buildAuthURL(d.ep.auth, cfg, state, nonce, challenge, extra)
}

func (d *googleDriver) Exchange(ctx context.Context, cfg *Config, code, verifier string) (*Token, error) {
	return postForm(ctx, cfg, d.ep.token, exchangeForm(cfg, code, verifier), ErrTokenExchange)
}

func (d *googleDriver) Refresh(ctx context.Context, cfg *Config, refreshToken string) (*Token, error) {
	return postForm(ctx, cfg, d.ep.token, refreshForm(cfg, refreshToken), ErrTokenExchange)
}

// googleUser maps the OIDC userinfo response. Google's stable identifier is
// "sub".
type googleUser struct {
	Sub     string `json:"sub"`
	Email   string `json:"email"`
	Name    string `json:"name"`
	Picture string `json:"picture"`
}

func (d *googleDriver) UserInfo(ctx context.Context, cfg *Config, tok *Token) (*User, error) {
	var gu googleUser
	if err := getJSON(ctx, cfg, d.ep.userInfo, tok.AccessToken, &gu, nil); err != nil {
		return nil, err
	}
	return &User{
		ID:       gu.Sub,
		Email:    gu.Email,
		Name:     gu.Name,
		Username: gu.Email,
		Avatar:   gu.Picture,
		Provider: Google,
	}, nil
}
