// start.go — `breeze start <target>`.
//
// # Why the MCP server is reachable from this binary
//
// An agent working on a Breeze project already has the `breeze` binary: it is what
// scaffolds, generates and migrates. Requiring it to also locate a second
// executable before it can talk MCP is a discovery problem with no upside — and
// the failure mode is bad, because "breeze-mcp: not found" looks like MCP is
// unsupported rather than like a PATH issue.
//
// So this is a second door into the same room. Every flag, default and diagnostic
// comes from internal/mcpcmd, which is also all cmd/breeze-mcp contains. There is
// no second implementation to drift: the loopback default and the mandatory token
// are decided in one place, and TestStartMCPServerMatchesStandalone asserts the
// two doors open onto the same server.
//
// cmd/breeze-mcp still exists, and should: an MCP client configuration expects a
// single-purpose executable, and a container image that serves only a control
// plane has no reason to carry the generator CLI's argv surface.

package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/nelthaarion/breeze/v2/internal/mcpcmd"
)

// startTargets are the things `breeze start` can start, for the error message.
var startTargets = []string{"mcp-server"}

// runStart dispatches `breeze start <target>`.
//
// Its writers are parameters rather than os.Stdout/os.Stderr because the stdio
// MCP transport uses them as the protocol stream: a test has to be able to read
// what a real client would, and nothing may be written to stdout that is not a
// JSON-RPC message.
func runStart(args []string, in io.Reader, out, errOut io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("breeze start needs something to start: %s", strings.Join(startTargets, ", "))
	}

	target, rest := args[0], args[1:]
	switch target {
	case "mcp-server":
		return startMCPServer(rest, in, out, errOut)
	default:
		return fmt.Errorf("cannot start %q; known targets: %s", target, strings.Join(startTargets, ", "))
	}
}

// startMCPServer serves MCP, exactly as cmd/breeze-mcp does.
//
// The command name passed through is what appears in usage and diagnostics, so a
// user who typed `breeze start mcp-server --help` is not answered with usage for a
// binary they did not invoke.
func startMCPServer(args []string, in io.Reader, out, errOut io.Writer) error {
	opts, err := mcpcmd.ParseFlags("breeze start mcp-server", args, errOut)
	if err != nil {
		if err == mcpcmd.ErrHelp {
			return nil
		}
		return err
	}
	return mcpcmd.Serve("breeze start mcp-server", startVersion(), opts, in, out, errOut)
}

// startVersion is the version string reported in the MCP handshake.
//
// cmd/breeze-mcp takes this from an -ldflags variable; this binary already knows
// its own module version through the generator, and using it means a client can
// tell which release it is talking to without the two commands needing to agree
// on a build flag.
func startVersion() string {
	if v := os.Getenv("BREEZE_MCP_VERSION"); v != "" {
		return v
	}
	return mcpcmd.ModuleVersion()
}
