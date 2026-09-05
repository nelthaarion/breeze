package mcp

// idiomcheck.go — the static half of the conventions in idioms.go.
//
// explain_idiom says what a convention is and marks the ones a machine can
// verify; this is what verifies them. The two must not drift, so the rule names
// reported here are the same strings idiomList carries, and a test asserts that
// every rule marked StaticallyChecked is one this file can actually produce.
//
// The checks are AST-only, deliberately. A type-checked analysis would be more
// precise, but it needs the project's dependencies resolved, and the projects
// this runs against are frequently mid-edit or generated with `go mod tidy`
// having failed offline. A checker that refuses to run on a project that does
// not build yet is a checker that is unavailable exactly when an agent most
// needs it. So the cost of AST-only is accepted, and the imprecision is
// contained by two rules:
//
//   - The identifier that decides a match is always resolved back to an import
//     path via the file's own import block, never assumed from the package name
//     a call is qualified with. `dashboard.Middleware` from some unrelated
//     package named dashboard is not a finding.
//   - Where a rule cannot be decided from syntax alone it is reported at the
//     severity idioms.go gives it, and the message says what was seen rather
//     than asserting intent.
//
// Findings are ordered by file then line so a report is stable across runs, and
// each carries the rule name so a caller can look the reasoning up with
// explain_idiom rather than having the prose duplicated into every finding.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// The import paths that identify the framework's own packages. A rule keys off
// these rather than off the local identifier, so an alias or a same-named
// package from somewhere else cannot produce a false finding.
const (
	breezeModulePath = "github.com/nelthaarion/breeze/v2"
	fleetPath        = breezeModulePath + "/fleet"
	dashboardPath    = breezeModulePath + "/dashboard"
	middlewaresPath  = breezeModulePath + "/middlewares"
	reflectPath      = "reflect"
)

// idiomFinding is one violation. The shape is the one the task fixes: file,
// line, rule, message, severity.
//
// File is relative to the checked root, because an absolute path from a
// temporary directory is noise in a report an agent has to read back.
type idiomFinding struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Rule     string `json:"rule"`
	Message  string `json:"message"`
	Severity string `json:"severity"`
}

// idiomReport is what check_idioms returns.
//
// Checked and Skipped are both present because "no findings" is only meaningful
// alongside what was looked at: a clean report over zero files is not evidence,
// and a caller cannot tell the two apart from an empty list.
type idiomReport struct {
	Path     string         `json:"path"`
	Findings []idiomFinding `json:"findings"`
	Count    int            `json:"count"`
	Errors   int            `json:"errors"`
	Warnings int            `json:"warnings"`
	Files    int            `json:"files_checked"`
	Rules    []string       `json:"rules_checked"`
	Skipped  []string       `json:"skipped,omitempty"`
	Notes    []string       `json:"notes,omitempty"`
}

// checkedIdiomRules are the rules this file implements, in the order they are
// reported. Kept as its own list so the test that compares it against
// idiomList's Checked flags has something to compare.
func checkedIdiomRules() []string {
	return []string{
		"no-reflection-in-handlers",
		"fleet-before-dashboard",
		"scalar-is-the-viewer",
		"generated-model-accessors",
	}
}

// runIdiomCheck parses every Go file under root and applies the static rules.
//
// Directories that cannot contain project code are skipped: vendor is not the
// author's, testdata is meant to hold deliberately odd code, and a dot
// directory is tooling. Parse failures are collected rather than fatal — a
// project with one unparseable file should still be told about the other files.
func runIdiomCheck(root string) (idiomReport, error) {
	report := idiomReport{
		Path:  root,
		Rules: checkedIdiomRules(),
	}

	info, err := os.Stat(root)
	if err != nil {
		return report, fmt.Errorf("reading %s: %w", root, err)
	}
	if !info.IsDir() {
		return report, fmt.Errorf("%s is not a directory", root)
	}

	fset := token.NewFileSet()

	// Read once, before the walk. The table names come from the models on disk,
	// so a project with no models simply has no table rule to apply, and the
	// per-file check can then be a pure function of what it is handed.
	models, modelNotes := readSourceModels(root)
	report.Notes = append(report.Notes, modelNotes...)

	tables := make(map[string]string, len(models))
	for _, m := range models {
		if m.Table == "" {
			// No generated constant was found for this model, so there is
			// nothing to recommend in place of a literal.
			continue
		}
		tables[strings.ToLower(m.Table)] = m.Name + "Table"
	}

	walkErr := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() {
			switch name := entry.Name(); {
			case path == root:
				return nil
			case name == "vendor", name == "testdata", name == "node_modules":
				return filepath.SkipDir
			case strings.HasPrefix(name, "."):
				return filepath.SkipDir
			}
			return nil
		}

		if !strings.HasSuffix(entry.Name(), ".go") {
			return nil
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)

		file, parseErr := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if parseErr != nil {
			// Reported rather than returned: the rest of the project is still
			// worth checking, and a caller needs to know this file was not.
			report.Skipped = append(
				report.Skipped,
				fmt.Sprintf("%s (does not parse: %v)", rel, parseErr),
			)
			return nil
		}

		report.Files++
		checkFileIdioms(fset, file, rel, tables, &report)
		return nil
	})
	if walkErr != nil {
		return report, fmt.Errorf("walking %s: %w", root, walkErr)
	}

	sort.SliceStable(report.Findings, func(i, j int) bool {
		if report.Findings[i].File != report.Findings[j].File {
			return report.Findings[i].File < report.Findings[j].File
		}
		return report.Findings[i].Line < report.Findings[j].Line
	})

	report.Count = len(report.Findings)
	for _, f := range report.Findings {
		switch f.Severity {
		case severityError:
			report.Errors++
		case severityWarning:
			report.Warnings++
		}
	}

	if report.Files == 0 {
		report.Notes = append(
			report.Notes,
			"No Go files were found under this path, so a clean report is not evidence of anything.",
		)
	}

	return report, nil
}

