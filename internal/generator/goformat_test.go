package generator

// goformat_test.go — the clean-code guarantees, checked against real generator
// output rather than against canonicalGoFile in isolation.
//
// Calling canonicalGoFile directly would prove the function works and say
// nothing about whether the generators go through it, which is the part that can
// regress: a new kind that formats its own output would pass a unit test of this
// file and fail every claim the guarantees make.

import (
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
)

// assertCanonical fails if src is not byte-identical to gofmt's output for it.
//
// The comparison is against format.Source rather than against a golden file
// because gofmt's exact spelling is not this package's to pin: what matters is
// that a later `gofmt -l` over a generated project reports nothing.
func assertCanonical(t *testing.T, path string) {
	t.Helper()

	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	want, err := format.Source(src)
	if err != nil {
		t.Fatalf("%s does not even parse: %v\n%s", path, err, src)
	}
	if string(want) != string(src) {
		t.Errorf(
			"%s is not gofmt-canonical; `gofmt -l` would report it.\n--- on disk ---\n%s\n--- gofmt ---\n%s",
			path,
			src,
			want,
		)
	}
}

// assertNoUnusedImports fails if the file imports a package nothing in it names.
//
// Blank and dot imports are exempt for the reason importIsNeeded gives: a blank
// import exists for its side effects, so being unreferenced is its normal state.
func assertNoUnusedImports(t *testing.T, path string) {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}

	used := referencedPackages(file)
	for _, imp := range file.Imports {
		if imp.Name != nil && (imp.Name.Name == "_" || imp.Name.Name == ".") {
			continue
		}
		name := imp.Name.String()
		if imp.Name == nil {
			name = packageNameFor(strings.Trim(imp.Path.Value, `"`))
			if name == "" {
				continue
			}
		}
		if !used[name] {
			t.Errorf(
				"%s imports %s but never refers to %s — the generated project would not compile",
				path,
				imp.Path.Value,
				name,
			)
		}
	}
}

// assertImportsGrouped fails if the import block is not standard library, then
// third party, then the project's own packages, with one blank line between.
//
// gofmt sorts within a group and never moves anything between groups, so this is
// the half of the convention gofmt cannot enforce — and the half a hand-written
// template gets wrong, because a template writes the imports in whatever order
// the code below happens to mention them.
func assertImportsGrouped(t *testing.T, path, modulePath string) {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}

	decl, ok := soleImportDecl(file)
	if !ok || !decl.Lparen.IsValid() {
		return // no factored import block to check
	}

	// Groups are separated by a blank line, which the AST records as a gap in
	// line numbers between consecutive specs.
	group := 0
	prevLine := fset.Position(decl.Specs[0].Pos()).Line
	seen := map[int]int{} // group index -> the importGroup it contains

	for i, spec := range decl.Specs {
		imp := spec.(*ast.ImportSpec)
		line := fset.Position(imp.Pos()).Line
		if i > 0 && line > prevLine+1 {
			group++
		}
		prevLine = line

		want := importGroup(strings.Trim(imp.Path.Value, `"`), modulePath)
		if existing, ok := seen[group]; ok && existing != want {
			t.Errorf("%s puts %s in the same group as a %s import; standard library, third-party "+
				"and project imports must be separated",
				path, imp.Path.Value, groupName(existing))
			continue
		}
		seen[group] = want
	}

	// The groups themselves must be in order: std, third-party, own.
	last := -1
	for g := 0; g <= group; g++ {
		kind, ok := seen[g]
		if !ok {
			continue
		}
		if kind < last {
			t.Errorf("%s orders its import groups %s before %s; the order is standard library, "+
				"third-party, then this project", path, groupName(kind), groupName(last))
		}
		last = kind
	}
}

func groupName(g int) string {
	switch g {
	case importGroupStd:
		return "standard library"
	case importGroupThirdParty:
		return "third-party"
	default:
		return "project"
	}
}

// TestEveryGeneratedFileIsCanonical is the sweep the acceptance criteria ask for:
// every kind, every generated file, checked for all three guarantees.
//
// It runs the kinds from kindsWithOutputFlags plus the two that write no
// standalone file of their own, so the marker-managed files
// (routes_generated.go, features_generated.go) are covered as well — those are
// generated too, and they are where an unused import is most likely, since
// ensureImports adds one per block without knowing what a later regeneration of
// that block still needs.
func TestEveryGeneratedFileIsCanonical(t *testing.T) {
	for _, tc := range kindsWithOutputFlags {
		t.Run(tc.kind, func(t *testing.T) {
			projectDir(t)
			if err := runGenerate(tc.args); err != nil {
				t.Fatalf("breeze generate %s: %v", strings.Join(tc.args, " "), err)
			}
			for _, path := range goFilesUnder(t, ".") {
				assertCanonical(t, path)
				assertNoUnusedImports(t, path)
				assertImportsGrouped(t, path, "example.com/proj")
			}
		})
	}
}

