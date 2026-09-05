package generator

// naming_test.go — the --filename / --package contract.
//
// The tests are written against runGenerate rather than against outputFlags,
// because what is being claimed is that the two flags reach every generator
// without being registered by hand anywhere. A test that called target()
// directly would pass even if no generator had wired the flags up.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// kindsWithOutputFlags is every generator kind that writes a standalone Go file,
// with an argument list minimal enough to run in a bare project, and the
// directory it writes into.
//
// grpc and view are absent deliberately and are covered by
// TestKindsWithoutOutputFlagsRefuseThemWithAReason instead: grpc writes five
// files per service under a protoc-dictated layout, and view writes an HTML
// template plus a marker block, so neither has one Go file for the overrides to
// name.
var kindsWithOutputFlags = []struct {
	kind string
	dir  string
	args []string
}{
	{"handler", "handlers", []string{"handler", "Session", "--methods=list"}},
	{"resource", "handlers", []string{"resource", "Widget", "name:string"}},
	{"model", "models", []string{"model", "Gadget", "sku:string"}},
	{"event", "events", []string{"event", "ThingHappened", "id:int64"}},
	{"listener", "listeners", []string{"listener", "ThingHappened"}},
	{"workflow", "workflows", []string{"workflow", "Checkout", "--steps=pay"}},
	{"middleware", "middleware", []string{"middleware", "RequestID"}},
	{"ws", "ws", []string{"ws", "Chat"}},
	{"job", "jobs", []string{"job", "Sweep", "--every=1m"}},
}

// TestFilenameAndPackageOverrideEveryApplicableKind is the end-to-end claim: the
// file lands at exactly the given name, declaring exactly the given package.
//
// Every kind is covered rather than one, because the flags are only useful if
// they are uniform — a kind that silently ignored them would be worse than one
// that rejected them, since the file would appear somewhere else with no
// complaint.
func TestFilenameAndPackageOverrideEveryApplicableKind(t *testing.T) {
	for _, tc := range kindsWithOutputFlags {
		t.Run(tc.kind, func(t *testing.T) {
			projectDir(t)

			args := append(append([]string{}, tc.args...),
				"--"+outputFilenameFlag+"=custom_output.go",
				"--"+outputPackageFlag+"=custompkg")
			if err := runGenerate(args); err != nil {
				t.Fatalf("breeze generate %s: %v", strings.Join(args, " "), err)
			}

			path := filepath.Join(tc.dir, "custom_output.go")
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("--%s did not put the file at %s: %v", outputFilenameFlag, path, err)
			}
			if pkg, _ := declsIn(t, path); pkg != "custompkg" {
				t.Errorf("%s declares package %q, want custompkg", path, pkg)
			}

			// The default-named file must not also exist: an override that wrote
			// both would leave a duplicate declaration behind.
			if entries, err := os.ReadDir(tc.dir); err == nil {
				for _, e := range entries {
					if e.Name() != "custom_output.go" && strings.HasSuffix(e.Name(), ".go") {
						t.Errorf(
							"%s also wrote %s; the override should replace the default name, not add to it",
							tc.kind,
							filepath.Join(tc.dir, e.Name()),
						)
					}
				}
			}
			if len(src) == 0 {
				t.Error("the generated file is empty")
			}
		})
	}
}

// TestDefaultFilenameAndPackageAreUnchanged is the regression half: omitting both
// flags must reproduce exactly what the generators produced before they existed.
//
// The expected names are written out rather than derived, so a change to the
// derivation fails here instead of quietly renaming files in projects that
// already have them.
func TestDefaultFilenameAndPackageAreUnchanged(t *testing.T) {
	cases := []struct {
		args []string
		path string
		pkg  string
	}{
		{[]string{"handler", "Session", "--methods=list"}, "handlers/session.go", "handlers"},
		{[]string{"resource", "Widget", "name:string"}, "handlers/widget.go", "handlers"},
		{[]string{"model", "UserAccount", "sku:string"}, "models/user_account.go", "models"},
		{[]string{"event", "ThingHappened", "id:int64"}, "events/thing_happened.go", "events"},
		{[]string{"listener", "ThingHappened"}, "listeners/on_thing_happened.go", "listeners"},
		{[]string{"workflow", "Checkout", "--steps=pay"}, "workflows/checkout.go", "workflows"},
		{[]string{"middleware", "RequestID"}, "middleware/request_id.go", "middleware"},
		{[]string{"ws", "Chat"}, "ws/chat.go", "ws"},
		{[]string{"job", "Sweep", "--every=1m"}, "jobs/sweep.go", "jobs"},
	}

	for _, tc := range cases {
		t.Run(tc.args[0], func(t *testing.T) {
			projectDir(t)
			if err := runGenerate(tc.args); err != nil {
				t.Fatalf("breeze generate %s: %v", strings.Join(tc.args, " "), err)
			}

			path := filepath.FromSlash(tc.path)
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("the default output moved: %s is absent (%v)", tc.path, err)
			}
			if pkg, _ := declsIn(t, path); pkg != tc.pkg {
				t.Errorf("%s declares package %q, want %q", tc.path, pkg, tc.pkg)
			}
		})
	}
}

