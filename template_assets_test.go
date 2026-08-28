package breeze

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The minified client bundles are committed to the repo and embedded into the
// binary, which means they are build output living in source control: the one
// failure mode that matters is drift — someone edits spa.js, ships, and users
// keep receiving the old runtime because nobody re-ran `go generate`. That bug
// is invisible locally (dev mode serves the readable source) and silent in CI
// (both files compile fine). These tests make it loud.

// esbuildArgs mirrors the //go:generate directive in gen_assets.go exactly.
// Keep the two in sync; TestSPAMinifiedMatchesSource is what enforces that the
// committed bundle came from these flags.
var esbuildArgs = []string{
	"--yes", "esbuild@0.28.2", "spa.js",
	"--minify", "--target=es2017", "--legal-comments=none",
}

// TestSPAMinifiedMatchesSource re-runs the generator into a temp file and
// asserts the committed spa.min.js is byte-identical. A failure means spa.js
// changed without `go generate ./...` being run — the drift bug above.
//
// Skipped without Node, matching the existing SPA harnesses: Node is required
// to regenerate or verify assets, never to build or run Breeze.
func TestSPAMinifiedMatchesSource(t *testing.T) {
	npx, err := exec.LookPath("npx")
	if err != nil {
		t.Skip("npx not installed; skipping minified-bundle drift check")
	}

	committed, err := os.ReadFile("spa.min.js")
	if err != nil {
		t.Fatalf("spa.min.js missing — run `go generate ./...`: %v", err)
	}

	out := t.TempDir() + "/spa.regen.js"
	args := append(append([]string{}, esbuildArgs...), "--outfile="+out)
	if b, err := exec.Command(npx, args...).CombinedOutput(); err != nil {
		t.Skipf("esbuild unavailable (offline?), skipping drift check: %v\n%s", err, b)
	}

	regen, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("esbuild produced no output: %v", err)
	}

	if string(committed) != string(regen) {
		t.Errorf("spa.min.js is stale: regenerating from spa.js produced different bytes "+
			"(committed %d bytes, regenerated %d bytes).\n"+
			"spa.js was edited without re-running `go generate ./...`, so browsers would "+
			"receive the OLD runtime while tests exercise the new source.",
			len(committed), len(regen))
	}
}

// TestSPAMinifiedPreservesPublicAPI guards the specific way minification can
// break this runtime: it resolves developer-supplied functions by *name* off
// window (data-spa-mount="myFn") and exposes window.breeze/window.Breeze. A
// minifier that renamed those, or that was ever run with --mangle-props, would
// produce a bundle that loads without error and then quietly does nothing.
func TestSPAMinifiedPreservesPublicAPI(t *testing.T) {
	min, err := os.ReadFile("spa.min.js")
	if err != nil {
		t.Fatalf("spa.min.js missing — run `go generate ./...`: %v", err)
	}
	src := string(min)

	// Every name reachable from HTML or user code, and therefore not
	// renameable. Sourced from the window assignments and attribute lookups
	// in spa.js.
	for _, name := range []string{
		"window.breeze", "window.Breeze",
		"data-spa-mount", "data-spa-run", "data-spa-bind", "data-key",
		// The ids the runtime *reads*. Note __breeze_spa__ is deliberately
		// absent: that is the id of the <script> wrapper Go emits around this
		// bundle, not a string inside it, and is asserted separately in
		// TestBreezeRuntimeServesMinifiedByDefault.
		"__breeze_data__", "__breeze_tmpl__", "__breeze_i18n__",
		"breeze-app", "breeze-loading",
		"X-Breeze-Partial",
		"onBeforeNavigate", "onAfterNavigate", "onBeforeSubmit", "onAfterSubmit",
		"setData", "watch", "poll", "swap", "render",
	} {
		if !strings.Contains(src, name) {
			t.Errorf("minified bundle lost %q — it is referenced by name from HTML "+
				"attributes or user code and must never be renamed "+
				"(was --mangle-props added to the generator?)", name)
		}
	}
}

// TestSPAMinifiedIsActuallySmaller is a cheap sanity check that the committed
// bundle is minified output and not, say, a copy of the source made by hand.
func TestSPAMinifiedIsActuallySmaller(t *testing.T) {
	min, err := os.ReadFile("spa.min.js")
	if err != nil {
		t.Fatalf("spa.min.js missing — run `go generate ./...`: %v", err)
	}
	if len(min) == 0 {
		t.Fatal("spa.min.js is empty; breezeRuntime would fall back to the source bundle")
	}
	if len(min) >= len(spaJS) {
		t.Errorf("spa.min.js (%d bytes) is not smaller than spa.js (%d bytes); "+
			"is it really minifier output?", len(min), len(spaJS))
	}
}

// TestBreezeRuntimeServesMinifiedByDefault pins the production default and the
// dev-mode override, since serving 74KB of comments to every visitor is the
// regression this whole change exists to prevent.
func TestBreezeRuntimeServesMinifiedByDefault(t *testing.T) {
	prev := useReadableRuntime.Load()
	t.Cleanup(func() { useReadableRuntime.Store(prev) })

	useReadableRuntime.Store(false)
	got := breezeRuntime()
	if !strings.Contains(got, spaJSMin) {
		t.Error("production runtime does not contain the minified bundle")
	}
	if strings.Contains(got, "// SPA navigation") {
		t.Error("production runtime contains source comments; minification not applied")
	}

	useReadableRuntime.Store(true)
	if dev := breezeRuntime(); !strings.Contains(dev, spaJS) {
		t.Error("DevMode runtime does not serve the readable source bundle")
	}

	// Both variants must carry the id the client and the injection tests
	// look for.
	if !strings.Contains(got, `id="__breeze_spa__"`) {
		t.Error("runtime script tag lost its __breeze_spa__ id")
	}
}

// TestDevModeSelectsReadableRuntime asserts the wiring from public config to
// the runtime switch, not just the switch itself.
func TestDevModeSelectsReadableRuntime(t *testing.T) {
	prev := useReadableRuntime.Load()
	t.Cleanup(func() { useReadableRuntime.Store(prev) })

	useReadableRuntime.Store(false)
	NewTemplateEngine(TemplateConfig{DevMode: true})
	if !useReadableRuntime.Load() {
		t.Error("TemplateConfig{DevMode: true} did not select the readable runtime")
	}
}
