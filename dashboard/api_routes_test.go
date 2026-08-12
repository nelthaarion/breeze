package dashboard

import (
	"strings"
	"testing"

	"github.com/nelthaarion/breeze"
)

func invokeDashboardRoute(t *testing.T, router *breeze.Router, method breeze.Method, path string) *breeze.Context {
	t.Helper()
	req := &breeze.HTTPRequest{
		Method: method,
		Path:   path,
		Header: map[string]string{},
	}
	handler, middlewares, params := router.Find(req)
	if handler == nil {
		t.Fatalf("router.Find(%s %s) did not resolve to a handler", method, path)
	}
	ctx := breeze.NewContext(method, path)
	ctx.Req = req
	ctx.SetParams(params)
	ctx.SetMiddlewareChain(middlewares, handler)
	ctx.Next()
	return ctx
}

func TestDashboardIndexServesSPA(t *testing.T) {
	router := breeze.NewRouter()
	cfg := DefaultConfig()
	cfg.DisableAuth = true
	Install(nil, router, cfg)

	ctx := invokeDashboardRoute(t, router, breeze.GET, "/dashboard")
	if ctx.Res == nil {
		t.Fatal("response is nil")
	}
	if ctx.Res.Status != 200 {
		t.Fatalf("status = %d, want 200", ctx.Res.Status)
	}
	if got, want := string(ctx.Res.Body), SPA(); got != want {
		t.Fatalf("dashboard index did not return SPA shell")
	}
	if !strings.Contains(string(ctx.Res.Body), "'database'") {
		t.Fatal("SPA shell is missing database route")
	}
}

func TestLegacyDashboardPagesRedirectToHashRoutes(t *testing.T) {
	router := breeze.NewRouter()
	cfg := DefaultConfig()
	cfg.DisableAuth = true
	Install(nil, router, cfg)

	pages := []string{
		"overview", "routes", "api", "requests",
		"database", "queries",
		"cache", "queue", "scheduler", "logs",
		"health", "performance", "timeline",
	}

	for _, page := range pages {
		page := page
		t.Run(page, func(t *testing.T) {
			path := "/dashboard/" + page
			ctx := invokeDashboardRoute(t, router, breeze.GET, path)
			if ctx.Res == nil {
				t.Fatal("response is nil")
			}
			if ctx.Res.Status != 302 {
				t.Fatalf("status = %d, want 302", ctx.Res.Status)
			}
			if got, want := ctx.Res.Headers["Location"], "/dashboard#/"+page; got != want {
				t.Fatalf("Location = %q, want %q", got, want)
			}
		})
	}
}

func TestSPAScriptRedirectsUnauthorizedAPICallsToLogin(t *testing.T) {
	shell := SPA()
	checks := []string{
		"if(r.status === 401){",
		"window.location.href = S.base + '/login';",
		"throw new Error('unauthorized');",
	}
	for _, check := range checks {
		if !strings.Contains(shell, check) {
			t.Fatalf("SPA shell missing unauthorized redirect snippet: %q", check)
		}
	}
}
