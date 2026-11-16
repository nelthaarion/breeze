package generator

import (
	"embed"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"
)

func TestNewCleansUpOnFailure(t *testing.T) {
	t.Chdir(t.TempDir())
	orig := renderTree
	renderTree = func(embed.FS, string, string, newProjectData) error {
		return errors.New("render failed")
	}
	defer func() { renderTree = orig }()

	if err := runNew([]string{"myapp"}); err == nil {
		t.Fatal("expected error, got nil")
	}
	if _, err := os.Stat("myapp"); !os.IsNotExist(err) {
		t.Error("myapp directory was not cleaned up after a failed scaffold")
	}
}

// TestScaffoldedMainDoesNotDropWhatRunReturns: Run returns when the listener cannot start, because
// the port is taken or was left in TIME_WAIT by a run that has just stopped. A main that calls it as
// a bare statement turns that into a process that prints "listening" and exits with status 0, which
// is what a service that never opened its port looks like from outside.
func TestScaffoldedMainDoesNotDropWhatRunReturns(t *testing.T) {
	for name, tc := range map[string]struct {
		fs   embed.FS
		root string
	}{
		"api":   {apiTemplateFS, "templates/api"},
		"views": {viewsTemplateFS, "templates/views"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "app")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := renderTemplateTree(tc.fs, tc.root, dir, newProjectData{Name: "app", Module: "example.com/app"}); err != nil {
				t.Fatalf("rendering the %s scaffold: %v", name, err)
			}
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, filepath.Join(dir, "main.go"), nil, 0)
			if err != nil {
				t.Fatalf("the scaffolded main.go does not parse: %v", err)
			}

			runs, dropped := 0, 0
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Run" {
					runs++
				}
				return true
			})
			ast.Inspect(file, func(n ast.Node) bool {
				stmt, ok := n.(*ast.ExprStmt)
				if !ok {
					return true
				}
				if call, ok := stmt.X.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Run" {
						dropped++
					}
				}
				return true
			})
			if runs != 1 {
				t.Fatalf("the scaffold calls Run %d times, want the one this test is about", runs)
			}
			if dropped != 0 {
				t.Errorf("the scaffolded main.go calls Run as a statement, dropping the error it returns")
			}
		})
	}
}
