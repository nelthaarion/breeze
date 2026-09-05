// Command breeze-mcp exposes the Breeze generator and project introspection
// tools over the Model Context Protocol.
//
// # Two transports, one at a time
//
// With no flags it speaks newline-delimited JSON-RPC 2.0 on stdin/stdout, which
// is what it has always done and what an editor launching it as a subprocess
// expects. Stdout is protocol only; diagnostics go to stderr. That separation is
// part of the protocol, not a style preference: one human-readable log line on
// stdout is one malformed MCP message to the peer.
//
// With --port it serves the same tools over MCP Streamable HTTP instead:
//
//	breeze-mcp --port 2000
//	breeze-mcp --port 2000 --host 0.0.0.0 --token $BREEZE_MCP_TOKEN
//
// It is deliberately one *or* the other, not both at once. A process serving both
// would have two peers with two views of the same mutable workspace — the
// generators chdir and capture os.Stdout under a process-wide lock, so a tool
// call from the network would serialise against one from stdio and, worse, the
// stdout capture that turns generator output into a tool result would be
// contending with the stdio transport's own protocol stream. Whichever way that
// race resolved would be a corrupted session for one of the two peers. Running
// two processes costs nothing and shares no state to get wrong.
//
// # The address served here is a control address
//
// --port is the *control port*: what an agent talks to in order to generate,
// modify, verify or provision. It is not the port a generated application listens
// on. Those are always different numbers, and the tools that read a running
// service take its address as an argument (service_url, aggregator_url) rather
// than inferring it from this one.
//
// # Filesystem confinement is on by default
//
// The tools that take a path are confined to the working directory unless
// --workspace names other roots, and --allow-any-path removes the confinement
// entirely. This is not tidiness: breeze_verify_project and breeze_run_benchmarks
// run `go test` in the directory they are given, which compiles and executes what
// is there — so an unconfined server handed a path is being asked to run whatever
// code is at it.
//
// # One implementation, two entrypoints
//
// Every flag, default and diagnostic here comes from internal/mcpcmd, which is
// also what `breeze start mcp-server` calls. This file is argv, a version string
// and an exit code; nothing about the server is decided in it. That matters
// because the defaults are a security posture — loopback bind, mandatory token —
// and a second copy of them would be a second place for one of the two commands
// to be widened by accident.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/nelthaarion/breeze/internal/mcpcmd"
)

// version can be set by a release build:
//
//	go build -ldflags "-X main.version=v1.7.0" ./cmd/breeze-mcp
//
// Keeping it here rather than parsing `breeze version` means the handshake is
// pure data and cannot accidentally write a banner into the protocol stream.
var version = "(devel)"

// commandName is what this executable calls itself in usage and diagnostics.
const commandName = "breeze-mcp"

func main() {
	if err := main1(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		// stderr is the only safe diagnostic channel for a stdio MCP server, and
		// the conventional one for the network mode too.
		fmt.Fprintln(os.Stderr, commandName+":", err)
		os.Exit(1)
	}
}

// main1 is main with its inputs and outputs supplied, so the whole executable —
// flag parsing included — is testable without spawning a child process.
func main1(args []string, in io.Reader, out, errOut io.Writer) error {
	opts, err := mcpcmd.ParseFlags(commandName, args, errOut)
	if err != nil {
		if errors.Is(err, mcpcmd.ErrHelp) {
			return nil
		}
		return err
	}
	return mcpcmd.Serve(commandName, version, opts, in, out, errOut)
}
