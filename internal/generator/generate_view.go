package generator

import (
	"flag"
	"fmt"
	"path/filepath"
	"strings"
)

// generateView writes a view template and the router.View call that serves it.
//
// The route goes into features_generated.go, not routes_generated.go, and that
// is load-bearing rather than a preference: router.View captures the engine
// pointer at registration time, and `Templates` is nil until setupTemplates
// runs. Registering from the routes file would capture the nil and panic on the
// first request if RegisterGeneratedRoutes happened to be called first.
// Unregistered feature names sort last, which puts this after templates
// (priority 160) unconditionally.
func generateView(modulePath, name string, args []string) error {
	fs := flag.NewFlagSet("generate view", flag.ContinueOnError)
	pathOverride := fs.String("path", "", "route path (default /<name>)")
	viewsDir := fs.String("views", "views", "directory holding the view templates")
	data := fs.Bool("data", false, "generate a per-request data function instead of passing nil")
	force := fs.Bool("force", false, "overwrite an existing view file")
	out := registerOutputFlags(fs)

	flagArgs, _ := splitFlagsAndPositional(fs, args)
	if err := parseFlags(fs, flagArgs); err != nil {
		return err
	}

	// Registered and refused for the same reason as `generate grpc`: an
	// unrecognised flag would report "flag provided but not defined", which reads
	// as though the flags do not exist anywhere. This kind writes one HTML
	// template and a block inside features_generated.go, so it produces no Go
	// file of its own for either override to apply to. --views moves the template.
	if out.set() {
		return fmt.Errorf("`breeze generate view` does not accept --%s or --%s: it writes an HTML "+
			"template and a block inside %s, and neither is a standalone Go file\n"+
			"       (use --views to change the template's directory)",
			outputFilenameFlag, outputPackageFlag, featuresFileName)
	}

	// Templates is declared by the templates feature block. Without it this
	// generates a file referencing an undeclared identifier, so refuse early
	// with the command that fixes it.
	//
	// A `new --template=views` scaffold builds its engine as a local in main(),
	// which no generated block can reach â€” package scope is the requirement, not
	// merely having an engine. Adding the block on top of that scaffold is
	// supported, but its router.View("/", ...) shadows the scaffold's, so the
	// message says so rather than letting the surprise happen at runtime.
	if !hasBlock(featuresFileName, featureMarkerPrefix, "templates") {
		return fmt.Errorf("no package-scope template engine in this project â€” run `breeze add templates` first\n"+
			"       (it declares the `Templates` var that serving a generated view requires)\n"+
			"       If you scaffolded with --template=views, its engine is a local in main()\n"+
			"       and unreachable from %s; adding the block will also take over \"/\"", featuresFileName)
	}

	viewName := strings.ToLower(name)
	routePath, err := resolveRoutePath(*pathOverride, viewName)
	if err != nil {
		return err
	}

	// --views names a directory inside the project and is written to. Checked for
	// the same reason the templates feature's copy of this flag is: breeze_generate
	// forwards a caller's flags object to this FlagSet verbatim, so
	// {"flags": {"views": "../../etc"}} arrives here as a place to write an HTML file.
	if err := validatePathFlag("views", *viewsDir); err != nil {
		return err
	}

	dir := strings.TrimPrefix(strings.TrimPrefix(*viewsDir, "./"), "/")
	if dir == "" {
		return fmt.Errorf("invalid --views %q", *viewsDir)
	}

	// The view file defines the "content" block the layout renders. The name is
	// fixed by the layout, not by the view, so every view defines "content".
	var h strings.Builder
	h.WriteString("{{define \"content\"}}\n")
	fmt.Fprintf(&h, "<div class=\"%s\">\n", viewName)
	fmt.Fprintf(&h, "  <h1>%s</h1>\n", name)
	if *data {
		fmt.Fprintf(&h, "  <p>Rendered from %s/%s.html.</p>\n", dir, viewName)
		h.WriteString("\n  <!-- Fields come from the data function in features_generated.go. -->\n")
		h.WriteString("  <p>{{.Message}}</p>\n")
	} else {
		fmt.Fprintf(&h, "  <p>Rendered from %s/%s.html inside %s/layout.html.</p>\n", dir, viewName, dir)
		h.WriteString("\n  <!-- Pass per-request data by regenerating with --data. -->\n")
	}
	h.WriteString("</div>\n\n")
	h.WriteString("<style>\n")
	fmt.Fprintf(&h, "  .%s h1 { font-size: 2rem; margin-bottom: .5rem; }\n", viewName)
	fmt.Fprintf(&h, "  .%s > p { color: #666; }\n", viewName)
	h.WriteString("</style>\n")
	h.WriteString("{{end}}\n")

	htmlPath := filepath.Join(dir, viewName+".html")
	if err := writeGeneratedTextFile(htmlPath, h.String(), *force); err != nil {
		return err
	}

	lower := lowerFirst(name)

	var body strings.Builder
	if *data {
		fmt.Fprintf(&body, "// %sData builds the data %s.html renders. It runs on every request.\n", lower, viewName)
		fmt.Fprintf(&body, "func %sData(ctx *breeze.Context) any {\n", lower)
		b := "\treturn map[string]any{\n\t\t\"Message\": \"Edit " + lower + "Data to change this.\",\n\t}\n}\n\n"
		body.WriteString(b)
	}
	fmt.Fprintf(&body, "// %s serves %s from %s.\n", featureSetupFunc("view"+name), routePath, htmlPath)
	fmt.Fprintf(&body, "func %s(app *breeze.Breeze, router *breeze.Router) {\n", featureSetupFunc("view"+name))
	if *data {
		fmt.Fprintf(&body, "\trouter.View(%q, Templates, %q, %sData)\n", routePath, viewName, lower)
	} else {
		fmt.Fprintf(&body, "\trouter.View(%q, Templates, %q, nil)\n", routePath, viewName)
	}
	body.WriteString("}\n")

	if err := upsertGeneratedFeature("view"+name, body.String(), []string{
		`"github.com/nelthaarion/breeze"`,
	}); err != nil {
		return err
	}

	notes := []string{
		fmt.Sprintf("Serving on:   %s", routePath),
		fmt.Sprintf("Edit:         %s", htmlPath),
	}
	if *data {
		notes = append(notes, fmt.Sprintf("Data:         %sData in %s", lower, featuresFileName))
	}
	notes = append(notes, "Preload parses every view at boot, so a template error is a boot failure with a line number.")
	printNotes(notes)
	return nil
}
