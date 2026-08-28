package generator

import (
	"flag"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

var handlerStubTemplate = template.Must(template.New("handler").Parse(`package handlers

import "github.com/nelthaarion/breeze"
{{range .Actions}}
// {{.FuncName}} handles {{.Verb}} {{.Path}}.
func {{.FuncName}}(ctx *breeze.Context) {
	// TODO: implement
}
{{end}}`))

func generateHandler(modulePath, name string, args []string) error {
	fs := flag.NewFlagSet("generate handler", flag.ContinueOnError)
	methods := parseMethodsFlag(fs)
	force := fs.Bool("force", false, "overwrite an existing handler file")
	pluralOverride := fs.String("plural", "", "override the pluralized handler name (e.g. --plural=people)")
	pathOverride := fs.String("path", "", "route prefix (default /<plural>, e.g. --path=/api/v1/users)")

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

	if err := writeHandlerFile(name, actions, pathBase, handlerStubTemplate, *force); err != nil {
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

func writeHandlerFile(name string, actions []action, pathBase string, tmpl *template.Template, force bool) error {
	if err := os.MkdirAll("handlers", 0o755); err != nil {
		return err
	}

	path := filepath.Join("handlers", strings.ToLower(name)+".go")
	if _, err := os.Stat(path); err == nil && !force {
		return fmt.Errorf("%s already exists â€” pass --force to overwrite", path)
	}

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

	formatted, err := format.Source([]byte(buf.String()))
	if err != nil {
		return fmt.Errorf("formatting %s: %w", path, err)
	}

	return os.WriteFile(path, formatted, 0o644)
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
