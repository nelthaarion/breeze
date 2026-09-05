package generator

// goformat.go — the canonical form every generated Go file is written in.
//
// Three guarantees, applied in one place rather than per generator:
//
//   - gofmt. Every generator builds source by concatenating strings, and the
//     indentation in those strings is a hand-maintained approximation of what
//     gofmt would do. Running format.Source over the result means the
//     approximation never has to be right.
//
//   - Import grouping. Standard library first, third-party second, the
//     generated project's own packages last, each group separated by a blank
//     line. That is the convention the hand-written packages in this repository
//     already follow (internal/mcp/tools_plan.go, tools_knowledge.go), so it is
//     borrowed rather than invented. gofmt sorts within a group but never moves
//     an import between groups, which is why this is not something format.Source
//     does on its own.
//
//   - No unused imports. A generator that conditionally omits code has to
//     conditionally omit the import that code needed, and getting that wrong
//     produces a project that does not compile — the one failure mode a
//     generator must not have. Rather than trusting every template's
//     conditionals, the import is dropped here if nothing in the finished file
//     refers to it.
//
// The package clause is validated here too, which is what makes the rule in
// naming.go global: a derived package name goes through exactly the same check
// as one a user typed, because both arrive at this function.
//
// Conservatism is deliberate. Where this cannot be certain what an import is
// called — a path whose last element is not an identifier, an aliased or blank
// import, a comment attached to an import spec — the import block is left
// exactly as the generator produced it. A wrongly kept import is a gofmt nit; a
// wrongly dropped one does not compile.

import (
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"sort"
	"strings"
)

// canonicalGoFile returns src as it must appear on disk.
//
// path is used only in error messages, so a caller that has not decided on a
// final location can pass the intended one.
func canonicalGoFile(path, src, modulePath string) (string, error) {
	formatted, err := format.Source([]byte(src))
	if err != nil {
		return "", fmt.Errorf("formatting %s: %w", path, err)
	}

	regrouped, pkg, err := organizeImports(path, string(formatted), modulePath)
	if err != nil {
		return "", err
	}
	// The package clause is checked after parsing rather than by scanning the
	// text, so `package` inside a comment or a string cannot be mistaken for it.
	if err := validateGoPackageName(pkg); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}

	// A second pass: gofmt sorts the specs inside each group it finds and
	// re-aligns whatever the regrouping moved.
	out, err := format.Source([]byte(regrouped))
	if err != nil {
		return "", fmt.Errorf("formatting %s after grouping its imports: %w", path, err)
	}
	return string(out), nil
}

// organizeImports rewrites src's import declaration and reports the package it
// declares.
func organizeImports(path, src, modulePath string) (out, pkg string, err error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return "", "", fmt.Errorf("parsing %s: %w", path, err)
	}
	pkg = file.Name.Name

	decl, ok := soleImportDecl(file)
	if !ok {
		// No imports, more than one import declaration, or a spec carrying a
		// comment. All three are left alone: the file is already
		// gofmt-canonical, and the reasons to rewrite it do not outweigh
		// discarding a comment or guessing at an unusual layout.
		return src, pkg, nil
	}

	used := referencedPackages(file)

	var groups [importGroupCount][]string
	for _, spec := range decl.Specs {
		imp := spec.(*ast.ImportSpec)
		if !importIsNeeded(imp, used) {
			continue
		}
		g := importGroup(importPathOf(imp), modulePath)
		groups[g] = append(groups[g], importLine(imp))
	}

	start := fset.Position(decl.Pos()).Offset
	end := fset.Position(decl.End()).Offset
	// The parenthesised form is preserved rather than collapsed when only one
	// import survives. ensureImports finds the anchor import by text and splices
	// a new line in after it, which only produces valid Go inside a factored
	// declaration — so rewriting `import (...)` as `import "..."` here would
	// break the next `breeze add` on the same file.
	return src[:start] + renderImportDecl(groups, decl.Lparen.IsValid()) + src[end:], pkg, nil
}

// soleImportDecl returns the file's one import declaration, and whether it is
// safe to rewrite.
func soleImportDecl(file *ast.File) (*ast.GenDecl, bool) {
	var found *ast.GenDecl
	for _, d := range file.Decls {
		decl, ok := d.(*ast.GenDecl)
		if !ok || decl.Tok != token.IMPORT {
			continue
		}
		if found != nil {
			return nil, false
		}
		found = decl
	}
	if found == nil || len(found.Specs) == 0 {
		return nil, false
	}
	for _, spec := range found.Specs {
		imp, ok := spec.(*ast.ImportSpec)
		if !ok {
			return nil, false
		}
		// A comment on a spec would be lost by re-rendering the block from the
		// paths alone, and a comment on an import usually explains exactly the
		// sort of thing nobody wants deleted (a blank import's purpose, say).
		if imp.Doc != nil || imp.Comment != nil {
			return nil, false
		}
	}
	return found, true
}

// The import groups, in the order they are emitted.
const (
	importGroupStd = iota
	importGroupThirdParty
	importGroupOwn
	importGroupCount
)

