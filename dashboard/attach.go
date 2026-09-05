package dashboard

// This file is intentionally minimal — it shows how to wire the dashboard
// into an existing Breeze application with three lines of code.
//
// A full runnable example lives in the cmd/dashboard-example/ directory.

import (
	"github.com/nelthaarion/breeze"
)

// Attach is a one-liner that installs the dashboard with the default
// configuration onto a Breeze app/router. It returns the Collector so the
// application can push additional data (queries, logs, queue jobs, etc.).
//
// Use Install() directly when you need to pass a custom Config.
func Attach(app *breeze.Breeze, router *breeze.Router) *Collector {
	return Install(app, router, DefaultConfig())
}
