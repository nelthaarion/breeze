package breeze

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// extractRuntimeJS returns the JavaScript inside the <script> wrapper that
// breezeRuntime() emits, ready to be evaluated on its own.
func extractRuntimeJS(t *testing.T) string {
	t.Helper()

	full := breezeRuntime()
	const openTag = `<script id="__breeze_spa__">`
	const closeTag = `</script>`

	i := strings.Index(full, openTag)
	if i < 0 {
		t.Fatalf("runtime is missing its opening script tag")
	}
	js := full[i+len(openTag):]

	j := strings.LastIndex(js, closeTag)
	if j < 0 {
		t.Fatalf("runtime is missing its closing script tag")
	}
	return js[:j]
}

// TestSPARuntimeBehaviour runs the runtime under Node against a minimal DOM
// and asserts on what it actually does: which requests it issues, when it
// notifies watchers, and whether cached fragments are reused. The runtime is
// browser code, so this is the only way to test it as code rather than as
// text. Skipped when Node is unavailable.
func TestSPARuntimeBehaviour(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping SPA runtime behaviour tests")
	}

	harness := filepath.Join("testdata", "spa_runtime_harness.js")
	if _, err := os.Stat(harness); err != nil {
		t.Fatalf("harness missing: %v", err)
	}

	dir := t.TempDir()
	runtimePath := filepath.Join(dir, "breeze-runtime.js")
	if err := os.WriteFile(runtimePath, []byte(extractRuntimeJS(t)), 0o644); err != nil {
		t.Fatalf("write runtime: %v", err)
	}

	out, err := exec.Command(node, harness, runtimePath).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "PASS") {
		t.Fatalf("SPA runtime harness failed:\n%s", out)
	}
}

// TestSPARuntimePatching drives the DOM patcher and the component lifecycle
// against a real (if minimal) DOM implemented in testdata/spa_patch_harness.js.
// These properties are about node identity across a render — a surviving
// input keeping focus and cursor, a scroll offset surviving, onMount firing
// once on insert and not on update — and none of them are observable through
// the stub DOM the behaviour harness uses, nor assertable as source text.
// Skipped when Node is unavailable.
func TestSPARuntimePatching(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping SPA patching tests")
	}

	harness := filepath.Join("testdata", "spa_patch_harness.js")
	if _, err := os.Stat(harness); err != nil {
		t.Fatalf("harness missing: %v", err)
	}

	dir := t.TempDir()
	runtimePath := filepath.Join(dir, "breeze-runtime.js")
	if err := os.WriteFile(runtimePath, []byte(extractRuntimeJS(t)), 0o644); err != nil {
		t.Fatalf("write runtime: %v", err)
	}

	out, err := exec.Command(node, harness, runtimePath).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "PASS") {
		t.Fatalf("SPA patching harness failed:\n%s", out)
	}
}

// TestSPARuntimeSyntax catches a malformed runtime early: a syntax error in
// the embedded script would otherwise only surface as a blank page in a
// browser, since Go never parses this string.
func TestSPARuntimeSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping SPA runtime syntax check")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.js")
	if err := os.WriteFile(path, []byte(extractRuntimeJS(t)), 0o644); err != nil {
		t.Fatalf("write runtime: %v", err)
	}

	if out, err := exec.Command(node, "--check", path).CombinedOutput(); err != nil {
		t.Fatalf("runtime has a syntax error:\n%s", out)
	}
}

// TestSPARuntimeInvariants pins the specific defects this code has regressed
// on before. Each assertion is a property of the source that a future edit
// could silently undo, and that the behavioural harness cannot observe
// (the form paths need a real DOM to exercise).
//
// These read spa.js directly rather than going through breezeRuntime(), which
// serves the minified bundle outside DevMode: esbuild renames every local and
// collapses the whitespace, so none of these snippets survive there as text.
// spa.js is the file a regressing edit would touch, and
// TestSPAMinifiedMatchesSource is what proves the shipped bundle still came
// from it — so asserting on the source loses no coverage.
func TestSPARuntimeInvariants(t *testing.T) {
	js := spaJS

	mustContain := []struct{ what, snippet string }{
		{
			"form fallback goes through the prototype, so a control named \"submit\" cannot shadow it",
			"HTMLFormElement.prototype.submit.call(form)",
		},
		{
			"non-GET submits post to the action's full path including its query string",
			"var actionPath = actionUrl.pathname + actionUrl.search;",
		},
		{"the submitted request uses that path", "fetch(actionPath, {"},
		{
			"GET submissions preserve the action fragment",
			"var url = actionUrl.pathname + (qs ? '?' + qs : '') + actionUrl.hash;",
		},
		{"responses are cached only when the server allows it", "if (_isCacheable(res)) _cacheRoute("},
		{"an identity change clears the cache", "function _syncAuthEpoch(res)"},
		{"navigation applies the cache policy", "_syncAuthEpoch(res);"},
		{"proxies are reused per underlying object", "var _proxyCache = new WeakMap();"},
		{"writes that change nothing do not schedule a render", "if (ok && (!had || !Object.is(prev, val))) onChange();"},
		{"deletes of absent properties do not schedule a render", "if (ok && had) onChange();"},
		{"binding passes share one server render", "var _renderBatch = null;"},
		{"click delegation checks the target is an element", "if (!e.target || e.target.nodeType !== 1) return;"},
		{"breeze.on tolerates a non-element target", "if (node && node.nodeType !== 1) node = node.parentElement;"},
		{"swap patches the DOM instead of rebuilding it", "_patch(el, html);"},
		{"the patcher can be turned off if it ever regresses", "if (window.__BREEZE_NO_PATCH__ === true) {"},
		{"attribute removal is limited to what a render declared", "var _ownedAttrs = new WeakMap();"},
		{"static subtrees are treated as incompatible, not just skipped", "return _isStatic(a) === _isStatic(b);"},
		{"mounts run where nodes are really inserted", "_runMounts(newNode);"},
		{"destroys run where nodes are really removed", "_runDestroys(leftover[i]);"},
	}

	for _, c := range mustContain {
		if !strings.Contains(js, c.snippet) {
			t.Errorf("%s\n  missing: %s", c.what, c.snippet)
		}
	}

	// The bare call is what a shadowing control breaks; it must be gone.
	if strings.Contains(js, "\n      form.submit();") {
		t.Error("form.submit() is called directly again; a field named \"submit\" would shadow it")
	}

	// breezeRuntime() wraps this bundle in a <script> tag, so a closing tag
	// anywhere inside it would end the script early and dump the remainder
	// into the page as text.
	if strings.Contains(js, "</script") {
		t.Error("runtime contains a </script> sequence, which would close the tag breezeRuntime wraps it in")
	}
}
