package main

import (
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"text/template"
)

//go:embed templates/api
var apiTemplateFS embed.FS

//go:embed templates/views
var viewsTemplateFS embed.FS

// fallbackGoDirective is used when the toolchain version cannot be parsed out
// of runtime.Version(). It matches the go directive in breeze's own go.mod —
// a scaffold that imports breeze cannot ask for less.
const fallbackGoDirective = "1.25.13"

// breezeModulePath is the framework's import path, needed both as a go.mod
// require and as the anchor ensureImports keys off.
const breezeModulePath = "github.com/nelthaarion/breeze"

type newProjectData struct {
	Name   string
	Module string
}

func runNew(args []string) error {
	flags := flag.NewFlagSet("new", flag.ContinueOnError)
	tmplName := flags.String("template", "api", "project template: api or views")
	module := flags.String("module", "", "Go module path (defaults to the project name)")

	flagArgs, positional := splitFlagsAndPositional(flags, args)
	if err := parseFlags(flags, flagArgs); err != nil {
		return err
	}

	if len(positional) != 1 {
		return fmt.Errorf("usage: breeze new <name> [--template=api|views] [--module=<import-path>]")
	}
	name := positional[0]

	if err := validateProjectName(name); err != nil {
		return err
	}

	var templateFS embed.FS
	var templateRoot string
	switch *tmplName {
	case "api":
		templateFS, templateRoot = apiTemplateFS, "templates/api"
	case "views":
		templateFS, templateRoot = viewsTemplateFS, "templates/views"
	default:
		return fmt.Errorf("unknown template %q — must be one of: api, views", *tmplName)
	}

	if _, err := os.Stat(name); err == nil {
		return fmt.Errorf("%s already exists", name)
	}

	modulePath := *module
	if modulePath == "" {
		modulePath = name
	}
	if err := validateModulePath(modulePath); err != nil {
		return err
	}

	data := newProjectData{Name: name, Module: modulePath}

	if err := os.MkdirAll(name, 0o755); err != nil {
		return err
	}

	// The existence check above guarantees name was created by this run, so
	// a failed scaffold can safely remove it rather than leave a partial
	// project behind.
	if err := populateProject(templateFS, templateRoot, name, modulePath, data); err != nil {
		os.RemoveAll(name)
		return err
	}

	if err := runGoModTidy(name); err != nil {
		fmt.Fprintf(os.Stderr, "warning: go mod tidy failed: %v\n", err)
	}

	fmt.Printf("Created %s (template: %s)\n\nNext steps:\n  cd %s\n  go run .\n\nAdd framework features with:\n  breeze add dashboard\n  breeze add --list\n", name, *tmplName, name)
	return nil
}

// illegalNameChars are rejected in a project name: the Windows-reserved set
// plus the path separators, which would make `breeze new` write outside the
// directory it just created.
var illegalNameChars = regexp.MustCompile(`[<>:"|?*\x00-\x1f/\\]`)

// validateProjectName checks that name can be both a directory and the last
// element of a module path. Catching this up front beats a confusing
// MkdirAll or go.mod error after the tree is half written.
func validateProjectName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("project name cannot be empty")
	case name == "." || name == "..":
		return fmt.Errorf("invalid project name %q", name)
	case strings.HasPrefix(name, "-"):
		return fmt.Errorf("invalid project name %q — a leading dash is parsed as a flag", name)
	case illegalNameChars.MatchString(name):
		return fmt.Errorf("invalid project name %q — avoid path separators and the characters < > : \" | ? *", name)
	case strings.HasSuffix(name, ".") || strings.HasSuffix(name, " "):
		return fmt.Errorf("invalid project name %q — a trailing dot or space is not a usable directory name on Windows", name)
	}
	return nil
}

// validateModulePath applies the parts of the module-path rules that catch
// real mistakes: no whitespace, no backslashes, no scheme, no leading or
// trailing slash. It is deliberately not a full implementation of
// golang.org/x/mod/module.CheckPath — that would mean taking on the
// dependency to reject inputs `go mod tidy` will reject a moment later
// anyway.
func validateModulePath(path string) error {
	switch {
	case path == "":
		return fmt.Errorf("module path cannot be empty")
	case strings.ContainsAny(path, " \t\\"):
		return fmt.Errorf("invalid module path %q — no spaces or backslashes", path)
	case strings.Contains(path, "://"):
		return fmt.Errorf("invalid module path %q — drop the scheme (use example.com/user/app)", path)
	case strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/"):
		return fmt.Errorf("invalid module path %q — no leading or trailing slash", path)
	}
	return nil
}

