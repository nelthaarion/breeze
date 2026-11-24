package dashboard

// pages_test.go — a dashboard page is four things that have to agree: a view template, a route, a
// navigation link and an entry in the script's page registry. The Database, Queue and Scheduler pages
// had the template (and, for two of them, code in the script) but no route, no link and no registry
// entry, so three documented features were unreachable and nothing noticed.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nelthaarion/breeze/v2"
)

func TestEveryViewIsRoutedLinkedAndRegistered(t *testing.T) {
	views, err := filepath.Glob(filepath.Join("templates", "views", "*.html"))
	if err != nil || len(views) == 0 {
		t.Fatalf("no views found: %v", err)
	}
	nav, err := os.ReadFile(filepath.Join("templates", "components", "nav.html"))
	if err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile(filepath.Join("templates", "public", "dashboard.js"))
	if err != nil {
		t.Fatal(err)
	}
	router := breeze.NewRouter()
	cfg := DefaultConfig()
	cfg.DisableAuth = true
	cfg.FleetAggregatorURL = "http://127.0.0.1:1" // the Fleet page is only routed when an aggregator is configured
	c := Install(nil, router, cfg)
	defer c.Close()

	for _, v := range views {
		page := strings.TrimSuffix(filepath.Base(v), ".html")
		switch page {
		case "layout", "login", "login_layout":
			continue
		}
		t.Run(page, func(t *testing.T) {
			path := "/dashboard/" + page
			if page == "overview" {
				path = "/dashboard"
			}
			req := &breeze.HTTPRequest{Method: breeze.GET, Path: path, Header: map[string]string{}}
			if h, _, _ := router.Find(req); h == nil {
				t.Errorf("view %s.html has no route %s", page, path)
			}
			if !strings.Contains(string(nav), `data-nav="`+page+`"`) {
				t.Errorf("view %s.html is not linked from the navigation", page)
			}
			if !strings.Contains(string(script), "\n  "+page+": {title:") {
				t.Errorf("view %s.html has no entry in the script's page registry", page)
			}
		})
	}
}
