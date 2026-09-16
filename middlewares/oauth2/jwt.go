package oauth2

import (
	"encoding/json"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// sessionClaims is the JWT payload used in SessionModeJWT. It embeds the
// registered claims (exp, iat, nbf, jti) for standard validation and carries
// the normalized user plus the minimal token material needed by Refresh.
type sessionClaims struct {
	jwt.RegisteredClaims
	User  *User  `json:"user"`
	Prov  int    `json:"prov"`
	Token *Token `json:"tok,omitempty"`
}

// issueJWT signs a session JWT (HS256) embedding the user and token. jti is a
// random unique id enabling session rotation / server-side revocation lists.
func issueJWT(cfg *Config, user *User, tok *Token) (string, error) {
	now := time.Now()
	claims := sessionClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "breeze-oauth2",
			Subject:   user.ID,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(cfg.SessionTTL)),
			ID:        randomString(12),
		},
		User:  user,
		Prov:  int(cfg.Provider),
		Token: tok,
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(cfg.JWTSecret))
}

// parseJWT validates a session JWT: HS256 signature, exp/nbf with clock-skew
// leeway, and returns the embedded user + token. Signature verification and
// expiration are enforced by the jwt library; we pin the algorithm to prevent
// "alg=none" / algorithm-confusion attacks.
func parseJWT(cfg *Config, raw string) (*sessionClaims, error) {
	claims := &sessionClaims{}
	_, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		if t.Method.Alg() != jwt.SigningMethodHS256.Alg() {
			return nil, ErrNoSession
		}
		return []byte(cfg.JWTSecret), nil
	}, jwt.WithLeeway(cfg.ClockSkew))
	if err != nil {
		return nil, err
	}
	if claims.User == nil {
		return nil, ErrNoSession
	}
	return claims, nil
}

// idTokenNonce extracts the "nonce" claim from an id_token without verifying
// its signature. The token was already retrieved directly from the
// provider's token endpoint over TLS (back-channel), not passed through the
// browser, so this is used purely to check the nonce we issued was echoed
// back — it is not a substitute for full id_token signature verification.
func idTokenNonce(idToken string) (string, error) {
	claims := jwt.MapClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(idToken, claims); err != nil {
		return "", err
	}
	nonce, _ := claims["nonce"].(string)
	if nonce == "" {
		return "", ErrNonceMismatch
	}
	return nonce, nil
}

// encodeCookieSession serializes the User+Token for SessionModeCookie. The blob
// is signed by the caller (signedValue) so it is tamper-proof; it is not
// encrypted, so it must not carry secrets beyond the OAuth tokens the app
// already trusts the client to hold.
func encodeCookieSession(user *User, tok *Token, ttl time.Duration) (string, error) {
	blob := struct {
		User  *User  `json:"user"`
		Token *Token `json:"token"`
		Exp   int64  `json:"exp"`
	}{User: user, Token: tok, Exp: time.Now().Add(ttl).Unix()}
	b, err := json.Marshal(blob)
	return string(b), err
}

// decodeCookieSession parses a SessionModeCookie payload, rejecting it once
// past Exp so a captured cookie cannot be replayed past its session TTL even
// if the Max-Age/Expires cookie attributes are stripped by the client.
func decodeCookieSession(payload string) (*User, *Token, error) {
	var blob struct {
		User  *User  `json:"user"`
		Token *Token `json:"token"`
		Exp   int64  `json:"exp"`
	}
	if err := json.Unmarshal([]byte(payload), &blob); err != nil {
		return nil, nil, err
	}
	if blob.User == nil {
		return nil, nil, ErrNoSession
	}
	if blob.Exp != 0 && time.Now().Unix() > blob.Exp {
		return nil, nil, ErrNoSession
	}
	return blob.User, blob.Token, nil
}
