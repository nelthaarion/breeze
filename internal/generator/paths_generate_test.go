package generator

// paths_generate_test.go — the `breeze generate` half of the path-flag audit.
//
// paths_test.go covers `breeze add`, deriving its list from the feature registry. The
// generate kinds are a separate registry with their own FlagSets, and they were the half
// that still had a hole after the features were fixed.

import (
	"os"
	"strings"
	"testing"
)

// TestEveryGenerateKindPathFlagIsValidated is the `breeze generate` half of the check
// above.
//
// TestEveryFeaturePathFlagIsValidated derives its list from the feature registry, which
// covers `breeze add`. The generate kinds are a separate registry with separate
// FlagSets, and `generate view --views` was unvalidated after every feature flag was
// fixed — it wrote its HTML template wherever it was pointed and embedded that path in
// the generated route, so the app then read templates from outside the project too.
//
// Discovery is by flag name rather than by default value: unlike the feature flags,
// these defaults are bare relative paths (`views`, `migrations`) rather than `./`-
// prefixed, so the convention the other test relies on does not apply here. `path` is
// excluded because on these kinds it is a *route* path — `--path=/api/v1/users` — and
// refusing a leading slash there would break the flag's actual purpose.
func TestEveryGenerateKindPathFlagIsValidated(t *testing.T) {
	pathFlagNames := map[string]bool{
		"dir": true, "views": true, "components": true, "layout": true,
		"root": true, "file": true, "out": true, "output": true,
	}

	const traversal = "../../../../breeze-escape"
	checked := 0

	for _, kind := range generatorNames() {
		g := generators[kind]

		// A throwaway FlagSet to discover the names: Build-style registration
		// captures pointers, so the set used for discovery cannot be the set the
		// run uses. Each generator registers its own flags inside Run, so the
		// names are discovered by running with --help-style parse failure instead —
		// which is why this drives the real entry point below and reads the error.
		for name := range pathFlagNames {
			args := []string{kind}
			if g.NeedsName {
				args = append(args, "Escape")
			}
			// model and resource need at least one field spec, and a generator
			// that refused for a missing field would look like it refused the flag.
			if kind == "model" || kind == "resource" {
				args = append(args, "title:string")
			}
			if kind == "workflow" {
				args = append(args, "--steps=one,two")
			}
			args = append(args, "--"+name+"="+traversal)

			err := runGeneratorInScratch(t, args)
			if err == nil {
				t.Errorf("`breeze generate %s --%s=%s` was accepted. This flag names a location "+
					"inside the project, and breeze_generate forwards a caller's flags object to "+
					"this FlagSet verbatim — so call validatePathFlag on it",
					kind, name, traversal)
				continue
			}
			// "flag provided but not defined" means this kind has no such flag,
			// which is the common case and not a finding.
			if strings.Contains(err.Error(), "not defined") {
				continue
			}
			checked++
		}
	}

	if checked == 0 {
		t.Fatal("no path-like generate flags were exercised, so this test is asserting nothing")
	}
}

// runGeneratorInScratch runs one `breeze generate` invocation in a fresh project.
//
// A fresh directory per call, because these generators write files and read the ones
// they wrote: a shared directory would make the second call's outcome depend on the
// first's. The templates feature is added because `generate view` requires it.
func runGeneratorInScratch(t *testing.T, args []string) error {
	t.Helper()

	dir := t.TempDir()
	t.Chdir(dir)

	if err := os.WriteFile("go.mod", []byte("module example.com/proj\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// view refuses without it, and that refusal would mask the flag's outcome.
	if err := Add([]string{"templates"}); err != nil {
		t.Fatalf("adding templates to the scratch project: %v", err)
	}
	return Generate(args)
}
