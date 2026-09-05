package mcpcmd

// confinement_test.go — the workspace flags, and the banner that reports them.
//
// These live apart from entrypoints_test.go because they need to install and remove a
// process-wide policy. mcp.ConfineToWorkspace is package-level state by design (the
// choke point is reached from package functions, not from a Server receiver), so every
// test here restores whatever was in force before it ran — otherwise a later test in
// this package would inherit a confinement it never asked for.

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nelthaarion/breeze/internal/mcp"
)

// restoreConfinement puts the previous policy back after one test.
//
// Unconfine is the state the package starts in, and it is what the tests that do not
// touch this must see.
func restoreConfinement(t *testing.T) {
	t.Helper()

	previous := mcp.WorkspaceRoots()
	t.Cleanup(func() {
		if len(previous) == 0 {
			mcp.Unconfine()
			return
		}
		if err := mcp.ConfineToWorkspace(previous...); err != nil {
			t.Errorf("restoring the previous confinement %v: %v", previous, err)
		}
	})
}

// TestWorkspaceFlagsParseIdentically extends the entrypoint-parity claim to the two new
// flags. The name is a label; everything else must be one decision made once.
func TestWorkspaceFlagsParseIdentically(t *testing.T) {
	t.Setenv(TokenEnv, "")

	argvs := [][]string{
		{"--mode", "generator", "--workspace", "/srv/projects"},
		{"--mode", "generator", "--workspace", "/srv/a,/srv/b"},
		{"--mode", "generator", "--allow-any-path"},
		{"--mode", "app-runtime", "--workspace", "/srv/projects"},
	}

	for _, argv := range argvs {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			first, err := ParseFlags(bothNames[0], argv, io.Discard)
			if err != nil {
				t.Fatalf("%s: %v", bothNames[0], err)
			}
			second, err := ParseFlags(bothNames[1], argv, io.Discard)
			if err != nil {
				t.Fatalf("%s: %v", bothNames[1], err)
			}

			if first.AllowAnyPath != second.AllowAnyPath {
				t.Errorf("AllowAnyPath differs: %v vs %v", first.AllowAnyPath, second.AllowAnyPath)
			}
			if strings.Join(first.Workspace, ",") != strings.Join(second.Workspace, ",") {
				t.Errorf("Workspace differs: %v vs %v", first.Workspace, second.Workspace)
			}
		})
	}
}

// TestWorkspaceAndAllowAnyPathAreMutuallyExclusive is a configuration-error test with a
// security consequence.
//
// "Confine to these roots, and also to nothing" is not a statement, and the two possible
// silent readings are both wrong: honouring --workspace makes --allow-any-path a flag
// that does nothing, and honouring --allow-any-path makes an operator who named a
// workspace get an unconfined server. Refusing is the only answer that cannot be
// misread.
func TestWorkspaceAndAllowAnyPathAreMutuallyExclusive(t *testing.T) {
	t.Setenv(TokenEnv, "")

	for _, name := range bothNames {
		_, err := ParseFlags(name, []string{
			"--mode", "generator", "--workspace", "/srv/projects", "--allow-any-path",
		}, io.Discard)
		if err == nil {
			t.Errorf("%s accepted --workspace together with --allow-any-path; one of them would "+
				"have to be silently ignored, and either choice is wrong", name)
		}
	}
}

