package generator

// A test for a class of bug the compiler cannot see.
//
// Every feature is a setupX function in features_generated.go, and one dispatcher
// calls them in priority order, not in the order they were written. Features hand
// each other package-level handles — EventBus, DashboardCollector, FleetTracer — and a
// handle is assigned by the function that creates it. A setup function that uses a
// handle at setup time therefore only works if the function that assigns it is
// dispatched first, and nothing says so: the code compiles, vets and builds, and the
// handle is simply nil when it is read.
//
// That is exactly how a generated service with the dashboard and Fleet came to crash
// within seconds of its first traced request. setupFleet is dispatched before
// setupDashboard, because Fleet's middleware has to be registered first, and it wrote
// `Logger: DashboardCollector.PushLog`, a method value, which binds to whatever the
// variable holds at that moment. That was nil. The first line the tracer logged
// dereferenced it, in a background goroutine, and took the process down. The end-to-end
// scaffold test compiled it and was satisfied.
//
// The check is structural rather than a search for that one line, so the next
// handle that is read too early fails here as well.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// setupTimeUses finds the package-level variables a setup function reads while it
// runs, and the ones it assigns.
//
// A function literal is skipped, deliberately. Its body does not run at setup time, so
// a handle read inside one is looked up when the literal is called, by which time every
// setup function has run. That is the shape of the fix, and the check has to allow it.
//
// Two reads are not uses: the left-hand side of an assignment is a write, and a
// comparison with nil is the guard that makes an early read safe.
func setupTimeUses(fn *ast.FuncDecl, pkg *ast.Scope) (uses map[string]token.Pos, assigns map[string]bool) {
	uses = map[string]token.Pos{}
	assigns = map[string]bool{}
	skip := map[*ast.Ident]bool{}

	isPackageVar := func(id *ast.Ident) bool {
		if id.Obj == nil || id.Obj.Kind != ast.Var {
			return false
		}
		// Resolved to the file's scope, not to a local that happens to share the name.
		return pkg.Lookup(id.Name) == id.Obj
	}

	ast.Inspect(fn.Body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.FuncLit:
			return false

		case *ast.AssignStmt:
			if n.Tok == token.ASSIGN {
				for _, lhs := range n.Lhs {
					if id, ok := lhs.(*ast.Ident); ok && isPackageVar(id) {
						assigns[id.Name] = true
						skip[id] = true
					}
				}
			}

		case *ast.BinaryExpr:
			if n.Op == token.EQL || n.Op == token.NEQ {
				for _, pair := range [][2]ast.Expr{{n.X, n.Y}, {n.Y, n.X}} {
					other, isNil := pair[1].(*ast.Ident)
					id, isID := pair[0].(*ast.Ident)
					if isNil && other.Name == "nil" && isID {
						skip[id] = true
					}
				}
			}

		case *ast.Ident:
			if !skip[n] && isPackageVar(n) {
				if _, seen := uses[n.Name]; !seen {
					uses[n.Name] = n.Pos()
				}
			}
		}
		return true
	})
	return uses, assigns
}

// dispatchOrderOf reads the dispatcher's calls, in order.
func dispatchOrderOf(file *ast.File) []string {
	var order []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "RegisterGeneratedFeatures" || fn.Body == nil {
			continue
		}
		for _, stmt := range fn.Body.List {
			expr, ok := stmt.(*ast.ExprStmt)
			if !ok {
				continue
			}
			call, ok := expr.X.(*ast.CallExpr)
			if !ok {
				continue
			}
			if id, ok := call.Fun.(*ast.Ident); ok {
				order = append(order, id.Name)
			}
		}
	}
	return order
}

func TestNoSetupFunctionUsesAHandleAssignedByOneDispatchedAfterIt(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/dispatch\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	// Everything that writes a block, in the order the end-to-end test applies it:
	// generators first, then every feature, then the view that needs Templates.
	for _, args := range e2eGenerators {
		if err := runGenerate(args); err != nil {
			t.Fatalf("breeze generate %s: %v", strings.Join(args, " "), err)
		}
	}
	for _, args := range e2eFeatures {
		if err := runAdd(args); err != nil {
			t.Fatalf("breeze add %s: %v", strings.Join(args, " "), err)
		}
	}
	if err := runGenerate([]string{"view", "About", "--data"}); err != nil {
		t.Fatalf("breeze generate view: %v", err)
	}

	fset := token.NewFileSet()
	// DeclarationErrors turns a name declared twice in the file into a parse error. Every
	// block is appended to one file, so two features that pick the same top-level name
	// (a helper, a package variable) only meet here, and the compiler is the only other
	// thing that would say so.
	file, err := parser.ParseFile(fset, filepath.Join(root, "features_generated.go"), nil, parser.DeclarationErrors)
	if err != nil {
		t.Fatalf("parsing what the generator wrote (a parse error here includes a top-level name declared by two blocks): %v", err)
	}

	funcs := map[string]*ast.FuncDecl{}
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Body != nil {
			funcs[fn.Name.Name] = fn
		}
	}

	order := dispatchOrderOf(file)
	position := map[string]int{}
	for i, name := range order {
		position[name] = i
	}

	// The premises, so a layout change cannot turn this into a test that checks
	// nothing and passes.
	if len(order) < 15 {
		t.Fatalf("the dispatcher calls only %d setup functions (%v); the test no longer sees the dispatcher", len(order), order)
	}
	fleetAt, hasFleet := position["setupFleet"]
	dashboardAt, hasDashboard := position["setupDashboard"]
	if !hasFleet || !hasDashboard {
		t.Fatalf("setupFleet and setupDashboard are not both dispatched: %v", order)
	}
	if fleetAt > dashboardAt {
		t.Fatalf("Fleet is now dispatched after the dashboard (%d > %d); the case this test exists for is gone", fleetAt, dashboardAt)
	}

	type facts struct {
		uses    map[string]token.Pos
		assigns map[string]bool
	}
	byName := map[string]facts{}
	for _, name := range order {
		fn, ok := funcs[name]
		if !ok {
			t.Errorf("the dispatcher calls %s, which features_generated.go does not define", name)
			continue
		}
		uses, assigns := setupTimeUses(fn, file.Scope)
		byName[name] = facts{uses: uses, assigns: assigns}
	}

	if !byName["setupDashboard"].assigns["DashboardCollector"] {
		t.Fatalf("setupDashboard no longer assigns DashboardCollector; assignments seen: %v", byName["setupDashboard"].assigns)
	}

	var problems []string
	for _, name := range order {
		for handle, pos := range byName[name].uses {
			if byName[name].assigns[handle] {
				continue
			}

			assignedBefore, assignedAfter := "", ""
			for _, other := range order {
				if !byName[other].assigns[handle] || other == name {
					continue
				}
				if position[other] < position[name] {
					assignedBefore = other
				} else if assignedAfter == "" {
					assignedAfter = other
				}
			}

			if assignedBefore == "" && assignedAfter != "" {
				problems = append(problems, fmt.Sprintf(
					"%s (dispatched #%d) uses %s at setup time (%s), but only %s (dispatched #%d) assigns it, so the use sees nil",
					name, position[name], handle, fset.Position(pos), assignedAfter, position[assignedAfter]))
			}
		}
	}

	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
}