// TestExistingFixturesAreCanonical runs the same three checks over the
// pre-existing generator fixtures rather than over cases written for this change.
//
// That direction matters: a guarantee that only holds for the examples introduced
// alongside it is not a guarantee. These are the flag combinations the e2e suite
// already exercised, including the ones that omit handlers and so change which
// imports are needed.
func TestExistingFixturesAreCanonical(t *testing.T) {
	for _, args := range e2eGenerators {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			projectDir(t)
			if err := runGenerate(args); err != nil {
				t.Fatalf("breeze generate %s: %v", strings.Join(args, " "), err)
			}
			for _, path := range goFilesUnder(t, ".") {
				assertCanonical(t, path)
				assertNoUnusedImports(t, path)
				assertImportsGrouped(t, path, "example.com/proj")
			}
		})
	}
}

// TestFeatureBlocksAreCanonicalToo covers features_generated.go after `breeze
// add`, which reaches the writer through upsertBlock rather than through
// writeGeneratedGoFile.
//
// The two paths are separate, so the guarantee has to be claimed separately for
// each. Several features are added in sequence because the interesting state is a
// file with blocks from more than one, which is when the import block accumulates
// entries no single block owns.
func TestFeatureBlocksAreCanonicalToo(t *testing.T) {
	projectDir(t)

	for _, args := range [][]string{
		{"recovery"}, {"logging"}, {"cors"}, {"events", "--async"}, {"dashboard"},
	} {
		if err := runAdd(args); err != nil {
			t.Fatalf("breeze add %s: %v", strings.Join(args, " "), err)
		}
	}
	// A generator that writes into the same file, so the check sees blocks from
	// both `add` and `generate`.
	if err := runGenerate([]string{"ws", "Chat"}); err != nil {
		t.Fatalf("breeze generate ws: %v", err)
	}

	for _, path := range goFilesUnder(t, ".") {
		assertCanonical(t, path)
		assertNoUnusedImports(t, path)
		assertImportsGrouped(t, path, "example.com/proj")
	}
}

// TestNonCanonicalTemplateOutputIsFormattedBeforeItIsWritten.
//
// The handler generator's stub comes from a text/template, and a template's
// whitespace is hand-maintained: nothing about writing one guarantees the result
// is what gofmt would produce. This substitutes a deliberately ugly template for
// the real one and asserts the file on disk is canonical anyway — so the
// guarantee belongs to the writer, not to the care taken over each template.
func TestNonCanonicalTemplateOutputIsFormattedBeforeItIsWritten(t *testing.T) {
	projectDir(t)

	// Four-space indents, no space after //, runs of blank lines inside the body,
	// and misaligned struct fields: every one of these is something gofmt fixes.
	ugly := template.Must(template.New("ugly").Parse(
		"//nolint\n" +
			"type wonky struct {\n" +
			"Name string\n" +
			"Count int\n" +
			"}\n" +
			"{{range .Actions}}\n" +
			"//{{.FuncName}} handles {{.Verb}} {{.Path}}.\n" +
			"func {{.FuncName}}(ctx *breeze.Context) error {\n" +
			"    _ = ctx\n" +
			"\n\n" +
			"        // TODO: implement\n" +
			"return nil\n" +
			"}\n" +
			"{{end}}"))

	actions, err := actionsFor("Session", "Sessions", []string{"list"})
	if err != nil {
		t.Fatal(err)
	}
	target, err := (*outputFlags)(nil).target("handlers", "session")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeHandlerFile(
		target,
		"example.com/proj",
		"Session",
		actions,
		"/sessions",
		ugly,
		false,
	); err != nil {
		t.Fatalf("writing a handler from a non-canonical template: %v", err)
	}

	path := filepath.Join("handlers", "session.go")
	assertCanonical(t, path)

	// And the specific things gofmt would have had to fix are gone, so the check
	// above cannot be passing because the template happened to be canonical.
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "    _ = ctx") {
		t.Error("the four-space indent survived; the output was written unformatted")
	}
	if strings.Contains(string(src), "\n\n\n") {
		t.Error("a run of blank lines survived gofmt")
	}
}

