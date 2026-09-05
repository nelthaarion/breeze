package generator

import (
	"flag"
	"fmt"
	"strings"
	"text/template"
)

// handlerStubTemplate is the stub `breeze generate handler` writes.
//
// The `error` result and the trailing `return nil` are the new HandlerFunc signature.
// A stub without them does not compile at its registration site, which would make a
// freshly generated project broken on arrival — the one thing a scaffold must never be.
var handlerStubTemplate = template.Must(template.New("handler").Parse(`{{range .Actions}}
// {{.FuncName}} handles {{.Verb}} {{.Path}}.
func {{.FuncName}}(ctx *breeze.Context) error {
	// TODO: implement
	return nil
}
{{end}}`))

func generateHandler(modulePath, name string, args []string) error {
	fs := flag.NewFlagSet("generate handler", flag.ContinueOnError)
	methods := parseMethodsFlag(fs)
	force := fs.Bool("force", false, "overwrite an existing handler file")
	pluralOverride := fs.String("plural", "", "override the pluralized handler name (e.g. --plural=people)")
	pathOverride := fs.String("path", "", "route prefix (default /<plural>, e.g. --path=/api/v1/users)")
	out := registerOutputFlags(fs)

	flagArgs, _ := splitFlagsAndPositional(fs, args)
	if err := parseFlags(fs, flagArgs); err != nil {
		return err
	}

	plural := *pluralOverride
	if plural == "" {
		plural = pluralize(name)
	}
	pathBase, err := resolveRoutePath(*pathOverride, plural)
	if err != nil {
		return err
	}

	actions, err := actionsFor(name, plural, splitMethods(*methods))
	if err != nil {
		return err
	}

	// The handler file's default stem is the lowercased name rather than the
	// snake_case slug the other kinds use. That is the existing convention for
	// this kind and is preserved: changing it here would rename files in projects
	// that already have them.
	target, err := out.target("handlers", strings.ToLower(name))
	if err != nil {
		return err
	}

	if err := writeHandlerFile(target, modulePath, name, actions, pathBase, handlerStubTemplate, *force); err != nil {
		return err
	}

	return registerActionRoutes(modulePath, name, pathBase, actions, nil)
}

// resolveRoutePath validates an explicit --path, or derives the default from
// the pluralized name. A prefix that does not begin with "/" would silently
// produce routes nothing can reach, and a trailing "/" would produce "/users/"
// plus "/users//:id", so both are corrected rather than passed through.
func resolveRoutePath(explicit, plural string) (string, error) {
	if explicit == "" {
		return "/" + strings.ToLower(plural), nil
	}

	p := strings.TrimSpace(explicit)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if len(p) > 1 {
		p = strings.TrimRight(p, "/")
	}
	if p == "" {
		return "", fmt.Errorf("invalid --path %q", explicit)
	}
	if strings.ContainsAny(p, " \t") {
		return "", fmt.Errorf("invalid --path %q â€” route paths cannot contain whitespace", explicit)
	}
	return p, nil
}

type actionWithPath struct {
	action
	Path string
	Verb string
}

func writeHandlerFile(target outputTarget, modulePath, name string, actions []action, pathBase string,
	tmpl *template.Template, force bool) error {

	withPaths := make([]actionWithPath, len(actions))
	for i, a := range actions {
		withPaths[i] = actionWithPath{
			action: a,
			Path:   pathBase + a.PathSuffix,
			Verb:   strings.TrimPrefix(a.Method, "breeze."),
		}
	}

	var buf strings.Builder
	if err := tmpl.Execute(&buf, struct {
		Name    string
		Actions []actionWithPath
	}{Name: name, Actions: withPaths}); err != nil {
		return err
	}

	// The package clause and the import block are not in the template any more:
	// they are the writer's business, which is what lets --package change one and
	// the unused-import prune change the other. The template is now only the
	// declarations.
	return writeGeneratedGoFile(generatedFile{
		Target:     target,
		Owner:      generateOwner("handler"),
		Imports:    []string{`"github.com/nelthaarion/breeze"`},
		Body:       buf.String(),
		ModulePath: modulePath,
		Force:      force,
	})
}

// registerActionRoutes writes a routes_generated.go block registering one
// router.Handle call per action. When docArg is non-empty, it's appended as
// a trailing middleware argument (used by the resource generator to attach
// swagger.RouteDoc middleware); handler generation passes nil for plain
// routes with no docs.
func registerActionRoutes(modulePath, name, pathBase string, actions []action, docArgs []string, extraImports ...string) error {
	var body strings.Builder
	for i, a := range actions {
		path := pathBase + a.PathSuffix
		if docArgs != nil {
			fmt.Fprintf(&body, "router.Handle(%s, %q, handlers.%s,\n%s,\n)\n", a.Method, path, a.FuncName, docArgs[i])
		} else {
			fmt.Fprintf(&body, "router.Handle(%s, %q, handlers.%s)\n", a.Method, path, a.FuncName)
		}
	}
	return upsertRouteBlock(modulePath, name, body.String(), extraImports...)
}
