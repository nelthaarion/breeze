package mcp

// tools_generate.go — the tools that write files.
//
// Each one is a thin adapter: turn a JSON argument object into the argv the
// generator already accepts, run it with stdout captured, and report what it
// printed. The generators are not reimplemented and their behaviour is not
// second-guessed — in particular, whether a given generator refuses to overwrite
// an existing file is its decision, and this layer reports that decision rather
// than pre-empting it.
//
// The argv-building is the only real work, and it is worth being careful about:
// a flag misspelled here reaches the generator as an invalid argument, so a tool
// call that should have generated files becomes a failed result.

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nelthaarion/breeze/internal/generator"
)

func registerGeneratorTools(s *Server) {
	s.addTool(newProjectTool())
	s.addTool(generateTool())
	s.addTool(addFeatureTool())
}

// ─── breeze_new ──────────────────────────────────────────────────────────────

type newProjectArgs struct {
	Name     string `json:"name"`
	Template string `json:"template"`
	Module   string `json:"module"`
	Dir      string `json:"dir"`
}

func newProjectTool() *tool {
	return &tool{
		name: "breeze_new",
		description: "Scaffold a new Breeze project. Creates a directory containing " +
			"a runnable main.go, go.mod, and the layout for the chosen template.",
		schema: objectSchema(map[string]any{
			"name":     stringProp("Project directory name, e.g. myapp."),
			"template": map[string]any{"type": "string", "enum": []string{"api", "views"}, "description": "api for a JSON service, views for server-rendered HTML. Defaults to api."},
			"module":   stringProp("Go module path, e.g. example.com/myapp. Defaults to the project name."),
			"dir":      stringProp("Parent directory to create the project in. Defaults to the server's working directory."),
		}, "name"),
		run: func(raw json.RawMessage) toolCallResult {
			var a newProjectArgs
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}
			if a.Name == "" {
				return errorResult("name is required")
			}

			argv := []string{a.Name}
			if a.Template != "" {
				argv = append(argv, "--template="+a.Template)
			}
			if a.Module != "" {
				argv = append(argv, "--module="+a.Module)
			}

			return runGenerator(a.Dir, func() error { return generator.New(argv) })
		},
	}
}

// ─── breeze_generate ─────────────────────────────────────────────────────────

type generateArgs struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Fields carries the trailing name:type pairs that model and resource take.
	// It is a list rather than a single string because splitting a string here
	// would mean guessing a separator, and a field type can legitimately contain
	// one.
	Fields []string `json:"fields"`
	Steps  []string `json:"steps"`
	// Flags carries kind-specific options such as path, force, retry, every,
	// and package. Keeping this open-ended means this adapter does not duplicate
	// every generator's FlagSet; the generator remains the authority and rejects
	// a flag the selected kind does not accept.
	Flags map[string]any `json:"flags"`
	Dir   string         `json:"dir"`
}

func generateTool() *tool {
	return &tool{
		name: "breeze_generate",
		description: "Generate code into an existing Breeze project: handler, resource, model, " +
			"event, listener, workflow, middleware, ws, view, job, or grpc. " +
			"Run breeze_generate with kind=model and fields for a struct plus its migration pair.",
		schema: objectSchema(map[string]any{
			"kind": map[string]any{
				"type": "string",
				"enum": []string{"handler", "resource", "model", "event", "listener",
					"workflow", "middleware", "ws", "view", "job", "grpc"},
				"description": "What to generate.",
			},
			"name":   stringProp("Name of the thing to generate, e.g. User. Required for every kind except where the generator says otherwise."),
			"fields": stringsProp(`For model and resource: field specs as "name:type", e.g. ["name:string","age:int"].`),
			"steps":  stringsProp("For workflow: step names, e.g. [\"validate\",\"create\"]."),
			"flags": map[string]any{
				"type":        "object",
				"description": `Kind-specific generator flags, e.g. {"path":"/healthz","force":true}, {"retry":true}, or {"every":"5m"}. Booleans become --flag; other values become --flag=value.`,
			},
			"dir": stringProp("Project root. Defaults to the server's working directory."),
		}, "kind"),
		run: func(raw json.RawMessage) toolCallResult {
			var a generateArgs
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}
			if a.Kind == "" {
				return errorResult("kind is required")
			}

			argv := []string{a.Kind}
			if a.Name != "" {
				argv = append(argv, a.Name)
			}
			if len(a.Steps) > 0 {
				argv = append(argv, "--steps="+strings.Join(a.Steps, ","))
			}
			argv = append(argv, flagsToArgv(a.Flags)...)
			// Fields go last: they are positional, and the generator reads
			// everything after the name as field specs.
			argv = append(argv, a.Fields...)

			return runGenerator(a.Dir, func() error { return generator.Generate(argv) })
		},
	}
}