// TestConditionalTemplateBranchesLeaveNoUnusedImport.
//
// The resource generator emits a different set of handlers per --methods, and
// three of its imports exist only for handlers some combinations exclude. That is
// the case where a template's conditionals and its import list drift: the
// combination nobody generates during development is the one that ships an unused
// import, and an unused import in Go is a build failure rather than a warning.
func TestConditionalTemplateBranchesLeaveNoUnusedImport(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		absent  []string
		present []string
	}{
		{
			name:    "list only",
			args:    []string{"resource", "Widget", "name:string", "--methods=list"},
			absent:  []string{`"errors"`, `"fmt"`, `"github.com/nelthaarion/breeze/v2/binding"`},
			present: []string{`"sync"`, `"github.com/nelthaarion/breeze/v2"`},
		},
		{
			name: "create needs binding and fmt",
			args: []string{"resource", "Widget", "name:string", "--methods=create"},
			// name:string infers `required`, so the 422 branch and errors are live.
			present: []string{`"errors"`, `"fmt"`, `"github.com/nelthaarion/breeze/v2/binding"`},
		},
		{
			name:    "no validation means no errors import",
			args:    []string{"resource", "Widget", "age:int", "--methods=create"},
			absent:  []string{`"errors"`},
			present: []string{`"github.com/nelthaarion/breeze/v2/binding"`},
		},
		{
			name:    "a time field pulls time in",
			args:    []string{"resource", "Widget", "signed_up_at:time.Time", "--methods=list"},
			present: []string{`"time"`},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			projectDir(t)
			if err := runGenerate(tc.args); err != nil {
				t.Fatalf("breeze generate %s: %v", strings.Join(tc.args, " "), err)
			}

			path := filepath.Join("handlers", "widget.go")
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			// The general guarantee first, then the specific expectations — so a
			// failure says whether the writer let an unused import through or the
			// generator stopped emitting one it needs.
			assertNoUnusedImports(t, path)
			assertImportsGrouped(t, path, "example.com/proj")
			assertCanonical(t, path)

			for _, want := range tc.present {
				if !strings.Contains(string(src), want) {
					t.Errorf("%s is missing the %s import it needs:\n%s", path, want, src)
				}
			}
			for _, unwanted := range tc.absent {
				if strings.Contains(string(src), unwanted) {
					t.Errorf(
						"%s imports %s, which nothing in this combination uses",
						path,
						unwanted,
					)
				}
			}
		})
	}
}

// TestUnusedImportIsDroppedEvenWhenAGeneratorAsksForIt.
//
// The writer takes a candidate list, not a final one, which is what lets a
// generator list an import its conditionals might not need. Passing one that is
// definitely unnecessary proves the pruning is real rather than a description of
// what the generators happen to do.
func TestUnusedImportIsDroppedEvenWhenAGeneratorAsksForIt(t *testing.T) {
	projectDir(t)

	target, err := (*outputFlags)(nil).target("models", "probe")
	if err != nil {
		t.Fatal(err)
	}
	err = writeGeneratedGoFile(generatedFile{
		Target: target,
		Owner:  generateOwner("model"),
		// "time" is used; "sync" and the dashboard package are not. A blank
		// import is included too, since those must survive: they exist for their
		// side effects, so being unreferenced is their normal state.
		Imports: []string{
			timeImport, `"sync"`, `"github.com/nelthaarion/breeze/v2/dashboard"`,
			`_ "github.com/nelthaarion/breeze/v2/events"`,
		},
		Body:       "// Probe is a fixture.\ntype Probe struct {\n\tAt time.Time\n}\n",
		ModulePath: "example.com/proj",
	})
	if err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}

	path := filepath.Join("models", "probe.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(src)

	if !strings.Contains(got, `"time"`) {
		t.Errorf("the used import was dropped:\n%s", got)
	}
	for _, unwanted := range []string{`"sync"`, `"github.com/nelthaarion/breeze/v2/dashboard"`} {
		if strings.Contains(got, unwanted) {
			t.Errorf("%s survived although nothing references it:\n%s", unwanted, got)
		}
	}
	if !strings.Contains(got, `_ "github.com/nelthaarion/breeze/v2/events"`) {
		t.Errorf("a blank import was pruned; those are there for their side effects:\n%s", got)
	}
	assertCanonical(t, path)
}

// goFilesUnder lists the Go files a generator run produced.
func goFilesUnder(t *testing.T, root string) []string {
	t.Helper()

	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(path, ".go") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	if len(out) == 0 {
		t.Fatalf("no Go files were generated under %s, so this check would pass vacuously", root)
	}
	return out
}
