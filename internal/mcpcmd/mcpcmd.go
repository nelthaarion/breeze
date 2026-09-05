// Package mcpcmd is the one implementation of "start an MCP server from a
// command line".
//
// # Why this package exists
//
// There are two entrypoints to the same server: cmd/breeze-mcp, which is the
// single-purpose executable an MCP client configuration expects, and
// `breeze start mcp-server`, which is the same thing reached from the CLI an
// agent already has. Two entrypoints are useful; two implementations would be a
// liability. Flag names, defaults, the loopback bind, token generation, the
// stdio-or-network rule and every diagnostic line live here, once, and both
// commands are a call into this package.
//
// That is not a stylistic preference. The defaults are a security posture — a
// loopback bind and a mandatory token — and a second copy of them is a second
// place for one of the two to be widened by accident. TestEntrypointsAgree
// asserts the two commands parse identically; they can only do so because there
// is nothing to compare but the same function called twice.
package mcpcmd

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/nelthaarion/breeze/internal/mcp"
	"github.com/nelthaarion/breeze/rpc"
)

// TokenEnv is the environment variable a bearer token may be supplied through.
//
// Offered alongside --token because a token on a command line is visible in `ps`
// and in `docker inspect`, and a reproducible deployment needs to set it somehow.
// The Category H orchestrator passes a provisioned instance its token through
// this same variable, so a provisioned instance and a hand-started one are
// configured identically.
const TokenEnv = "BREEZE_MCP_TOKEN"

// ErrHelp reports that -h was handled and the process should stop successfully.
//
// It is a sentinel rather than a nil error with a flag, because every caller has
// to distinguish "the user asked for usage" from "the user made a mistake" and a
// boolean return would let one of them forget.
var ErrHelp = errors.New("mcpcmd: help requested")

// Options is the parsed command line.
//
// The fields are exported because two commands and their tests read them. The
// zero value is the stdio server, which is the default this package must never
// change by accident: Port 0 means stdio, and nothing else consults the rest.
type Options struct {
	// Mode is the server kind: "generator" or "app-runtime". Required, with no
	// default, on both entrypoints. See internal/mcp/mode.go for why neither value
	// is a safe fallback.
	Mode mcp.ServerMode

	// Port is the control port. Zero means stdio.
	Port int
	// Host is the bind address for Port.
	Host string
	// Token is the bearer token every network request must carry. Empty means one
	// is generated and printed once.
	Token string
	// Origins extends the Origin allowlist beyond loopback.
	Origins []string

	// Scope is what the token may do. The zero value is unscoped — every capability —
	// which keeps an existing command line behaving exactly as it did.
	Scope mcp.Scope

	// Workspace is the directory root(s) filesystem tools may touch. Empty means
	// the working directory, which is the safe default: an MCP client launches this
	// as a subprocess from the project it is working on.
	//
	// This is a separate concern from Scope. Scope decides which tools a token may
	// call; Workspace decides what those tools may reach. A token scoped to
	// `generation` is still a token that can write files — the question this
	// answers is *where*.
	Workspace []string

	// AllowAnyPath disables confinement entirely.
	//
	// It exists because there is one legitimate case: a disposable container whose
	// entire filesystem is the sandbox, where confining to a subdirectory adds
	// nothing. It is a flag rather than the default because the consequence of
	// getting it wrong is that `breeze_verify_project` will run `go test` in any
	// directory named — which compiles and executes whatever is there.
	AllowAnyPath bool

	// Log turns on the per-event stderr log described in logger.go.
	//
	// Off by default because both transports' output streams belong to somebody else: on
	// stdio, stdout is the protocol and stderr is whatever the launching editor does with
	// it. An operator who wants the lines asks for them.
	Log bool
}

