package generator

// `breeze routes` â€” list the project's generated routes.
//
// It works by parsing routes_generated.go rather than by building and starting
// the app. That is a deliberate trade: it means the command works on a project
// that does not currently compile â€” which is when you most want to see what is
// registered â€” at the cost of only knowing about routes in that file. Routes
// added by hand in main.go, and the ones features mount at runtime (the
// dashboard's pages, the video prefix, the docs UI), are invisible here, so the
// output says so rather than implying it is the whole routing table.

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
)

type routeEntry struct {
	Block      string `json:"block"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	Handler    string `json:"handler"`
	Blocking   bool   `json:"blocking"`
	Middleware int    `json:"middleware"`
}

func runRoutes(args []string) error {
	fs := flag.NewFlagSet("routes", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "emit JSON instead of a table")
	file := fs.String("file", registryFileName, "generated routes file to parse")

	flagArgs, positional := splitFlagsAndPositional(fs, args)
	if len(positional) > 0 {
		return fmt.Errorf("`breeze routes` takes no positional arguments, got %q", positional[0])
	}
	if err := parseFlags(fs, flagArgs); err != nil {
		return err
	}

	routes, err := parseRoutes(*file)
	if err != nil {
		return err
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(routes)
	}

	printRouteTable(routes, *file)
	return nil
}

// parseRoutes extracts every route registration from fileName.
func parseRoutes(fileName string) ([]routeEntry, error) {
	src, err := os.ReadFile(fileName)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%s not found â€” run this from a project root, after `breeze generate`", fileName)
		}
		return nil, err
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, fileName, src, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", fileName, err)
	}

	blocks := markerRanges(fset, f, routeMarkerPrefix)

	var routes []routeEntry
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}

		line := fset.Position(call.Pos()).Line
		block := blockAtLine(blocks, line)

		switch sel.Sel.Name {
		case "Handle", "HandleBlocking":
			if e, ok := handleRoute(call, sel.Sel.Name == "HandleBlocking", block); ok {
				routes = append(routes, e)
			}
		case "View":
			// router.View(pattern, engine, viewName, dataFn)
			if len(call.Args) >= 3 {
				routes = append(routes, routeEntry{
					Block:   block,
					Method:  "GET",
					Path:    stringArg(call.Args[0]),
					Handler: "view " + strings.Trim(stringArg(call.Args[2]), `"`),
				})
			}
		case "ServeStatic":
			if len(call.Args) >= 2 {
				routes = append(routes, routeEntry{
					Block:   block,
					Method:  "GET",
					Path:    stringArg(call.Args[0]) + "/*",
					Handler: "static " + stringArg(call.Args[1]),
				})
			}
		}
		return true
	})

	return routes, nil
}

func handleRoute(call *ast.CallExpr, blocking bool, block string) (routeEntry, bool) {
	if len(call.Args) < 3 {
		return routeEntry{}, false
	}
	return routeEntry{
		Block:    block,
		Method:   methodName(call.Args[0]),
		Path:     stringArg(call.Args[1]),
		Handler:  types.ExprString(call.Args[2]),
		Blocking: blocking,
		// Everything past the handler is middleware or a Doc wrapper.
		Middleware: len(call.Args) - 3,
	}, true
}

// methodName renders the method argument: breeze.GET becomes GET, anything
// else is shown as written so an unexpected form is visible rather than blank.
func methodName(expr ast.Expr) string {
	if sel, ok := expr.(*ast.SelectorExpr); ok {
		return sel.Sel.Name
	}
	return types.ExprString(expr)
}

// stringArg unquotes a string literal argument, falling back to the source form
// for a path built from a constant or variable.
func stringArg(expr ast.Expr) string {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return types.ExprString(expr)
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return lit.Value
	}
	return s
}

type markerRange struct {
	Name       string
	Start, End int
}

// markerRanges maps each marker block to the line range it spans, so a route
// can be attributed to the resource that generated it.
func markerRanges(fset *token.FileSet, f *ast.File, prefix string) []markerRange {
	var ranges []markerRange
	open := map[string]int{}

	for _, group := range f.Comments {
		for _, c := range group.List {
			text := strings.TrimSpace(strings.TrimPrefix(c.Text, "//"))
			if !strings.HasPrefix(text, prefix+":") {
				continue
			}
			rest := strings.TrimPrefix(text, prefix+":")
			line := fset.Position(c.Pos()).Line

			switch {
			case strings.HasSuffix(rest, ":start"):
				open[strings.TrimSuffix(rest, ":start")] = line
			case strings.HasSuffix(rest, ":end"):
				name := strings.TrimSuffix(rest, ":end")
				if start, ok := open[name]; ok {
					ranges = append(ranges, markerRange{Name: name, Start: start, End: line})
					delete(open, name)
				}
			}
		}
	}

	sort.Slice(ranges, func(i, j int) bool { return ranges[i].Start < ranges[j].Start })
	return ranges
}

func blockAtLine(ranges []markerRange, line int) string {
	for _, r := range ranges {
		if line >= r.Start && line <= r.End {
			return r.Name
		}
	}
	return ""
}

func printRouteTable(routes []routeEntry, fileName string) {
	if len(routes) == 0 {
		fmt.Printf("No routes in %s.\n\nGenerate some with:\n  breeze generate resource User name:string email:string\n", fileName)
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 8, 2, ' ', 0)
	fmt.Fprintln(w, "METHOD\tPATH\tHANDLER\tBLOCK\tNOTES")

	for _, r := range routes {
		var notes []string
		if r.Blocking {
			notes = append(notes, "blocking")
		}
		if r.Middleware > 0 {
			notes = append(notes, fmt.Sprintf("+%d mw", r.Middleware))
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			r.Method, r.Path, r.Handler, r.Block, strings.Join(notes, " "))
	}
	w.Flush()

	fmt.Printf("\n%d route(s) in %s.\n", len(routes), fileName)
	fmt.Printf("Routes registered outside this file â€” by hand in main.go, or mounted by a\n" +
		"feature at runtime (dashboard, video, docs) â€” are not listed.\n")
}
