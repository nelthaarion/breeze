package oauth2

import (
	"crypto/subtle"

	"github.com/nelthaarion/breeze/v2"
)

// Callback returns the handler for the provider's redirect_uri. It performs the
// entire back half of the OAuth2 dance automatically:
//
//  1. surface any provider error (?error=access_denied),
//  2. validate the state against the signed flow cookie (CSRF protection),
//  3. exchange the authorization code (with the PKCE verifier) for tokens,
//  4. fetch and normalize the user profile,
//  5. write the session (cookie or JWT, rotated),
//  6. redirect to SuccessRedirect (or the per-login ?redirect override).
//
// A downstream handler on the same route can read oauth2.CurrentUser(ctx) /
// oauth2.CurrentToken(ctx), because the callback also populates the context
// before finishing.
func Callback(cfg Config) breeze.HandlerFunc {
	c, counts := prepareConfig(cfg)

	return func(ctx *breeze.Context) error {
		// (1) Provider-side error (user denied consent, etc.).
		if e := ctx.Query("error"); e != "" {
			counts.failCallback(ctx, c, 401, ErrProviderError)
			return nil
		}

		code := ctx.Query("code")
		returnedState := ctx.Query("state")
		if code == "" {
			counts.failCallback(ctx, c, 400, ErrMissingCode)
			return nil
		}
		if returnedState == "" {
			counts.failCallback(ctx, c, 400, ErrMissingState)
			return nil
		}

		// (2) Load + validate flow state (signed, unexpired) and compare the
		// state value in constant time. The flow cookie is single-use: we clear
		// it immediately so a replayed callback cannot succeed.
		fs, err := loadFlowState(ctx, c)
		if err != nil {
			counts.failCallback(ctx, c, 401, err)
			return nil
		}
		clearFlowState(ctx, c)
		if subtle.ConstantTimeCompare([]byte(fs.State), []byte(returnedState)) != 1 {
			counts.failCallback(ctx, c, 401, ErrStateMismatch)
			return nil
		}

		reqCtx, cancel := reqContext()
		defer cancel()

		// (3) Exchange the code (+ PKCE verifier) for tokens.
		tok, err := c.driver.Exchange(reqCtx, c, code, fs.Verifier)
		if err != nil {
			counts.failCallback(ctx, c, 502, err)
			return nil
		}

		// (3b) If the provider returned an id_token, verify its nonce claim
		// matches the one we issued for this flow (ID token replay protection).
		if tok.IDToken != "" {
			nonce, err := idTokenNonce(tok.IDToken)
			if err != nil || subtle.ConstantTimeCompare([]byte(nonce), []byte(fs.Nonce)) != 1 {
				counts.failCallback(ctx, c, 401, ErrNonceMismatch)
				return nil
			}
		}

		// (4) Fetch + normalize the user profile.
		user, err := c.driver.UserInfo(reqCtx, c, tok)
		if err != nil {
			counts.failCallback(ctx, c, 502, err)
			return nil
		}

		// (5) Persist the session (rotated) and expose it on the context.
		if err := writeSession(ctx, c, user, tok); err != nil {
			counts.failCallback(ctx, c, 500, err)
			return nil
		}
		setContext(ctx, &session{User: user, Token: tok})

		// (6) Redirect to the post-login destination.
		dest := c.SuccessRedirect
		if fs.Redirect != "" {
			dest = fs.Redirect
		}
		redirect(ctx, dest)

		counts.callbacksOK.Add(1)
		return nil
	}
}
