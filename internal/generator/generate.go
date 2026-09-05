package generator

// `breeze generate` â€” scaffold code you are then expected to edit.
//
// The dispatch is table-driven rather than a switch so that the unknown-kind
// error can enumerate what actually exists (it used to hardcode "handler,
// resource, or grpc" and went stale the moment a generator was added), and so
// that name validation is a per-generator property. The old version required an
// exported identifier as args[1] before dispatching at all, which made a
// name-less generator impossible to add.

import (
	"errors"
	"flag"
	"fmt"
	"go/token"
	"io"
	"os"
	"sort"
	"strings"
)

// generator is one `breeze generate` kind.
type generator struct {
	Name    string
	Args    string // argument summary for help, e.g. "<Name> [field:type ...]"
	Summary string
	// NeedsName requires a leading positional that is an exported Go
	// identifier â€” true for everything that names a type or a handler group.
	NeedsName bool
	Run       func(modulePath, name string, args []string) error
}

var generators = map[string]*generator{}

func registerGenerator(g *generator) { generators[g.Name] = g }

func generatorNames() []string {
	names := make([]string, 0, len(generators))
	for n := range generators {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func init() {
	registerGenerator(&generator{
		Name:      "handler",
		Args:      "<Name> [flags]",
		NeedsName: true,
		Summary:   "route group with CRUD handler stubs",
		Run:       func(mod, name string, args []string) error { return generateHandler(mod, name, args) },
	})
	registerGenerator(&generator{
		Name:      "resource",
		Args:      "<Name> field:type[:rules] ...",
		NeedsName: true,
		Summary:   "handler + struct + OpenAPI docs + validation",
		Run:       func(mod, name string, args []string) error { return generateResource(mod, name, args) },
	})
	registerGenerator(&generator{
		Name:      "grpc",
		Args:      "<InterfaceName> [flags]",
		NeedsName: true,
		Summary:   "gRPC service skeleton",
		Run:       func(mod, name string, args []string) error { return generateGRPC(mod, name, args) },
	})
	registerGenerator(&generator{
		Name: "model", Args: "<Name> field:type [field:type ...]", NeedsName: true,
		Summary: "struct and a matching migration pair",
		Run:     generateModel,
	})
	registerGenerator(&generator{
		Name: "event", Args: "<Name> [field:type ...]", NeedsName: true,
		Summary: "event type with emit and subscribe helpers",
		Run:     generateEvent,
	})
	registerGenerator(&generator{
		Name: "listener", Args: "<EventName> [flags]", NeedsName: true,
		Summary: "subscriber for an existing event",
		Run:     generateListener,
	})
	registerGenerator(&generator{
		Name: "workflow", Args: "<Name> --steps=a,b,c", NeedsName: true,
		Summary: "workflow definition with steps, retries and compensation",
		Run:     generateWorkflow,
	})
	registerGenerator(&generator{
		Name: "middleware", Args: "<Name>", NeedsName: true,
		Summary: "breeze.HandlerFunc middleware stub",
		Run:     generateMiddleware,
	})
	registerGenerator(&generator{
		Name: "ws", Args: "<Name> [--path=/ws]", NeedsName: true,
		Summary: "WebSocket handler with connect/message/close hooks",
		Run:     generateWS,
	})
	registerGenerator(&generator{
		Name: "view", Args: "<Name> [--path=/name]", NeedsName: true,
		Summary: "HTML view and the route that renders it",
		Run:     generateView,
	})
	registerGenerator(&generator{
		Name: "job", Args: "<Name> [--every=1m]", NeedsName: true,
		Summary: "background job, registered with the dashboard",
		Run:     generateJob,
	})
}

func runGenerate(args []string) error {
	if len(args) == 0 {
		printGenerateHelp(os.Stderr)
		return errors.New("no generator given")
	}

	kind := args[0]
	switch kind {
	case "-h", "--help", "help":
		printGenerateHelp(os.Stdout)
		return nil
	}

	g, ok := generators[kind]
	if !ok {
		return fmt.Errorf("unknown generator %q â€” must be one of: %s",
			kind, strings.Join(generatorNames(), ", "))
	}

	rest := args[1:]
	name := ""
	if g.NeedsName {
		// The name is the first positional token. Scanning for it rather than
		// taking rest[0] means `generate handler --force Session` works as well
		// as `generate handler Session --force`.
		idx := -1
		for i, a := range rest {
			if !strings.HasPrefix(a, "-") {
				idx = i
				break
			}
		}
		if idx == -1 {
			return fmt.Errorf("usage: breeze generate %s %s", g.Name, g.Args)
		}
		name = rest[idx]
		if !token.IsIdentifier(name) || !strings.HasPrefix(name, strings.ToUpper(name[:1])) {
			return fmt.Errorf(
				"invalid name %q â€” must be an exported Go identifier (e.g. User)",
				name,
			)
		}
		rest = append(append([]string{}, rest[:idx]...), rest[idx+1:]...)
	}

	modulePath, err := currentModulePath()
	if err != nil {
		return err
	}
	return g.Run(modulePath, name, rest)
}

func printGenerateHelp(w io.Writer) {
	fmt.Fprint(w, "Usage: breeze generate <kind> [Name] [args...]   (alias: breeze g)\n\nKinds:\n")
	for _, name := range generatorNames() {
		g := generators[name]
		fmt.Fprintf(w, "  %-11s %-34s %s\n", g.Name, g.Args, g.Summary)
	}
	fmt.Fprint(w, `
Field types:
  string  int  int64  uint  uint64  float32  float64  bool
  []string  time.Time  time.Duration

Validation (resource only):
  A third segment becomes the validate:"..." tag that binding.Bind enforces,
  so the generated handler answers 422 with an RFC 9457 body instead of
  storing whatever arrived:

    name:string:required,min=2,max=40
    role:string:required,oneof=admin editor viewer

  Rules:      required, email, min=N, max=N, oneof=a b c
  Inferred:   a string field given no rules gets "required", and one named
              like an email gets "required,email". Numbers and bools get
              nothing â€” "required" means non-zero to the validator, so it
              would reject age=0 and active=false.
  --no-validate turns inference off. Explicit rules always win over it.

  model and event reject a rules segment rather than dropping it: nothing
  binds those structs from a request, so the tag would never run.

Flags common to most kinds:
  --force    overwrite an existing file
  --plural   override the pluralized form used for paths and list handlers

Output overrides (every kind that writes a Go file):
  --filename  the file to write, within that kind's directory
              (default: derived from the name, e.g. user_model.go)
  --package   the package clause of the generated file
              (default: the name of the directory it lands in)

  Both are validated before anything is written: --package must be a legal Go
  identifier and must agree with the other files in that directory, and
  --filename is refused if it names a file another generator owns.

Examples:
  breeze generate resource User name:string email:string age:int
  breeze generate resource User name:string:required,min=2 role:string:oneof=admin viewer
  breeze generate handler Session --methods=list,create --path=/auth/sessions
  breeze generate model Order total:float64 placed_at:time.Time
  breeze generate model User name:string --filename user_model.go --package models
  breeze generate event UserCreated id:int64 email:string
  breeze generate listener UserCreated --name=SendWelcomeEmail
  breeze generate workflow Signup --steps=validate,create,notify --retry
  breeze generate ws Chat --path=/chat
  breeze generate view About --path=/about
  breeze generate job CleanupSessions --every=1h

To wire an existing framework feature instead of scaffolding code, see
breeze add --list.
`)
}

// action describes a single generated CRUD operation shared by both the
// handler and resource generators.
type action struct {
	Name       string // "list", "get", "create", "update", "delete"
	Method     string // breeze.GET, breeze.POST, ...
	PathSuffix string // "" or "/:id"
	FuncName   string
}

var allActions = []string{"list", "get", "create", "update", "delete"}

func actionsFor(name, plural string, requested []string) ([]action, error) {
	if len(requested) == 0 {
		requested = allActions
	}

	valid := make(map[string]bool, len(allActions))
	for _, a := range allActions {
		valid[a] = true
	}

	actions := make([]action, 0, len(requested))
	for _, r := range requested {
		r = strings.ToLower(strings.TrimSpace(r))
		if !valid[r] {
			return nil, fmt.Errorf(
				"unknown method %q â€” must be one of: %s",
				r,
				strings.Join(allActions, ", "),
			)
		}
		switch r {
		case "list":
			actions = append(
				actions,
				action{Name: r, Method: "breeze.GET", PathSuffix: "", FuncName: "List" + plural},
			)
		case "get":
			actions = append(
				actions,
				action{Name: r, Method: "breeze.GET", PathSuffix: "/:id", FuncName: "Get" + name},
			)
		case "create":
			actions = append(
				actions,
				action{Name: r, Method: "breeze.POST", PathSuffix: "", FuncName: "Create" + name},
			)
		case "update":
			actions = append(
				actions,
				action{
					Name:       r,
					Method:     "breeze.PUT",
					PathSuffix: "/:id",
					FuncName:   "Update" + name,
				},
			)
		case "delete":
			actions = append(
				actions,
				action{
					Name:       r,
					Method:     "breeze.DELETE",
					PathSuffix: "/:id",
					FuncName:   "Delete" + name,
				},
			)
		}
	}
	return actions, nil
}

// splitFlagsAndPositional separates flag tokens from positional arguments,
// regardless of order, so commands like `breeze generate resource User
// name:string --plural=people` work (the stdlib flag package alone stops
// parsing at the first positional token). Both "--name=value" and
// "--name value" forms are supported: when a token names a non-boolean flag
// registered on fs and carries no "=", the following token is consumed as
// its value. Unknown flag tokens are kept so fs.Parse reports them.
func splitFlagsAndPositional(fs *flag.FlagSet, args []string) (flagArgs, positional []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			positional = append(positional, a)
			continue
		}
		flagArgs = append(flagArgs, a)

		name := strings.TrimLeft(a, "-")
		if strings.Contains(name, "=") {
			continue
		}
		f := fs.Lookup(name)
		if f == nil {
			continue
		}
		if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
			continue
		}
		if i+1 < len(args) {
			i++
			flagArgs = append(flagArgs, args[i])
		}
	}
	return flagArgs, positional
}

// parseFlags parses args with fs, returning errors to the caller instead of
// exiting (flag.ExitOnError would bypass main's error handling). The
// FlagSet's own output is discarded â€” main prints the returned error â€” except
// for -h/--help, where the flag defaults are printed.
func parseFlags(fs *flag.FlagSet, args []string) error {
	fs.SetOutput(io.Discard)
	err := fs.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		fs.SetOutput(os.Stderr)
		fs.PrintDefaults()
	}
	return err
}

func parseMethodsFlag(fs *flag.FlagSet) *string {
	return fs.String(
		"methods",
		strings.Join(allActions, ","),
		"comma-separated actions: list,get,create,update,delete",
	)
}

func splitMethods(s string) []string {
	return splitList(s)
}
