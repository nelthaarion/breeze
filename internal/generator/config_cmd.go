package generator

// --config plumbing: the bridge between the configuration schema and the
// commands.
//
// The schema and its validation existed before anything called them, which meant
// `breeze.yaml` was a file the CLI could describe and not read. This is the part
// that makes it reachable.
//
// The design constraint is that --config must not become a second way to say
// everything. A command that already takes its inputs positionally keeps doing
// so; the config file supplies the settings that have nowhere else to live â€”
// the module path, and the feature configuration a project wants applied
// repeatably rather than typed once.

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// configFlagName is the flag, and defaultConfigFile the name looked for when it
// is absent.
const (
	configFlagName    = "--config"
	defaultConfigFile = "breeze.yaml"
)

// extractConfigFlag pulls --config out of an argument list, returning the path
// and the arguments with it removed.
//
// It is done by hand rather than through a FlagSet because every command builds
// its own FlagSet with its own flags, and --config has to be readable before
// that happens: whether the flag is even valid for a command depends on nothing
// else, and threading it through each command's parser would mean declaring it
// in six places.
//
// Both spellings are accepted, matching the flag package: --config=path and
// --config path.
func extractConfigFlag(args []string) (path string, rest []string, err error) {
	rest = make([]string, 0, len(args))

	for i := 0; i < len(args); i++ {
		arg := args[i]

		if arg == configFlagName {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				return "", nil, fmt.Errorf("%s needs a file path", configFlagName)
			}
			path = args[i+1]
			i++
			continue
		}
		if name, value, ok := strings.Cut(arg, "="); ok && name == configFlagName {
			if value == "" {
				return "", nil, fmt.Errorf("%s needs a file path", configFlagName)
			}
			path = value
			continue
		}
		rest = append(rest, arg)
	}
	return path, rest, nil
}

// resolveConfigPath decides which file to read.
//
// An explicit --config that does not exist is an error: the user named a file
// and is entitled to know it was not there, rather than having the command
// silently proceed on defaults. A breeze.yaml found in the working directory is
// used without being asked for, which is what makes the file worth keeping in a
// repository â€” but its absence is not an error, because most projects do not
// have one.
func resolveConfigPath(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("reading %s %s: %w", configFlagName, explicit, err)
		}
		return explicit, nil
	}
	if _, err := os.Stat(defaultConfigFile); err == nil {
		return defaultConfigFile, nil
	}
	return "", nil
}

// loadProjectConfig resolves the configuration for a command invocation.
//
// The returned []string is the arguments with --config and every --section.field
// flag removed, so the calling command sees only its own arguments and needs no
// knowledge that a config layer ran first.
func loadProjectConfig(args []string) (ProjectConfig, []string, error) {
	explicit, rest, err := extractConfigFlag(args)
	if err != nil {
		return Defaults(), nil, err
	}

	path, err := resolveConfigPath(explicit)
	if err != nil {
		return Defaults(), nil, err
	}

	cfg, rest, err := Load(path, rest)
	if err != nil {
		return cfg, nil, err
	}

	// Reported so a run that silently picked up a file in the working directory
	// says so. A generated project that differs from the flags on the command
	// line, for reasons in a file nobody mentioned, is a bad afternoon.
	if path != "" && explicit == "" {
		fmt.Fprintf(os.Stderr, "note: using %s from the current directory\n", path)
	}
	return cfg, rest, nil
}

// applyConfigFeatures adds every feature the configuration asks for.
//
// This is what makes a config file worth having over a shell script: the
// middleware list in YAML becomes the same feature blocks `breeze add` writes,
// through the same generators, so there is one implementation of each feature's
// wiring rather than one per input path.
func applyConfigFeatures(cfg ProjectConfig, modulePath string) error {
	for _, name := range configFeatureNames(cfg) {
		f, ok := features[name]
		if !ok {
			// Unreachable: Validate resolves every configured middleware name
			// against the registry before this runs.
			return fmt.Errorf("configuration names feature %q, which is not registered", name)
		}
		if err := addFeatureFromConfig(f, cfg, modulePath); err != nil {
			return fmt.Errorf("applying feature %s from configuration: %w", name, err)
		}
	}
	return nil
}

