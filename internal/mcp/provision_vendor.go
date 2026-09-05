package mcp

// provision_vendor.go — copying Breeze's own source into a build context.
//
// See provision_image.go for why a provisioned image compiles the framework from source
// rather than fetching it: the orchestrator's working tree is routinely ahead of the
// newest published tag, and a container built from the proxy would not contain the code
// that provisioned it.
//
// What this file decides is *what* to copy. The answer is "what `go build` reads, and
// nothing else": a build context is hashed and sent to the daemon in full, so a stray
// .git directory is minutes of wasted transfer on every provision.

import (
	"io"
	"os"
	"path/filepath"
	"strings"
)

// skipVendorDir reports whether a directory in Breeze's own tree should be left out of
// the copy.
//
// The image needs the module's *source*: what `go build` reads. Everything else is
// weight in a build context that Docker has to hash and send to the daemon, and some of
// it is actively harmful — .git can be hundreds of megabytes, and a nested build output
// directory would be copied into an image that is about to produce its own.
func skipVendorDir(name string) bool {
	switch name {
	case ".git", ".github", ".idea", ".vscode", ".claude",
		"node_modules", "testdata", ".compose-bin":
		return true
	}
	return false
}

// vendorBreezeSource copies Breeze's module tree into the build context.
//
// Only .go files and the files the build genuinely needs come across: go.mod, go.sum,
// and the assets embedded with //go:embed. Filtering by extension would be simpler and
// wrong — a missing embedded template is a compile error inside the image, reported as
// "pattern ...: no matching files found", which is a confusing way to learn that a copy
// was too clever.
//
// _test.go files are skipped. They are not needed to build either binary, they pull in
// test-only dependencies that go.mod's require list may not cover from a clean cache,
// and they are the bulk of the tree by file count.
func vendorBreezeSource(sourceRoot, projectDir string) error {
	dest := filepath.Join(projectDir, vendoredBreezeDir)

	return filepath.WalkDir(sourceRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, relErr := filepath.Rel(sourceRoot, path)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return os.MkdirAll(dest, 0o755)
		}

		if entry.IsDir() {
			if skipVendorDir(entry.Name()) {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dest, rel), 0o755)
		}
		if !vendorIncludesFile(entry.Name()) {
			return nil
		}
		return copyFileTo(path, filepath.Join(dest, rel))
	})
}

// vendorIncludesFile decides whether one file is needed to build the module.
func vendorIncludesFile(name string) bool {
	switch name {
	case "go.mod", "go.sum":
		return true
	}
	if strings.HasSuffix(name, "_test.go") {
		return false
	}
	if strings.HasSuffix(name, ".go") {
		return true
	}

	// Embedded assets. These are referenced by //go:embed directives, so the compiler
	// requires them present even though nothing imports them: the dashboard's and
	// scalar's templates, stylesheets and scripts.
	switch filepath.Ext(name) {
	case ".html", ".css", ".js", ".json", ".svg", ".ico", ".txt", ".tmpl", ".map":
		return true
	}
	return false
}

// copyFileTo copies one file, creating its parent directory.
//
// The parent is created here as well as during the walk because a file can be reached
// before its directory in a filtered walk, and an os.Create into a missing directory
// fails with a path error that does not say which of the two was missing.
func copyFileTo(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// rewriteGoModForVendoredBreeze points the generated project's go.mod at the copy.
//
// A replace directive, and a require line if the generator left it out — which it does
// for a development build, precisely because it has no resolvable version to name. A
// replace without a require is not enough: the module graph needs the requirement to
// exist before it can be replaced, and `go build` reports "missing go.sum entry" rather
// than anything about the replace.
//
// The version on the require line is irrelevant once replaced — nothing fetches it — but
// it has to be syntactically valid, so 2.0.2 stands in.
func rewriteGoModForVendoredBreeze(projectDir string) error {
	path := filepath.Join(projectDir, "go.mod")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	content := string(data)
	if !strings.Contains(content, breezeModulePath+" ") {
		content = strings.TrimRight(content, "\n") +
			"\n\nrequire " + breezeModulePath + " v2.0.2\n"
	}

	content = strings.TrimRight(content, "\n") + "\n\n" +
		"// Points at the Breeze source copied into this build context by provision_service.\n" +
		"// The container therefore builds the framework from the tree that provisioned it,\n" +
		"// rather than from whatever the module proxy's newest release contains.\n" +
		"replace " + breezeModulePath + " => ./" + vendoredBreezeDir + "\n"

	return os.WriteFile(path, []byte(content), 0o644)
}
