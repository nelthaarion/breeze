package mcp

// provision_source.go — locating the Breeze module's own source tree.
//
// # Why this is a search rather than a constant
//
// A provisioned image compiles Breeze from source when source is available (see
// provision_image.go for why). "Available" is a runtime question: the orchestrator may
// be `go run`-ing from a clone, a `go build` binary invoked from anywhere, or a
// released binary on a machine with no clone at all. Each answers differently, and
// guessing wrong in either direction is bad — a wrong path breaks a build that would
// have worked from the proxy, and missing a real clone silently provisions stale code.
//
// So it looks, and it verifies what it finds. A directory only counts as the Breeze
// module if its go.mod actually declares this module path; a go.mod naming something
// else is somebody else's project that happens to sit in the search path.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// breezeSourceRoot returns the directory containing Breeze's own go.mod, or "" when
// there is none to be found.
//
// Two candidates, in order of trustworthiness:
//
//  1. The compiled-in path of this very file, via runtime.Caller. This is exact when it
//     works and needs no assumption about the working directory. It fails for a
//     -trimpath binary, or when the tree has been moved since it was built, which is
//     why it is checked rather than trusted.
//  2. The working directory and its parents. This covers `breeze-mcp` launched from
//     inside a clone, including a subdirectory of one.
//
// A released binary run outside any clone matches neither and gets "", which is the
// correct answer: there is no local source, so the proxy is the only option.
func breezeSourceRoot() string {
	if _, file, _, ok := runtime.Caller(0); ok {
		// This file is at <root>/internal/mcp/provision_source.go, so the module root
		// is two directories up. Derived by walking rather than by trimming a literal
		// suffix, so moving this file between packages cannot silently produce a path
		// that is wrong by one level.
		if root := ascendToModule(filepath.Dir(file)); root != "" {
			return root
		}
	}

	if wd, err := os.Getwd(); err == nil {
		if root := ascendToModule(wd); root != "" {
			return root
		}
	}
	return ""
}

// ascendToModule walks up from dir looking for the Breeze module's go.mod.
//
// Bounded by reaching the filesystem root, and by a hop limit as a second guard: a
// symlink loop would otherwise make this walk forever, and a provisioning tool that
// hangs is worse than one that reports no local source.
func ascendToModule(dir string) string {
	const maxHops = 64

	for hop := 0; hop < maxHops; hop++ {
		if isBreezeModuleRoot(dir) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	return ""
}

// isBreezeModuleRoot reports whether dir holds a go.mod declaring this module.
//
// The module path is checked, not merely the file's existence. Any Go project has a
// go.mod; only Breeze's declares breezeModulePath, and copying an unrelated module into
// a build context would produce a `replace` pointing at the wrong code — a failure that
// surfaces as a confusing compile error inside the image rather than as a bad path.
func isBreezeModuleRoot(dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return false
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "module") {
			continue
		}
		// "module <path>" — and only the first module line matters, since a valid
		// go.mod has exactly one.
		fields := strings.Fields(line)
		return len(fields) >= 2 && fields[1] == breezeModulePath
	}
	return false
}
