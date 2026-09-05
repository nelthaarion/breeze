package oauth2

import "github.com/nelthaarion/breeze"

// Context store keys. Unexported so only this package can write them; readers
// use the typed accessor functions below.
const (
	ctxUserKey  = "oauth2.user"
	ctxTokenKey = "oauth2.token"
)

// setContext stores the authenticated user and token on the Breeze context so
// downstream handlers can retrieve them with CurrentUser()/CurrentToken().
func setContext(ctx *breeze.Context, s *session) {
	if s == nil {
		return
	}
	if s.User != nil {
		ctx.Set(ctxUserKey, s.User)
	}
	if s.Token != nil {
		ctx.Set(ctxTokenKey, s.Token)
	}
}

// CurrentUser returns the authenticated user for the current request, or nil
// when the request is unauthenticated. Safe to call in any handler after Auth,
// Optional or Callback.
//
//	user := oauth2.CurrentUser(ctx)
//	if user != nil { ... }
//
// Note: the accessor is named CurrentUser (not User) because User is the
// identity struct type — Go does not allow a type and a function to share a
// name. UserFrom is provided as a shorter alias.
func CurrentUser(ctx *breeze.Context) *User {
	if v, ok := ctx.Get(ctxUserKey); ok {
		if u, ok := v.(*User); ok {
			return u
		}
	}
	return nil
}

// CurrentToken returns the OAuth token for the current request, or nil when
// absent.
//
//	tok := oauth2.CurrentToken(ctx)
func CurrentToken(ctx *breeze.Context) *Token {
	if v, ok := ctx.Get(ctxTokenKey); ok {
		if t, ok := v.(*Token); ok {
			return t
		}
	}
	return nil
}

// UserFrom is a shorter alias for CurrentUser.
func UserFrom(ctx *breeze.Context) *User { return CurrentUser(ctx) }

// TokenFrom is a shorter alias for CurrentToken.
func TokenFrom(ctx *breeze.Context) *Token { return CurrentToken(ctx) }

// IsAuthenticated reports whether the current request carries an authenticated
// user. Convenience for templates/handlers.
func IsAuthenticated(ctx *breeze.Context) bool {
	return CurrentUser(ctx) != nil
}
