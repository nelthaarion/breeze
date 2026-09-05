package oauth2

import "github.com/nelthaarion/breeze"

// Refresh returns a middleware that transparently refreshes an access token
// that is expired (or within ClockSkew of expiring) using the stored refresh
// token, then rotates the session cookie with the new token. If there is no
// session or no refresh token it behaves like Optional (continues without
// blocking) so it can be layered in front of Auth.
//
// Typical usage places Refresh before Auth on protected routes:
//
//	app.Router.Handle(breeze.GET, "/dashboard",
//	    DashboardHandler,
//	    oauth2.Refresh(cfg),
//	    oauth2.Auth(cfg),
//	)
func Refresh(cfg Config) breeze.HandlerFunc {
	c, counts := prepareConfig(cfg)

	return func(ctx *breeze.Context) error {
		s, err := readSession(ctx, c)
		if err != nil {
			// No session to refresh; let downstream (e.g. Auth) decide — including
			// whatever error it reports.
			return ctx.Next()
		}
		counts.sessionsRead.Add(1)

		// Only refresh when we have a refresh token AND the access token is
		// (near) expired. This keeps the common case allocation- and
		// network-free.
		if s.Token != nil && s.Token.RefreshToken != "" && needsRefresh(c, s.Token) {
			reqCtx, cancel := reqContext()
			newTok, rerr := c.driver.Refresh(reqCtx, c, s.Token.RefreshToken)
			cancel()
			if rerr == nil && newTok != nil {
				// Providers often omit the refresh token on refresh responses;
				// carry the previous one forward so the session stays
				// refreshable.
				if newTok.RefreshToken == "" {
					newTok.RefreshToken = s.Token.RefreshToken
				}
				s.Token = newTok
				_ = writeSession(ctx, c, s.User, s.Token) // rotate cookie
			}
		}

		setContext(ctx, s)
		return ctx.Next()
	}
}

// needsRefresh reports whether the token is expired or will expire within the
// configured clock-skew window.
func needsRefresh(c *Config, tok *Token) bool {
	if tok.Expiry.IsZero() {
		return false // no expiry info: nothing to refresh
	}
	// expired or within ClockSkew of expiry.
	return !tok.Expiry.After(nowPlus(c.ClockSkew))
}