// configFeatureNames is the features a configuration enables, in registry
// priority order.
//
// Ordering by priority rather than by the order they appear in the file matters
// because a feature that composes with another reads whether it is installed at
// generation time: applying them out of order would leave a block wired to a
// fallback that the very next step makes wrong.
func configFeatureNames(cfg ProjectConfig) []string {
	wanted := map[string]bool{}

	for _, m := range cfg.Middleware {
		wanted[canonicalFeatureName(m.Name)] = true
	}
	// The typed sections enable their own features, so a project does not have
	// to name "websocket" twice to both switch it on and configure it.
	if cfg.WebSocket.Enabled {
		wanted["websocket"] = true
	}
	if cfg.JSONRPC.Enabled {
		wanted["jsonrpc"] = true
	}
	if cfg.Fleet.Enabled {
		wanted["fleet"] = true
	}
	if cfg.Docs.Enabled {
		wanted["docs"] = true
	}

	ordered := make([]string, 0, len(wanted))
	for _, name := range featureNames() {
		if wanted[name] {
			ordered = append(ordered, name)
		}
	}
	return ordered
}

// addFeatureFromConfig runs one feature's generator with flags derived from the
// configuration, then writes its block.
//
// The flags are handed to the feature's own FlagSet rather than to a parallel
// config-to-code path, which is the only way the two input routes can be
// guaranteed to produce the same block: there is exactly one generator per
// feature and it is this one.
func addFeatureFromConfig(f *feature, cfg ProjectConfig, modulePath string) error {
	fs := flag.NewFlagSet("config "+f.Name, flag.ContinueOnError)
	// The flags come from a file rather than a terminal, so a usage dump on
	// error would be addressed to the wrong audience; parseFlags reports the
	// problem itself.
	fs.SetOutput(io.Discard)
	generate := f.Build(fs)

	if err := parseFlags(fs, configFlagsFor(f.Name, cfg)); err != nil {
		return err
	}

	out, err := generate(featureCtx{
		ModulePath:       modulePath,
		HasEvents:        hasBlock(featuresFileName, featureMarkerPrefix, "events"),
		HasObservability: hasBlock(featuresFileName, featureMarkerPrefix, "observability"),
		HasDashboard:     hasBlock(featuresFileName, featureMarkerPrefix, "dashboard"),
	})
	if err != nil {
		return err
	}

	if _, _, err := writeFeatureFiles(out, false); err != nil {
		return err
	}
	if f.Standalone {
		return nil
	}
	// force: a config file is the declared state of the project, so applying it
	// is expected to overwrite what a previous apply generated. Without this a
	// second run would refuse every block whose settings had changed.
	if _, err := applyFeatureBlock(f, out, true); err != nil {
		return err
	}
	return rebuildFeatureCalls()
}

