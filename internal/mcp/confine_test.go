package mcp

// confine_test.go — the adversarial tests for the workspace boundary.
//
// These are written from the attacker's side rather than the feature's. Each one is a
// path an agent could actually put in a tool argument, and the assertion is that it is
// refused — not that resolvePath returns something reasonable.
//
// # Why every case restores the previous policy
//
// confinement is package-level state and this package's tests do not call t.Parallel
// anywhere, so a test may install a policy provided it removes it again. withWorkspace
// does both, and it is the only way these tests set one: a test that installed a policy
// and returned would silently confine every test that ran after it, and the ones that
// use t.TempDir would then fail for a reason that has nothing to do with what they
// assert.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// withWorkspace confines the process to roots for the duration of one test.
func withWorkspace(t *testing.T, roots ...string) {
	t.Helper()

	previous := confinement.Load()
	t.Cleanup(func() { confinement.Store(previous) })

	if err := ConfineToWorkspace(roots...); err != nil {
		t.Fatalf("confining to %v: %v", roots, err)
	}
}

// requireOutside asserts that a path was refused as outside the workspace.
//
// The error type is checked, not the message: a caller — the tools, and anything
// wrapping them — has to be able to distinguish "outside the workspace" from "that
// directory does not exist", and only the type carries that.
func requireOutside(t *testing.T, path string) {
	t.Helper()

	got, err := resolvePath(path)
	if err == nil {
		t.Fatalf(
			"resolvePath(%q) = %q, want a refusal: this path is outside the workspace",
			path,
			got,
		)
	}
	var outside *errOutsideWorkspace
	if !errors.As(err, &outside) {
		t.Fatalf("resolvePath(%q) failed with %v (%T), want *errOutsideWorkspace", path, err, err)
	}
}

// TestConfinementRefusesTraversalOutOfTheWorkspace is the plain case: a relative path
// that climbs out.
//
// It matters because the pre-confinement code called filepath.Abs and stopped, and Abs
// resolves "../../.." happily — that is its job. Cleaning a path is not checking one.
func TestConfinementRefusesTraversalOutOfTheWorkspace(t *testing.T) {
	root := t.TempDir()
	withWorkspace(t, root)

	for _, attempt := range []string{
		filepath.Join(root, ".."),
		filepath.Join(root, "..", ".."),
		filepath.Join(root, "project", "..", "..", "elsewhere"),
		filepath.Join(root, "..", filepath.Base(root)+"-sibling"),
	} {
		requireOutside(t, attempt)
	}
}