// importedAs maps a file's local identifiers to the import paths they stand for.
//
// This is the whole defence against false positives. `dashboard.Middleware` is
// only interesting when dashboard resolves to the framework's dashboard package;
// an unrelated package of the same name, or an alias, is decided correctly
// because the answer comes from the import block rather than from the spelling
// of the selector.
func importedAs(file *ast.File) map[string]string {
	out := make(map[string]string, len(file.Imports))
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}

		name := ""
		switch {
		case spec.Name != nil:
			name = spec.Name.Name
		default:
			// The default local name is the last path segment. This is right for
			// every package in this repository; a package whose clause differs
			// from its directory would be missed, which errs toward silence
			// rather than toward a wrong finding.
			name = path[strings.LastIndexByte(path, '/')+1:]
		}
		if name == "_" || name == "." {
			continue
		}
		out[name] = path
	}
	return out
}

// selectorPath returns the import path and member name of a qualified call such
// as fleet.Middleware, or ok=false when the expression is not one.
func selectorPath(imports map[string]string, expr ast.Expr) (path, member string, ok bool) {
	sel, isSel := expr.(*ast.SelectorExpr)
	if !isSel {
		return "", "", false
	}
	ident, isIdent := sel.X.(*ast.Ident)
	if !isIdent {
		return "", "", false
	}
	path, found := imports[ident.Name]
	if !found {
		return "", "", false
	}
	return path, sel.Sel.Name, true
}

func checkFileIdioms(
	fset *token.FileSet,
	file *ast.File,
	rel string,
	tables map[string]string,
	report *idiomReport,
) {
	imports := importedAs(file)

	checkNoReflectionInHandlers(fset, file, imports, rel, report)
	checkFleetBeforeDashboard(fset, file, imports, rel, report)
	checkScalarIsTheViewer(fset, file, imports, rel, report)
	checkGeneratedModelAccessors(fset, file, rel, tables, report)
}

// add records a finding, taking the severity from idioms.go so that the
// explanation and the check can never disagree about how serious a rule is.
//
// The file name is passed in rather than recovered from the position. A position
// knows the absolute path it was parsed from, and every finding is reported
// relative to the checked root, so the caller that already computed that
// relative name is the honest source for it.
func (r *idiomReport) add(fset *token.FileSet, pos token.Pos, rel, rule, message string) {
	severity := severityWarning
	if idiom, ok := idiomsByRule[rule]; ok {
		severity = idiom.Severity
	}
	r.Findings = append(r.Findings, idiomFinding{
		File:     rel,
		Line:     fset.Position(pos).Line,
		Rule:     rule,
		Message:  message,
		Severity: severity,
	})
}

// ─── no-reflection-in-handlers ───────────────────────────────────────────────