// ParseFlags parses args for the command called name.
//
// name appears in usage text and error messages only; it changes no behaviour,
// which is the property that lets two differently-named commands share this.
func ParseFlags(name string, args []string, errOut io.Writer) (Options, error) {
	var opts Options

	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(errOut)
	mode := fs.String("mode", "",
		"REQUIRED, no default: \"generator\" to build and change a project, "+
			"\"app-runtime\" to inspect a running instance")
	fs.IntVar(&opts.Port, "port", 0,
		"serve MCP Streamable HTTP on this control port instead of stdio (0 = stdio)")
	fs.StringVar(&opts.Host, "host", mcp.DefaultNetworkHost,
		"bind address for --port; the default is loopback only")
	fs.StringVar(&opts.Token, "token", os.Getenv(TokenEnv),
		"bearer token required on every network request (default $"+TokenEnv+"; generated and printed once if unset)")
	origins := fs.String("allow-origin", "",
		"comma-separated Origin values to accept in addition to loopback, or * to disable the check")
	scope := fs.String("scope", "",
		"comma-separated capability categories this token may use (default: all). "+
			"Values: "+strings.Join(capabilityList(), ", "))
	workspace := fs.String("workspace", "",
		"comma-separated directory root(s) filesystem tools may touch "+
			"(default: the working directory)")
	anyPath := fs.Bool("allow-any-path", false,
		"REMOVES workspace confinement: filesystem tools may then read, write and run "+
			"\"go test\" anywhere on this host. Only for a disposable sandbox.")
	fs.BoolVar(&opts.Log, "log", false,
		"write one stderr line per tool call, refused call and rejected request "+
			"(argument names only, never values)")
	fs.Usage = func() { printUsage(errOut, name, fs) }

	if err := fs.Parse(args); err != nil {
		// flag has already written the reason to errOut, so the error returned
		// here is short rather than a second copy of it. -h is not a failure.
		if errors.Is(err, flag.ErrHelp) {
			return Options{}, ErrHelp
		}
		return opts, errors.New("invalid arguments")
	}
	if rest := fs.Args(); len(rest) > 0 {
		return opts, fmt.Errorf("unexpected argument %q; %s takes flags only", rest[0], name)
	}
	if opts.Port < 0 || opts.Port > 65535 {
		return opts, fmt.Errorf("--port %d is not a port number", opts.Port)
	}

	// Parsed after the flag set so that -h still prints usage rather than this
	// error: someone running `breeze-mcp -h` to find out what --mode accepts should
	// be told, not scolded for not knowing yet.
	parsed, err := mcp.ParseMode(*mode)
	if err != nil {
		return opts, err
	}
	opts.Mode = parsed

	parsedScope, err := mcp.ParseScope(*scope)
	if err != nil {
		return opts, err
	}
	opts.Scope = parsedScope

	for _, origin := range strings.Split(*origins, ",") {
		if origin = strings.TrimSpace(origin); origin != "" {
			opts.Origins = append(opts.Origins, origin)
		}
	}

	for _, root := range strings.Split(*workspace, ",") {
		if root = strings.TrimSpace(root); root != "" {
			opts.Workspace = append(opts.Workspace, root)
		}
	}
	opts.AllowAnyPath = *anyPath
	// Refused rather than resolved in one direction or the other. A caller who
	// passed both stated two incompatible intentions, and picking one would mean
	// either silently ignoring named roots or silently ignoring an explicit escape
	// hatch — and the second of those is the dangerous reading.
	if opts.AllowAnyPath && len(opts.Workspace) > 0 {
		return opts, errors.New("--allow-any-path and --workspace contradict each other: " +
			"one removes confinement, the other configures it. Pass only one")
	}
	return opts, nil
}

// printUsage writes the usage block for either command.
func printUsage(w io.Writer, name string, fs *flag.FlagSet) {
	fmt.Fprintf(w, "%s serves the Breeze toolchain over MCP.\n", name)
	fmt.Fprintf(w, "\n  %s                stdio (for an editor launching it as a subprocess)\n", name)
	fmt.Fprintf(w, "  %s --port 2000    MCP Streamable HTTP on a control port, loopback only\n", name)
	fmt.Fprintln(w, "\nFlags:")
	fs.PrintDefaults()
}

// Serve runs whichever transport the options select.
//
// It is deliberately one *or* the other, never both. A process serving both would
// have two peers with two views of the same mutable workspace: the generators
// chdir and capture os.Stdout under a process-wide lock, so a tool call from the
// network would serialise against one from stdio and, worse, the stdout capture
// that turns generator output into a tool result would contend with the stdio
// transport's own protocol stream. Whichever way that race resolved would be a
// corrupted session for one of the two peers. Two processes cost nothing and
// share no state to get wrong.
func Serve(name, version string, opts Options, in io.Reader, out, errOut io.Writer) error {
	// Confinement first, before any transport is listening. Both branches need it
	// and neither should be able to forget it, so it happens here rather than in
	// each — a tool call that arrived before the policy was installed would be
	// unconfined, and on stdio the first call can arrive immediately.
	if err := applyConfinement(opts); err != nil {
		return err
	}

	// The terminal question is answered once, here, because this is the only function that
	// holds stdin. Reaching for os.Stdin further down would make the decision untestable
	// and would be wrong for any caller that supplied its own reader.
	interactive := interactiveStdin(in)

	if opts.Port == 0 {
		// Printed after confinement is applied, so the workspace line reports what is
		// actually in force. An editor gets a pipe and therefore silence — see guide.go for
		// why that distinction is the whole design.
		if interactive {
			printStdioGuide(errOut, name, opts)
		}
		return ServeStdio(version, opts.Mode, opts.Scope, in, out, loggerFor(name, opts, errOut))
	}
	return ServeNetwork(name, version, opts, errOut, interactive)
}

