package oauth2

import (
	"context"
	"strconv"
)

// githubDriver implements ProviderDriver for GitHub OAuth apps.
type githubDriver struct {
	ep      endpoints
	emailEP string
}

func init() {
	RegisterDriver(&githubDriver{
		ep: endpoints{
			auth:     "https://github.com/login/oauth/authorize",
			token:    "https://github.com/login/oauth/access_token",
			userInfo: "https://api.github.com/user",
		},
		emailEP: "https://api.github.com/user/emails",
	})
}

func (d *githubDriver) Provider() Provider { return GitHub }

func (d *githubDriver) DefaultScopes() []string {
	return []string{"read:user", "user:email"}
}

func (d *githubDriver) AuthURL(cfg *Config, state, nonce, challenge string) string {
	// GitHub ignores nonce; PKCE is supported and passed through.
	return buildAuthURL(d.ep.auth, cfg, state, "", challenge, nil)
}

func (d *githubDriver) Exchange(
	ctx context.Context,
	cfg *Config,
	code, verifier string,
) (*Token, error) {
	return postForm(ctx, cfg, d.ep.token, exchangeForm(cfg, code, verifier), ErrTokenExchange)
}

// Refresh: classic GitHub OAuth tokens do not expire and have no refresh
// token. GitHub Apps with expiring tokens do; the standard grant is attempted
// and any provider error surfaces to the caller.
func (d *githubDriver) Refresh(
	ctx context.Context,
	cfg *Config,
	refreshToken string,
) (*Token, error) {
	return postForm(ctx, cfg, d.ep.token, refreshForm(cfg, refreshToken), ErrTokenExchange)
}

// githubUser maps the /user response. GitHub's stable identifier is the numeric
// "id"; "login" is the mutable username.
type githubUser struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	AvatarURL string `json:"avatar_url"`
}

// githubEmail is one entry from the /user/emails endpoint, used when the
// primary profile hides the email (common when the user set it private).
type githubEmail struct {
	Email    string `json:"email"`
	Primary  bool   `json:"primary"`
	Verified bool   `json:"verified"`
}

func (d *githubDriver) UserInfo(ctx context.Context, cfg *Config, tok *Token) (*User, error) {
	// GitHub recommends pinning the API version.
	hdr := map[string]string{"X-GitHub-Api-Version": "2022-11-28"}

	var gu githubUser
	if err := getJSON(ctx, cfg, d.ep.userInfo, tok.AccessToken, &gu, hdr); err != nil {
		return nil, err
	}

	email := gu.Email
	if email == "" {
		// Fall back to the emails endpoint and pick the primary verified one.
		var emails []githubEmail
		if err := getJSON(ctx, cfg, d.emailEP, tok.AccessToken, &emails, hdr); err == nil {
			email = pickPrimaryEmail(emails)
		}
	}

	return &User{
		ID:       strconv.FormatInt(gu.ID, 10),
		Email:    email,
		Name:     gu.Name,
		Username: gu.Login,
		Avatar:   gu.AvatarURL,
		Provider: GitHub,
	}, nil
}

// pickPrimaryEmail returns the primary verified email, falling back to the
// first verified, then the first, then "".
func pickPrimaryEmail(emails []githubEmail) string {
	var firstVerified, first string
	for _, e := range emails {
		if first == "" {
			first = e.Email
		}
		if e.Verified && firstVerified == "" {
			firstVerified = e.Email
		}
		if e.Primary && e.Verified {
			return e.Email
		}
	}
	if firstVerified != "" {
		return firstVerified
	}
	return first
}
