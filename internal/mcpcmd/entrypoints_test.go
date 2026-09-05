package mcpcmd

// entrypoints_test.go — the two commands are one implementation.
//
// `breeze-mcp` and `breeze start mcp-server` are separate entrypoints on purpose,
// but every decision behind them — flag names, defaults, the loopback bind, the
// mandatory token, the required mode, and the startup banner — must be the same
// decision made once. These tests are what fails if a second copy of any of it
// appears.
//
// The banner is included because it was, in fact, missing from the subcommand at one
// point: the shared refactor covered server construction but the announcement was
// still inline in cmd/breeze-mcp. An operator launching the subcommand had no way to
// see the generated token. Comparing structure rather than only checking "some
// output exists" is what catches that.

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/nelthaarion/breeze/v2/internal/mcp"
)

// bothNames are the two command names, exactly as each entrypoint passes them.
var bothNames = []string{"breeze-mcp", "breeze start mcp-server"}

// TestEntrypointsParseIdentically — the same argv must produce the same Options
// under either name, because the name is a label and nothing else.
func TestEntrypointsParseIdentically(t *testing.T) {
	t.Setenv(TokenEnv, "")

	argvs := [][]string{
		{"--mode", "generator"},
		{"--mode", "app-runtime"},
		{"--mode", "generator", "--port", "2000"},
		{"--mode", "generator", "--port", "2000", "--host", "0.0.0.0"},
		{"--mode", "generator", "--token", "fixed-token"},
		{"--mode", "app-runtime", "--allow-origin", "https://a.example.com,https://b.example.com"},
		{"--mode", "generator", "--scope", "fleet,runtime"},
		{"--mode", "app-runtime", "--scope", "runtime"},
		{"--mode", "generator", "--log"},
		{"--mode", "app-runtime", "--port", "2000", "--log"},
	}

	for _, argv := range argvs {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			first, err := ParseFlags(bothNames[0], argv, io.Discard)
			if err != nil {
				t.Fatalf("%s: %v", bothNames[0], err)
			}
			second, err := ParseFlags(bothNames[1], argv, io.Discard)
			if err != nil {
				t.Fatalf("%s: %v", bothNames[1], err)
			}

			if first.Mode != second.Mode || first.Port != second.Port ||
				first.Host != second.Host || first.Token != second.Token ||
				first.Log != second.Log ||
				len(first.Origins) != len(second.Origins) {
				t.Fatalf(
					"the two entrypoints parsed %v differently:\n%+v\n%+v",
					argv,
					first,
					second,
				)
			}
			// Scope compared through its own accessors: the struct is unexported
			// inside, so == would compare fields this package cannot see change.
			if first.Scope.IsScoped() != second.Scope.IsScoped() {
				t.Fatalf("%v: scoped differs: %v vs %v", argv,
					first.Scope.IsScoped(), second.Scope.IsScoped())
			}
			if got, want := strings.Join(scopeNames(first.Scope), ","),
				strings.Join(scopeNames(second.Scope), ","); got != want {
				t.Fatalf("%v: granted differs: %q vs %q", argv, got, want)
			}
			for i := range first.Origins {
				if first.Origins[i] != second.Origins[i] {
					t.Errorf("origin %d differs: %q vs %q", i, first.Origins[i], second.Origins[i])
				}
			}
		})
	}
}

// TestBothEntrypointsRequireMode is the Correction's second clause: --mode is
// required identically, so neither command can be started without choosing.
func TestBothEntrypointsRequireMode(t *testing.T) {
	t.Setenv(TokenEnv, "")

	for _, name := range bothNames {
		// No --mode at all.
		if _, err := ParseFlags(name, nil, io.Discard); err == nil {
			t.Errorf("%s accepted no --mode", name)
		}
		// A port but still no mode: the combination someone actually types.
		if _, err := ParseFlags(name, []string{"--port", "2000"}, io.Discard); err == nil {
			t.Errorf("%s accepted --port with no --mode", name)
		}
		// An empty value, which is what an unset shell variable expands to.
		if _, err := ParseFlags(name, []string{"--mode", ""}, io.Discard); err == nil {
			t.Errorf("%s accepted an empty --mode", name)
		}
	}
}

