package generator

// Declaration-level tests for the generators added in this change.
//
// Every generator routes its output through format.Source, so "it generated
// without an error" already means "the file parses". These tests assert the
// next thing up: that the declarations a caller is told to use are actually the
// ones emitted. A generator that silently renamed Register<Listener> to
// Subscribe<Listener> would still format, still compile in isolation, and still
// break the note printed next to it.
//
// They drive runGenerate rather than the generate* functions directly, so a
// generator missing from the dispatch table fails here too.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// projectDir sets up a temp directory that looks enough like a Breeze project
// for the generators: they need go.mod for the module path, and the ones that
// write feature blocks need somewhere to put them.
func projectDir(t *testing.T) {
	t.Helper()
	t.Chdir(t.TempDir())
	if err := os.WriteFile(
		"go.mod",
		[]byte("module example.com/proj\n\ngo 1.25\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
}

// declsIn parses a generated file and returns its package name plus the set of
// top-level declarations. Methods are keyed "Type.Method".
func declsIn(t *testing.T, path string) (string, map[string]bool) {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		src, _ := os.ReadFile(path)
		t.Fatalf("generated %s does not parse: %v\n%s", path, err, src)
	}

	decls := map[string]bool{}
	for _, d := range file.Decls {
		switch decl := d.(type) {
		case *ast.FuncDecl:
			name := decl.Name.Name
			if decl.Recv != nil && len(decl.Recv.List) == 1 {
				name = receiverTypeName(decl.Recv.List[0].Type) + "." + name
			}
			decls[name] = true
		case *ast.GenDecl:
			for _, spec := range decl.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					decls[s.Name.Name] = true
				case *ast.ValueSpec:
					for _, n := range s.Names {
						decls[n.Name] = true
					}
				}
			}
		}
	}
	return file.Name.Name, decls
}

func receiverTypeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return receiverTypeName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr: // a generic receiver, Foo[T]
		return receiverTypeName(t.X)
	}
	return ""
}

func TestGeneratorDeclarations(t *testing.T) {
	cases := []struct {
		desc string
		args []string
		file string
		pkg  string
		want []string
	}{
		{
			desc: "event",
			args: []string{"event", "UserCreated", "id:int64", "email:string"},
			file: filepath.Join("events", "user_created.go"),
			pkg:  "events",
			// Emit/On are the pair the printed notes tell the user to call.
			want: []string{"UserCreated", "EmitUserCreated", "OnUserCreated"},
		},
		{
			desc: "listener with --name",
			args: []string{"listener", "UserCreated", "--name=SendWelcomeEmail"},
			file: filepath.Join("listeners", "send_welcome_email.go"),
			pkg:  "listeners",
			want: []string{"SendWelcomeEmail", "RegisterSendWelcomeEmail"},
		},
		{
			desc: "workflow with retry and compensation",
			args: []string{
				"workflow",
				"Signup",
				"--steps=validate,charge-card",
				"--retry",
				"--compensate",
			},
			file: filepath.Join("workflows", "signup.go"),
			pkg:  "workflows",
			// The hyphen in charge-card must become an identifier fragment, and
			// --compensate must produce a paired Undo for every step.
			want: []string{
				"SignupName", "Signup",
				"signupValidate", "signupChargeCard",
				"signupValidateUndo", "signupChargeCardUndo",
			},
		},
		{
			desc: "workflow without compensation",
			args: []string{"workflow", "Checkout", "--steps=pay"},
			file: filepath.Join("workflows", "checkout.go"),
			pkg:  "workflows",
			want: []string{"CheckoutName", "Checkout", "checkoutPay"},
		},
		{
			desc: "middleware",
			args: []string{"middleware", "RequestID"},
			file: filepath.Join("middleware", "request_id.go"),
			pkg:  "middleware",
			want: []string{"RequestID"},
		},
		{
			desc: "ws handler",
			args: []string{"ws", "Chat"},
			file: filepath.Join("ws", "chat.go"),
			pkg:  "ws",
			// OnConnect/OnMessage/OnClose are what makes it a breeze.WSHandler;
			// dropping one turns a compile error in the user's project into the
			// only signal.
			want: []string{
				"Chat", "NewChat", "Chat.SetHub",
				"Chat.OnConnect", "Chat.OnMessage", "Chat.OnClose",
			},
		},
		{
			desc: "model",
			args: []string{"model", "Product", "sku:string", "price:float64"},
			file: filepath.Join("models", "product.go"),
			pkg:  "models",
			want: []string{"Product", "ProductTable", "ProductColumns", "Product.ScanDest"},
		},
		{
			desc: "job",
			args: []string{"job", "CleanupSessions", "--every=30s"},
			file: filepath.Join("jobs", "cleanup_sessions.go"),
			pkg:  "jobs",
			want: []string{
				"CleanupSessionsName", "CleanupSessionsInterval",
				"CleanupSessions", "NewCleanupSessions",
				"CleanupSessions.Run", "CleanupSessions.Start",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			projectDir(t)
			if err := runGenerate(tc.args); err != nil {
				t.Fatalf("breeze generate %s: %v", strings.Join(tc.args, " "), err)
			}

			pkg, decls := declsIn(t, tc.file)
			if pkg != tc.pkg {
				t.Errorf("%s declares package %q, want %q", tc.file, pkg, tc.pkg)
			}
			for _, name := range tc.want {
				if !decls[name] {
					t.Errorf("%s does not declare %s (got: %s)", tc.file, name, sortedKeys(decls))
				}
			}
		})
	}
}