// TestConfinementDefaultsToTheWorkingDirectory is the security-relevant default.
//
// An MCP client launches this as a subprocess from the project being worked on, so the
// working directory is already the answer in almost every case — and it is the answer the
// tools already documented, since every path argument defaults to it. Making it the
// boundary as well costs a correct caller nothing. The previous behaviour was unconfined,
// which is what made `{"path": "/"}` a working argument to a tool that runs `go test`.
func TestConfinementDefaultsToTheWorkingDirectory(t *testing.T) {
	restoreConfinement(t)

	wd := t.TempDir()
	t.Chdir(wd)

	opts, err := ParseFlags("breeze-mcp", []string{"--mode", "generator"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyConfinement(opts); err != nil {
		t.Fatalf("applying the default confinement: %v", err)
	}

	roots := mcp.WorkspaceRoots()
	if len(roots) != 1 {
		t.Fatalf("WorkspaceRoots() = %v, want exactly the working directory", roots)
	}
	// Compared after resolution on both sides: t.TempDir can return a path with
	// unresolved components, and a mismatch there would be a test artefact rather
	// than a finding.
	want, err := filepath.EvalSymlinks(wd)
	if err != nil {
		t.Fatalf("resolving the working directory: %v", err)
	}
	got, err := filepath.EvalSymlinks(roots[0])
	if err != nil {
		t.Fatalf("resolving the reported root: %v", err)
	}
	if got != want {
		t.Errorf("confined to %q, want the working directory %q", got, want)
	}
}

// TestWorkspaceFlagConfinesToItsRoots covers the explicit form, including several roots:
// an orchestrator legitimately has more than one project tree, and a policy that only
// honoured the first would pass every single-root test.
func TestWorkspaceFlagConfinesToItsRoots(t *testing.T) {
	restoreConfinement(t)

	first := t.TempDir()
	second := t.TempDir()

	opts, err := ParseFlags("breeze-mcp",
		[]string{"--mode", "generator", "--workspace", first + "," + second}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyConfinement(opts); err != nil {
		t.Fatalf("applying the confinement: %v", err)
	}

	if roots := mcp.WorkspaceRoots(); len(roots) != 2 {
		t.Fatalf("WorkspaceRoots() = %v, want both roots", roots)
	}
}

// TestWorkspaceFlagRejectsAMissingRoot is a usability property with a security edge.
//
// A root that does not exist is almost always a typo. Accepting it silently would produce
// a server that refuses every path with a message about confinement rather than about the
// missing directory — and an operator debugging that is an operator likely to reach for
// --allow-any-path.
func TestWorkspaceFlagRejectsAMissingRoot(t *testing.T) {
	restoreConfinement(t)

	missing := filepath.Join(t.TempDir(), "no-such-directory")
	opts, err := ParseFlags("breeze-mcp",
		[]string{"--mode", "generator", "--workspace", missing}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyConfinement(opts); err == nil {
		t.Errorf("applyConfinement accepted the non-existent root %q", missing)
	}
}

// TestAllowAnyPathRemovesConfinement states the escape hatch's behaviour as a test.
//
// It is what --allow-any-path is documented to do, and a change that made confinement
// unconditional should fail here rather than in a deployment that relies on it.
func TestAllowAnyPathRemovesConfinement(t *testing.T) {
	restoreConfinement(t)

	// Confined first, so the assertion is that the flag *removes* a policy rather
	// than that none was ever installed.
	if err := mcp.ConfineToWorkspace(t.TempDir()); err != nil {
		t.Fatal(err)
	}

	opts, err := ParseFlags("breeze-mcp",
		[]string{"--mode", "generator", "--allow-any-path"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyConfinement(opts); err != nil {
		t.Fatalf("applying --allow-any-path: %v", err)
	}
	if roots := mcp.WorkspaceRoots(); roots != nil {
		t.Errorf("WorkspaceRoots() = %v after --allow-any-path, want nil", roots)
	}
}

// TestBannerReportsConfinement is why the banner exists.
//
// An operator cannot see a policy they were not told about. The confined case names the
// roots so a misconfiguration is visible at startup rather than at the first refusal, and
// the unconfined case warns — because a server that can write anywhere on the host must
// not be indistinguishable from one that cannot.
func TestBannerReportsConfinement(t *testing.T) {
	t.Setenv(TokenEnv, "")
	restoreConfinement(t)

	root := t.TempDir()

	confined, err := ParseFlags("breeze-mcp",
		[]string{"--mode", "generator", "--port", "2000", "--workspace", root}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyConfinement(confined); err != nil {
		t.Fatalf("applying the confinement: %v", err)
	}

	banner := bannerFor(t, "breeze-mcp", confined)
	if !strings.Contains(banner, "confined to") {
		t.Errorf("the banner does not report the workspace:\n%s", banner)
	}
	if !strings.Contains(banner, filepath.Clean(root)) {
		t.Errorf("the banner does not name the root %q:\n%s", root, banner)
	}
	if strings.Contains(banner, "WARNING") {
		t.Errorf("a confined server warns about confinement:\n%s", banner)
	}

	unconfined, err := ParseFlags("breeze-mcp",
		[]string{"--mode", "generator", "--port", "2000", "--allow-any-path"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyConfinement(unconfined); err != nil {
		t.Fatalf("applying --allow-any-path: %v", err)
	}

	banner = bannerFor(t, "breeze-mcp", unconfined)
	if !strings.Contains(banner, "WARNING") {
		t.Errorf("an unconfined server does not warn; it can read, write and run `go test` "+
			"anywhere on the host and the banner has to say so:\n%s", banner)
	}
	if !strings.Contains(banner, "--allow-any-path") {
		t.Errorf("the warning does not name the flag that caused it, so an operator cannot "+
			"find what to remove:\n%s", banner)
	}
}

// TestServeAppliesConfinementBeforeAnyTransport is an ordering property, and it is the
// one that decides whether confinement is real on stdio.
//
// Serve is the single place both transports are reached from, so the policy is installed
// there rather than in each. On stdio the first tool call can arrive immediately after the
// process starts — before anything a per-transport setup would do — so a policy installed
// later would leave a window in which calls are unconfined.
//
// The check is made by driving Serve with a closed input: the stdio loop returns at once
// on EOF, which is long after applyConfinement has run and before anything else can.
func TestServeAppliesConfinementBeforeAnyTransport(t *testing.T) {
	restoreConfinement(t)
	mcp.Unconfine()

	root := t.TempDir()
	opts, err := ParseFlags("breeze-mcp",
		[]string{"--mode", "generator", "--workspace", root}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if err := Serve("breeze-mcp", "test", opts, strings.NewReader(""), &out, &errOut); err != nil {
		t.Fatalf("Serve on a closed stdin: %v", err)
	}

	if roots := mcp.WorkspaceRoots(); len(roots) == 0 {
		t.Fatal("Serve ran the stdio transport without installing the workspace policy; a tool " +
			"call arriving before the policy would be unconfined")
	}
}

// TestServeRefusesAnUnusableWorkspace is the other half of that ordering: a confinement
// that cannot be installed must stop the server rather than fall back to unconfined.
//
// Falling back would be the worst available behaviour — an operator who asked for a
// workspace, mistyped it, and got a server that can write anywhere.
func TestServeRefusesAnUnusableWorkspace(t *testing.T) {
	restoreConfinement(t)
	mcp.Unconfine()

	missing := filepath.Join(t.TempDir(), "no-such-directory")
	opts, err := ParseFlags("breeze-mcp",
		[]string{"--mode", "generator", "--workspace", missing}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if err := Serve("breeze-mcp", "test", opts, strings.NewReader(""), &out, &errOut); err == nil {
		t.Fatal("Serve started with a workspace that could not be installed; it must refuse " +
			"rather than serve unconfined")
	}
	if roots := mcp.WorkspaceRoots(); roots != nil {
		t.Errorf("a failed confinement left a policy in place: %v", roots)
	}
}

// TestWorkspaceRootMustBeADirectory covers the mistake of naming a file — breeze.yaml
// rather than the directory containing it — which is a plausible typo and would otherwise
// produce a workspace containing nothing.
func TestWorkspaceRootMustBeADirectory(t *testing.T) {
	restoreConfinement(t)

	file := filepath.Join(t.TempDir(), "breeze.yaml")
	if err := os.WriteFile(file, []byte("app:\n  name: x\n"), 0o644); err != nil {
		t.Fatalf("writing the fixture file: %v", err)
	}

	opts, err := ParseFlags("breeze-mcp",
		[]string{"--mode", "generator", "--workspace", file}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyConfinement(opts); err == nil {
		t.Errorf("applyConfinement accepted the file %q as a workspace root", file)
	}
}
