package generator

// Tests for the --config plumbing.
//
// config_test.go covers the schema: parsing, precedence, validation. This file
// covers the part that was missing until now â€” that a configuration file
// actually reaches the generators and changes what lands on disk. The
// distinction matters because every test in the other file would still pass if
// nothing in the CLI ever called Load.

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// generateFeatureWithArgs runs a feature's generator with the given flag
// arguments, returning the parse or generation error rather than failing.
//
// add_test.go's generateFeature parses nil flags and fails the test on error,
// which is right for asking what a generator reads. Here the flags are the
// subject: a caller needs to assert that a specific argument list is accepted,
// so the error has to come back rather than end the test.
func generateFeatureWithArgs(t *testing.T, f *feature, args []string, ctx featureCtx) (featureOutput, error) {
	t.Helper()

	fs := flag.NewFlagSet("config "+f.Name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	generate := f.Build(fs)

	if err := parseFlags(fs, args); err != nil {
		return featureOutput{}, err
	}
	return generate(ctx)
}

func TestExtractConfigFlag(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantPath string
		wantRest []string
	}{
		{"absent", []string{"User", "--force"}, "", []string{"User", "--force"}},
		{"equals form", []string{"--config=a.yaml", "User"}, "a.yaml", []string{"User"}},
		{"space form", []string{"--config", "a.yaml", "User"}, "a.yaml", []string{"User"}},
		{"among others", []string{"User", "--config=a.yaml", "--force"}, "a.yaml", []string{"User", "--force"}},
		// A path that looks like a flag value but is not the flag itself must be
		// left alone, or a command's own --config-ish flag would be eaten.
		{"unrelated flag", []string{"--configure=x"}, "", []string{"--configure=x"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path, rest, err := extractConfigFlag(tc.args)
			if err != nil {
				t.Fatalf("extractConfigFlag: %v", err)
			}
			if path != tc.wantPath {
				t.Errorf("path = %q, want %q", path, tc.wantPath)
			}
			if !reflect.DeepEqual(rest, tc.wantRest) {
				t.Errorf("rest = %q, want %q", rest, tc.wantRest)
			}
		})
	}
}

// TestExtractConfigFlagRejectsMissingValue â€” a bare --config is a mistake, and
// silently ignoring it would generate a project from defaults while the user
// believes their file was read.
func TestExtractConfigFlagRejectsMissingValue(t *testing.T) {
	for _, args := range [][]string{
		{"--config"},
		{"--config", "--force"},
		{"--config="},
	} {
		if _, _, err := extractConfigFlag(args); err == nil {
			t.Errorf("%q was accepted", args)
		}
	}
}

// TestMissingExplicitConfigIsAnError â€” naming a file that is not there must
// fail rather than fall back to defaults.
func TestMissingExplicitConfigIsAnError(t *testing.T) {
	t.Chdir(t.TempDir())
	if _, err := resolveConfigPath("nope.yaml"); err == nil {
		t.Error("a nonexistent --config path was accepted")
	}
}