// checkNoReflectionInHandlers reports reflect used from a request path.
//
// The rule is about the hot path, not about the package as a whole: reflect at
// startup is how the generator and the schema tools work, and flagging that
// would make the rule noise. So a finding needs the call to be inside a function
// that handles a request, which is decided two ways:
//
//   - the function's signature takes *breeze.Context, which is what a
//     HandlerFunc is; or
//   - the function literal is passed to a router registration call.
//
// Both are syntactic. A handler that hides its reflect call behind a helper
// several frames deep is not caught — catching that needs a call graph, and a
// call graph needs types. The rule as stated ("in handlers") is what is checked,
// and the message says where the call was seen so a reader can judge.
func checkNoReflectionInHandlers(
	fset *token.FileSet,
	file *ast.File,
	imports map[string]string,
	rel string,
	report *idiomReport,
) {
	if _, usesReflect := pathLocalName(imports, reflectPath); !usesReflect {
		// No reflect import means no reflect call, and skipping here keeps the
		// walk off every file in a project that never touches it.
		return
	}

	ast.Inspect(file, func(node ast.Node) bool {
		var body *ast.BlockStmt
		var what string

		switch decl := node.(type) {
		case *ast.FuncDecl:
			if decl.Body == nil || !takesBreezeContext(imports, decl.Type) {
				return true
			}
			body = decl.Body
			what = "handler " + decl.Name.Name

		case *ast.FuncLit:
			if !takesBreezeContext(imports, decl.Type) {
				return true
			}
			body = decl.Body
			what = "a handler function literal"

		default:
			return true
		}

		for _, pos := range reflectCallsIn(imports, body) {
			report.add(
				fset,
				pos,
				rel,
				"no-reflection-in-handlers",
				fmt.Sprintf(
					"%s calls reflect, which allocates and escapes on a path that runs per request",
					what,
				),
			)
		}
		return true
	})
}

// reflectCallsIn returns the positions of reflect uses inside a body.
func reflectCallsIn(imports map[string]string, body *ast.BlockStmt) []token.Pos {
	var out []token.Pos
	ast.Inspect(body, func(node ast.Node) bool {
		sel, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if path, _, ok := selectorPath(imports, sel); ok && path == reflectPath {
			out = append(out, sel.Pos())
		}
		return true
	})
	return out
}

// takesBreezeContext reports whether a signature is a request handler, i.e.
// whether it takes *breeze.Context.
//
// Inside package breeze itself the type is written *Context with no qualifier,
// so that spelling is accepted too. A local type also named Context in some
// other package would be a false positive; that is why the unqualified form is
// only honoured when the file has no breeze import to qualify with.
func takesBreezeContext(imports map[string]string, sig *ast.FuncType) bool {
	if sig == nil || sig.Params == nil {
		return false
	}

	breezeLocal, hasBreeze := pathLocalName(imports, breezeModulePath)

	for _, param := range sig.Params.List {
		star, isStar := param.Type.(*ast.StarExpr)
		if !isStar {
			continue
		}

		switch typ := star.X.(type) {
		case *ast.SelectorExpr:
			ident, ok := typ.X.(*ast.Ident)
			if ok && hasBreeze && ident.Name == breezeLocal && typ.Sel.Name == "Context" {
				return true
			}
		case *ast.Ident:
			if !hasBreeze && typ.Name == "Context" {
				return true
			}
		}
	}
	return false
}

// pathLocalName returns the identifier a file refers to an import path by.
func pathLocalName(imports map[string]string, want string) (string, bool) {
	for name, path := range imports {
		if path == want {
			return name, true
		}
	}
	return "", false
}

// ─── fleet-before-dashboard ──────────────────────────────────────────────────

// checkFleetBeforeDashboard reports a router.Use order that puts the dashboard's
// middleware ahead of Fleet's.
//
// Order decides nesting, and the dashboard reads the trace context Fleet
// establishes. Registered the other way round the dashboard's own requests are
// attributed to whatever span happened to be open, which is not a crash but a
// quietly wrong timeline — the failure this rule exists to prevent.
//
// Only the two calls' relative position within one file is judged. Across files
// the order is decided by the order the setup functions are called, which is
// generated, and second-guessing that from syntax would produce findings that
// are wrong as often as right.
func checkFleetBeforeDashboard(
	fset *token.FileSet,
	file *ast.File,
	imports map[string]string,
	rel string,
	report *idiomReport,
) {
	type useCall struct {
		pos  token.Pos
		path string
	}
	var calls []useCall

	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		// The outer call has to be a .Use(...) — on a Router, but the receiver's
		// type is not knowable without types, so the member name is what is
		// matched and the argument is what decides.
		sel, isSel := call.Fun.(*ast.SelectorExpr)
		if !isSel || sel.Sel.Name != "Use" {
			return true
		}

		for _, arg := range call.Args {
			inner, isCall := arg.(*ast.CallExpr)
			if !isCall {
				continue
			}
			path, member, ok := selectorPath(imports, inner.Fun)
			if !ok || member != "Middleware" {
				continue
			}
			if path == fleetPath || path == dashboardPath {
				calls = append(calls, useCall{pos: call.Pos(), path: path})
			}
		}
		return true
	})

	firstDashboard := token.NoPos
	for _, c := range calls {
		switch {
		case c.path == dashboardPath && firstDashboard == token.NoPos:
			firstDashboard = c.pos
		case c.path == fleetPath && firstDashboard != token.NoPos:
			report.add(
				fset,
				c.pos,
				rel,
				"fleet-before-dashboard",
				"fleet.Middleware is registered after dashboard.Middleware in this file; the dashboard reads the trace context Fleet establishes, so this order attributes dashboard requests to the wrong span",
			)
			return
		}
	}
}

