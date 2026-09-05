package oauth2

import (
	"sync"

	"github.com/nelthaarion/breeze"
)

// driverRegistry holds the ProviderDriver for each Provider. It is populated by
// each provider file's init() (RegisterDriver) so adding a provider never
// requires touching middleware code — the single extension point.
var (
	driverMu sync.RWMutex
	drivers  = map[Provider]ProviderDriver{}
)

// RegisterDriver installs a ProviderDriver. It is safe for concurrent use and
// intended to be called from a provider file's init(). Registering the same
// provider twice overwrites the previous driver (last one wins), which lets an
// application swap in a custom implementation.
func RegisterDriver(d ProviderDriver) {
	driverMu.Lock()
	drivers[d.Provider()] = d
	driverMu.Unlock()
}

// lookupDriver returns the driver for p, if registered.
func lookupDriver(p Provider) (ProviderDriver, bool) {
	driverMu.RLock()
	d, ok := drivers[p]
	driverMu.RUnlock()
	return d, ok
}

// router is the minimal surface of *breeze.Router this package needs. Using an
// interface keeps Register testable and decoupled from the concrete type.
type router interface {
	Handle(method breeze.Method, pattern string, handler breeze.HandlerFunc, middlewares ...breeze.HandlerFunc)
	HandleBlocking(method breeze.Method, pattern string, handler breeze.HandlerFunc, middlewares ...breeze.HandlerFunc)
}

// Register wires the full login/callback/logout flow for a provider onto the
// router in a single call:
//
//	oauth2.Register(app.Router, cfg)
//
// It creates:
//
//	GET /login/{provider}            -> Login(cfg)
//	GET /auth/{provider}/callback    -> Callback(cfg)
//	GET /logout/{provider}           -> Logout(cfg)
//
// The callback path always matches the auto-generated RedirectURL so the two
// can never drift. Pass a custom RedirectURL in cfg to change both.
func Register(r router, cfg Config) {
	cfg.mustNormalize()
	slug := cfg.Provider.String()

	// All three are registered as blocking. The callback exchanges the
	// authorization code with the provider over the network and then fetches
	// the userinfo endpoint — two outbound round trips to a third party. Login
	// and Logout are cheap today, but they are part of the same flow and are hit
	// once per session, so there is nothing to win by running them on an event
	// loop and a stalled reactor to lose if a future revision adds a lookup.
	r.HandleBlocking(breeze.GET, "/login/"+slug, Login(cfg))
	r.HandleBlocking(breeze.GET, callbackPath(&cfg), Callback(cfg))
	r.HandleBlocking(breeze.GET, "/logout/"+slug, Logout(cfg))
}

// callbackPath derives the router pattern for the callback from the configured
// RedirectURL, falling back to the conventional /auth/{provider}/callback.
func callbackPath(cfg *Config) string {
	if p := pathFromURL(cfg.RedirectURL); p != "" {
		return p
	}
	return "/auth/" + cfg.Provider.String() + "/callback"
}
