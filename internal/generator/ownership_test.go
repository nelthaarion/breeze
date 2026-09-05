package generator

// ownership_test.go — the file-ownership half of the collision rules.
//
// The claim under test is narrow and worth stating precisely: a generator will
// not write over a file another generator owns, and --force does not change that.
// --force means "replace what I generated", and reading it as "delete another
// feature's work" is what a mistyped --filename would otherwise do.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFilenameCollisionWithAnotherFeaturesFileIsRefused.
//
// Both kinds write into handlers/, so pointing the resource generator at the
// handler generator's file needs no contrivance — it is one --filename away, and
// before ownership was checked it silently replaced a file the other generator
// still believes it owns.
func TestFilenameCollisionWithAnotherFeaturesFileIsRefused(t *testing.T) {
	projectDir(t)

	if err := runGenerate([]string{"handler", "Session", "--methods=list"}); err != nil {
		t.Fatalf("breeze generate handler: %v", err)
	}
	owned := filepath.Join("handlers", "session.go")
	before, err := os.ReadFile(owned)
	if err != nil {
		t.Fatal(err)
	}

	err = runGenerate([]string{
		"resource", "Widget", "name:string",
		"--" + outputFilenameFlag + "=session.go",
	})
	if err == nil {
		t.Fatal("the resource generator overwrote a file the handler generator owns")
	}

	msg := err.Error()
	for _, want := range []string{"session.go", "breeze generate handler", "breeze generate resource"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
	// The refusal has to be complete: a partially written file is worse than
	// either outcome, because the project then compiles as neither feature.
	after, err := os.ReadFile(owned)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Error("the refused generation still modified the file")
	}
}

// TestForceDoesNotOverrideAnotherFeaturesOwnership.
//
// --force exists to replace a stub the user has since filled in. Extending it to
// cross-feature overwrites would make the flag mean two different things, one of
// which destroys work the user did not name.
func TestForceDoesNotOverrideAnotherFeaturesOwnership(t *testing.T) {
	projectDir(t)

	if err := runGenerate([]string{"handler", "Session", "--methods=list"}); err != nil {
		t.Fatalf("breeze generate handler: %v", err)
	}

	err := runGenerate([]string{
		"resource", "Widget", "name:string",
		"--" + outputFilenameFlag + "=session.go", "--force",
	})
	if err == nil {
		t.Fatal("--force overwrote another generator's file")
	}
	if !strings.Contains(err.Error(), "--force does not apply here") {
		t.Errorf("the error does not explain that --force is not the escape hatch: %v", err)
	}
}

// TestSameGeneratorStillNeedsForceToReplaceItsOwnFile — the unchanged half.
//
// Ownership must not turn a re-run into a hard failure: the same command writing
// the same file is the ordinary case, and it keeps the pre-existing rule of
// refusing without --force and proceeding with it.
func TestSameGeneratorStillNeedsForceToReplaceItsOwnFile(t *testing.T) {
	projectDir(t)

	if err := runGenerate([]string{"model", "Gadget", "sku:string"}); err != nil {
		t.Fatalf("breeze generate model: %v", err)
	}

	err := runGenerate([]string{"model", "Gadget", "sku:string", "price:float64"})
	if err == nil {
		t.Fatal("a re-run replaced the file without --force")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("the error does not name the escape hatch: %v", err)
	}

	if err := runGenerate(
		[]string{"model", "Gadget", "sku:string", "price:float64", "--force"},
	); err != nil {
		t.Fatalf("--force did not let the owning generator replace its own file: %v", err)
	}
	src, err := os.ReadFile(filepath.Join("models", "gadget.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "Price") {
		t.Error("--force did not actually rewrite the file")
	}
}

// TestHandWrittenFileIsNotTreatedAsAnotherFeaturesProperty.
//
// A file with no generated header is the user's, not a rival generator's, and the
// pre-existing --force rule is the right one for it. Refusing outright would make
// the generators unable to write into a project that happens to have a file of
// that name.
func TestHandWrittenFileIsNotTreatedAsAnotherFeaturesProperty(t *testing.T) {
	projectDir(t)

	if err := os.MkdirAll("models", 0o755); err != nil {
		t.Fatal(err)
	}
	handWritten := filepath.Join("models", "gadget.go")
	if err := os.WriteFile(handWritten, []byte("package models\n\n// mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := runGenerate([]string{"model", "Gadget", "sku:string"})
	if err == nil {
		t.Fatal("a hand-written file was replaced without --force")
	}
	if strings.Contains(err.Error(), "owned by") {
		t.Errorf("a hand-written file was reported as another feature's: %v", err)
	}

	if err := runGenerate([]string{"model", "Gadget", "sku:string", "--force"}); err != nil {
		t.Fatalf("--force did not replace a hand-written file: %v", err)
	}
}

// TestEveryGeneratedFileRecordsItsOwner is what the check above depends on.
//
// Ownership is read back out of the header, so a kind that stopped writing one
// would silently become overwritable by every other kind — and nothing else in
// the suite would notice.
func TestEveryGeneratedFileRecordsItsOwner(t *testing.T) {
	for _, tc := range kindsWithOutputFlags {
		t.Run(tc.kind, func(t *testing.T) {
			projectDir(t)
			if err := runGenerate(tc.args); err != nil {
				t.Fatalf("breeze generate %s: %v", strings.Join(tc.args, " "), err)
			}

			entries, err := os.ReadDir(tc.dir)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, e := range entries {
				if !strings.HasSuffix(e.Name(), ".go") {
					continue
				}
				path := filepath.Join(tc.dir, e.Name())
				owner := fileOwner(path)
				if owner == "" {
					t.Errorf(
						"%s carries no generated-by header, so no other generator can tell it is owned",
						path,
					)
					continue
				}
				if want := generateOwner(tc.kind); owner != want {
					t.Errorf("%s reports owner %q, want %q", path, owner, want)
				}
				found = true
			}
			if !found {
				t.Errorf("%s wrote no Go file into %s", tc.kind, tc.dir)
			}
		})
	}
}
