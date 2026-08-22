package breeze

import "testing"

// router_index_test.go — the exact-path map must agree with the ordered scan.
//
// lookup consults the map before it scans, so a route that is in the map is
// served without the scan ever running. Route matching is first-registered-wins,
// which means a static route may only be in the map when no earlier route in its
// bucket could also match that path. Get this wrong and the router silently
// serves the wrong handler — the failure mode is a correct-looking 200 with the
// wrong body, which no smoke test catches.
//
// These tests pin both directions: shadowed routes stay out, unshadowed ones get
// in, and the answer lookup returns is the one the scan alone would have given.

// mapHolds reports whether the bucket for method has path in its exact-path map.
// path is the normalized form lookup uses: no leading or trailing slash.
func mapHolds(r *Router, method Method, path string) bool {
	idx := &r.byMethod[methodBucket(method)]
	if idx.static == nil {
		return false
	}
	_, ok := idx.static[path]
	return ok
}

func lookupPath(t *testing.T, r *Router, method Method, path string) *route {
	t.Helper()
	rt, params, _ := r.lookup(&HTTPRequest{Method: method, Path: path})
	if params != nil {
		releaseParams(params)
	}
	return rt
}

// TestStaticMapExcludesShadowedRoute is the case the map must refuse. Both
// routes have two segments and "/users/:id" is registered first, so the scan
// reaches it before "/users/me" ever gets a chance. If "/users/me" were in the
// map, lookup would answer with it and quietly contradict the scan.
func TestStaticMapExcludesShadowedRoute(t *testing.T) {
	r := NewRouter()
	r.autoServeRoot = false
	r.Handle(GET, "/users/:id", func(*Context) {})
	r.Handle(GET, "/users/me", func(*Context) {})

	if mapHolds(r, GET, "users/me") {
		t.Fatal("shadowed static route was admitted to the exact-path map")
	}

	rt := lookupPath(t, r, GET, "/users/me")
	if rt == nil {
		t.Fatal("no route matched /users/me")
	}
	if rt.pattern != "/users/:id" {
		t.Fatalf("first-registered route lost: got %q, want %q", rt.pattern, "/users/:id")
	}
}

// TestStaticMapAdmitsRouteAfterWildcard is the case the map must accept, and the
// one the old whole-bucket seal got wrong. ServeStatic registers a wildcard, and
// under the seal every static GET registered afterwards fell back to the ordered
// scan for the rest of the process's life. A wildcard rooted at "files" cannot
// match "/users", so there is nothing to protect against here.
func TestStaticMapAdmitsRouteAfterWildcard(t *testing.T) {
	r := NewRouter()
	r.autoServeRoot = false
	r.HandleBlocking(GET, "/files/*", func(*Context) {})
	r.Handle(GET, "/users", func(*Context) {})
	r.Handle(GET, "/", func(*Context) {})

	if !mapHolds(r, GET, "users") {
		t.Error("/users was kept out of the exact-path map by an unrelated wildcard")
	}
	if !mapHolds(r, GET, "") {
		t.Error("/ was kept out of the exact-path map by an unrelated wildcard")
	}

	if rt := lookupPath(t, r, GET, "/users"); rt == nil || rt.pattern != "/users" {
		t.Fatalf("lookup(/users) = %v, want /users", rt)
	}
	if rt := lookupPath(t, r, GET, "/files/a/b.txt"); rt == nil || rt.pattern != "/files/*" {
		t.Fatalf("lookup(/files/a/b.txt) = %v, want /files/*", rt)
	}
}

// TestStaticMapAdmitsDifferentSegmentCount covers the other half of the same
// point: a param route only shadows paths with a matching shape, so a
// single-segment static route registered after "/users/:id" is still eligible.
func TestStaticMapAdmitsDifferentSegmentCount(t *testing.T) {
	r := NewRouter()
	r.autoServeRoot = false
	r.Handle(GET, "/users/:id", func(*Context) {})
	r.Handle(GET, "/health", func(*Context) {})

	if !mapHolds(r, GET, "health") {
		t.Error("/health was kept out of the map by a two-segment param route")
	}
	if rt := lookupPath(t, r, GET, "/health"); rt == nil || rt.pattern != "/health" {
		t.Fatalf("lookup(/health) = %v, want /health", rt)
	}
}

// TestStaticMapRootWildcardShadowsEverything is the conservative extreme: a
// wildcard at the root matches any path, so no static route registered after it
// may be admitted.
func TestStaticMapRootWildcardShadowsEverything(t *testing.T) {
	r := NewRouter()
	r.autoServeRoot = false
	r.HandleBlocking(GET, "/*", func(*Context) {})
	r.Handle(GET, "/users", func(*Context) {})

	if mapHolds(r, GET, "users") {
		t.Fatal("root wildcard did not shadow a later static route")
	}
	if rt := lookupPath(t, r, GET, "/users"); rt == nil || rt.pattern != "/*" {
		t.Fatalf("lookup(/users) = %v, want /*", rt)
	}
}

// TestStaticMapEligibilityMatchesScan is the property the two hand-written cases
// above are examples of: for every registered path, answering from the map must
// give the same route as scanning would. It re-runs each lookup with the map
// removed and compares.
func TestStaticMapEligibilityMatchesScan(t *testing.T) {
	paths := []string{
		"/users", "/users/:id", "/users/me", "/users/me/settings",
		"/health", "/files/*", "/", "/a/:b/c",
	}

	r := NewRouter()
	r.autoServeRoot = false
	for _, p := range paths {
		r.Handle(GET, p, func(*Context) {})
	}

	// Concrete request paths, including ones that only a param or wildcard
	// route can answer.
	probes := []string{
		"/users", "/users/42", "/users/me", "/users/me/settings",
		"/health", "/files/x/y.txt", "/", "/a/1/c", "/nope", "/a/b/c/d",
	}

	idx := &r.byMethod[bucketGET]
	saved := idx.static

	for _, p := range probes {
		idx.static = saved
		withMap := lookupPath(t, r, GET, p)

		idx.static = nil // force the ordered scan
		scanOnly := lookupPath(t, r, GET, p)

		if withMap != scanOnly {
			gotPattern, wantPattern := "<nil>", "<nil>"
			if withMap != nil {
				gotPattern = withMap.pattern
			}
			if scanOnly != nil {
				wantPattern = scanOnly.pattern
			}
			t.Errorf("%s: map answered %q, scan answered %q", p, gotPattern, wantPattern)
		}
	}
	idx.static = saved
}
