package generator

// `breeze version` â€” what this binary is, and what it will pin a new project
// to.
//
// The breeze version is the interesting one: `breeze new` writes it as a
// require line, and every generator emits code against that release's APIs. A
// mismatch between the CLI and the framework a project actually builds against
// is exactly the kind of thing that produces code which does not compile, so
// the version is worth being able to read directly.

import (
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
)

func printVersion(w io.Writer) {
	version := breezeVersion()
	if version == "" {
		// A local `go build` or `go run` leaves no resolvable module version.
		version = "(devel)"
	}
	fmt.Fprintf(w, "breeze   %s\n", version)
	fmt.Fprintf(w, "go       %s\n", runtime.Version())
	fmt.Fprintf(w, "platform %s/%s\n", runtime.GOOS, runtime.GOARCH)

	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			// vcs.revision is what identifies a dev build, which is the case
			// where the module version above says nothing useful.
			if s.Key == "vcs.revision" {
				rev := s.Value
				if len(rev) > 12 {
					rev = rev[:12]
				}
				fmt.Fprintf(w, "revision %s\n", rev)
			}
		}
	}

	if version == "(devel)" {
		fmt.Fprintf(w, "\nBuilt from source, so `breeze new` omits the require line and lets\n"+
			"`go mod tidy` resolve the framework version.\n")
	}
}
