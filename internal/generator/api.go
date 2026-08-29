package generator

// api.go — the exported surface of the generator.
//
// Everything else in this package is unexported and was written to be called
// from one place: the CLI's main(). That is still the primary caller, but it is
// no longer the only one — cmd/breeze-mcp drives the same generators to serve an
// agent, and a program cannot import package main.
//
// So this file exists to be the seam. It is deliberately thin: each function
// forwards to the unexported implementation and adds nothing. The alternative
// was exporting three dozen symbols across the package, which would have turned
// every internal helper into API that has to be kept stable. Keeping the seam in
// one file means the MCP server's dependency on the generator is visible in a
// single place, and the generators themselves stay free to change.
//
// Because this is under internal/, none of it is public API of the module.

import (
	"flag"
	"io"
)

// ─── Command entry points ────────────────────────────────────────────────────
//
// These take the argument list a user typed, exactly as the CLI received it, and
// return an error rather than exiting. They are the same functions the CLI has
// always called; only the name is new.

// New scaffolds a project. Args is everything after `breeze new`.
func New(args []string) error { return runNew(args) }

// Generate scaffolds one piece of code. Args is everything after `breeze generate`.
func Generate(args []string) error { return runGenerate(args) }

// Add wires a framework feature into the project in the working directory.
func Add(args []string) error { return runAdd(args) }

// Routes lists the routes in routes_generated.go by parsing it.
func Routes(args []string) error { return runRoutes(args) }

// Migrate runs database migrations.
func Migrate(args []string) error { return runMigrate(args) }

// MakeMigration creates an up/down migration pair.
func MakeMigration(args []string) error { return runMakeMigration(args) }

// ─── Help and version ────────────────────────────────────────────────────────

// PrintVersion writes the CLI's breeze version, Go toolchain and platform.
func PrintVersion(w io.Writer) { printVersion(w) }

// PrintGenerateHelp writes per-generator help.
func PrintGenerateHelp(w io.Writer) { printGenerateHelp(w) }

// PrintAddHelp writes the feature list with each feature's flags.
func PrintAddHelp(w io.Writer) { printAddHelp(w) }

// PrintMigrateHelp writes migration subcommand help.
func PrintMigrateHelp(w io.Writer) { printMigrateHelp(w) }

// ─── Feature registry ────────────────────────────────────────────────────────
//
// The MCP server needs to describe the available features to an agent, and to
// drive one directly rather than through an argv round-trip. These expose the
// registry without exposing the feature struct itself.

// FeatureNames returns every registered feature name, in priority order —
// the order they must be applied in, since features read whether others are
// installed at generation time.
func FeatureNames() []string { return featureNames() }

// CanonicalFeatureName resolves an alias ("rate-limit") to the registered name
// ("ratelimit"). Names that are not aliases are returned unchanged; callers
// that need validation should use FeatureSummary or FeatureFlags as the registry
// lookup.
func CanonicalFeatureName(name string) string { return canonicalFeatureName(name) }

// FeatureSummary returns the one-line description shown by `breeze add --list`,
// and whether the feature exists at all.
func FeatureSummary(name string) (string, bool) {
	f, ok := features[canonicalFeatureName(name)]
	if !ok {
		return "", false
	}
	return f.Summary, true
}

// FeatureFlags returns the flags a feature accepts, as name/default/usage
// triples, by building its FlagSet and walking it.
//
// This is how an agent-facing schema stays honest: the flags reported here are
// the same FlagSet the generator will parse, so a described flag cannot drift
// from an accepted one.
func FeatureFlags(name string) ([]FlagInfo, bool) {
	f, ok := features[canonicalFeatureName(name)]
	if !ok {
		return nil, false
	}

	fs := flag.NewFlagSet(f.Name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	f.Build(fs) // registers the flags; the returned generator is discarded

	var out []FlagInfo
	fs.VisitAll(func(fl *flag.Flag) {
		out = append(out, FlagInfo{
			Name:    fl.Name,
			Default: fl.DefValue,
			Usage:   fl.Usage,
		})
	})
	return out, true
}

// FlagInfo describes a single flag a feature accepts.
type FlagInfo struct {
	Name    string `json:"name"`
	Default string `json:"default"`
	Usage   string `json:"usage"`
}

// ─── Configuration ───────────────────────────────────────────────────────────
//
// ProjectConfig, Defaults and Validate were already exported before this move,
// so they need nothing here. What was missing is the ability to apply a
// configuration to a directory without going through argv.

// ApplyConfig applies every feature a configuration enables to the project
// rooted at dir, in priority order, and reports how many were applied.
//
// This is the same path `breeze new` takes when a breeze.yaml is present. Note
// that it changes the working directory for the duration of the call: the
// feature generators resolve features_generated.go relative to the process
// working directory, so applying them from the parent would write the blocks
// beside the project rather than into it. That makes this call unsafe to run
// concurrently with anything else that depends on the working directory.
func ApplyConfig(dir string, cfg ProjectConfig, modulePath string) (int, error) {
	return applyConfigInProject(dir, cfg, modulePath)
}

// ConfigFeatureNames returns the features a configuration enables, in the order
// they must be applied.
func ConfigFeatureNames(cfg ProjectConfig) []string { return configFeatureNames(cfg) }

// UnsupportedConfigKeys returns human-readable notes for settings that parse and
// validate but reach no generator — a key that changes nothing is worse than one
// that errors, because nothing tells the user.
func UnsupportedConfigKeys(cfg ProjectConfig) []string { return unsupportedConfigKeys(cfg) }

// ResolveConfigPath returns the configuration file to read: the explicit path if
// given, else breeze.yaml when it exists, else "". A named file that is missing
// is an error rather than a silent fallback to defaults.
func ResolveConfigPath(explicit string) (string, error) { return resolveConfigPath(explicit) }

// DefaultConfigFile is the file name looked for when none is named.
const DefaultConfigFile = defaultConfigFile
