// Command breeze-mcp exposes the Breeze generator and project introspection
// tools over the Model Context Protocol.
//
// It speaks newline-delimited JSON-RPC 2.0 on stdin/stdout. Stdout is protocol
// only; diagnostics go to stderr. That separation is part of the protocol, not a
// style preference: one human-readable log line on stdout is one malformed MCP
// message to the peer.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/nelthaarion/breeze/internal/mcp"
	"github.com/nelthaarion/breeze/rpc"
)

// version can be set by a release build:
//
//	go build -ldflags "-X main.version=v1.7.0" ./cmd/breeze-mcp
//
// Keeping it here rather than parsing `breeze version` means the handshake is
// pure data and cannot accidentally write a banner into the protocol stream.
var version = "(devel)"

func main() {
	if err := run(os.Stdin, os.Stdout); err != nil {
		// stderr is the only safe diagnostic channel for a stdio MCP server.
		fmt.Fprintln(os.Stderr, "breeze-mcp:", err)
		os.Exit(1)
	}
}

// run owns the transport loop. Its parameters make the complete executable
// testable without replacing process-global stdin/stdout or spawning a child.
func run(in io.Reader, out io.Writer) error {
	server := mcp.NewServer(version)
	transport := rpc.NewStdioServer(server.RPCServer(), in, out)
	return transport.Serve()
}