// TestConfinementRefusesAbsolutePathsOutsideTheWorkspace covers the case that needed no
// cleverness at all.
//
// `{"path": "/etc"}` was honoured before confinement existed, and breeze_verify_project
// runs `go test` in the directory it is given — so this exact argument was arbitrary
// code execution by design, with no traversal and no injection involved.
func TestConfinementRefusesAbsolutePathsOutsideTheWorkspace(t *testing.T) {
	root := t.TempDir()
	withWorkspace(t, root)

	attempts := []string{"/etc", "/", "/tmp", "/usr/bin"}
	if runtime.GOOS == "windows" {
		attempts = []string{`C:\Windows`, `C:\`, `C:\Windows\System32`, `\\server\share`}
	}
	for _, attempt := range attempts {
		requireOutside(t, attempt)
	}
}

// TestConfinementAllowsPathsInsideTheWorkspace is the other half, and it is not
// filler: a boundary that refuses everything is indistinguishable from a broken
// server, and the tools' own tests would not catch it because they run unconfined.
func TestConfinementAllowsPathsInsideTheWorkspace(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "services", "orders")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("creating the fixture tree: %v", err)
	}
	withWorkspace(t, root)

	for _, attempt := range []string{root, nested, filepath.Join(root, "services")} {
		got, err := resolvePath(attempt)
		if err != nil {
			t.Fatalf("resolvePath(%q) was refused: %v", attempt, err)
		}
		if got != filepath.Clean(attempt) {
			t.Errorf("resolvePath(%q) = %q, want %q", attempt, got, filepath.Clean(attempt))
		}
	}
}

// TestConfinementAllowsANonExistentPathUnderAValidRoot is the case a naive
// implementation gets wrong in the other direction.
//
// `breeze new` names a directory precisely because it does not exist yet, and
// EvalSymlinks fails on a path that is not there. An implementation that refused
// unresolvable paths would refuse every scaffold — so resolveExisting resolves the
// nearest existing ancestor instead, and this asserts that the result is still
// permitted.
func TestConfinementAllowsANonExistentPathUnderAValidRoot(t *testing.T) {
	root := t.TempDir()
	withWorkspace(t, root)

	target := filepath.Join(root, "not-created-yet", "nor-this", "project")
	got, err := resolvePath(target)
	if err != nil {
		t.Fatalf("resolvePath(%q) was refused: %v — a path that does not exist yet is what "+
			"breeze_new is given", target, err)
	}
	if got != filepath.Clean(target) {
		t.Errorf("resolvePath(%q) = %q, want the path unchanged", target, got)
	}
}

// TestConfinementRefusesANonExistentPathOutsideEveryRoot is the same mechanism used
// adversarially.
//
// Resolving the nearest existing ancestor must not become a way to be permitted by
// default: a path under a directory that does not exist and never will is still
// outside, and the walk up to the volume root has to end in a refusal rather than in
// an unchecked return.
func TestConfinementRefusesANonExistentPathOutsideEveryRoot(t *testing.T) {
	root := t.TempDir()
	withWorkspace(t, root)

	outside := filepath.Join(
		filepath.Dir(root),
		"no-such-directory-"+filepath.Base(root),
		"project",
	)
	requireOutside(t, outside)
}

// linkKind is what linkDir managed to create.
type linkKind int

const (
	linkNone linkKind = iota
	// linkSymlink is a real symlink, which filepath.EvalSymlinks follows.
	linkSymlink
	// linkJunction is a Windows directory junction, which it does not: EvalSymlinks
	// returns the junction's own path and reports success. Confinement therefore
	// cannot establish where it points and refuses it by name.
	linkJunction
)

// linkDir creates a directory link at link pointing to target, and says which kind.
//
// os.Symlink first, which is the real thing everywhere. On Windows an unprivileged
// account cannot create a symlink unless Developer Mode is on, so a directory junction
// is the fallback — a different reparse point with a different resolution story, which
// is why the kind is returned rather than hidden. Without the fallback the escape tests
// skip on the platform where a path-confinement bug is most likely to be written.
func linkDir(t *testing.T, target, link string) (linkKind, error) {
	t.Helper()

	if err := os.Symlink(target, link); err == nil {
		return linkSymlink, nil
	} else if runtime.GOOS != "windows" {
		return linkNone, err
	}
	// Argument array, not a shell string: the paths come from t.TempDir, but a
	// helper that interpolated them into a command line would be the wrong pattern
	// to leave in a security test.
	out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		return linkNone, fmt.Errorf("mklink /J: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return linkJunction, nil
}

// TestConfinementRefusesALinkEscape is the case a prefix comparison cannot catch, and
// the reason both sides are resolved before being compared.
//
// A link inside the workspace pointing at a directory outside it satisfies any textual
// containment test — the path really does begin with the root — while every read and
// write through it lands outside. It is the standard way out of a directory jail, so it
// is tested rather than reasoned about.
//
// Both reparse kinds are refused, for different reasons, and the assertion says which
// applies: a symlink is resolved and found to be outside, while a junction cannot be
// resolved at all and is refused because containment cannot be established. Asserting
// only "some error" would let the junction case pass for the wrong reason — including
// if it started failing with "no such directory".
func TestConfinementRefusesALinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	link := filepath.Join(root, "escape")
	kind, err := linkDir(t, outside, link)
	if err != nil {
		// No symlink and no junction: nothing to test with.
		t.Skipf("cannot create a directory link on this platform/account: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(outside, "project"), 0o755); err != nil {
		t.Fatalf("creating the fixture tree: %v", err)
	}
	withWorkspace(t, root)

	for _, attempt := range []string{link, filepath.Join(link, "project")} {
		got, err := resolvePath(attempt)
		if err == nil {
			t.Fatalf("resolvePath(%q) = %q, want a refusal: it is a %v into %q, which is outside "+
				"the workspace", attempt, got, kind, outside)
		}
		switch kind {
		case linkSymlink:
			var outsideErr *errOutsideWorkspace
			if !errors.As(err, &outsideErr) {
				t.Errorf("resolvePath(%q) through a symlink failed with %v (%T), want "+
					"*errOutsideWorkspace — the link resolves, so the answer is that the target "+
					"is outside", attempt, err, err)
			}
		case linkJunction:
			var unresolved *errUnresolvedLink
			if !errors.As(err, &unresolved) {
				t.Errorf("resolvePath(%q) through a junction failed with %v (%T), want "+
					"*errUnresolvedLink — EvalSymlinks reports success on a junction while "+
					"returning its own path, so containment cannot be established and the "+
					"refusal has to say that rather than something else", attempt, err, err)
			}
		}
	}
}

// TestConfinementResolvesALinkedRoot covers the mirror image, which is not
// hypothetical: /tmp is a symlink to /private/tmp on macOS, so t.TempDir returns an
// unresolved path there and every test in this file would fail if only the candidate
// were resolved.
func TestConfinementResolvesALinkedRoot(t *testing.T) {
	real := t.TempDir()
	parent := t.TempDir()

	link := filepath.Join(parent, "workspace-link")
	kind, err := linkDir(t, real, link)
	if err != nil {
		t.Skipf("cannot create a directory link on this platform/account: %v", err)
	}

	previous := confinement.Load()
	t.Cleanup(func() { confinement.Store(previous) })

	confineErr := ConfineToWorkspace(link)

	if kind == linkJunction {
		// A junction as the *root* is refused at configuration time rather than
		// silently accepted. Accepting it would store a root that does not say where
		// the filesystem goes, and every path under it would then be refused —
		// which looks like the tools being broken rather than the root being wrong.
		if confineErr == nil {
			t.Fatal("ConfineToWorkspace accepted a junction as a workspace root; its target " +
				"cannot be established, so every path under it would be refused and the " +
				"failure would look like a tool bug")
		}
		return
	}

	if confineErr != nil {
		t.Fatalf("confining to the symlinked root %q: %v", link, confineErr)
	}
	// The workspace is declared through the link; the path is given as the real
	// directory. Both spellings name the same place, so both must be accepted.
	for _, attempt := range []string{link, real, filepath.Join(real, "project")} {
		if _, err := resolvePath(attempt); err != nil {
			t.Errorf("resolvePath(%q) was refused with the workspace declared as %q: %v",
				attempt, link, err)
		}
	}
}

// TestConfinementAcceptsSeveralRoots covers the multi-root policy, because an
// orchestrator legitimately has more than one project tree and a check that only ever
// consulted roots[0] would pass every single-root test in this file.
func TestConfinementAcceptsSeveralRoots(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	third := t.TempDir()

	withWorkspace(t, first, second)

	for _, attempt := range []string{first, second, filepath.Join(second, "project")} {
		if _, err := resolvePath(attempt); err != nil {
			t.Errorf(
				"resolvePath(%q) was refused with roots %q and %q: %v",
				attempt,
				first,
				second,
				err,
			)
		}
	}
	requireOutside(t, third)
}

// pathTakingTools names every tool that accepts a filesystem path, the argument that
// carries it, and any other required arguments.
//
// Required arguments matter: a tool refused for a missing argument would look confined
// without the path ever being examined.
//
// This is a hardcoded list because there is nothing to derive it from — a path argument
// is a JSON property called "path", "dir", "project_path" or "existing_path", and a tool
// taking a fifth spelling would be missed by any pattern. A hardcoded list has the
// opposite failure mode, going stale, which is what TestNoPathTakingToolIsMissedFromTheList
// exists to catch.
var pathTakingTools = []struct {
	tool string
	// arg is the property that carries the path, substituted per test.
	arg string
	// extra are the tool's other required arguments.
	extra map[string]any
}{
	{tool: "breeze_check_idioms", arg: "path"},
	{tool: "breeze_verify_project", arg: "path"},
	{tool: "breeze_run_benchmarks", arg: "path"},
	{tool: "breeze_get_test_coverage", arg: "path"},
	{tool: "breeze_explain_project", arg: "path"},
	{tool: "breeze_suggest_next_steps", arg: "path"},
	{tool: "breeze_generate_llms_txt", arg: "path"},
	{tool: "breeze_check_llms_txt_freshness", arg: "path"},
	{tool: "breeze_search_llms_txt", arg: "path", extra: map[string]any{"query": "router"}},
	{tool: "breeze_routes", arg: "dir"},
	{tool: "breeze_begin_change_set", arg: "project_path"},
	{tool: "breeze_get_change_history", arg: "project_path"},
	{tool: "breeze_diff_config", arg: "existing_path"},
	{tool: "breeze_new", arg: "dir", extra: map[string]any{"name": "app"}},
	{tool: "breeze_add", arg: "dir", extra: map[string]any{"feature": "jwt"}},
	{tool: "breeze_generate", arg: "dir", extra: map[string]any{
		"kind": "model", "name": "User", "args": []string{"email:string"},
	}},
}

// TestEveryPathTakingToolIsConfined is the coverage test for the workspace boundary.
//
// The per-function tests above assert that resolvePath refuses what it should. This one
// asserts that the tools actually call it — a different claim, and the one that was false
// for `breeze_diff_config` and for the read path behind `breeze_explain_project` even
// after every other tool was confined.
func TestEveryPathTakingToolIsConfined(t *testing.T) {
	withWorkspace(t, t.TempDir())
	outside := t.TempDir()

	srv := NewServer("test")
	for _, tc := range pathTakingTools {
		t.Run(tc.tool, func(t *testing.T) {
			args := map[string]any{tc.arg: outside}
			for k, v := range tc.extra {
				args[k] = v
			}
			result := callTool(t, srv, tc.tool, args)

			// The whole result is searched, not only the error text. diff_config
			// reported the refusal inside a *successful* result's notes — a tool
			// that says "0 files would be touched" for a directory it was refused
			// access to has answered a question it could not answer, and an agent
			// reading `valid: true` would act on it.
			whole := result.Content[0].Text
			if result.StructuredContent != nil {
				encoded, err := json.Marshal(result.StructuredContent)
				if err != nil {
					t.Fatalf("re-encoding the structured result: %v", err)
				}
				whole += "\n" + string(encoded)
			}

			if !result.IsError {
				t.Fatalf("%s succeeded for a path outside the workspace:\n%s", tc.tool, whole)
			}
			if !strings.Contains(whole, "outside this server's workspace") {
				t.Errorf("%s failed for a path outside the workspace, but not because of the "+
					"workspace — so the refusal is incidental and a differently-shaped call "+
					"could still get through:\n%s", tc.tool, whole)
			}
		})
	}
}

// TestNoPathTakingToolIsMissedFromTheList keeps the list above from going stale.
//
// It reads every registered tool's own JSON Schema and flags any property whose name
// looks like a path but whose tool is not covered above. That is the failure mode a
// hardcoded list has, and it is the one that matters: a new tool with a `dir` argument
// would be unconfined and every existing test would still pass.
//
// The property names are matched rather than the types, because JSON Schema has no
// "path" type — every one of these is `{"type": "string"}`, and the name is the only
// signal available. A tool taking a path under a name not listed here is a tool this
// test cannot see, which is why the message says so rather than reporting success.
func TestNoPathTakingToolIsMissedFromTheList(t *testing.T) {
	pathArgNames := map[string]bool{
		"path": true, "dir": true, "project_path": true, "existing_path": true,
		"root": true, "directory": true, "file": true, "filename": true,
	}

	covered := map[string]bool{}
	for _, tc := range pathTakingTools {
		covered[tc.tool] = true
	}

	srv := NewServer("test")
	for name, tl := range srv.tools {
		var parsed struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(tl.schema, &parsed); err != nil {
			t.Fatalf("%s: decoding its schema: %v", name, err)
		}

		for prop := range parsed.Properties {
			if !pathArgNames[prop] || covered[name] {
				continue
			}
			// simulate's "path" is a request path, not a filesystem path. It is
			// named explicitly rather than pattern-excluded, so a genuinely new
			// filesystem argument cannot hide behind the same exemption.
			if name == "breeze_simulate_request" && prop == "path" {
				continue
			}
			t.Errorf("%s takes a %q argument and is not in pathTakingTools, so nothing asserts "+
				"that it confines the path. Either add it to that list or, if the argument is "+
				"not a filesystem path, exempt it here by name", name, prop)
		}
	}
}

// resolvedByCaller are the functions whose path argument was resolved upstream.
//
// Each is here because it is an implementation detail of something that did resolve,
// not a tool entry point. This list *is* the audit: adding a name to it is the
// deliberate act of claiming a function's caller has already checked, and the reason
// string is where that claim is made in words a later reader can disagree with.
var resolvedByCaller = map[string]string{
	// confine.go — these are the containment check itself.
	"ConfineToWorkspace":  "establishes the roots; there is nothing above it to resolve against",
	"resolveExisting":     "resolves the path resolvePath is deciding about",
	"unresolvedLinkBelow": "walks the components of an already-resolved path",

	// workspace.go — the sandbox internals. newSandbox resolves; these move files
	// between a resolved project and a temporary directory this package created.
	"snapshotTree":    "walks a sandbox or an already-resolved project root",
	"copyTree":        "copies between a resolved source and a temporary directory",
	"copyFile":        "one file of a copyTree",
	"fileDigest":      "hashes a file snapshotTree already walked to",
	"applyChanges":    "writes into the project path the change set resolved at begin",
	"newEmptySandbox": "creates a temporary directory and takes no caller path",
	"remove":          "deletes a temporary directory this package created",
	"withEnv":         "sets environment variables, not paths",

	// capture.go — the chdir helper. Its two callers resolve first, and
	// TestChdirHelperCallersResolveFirst below is what keeps that true: this is the
	// function the three planning tools reached unconfined.
	"runInDir": "chdir helper; runGenerator and inProject resolve before calling it",

	// The provisioning build context: a temporary directory, and this module's own
	// source tree located by construction rather than by argument.
	"generateProject":               "scaffolds into os.MkdirTemp output",
	"writeProvisionBuildFiles":      "writes into that temporary project directory",
	"vendorBreezeSource":            "copies this module's own source into a build context",
	"rewriteGoModForVendoredBreeze": "edits the go.mod of that temporary project",
	"writeTempConfig":               "writes a temporary file whose name it chooses",
	"breezeSourceRoot":              "derives this module's root from runtime.Caller",
	"ascendToModule":                "walks up from the path breezeSourceRoot gave it",
	"isBreezeModuleRoot":            "probes for go.mod while ascending",

	// The provisioning registry: the orchestrator's own state file, beside its
	// executable. No caller names it.
	"registryPath":          "derives the path from os.Executable",
	"loadProvisionRegistry": "reads the orchestrator's own registry",
	"save":                  "writes the orchestrator's own registry",
	"persistLocked":         "writes the orchestrator's own registry",
	"copyFileTo":            "one file of a vendorBreezeSource copy",

	// Reads and writes inside a root the tool above already resolved. Each of these
	// takes the *result* of orWorkingDir or verifyRoot, never a caller's string —
	// which is checked, for the three planning tools' shared reader, by
	// TestEveryPathTakingToolIsConfined driving the tools themselves.
	"appendHistory":      "writes .breeze/ inside the project path the change set resolved",
	"readHistory":        "reads .breeze/ inside the path breeze_get_change_history resolved",
	"runIdiomCheck":      "walks the root verifyRoot resolved",
	"readSourceModels":   "reads models/ inside a root gatherProjectFacts resolved",
	"generateLLMS":       "writes inside the root orWorkingDir resolved",
	"checkLLMSFreshness": "reads inside the root orWorkingDir resolved",
	"searchLLMS":         "reads inside the root orWorkingDir resolved",
}

// TestNoToolReachesTheFilesystemOutsideResolvePath is the audit for Part 6's second
// requirement, stated as a property of the source rather than of one call.
//
// TestEveryPathTakingToolIsConfined proves the 16 tools that take a path refuse one
// outside the workspace. It cannot prove the absence of a *seventeenth* route to the
// filesystem — a tool that reads a fixed path, or derives one from something other than
// its path argument, would pass that test by not being in its list.
//
// So this reads the package's own filesystem calls and requires each to be reachable
// only through a path resolvePath produced. The mechanism is deliberately crude: it
// flags an os.ReadFile / os.WriteFile / os.MkdirAll / os.Chdir-family call in a function
// that neither calls resolvePath itself nor is on the list of functions that operate on
// paths their caller already resolved.
//
// A crude check is the right trade here. The alternative — dataflow analysis of where
// each path came from — is a program this repository would then have to maintain, and
// its failure mode is a false pass. The failure mode of this one is a false *failure*,
// which is loud and is fixed either by resolving the path or by adding the function to
// the list below with a reason.
func TestNoToolReachesTheFilesystemOutsideResolvePath(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}

		for _, fn := range functionsIn(t, name, string(source)) {
			if strings.Contains(fn.body, "resolvePath(") {
				continue
			}
			if _, allowed := resolvedByCaller[fn.name]; allowed {
				continue
			}
			for _, call := range filesystemCalls {
				if !strings.Contains(fn.body, call) {
					continue
				}
				t.Errorf("%s: %s calls %s without resolvePath. Either resolve the path there, "+
					"or — if its caller already did — add %q to resolvedByCaller with the reason. "+
					"A filesystem call reachable from a tool without passing resolvePath is "+
					"outside the workspace boundary",
					name, fn.name, strings.TrimSuffix(call, "("), fn.name)
				break
			}
		}
	}
}

// filesystemCalls are the operations that touch a path.
//
// os.Stat and os.Lstat are absent on purpose: confine.go itself has to stat paths to
// decide containment, and a probe of existence is not a read of contents.
var filesystemCalls = []string{
	"os.ReadFile(", "os.WriteFile(", "os.Open(", "os.OpenFile(", "os.Create(",
	"os.MkdirAll(", "os.Mkdir(", "os.Remove(", "os.RemoveAll(", "os.ReadDir(",
	"os.Rename(", "os.Chdir(", "os.Symlink(", "os.Chmod(",
	"filepath.WalkDir(", "filepath.Walk(", "filepath.Glob(",
}

// sourceFunc is one function's name and body text.
type sourceFunc struct {
	name string
	body string
}

// functionsIn splits a file into its top-level functions.
//
// Text rather than AST, because what the check above needs is "does this function
// mention resolvePath", and a body's text answers that exactly. A method's receiver is
// dropped: the name is what the allow-list is keyed on, and two methods called remove
// on different types would be indistinguishable to a reader of that list anyway.
func functionsIn(t *testing.T, file, source string) []sourceFunc {
	t.Helper()

	var out []sourceFunc
	lines := strings.Split(source, "\n")

	for i, line := range lines {
		if !strings.HasPrefix(line, "func ") {
			continue
		}
		name := functionName(line)
		if name == "" {
			continue
		}

		// The body runs to the next top-level "}" — the one at column zero, which
		// gofmt guarantees for a function's closing brace.
		var body strings.Builder
		for _, rest := range lines[i+1:] {
			if rest == "}" {
				break
			}
			body.WriteString(rest)
			body.WriteString("\n")
		}
		out = append(out, sourceFunc{name: name, body: body.String()})
	}

	if len(out) == 0 {
		t.Fatalf("%s: no functions were found, so the audit read nothing", file)
	}
	return out
}

// functionName extracts the identifier from a func declaration line.
func functionName(line string) string {
	rest := strings.TrimPrefix(line, "func ")

	// A method: skip the receiver.
	if strings.HasPrefix(rest, "(") {
		closing := strings.Index(rest, ")")
		if closing < 0 {
			return ""
		}
		rest = strings.TrimSpace(rest[closing+1:])
	}

	end := strings.IndexAny(rest, "([")
	if end <= 0 {
		return ""
	}
	return rest[:end]
}

// TestUnconfinedResolvesAnyPath states the escape hatch's behaviour as a test rather
// than as a comment.
//
// Two reasons. It is the mode the rest of this package's tests run in — they call
// t.TempDir and never confine — so if the default were confined they would all fail
// for an unrelated reason. And --allow-any-path is a documented flag, so what it does
// belongs in the suite: if confinement ever became unconditional, this failing is the
// correct outcome rather than a surprise in production.
func TestUnconfinedResolvesAnyPath(t *testing.T) {
	previous := confinement.Load()
	t.Cleanup(func() { confinement.Store(previous) })
	Unconfine()

	attempt := "/etc"
	if runtime.GOOS == "windows" {
		attempt = `C:\Windows`
	}
	if _, err := resolvePath(attempt); err != nil {
		t.Fatalf("unconfined resolvePath(%q) was refused: %v", attempt, err)
	}
}

// TestPackageTestsRunUnconfinedByDefault is the assumption the rest of the suite
// rests on, asserted instead of assumed.
//
// Every other test in internal/mcp uses t.TempDir, which is outside any workspace a
// server would be confined to. If the package's default were ever changed to confined,
// those tests would start failing with workspace errors that look like tool bugs. This
// one fails first, and says why.
func TestPackageTestsRunUnconfinedByDefault(t *testing.T) {
	if policy := confinement.Load(); policy != nil {
		t.Fatalf("internal/mcp tests are running confined to %v; the suite's fixtures live in "+
			"t.TempDir, which is outside any workspace, so a test that installed a policy did "+
			"not restore it", policy.declared)
	}
	if roots := WorkspaceRoots(); roots != nil {
		t.Fatalf("WorkspaceRoots() = %v, want nil when unconfined", roots)
	}
}

// TestConfineToWorkspaceRefusesNonsense covers the configuration side.
//
// The empty call is the important one: "confine to nothing" and "do not confine" are
// opposite intentions, and if ConfineToWorkspace() with no arguments quietly meant the
// second, an operator's misconfiguration would produce an unconfined server that
// reported itself as confined.
func TestConfineToWorkspaceRefusesNonsense(t *testing.T) {
	previous := confinement.Load()
	t.Cleanup(func() { confinement.Store(previous) })

	if err := ConfineToWorkspace(); err == nil {
		t.Error("ConfineToWorkspace() with no roots was accepted; it must be an error, " +
			"because it reads as \"confine to nothing\" and would be indistinguishable from Unconfine")
	}
	if err := ConfineToWorkspace("  ", "\t"); err == nil {
		t.Error("ConfineToWorkspace with only blank roots was accepted")
	}

	missing := filepath.Join(t.TempDir(), "no-such-root")
	if err := ConfineToWorkspace(missing); err == nil {
		t.Errorf("ConfineToWorkspace(%q) accepted a root that does not exist; a typo would "+
			"otherwise produce a server that refuses every path with a message about "+
			"confinement rather than about the missing directory", missing)
	}

	file := filepath.Join(t.TempDir(), "breeze.yaml")
	if err := os.WriteFile(file, []byte("app:\n  name: x\n"), 0o644); err != nil {
		t.Fatalf("writing the fixture file: %v", err)
	}
	if err := ConfineToWorkspace(file); err == nil {
		t.Errorf("ConfineToWorkspace(%q) accepted a file as a root", file)
	}
}

// TestOutsideWorkspaceErrorNamesTheRoots checks the message, because a refusal an
// operator cannot act on is a support ticket.
//
// The declared root is named rather than the symlink-resolved one: a message quoting a
// path the operator never typed is one they have to work backwards from.
func TestOutsideWorkspaceErrorNamesTheRoots(t *testing.T) {
	root := t.TempDir()
	withWorkspace(t, root)

	_, err := resolvePath(filepath.Join(root, "..", "elsewhere"))
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), root) {
		t.Errorf("the refusal does not name the permitted root %q: %v", root, err)
	}
}