// importGroup places one path.
//
// The project's own module is checked first, because a module path is a domain
// and would otherwise be indistinguishable from any other third-party one.
func importGroup(path, modulePath string) int {
	switch {
	case modulePath != "" && (path == modulePath || strings.HasPrefix(path, modulePath+"/")):
		return importGroupOwn
	case isStdImportPath(path):
		return importGroupStd
	default:
		return importGroupThirdParty
	}
}

// isStdImportPath reports whether a path names a standard library package.
//
// The test is the one the go command itself uses for this distinction: a
// standard library path's first element contains no dot, because it is not a
// domain name.
func isStdImportPath(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}

// renderImportDecl writes the grouped import declaration.
//
// factored says whether the declaration arrived parenthesised. An unfactored
// single import is left unfactored, which is what the generators' own templates
// emit and what gofmt leaves alone; a factored one stays factored even if it is
// down to one spec, because other code in this package edits factored blocks by
// text.
func renderImportDecl(groups [importGroupCount][]string, factored bool) string {
	total, only := 0, ""
	for _, g := range groups {
		total += len(g)
		if len(g) == 1 {
			only = g[0]
		}
	}
	switch {
	case total == 0:
		return ""
	case total == 1 && !factored:
		return "import " + only
	}

	var b strings.Builder
	b.WriteString("import (\n")
	first := true
	for _, g := range groups {
		if len(g) == 0 {
			continue
		}
		if !first {
			b.WriteString("\n")
		}
		first = false
		sort.Strings(g)
		for _, line := range g {
			b.WriteString("\t")
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	b.WriteString(")")
	return b.String()
}

// importLine renders one spec as it appears inside the declaration.
func importLine(imp *ast.ImportSpec) string {
	if imp.Name != nil {
		return imp.Name.Name + " " + imp.Path.Value
	}
	return imp.Path.Value
}

// importPathOf is the unquoted path.
func importPathOf(imp *ast.ImportSpec) string {
	return strings.Trim(imp.Path.Value, `"`)
}

// importIsNeeded reports whether an import has to stay.
//
// Blank and dot imports stay unconditionally: a blank import is there for its
// side effects, so "nothing refers to it" is the normal case rather than
// evidence it is unused, and a dot import puts names into file scope under no
// qualifier at all.
func importIsNeeded(imp *ast.ImportSpec, used map[string]bool) bool {
	if imp.Name != nil {
		switch imp.Name.Name {
		case "_", ".":
			return true
		}
		return used[imp.Name.Name]
	}

	name := packageNameFor(importPathOf(imp))
	if name == "" {
		// The package's name cannot be derived from its path, so whether it is
		// used cannot be answered here.
		return true
	}
	return used[name]
}

// packageNameFor guesses the identifier an unaliased import is referred to by,
// or returns "" when it cannot.
//
// The guess is the path's last element, which is right for every import the
// generators emit and for the overwhelming majority of Go packages. When the
// last element is not a valid identifier — "yaml.v3", "go-json" — the real
// package name is declared in source this has no access to, and "" is the
// honest answer.
//
// A major-version suffix is the one systematic exception. Under semantic import
// versioning `github.com/nelthaarion/breeze/v2` declares `package breeze`, not
// `package v2`, so the last element is the wrong answer for every v2+ module —
// including this one. Reading it literally made the import writer decide the
// framework's own import was unreferenced and prune it out of every generated
// handler, which does not fail here but fails to compile in the user's project.
func packageNameFor(path string) string {
	last := path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		last = path[i+1:]
	}
	if isMajorVersionSegment(last) {
		rest := path[:len(path)-len(last)-1]
		last = rest
		if i := strings.LastIndex(rest, "/"); i >= 0 {
			last = rest[i+1:]
		}
	}
	if last == "" || !token.IsIdentifier(last) {
		return ""
	}
	return last
}

// isMajorVersionSegment reports whether a path element is a "vN" major-version
// suffix, N >= 2.
//
// v0 and v1 are excluded because they are never spelled in an import path; a
// directory genuinely named v1 would be an ordinary package. Everything after
// the "v" must be a digit, so "v2beta" — a plausible real directory name — is
// not mistaken for one.
func isMajorVersionSegment(segment string) bool {
	if len(segment) < 2 || segment[0] != 'v' {
		return false
	}
	for i := 1; i < len(segment); i++ {
		if segment[i] < '0' || segment[i] > '9' {
			return false
		}
	}
	return segment != "v0" && segment != "v1"
}

// referencedPackages collects every identifier used as a qualifier.
//
// Only the left-hand side of a selector can name a package, so that is the only
// thing looked at. This over-collects — a local variable used as x.Field puts
// "x" in the set — and over-collecting is the safe direction: it keeps an
// import that could have gone.
func referencedPackages(file *ast.File) map[string]bool {
	used := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if ident, ok := sel.X.(*ast.Ident); ok {
				used[ident.Name] = true
			}
		}
		return true
	})
	return used
}