// TestGenerateWorkflowWithoutCompensateEmitsNoUndo is the negative half of the
// --compensate case above: an Undo stub emitted when nothing references it is
// dead code the user has to work out the purpose of.
func TestGenerateWorkflowWithoutCompensateEmitsNoUndo(t *testing.T) {
	projectDir(t)
	if err := runGenerate([]string{"workflow", "Checkout", "--steps=pay,ship"}); err != nil {
		t.Fatal(err)
	}
	_, decls := declsIn(t, filepath.Join("workflows", "checkout.go"))
	for _, name := range []string{"checkoutPayUndo", "checkoutShipUndo"} {
		if decls[name] {
			t.Errorf("%s emitted without --compensate", name)
		}
	}
}

// TestGenerateModelWritesMigration covers the paired SQL, which is the half of
// `generate model` that is not Go and so not covered by format.Source at all.
func TestGenerateModelWritesMigration(t *testing.T) {
	projectDir(t)
	if err := runGenerate([]string{"model", "Product", "sku:string", "--timestamps"}); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatalf("no migrations directory: %v", err)
	}
	var up, down string
	for _, e := range entries {
		switch {
		case strings.HasSuffix(e.Name(), ".up.sql"):
			up = e.Name()
		case strings.HasSuffix(e.Name(), ".down.sql"):
			down = e.Name()
		}
	}
	if up == "" || down == "" {
		t.Fatalf("expected an up and a down migration, got %v", entries)
	}
	// The version prefix is what `breeze migrate` orders by.
	if !strings.HasPrefix(up, "0001_") {
		t.Errorf("first migration is %q, expected an 0001_ prefix", up)
	}

	upSQL, err := os.ReadFile(filepath.Join("migrations", up))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"CREATE TABLE", "sku", "created_at", "updated_at"} {
		if !strings.Contains(string(upSQL), want) {
			t.Errorf("up migration missing %q:\n%s", want, upSQL)
		}
	}

	downSQL, err := os.ReadFile(filepath.Join("migrations", down))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(downSQL), "DROP TABLE") {
		t.Errorf("down migration does not drop the table:\n%s", downSQL)
	}
}

// TestGenerateModelNoMigration checks the escape hatch, for a model mapped to a
// table that already exists.
func TestGenerateModelNoMigration(t *testing.T) {
	projectDir(t)
	if err := runGenerate(
		[]string{"model", "Product", "sku:string", "--no-migration"},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat("migrations"); !os.IsNotExist(err) {
		t.Errorf("--no-migration still created the migrations directory (err=%v)", err)
	}
}

// TestGenerateModelRejectsValidationRules checks the wiring, not the parser: a
// rules segment must fail at the command, since the model struct carries db
// tags and is filled by Scan rather than bound from a request.
func TestGenerateModelRejectsValidationRules(t *testing.T) {
	projectDir(t)
	err := runGenerate([]string{"model", "Product", "sku:string:required"})
	if err == nil {
		t.Fatal("generate model accepted validation rules")
	}
	if !strings.Contains(err.Error(), "generate resource") {
		t.Errorf("error should name the generator that does validate, got: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join("models", "product.go")); statErr == nil {
		t.Error("wrote the model anyway")
	}
}

// TestGenerateViewRequiresTemplates pins the hard requirement rather than
// letting it fail later as a nil template engine at request time.
func TestGenerateViewRequiresTemplates(t *testing.T) {
	projectDir(t)
	err := runGenerate([]string{"view", "About"})
	if err == nil {
		t.Fatal("generate view succeeded with no templates block")
	}
	if !strings.Contains(err.Error(), "breeze add templates") {
		t.Errorf("error should name the fix, got: %v", err)
	}
}

func TestGenerateViewAfterTemplates(t *testing.T) {
	projectDir(t)
	if err := runAdd([]string{"templates"}); err != nil {
		t.Fatalf("breeze add templates: %v", err)
	}
	if err := runGenerate([]string{"view", "About", "--data"}); err != nil {
		t.Fatalf("breeze generate view: %v", err)
	}

	html, err := os.ReadFile(filepath.Join("views", "about.html"))
	if err != nil {
		t.Fatal(err)
	}
	// The layout renders a "content" block; a view that defines anything else
	// renders as a blank page.
	if !strings.Contains(string(html), `{{define "content"}}`) {
		t.Errorf("view does not define the content block:\n%s", html)
	}

	// The route and its data function belong to the features file, because
	// router.View captures the engine pointer and Templates is nil until the
	// templates block has run.
	_, decls := declsIn(t, featuresFileName)
	for _, name := range []string{"setupViewAbout", "aboutData"} {
		if !decls[name] {
			t.Errorf("%s missing from %s (got: %s)", name, featuresFileName, sortedKeys(decls))
		}
	}
}

// TestGeneratedBlocksLandInFeaturesFile guards the placement decision that both
// the ws and view generators depend on: their setup needs *breeze.Breeze or a
// Templates that only exists once another block has run, neither of which is
// available in routes_generated.go.
func TestGeneratedBlocksLandInFeaturesFile(t *testing.T) {
	projectDir(t)
	if err := runGenerate([]string{"ws", "Chat"}); err != nil {
		t.Fatal(err)
	}
	if _, decls := declsIn(t, featuresFileName); !decls["setupWsChat"] {
		t.Errorf("setupWsChat not in %s", featuresFileName)
	}
	if _, err := os.Stat(registryFileName); err == nil {
		if _, decls := declsIn(t, registryFileName); decls["setupWsChat"] {
			t.Errorf("setupWsChat landed in %s, where app is not in scope", registryFileName)
		}
	}
}

func sortedKeys(m map[string]bool) string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	// Sorted so a failure message is stable between runs.
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	return strings.Join(names, ", ")
}
