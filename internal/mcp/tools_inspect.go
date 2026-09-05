package mcp

// tools_inspect.go — the read-only tools.
//
// These matter more than they look. A model driving the generation tools has to
// guess otherwise: which features exist, what flags they take, what routes a
// project already declares. A guessed feature name is a failed call, and a guessed
// flag is silently ignored by flag.Parse — so the cheapest way to make the
// generation tools reliable is to make the facts available.
//
// Nothing here writes to disk.

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nelthaarion/breeze/v2/internal/generator"
)

func registerIntrospectionTools(s *Server) {
	s.addTool(featuresTool())
	s.addTool(routesTool())
}

// ─── breeze_features ─────────────────────────────────────────────────────────

type featuresArgs struct {
	// Feature narrows the report to one entry, with its flags. Without it the
	// reply is the whole catalogue, which is long; a model that already knows
	// the name should be able to ask about just that one.
	Feature string `json:"feature"`
}

func featuresTool() *tool {
	return &tool{
		name: "breeze_features",
		description: "List the framework features breeze_add can wire in, with a one-line " +
			"summary each. Pass feature to get that feature's flags. " +
			"Read this before calling breeze_add so the feature name and flags are real.",
		schema: objectSchema(map[string]any{
			"feature": stringProp(
				"Optional. A single feature to describe in detail, including its flags.",
			),
		}),
		run: func(raw json.RawMessage) toolCallResult {
			var a featuresArgs
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}

			if a.Feature != "" {
				return describeFeature(a.Feature)
			}
			return listFeatures()
		},
	}
}

// listFeatures reports every feature with its summary.
func listFeatures() toolCallResult {
	names := generator.FeatureNames()
	if len(names) == 0 {
		// Not reachable with the current registry, but an empty catalogue
		// reported as an empty string would look like a broken tool.
		return errorResult("no features are registered")
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d features available to breeze_add:\n\n", len(names))

	for _, name := range names {
		summary, ok := generator.FeatureSummary(name)
		if !ok {
			summary = "(no summary)"
		}
		fmt.Fprintf(&b, "  %-14s %s\n", name, summary)
	}

	b.WriteString("\nCall breeze_features with a feature name for its flags.")
	return textResult(b.String())
}

// describeFeature reports one feature and its flags.
func describeFeature(name string) toolCallResult {
	canonical, ok := knownFeature(name)
	if !ok {
		return errorResult(fmt.Sprintf("unknown feature %q; available: %s",
			name, strings.Join(generator.FeatureNames(), ", ")))
	}

	var b strings.Builder

	summary, _ := generator.FeatureSummary(canonical)
	fmt.Fprintf(&b, "%s — %s\n", canonical, summary)

	// The canonical name is surfaced when it differs from what was asked, so a
	// caller that used an alias learns the real name instead of silently
	// depending on the alias surviving.
	if canonical != name {
		fmt.Fprintf(&b, "\n(%q is an alias for %q.)\n", name, canonical)
	}

	flags, ok := generator.FeatureFlags(canonical)
	if !ok || len(flags) == 0 {
		b.WriteString("\nThis feature takes no flags.")
		return textResult(b.String())
	}

	b.WriteString("\nFlags:\n")
	for _, f := range flags {
		fmt.Fprintf(&b, "  --%-18s %s\n", f.Name, f.Usage)
	}

	fmt.Fprintf(&b, "\nExample: breeze_add with feature=%q and flags={\"%s\":true}\n",
		canonical, flags[0].Name)

	return textResult(b.String())
}

// knownFeature resolves aliases and validates the result.
//
// generator.CanonicalFeatureName intentionally leaves unknown names untouched:
// it is a resolver, not a validator. Checking the summary is the registry lookup
// that tells those two cases apart.
func knownFeature(name string) (string, bool) {
	canonical := generator.CanonicalFeatureName(name)
	_, ok := generator.FeatureSummary(canonical)
	return canonical, ok
}

// ─── breeze_routes ───────────────────────────────────────────────────────────

type routesArgs struct {
	Dir  string `json:"dir"`
	JSON bool   `json:"json"`
}

func routesTool() *tool {
	return &tool{
		name: "breeze_routes",
		description: "List the routes a Breeze project declares, read from its generated " +
			"route registry. Use this to see what already exists before generating " +
			"a handler that would collide with it.",
		schema: objectSchema(map[string]any{
			"dir": stringProp("Project root. Defaults to the server's working directory."),
			"json": boolProp(
				"Return JSON instead of a table. Prefer this when the output is going to be parsed.",
			),
		}),
		run: func(raw json.RawMessage) toolCallResult {
			var a routesArgs
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}

			argv := []string{}
			if a.JSON {
				argv = append(argv, "--json")
			}

			res := runGenerator(a.Dir, func() error { return generator.Routes(argv) })

			// A project with no registry yet is the common case for a fresh
			// scaffold, and "no such file" is a poor answer to "what routes do I
			// have". The generator's own message is kept, with the reason added.
			if res.IsError &&
				strings.Contains(strings.ToLower(res.Content[0].Text), "no such file") {
				return errorResult(res.Content[0].Text +
					"\n\nThis project has no generated route registry yet. " +
					"Generate a handler or resource first, or check that dir points at the project root.")
			}
			return res
		},
	}
}
