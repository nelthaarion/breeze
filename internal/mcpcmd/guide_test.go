package mcpcmd

// guide_test.go — the guide must help a person and must never reach a protocol stream.
//
// Two properties, and the second is the one that can break a working editor integration:
//
//   - A person running this by hand gets told what it is and what to do next. The bug was
//     zero bytes on both streams, which is indistinguishable from a hang.
//   - An editor gets silence on stderr and nothing but JSON-RPC on stdout. One guide line
//     on stdout is one malformed MCP message to the peer, and some clients abandon the
//     session over it.
//
// The terminal check is what separates them, so these tests exercise both sides of it
// rather than only the output format.

import (
	"bytes"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/nelthaarion/breeze/v2/internal/mcp"
)

// TestAPipedStdinGetsNoGuide is the property that protects an editor.
//
// Serve is driven with a strings.Reader, which is what an editor's pipe looks like to
// interactiveStdin: not an *os.File, therefore not a terminal. stdout must be JSON-RPC and
// stderr must be empty.
func TestAPipedStdinGetsNoGuide(t *testing.T) {
	restoreConfinement(t)

	const input = `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}` + "\n"

	opts, err := ParseFlags("breeze-mcp", []string{"--mode", "app-runtime"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if err := Serve(
		"breeze-mcp",
		"test-version",
		opts,
		strings.NewReader(input),
		&out,
		&errOut,
	); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	if errOut.Len() != 0 {
		t.Errorf("a piped stdin produced %d bytes of stderr; an editor's log fills with text "+
			"nobody asked for:\n%s", errOut.Len(), errOut.String())
	}
	// Every line of stdout has to be a protocol message. This is the assertion that would
	// catch a guide line written to the wrong stream.
	for i, line := range bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n")) {
		if !bytes.HasPrefix(line, []byte(`{"jsonrpc"`)) {
			t.Fatalf("stdout line %d is not a JSON-RPC message: %q", i, line)
		}
	}
}

// TestARealTerminalGetsTheGuide is the other side of the same branch.
//
// os.Stdin under `go test` is not a terminal, so Serve cannot be made to take the
// interactive path here — which is exactly what the test above relies on. So the guide
// function is called directly, and interactiveStdin is asserted separately below.
func TestARealTerminalGetsTheGuide(t *testing.T) {
	restoreConfinement(t)

	opts, err := ParseFlags("breeze-mcp", []string{"--mode", "generator"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyConfinement(opts); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	printStdioGuide(&out, "breeze-mcp", opts)
	guide := out.String()

	// Each of these answers a question the silence left open. Asserted by substring rather
	// than against the whole block, so rewording a sentence does not fail the test but
	// dropping a fact does.
	for _, want := range []string{
		"not an interactive command", // it is not broken
		"waiting for JSON-RPC",       // what it is doing
		"silence is the",             // the silence is expected
		"Ctrl+C",                     // how to stop
		"mode        generator",      // what it will serve
		"no port, no token",          // why there is nothing to copy here
		"mcpServers",                 // how an editor is pointed at it
		"--port 2000",                // where a token comes from instead
		"-h",                         // where the rest is
	} {
		if !strings.Contains(guide, want) {
			t.Errorf("the guide never mentions %q:\n%s", want, guide)
		}
	}

	// The tool count comes from the mode filter rather than a literal, so this cannot drift
	// when a tool is added.
	if want := len(mcp.ModeToolNames(mcp.ModeGenerator)); !strings.Contains(guide,
		"tools       "+strconv.Itoa(want)) {
		t.Errorf("the guide does not report %d tools:\n%s", want, guide)
	}
}

// TestTheStdioGuideNamesNoSecret is the security property for the quiet transport.
//
// stdio has no credential — the process boundary is the trust boundary — so a guide that
// mentioned a token would be inventing one, and an operator following it would go looking
// for a value that does not exist. It must also not print the token from the environment,
// which is set for the network transport and is irrelevant here.
func TestTheStdioGuideNamesNoSecret(t *testing.T) {
	restoreConfinement(t)

	const envToken = "STDIO-MUST-NOT-PRINT-THIS-4c5d6e"
	t.Setenv(TokenEnv, envToken)

	opts, err := ParseFlags("breeze-mcp", []string{"--mode", "generator"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Token != envToken {
		t.Fatalf("the fixture did not take effect: Token = %q", opts.Token)
	}
	if err := applyConfinement(opts); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	printStdioGuide(&out, "breeze-mcp", opts)

	if strings.Contains(out.String(), envToken) {
		t.Errorf("the stdio guide printed $%s. stdio authenticates nothing, so the value is "+
			"both useless here and a secret:\n%s", TokenEnv, out.String())
	}
}

// TestInteractiveStdinIsFalseForEverythingThatIsNotATerminal covers the discriminator.
//
// The two directions have asymmetric consequences: a false negative costs a missing guide,
// a false positive writes text into somebody's protocol tooling. So the cases that must
// answer false are enumerated, including an *os.File that is a pipe — the shape an editor
// actually produces, which a bare `_, ok := in.(*os.File)` check would get wrong.
func TestInteractiveStdinIsFalseForEverythingThatIsNotATerminal(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	file, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	cases := map[string]io.Reader{
		"a strings.Reader, as tests use": strings.NewReader(""),
		"an os.Pipe, as an editor uses":  r,
		"a regular file, as < redirects": file,
		"nil":                            nil,
	}
	for name, in := range cases {
		if interactiveStdin(in) {
			t.Errorf("%s was treated as a terminal; the guide would be written into a "+
				"stream something else is parsing", name)
		}
	}

	// os.Stdin is deliberately absent from that table. Under `go test` on Windows it is a
	// character device and therefore answers true, while under a CI runner's pipe it answers
	// false — asserting either would encode one environment's answer as the rule. It is also
	// not the value that matters: Serve answers this question about the reader it was given,
	// which is why every test above can drive the quiet path.
}

// TestTheNetworkGuideExplainsTheTokenWithoutPrintingIt is the security property for the
// transport that has one.
//
// The banner prints a generated token exactly once, and
// TestBannerPrintsAGeneratedTokenExactlyOnce holds that count — so a second copy inside a
// usage example would widen the window for a log to keep it, for nothing. Every example
// therefore reads the environment variable, which is also the form that keeps a token out
// of `ps` and `docker inspect`.
func TestTheNetworkGuideExplainsTheTokenWithoutPrintingIt(t *testing.T) {
	restoreConfinement(t)
	t.Setenv(TokenEnv, "")

	opts, err := ParseFlags("breeze-mcp",
		[]string{"--mode", "generator", "--port", "2000"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}

	server, token, err := Build("test-version", opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	listenOpts := opts
	listenOpts.Port = 0
	if err := server.Listen(NetworkConfig(listenOpts)); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer server.Close()

	if token == "" {
		t.Fatal("no token was generated; there would be nothing to explain")
	}

	var out bytes.Buffer
	printNetworkGuide(&out, "breeze-mcp", opts, server)
	guide := out.String()

	if strings.Contains(guide, token) {
		t.Errorf("the guide reprinted the token. The banner already showed it once, and a "+
			"second copy only widens the window for a log to keep it:\n%s", guide)
	}
	for _, want := range []string{
		TokenEnv,                // where to put it
		"Authorization: Bearer", // what to do with it
		"mcpServers",            // the client config
		"401",                   // what happens without it
		featuresEndpoint,        // how to check it works
	} {
		if !strings.Contains(guide, want) {
			t.Errorf("the guide never mentions %q, so it does not answer how to use the "+
				"token:\n%s", want, guide)
		}
	}
}

// TestTheNetworkGuideDoesNotEchoASuppliedToken is the case a generated-token test cannot
// see.
//
// When --token or $BREEZE_MCP_TOKEN supplied the value, the operator already has it and the
// banner deliberately stays quiet. The guide has to stay quiet too — the same distinction
// secrets_test.go draws for the banner, applied to the block printed after it.
func TestTheNetworkGuideDoesNotEchoASuppliedToken(t *testing.T) {
	restoreConfinement(t)

	const supplied = "operator-supplied-guide-token-8b7a6c"
	t.Setenv(TokenEnv, supplied)

	opts, err := ParseFlags("breeze-mcp",
		[]string{"--mode", "app-runtime", "--port", "2000"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Token != supplied {
		t.Fatalf("the fixture did not take effect: Token = %q", opts.Token)
	}

	server, _, err := Build("test-version", opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	listenOpts := opts
	listenOpts.Port = 0
	if err := server.Listen(NetworkConfig(listenOpts)); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer server.Close()

	var out bytes.Buffer
	printNetworkGuide(&out, "breeze-mcp", opts, server)
	guide := out.String()

	if strings.Contains(guide, supplied) {
		t.Errorf("the guide echoed the supplied token:\n%s", guide)
	}
	// And it must not tell an operator to write down a value that was never shown.
	if strings.Contains(guide, "shown once") {
		t.Errorf("the guide claims the token was shown once, for one it was given:\n%s", guide)
	}
}

// TestTheGuideReportsUnconfinedAsAWarning is the one line in the guide that is not merely
// informational.
//
// --allow-any-path means breeze_verify_project will run `go test` in any directory it is
// handed. On a terminal that is the fact most worth seeing, and printing it as an ordinary
// row beside "mode" and "tools" would bury it.
func TestTheGuideReportsUnconfinedAsAWarning(t *testing.T) {
	restoreConfinement(t)

	opts, err := ParseFlags("breeze-mcp",
		[]string{"--mode", "generator", "--allow-any-path"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyConfinement(opts); err != nil {
		t.Fatal(err)
	}
	if roots := mcp.WorkspaceRoots(); len(roots) != 0 {
		t.Fatalf("--allow-any-path left confinement in place: %v", roots)
	}

	var out bytes.Buffer
	printStdioGuide(&out, "breeze-mcp", opts)
	guide := out.String()

	if !strings.Contains(guide, "UNCONFINED") {
		t.Errorf("the guide does not flag missing confinement:\n%s", guide)
	}
	if !strings.Contains(guide, "go test") {
		t.Errorf("the guide states the risk without naming the consequence:\n%s", guide)
	}
}
