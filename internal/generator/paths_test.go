package generator

// paths_test.go — adversarial tests for the path-like generator flags.
//
// These matter because breeze_generate forwards its `flags` object to the generator's
// FlagSet verbatim, so `{"flags": {"dir": "../../../../etc"}}` reaches --dir. internal/mcp
// confines the working directory the generator runs *in*; only this package can confine
// what the generator does with a path once it is there.

import (
	"flag"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

// TestValidatePathFlagRefusesEscapes is the traversal set.
//
// filepath.Clean is what makes this work: "a/../.." reduces to ".." and a textual search
// for ".." would both miss it and refuse "..foo", which is a legal directory name.
func TestValidatePathFlagRefusesEscapes(t *testing.T) {
	for _, value := range []string{
		"..",
		"../etc",
		"../../etc",
		"migrations/../../../../tmp",
		"./../..",
		"a/b/../../..",
		`..\windows`,
		`a\..\..\..`,
	} {
		if err := validatePathFlag("dir", value); err == nil {
			t.Errorf("validatePathFlag(%q) was accepted; it resolves outside the project", value)
		}
	}
}

// TestValidatePathFlagRefusesAbsolutePaths covers the case needing no cleverness.
//
// An absolute path is refused rather than resolved-and-compared: one that happens to point
// inside the project is a value nobody types, so accepting it would add a second shape to
// validate for no benefit.
func TestValidatePathFlagRefusesAbsolutePaths(t *testing.T) {
	for _, value := range []string{
		"/etc",
		"/tmp/x",
		`C:\Windows`,
		`C:/Windows`,
		`\\server\share`,
		`\windows`,
		"/",
		// Volume-relative on Windows: neither absolute nor safely relative, because it
		// resolves against that volume's own current directory rather than this
		// process's working directory.
		"C:migrations",
	} {
		if err := validatePathFlag("dir", value); err == nil {
			t.Errorf("validatePathFlag(%q) was accepted; this flag names a location inside the "+
				"project", value)
		}
	}
}

// TestValidatePathFlagAcceptsProjectPaths is the necessary other half: every default this
// package ships is a relative path, and a check that refused them would break every
// generator.
func TestValidatePathFlagAcceptsProjectPaths(t *testing.T) {
	for _, value := range []string{
		"",
		"   ",
		"migrations",
		"./migrations",
		"./locales",
		"./public",
		"./media",
		"./views",
		"./components",
		"./views/layout.html",
		"db/migrations",
		"a/b/c/d",
		"..foo",
		"foo..bar",
		filepath.Join("nested", "deeper"),
	} {
		if err := validatePathFlag("dir", value); err != nil {
			t.Errorf("validatePathFlag(%q) was refused: %v — this is a default or an ordinary "+
				"project-relative value", value, err)
		}
	}
}

// TestValidatePathFlagNamesTheFlag checks the message. A command with several path
// arguments that reports only "invalid path" makes the caller guess which one.
func TestValidatePathFlagNamesTheFlag(t *testing.T) {
	err := validatePathFlag("layout", "../../etc/passwd")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "--layout") {
		t.Errorf("the refusal does not name the flag: %v", err)
	}
}

// TestEveryFeaturePathFlagIsValidated derives the check instead of trusting a list.
//
// The individual call sites are easy to audit today and impossible to keep audited: a new
// feature with a `--root` or `--dir` flag would inherit the traversal hole silently,
// because nothing in the feature registry requires a path flag to be validated. So this
// finds them the way the DependsOn test finds dependencies — by flipping the input and
// observing the generator.
//
// A flag is treated as path-like when its default starts with "./", which is this
// package's convention for "a location inside the project" and is what distinguishes
// --root=./public from --prefix=/public. The two are otherwise indistinguishable from
// outside the feature.
func TestEveryFeaturePathFlagIsValidated(t *testing.T) {
	// Generation builds strings, but chdir into a scratch directory so a feature that
	// stats anything cannot see this repository.
	t.Chdir(t.TempDir())

	const traversal = "../../../../etc/breeze-escape"
	checked := 0

	for _, name := range featureNames() {
		f := features[name]

		// The flag names have to come from a throwaway FlagSet: Build captures
		// pointers into whichever set it was given, so the set used for discovery
		// cannot be the set used for the run.
		var pathFlags []string
		discovery := flag.NewFlagSet("discover "+name, flag.ContinueOnError)
		discovery.SetOutput(io.Discard)
		f.Build(discovery)
		discovery.VisitAll(func(fl *flag.Flag) {
			if strings.HasPrefix(fl.DefValue, "./") {
				pathFlags = append(pathFlags, fl.Name)
			}
		})
		if len(pathFlags) == 0 {
			continue
		}

		// Baseline: with defaults this feature generates cleanly, so a failure below
		// is attributable to the flag rather than to the feature.
		if _, err := runFeatureWithFlags(t, f, nil); err != nil {
			t.Fatalf("feature %q failed with default flags, so this test cannot attribute a "+
				"later failure to a flag: %v", name, err)
		}

		for _, flagName := range pathFlags {
			checked++
			_, err := runFeatureWithFlags(t, f, []string{"-" + flagName + "=" + traversal})
			if err == nil {
				t.Errorf("feature %q accepted --%s=%s. This flag names a location inside the "+
					"project and is both written to and embedded in the generated code, and "+
					"breeze_generate forwards a caller's flags object to this FlagSet verbatim — "+
					"so call validatePathFlag on it", name, flagName, traversal)
			}
		}
	}

	if checked == 0 {
		t.Fatal("no path-like feature flags were found; the discovery convention (a default " +
			"starting with \"./\") no longer matches the registry, so this test is asserting nothing")
	}
}

// runFeatureWithFlags builds one feature with the given command-line arguments and
// returns what it produced.
//
// Unlike generateFeature it returns the error rather than failing, because the caller
// above is asserting that some arguments *are* refused.
func runFeatureWithFlags(t *testing.T, f *feature, args []string) (featureOutput, error) {
	t.Helper()

	fs := flag.NewFlagSet("add "+f.Name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	generate := f.Build(fs)
	if err := fs.Parse(args); err != nil {
		return featureOutput{}, err
	}
	return generate(featureCtx{ModulePath: "example.com/proj"})
}