// bannerFor captures what one entrypoint prints at startup, with the port and token
// normalised away so two runs are comparable.
//
// It binds port 0 and then substitutes the real address back out, because the banner
// legitimately contains a port and a generated token that differ per run — and those
// are exactly the two things that must not make an otherwise-identical banner look
// different.
func bannerFor(t *testing.T, name string, opts Options) string {
	t.Helper()

	server, token, err := Build("test-version", opts)
	if err != nil {
		t.Fatalf("%s: Build: %v", name, err)
	}
	// Port 0 so the test never competes for a fixed number.
	listenOpts := opts
	listenOpts.Port = 0
	if err := server.Listen(NetworkConfig(listenOpts)); err != nil {
		t.Fatalf("%s: Listen: %v", name, err)
	}
	defer server.Close()

	var errOut bytes.Buffer
	announce(&errOut, name, opts, server, token)

	out := errOut.String()
	// Normalise the three per-run values: the command name (which is the one thing
	// that is *meant* to differ), the token, and the bound address.
	out = strings.ReplaceAll(out, name+":", "CMD:")
	out = strings.ReplaceAll(out, token, "<TOKEN>")
	if addr := server.Addr(); addr != nil {
		out = strings.ReplaceAll(out, addr.String(), "<ADDR>")
	}
	return out
}

