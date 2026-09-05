package generator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func generateTestResourceFile(t *testing.T) string {
	t.Helper()
	return generateTestResourceFileWith(t, []string{"name:string", "age:int"})
}

// generateTestResourceFileWith runs the same two steps generateResource does â€”
// parse, then fill in defaults â€” so a test sees what a user would get rather
// than what the raw parser returned.
func generateTestResourceFileWith(t *testing.T, args []string) string {
	t.Helper()
	t.Chdir(t.TempDir())
	fields, err := parseFields(args)
	if err != nil {
		t.Fatal(err)
	}
	fields = withValidationDefaults(fields)
	actions, err := actionsFor("User", "Users", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeResourceHandlerFile("User", "Users", "/users", fields, actions, false); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(filepath.Join("handlers", "user.go"))
	if err != nil {
		t.Fatal(err)
	}
	return string(src)
}

// TestResourceHandlerBindsThroughBindingPackage pins the decode path.
//
// It used to be json.Unmarshal straight into the request struct, which meant
// the validate tags were inert and `generate resource` advertised "validation"
// in its help while emitting a handler that accepted an empty body. Only
// binding.Bind reads those tags.
func TestResourceHandlerBindsThroughBindingPackage(t *testing.T) {
	src := generateTestResourceFile(t)
	if !strings.Contains(src, `"github.com/nelthaarion/breeze/v2binding"`) {
		t.Error("generated handler does not import the binding package")
	}
	if !strings.Contains(src, "binding.Bind(&req, binding.JSONBody(ctx.Req.Body))") {
		t.Error("generated handler does not decode through binding.Bind")
	}
	if strings.Contains(src, "json.Unmarshal") {
		t.Error("generated handler still decodes with json.Unmarshal, which ignores validate tags")
	}
}

// TestResourceHandlerSeparates422From400 checks the two failure modes stay
// distinct: a body that is not JSON is a bad request, a body that is JSON but
// breaks a rule is understood and refused.
func TestResourceHandlerSeparates422From400(t *testing.T) {
	src := generateTestResourceFile(t)
	for _, want := range []string{
		"var ve *binding.ValidationError",
		"errors.As(err, &ve)",
		"ctx.Status(422)",
		"ctx.JSON(ve.ToProblemJSON())",
		"ctx.Status(400)",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("generated handler is missing %q", want)
		}
	}
}

// TestResourceHandlerOmitsDeadValidationBranch is the other half: with nothing
// to validate, the 422 branch can never be reached and the errors import would
// not compile.
func TestResourceHandlerOmitsDeadValidationBranch(t *testing.T) {
	// age:int gets no inferred rule, so this resource has no validation at all.
	src := generateTestResourceFileWith(t, []string{"age:int"})
	if strings.Contains(src, "ValidationError") {
		t.Error("emitted an unreachable 422 branch for a resource with no rules")
	}
	if strings.Contains(src, `"errors"`) {
		t.Error("imported errors without using it")
	}
	if !strings.Contains(src, "binding.Bind") {
		t.Error("should still bind through the binding package")
	}
}

// TestResourceValidateTagsReachTheStruct covers inference and explicit rules in
// one pass, since the interesting part is which fields get what.
func TestResourceValidateTagsReachTheStruct(t *testing.T) {
	src := generateTestResourceFileWith(t, []string{
		"name:string",
		"email:string",
		"age:int",
		"active:bool",
		"role:string:required,oneof=admin viewer",
	})
	// gofmt pads struct fields into columns, so compare with runs of spaces
	// collapsed rather than against a guess at the alignment.
	got := collapseSpaces(src)

	for _, want := range []string{
		"Name string `json:\"name\" validate:\"required\"`",
		"Email string `json:\"email\" validate:\"required,email\"`",
		"Role string `json:\"role\" validate:\"required,oneof=admin viewer\"`",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("generated struct is missing:\n  %s\ngot:\n%s", want, src)
		}
	}

	// required means "non-zero" to binding's validator, so inferring it here
	// would reject age 0 and active false â€” a generated bug, not a convenience.
	if strings.Contains(got, "Age int `json:\"age\" validate:") {
		t.Error("inferred a rule for an int field; age=0 would be rejected")
	}
	if strings.Contains(got, "Active bool `json:\"active\" validate:") {
		t.Error("inferred a rule for a bool field; active=false would be rejected")
	}
}

