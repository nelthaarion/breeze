package generator

import (
	"flag"
	"fmt"
	"strings"
)

// generateMiddleware writes a breeze.HandlerFunc middleware stub.
//
// Deliberately not auto-wired. Two reasons, both real:
//
//   - Ordering is the decision that makes a middleware correct or broken, and
//     only the user knows where theirs belongs. Appending it blindly to the end
//     of the chain would put it inside auth and rate limiting, which is wrong
//     for most of what people write.
//   - A local package named `middleware` collides with the framework's
//     `middleware "github.com/nelthaarion/breeze/middlewares"` alias in any
//     generated file that imports both â€” and features_generated.go imports the
//     framework one for nine of its features.
//
// So this prints the exact router.Use line instead of writing it.
func generateMiddleware(modulePath, name string, args []string) error {
	fs := flag.NewFlagSet("generate middleware", flag.ContinueOnError)
	force := fs.Bool("force", false, "overwrite an existing middleware file")
	out := registerOutputFlags(fs)

	flagArgs, _ := splitFlagsAndPositional(fs, args)
	if err := parseFlags(fs, flagArgs); err != nil {
		return err
	}

	target, err := out.target("middleware", fileSlug(name))
	if err != nil {
		return err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "// %s returns a middleware that ... (describe the behaviour).\n//\n", name)
	b.WriteString("// The chain only advances when ctx.Next() is called, so returning without\n")
	b.WriteString("// calling it short-circuits the request â€” that is how a middleware rejects\n")
	b.WriteString("// one. Set the status and write the body first. ctx.Abort() does the same\n")
	b.WriteString("// thing explicitly and also stops any outer middleware from continuing.\n")
	b.WriteString("//\n")
	b.WriteString("// Returning ctx.Next()'s error propagates a downstream failure to the\n")
	b.WriteString("// framework's error handler. Discard it deliberately, with `_ =`, only if\n")
	b.WriteString("// this middleware is choosing to treat the failure as handled.\n")
	fmt.Fprintf(&b, "func %s() breeze.HandlerFunc {\n", name)
	b.WriteString("\treturn func(ctx *breeze.Context) error {\n")
	b.WriteString("\t\t// Runs before the handler. To reject the request:\n")
	b.WriteString("\t\t//\n")
	b.WriteString("\t\t//\tctx.Status(401)\n")
	b.WriteString("\t\t//\treturn ctx.JSON(...)\n")
	b.WriteString("\t\t//\n")
	b.WriteString("\t\t// or, to report the failure and let the error handler render it:\n")
	b.WriteString("\t\t//\n")
	b.WriteString("\t\t//\treturn breeze.NewHTTPError(401, \"unauthorized\")\n\n")
	b.WriteString("\t\tif err := ctx.Next(); err != nil {\n")
	b.WriteString("\t\t\treturn err\n")
	b.WriteString("\t\t}\n\n")
	b.WriteString("\t\t// Runs after the handler. The response body may already be\n")
	b.WriteString("\t\t// written by this point, so headers set here can be too late.\n")
	b.WriteString("\t\treturn nil\n")
	b.WriteString("\t}\n}\n")

	if err := writeGeneratedGoFile(generatedFile{
		Target:     target,
		Owner:      generateOwner("middleware"),
		Imports:    []string{`"github.com/nelthaarion/breeze"`},
		Body:       b.String(),
		ModulePath: modulePath,
		Force:      *force,
	}); err != nil {
		return err
	}

	printNotes([]string{
		"Not wired automatically — middleware order decides behaviour, so it is yours to place.",
		fmt.Sprintf("In main.go, add:  router.Use(middleware.%s())", name),
		fmt.Sprintf("Import as:        middleware \"%s/middleware\"", modulePath),
		"Earlier Use() calls run further outside; place auth after rate limiting, recovery first.",
	})
	return nil
}