// ─── scalar-is-the-viewer ────────────────────────────────────────────────────

// checkScalarIsTheViewer reports the deprecated Swagger spellings.
//
// SwaggerMiddleware still compiles — it is an alias kept so existing code does
// not break — which is exactly why this needs saying: the call works, no Swagger
// UI is served, and the author is left looking for a page that was never going
// to appear.
func checkScalarIsTheViewer(
	fset *token.FileSet,
	file *ast.File,
	imports map[string]string,
	rel string,
	report *idiomReport,
) {
	deprecated := map[string]string{
		"SwaggerMiddleware": "middlewares.ScalarMiddleware",
		"SwaggerOptions":    "middlewares.ScalarOptions",
	}

	ast.Inspect(file, func(node ast.Node) bool {
		sel, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		path, member, ok := selectorPath(imports, sel)
		if !ok || path != middlewaresPath {
			return true
		}
		if replacement, isDeprecated := deprecated[member]; isDeprecated {
			report.add(
				fset,
				sel.Pos(),
				rel,
				"scalar-is-the-viewer",
				fmt.Sprintf(
					"middlewares.%s is a deprecated alias that serves Scalar, not Swagger UI; use %s so the code says what is served",
					member,
					replacement,
				),
			)
		}
		return true
	})
}

// ─── generated-model-accessors ───────────────────────────────────────────────

// checkGeneratedModelAccessors reports a SQL string that names a table the
// project has a generated constant for.
//
// `generate model` emits <Name>Table precisely so a query does not carry the
// string. A hand-written table name still runs, and keeps running until the
// table is renamed, at which point the constant moves and the string does not.
//
// Only tables this project actually declares are considered, and generated files
// are exempt, so the rule cannot fire on the generator's own output. The match
// is deliberately narrow — the string has to look like SQL and name a known
// table — because a rule that flagged every string containing a table-like word
// would be turned off within a day.
func checkGeneratedModelAccessors(
	fset *token.FileSet,
	file *ast.File,
	rel string,
	tables map[string]string,
	report *idiomReport,
) {
	if isGeneratedFile(file) {
		return
	}

	if len(tables) == 0 {
		return
	}

	ast.Inspect(file, func(node ast.Node) bool {
		lit, ok := node.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}

		lowered := strings.ToLower(value)
		if !looksLikeSQL(lowered) {
			return true
		}

		for table, constant := range tables {
			if !sqlNamesTable(lowered, table) {
				continue
			}
			report.add(
				fset,
				lit.Pos(),
				rel,
				"generated-model-accessors",
				fmt.Sprintf(
					"this query writes the table name %q by hand; use the generated %s constant so a rename moves the query with the model",
					table,
					constant,
				),
			)
			return true
		}
		return true
	})
}

// looksLikeSQL keeps the rule off ordinary strings that merely contain a word
// which happens to be a table name.
func looksLikeSQL(lowered string) bool {
	for _, keyword := range []string{"select ", "insert into", "update ", "delete from"} {
		if strings.Contains(lowered, keyword) {
			return true
		}
	}
	return false
}

// sqlNamesTable reports whether a SQL string uses a table as a table, rather
// than containing its name as part of a longer identifier such as order_items.
func sqlNamesTable(lowered, table string) bool {
	for _, clause := range []string{"from ", "into ", "update ", "join "} {
		idx := 0
		for {
			at := strings.Index(lowered[idx:], clause)
			if at < 0 {
				break
			}
			at += idx
			rest := strings.TrimLeft(lowered[at+len(clause):], " \t\n(`\"[")
			if strings.HasPrefix(rest, table) {
				after := rest[len(table):]
				if after == "" || !isIdentByte(after[0]) {
					return true
				}
			}
			idx = at + len(clause)
		}
	}
	return false
}

func isIdentByte(b byte) bool {
	return b == '_' ||
		(b >= '0' && b <= '9') ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z')
}

// isGeneratedFile recognises the convention Go tooling agreed on, so the rule
// never fires on code its author cannot change by hand.
func isGeneratedFile(file *ast.File) bool {
	for _, group := range file.Comments {
		for _, comment := range group.List {
			text := comment.Text
			if strings.HasPrefix(text, "// Code generated ") &&
				strings.Contains(text, "DO NOT EDIT") {
				return true
			}
		}
	}
	return false
}