// loggerFor builds the event logger, or nil when --log was not passed.
//
// One place, so both transports get the same format and the same "off by default" —
// and so nil means the same thing on both: SetLogger(nil) disables logging entirely.
func loggerFor(name string, opts Options, errOut io.Writer) mcp.Logger {
	if !opts.Log {
		return nil
	}
	return &stderrLogger{out: errOut, name: name}
}

// applyConfinement installs the workspace policy for this process.
//
// # Why the default is the working directory rather than unconfined
//
// An MCP client launches this as a subprocess from the project being worked on, so
// the working directory is already the answer in the overwhelming majority of cases
// — and it is the answer the tools already documented, since every path argument
// defaults to it. Making it the *boundary* as well as the default costs a correct
// caller nothing.
//
// The alternative default, unconfined, was the previous behaviour and is what makes
// `{"path": "/"}` a working argument to a tool that runs `go test`.
func applyConfinement(opts Options) error {
	if opts.AllowAnyPath {
		mcp.Unconfine()
		return nil
	}
	if len(opts.Workspace) > 0 {
		return mcp.ConfineToWorkspace(opts.Workspace...)
	}

	wd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("mcpcmd: cannot determine the working directory to confine to: %w. "+
			"Pass --workspace to name one explicitly", err)
	}
	return mcp.ConfineToWorkspace(wd)
}

// ServeStdio owns the stdio transport loop.
//
// Its parameters make the whole path testable without replacing process-global
// stdin and stdout or spawning a child. Stdout is protocol only: one
// human-readable line written there is one malformed MCP message to the peer.
//
// Scope applies here even though stdio has no token to scope. On this transport the
// process boundary is the trust boundary, so a scope is not a credential restriction —
// it is the operator saying what this subprocess should offer at all, which an editor
// config can legitimately want ("launch me a read-only Breeze"). Dropping it would make
// --scope a flag that parses and does nothing on the transport most people use, and
// would make the handshake's breezeCapabilities lie about being available everywhere.
//
// logger may be nil, which is logging off. It is a parameter rather than read from
// Options because this function is also the one a test drives directly, and a test that
// wants to see the events should not have to build a command line to get them.
func ServeStdio(version string, mode mcp.ServerMode, scope mcp.Scope,
	in io.Reader, out io.Writer, logger mcp.Logger) error {
	server, err := mcp.NewServerForMode(version, mode)
	if err != nil {
		return err
	}
	server.SetScope(scope)
	server.SetLogger(logger)
	return rpc.NewStdioServer(server.RPCServer(), in, out).Serve()
}

// ServeNetwork serves the same tool registry over MCP Streamable HTTP.
//
// This is the function both entrypoints reach for --port, so the bind, the token
// and every printed line are identical between them by construction rather than
// by agreement.
//
// interactive selects whether the token guide follows the banner. It is a parameter rather
// than a check inside, so the terminal question is answered exactly once — in Serve, which
// is the only place that holds stdin — and so a test can drive either path.
func ServeNetwork(name, version string, opts Options, errOut io.Writer, interactive bool) error {
	server, token, err := Build(version, opts)
	if err != nil {
		return err
	}
	// Attached before Listen, so a request arriving in the same instant the listener
	// opens is still recorded. The transport logs refusals; the dispatcher inside it logs
	// calls. Both go to the same writer, in the same format.
	if logger := loggerFor(name, opts, errOut); logger != nil {
		server.SetLogger(logger)
	}
	if err := server.Listen(NetworkConfig(opts)); err != nil {
		return err
	}
	announce(errOut, name, opts, server, token)
	// The banner states facts unconditionally; this adds instruction, and only for a
	// person. A container log wants the former and not the latter. See guide.go.
	if interactive {
		printNetworkGuide(errOut, name, opts, server)
	}
	return server.Serve()
}

// Build constructs a network server without binding anything.
//
// Separated from ServeNetwork so a test can compare what two entrypoints build —
// the tool list, the schemas, the auth behaviour — without racing two listeners,
// and so a caller embedding this can mount the handler itself.
func Build(version string, opts Options) (*mcp.NetworkServer, string, error) {
	server, err := mcp.NewServerForMode(version, opts.Mode)
	if err != nil {
		return nil, "", err
	}
	return mcp.NewNetworkServer(server, NetworkConfig(opts))
}

