# oauth2

Zero-config, secure OAuth2 / OpenID Connect login for [Breeze](https:// github.com/nelthaarion/breeze/v2).

Four providers work out of the box — **Google, GitHub, Microsoft, Discord** — with
PKCE, CSRF-protected state, signed HttpOnly cookies (or JWT sessions), and
transparent token refresh. You wire a login route, a callback route, and protect
whatever you like. That's it.

## Quick start

```go
package main

import (
	"github.com/nelthaarion/breeze/v2"
	"github.com/nelthaarion/breeze/v2/middlewares/oauth2"
)

func main() {
	app := breeze.New()

	cfg := oauth2.Config{
		Provider:     oauth2.Google,
		ClientID:     "your-client-id",
		ClientSecret: "your-client-secret",
		BaseURL:      "https://app.example.com", // used to build the callback URL
		CookieSecret: "a-long-random-secret",    // set this in production
	}

	// 1. Start login.
	app.Router.Handle(breeze.GET, "/auth/google", oauth2.Login(cfg))

	// 2. Handle the provider redirect (matches BaseURL + /auth/google/callback).
	app.Router.Handle(breeze.GET, "/auth/google/callback", oauth2.Callback(cfg))

	// 3. Log out.
	app.Router.Handle(breeze.GET, "/auth/logout", oauth2.Logout(cfg))

	// 4. Protect a route.
	app.Router.Handle(breeze.GET, "/dashboard", dashboard, oauth2.Auth(cfg))

	app.Listen(":3000")
}

func dashboard(ctx *breeze.Context) error {
	user := oauth2.CurrentUser(ctx) // *oauth2.User
	return ctx.JSON(user)
}
```

Only `ClientID` and `ClientSecret` are required. Every other field has a secure
default (see [Configuration](#configuration)).

## Middlewares

| Function        | Purpose                                                                 |
|-----------------|-------------------------------------------------------------------------|
| `Login(cfg)`    | Begins the flow: random state + nonce + PKCE, redirect to the provider. |
| `Callback(cfg)` | Validates state, exchanges the code, fetches the user, writes session.  |
| `Auth(cfg)`     | Requires a valid session; 401 / redirect otherwise.                     |
| `Optional(cfg)` | Attaches the session if present, never blocks.                          |
| `Refresh(cfg)`  | Transparently refreshes an expiring access token, then rotates cookie.  |
| `Logout(cfg)`   | Clears the session and redirects.                                       |

`Refresh` is layered before `Auth` on protected routes when you need fresh
access tokens:

```go
app.Router.Handle(breeze.GET, "/dashboard", dashboard,
	oauth2.Refresh(cfg),
	oauth2.Auth(cfg),
)
```

## Reading the user

From any handler that ran after `Auth`, `Optional`, or `Callback`:

```go
user := oauth2.CurrentUser(ctx)   // *User or nil
tok  := oauth2.CurrentToken(ctx)  // *Token or nil
if oauth2.IsAuthenticated(ctx) {
	// ...
}
```

`UserFrom` / `TokenFrom` are short aliases for `CurrentUser` / `CurrentToken`.
(The accessors are named `CurrentUser`/`CurrentToken` rather than `User`/`Token`
because `User` and `Token` are the struct types.)

The normalized `User` is provider-independent:

```go
type User struct {
	ID       string   // stable provider id (Google sub, GitHub numeric id, ...)
	Email    string
	Name     string
	Username string
	Avatar   string   // URL
	Provider Provider
}
```

## Sessions

Two modes, selected by `Config.SessionMode`:

- **`SessionModeCookie`** (default) — the user + token are serialized into a
  signed, HttpOnly cookie. Tamper-proof (HMAC), stateless server-side.
- **`SessionModeJWT`** — an HS256 JWT carrying the claims, stored in the cookie.
  The algorithm is pinned to prevent `alg=none` / confusion attacks.

Every write **rotates** the session (new JWT `jti` / freshly signed blob) so a
pre-login cookie can't be reused post-login.

## Security

- **PKCE** (S256) on by default for every provider.
- **CSRF**: the `state` is stored in a signed, short-lived, single-use cookie and
  compared in constant time. The flow cookie is cleared on the callback.
- **Open-redirect guard**: `?redirect=` overrides accept same-site paths only.
- **HttpOnly + Secure + SameSite=Lax** cookies. `Secure` auto-enables for
  `https://` `BaseURL`s and can be forced.
- **Bounded provider calls**: every outbound token/userinfo request runs under a
  timeout so a slow provider can't pin a worker.

## Configuration

| Field             | Default                          | Notes                                        |
|-------------------|----------------------------------|----------------------------------------------|
| `Provider`        | —                                | `Google`, `GitHub`, `Microsoft`, `Discord`.  |
| `ClientID`        | — (required)                     |                                              |
| `ClientSecret`    | — (required)                     |                                              |
| `BaseURL`         | `http://localhost:3000`          | Origin used to build `RedirectURL`.          |
| `RedirectURL`     | `BaseURL/auth/{provider}/callback` | The registered callback URL.               |
| `Scopes`          | provider defaults                | Override to request more scopes.             |
| `SessionMode`     | `SessionModeCookie`              | Or `SessionModeJWT`.                         |
| `CookieName`      | `breeze_oauth_session`           |                                              |
| `SuccessRedirect` | `/`                              | Post-login destination.                      |
| `FailureRedirect` | `""` → 401                       | Set to redirect on failure instead.          |
| `CookieSecret`    | random per process               | **Set in production** (survives restarts).   |
| `JWTSecret`       | falls back to `CookieSecret`     | Used in JWT mode.                            |
| `SessionTTL`      | `24h`                            |                                              |
| `StateTTL`        | `10m`                            | Max time to complete a login.                |
| `ClockSkew`       | `1m`                             | Expiry tolerance.                            |
| `Secure`          | `true` (auto for https BaseURL)  | Set `false` only for local http.             |
| `DisablePKCE`     | `false`                          | Leave PKCE on.                               |
| `HTTPClient`      | shared pooled client             | Inject for tests / custom transports.        |

## Default scopes

| Provider  | Scopes                                        |
|-----------|-----------------------------------------------|
| Google    | `openid profile email`                        |
| GitHub    | `read:user user:email`                        |
| Microsoft | `openid profile email offline_access`         |
| Discord   | `identify email`                              |

## Provider notes

- **Google** requests `access_type=offline` + `prompt=consent` so a refresh
  token is returned.
- **GitHub** resolves a private primary email via `/user/emails` when the
  profile hides it; classic OAuth tokens don't expire.
- **Microsoft** uses the `common` tenant (work + personal accounts) and echoes
  scopes on the token request as v2.0 requires.
- **Discord** expands the avatar hash into a CDN URL.

## Testing

```bash
go test -race ./middlewares/oauth2/
```

The suite exercises the full `Login → Callback → Auth` flow against an in-process
mock provider, plus PKCE, cookie signing, JWT, state expiry, config defaults,
open-redirect protection, and concurrent registry access (race-clean).
