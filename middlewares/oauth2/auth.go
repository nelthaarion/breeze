package oauth2

import "github.com/nelthaarion/breeze/v2"

// Auth returns a middleware that requires a valid session. On success it
// populates the context (CurrentUser/CurrentToken) and calls the next handler.
// On failure it redirects to FailureRedirect when set, otherwise writes 401 —
// and never calls the protected handler.
//
//	app.Router.Handle(breeze.GET, "/dashboard",
//	    DashboardHandler,
//	    oauth2.Auth(cfg), // as a route middleware
//	)
func Auth(cfg Config) breeze.HandlerFunc {
	c, counts := prepareConfig(cfg)

	return func(ctx *breeze.Context) error {
		s, err := readSession(ctx, c)
		if err != nil {
			counts.sessionsRejected.Add(1)
			fail(ctx, c, 401, ErrNoSession)
			return nil
		}
		counts.sessionsRead.Add(1)
		setContext(ctx, s)
		return ctx.Next()
	}
}
