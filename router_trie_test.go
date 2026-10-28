package breeze

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

// lookupScanOracle is the ordered scan Router.lookup used before the segment trie,
// kept verbatim as an executable specification: the first route in registration
// order that matches wins.
func lookupScanOracle(r *Router, req *HTTPRequest) (*route, map[string]string) {
	path := req.Path
	if len(path) > 0 && path[0] == '/' {
		path = path[1:]
	}
	if len(path) > 0 && path[len(path)-1] == '/' {
		path = path[:len(path)-1]
	}
	idx := &r.byMethod[methodBucket(req.Method)]
	var reqSegments []string
	if path != "" {
		reqSegments = strings.Split(path, "/")
	}
	nReq := len(reqSegments)
	checkMethod := methodBucket(req.Method) == bucketOther
	for _, rt := range idx.routes {
		if checkMethod && rt.method != req.Method {
			continue
		}
		if rt.hasWildcard {
			if nReq < len(rt.segments) {
				continue
			}
			match := true
			for i, rseg := range rt.segments {
				if rt.paramIndex[i] {
					continue
				}
				if rseg != reqSegments[i] {
					match = false
					break
				}
			}
			if !match {
				continue
			}
			params := map[string]string{}
			for i, rseg := range rt.segments {
				if rt.paramIndex[i] {
					params[rseg[1:]] = reqSegments[i]
				}
			}
			params[rt.wildcardName] = strings.Join(reqSegments[len(rt.segments):], "/")
			return rt, params
		}
		if len(rt.segments) != nReq {
			continue
		}
		match := true
		for i, rseg := range rt.segments {
			if !rt.paramIndex[i] && rseg != reqSegments[i] {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		if rt.paramCount > 0 {
			params := map[string]string{}
			for i, rseg := range rt.segments {
				if rt.paramIndex[i] {
					params[rseg[1:]] = reqSegments[i]
				}
			}
			return rt, params
		}
		return rt, nil
	}
	return nil, nil
}

func trieRandPattern(rng *rand.Rand) string {
	alphabet := []string{"a", "b", "c", "users", "x", ""}
	segs := make([]string, 0, 6)
	for i, n := 0, rng.Intn(5); i < n; i++ {
		if rng.Intn(5) < 3 {
			segs = append(segs, alphabet[rng.Intn(len(alphabet))])
		} else {
			segs = append(segs, fmt.Sprintf(":p%d", rng.Intn(3)))
		}
	}
	if rng.Intn(5) == 0 {
		if rng.Intn(2) == 0 {
			segs = append(segs, "*")
		} else {
			segs = append(segs, fmt.Sprintf("*w%d", rng.Intn(2)))
		}
	}
	return "/" + strings.Join(segs, "/")
}

func trieRandPath(rng *rand.Rand) string {
	pool := []string{"a", "b", "c", "users", "x", "", "zzz", ":p0", "*"}
	segs := make([]string, rng.Intn(6))
	for i := range segs {
		segs[i] = pool[rng.Intn(len(pool))]
	}
	p := strings.Join(segs, "/")
	if rng.Intn(10) != 0 {
		p = "/" + p
	}
	if rng.Intn(5) == 0 {
		p += "/"
	}
	return p
}

// The segment trie must return exactly what the ordered scan returned: the same
// route, and the same params, on random route tables full of overlaps (a :param
// registered before a literal it shadows, duplicate shapes, wildcards that also
// match zero segments, empty segments, several methods).
func TestRouterTrieMatchesOrderedScan(t *testing.T) {
	methods := []Method{GET, GET, GET, POST, PUT, Method("HEAD"), Method("PURGE")}
	rng := rand.New(rand.NewSource(20261028))
	checked, hits := 0, 0
	for table := 0; table < 1500; table++ {
		r := NewRouter()
		for i, n := 0, 1+rng.Intn(45); i < n; i++ {
			r.Handle(methods[rng.Intn(len(methods))], trieRandPattern(rng), trieNoop)
		}
		for q := 0; q < 250; q++ {
			req := &HTTPRequest{Method: methods[rng.Intn(len(methods))], Path: trieRandPath(rng)}
			gotRt, gotParams, _ := r.lookup(req)
			wantRt, wantParams := lookupScanOracle(r, req)
			checked++
			if gotRt != nil {
				hits++
			}
			if gotRt != wantRt {
				t.Fatalf("table %d: %s %q: route mismatch: got %v want %v", table, req.Method, req.Path, patternOf(gotRt), patternOf(wantRt))
			}
			if (gotParams == nil) != (wantParams == nil) || (gotParams != nil && !reflect.DeepEqual(gotParams, wantParams)) {
				t.Fatalf("table %d: %s %q (%s): params got %v want %v", table, req.Method, req.Path, patternOf(gotRt), gotParams, wantParams)
			}
			if gotParams != nil {
				releaseParams(gotParams)
			}
		}
	}
	if hits < checked/10 {
		t.Fatalf("generator too sparse: only %d of %d lookups matched a route", hits, checked)
	}
}

func patternOf(rt *route) string {
	if rt == nil {
		return "<none>"
	}
	return string(rt.method) + " " + rt.pattern
}

func TestRouterTrieFirstRegisteredWins(t *testing.T) {
	r := NewRouter()
	r.Handle(GET, "/users/:id", trieNoop)   // 0
	r.Handle(GET, "/users/me", trieNoop)    // 1: shadowed by 0, as before
	r.Handle(GET, "/files/*path", trieNoop) // 2
	r.Handle(GET, "/files", trieNoop)       // 3: shadowed, the wildcard matches zero segments
	r.Handle(GET, "/a/b/c", trieNoop)       // 4
	r.Handle(GET, "/a/:x/d", trieNoop)      // 5
	r.Handle(GET, "/", trieNoop)            // 6
	cases := []struct {
		path   string
		want   int
		params map[string]string
	}{
		{"/users/me", 0, map[string]string{"id": "me"}},
		{"/users/42", 0, map[string]string{"id": "42"}},
		{"/files", 2, map[string]string{"path": ""}},
		{"/files/", 2, map[string]string{"path": ""}},
		{"/files/x/y/z", 2, map[string]string{"path": "x/y/z"}},
		{"/files/x//y", 2, map[string]string{"path": "x//y"}},
		{"/a/b/c", 4, nil},
		{"/a/q/d", 5, map[string]string{"x": "q"}},
		{"/", 6, nil},
		{"/nope", -1, nil},
		{"/users", -1, nil},
		{"/users/1/2", -1, nil},
	}
	for _, c := range cases {
		rt, params, _ := r.lookup(&HTTPRequest{Method: GET, Path: c.path})
		switch {
		case c.want < 0 && rt != nil:
			t.Errorf("%s: expected no route, got %s", c.path, patternOf(rt))
		case c.want >= 0 && rt != r.routes[c.want]:
			t.Errorf("%s: got %s, want %s", c.path, patternOf(rt), patternOf(r.routes[c.want]))
		}
		if len(params) != len(c.params) || (len(c.params) > 0 && !reflect.DeepEqual(params, c.params)) {
			t.Errorf("%s: params %v, want %v", c.path, params, c.params)
		}
	}
}
