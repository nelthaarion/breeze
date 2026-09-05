package oauth2

import "context"

// discordDriver implements ProviderDriver for Discord OAuth2.
type discordDriver struct{ ep endpoints }

func init() {
	RegisterDriver(&discordDriver{ep: endpoints{
		auth:     "https://discord.com/api/oauth2/authorize",
		token:    "https://discord.com/api/oauth2/token",
		userInfo: "https://discord.com/api/users/@me",
	}})
}

func (d *discordDriver) Provider() Provider { return Discord }

func (d *discordDriver) DefaultScopes() []string {
	return []string{"identify", "email"}
}

func (d *discordDriver) AuthURL(cfg *Config, state, nonce, challenge string) string {
	// Discord ignores nonce; PKCE is supported.
	return buildAuthURL(d.ep.auth, cfg, state, "", challenge, nil)
}

func (d *discordDriver) Exchange(
	ctx context.Context,
	cfg *Config,
	code, verifier string,
) (*Token, error) {
	return postForm(ctx, cfg, d.ep.token, exchangeForm(cfg, code, verifier), ErrTokenExchange)
}

func (d *discordDriver) Refresh(
	ctx context.Context,
	cfg *Config,
	refreshToken string,
) (*Token, error) {
	return postForm(ctx, cfg, d.ep.token, refreshForm(cfg, refreshToken), ErrTokenExchange)
}

// discordUser maps the /users/@me response. Discord's stable identifier is the
// snowflake "id". The avatar field is a hash that must be expanded into a CDN
// URL.
type discordUser struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Email    string `json:"email"`
	Avatar   string `json:"avatar"`
	Global   string `json:"global_name"`
}

func (d *discordDriver) UserInfo(ctx context.Context, cfg *Config, tok *Token) (*User, error) {
	var du discordUser
	if err := getJSON(ctx, cfg, d.ep.userInfo, tok.AccessToken, &du, nil); err != nil {
		return nil, err
	}
	name := du.Global
	if name == "" {
		name = du.Username
	}
	return &User{
		ID:       du.ID,
		Email:    du.Email,
		Name:     name,
		Username: du.Username,
		Avatar:   discordAvatarURL(du.ID, du.Avatar),
		Provider: Discord,
	}, nil
}

// discordAvatarURL expands an avatar hash into a full CDN URL; returns "" when
// the user has no custom avatar.
func discordAvatarURL(id, hash string) string {
	if hash == "" {
		return ""
	}
	return "https://cdn.discordapp.com/avatars/" + id + "/" + hash + ".png"
}