// TestEntrypointsPrintTheSameBanner is the Correction's required test.
//
// It compares the whole normalised banner, not the presence of output: the bug it
// guards against was a subcommand that printed nothing at all, and a weaker
// assertion would also have passed for a subcommand printing something different.
func TestEntrypointsPrintTheSameBanner(t *testing.T) {
	t.Setenv(TokenEnv, "")

	cases := []struct {
		name string
		argv []string
	}{
		// The generated-token case: no token pre-set, so the banner must contain the
		// "shown once" line. This is the one an operator depends on.
		{"generated token", []string{"--mode", "generator", "--port", "2000"}},
		{"app-runtime", []string{"--mode", "app-runtime", "--port", "2000"}},
		// A supplied token must NOT be echoed, identically in both.
		{
			"supplied token",
			[]string{"--mode", "generator", "--port", "2000", "--token", "supplied-secret"},
		},
		// A widened bind adds warning lines; both must add the same ones.
		{"widened bind", []string{"--mode", "generator", "--port", "2000", "--host", "0.0.0.0"}},
		// A scoped token changes the banner's scope line, and suppresses the
		// unscoped-off-host warning. Both entrypoints must agree on both changes.
		{"scoped token", []string{"--mode", "generator", "--port", "2000", "--scope", "fleet"}},
		{"scoped widened bind", []string{
			"--mode", "generator", "--port", "2000",
			"--host", "0.0.0.0", "--scope", "fleet",
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			first, err := ParseFlags(bothNames[0], tc.argv, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			second, err := ParseFlags(bothNames[1], tc.argv, io.Discard)
			if err != nil {
				t.Fatal(err)
			}

			a := bannerFor(t, bothNames[0], first)
			b := bannerFor(t, bothNames[1], second)

			if a != b {
				t.Errorf("the two entrypoints print different banners for %v\n%s: %q\n%s: %q",
					tc.argv, bothNames[0], a, bothNames[1], b)
			}
			if strings.TrimSpace(a) == "" {
				t.Fatal("the banner is empty; an operator has no way to see the endpoint or token")
			}
		})
	}
}

// TestScopeFlagIsParsed covers the flag itself: the values an operator types, the
// blank one an unset shell variable expands to, and the typo.
func TestScopeFlagIsParsed(t *testing.T) {
	t.Setenv(TokenEnv, "")

	// No --scope at all: unscoped, which is what every existing command line is.
	opts, err := ParseFlags("breeze-mcp", []string{"--mode", "generator"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Scope.IsScoped() {
		t.Error("omitting --scope produced a scoped token")
	}

	// An empty value is the same intent as omitting it. An unset BREEZE_MCP_SCOPE in
	// a wrapper script expands to exactly this, and failing there would break a
	// deployment over a variable that was never set.
	opts, err = ParseFlags("breeze-mcp", []string{"--mode", "generator", "--scope", ""}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Scope.IsScoped() {
		t.Error(`--scope="" produced a scoped token`)
	}

	opts, err = ParseFlags("breeze-mcp",
		[]string{"--mode", "generator", "--scope", "provisioning,fleet"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(scopeNames(opts.Scope), ","); got != "fleet,provisioning" {
		t.Errorf("granted = %q, want \"fleet,provisioning\" (sorted)", got)
	}

	// A typo is refused at startup rather than silently granting less than intended,
	// which is the failure an operator would not notice until a tool went missing.
	if _, err := ParseFlags("breeze-mcp",
		[]string{"--mode", "generator", "--scope", "fleet,provisionning"}, io.Discard); err == nil {
		t.Error("a misspelled capability was accepted")
	}
}

// TestScopeReachesTheServer — the flag has to arrive at the transport, not merely
// parse. NetworkConfig is the one conversion between them, so this pins it.
func TestScopeReachesTheServer(t *testing.T) {
	t.Setenv(TokenEnv, "")

	opts, err := ParseFlags("breeze-mcp",
		[]string{"--mode", "generator", "--scope", "fleet"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg := NetworkConfig(opts); !cfg.Scope.IsScoped() {
		t.Fatal("NetworkConfig dropped the scope; the flag would parse and do nothing")
	}

	server, _, err := Build("test-version", opts)
	if err != nil {
		t.Fatal(err)
	}
	if !server.Scope().IsScoped() {
		t.Error("the built server is unscoped despite --scope fleet")
	}
	if got := strings.Join(scopeNames(server.Scope()), ","); got != "fleet" {
		t.Errorf("the server's granted set is %q, want \"fleet\"", got)
	}
}

// TestBannerReportsScope — the scope line is printed either way, because "all
// capabilities" is a security-relevant fact and an operator who passed --scope needs
// to see that it took.
func TestBannerReportsScope(t *testing.T) {
	t.Setenv(TokenEnv, "")

	scoped, err := ParseFlags("breeze-mcp",
		[]string{"--mode", "generator", "--port", "2000", "--scope", "fleet"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	banner := bannerFor(t, "breeze-mcp", scoped)
	if !strings.Contains(banner, "token scope fleet") {
		t.Errorf("the banner does not report the scope:\n%s", banner)
	}
	if !strings.Contains(banner, FeaturesEndpoint) {
		t.Errorf("the banner does not name the capability report endpoint:\n%s", banner)
	}

	unscoped, err := ParseFlags("breeze-mcp",
		[]string{"--mode", "generator", "--port", "2000"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	banner = bannerFor(t, "breeze-mcp", unscoped)
	if !strings.Contains(banner, "unscoped") {
		t.Errorf("the banner does not say the token is unscoped:\n%s", banner)
	}
}

// TestOffHostWarningTracksScope — the warning names --scope as the fix, and stops once
// the operator has applied it. A warning that fires after it has been acted on is one
// that gets filtered out, taking the useful cases with it.
func TestOffHostWarningTracksScope(t *testing.T) {
	t.Setenv(TokenEnv, "")

	const marker = "consider --scope"

	unscoped, err := ParseFlags("breeze-mcp",
		[]string{"--mode", "generator", "--port", "2000", "--host", "0.0.0.0"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if banner := bannerFor(t, "breeze-mcp", unscoped); !strings.Contains(banner, marker) {
		t.Errorf("an unscoped generator on 0.0.0.0 was not warned:\n%s", banner)
	}

	scoped, err := ParseFlags("breeze-mcp",
		[]string{"--mode", "generator", "--port", "2000", "--host", "0.0.0.0", "--scope", "fleet"},
		io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if banner := bannerFor(t, "breeze-mcp", scoped); strings.Contains(banner, marker) {
		t.Errorf("a scoped generator was still told to use --scope:\n%s", banner)
	}
}

// TestScopeAppliesOnStdioToo — `--scope` must not be a flag that parses and then does
// nothing on the transport most people use.
//
// Stdio has no token to restrict, so the scope is not a credential boundary here: it is
// the operator saying what this subprocess should offer at all, which an editor
// configuration can legitimately want. The handshake advertises `breezeCapabilities` on
// every transport, so it has to be true on every transport.
func TestScopeAppliesOnStdioToo(t *testing.T) {
	scope, err := mcp.NewScope(mcp.CapFleet)
	if err != nil {
		t.Fatal(err)
	}

	const input = `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}` + "\n"

	var scopedOut, unscopedOut bytes.Buffer
	if err := ServeStdio("test-version", mcp.ModeGenerator, scope,
		strings.NewReader(input), &scopedOut, nil); err != nil {
		t.Fatal(err)
	}
	if err := ServeStdio("test-version", mcp.ModeGenerator, mcp.UnscopedScope(),
		strings.NewReader(input), &unscopedOut, nil); err != nil {
		t.Fatal(err)
	}

	// A fleet-scoped listing must be shorter, and must not name a generator tool.
	if scopedOut.Len() >= unscopedOut.Len() {
		t.Errorf("the scoped listing is not smaller: %d vs %d bytes",
			scopedOut.Len(), unscopedOut.Len())
	}
	if strings.Contains(scopedOut.String(), `"breeze_generate"`) {
		t.Error("a fleet-scoped stdio server advertised breeze_generate")
	}
	if !strings.Contains(scopedOut.String(), `"breeze_get_trace"`) {
		t.Errorf("the scoped listing omits breeze_get_trace:\n%s", scopedOut.String())
	}
	// And the unscoped one is unchanged, so the default behaviour is intact.
	if !strings.Contains(unscopedOut.String(), `"breeze_generate"`) {
		t.Error("an unscoped stdio server no longer advertises breeze_generate")
	}
}

// TestStdioHandshakeReportsCapabilities — the payload an agent depends on has to be
// present on stdio, since that is where /mcp/features is not.
func TestStdioHandshakeReportsCapabilities(t *testing.T) {
	scope, err := mcp.NewScope(mcp.CapFleet, mcp.CapRuntime)
	if err != nil {
		t.Fatal(err)
	}

	const input = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":` +
		`{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}` + "\n"

	var out bytes.Buffer
	if err := ServeStdio("test-version", mcp.ModeGenerator, scope,
		strings.NewReader(input), &out, nil); err != nil {
		t.Fatal(err)
	}

	var reply struct {
		Result struct {
			BreezeServerKind   string `json:"breezeServerKind"`
			BreezeCapabilities *struct {
				Granted []string `json:"granted"`
				Known   []string `json:"known"`
				Scoped  bool     `json:"scoped"`
			} `json:"breezeCapabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &reply); err != nil {
		t.Fatalf("the handshake reply is not JSON: %v\n%s", err, out.String())
	}

	if reply.Result.BreezeCapabilities == nil {
		t.Fatal("stdio's initialize carried no breezeCapabilities; on this transport there is " +
			"no /mcp/features to fall back to")
	}
	if got := strings.Join(reply.Result.BreezeCapabilities.Granted, ","); got != "fleet,runtime" {
		t.Errorf("granted = %q, want \"fleet,runtime\"", got)
	}
	if !reply.Result.BreezeCapabilities.Scoped {
		t.Error("scoped = false for a two-capability scope")
	}
	if len(reply.Result.BreezeCapabilities.Known) != len(mcp.KnownCapabilities()) {
		t.Errorf("known = %v, want all %d",
			reply.Result.BreezeCapabilities.Known, len(mcp.KnownCapabilities()))
	}
	if reply.Result.BreezeServerKind != string(mcp.ModeGenerator) {
		t.Errorf("breezeServerKind = %q, want %q",
			reply.Result.BreezeServerKind, mcp.ModeGenerator)
	}
}
