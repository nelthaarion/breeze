package mcpcmd

// secrets_test.go — the banner must not print a secret it was given.
//
// The startup banner is the one place this package writes a token deliberately, and it is
// written to stderr — which is the stream an operator redirects to a log file, a container
// runtime captures, and a supervisor ships elsewhere. So the distinction between "printed
// because it was generated here and nobody else can know it" and "echoed back after the
// operator already had it" is the difference between a one-time disclosure and a secret
// copied into a log that outlives the process.
//
// entrypoints_test.go compares the two entrypoints' banners with the token normalised out,
// which is the right assertion for parity and cannot see this: an echoed token would be
// normalised identically in both and the comparison would pass.

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// TestBannerDoesNotEchoASuppliedToken is the case the parity test structurally cannot
// catch.
func TestBannerDoesNotEchoASuppliedToken(t *testing.T) {
	t.Setenv(TokenEnv, "")

	const secret = "operator-supplied-token-9f3c4d5e6f"

	for _, name := range bothNames {
		opts, err := ParseFlags(name, []string{
			"--mode", "generator", "--port", "2000", "--token", secret,
		}, io.Discard)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}

		banner := rawBanner(t, name, opts)
		if strings.Contains(banner, secret) {
			t.Errorf("%s echoed the supplied token to stderr. The operator already has it, and "+
				"stderr is what gets captured into logs — so printing it copies a secret "+
				"somewhere it outlives the process:\n%s", name, banner)
		}
		// "shown once" is the generated-token line, which must not appear either: it
		// would tell an operator to write down a value that was never printed.
		if strings.Contains(banner, "shown once") {
			t.Errorf(
				"%s printed the generated-token line for a token it was given:\n%s",
				name,
				banner,
			)
		}
	}
}

// TestBannerPrintsAGeneratedTokenExactlyOnce is the other half.
//
// A generated token that is never printed is a server nobody can authenticate against:
// the value exists only inside the process. So this line has to be there, and the test
// asserts it is there once rather than merely present — a value printed twice is twice as
// likely to survive a truncated log, and the "shown once" wording would be false.
func TestBannerPrintsAGeneratedTokenExactlyOnce(t *testing.T) {
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

	var errOut bytes.Buffer
	announce(&errOut, "breeze-mcp", opts, server, token)
	banner := errOut.String()

	if token == "" {
		t.Fatal(
			"no token was generated for a network server; the endpoint would be unauthenticated",
		)
	}
	if n := strings.Count(banner, token); n != 1 {
		t.Errorf(
			"the generated token appears %d times in the banner, want exactly 1:\n%s",
			n,
			banner,
		)
	}
}

// rawBanner captures the banner with nothing normalised.
//
// bannerFor in entrypoints_test.go substitutes the token out, which is correct for
// comparing two banners and wrong for asking whether a secret is in one.
func rawBanner(t *testing.T, name string, opts Options) string {
	t.Helper()

	server, token, err := Build("test-version", opts)
	if err != nil {
		t.Fatalf("%s: Build: %v", name, err)
	}
	listenOpts := opts
	listenOpts.Port = 0
	if err := server.Listen(NetworkConfig(listenOpts)); err != nil {
		t.Fatalf("%s: Listen: %v", name, err)
	}
	defer server.Close()

	var errOut bytes.Buffer
	announce(&errOut, name, opts, server, token)
	return errOut.String()
}
