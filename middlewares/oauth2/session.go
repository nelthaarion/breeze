package oauth2

import "github.com/nelthaarion/breeze"

// session is the resolved, authenticated session attached to a request after a
// successful Auth/Optional/Callback. It is what oauth2.User and oauth2.Token
// read from the Context.
type session struct {
	User  *User
	Token *Token
}

// writeSession persists an authenticated session into the response cookie using
// the configured SessionMode. Writing always issues a fresh cookie value
// (session rotation): a new JWT jti or a freshly signed cookie blob on every
// write, so a stolen pre-login cookie cannot be reused post-login.
func writeSession(ctx *breeze.Context, cfg *Config, user *User, tok *Token) error {
	var value string
	switch cfg.SessionMode {
	case SessionModeJWT:
		jwtStr, err := issueJWT(cfg, user, tok)
		if err != nil {
			return err
		}
		value = jwtStr
	default: // SessionModeCookie
		payload, err := encodeCookieSession(user, tok, cfg.SessionTTL)
		if err != nil {
			return err
		}
		value = signedValue(cfg.CookieSecret, payload)
	}

	setCookie(ctx, cookieOptions{
		Name:     cfg.CookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   int(cfg.SessionTTL.Seconds()),
		Secure:   cfg.Secure,
		HTTPOnly: true,
		SameSite: "Lax",
	})
	return nil
}

// readSession loads and validates the session cookie, returning the session or
// an error. In JWT mode the token signature + expiry are verified; in cookie
// mode the HMAC signature is verified. Both modes reject tampered or absent
// cookies with ErrNoSession.
func readSession(ctx *breeze.Context, cfg *Config) (*session, error) {
	raw, ok := readCookie(ctx, cfg.CookieName)
	if !ok || raw == "" {
		return nil, ErrNoSession
	}

	switch cfg.SessionMode {
	case SessionModeJWT:
		claims, err := parseJWT(cfg, raw)
		if err != nil {
			return nil, ErrNoSession
		}
		return &session{User: claims.User, Token: claims.Token}, nil
	default: // SessionModeCookie
		payload, ok := unsignValue(cfg.CookieSecret, raw)
		if !ok {
			return nil, ErrNoSession
		}
		user, tok, err := decodeCookieSession(payload)
		if err != nil {
			return nil, ErrNoSession
		}
		return &session{User: user, Token: tok}, nil
	}
}

// clearSession deletes the session cookie (logout).
func clearSession(ctx *breeze.Context, cfg *Config) {
	deleteCookie(ctx, cfg.CookieName, "/", cfg.Secure)
}
