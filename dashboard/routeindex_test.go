package dashboard

import (
	"fmt"
	"strings"
	"testing"

	"github.com/nelthaarion/breeze/v2"
)

// servedBy asks the real router which route serves (method, path) and returns that route's
// pattern, or "" when none does. Each handler in the fixture answers with its own pattern, so
// the router is asked the question the way a request asks it, and nothing about its tables is
// assumed.
func servedBy(t testing.TB, router *breeze.Router, method breeze.Method, path string) string {
	t.Helper()
	probe := breeze.NewContext(method, path)
	handler, _, _ := router.Find(probe.Req)
	if handler == nil {
		return ""
	}
	ctx := breeze.NewContext(method, path)
	ctx.Res = &breeze.HTTPResponse{}
	if err := handler(ctx); err != nil {
		t.Fatalf("%s %s: handler: %v", method, path, err)
	}
	return string(ctx.Res.Body)
}

type routeSpec struct {
	method  breeze.Method
	pattern string
}

// overlappingRoutes is a table built to make the index and the router disagree if they ever
// follow different rules: parameter routes registered before static ones that they shadow and
// after, wildcards in both orders, the same shape under two parameter names, a root route,
// every method bucket, and enough siblings under one node to make the router build a map.
func overlappingRoutes() []routeSpec {
	routes := []routeSpec{
		{breeze.GET, "/"}, {breeze.POST, "/"},
		{breeze.GET, "/users/:id"}, {breeze.GET, "/users/me"}, // the parameter is first, so it wins /users/me
		{breeze.GET, "/orders/new"}, {breeze.GET, "/orders/:id"}, // the static is first
		{breeze.GET, "/files/*path"}, {breeze.GET, "/files/special/readme"}, // the wildcard is first
		{breeze.GET, "/docs/special/readme"}, {breeze.GET, "/docs/*path"}, // the static is first
		{breeze.GET, "/a/:x/b/:y"}, {breeze.GET, "/a/:x/b/c"}, {breeze.GET, "/a/c/b/:y"}, {breeze.GET, "/a/c/b/c"},
		{breeze.GET, "/dup/:a"}, {breeze.GET, "/dup/:b"},
		{breeze.GET, "/deep/:a/:b/:c/:d/end"},
		{breeze.GET, "/api/v1/ping"}, {breeze.POST, "/api/v1/ping"}, {breeze.PUT, "/api/v1/ping"},
		{breeze.PATCH, "/api/v1/ping"}, {breeze.DELETE, "/api/v1/ping"},
		{breeze.Method("HEAD"), "/head/only"}, {breeze.OPTIONS, "/opt/:id"}, {breeze.Method("TRACE"), "/trace"},
	}
	for i := 0; i < 12; i++ {
		routes = append(routes, routeSpec{breeze.GET, fmt.Sprintf("/many/s%d", i)})
	}
	return append(routes, routeSpec{breeze.GET, "/many/:id"})
}

func parityRouter(t testing.TB) *breeze.Router {
	router := breeze.NewRouter()
	for _, r := range overlappingRoutes() {
		pattern := r.pattern
		router.Handle(r.method, pattern, func(ctx *breeze.Context) error { return ctx.WriteString(pattern) })
	}
	return router
}

// instances turns a route pattern into request paths that hit it, and some that only nearly do.
func instances(pattern string) []string {
	var out []string
	for _, fill := range []string{"42", "me", "new", "special", "x"} {
		segs := strings.Split(strings.Trim(pattern, "/"), "/")
		for i, s := range segs {
			switch {
			case strings.HasPrefix(s, ":"):
				segs[i] = fill
			case strings.HasPrefix(s, "*"):
				segs[i] = fill + "/deeper/still"
			}
		}
		p := "/" + strings.Join(segs, "/")
		out = append(out, p, p+"/", strings.Replace(p, "/", "//", 1), p+"/extra", strings.TrimSuffix(p, "/"+segs[len(segs)-1]))
	}
	return out
}

