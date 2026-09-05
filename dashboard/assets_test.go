package dashboard

// assets_test.go — the dashboard links the minified bundles by default.
//
// The regression these guard is silent: linking dashboard.js instead of
// dashboard.min.js produces a dashboard that works perfectly and costs every
// visitor ~55KB extra. Nothing fails, nothing logs, and the only symptom is a
// number in a network panel nobody is looking at.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAssetNamesDefaultToMinified pins the default and the DevMode override.
func TestAssetNamesDefaultToMinified(t *testing.T) {
	style, script := assetNames(false)
	if style != "dashboard.min.css" {
		t.Errorf("production stylesheet = %q, want dashboard.min.css", style)
	}
	if script != "dashboard.min.js" {
		t.Errorf("production script = %q, want dashboard.min.js", script)
	}

	devStyle, devScript := assetNames(true)
	if devStyle != "dashboard.css" {
		t.Errorf("DevMode stylesheet = %q, want dashboard.css", devStyle)
	}
	if devScript != "dashboard.js" {
		t.Errorf("DevMode script = %q, want dashboard.js", devScript)
	}
}

// TestViewDataCarriesAssetNames checks the wiring rather than the helper: a
// correct assetNames that viewData never calls would leave the template
// referencing an empty filename, which is a 404 for the whole stylesheet.
func TestViewDataCarriesAssetNames(t *testing.T) {
	prod := &Collector{cfg: Config{BasePath: "/dashboard"}}
	data := prod.viewData(nil, "overview")

	if got := data["StyleFile"]; got != "dashboard.min.css" {
		t.Errorf("StyleFile = %v, want dashboard.min.css", got)
	}
	if got := data["ScriptFile"]; got != "dashboard.min.js" {
		t.Errorf("ScriptFile = %v, want dashboard.min.js", got)
	}

	dev := &Collector{cfg: Config{BasePath: "/dashboard", DevMode: true}}
	devData := dev.viewData(nil, "overview")
	if got := devData["ScriptFile"]; got != "dashboard.js" {
		t.Errorf("DevMode ScriptFile = %v, want dashboard.js", got)
	}
}

// TestLayoutReferencesAssetsThroughData is what catches a hardcoded filename
// coming back. The template is the thing that actually decides what a browser
// fetches, so asserting on Go alone would miss an edit made only there.
func TestLayoutReferencesAssetsThroughData(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("templates", "views", "layout.html"))
	if err != nil {
		t.Fatalf("layout.html unreadable: %v", err)
	}
	layout := string(raw)

	for _, want := range []string{
		`href="{{.Data.AssetsPath}}/{{.Data.StyleFile}}"`,
		`src="{{.Data.AssetsPath}}/{{.Data.ScriptFile}}"`,
	} {
		if !strings.Contains(layout, want) {
			t.Errorf("layout.html does not contain %s — is a filename hardcoded again?", want)
		}
	}

	// The specific regression: a literal unminified name in a link or script tag.
	for _, bad := range []string{`/dashboard.css"`, `/dashboard.js"`} {
		if strings.Contains(layout, bad) {
			t.Errorf("layout.html hardcodes %s; every visitor pays for the unminified bundle", bad)
		}
	}
}

// TestMinifiedBundlesExistAndAreSmaller guards the other half: a layout pointing
// at a file that is missing or is a hand-made copy of the source.
func TestMinifiedBundlesExistAndAreSmaller(t *testing.T) {
	pairs := []struct{ min, src string }{
		{"dashboard.min.css", "dashboard.css"},
		{"dashboard.min.js", "dashboard.js"},
	}

	for _, p := range pairs {
		minPath := filepath.Join("templates", "public", p.min)
		srcPath := filepath.Join("templates", "public", p.src)

		minStat, err := os.Stat(minPath)
		if err != nil {
			t.Errorf("%s missing — run `go generate ./...`: %v", p.min, err)
			continue
		}
		srcStat, err := os.Stat(srcPath)
		if err != nil {
			t.Errorf("%s missing: %v", p.src, err)
			continue
		}

		if minStat.Size() == 0 {
			t.Errorf("%s is empty; the dashboard would load no %s at all", p.min, p.src)
		}
		if minStat.Size() >= srcStat.Size() {
			t.Errorf(
				"%s (%d bytes) is not smaller than %s (%d bytes); is it really minifier output?",
				p.min,
				minStat.Size(),
				p.src,
				srcStat.Size(),
			)
		}
	}
}