// ─── breeze_add ──────────────────────────────────────────────────────────────

type addFeatureArgs struct {
	Feature string `json:"feature"`
	// Flags is a free-form map because each feature defines its own, and
	// enumerating all of them here would duplicate the registry and go stale.
	// breeze_features reports the real set.
	Flags map[string]any `json:"flags"`
	Dir   string         `json:"dir"`
}

func addFeatureTool() *tool {
	return &tool{
		name: "breeze_add",
		description: "Wire a framework feature into an existing project (events, dashboard, jwt, " +
			"cors, observability, and others). Call breeze_features first to see the " +
			"available features and their flags.",
		schema: objectSchema(map[string]any{
			"feature": stringProp("Feature name, e.g. dashboard. Use breeze_features for the full list."),
			"flags": map[string]any{
				"type":        "object",
				"description": `Feature-specific flags, e.g. {"allow-writes":true} or {"driver":"postgres"}. Booleans become --flag, other values become --flag=value.`,
			},
			"dir": stringProp("Project root. Defaults to the server's working directory."),
		}, "feature"),
		run: func(raw json.RawMessage) toolCallResult {
			var a addFeatureArgs
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}
			if a.Feature == "" {
				return errorResult("feature is required")
			}

			// Reject an unknown feature here rather than passing it through.
			// The generator would also reject it, but this way the reply can
			// name the closest real feature, and a model that guessed "auth"
			// learns that the feature is called jwt.
			canonical, ok := knownFeature(a.Feature)
			if !ok {
				return errorResult(fmt.Sprintf("unknown feature %q; available: %s",
					a.Feature, strings.Join(generator.FeatureNames(), ", ")))
			}

			argv := append([]string{canonical}, flagsToArgv(a.Flags)...)

			return runGenerator(a.Dir, func() error { return generator.Add(argv) })
		},
	}
}

// flagsToArgv converts a flag map to command-line form.
//
// A false boolean is omitted rather than passed as --flag=false. Every flag in
// the generator defaults to false, so omitting is equivalent, and it keeps the
// echoed command line honest about what was actually asked for.
func flagsToArgv(flags map[string]any) []string {
	if len(flags) == 0 {
		return nil
	}

	argv := make([]string, 0, len(flags))
	for k, v := range flags {
		switch val := v.(type) {
		case bool:
			if val {
				argv = append(argv, "--"+k)
			}
		case nil:
			// A null value means "mentioned but unset", which is not a request
			// for anything. Passing --flag= would set it to the empty string.
		default:
			argv = append(argv, fmt.Sprintf("--%s=%v", k, val))
		}
	}
	return argv
}

// runGenerator runs one generator call and shapes its output into a result.
//
// The output is returned on failure as well as on success, and that is
// deliberate: the generators print what they did as they go, so when one fails
// half-way the printed lines are the record of what already exists on disk. A
// caller that only saw the error would not know whether to retry or clean up.
func runGenerator(dir string, fn func() error) toolCallResult {
	var (
		out    string
		runErr error
	)

	// captureStdout takes the lock that also guards chdir, so runInDir goes
	// inside it rather than around it.
	out, capErr := captureStdout(func() error {
		return runInDir(dir, func() error {
			runErr = fn()
			return nil
		})
	})
	if capErr != nil {
		return errorResult(capErr.Error())
	}

	out = strings.TrimSpace(out)

	if runErr != nil {
		if out == "" {
			return errorResult(runErr.Error())
		}
		return errorResult(out + "\n\nfailed: " + runErr.Error())
	}

	if out == "" {
		// Success with nothing printed. Saying so is better than an empty
		// result, which reads as a broken tool.
		return textResult("done (the generator reported no output)")
	}
	return textResult(out)
}