// TestKindsWithoutOutputFlagsRefuseThemWithAReason.
//
// grpc and view accept the flags on their FlagSet and then refuse them. The
// alternative — not registering them — makes the flag package report "flag
// provided but not defined", which reads as though --package does not exist at
// all rather than as "not for this kind", and sends the user looking for a
// spelling mistake.
func TestKindsWithoutOutputFlagsRefuseThemWithAReason(t *testing.T) {
	for _, args := range [][]string{
		{"grpc", "UserService"},
		{"view", "About"},
	} {
		t.Run(args[0], func(t *testing.T) {
			projectDir(t)
			err := runGenerate(append(args, "--"+outputPackageFlag+"=whatever"))
			if err == nil {
				t.Fatalf(
					"breeze generate %s accepted --%s, which it cannot honour",
					args[0],
					outputPackageFlag,
				)
			}
			msg := err.Error()
			if strings.Contains(msg, "not defined") {
				t.Errorf(
					"the refusal reads as an unknown flag rather than an inapplicable one: %v",
					err,
				)
			}
			for _, want := range []string{outputPackageFlag, outputFilenameFlag} {
				if !strings.Contains(msg, want) {
					t.Errorf("the error does not name --%s: %v", want, err)
				}
			}
		})
	}
}

// TestInvalidPackageNameIsRejectedWithASpecificError.
//
// Every one of these reaches gofmt as a syntax error otherwise, reported at a
// line and column in source the user never wrote. The assertion is on the
// wording as well as the failure, because "expected 'IDENT', found 'range'" does
// not tell anybody which flag to change.
func TestInvalidPackageNameIsRejectedWithASpecificError(t *testing.T) {
	cases := map[string]string{
		"1models":      "identifier",
		"range":        "keyword",
		"my-models":    "identifier",
		"models.thing": "identifier",
		"_":            "blank identifier",
		"":             "", // empty means "flag not passed", so nothing is rejected
	}

	for value, want := range cases {
		if value == "" {
			continue
		}
		t.Run(value, func(t *testing.T) {
			projectDir(t)
			err := runGenerate(
				[]string{"model", "Gadget", "sku:string", "--" + outputPackageFlag + "=" + value},
			)
			if err == nil {
				t.Fatalf("--%s=%s was accepted", outputPackageFlag, value)
			}
			msg := err.Error()
			if !strings.Contains(msg, outputPackageFlag) {
				t.Errorf("the error does not name the flag that caused it: %v", err)
			}
			if !strings.Contains(msg, value) {
				t.Errorf("the error does not quote the offending value: %v", err)
			}
			if !strings.Contains(msg, want) {
				t.Errorf(
					"the error does not say why %q is illegal (want it to mention %q): %v",
					value,
					want,
					err,
				)
			}
		})
	}
}

// TestPackageThatDisagreesWithItsDirectoryIsRejected.
//
// Go permits one package per directory. Writing `package widgets` next to
// `package handlers` compiles nowhere, and the compiler reports it as "found
// packages handlers and widgets", naming neither the flag nor the file that
// introduced the second one.
func TestPackageThatDisagreesWithItsDirectoryIsRejected(t *testing.T) {
	projectDir(t)

	// An existing handlers package, written by the real generator.
	if err := runGenerate([]string{"handler", "Session", "--methods=list"}); err != nil {
		t.Fatalf("breeze generate handler: %v", err)
	}

	err := runGenerate([]string{
		"resource", "Widget", "name:string",
		"--" + outputPackageFlag + "=widgets",
	})
	if err == nil {
		t.Fatal("a second package was accepted in a directory that already has one")
	}
	msg := err.Error()
	for _, want := range []string{"handlers", "widgets", "one package per directory"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

// TestDerivedPackageGoesThroughTheSameValidation.
//
// The check has to be global rather than conditional on the flag being passed.
// Here nothing is overridden except the filename, which points a kind at a
// directory whose package is already something else — a mistake reachable with no
// --package at all.
func TestDerivedPackageGoesThroughTheSameValidation(t *testing.T) {
	projectDir(t)

	// models/ declaring `package models`, then a listener whose derived package
	// would be `listeners` — but --filename cannot move it out of listeners/, so
	// the collision has to be provoked the other way round: put a foreign package
	// into the directory the kind writes to.
	if err := os.MkdirAll("models", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("models", "hand_written.go"),
		[]byte("package somethingelse\n\nvar X = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := runGenerate([]string{"model", "Gadget", "sku:string"})
	if err == nil {
		t.Fatal("a derived package that disagrees with its directory was accepted")
	}
	if !strings.Contains(err.Error(), "somethingelse") {
		t.Errorf("the error does not name the package already in the directory: %v", err)
	}
}
