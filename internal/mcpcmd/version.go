package mcpcmd

// version.go — what to report in the MCP handshake.

import (
	"runtime/debug"

	"github.com/nelthaarion/breeze/v2/internal/mcp"
)

// FeaturesEndpoint is the path the capability-report convenience is served on.
//
// Re-exported so the startup banner can name a real URL without either command
// hardcoding a path that internal/mcp owns.
const FeaturesEndpoint = mcp.FeaturesPath

// featuresEndpoint is the unexported spelling used in this file's format strings.
const featuresEndpoint = FeaturesEndpoint

// ModuleVersion reports the breeze module version this binary was built from, or
// "(devel)" when there is not a resolvable one.
//
// It exists so `breeze start mcp-server` can report a real version in the MCP
// handshake without an -ldflags variable of its own. cmd/breeze-mcp keeps its
// build-flag version because a single-purpose binary is often built and stamped
// deliberately; this is the answer for the CLI, which is usually `go install`ed.
func ModuleVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "(devel)"
	}
	// Built from inside the breeze module itself, breeze is Main rather than a
	// dependency — which is the case for a repository checkout.
	if info.Main.Path == breezeModulePath && resolved(info.Main.Version) {
		return info.Main.Version
	}
	for _, dep := range info.Deps {
		if dep.Path == breezeModulePath && resolved(dep.Version) {
			return dep.Version
		}
	}
	return "(devel)"
}

// breezeModulePath is the framework's import path.
const breezeModulePath = "github.com/nelthaarion/breeze/v2"

// resolved reports whether a module version is a real one rather than the
// placeholder a local build produces.
func resolved(v string) bool {
	return v != "" && v != "(devel)" && v != "devel"
}
