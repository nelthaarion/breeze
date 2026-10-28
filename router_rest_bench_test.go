package breeze

import (
	"fmt"
	"testing"
)

func trieNoop(*Context) error { return nil }

// A GitHub-API-shaped table: the shape on which the ordered scan degrades.
func trieRESTTable() (*Router, []string) {
	r := NewRouter()
	res := []string{"users", "orgs", "teams", "repos", "issues", "pulls", "gists", "hooks", "keys", "labels", "milestones",
		"releases", "events", "notifications", "projects", "boards", "cards", "columns", "assets", "comments", "reviews",
		"commits", "branches", "tags", "forks", "stars", "watchers", "invites", "billing", "audit"}
	for _, n := range res {
		r.Handle(GET, "/api/v1/"+n, trieNoop)
		r.Handle(POST, "/api/v1/"+n, trieNoop)
		r.Handle(GET, "/api/v1/"+n+"/:a", trieNoop)
		r.Handle(PUT, "/api/v1/"+n+"/:a", trieNoop)
		r.Handle(DELETE, "/api/v1/"+n+"/:a", trieNoop)
		r.Handle(GET, "/api/v1/"+n+"/:a/sub", trieNoop)
		r.Handle(GET, "/api/v1/"+n+"/:a/sub/:b", trieNoop)
		r.Handle(POST, "/api/v1/"+n+"/:a/sub", trieNoop)
	}
	for _, p := range []string{"/health", "/version", "/metrics", "/api/v1/ping"} {
		r.Handle(GET, p, trieNoop)
	}
	return r, []string{"/api/v1/users/17", "/api/v1/orgs/9/sub", "/api/v1/teams/3/sub/4", "/api/v1/gists/5",
		"/api/v1/commits/8/sub/2", "/api/v1/billing/1", "/api/v1/audit/6/sub/1", "/health", "/api/v1/ping", "/api/v1/stars/12/sub"}
}

func BenchmarkRouterLookupREST244(b *testing.B) {
	r, paths := trieRESTTable()
	req := &HTTPRequest{Method: GET}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req.Path = paths[i%len(paths)]
		rt, params, _ := r.lookup(req)
		if rt == nil {
			b.Fatal("no route for ", req.Path)
		}
		if params != nil {
			releaseParams(params)
		}
	}
}

// Lookup cost as the table grows: the ordered scan is O(routes), the trie O(path
// depth). Paths are the last-registered resource (worst case for a scan), one in
// the middle, and the first.
func BenchmarkRouterLookupScaling(b *testing.B) {
	for _, n := range []int{25, 100, 250, 1000} {
		b.Run(fmt.Sprintf("resources=%d", n), func(b *testing.B) {
			r := NewRouter()
			for i := 0; i < n; i++ {
				base := fmt.Sprintf("/api/v1/res%d", i)
				r.Handle(GET, base, trieNoop)
				r.Handle(GET, base+"/:id", trieNoop)
				r.Handle(GET, base+"/:id/sub", trieNoop)
				r.Handle(GET, base+"/:id/sub/:n", trieNoop)
			}
			paths := []string{fmt.Sprintf("/api/v1/res%d/42/sub/7", n-1), fmt.Sprintf("/api/v1/res%d/42", n/2), "/api/v1/res0/1"}
			req := &HTTPRequest{Method: GET}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				req.Path = paths[i%3]
				rt, params, _ := r.lookup(req)
				if rt == nil {
					b.Fatal("no route")
				}
				if params != nil {
					releaseParams(params)
				}
			}
		})
	}
}
