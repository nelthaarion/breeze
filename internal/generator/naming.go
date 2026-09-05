package generator

// naming.go — where a generated file goes and what package it declares.
//
// Both answers used to be inlined at each generator's call to
// writeGeneratedGoFile: `filepath.Join("models", fileSlug(name)+".go")` and a
// literal `package models` in the template. That was fine while neither was
// overridable and wrong the moment they were, because the two have to agree —
// a file written into a directory whose other files declare a different package
// does not compile, and the error names the package rather than the flag that
// caused it.
//
// So the derivation lives here, once, and the overrides go through it rather
// than around it. A default-derived name and a user-supplied one are the same
// value by the time anything validates them, which is the point: a bug in the
// derivation is caught by the same check that catches a bad --package.
//
// The flag names are not written here either. They are read off the yaml tags
// of the configuration fields that carry the same two settings, so
// `--filename` and `--package` cannot drift from `models.<name>.filename` and
// `models.<name>.package` — renaming the tag renames the flag.

import (
	"flag"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

// outputFilenameFlag and outputPackageFlag are the CLI spellings, derived from
// ModelConfig's tags rather than declared.
var (
	outputFilenameFlag = configFieldTag(reflect.TypeOf(ModelConfig{}), "Filename")
	outputPackageFlag  = configFieldTag(reflect.TypeOf(ModelConfig{}), "Package")
)

// configFieldTag is the yaml tag of one field of a configuration struct.
//
// A missing field is a programmer error rather than a user one — it means this
// file and config.go disagree about what exists — so it panics at
// initialisation, where any test run catches it, instead of degrading into a
// flag named "".
func configFieldTag(t reflect.Type, field string) string {
	f, ok := t.FieldByName(field)
	if !ok {
		panic(
			fmt.Sprintf(
				"generator: %s has no field %s; the output flags are derived from it",
				t,
				field,
			),
		)
	}
	return yamlTagName(f)
}

// outputFlags are the two overrides every code-writing generator accepts.
type outputFlags struct {
	filename *string
	pkg      *string
}

// registerOutputFlags adds --filename and --package to a generator's FlagSet.
//
// Registering them in one place is what makes them uniform: a generator opts in
// with a single call rather than by spelling out two flags and their help text,
// so the wording, the defaults and the validation cannot differ between kinds.
func registerOutputFlags(fs *flag.FlagSet) *outputFlags {
	return &outputFlags{
		filename: fs.String(
			outputFilenameFlag,
			"",
			"output file name, e.g. --"+outputFilenameFlag+"=user_model.go (default derived from the name)",
		),
		pkg: fs.String(outputPackageFlag, "",
			"package clause of the generated file (default the containing directory's name)"),
	}
}

// set reports whether either override was given, which is all the generators
// that cannot honour them need to know.
func (o *outputFlags) set() bool {
	return o != nil && (strings.TrimSpace(*o.filename) != "" || strings.TrimSpace(*o.pkg) != "")
}

// outputTarget is a resolved destination: an exact path and an exact package.
type outputTarget struct {
	Path    string
	Package string
}

// target resolves the destination for a generated file.
//
// dir is the directory the kind writes into and base the file's stem without
// ".go" — the values the generator would have used before overrides existed, so
// omitting both flags reproduces the previous behaviour exactly.
func (o *outputFlags) target(dir, base string) (outputTarget, error) {
	name := base + ".go"
	pkg := ""
	if o != nil {
		if given := strings.TrimSpace(*o.filename); given != "" {
			resolved, err := resolveOutputFilename(given)
			if err != nil {
				return outputTarget{}, err
			}
			name = resolved
		}
		pkg = strings.TrimSpace(*o.pkg)
	}

	path := filepath.Join(dir, name)
	if pkg == "" {
		pkg = derivePackageName(filepath.Dir(path))
	}
	// Validated here as well as at the point of writing. Reporting it now names
	// the flag that caused it, which the writer cannot do — by then a supplied
	// name and a derived one look identical.
	if err := validateGoPackageName(pkg); err != nil {
		return outputTarget{}, fmt.Errorf("--%s: %w", outputPackageFlag, err)
	}
	// A legal identifier that disagrees with the directory is the other half of
	// the same problem, and it applies to derived names too: a kind pointed at an
	// existing directory by --filename would otherwise produce a file that only
	// fails at `go build`.
	if err := checkPackageAgreesWithDirectory(filepath.Dir(path), pkg, path); err != nil {
		return outputTarget{}, err
	}
	return outputTarget{Path: path, Package: pkg}, nil
}

// resolveOutputFilename validates a --filename value and adds the extension if
// it was left off.
//
// A path is refused rather than accepted, because the directory a kind writes
// into is part of what the kind means: a model in handlers/ is not a model the
// rest of the generated project can find. The error says which flag moves the
// file instead.
func resolveOutputFilename(name string) (string, error) {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return "", fmt.Errorf("--%s cannot be empty", outputFilenameFlag)
	case strings.ContainsAny(name, `/\`):
		return "", fmt.Errorf("--%s %q must be a file name, not a path — "+
			"the directory is fixed by the kind being generated", outputFilenameFlag, name)
	case name == "." || name == "..":
		return "", fmt.Errorf("--%s %q is not a file name", outputFilenameFlag, name)
	case strings.HasSuffix(name, ".go"):
		if strings.TrimSuffix(name, ".go") == "" {
			return "", fmt.Errorf(
				"--%s %q has no name before the extension",
				outputFilenameFlag,
				name,
			)
		}
		return name, nil
	case filepath.Ext(name) != "":
		return "", fmt.Errorf("--%s %q must end in .go — this generator writes Go source",
			outputFilenameFlag, name)
	}
	return name + ".go", nil
}

// derivePackageName is the existing convention stated once: the package a
// generated file declares is named after the directory it lands in.
//
// Hyphens and dots are the one accommodation. They are legal in a directory
// name and not in an identifier, so "user-models" derives "user_models" rather
// than failing on a name nobody typed. Anything else that does not survive is
// reported by validateGoPackageName, which is deliberate: silently rewriting a
// directory name into something unrecognisable would be worse than refusing.
func derivePackageName(dir string) string {
	base := filepath.Base(filepath.Clean(dir))
	if base == "." || base == string(filepath.Separator) {
		return "main"
	}
	return strings.NewReplacer("-", "_", ".", "_", " ", "_").Replace(base)
}

// validateGoPackageName checks a package clause.
//
// This is the check the compiler would apply, moved earlier. Without it a bad
// value reaches gofmt, which reports a syntax error at a line and column in
// generated source the user never wrote — and a reserved word gets there too,
// since `package range` parses as far as the keyword.
func validateGoPackageName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("package name is empty")
	case name == "_":
		return fmt.Errorf(
			"package name %q is the blank identifier, which cannot name a package",
			name,
		)
	case token.Lookup(name).IsKeyword():
		return fmt.Errorf("package name %q is a Go keyword", name)
	case !token.IsIdentifier(name):
		return fmt.Errorf("package name %q is not a legal Go identifier — "+
			"letters, digits and underscores only, and not starting with a digit", name)
	}
	return nil
}

// checkPackageAgreesWithDirectory refuses a package that would disagree with the
// files already in its directory.
//
// Go allows exactly one package per directory (plus its _test variant), so a
// file declaring `package models` next to files declaring `package handlers`
// does not compile — and the compiler reports it as "found packages handlers and
// models", which names neither the flag nor the derived value that produced it.
//
// Only a genuine disagreement is refused. A directory that does not exist, is
// empty of Go files, or already declares this package is fine, and an external
// test package (foo_test beside foo) is fine because the language permits it.
// Files that do not parse are ignored rather than reported: this is a collision
// check, and a project mid-edit should not be blocked from generating by a
// syntax error somewhere else in the directory.
func checkPackageAgreesWithDirectory(dir, pkg, forPath string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		// Not there yet, which is the common case: the generator is about to
		// create it.
		return nil
	}

	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if path == forPath {
			// The file being written. Its current package is about to be
			// replaced, so it cannot be the thing that conflicts.
			continue
		}
		f, parseErr := parser.ParseFile(
			fset,
			path,
			nil,
			parser.PackageClauseOnly|parser.SkipObjectResolution,
		)
		if parseErr != nil {
			continue
		}
		found := f.Name.Name
		if found == pkg || found == pkg+"_test" || pkg == found+"_test" {
			continue
		}
		return fmt.Errorf("%s would declare `package %s`, but %s in the same directory declares "+
			"`package %s` — Go allows one package per directory, so this would not compile\n"+
			"       (pass --%s=%s to match, or --%s to write into a different file)",
			forPath, pkg, e.Name(), found, outputPackageFlag, found, outputFilenameFlag)
	}
	return nil
}
