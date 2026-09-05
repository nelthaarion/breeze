package mcpcmd

// guide.go — what a human sees when they run this by hand.
//
// # The problem
//
// `breeze-mcp --mode generator` printed nothing at all and blocked forever. That is
// correct behaviour for the transport — stdio waits for a client — and completely
// useless to a person: zero bytes on both streams is indistinguishable from a hang, a
// deadlock, or a binary that failed silently. The commonest first contact with this
// command was therefore a command that appeared broken.
//
// # Why a terminal check rather than a flag
//
// Two callers run this, and they want opposite things:
//
//   - An editor launches it as a subprocess with pipes. It wants silence: stdout is the
//     protocol stream, and anything written there is a malformed MCP message to the peer.
//   - A person runs it in a shell to see whether it works. They want to be told what it
//     is, that the silence is expected, and what to do next.
//
// A flag would have to be discovered before it could help, which is the wrong shape for
// the problem — the person who needs this is the person who does not yet know the flags.
// Whether stdin is a character device separates the two cases exactly, and it is a
// property of how the process was started rather than something either caller has to
// declare.
//
// # Where this writes, and where it must never write
//
// stderr, always. Not because stderr is tidier but because stdout is the wire: a stdio
// MCP peer parses every byte of it as JSON-RPC, so one guide line there would corrupt the
// session it was trying to explain. The guide is also skipped entirely for a piped stdin,
// so an editor's stderr panel does not fill with text nobody asked for.
//
// Nothing here prints a secret, because stdio has none: the process boundary is the trust
// boundary on that transport, and there is no token to leak. The network transport's
// banner, which does handle a token, is announce() in mcpcmd.go.

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/nelthaarion/breeze/internal/mcp"
)

// interactiveStdin reports whether stdin is a terminal.
//
// The type assertion is the first gate: ServeStdio takes an io.Reader so that tests can
// hand it a strings.Reader, and anything that is not an *os.File cannot be a terminal.
// That makes every existing test take the quiet path without having to know this function
// exists.
//
// The check itself is os.Stat plus ModeCharDevice rather than golang.org/x/term. It needs
// no new dependency, and it is the same test x/term performs on Windows and Unix alike for
// this purpose. A Stat error means "cannot tell", which resolves to not-a-terminal — the
// safe direction, since the cost of guessing wrong that way is a missing guide rather than
// text in somebody's protocol stream.
func interactiveStdin(in io.Reader) bool {
	f, ok := in.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// printStdioGuide explains a stdio server to whoever started it by hand.
func printStdioGuide(w io.Writer, name string, opts Options) {
	tools := mcp.ModeToolNames(opts.Mode)

	fmt.Fprintf(w, "%s: MCP server ready on stdin/stdout. This is not an interactive command.\n\n", name)
	fmt.Fprint(w, "  It is now waiting for JSON-RPC 2.0 messages on stdin and will answer on stdout.\n")
	fmt.Fprint(w, "  Nothing else will be printed until a client speaks to it — the silence is the\n")
	fmt.Fprint(w, "  server working, not a hang. Ctrl+C to stop.\n\n")

	fmt.Fprintf(w, "  mode        %s (%s)\n", opts.Mode, modeSummary(opts.Mode))
	fmt.Fprint(w, "  transport   stdio — no port, no token; the process boundary is the trust boundary\n")
	fmt.Fprintf(w, "  tools       %d\n", len(tools))
	if opts.Scope.IsScoped() {
		fmt.Fprintf(w, "  scope       %s\n", strings.Join(scopeNames(opts.Scope), ", "))
	} else {
		fmt.Fprint(w, "  scope       all capabilities (unscoped)\n")
	}
	if roots := mcp.WorkspaceRoots(); len(roots) > 0 {
		fmt.Fprintf(w, "  workspace   %s\n", strings.Join(roots, ", "))
	} else {
		fmt.Fprint(w, "  workspace   UNCONFINED (--allow-any-path): tools may read, write and run\n")
		fmt.Fprint(w, "              \"go test\" anywhere on this host\n")
	}

	fmt.Fprint(w, "\n  An editor normally starts this for you. Point its MCP config at:\n\n")
	fmt.Fprintf(w, "    {\"mcpServers\": {\"breeze\": {\"command\": %q, \"args\": [\"--mode\", %q]}}}\n", name, opts.Mode)

	fmt.Fprint(w, "\n  To drive it by hand, pipe a message in:\n\n")
	fmt.Fprintf(w, "    echo '{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/list\"}' | %s --mode %s\n", name, opts.Mode)

	fmt.Fprint(w, "\n  For a control port with a bearer token instead, which is what a remote or\n")
	fmt.Fprint(w, "  containerised agent needs:\n\n")
	fmt.Fprintf(w, "    %s --mode %s --port 2000\n", name, opts.Mode)
	fmt.Fprintf(w, "\n  %s --log     one stderr line per call\n", name)
	fmt.Fprintf(w, "  %s -h        every flag\n\n", name)
}

// printNetworkGuide explains what to do with the token the banner just printed.
//
// # Why this is separate from announce
//
// announce states facts about a running server and is written unconditionally, because a
// container log wants those facts. This is instruction, and instruction in a container log
// is noise — so it is terminal-gated like the stdio guide.
//
// # Why it never prints the token
//
// The banner has already printed it exactly once, and
// TestBannerPrintsAGeneratedTokenExactlyOnce enforces that count. A second copy in a usage
// example would double the chance of a secret surviving a truncated log for no gain, so
// every example below reads $BREEZE_MCP_TOKEN instead. That is also the form somebody
// should be using: a token on a command line is visible in `ps` and in `docker inspect`.
func printNetworkGuide(w io.Writer, name string, opts Options, server *mcp.NetworkServer) {
	endpoint := fmt.Sprintf("http://%s%s", server.Addr(), server.Endpoint())

	fmt.Fprint(w, "\n  Using the token\n\n")
	if strings.TrimSpace(opts.Token) == "" {
		// Generated. The one line above this is the only place it appears, so the
		// instruction is to capture it rather than to look it up later.
		fmt.Fprint(w, "    The token above is shown once and is not stored. Copy it now, or restart\n")
		fmt.Fprintf(w, "    with %s set to a value you control:\n\n", TokenEnv)
	} else {
		fmt.Fprintf(w, "    The token came from --token or %s, so it is not reprinted here.\n\n", TokenEnv)
	}
	fmt.Fprintf(w, "      export %s=<token>          # on Windows: $env:%s=\"<token>\"\n\n", TokenEnv, TokenEnv)

	fmt.Fprint(w, "    Check it works — this needs no MCP session and answers what this server can do:\n\n")
	fmt.Fprintf(w, "      curl -H \"Authorization: Bearer $%s\" %s%s\n\n",
		TokenEnv, strings.TrimSuffix(endpoint, server.Endpoint()), featuresEndpoint)

	fmt.Fprint(w, "    Point an MCP client at it:\n\n")
	fmt.Fprintf(w, "      {\"mcpServers\": {\"breeze\": {\"url\": %q,\n", endpoint)
	fmt.Fprint(w, "        \"headers\": {\"Authorization\": \"Bearer <token>\"}}}}\n\n")

	fmt.Fprint(w, "    Every request needs that header, including the handshake. Without it the\n")
	fmt.Fprint(w, "    endpoint answers 401 and no tool is reachable.\n")

	if opts.Host == mcp.DefaultNetworkHost {
		fmt.Fprint(w, "\n    Bound to loopback, so only this machine can reach it. --host widens that,\n")
		fmt.Fprint(w, "    and then the token is the only guard.\n")
	}
	fmt.Fprintln(w)
}