// TestRouteIndexAgreesWithTheRouter is the test the index exists on: it must attribute a request
// to the route the router served it with, on every path, including where routes overlap.
func TestRouteIndexAgreesWithTheRouter(t *testing.T) {
	router := parityRouter(t)
	c := newCollector(DefaultConfig(), router)
	ix := c.buildRouteIndex()

	methods := []breeze.Method{breeze.GET, breeze.POST, breeze.PUT, breeze.PATCH, breeze.DELETE, breeze.OPTIONS,
		breeze.Method("HEAD"), breeze.Method("TRACE")}

	paths := map[string]bool{"/": true, "": true, "//": true, "/nope": true, "/users": true, "/users/": true,
		"/many": true, "/many/s3": true, "/many/s99": true, "/many/s3/extra": true, "/a/c/b/c": true, "/a/q/b/c": true}
	for _, r := range overlappingRoutes() {
		for _, p := range instances(r.pattern) {
			paths[p] = true
		}
	}

	var matched, unmatched, checked int
	for _, m := range methods {
		for p := range paths {
			want := servedBy(t, router, m, p)
			var got string
			if e := ix.find(string(m), p); e != nil {
				got = e.pattern
			}
			checked++
			if got != want {
				t.Errorf("%s %q: the index says %q, the router served %q", m, p, got, want)
			}
			if want == "" {
				unmatched++
			} else {
				matched++
			}
		}
	}
	// Agreeing about nothing proves nothing.
	if matched < 100 || unmatched < 100 {
		t.Fatalf("the corpus is too one-sided to test anything: %d matched, %d unmatched of %d", matched, unmatched, checked)
	}

	// The overlaps that decide whether the index follows the router's rule or a plausible other.
	for _, tc := range []struct {
		method  breeze.Method
		path    string
		pattern string
	}{
		{breeze.GET, "/users/me", "/users/:id"},               // not the static route registered after it
		{breeze.GET, "/orders/new", "/orders/new"},            // the static route registered first
		{breeze.GET, "/files/special/readme", "/files/*path"}, // not the static route registered after it
		{breeze.GET, "/docs/special/readme", "/docs/special/readme"},
		{breeze.GET, "/docs/special/other", "/docs/*path"},
		{breeze.GET, "/dup/1", "/dup/:a"},
		{breeze.GET, "/a/c/b/c", "/a/:x/b/:y"},
	} {
		e := ix.find(string(tc.method), tc.path)
		if e == nil || e.pattern != tc.pattern {
			t.Errorf("%s %s resolved to %v, want %s", tc.method, tc.path, e, tc.pattern)
		}
		if got := servedBy(t, router, tc.method, tc.path); got != tc.pattern {
			t.Errorf("the premise is wrong: the router serves %s %s with %q, not %q", tc.method, tc.path, got, tc.pattern)
		}
	}
}

// TestRouteIndexLeavesOutTheDashboardsOwnRoutes: the middleware does not instrument them, and
// the routes endpoint would otherwise list them with zero requests.
func TestRouteIndexLeavesOutTheDashboardsOwnRoutes(t *testing.T) {
	router := breeze.NewRouter()
	cfg := DefaultConfig()
	cfg.DisableAuth = true
	coll := Install(nil, router, cfg)
	router.Handle(breeze.GET, "/api/mine", func(ctx *breeze.Context) error { return nil })

	ix := coll.buildRouteIndex()
	if e := ix.find("GET", "/api/mine"); e == nil {
		t.Fatal("the application's route is missing")
	}
	if e := ix.find("GET", "/dashboard/api/logs"); e != nil {
		t.Errorf("the dashboard's own route is in the index: %s", e.pattern)
	}
	for key := range coll.routeStats {
		if strings.Contains(key, "/dashboard") {
			t.Errorf("an accumulator exists for a dashboard route: %s", key)
		}
	}
}