// configFlagsFor translates the configuration into the flag arguments a
// feature's own FlagSet accepts.
//
// Only the settings the schema actually carries are translated. A feature whose
// flags have no representation in ProjectConfig is applied with its defaults,
// which is the honest outcome: inventing a mapping here would mean a YAML key
// that appears to work and silently does nothing.
func configFlagsFor(name string, cfg ProjectConfig) []string {
	switch name {
	case "websocket":
		// Only --path. The feature's other flag is --broadcast, which relays
		// every message to every client; websocket.rooms asks for per-room
		// addressing, which is a different thing the generator does not emit.
		// Mapping one to the other would generate a project that answers a
		// question the config file did not ask. unsupportedConfigKeys reports
		// the gap instead.
		return []string{"--path=" + cfg.WebSocket.Path}

	case "jsonrpc":
		args := []string{fmt.Sprintf("--port=%d", cfg.JSONRPC.Port)}
		if len(cfg.JSONRPC.Methods) > 0 {
			args = append(args, "--methods="+strings.Join(cfg.JSONRPC.Methods, ","))
		}
		if len(cfg.JSONRPC.BlockingMethods) > 0 {
			args = append(args, "--blocking="+strings.Join(cfg.JSONRPC.BlockingMethods, ","))
		}
		if cfg.JSONRPC.MaxMessageBytes > 0 {
			args = append(args, fmt.Sprintf("--max-message-bytes=%d", cfg.JSONRPC.MaxMessageBytes))
		}
		return args

	case "fleet":
		args := []string{
			"--transport=" + cfg.Fleet.Transport,
			"--aggregator-url=" + cfg.Fleet.AggregatorURL,
			fmt.Sprintf("--sample-rate=%v", cfg.Fleet.SampleRate),
		}
		if cfg.Fleet.ServiceName != "" {
			args = append(args, "--service="+cfg.Fleet.ServiceName)
		}
		if cfg.Fleet.AggregatorWSURL != "" {
			args = append(args, "--aggregator-ws-url="+cfg.Fleet.AggregatorWSURL)
		}
		return args

	case "docs":
		// --ui-path and --json-path, not --path: the docs feature serves two
		// endpoints and names both.
		return []string{
			"--ui-path=" + cfg.Docs.UIPath,
			"--json-path=" + cfg.Docs.SpecPath,
			"--title=" + cfg.Docs.Title,
		}

	case "cors":
		if m, ok := middlewareByName(cfg, "cors"); ok && len(m.Origins) > 0 {
			return []string{"--origins=" + strings.Join(m.Origins, ",")}
		}

	case "ratelimit":
		if m, ok := middlewareByName(cfg, "ratelimit"); ok && m.RPS > 0 {
			return []string{fmt.Sprintf("--requests=%d", m.RPS), "--per=1s"}
		}
	}
	return nil
}

// unsupportedConfigKeys reports the settings this configuration specifies that
// no generator can express.
//
// The schema is broader than the generators, and validation cannot catch this
// class of problem: `websocket.rooms: true` is a valid boolean in a valid
// section, so it passes every check and then changes nothing about the emitted
// code. That is the worst kind of configuration bug â€” the file says one thing,
// the project does another, and nothing anywhere disagrees out loud.
//
// Listing them is the honest alternative to either silently dropping them or
// inventing a mapping to a flag that means something else.
func unsupportedConfigKeys(cfg ProjectConfig) []string {
	var unsupported []string

	if cfg.WebSocket.Enabled && cfg.WebSocket.Rooms {
		unsupported = append(
			unsupported,
			"websocket.rooms â€” the generated handler has no room registry; use breeze.Rooms in your own code",
		)
	}
	// middleware.secret is read by no generator, and that is deliberate: the
	// jwt feature takes --secret-env, the *name* of an environment variable, so
	// the signing key is never written into a source file. A literal secret in
	// YAML would be committed. Saying so matters more than the other entries
	// here â€” the user believes they have configured authentication.
	for _, m := range cfg.Middleware {
		if m.Secret != "" {
			unsupported = append(unsupported, fmt.Sprintf(
				"middleware.%s.secret â€” secrets are read from the environment, not generated into source; "+
					"set the variable named by the feature's --secret-env instead",
				m.Name,
			))
		}
	}

	if cfg.Fleet.Enabled && cfg.Fleet.Backend != "" && cfg.Fleet.Backend != "memory" {
		unsupported = append(
			unsupported,
			"fleet.backend â€” only the in-memory span store is generated; a persistent store is aggregator-side configuration",
		)
	}
	return unsupported
}

// reportUnsupportedConfigKeys prints those settings, if any.
func reportUnsupportedConfigKeys(cfg ProjectConfig) {
	unsupported := unsupportedConfigKeys(cfg)
	if len(unsupported) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "\n  ! These settings were read but have no effect on generated code:\n")
	for _, key := range unsupported {
		fmt.Fprintf(os.Stderr, "      %s\n", key)
	}
}

// middlewareByName finds a middleware entry by canonical feature name, so a
// hyphenated spelling in YAML matches the registry name the switch above uses.
func middlewareByName(cfg ProjectConfig, name string) (MiddlewareConfig, bool) {
	for _, m := range cfg.Middleware {
		if canonicalFeatureName(m.Name) == name {
			return m, true
		}
	}
	return MiddlewareConfig{}, false
}