// renderTree is swappable in tests to exercise runNew's failure cleanup.
var renderTree = renderTemplateTree

func populateProject(templateFS embed.FS, templateRoot, name, modulePath string, data newProjectData) error {
	if err := renderTree(templateFS, templateRoot, name, data); err != nil {
		return err
	}

	if err := os.WriteFile(filepath.Join(name, "go.mod"), []byte(goModContent(modulePath)), 0o644); err != nil {
		return err
	}

	if err := os.WriteFile(filepath.Join(name, registryFileName), []byte(registryTemplate()), 0o644); err != nil {
		return err
	}

	// The features file is written up front, with an empty call list, so the
	// RegisterGeneratedFeatures call in main.go always resolves. `breeze add`
	// then only has to fill in blocks rather than also patch main.go.
	if err := os.WriteFile(filepath.Join(name, featuresFileName), []byte(featuresTemplate()), 0o644); err != nil {
		return err
	}

	return os.MkdirAll(filepath.Join(name, "handlers"), 0o755)
}

// goModContent builds the scaffold's go.mod.
//
// Both values used to be wrong. The go directive was hardcoded to 1.24.3
// while breeze itself requires newer, and there was no require for breeze at
// all — so the scaffold only compiled if `go mod tidy` could reach the
// network, and silently produced a project pinned to whatever version the
// proxy happened to serve rather than the one this CLI was built against.
func goModContent(modulePath string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "module %s\n\ngo %s\n", modulePath, goDirective())

	// When the CLI is built from a module cache download, build info knows
	// which breeze version this binary corresponds to, and pinning the
	// scaffold to it guarantees the generated code matches the APIs this CLI
	// generates against. A dev build reports "(devel)" and has no usable
	// version, so we leave the require out and let `go mod tidy` resolve it.
	if v := breezeVersion(); v != "" {
		fmt.Fprintf(&b, "\nrequire %s %s\n", breezeModulePath, v)
	}
	return b.String()
}

// goDirectiveRe pulls "1.25.13" out of a runtime version string like
// "go1.25.13". Release candidates ("go1.26rc1") and devel builds
// ("devel +abc123") intentionally fail to match and fall back.
var goDirectiveRe = regexp.MustCompile(`^go(\d+\.\d+(?:\.\d+)?)$`)

func goDirective() string {
	if m := goDirectiveRe.FindStringSubmatch(runtime.Version()); m != nil {
		return m[1]
	}
	return fallbackGoDirective
}

// breezeVersion reports the breeze module version this CLI was built from,
// or "" when that is not a resolvable version (a local `go build`, or a
// binary built outside module mode).
func breezeVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	// When the CLI is built from within the breeze module itself, breeze is
	// info.Main rather than a dependency.
	if info.Main.Path == breezeModulePath && isResolvedVersion(info.Main.Version) {
		return info.Main.Version
	}
	for _, dep := range info.Deps {
		if dep.Path == breezeModulePath && isResolvedVersion(dep.Version) {
			return dep.Version
		}
	}
	return ""
}

func isResolvedVersion(v string) bool {
	return v != "" && v != "(devel)" && strings.HasPrefix(v, "v")
}

// renderTemplateTree walks every file under root in srcFS, rendering it
// through text/template with data and writing the result under destDir,
// preserving relative paths. Files named "*.tmpl" have that suffix stripped;
// a file named "gitignore.tmpl" becomes ".gitignore".
func renderTemplateTree(srcFS embed.FS, root, destDir string, data newProjectData) error {
	return fs.WalkDir(srcFS, root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		destRel := rel
		isTemplate := filepath.Ext(rel) == ".tmpl"
		if isTemplate {
			destRel = rel[:len(rel)-len(".tmpl")]
			if filepath.Base(destRel) == "gitignore" {
				destRel = filepath.Join(filepath.Dir(destRel), ".gitignore")
			}
		}
		destPath := filepath.Join(destDir, destRel)

		if d.IsDir() {
			return os.MkdirAll(destPath, 0o755)
		}

		content, err := srcFS.ReadFile(path)
		if err != nil {
			return err
		}

		if !isTemplate {
			return os.WriteFile(destPath, content, 0o644)
		}

		tmpl, err := template.New(rel).Parse(string(content))
		if err != nil {
			return fmt.Errorf("parsing template %s: %w", path, err)
		}

		f, err := os.Create(destPath)
		if err != nil {
			return err
		}
		defer f.Close()

		return tmpl.Execute(f, data)
	})
}

func runGoModTidy(dir string) error {
	cmd := exec.Command("go", "mod", "tidy")
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