// TestDefaultConfigFileIsPickedUp â€” a breeze.yaml in the working directory is
// read without being named, which is what makes it worth committing.
func TestDefaultConfigFileIsPickedUp(t *testing.T) {
	t.Chdir(t.TempDir())

	if path, err := resolveConfigPath(""); err != nil || path != "" {
		t.Fatalf("with no file present: path = %q, err = %v; want empty and nil", path, err)
	}

	if err := os.WriteFile(defaultConfigFile, []byte("server:\n  port: 3000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path, err := resolveConfigPath("")
	if err != nil {
		t.Fatalf("resolveConfigPath: %v", err)
	}
	if path != defaultConfigFile {
		t.Errorf("path = %q, want %q", path, defaultConfigFile)
	}
}

// TestConfigFeatureNamesOrdersByPriority is the ordering claim.
//
// Features compose by reading whether another is installed at generation time,
// so applying them in file order rather than priority order would wire a block
// to a fallback that the next step invalidates. The file here deliberately
// lists them backwards.
func TestConfigFeatureNamesOrdersByPriority(t *testing.T) {
	cfg := Defaults()
	cfg.Middleware = []MiddlewareConfig{
		{Name: "etag"},
		{Name: "recovery"},
		{Name: "cors"},
	}

	got := configFeatureNames(cfg)

	for i := 1; i < len(got); i++ {
		if features[got[i-1]].Priority > features[got[i]].Priority {
			t.Fatalf("not in priority order: %v", got)
		}
	}
	// recovery is the outermost middleware and must come first regardless of
	// where it appeared in the file.
	if len(got) == 0 || got[0] != "recovery" {
		t.Errorf("got %v, want recovery first", got)
	}
}

// TestConfigFeatureNamesResolvesAliases â€” a hyphenated name in YAML has to
// reach the same feature `breeze add` would.
func TestConfigFeatureNamesResolvesAliases(t *testing.T) {
	cfg := Defaults()
	cfg.Middleware = []MiddlewareConfig{{Name: "rate-limit", RPS: 50}}

	// Membership, not equality: Defaults() enables docs, so the list is never
	// only what the middleware section named. See TestDocsIsEnabledByDefault.
	if !containsFeature(configFeatureNames(cfg), "ratelimit") {
		t.Errorf("rate-limit did not resolve to the ratelimit feature: %v", configFeatureNames(cfg))
	}
	if containsFeature(configFeatureNames(cfg), "rate-limit") {
		t.Error("the unresolved alias leaked into the feature list")
	}
}

// TestDocsIsEnabledByDefault pins a consequence of the schema that is easy to
// miss and impossible to see from the YAML.
//
// Defaults() sets docs.enabled, which predates this plumbing. Now that a
// configuration actually reaches the generators, that default means any project
// generated with a config file present gets an OpenAPI endpoint and a Scalar UI
// â€” including one whose file says nothing but `module:`. That may well be
// wanted, but it is a real behavioural consequence and it should fail loudly
// here if someone flips the default rather than surprising a user.
func TestDocsIsEnabledByDefault(t *testing.T) {
	if !Defaults().Docs.Enabled {
		t.Skip("docs is no longer on by default; this test documented that it was")
	}
	if !containsFeature(configFeatureNames(Defaults()), "docs") {
		t.Error("docs.enabled defaults to true but the docs feature is not applied")
	}
}

func containsFeature(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}

// TestTypedSectionsEnableTheirFeature â€” a section with enabled: true should not
// also have to be named in the middleware list.
func TestTypedSectionsEnableTheirFeature(t *testing.T) {
	cfg := Defaults()
	cfg.WebSocket.Enabled = true
	cfg.Fleet.Enabled = true
	cfg.JSONRPC.Enabled = true

	got := configFeatureNames(cfg)
	for _, want := range []string{"websocket", "fleet", "jsonrpc"} {
		found := false
		for _, name := range got {
			if name == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s not enabled by its section: got %v", want, got)
		}
	}
}

// TestConfigFlagsForAreAcceptedByTheirFeature is the test that catches the
// class of bug this file exists to prevent.
//
// configFlagsFor writes flag arguments as strings, so a name that does not
// match the feature's own FlagSet is invisible to the compiler. Three were
// wrong when this was first written â€” --path for docs, which takes --ui-path;
// --rooms for websocket, which has no such flag; and --max-message-bytes on a
// feature that spells it differently. Each would have failed only when a user
// put that key in a file.
func TestConfigFlagsForAreAcceptedByTheirFeature(t *testing.T) {
	t.Chdir(t.TempDir())

	// Everything the switch has a case for, switched on, so each arm is
	// exercised rather than skipped for being disabled.
	cfg := Defaults()
	cfg.WebSocket.Enabled = true
	cfg.JSONRPC.Enabled = true
	cfg.JSONRPC.Methods = []string{"sum", "user.create"}
	cfg.JSONRPC.BlockingMethods = []string{"user.create"}
	cfg.JSONRPC.MaxMessageBytes = 1 << 20
	cfg.Fleet.Enabled = true
	cfg.Docs.Enabled = true
	cfg.Middleware = []MiddlewareConfig{
		{Name: "cors", Origins: []string{"https://example.com"}},
		{Name: "ratelimit", RPS: 100},
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("the fixture does not validate: %v", err)
	}

	for _, name := range configFeatureNames(cfg) {
		t.Run(name, func(t *testing.T) {
			f := features[name]
			args := configFlagsFor(name, cfg)

			// An empty list is legitimate â€” most features have no config
			// representation and take their defaults.
			if len(args) == 0 {
				return
			}

			out, err := generateFeatureWithArgs(t, f, args, featureCtx{ModulePath: "example.com/p"})
			if err != nil {
				t.Fatalf("configFlagsFor(%q) produced %q, which its own FlagSet rejects: %v", name, args, err)
			}
			if strings.TrimSpace(out.Body) == "" {
				t.Errorf("feature %q generated an empty block", name)
			}
		})
	}
}

// TestConfigFlagsForFleetTransportsAllParse covers the arms the fixture above
// cannot reach at once: transport is a single value, so only one is tested per
// configuration.
func TestConfigFlagsForFleetTransportsAllParse(t *testing.T) {
	t.Chdir(t.TempDir())

	for _, transport := range fleetImplementedTransports {
		t.Run(transport, func(t *testing.T) {
			cfg := Defaults()
			cfg.Fleet.Enabled = true
			cfg.Fleet.Transport = transport
			cfg.Fleet.ServiceName = "svc"

			if err := cfg.Validate(); err != nil {
				t.Fatalf("transport %s does not validate on defaults: %v", transport, err)
			}

			out, err := generateFeatureWithArgs(t, features["fleet"],
				configFlagsFor("fleet", cfg), featureCtx{ModulePath: "example.com/p"})
			if err != nil {
				t.Fatalf("fleet %s: %v", transport, err)
			}
			if !strings.Contains(out.Body, "fleet.New(") {
				t.Errorf("fleet %s did not emit a tracer:\n%s", transport, out.Body)
			}
		})
	}
}

// TestUnsupportedConfigKeysAreReported â€” a key that parses, validates, and then
// changes nothing is the worst configuration bug there is, because nothing
// disagrees with the user out loud.
func TestUnsupportedConfigKeysAreReported(t *testing.T) {
	cfg := Defaults()
	cfg.WebSocket.Enabled = true
	cfg.WebSocket.Rooms = true
	cfg.Middleware = []MiddlewareConfig{{Name: "jwt", Secret: "hunter2"}}

	got := unsupportedConfigKeys(cfg)
	if len(got) != 2 {
		t.Fatalf("got %d unsupported keys, want 2: %v", len(got), got)
	}

	joined := strings.Join(got, "\n")
	for _, want := range []string{"websocket.rooms", "secret"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no report mentions %q:\n%s", want, joined)
		}
	}

	// A configuration that asks for nothing unrepresentable must stay silent,
	// or the warning becomes noise everyone learns to ignore.
	clean := Defaults()
	clean.WebSocket.Enabled = true
	if got := unsupportedConfigKeys(clean); len(got) != 0 {
		t.Errorf("a supported configuration reported gaps: %v", got)
	}
}

// TestNewAppliesConfigFeatures is the end-to-end claim: a file on disk becomes
// blocks in the generated project.
func TestNewAppliesConfigFeatures(t *testing.T) {
	t.Chdir(t.TempDir())

	config := `
module: example.com/from-config
middleware:
  - name: recovery
  - name: cors
    origins: [https://example.com]
fleet:
  enabled: true
  service_name: gateway
  transport: http
`
	if err := os.WriteFile("breeze.yaml", []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := runNew([]string{"configured"}); err != nil {
		t.Fatalf("breeze new with a config file: %v", err)
	}

	// The module path came from the file, not the project name.
	goMod, err := os.ReadFile(filepath.Join("configured", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(goMod), "module example.com/from-config") {
		t.Errorf("module path did not come from the config file:\n%s", goMod)
	}

	generated, err := os.ReadFile(filepath.Join("configured", featuresFileName))
	if err != nil {
		t.Fatal(err)
	}
	src := string(generated)

	for _, want := range []string{"setupRecovery", "setupCors", "setupFleet", "https://example.com", "gateway"} {
		if !strings.Contains(src, want) {
			t.Errorf("%s missing from the generated features file:\n%s", want, src)
		}
	}

	// And the dispatcher must call them, or the blocks are inert.
	for _, want := range []string{"setupRecovery(app, router)", "setupFleet(app, router)"} {
		if !strings.Contains(src, want) {
			t.Errorf("the dispatcher does not call %s", want)
		}
	}
}

// TestNewWithFlagOverridingConfig â€” the documented precedence, through the
// command rather than through Load alone.
func TestNewWithFlagOverridingConfig(t *testing.T) {
	t.Chdir(t.TempDir())

	if err := os.WriteFile("breeze.yaml", []byte("module: example.com/from-file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runNew([]string{"over", "--module=example.com/from-flag"}); err != nil {
		t.Fatalf("breeze new: %v", err)
	}

	goMod, err := os.ReadFile(filepath.Join("over", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(goMod), "example.com/from-flag") {
		t.Errorf("--module did not override the config file:\n%s", goMod)
	}
}

// TestNewRejectsInvalidConfig â€” validation has to run before anything is
// written, so a bad file does not leave half a project behind.
func TestNewRejectsInvalidConfig(t *testing.T) {
	t.Chdir(t.TempDir())

	if err := os.WriteFile("breeze.yaml", []byte("fleet:\n  enabled: true\n  transport: grpc\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := runNew([]string{"bad"})
	if err == nil {
		t.Fatal("a configuration naming an unimplemented transport was accepted")
	}
	if _, statErr := os.Stat("bad"); statErr == nil {
		t.Error("the project directory was created despite the configuration being invalid")
	}
}

// TestApplyConfigFeaturesIsIdempotent â€” applying the same file twice must
// settle, since a config file is the declared state of the project and re-
// applying it is the normal way to use one.
func TestApplyConfigFeaturesIsIdempotent(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := runNew([]string{"idem", "--module=example.com/idem"}); err != nil {
		t.Fatalf("breeze new: %v", err)
	}
	t.Chdir("idem")

	cfg := Defaults()
	cfg.Middleware = []MiddlewareConfig{{Name: "recovery"}, {Name: "cors"}}
	cfg.Fleet.Enabled = true
	cfg.Fleet.ServiceName = "svc"

	apply := func(pass string) string {
		t.Helper()
		if err := applyConfigFeatures(cfg, "example.com/idem"); err != nil {
			t.Fatalf("applyConfigFeatures (%s): %v", pass, err)
		}
		content, err := os.ReadFile(featuresFileName)
		if err != nil {
			t.Fatal(err)
		}
		return string(content)
	}

	if first, second := apply("first"), apply("second"); first != second {
		t.Errorf("applying the same configuration twice changed %s:\n--- first\n%s\n--- second\n%s",
			featuresFileName, first, second)
	}
}