// collapseSpaces reduces every run of spaces to one, so an assertion about
// generated source does not have to reproduce gofmt's column alignment.
func collapseSpaces(s string) string {
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return s
}

func TestResourceStoreMarkedAsDemo(t *testing.T) {
	src := generateTestResourceFile(t)
	if !strings.Contains(src, "In-memory store for scaffolding only") {
		t.Error("generated in-memory store is not marked as scaffolding/demo code")
	}
}

func TestNoValidateFlagSuppressesInference(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := generateResource("example.com/x", "User", []string{"name:string", "--no-validate"}); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(filepath.Join("handlers", "user.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "validate:") {
		t.Errorf("--no-validate still emitted validate tags:\n%s", src)
	}
	// Explicit rules are a different question from inferred ones; the flag only
	// turns off inference, and binding.Bind stays either way.
	if !strings.Contains(string(src), "binding.Bind") {
		t.Error("--no-validate should not change the decode path")
	}
}

func TestParseFieldsEmptyNameNoPanic(t *testing.T) {
	for _, c := range []string{"", ":", ":string", "name:"} {
		if _, err := parseFields([]string{c}); err == nil {
			t.Errorf("parseFields(%q) expected error, got nil", c)
		}
	}
}

// TestParseFieldsValidationRules covers the rules segment. Rule names are
// checked at generate time because binding's applyRule switch has no default:
// an unrecognised rule validates nothing and reports nothing.
func TestParseFieldsValidationRules(t *testing.T) {
	ok := []struct {
		arg  string
		want string
	}{
		{"name:string", ""},
		{"name:string:required", "required"},
		{"name:string:required,min=2,max=40", "required,min=2,max=40"},
		{"email:string:required,email", "required,email"},
		{"role:string:oneof=a b c", "oneof=a b c"},
		{"age:int:min=0,max=130", "min=0,max=130"},
		// Whitespace and empty segments are tolerated rather than becoming an
		// empty rule in the tag, which binding would skip anyway.
		{"name:string: required , min=2 ,", "required,min=2"},
	}
	for _, c := range ok {
		fields, err := parseFields([]string{c.arg})
		if err != nil {
			t.Errorf("parseFields(%q) = %v, want no error", c.arg, err)
			continue
		}
		if fields[0].Validate != c.want {
			t.Errorf("parseFields(%q).Validate = %q, want %q", c.arg, fields[0].Validate, c.want)
		}
	}

	bad := []string{
		"name:string:requird",      // typo binding would silently ignore
		"name:string:min",          // min needs an argument
		"name:string:oneof=",       // so does oneof
		"name:string:required=yes", // required takes none
		"age:int:email",            // checkEmail returns false for non-strings
		"name:notatype:required",   // type still validated
	}
	for _, arg := range bad {
		if _, err := parseFields([]string{arg}); err == nil {
			t.Errorf("parseFields(%q) expected an error, got nil", arg)
		}
	}
}

func TestResourceRoutesUseScalarPackage(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := generateResource("example.com/x", "User", []string{"name:string"}); err != nil {
		t.Fatal(err)
	}
	routes, err := os.ReadFile(registryFileName)
	if err != nil {
		t.Fatal(err)
	}
	src := string(routes)
	if !strings.Contains(src, `"github.com/nelthaarion/breeze/v2/scalar"`) || !strings.Contains(src, "scalar.RouteDoc") {
		t.Errorf("routes must use the scalar package (middleware.Doc* takes scalar.RouteDoc), got:\n%s", src)
	}
	if strings.Contains(src, "swagger.") {
		t.Errorf("routes still reference the old swagger package:\n%s", src)
	}
}
