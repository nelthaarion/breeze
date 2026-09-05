package mcp

// provision_vendor_test.go — the build context carries the right source.
//
// These matter because the failure they guard against is invisible until a container
// build runs: a missing embedded asset, or a go.mod without the replace, produces a
// compile error inside Docker minutes later, attributed to the generated project rather
// than to the copy that was too clever.

import (
	"os"
	"path/filepath"
	"testing"
)

// TestBreezeSourceRootFindsThisModule — the whole vendoring path is dead code if the
// search fails, and it would fail silently: provisioning would fall back to the proxy
// and only the note would say so.
func TestBreezeSourceRootFindsThisModule(t *testing.T) {
	root := breezeSourceRoot()
	if root == "" {
		t.Fatal("the Breeze module's own source was not found from inside its own test binary")
	}
	if !isBreezeModuleRoot(root) {
		t.Errorf("%s was returned but does not hold Breeze's go.mod", root)
	}
	// A landmark that exists only in this module, so a go.mod belonging to something
	// else cannot satisfy the check above by coincidence.
	if _, err := os.Stat(
		filepath.Join(root, "internal", "mcp", "provision_vendor.go"),
	); err != nil {
		t.Errorf("%s does not look like this module: %v", root, err)
	}
}

// TestIsBreezeModuleRootChecksTheModulePath — any Go project has a go.mod. Only this
// module's declares breezeModulePath, and vendoring the wrong tree would produce a
// replace pointing at unrelated code.
func TestIsBreezeModuleRootChecksTheModulePath(t *testing.T) {
	dir := t.TempDir()
	if isBreezeModuleRoot(dir) {
		t.Error("a directory with no go.mod was accepted")
	}

	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/somebody-else\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if isBreezeModuleRoot(dir) {
		t.Error("another module's go.mod was accepted as Breeze's")
	}

	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module "+breezeModulePath+"\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !isBreezeModuleRoot(dir) {
		t.Error("Breeze's own go.mod was rejected")
	}
}

// TestVendorIncludesTheFilesTheBuildNeeds pins the filter both ways.
//
// The embedded assets are the interesting half: nothing imports them, so a
// .go-extension-only filter compiles fine here and fails inside the image with
// "pattern ...: no matching files found".
func TestVendorIncludesTheFilesTheBuildNeeds(t *testing.T) {
	included := []string{
		"go.mod", "go.sum", "breeze.go", "mcp_server.go",
		"layout.html", "dashboard.min.css", "spa.min.js", "openapi.json",
	}
	for _, name := range included {
		if !vendorIncludesFile(name) {
			t.Errorf("%s is needed to build the module but is excluded", name)
		}
	}

	excluded := []string{
		// Tests are not needed by either binary and pull in test-only dependencies.
		"breeze_test.go", "server_test.go",
		// Not read by the compiler.
		"README.md", "CHANGELOG.md", "Dockerfile", ".gitignore", "coverage.html.bak",
	}
	for _, name := range excluded {
		if vendorIncludesFile(name) {
			t.Errorf("%s is not needed to build and would only add weight", name)
		}
	}
}

// TestSkipVendorDirExcludesTheExpensiveDirectories — a build context is hashed and sent
// to the daemon whole, so .git is minutes of transfer on every single provision.
func TestSkipVendorDirExcludesTheExpensiveDirectories(t *testing.T) {
	for _, name := range []string{".git", ".github", "node_modules", "testdata", ".compose-bin"} {
		if !skipVendorDir(name) {
			t.Errorf("%s would be copied into every build context", name)
		}
	}
	// The directories the build genuinely reads must not be skipped.
	for _, name := range []string{"internal", "dashboard", "fleet", "cmd", "templates", "public"} {
		if skipVendorDir(name) {
			t.Errorf("%s holds source or embedded assets and must be copied", name)
		}
	}
}