// NetworkConfig maps command-line options onto the transport's configuration.
//
// One function, so the two commands cannot map them differently — which is the
// failure this package exists to make impossible.
func NetworkConfig(opts Options) mcp.NetworkConfig {
	return mcp.NetworkConfig{
		Mode:           opts.Mode,
		Host:           opts.Host,
		Port:           opts.Port,
		Token:          opts.Token,
		Scope:          opts.Scope,
		AllowedOrigins: opts.Origins,
	}
}

// capabilityList is the capability names, for flag help.
//
// A function rather than a constant string so the help text cannot fall behind the
// set: adding a capability updates the usage message with no second edit.
func capabilityList() []string {
	names := make([]string, 0, len(mcp.KnownCapabilities()))
	for _, c := range mcp.KnownCapabilities() {
		names = append(names, string(c))
	}
	return names
}

// announce writes the startup lines to stderr.
//
// stderr, not stdout, even in network mode: a process whose output is being read
// by anything at all should not have its diagnostics mistaken for protocol.
func announce(errOut io.Writer, name string, opts Options, server *mcp.NetworkServer, token string) {
	// The token is printed only when it was generated here. Echoing one the
	// operator supplied would copy a secret into logs that already have it under
	// control.
	if strings.TrimSpace(opts.Token) == "" {
		fmt.Fprintf(errOut, "%s: generated control token (shown once): %s\n", name, token)
		fmt.Fprintf(errOut, "%s: set %s to keep it across restarts\n", name, TokenEnv)
	}
	fmt.Fprintf(errOut, "%s: control endpoint http://%s%s (MCP Streamable HTTP, bearer token required)\n",
		name, server.Addr(), server.Endpoint())
	// The mode is printed unconditionally because it decides what the server can
	// do, and an operator scanning a log for "why can this thing generate code"
	// should find the answer on the startup line rather than by reading a config.
	fmt.Fprintf(errOut, "%s: mode %s (%s)\n", name, opts.Mode, modeSummary(opts.Mode))
	// The scope line is printed either way: "all capabilities" is a security-relevant
	// fact about a running server, and an operator who scoped a token wants to see
	// that it took effect rather than trusting that it did.
	if opts.Scope.IsScoped() {
		fmt.Fprintf(errOut, "%s: token scope %s\n", name, strings.Join(scopeNames(opts.Scope), ", "))
	} else {
		fmt.Fprintf(errOut, "%s: token scope all capabilities (unscoped)\n", name)
	}
	fmt.Fprintf(errOut, "%s: capability report at http://%s%s\n", name, server.Addr(), featuresEndpoint)
	// The workspace is a security-relevant fact about a running server, printed
	// either way for the same reason the scope line is: an operator who confined it
	// wants to see that it took effect, and one who removed confinement should find
	// that on the startup line rather than by reading a config.
	if roots := mcp.WorkspaceRoots(); len(roots) > 0 {
		fmt.Fprintf(errOut, "%s: filesystem tools confined to %s\n", name, strings.Join(roots, ", "))
	} else {
		fmt.Fprintf(errOut, "%s: WARNING filesystem confinement is OFF (--allow-any-path) — "+
			"tools may read, write and run `go test` anywhere on this host\n", name)
	}
	if opts.Host != mcp.DefaultNetworkHost {
		// Worth one line: the bind was widened deliberately, and the token is now
		// the only thing between the network and a code generator.
		fmt.Fprintf(errOut, "%s: bound to %s — reachable beyond loopback; the bearer token is the only guard\n",
			name, opts.Host)
		// A generator-mode server on a non-loopback bind is the highest-consequence
		// combination this package can produce: whoever holds the token can write
		// files and start containers. The warning names the fix rather than only
		// stating the risk — and it is suppressed once a scope is in force, because
		// telling an operator who has already narrowed the token to narrow it is how
		// a warning becomes noise that gets filtered out.
		if opts.Mode == mcp.ModeGenerator && !opts.Scope.IsScoped() {
			fmt.Fprintf(errOut, "%s: this is a %s server reachable off-host with an unscoped token — "+
				"consider --scope so a leaked credential cannot generate code or provision containers\n",
				name, mcp.ModeGenerator)
		}
	}
}

// scopeNames renders a scope's granted capabilities for the banner.
func scopeNames(scope mcp.Scope) []string {
	granted := scope.Granted()
	out := make([]string, 0, len(granted))
	for _, c := range granted {
		out = append(out, string(c))
	}
	return out
}

// modeSummary is the half-sentence that says what a mode means, so the startup
// line is legible to someone who has not read mode.go.
func modeSummary(mode mcp.ServerMode) string {
	switch mode {
	case mcp.ModeGenerator:
		return "full toolchain: generates and changes projects"
	case mcp.ModeAppRuntime:
		return "read-only: inspects a running instance, no mutating tools registered"
	default:
		return "unknown"
	}
}
