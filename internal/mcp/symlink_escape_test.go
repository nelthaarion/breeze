package mcp

// symlink_escape_test.go — a project is not trusted, and a link inside it must not turn a
// confined write into one that lands elsewhere.
//
// confine.go resolves the *root* a tool is pointed at. These tests are about what is
// inside that root: a cloned repository can ship `internal -> /somewhere/else`, and every
// write below it — a commit, a generator, a history append, llms.txt — would follow it.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// symlinkOrSkip makes a link, or skips where the platform will not allow one.
func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symbolic links here: %v", err)
	}
}

func writeFileT(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFileT(t *testing.T, path string) (string, bool) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return string(b), true
}

func TestEnsureWriteStaysInside(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, outside, filepath.Join(root, "out"))
	symlinkOrSkip(t, filepath.Join(root, "real"), filepath.Join(root, "alias"))
	writeFileT(t, filepath.Join(outside, "victim"), "precious")
	symlinkOrSkip(t, filepath.Join(outside, "victim"), filepath.Join(root, "file-link"))

	for _, tc := range []struct {
		name   string
		target string
		ok     bool
	}{
		{"plain new file", "a.go", true},
		{"new file in a new directory", "pkg/new/a.go", true},
		{"existing directory", "real/a.go", true},
		{"link that stays inside the project", "alias/a.go", true},
		{"directory link leading outside", "out/a.go", false},
		{"deep path through an outside link", "out/deep/er/a.go", false},
		{"file link at the target", "file-link", false},
		{"lexical escape", "../elsewhere.go", false},
	} {
		err := ensureWriteStaysInside(root, filepath.Join(root, tc.target))
		if tc.ok && err != nil {
			t.Errorf("%s: refused a safe write: %v", tc.name, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: accepted a write that leaves the project", tc.name)
		}
	}
}

func TestCommitDoesNotWriteThroughALinkedDirectory(t *testing.T) {
	project := t.TempDir()
	outside := t.TempDir()
	sandboxDir := t.TempDir()
	symlinkOrSkip(t, outside, filepath.Join(project, "internal"))
	writeFileT(t, filepath.Join(sandboxDir, "internal", "evil.go"), "package evil\n")

	err := applyChanges(sandboxDir, project, []fileChange{{Path: "internal/evil.go", Change: changeModified}})
	if err == nil {
		t.Fatal("a commit wrote through a directory link that leads out of the project")
	}
	if _, found := readFileT(t, filepath.Join(outside, "evil.go")); found {
		t.Fatal("the file was written outside the project")
	}
	if !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

func TestCommitStillFollowsALinkThatStaysInsideTheProject(t *testing.T) {
	// The check must not break projects that use links legitimately.
	project := t.TempDir()
	sandboxDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, filepath.Join(project, "real"), filepath.Join(project, "alias"))
	writeFileT(t, filepath.Join(sandboxDir, "alias", "ok.go"), "package ok\n")

	if err := applyChanges(sandboxDir, project, []fileChange{{Path: "alias/ok.go", Change: changeModified}}); err != nil {
		t.Fatalf("a link that stays inside the project was refused: %v", err)
	}
	if got, found := readFileT(t, filepath.Join(project, "real", "ok.go")); !found || got != "package ok\n" {
		t.Errorf("the file did not land where the link points: %q found=%v", got, found)
	}
}

func TestHistoryAppendDoesNotFollowALinkedDotBreeze(t *testing.T) {
	project := t.TempDir()
	outside := t.TempDir()
	symlinkOrSkip(t, outside, filepath.Join(project, ".breeze"))

	if err := appendHistory(project, historyEntry{}); err == nil {
		t.Fatal("history was appended through a .breeze link that leads out of the project")
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 0 {
		t.Errorf("files appeared outside the project: %v", entries)
	}
}

func TestLLMSWriteDoesNotOverwriteWhatALinkPointsAt(t *testing.T) {
	project := newFixtureProject(t, "scratch", "docs:\n  enabled: true\n")
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim.txt")
	writeFileT(t, victim, "precious")
	symlinkOrSkip(t, victim, filepath.Join(project, llmsShortName))

	res := generateLLMS(generateLLMSArgs{Path: project, Write: true})
	if res.IsError {
		t.Fatalf("generate_llms_txt failed outright: %s", res.Content[0].Text)
	}
	if got, _ := readFileT(t, victim); got != "precious" {
		t.Fatalf("the file a link pointed at was overwritten: %q", got)
	}
	view := res.StructuredContent.(llmsResult)
	for _, f := range view.Files {
		if f.Name == llmsShortName && f.Written {
			t.Error("the linked file was reported as written")
		}
	}
	if !strings.Contains(strings.Join(view.Notes, "\n"), "symbolic link") {
		t.Errorf("no note explains why %s was not written: %v", llmsShortName, view.Notes)
	}
	// The other file is an ordinary one and is still written.
	if _, found := readFileT(t, filepath.Join(project, llmsFullName)); !found {
		t.Errorf("%s was not written; one refused file should not stop the other", llmsFullName)
	}
}

func TestGeneratorRefusesAProjectWithALinkLeadingOutOfTheWorkspace(t *testing.T) {
	resetConfinement(t)
	workspace := t.TempDir()
	outside := t.TempDir()
	project := filepath.Join(workspace, "app")
	writeFileT(t, filepath.Join(project, "go.mod"), "module example.com/app\n")
	symlinkOrSkip(t, outside, filepath.Join(project, "docs"))
	if err := ConfineToWorkspace(workspace); err != nil {
		t.Fatal(err)
	}

	ran := false
	res := runGenerator(project, func() error { ran = true; return nil })
	if !res.IsError || !strings.Contains(res.Content[0].Text, "symbolic link") {
		t.Fatalf("a project with an escaping link was handed to a generator: %+v", res)
	}
	if ran {
		t.Error("the generator ran before the preflight refused")
	}
}

func TestGeneratorAcceptsLinksThatStayInsideTheWorkspace(t *testing.T) {
	resetConfinement(t)
	workspace := t.TempDir()
	project := filepath.Join(workspace, "app")
	writeFileT(t, filepath.Join(project, "go.mod"), "module example.com/app\n")
	writeFileT(t, filepath.Join(workspace, "shared", "x.go"), "package shared\n")
	symlinkOrSkip(t, filepath.Join(project, "inner"), filepath.Join(project, "inner-alias"))
	symlinkOrSkip(t, filepath.Join(workspace, "shared"), filepath.Join(project, "shared")) // another directory of the same workspace
	if err := ConfineToWorkspace(workspace); err != nil {
		t.Fatal(err)
	}

	ran := false
	res := runGenerator(project, func() error { ran = true; return nil })
	if res.IsError || !ran {
		t.Fatalf("a link inside the workspace was refused: %+v", res)
	}
}

func TestNewProjectRefusesALinkAtItsTargetName(t *testing.T) {
	resetConfinement(t)
	workspace := t.TempDir()
	outside := t.TempDir()
	symlinkOrSkip(t, outside, filepath.Join(workspace, "app"))
	if err := ConfineToWorkspace(workspace); err != nil {
		t.Fatal(err)
	}

	ran := false
	res := runGenerator(workspace, func() error { ran = true; return nil }, "app")
	if !res.IsError || ran {
		t.Fatalf("breeze_new wrote into a project directory that is a link leading outside (ran=%v)", ran)
	}
}

func TestNoPreflightWalkWithoutAWorkspace(t *testing.T) {
	// With nothing to enforce there is nothing to walk: the unconfined, sandboxed
	// deployment keeps exactly the behaviour it had.
	resetConfinement(t)
	Unconfine()
	project := t.TempDir()
	outside := t.TempDir()
	writeFileT(t, filepath.Join(project, "go.mod"), "module example.com/app\n")
	symlinkOrSkip(t, outside, filepath.Join(project, "docs"))

	ran := false
	res := runGenerator(project, func() error { ran = true; return nil })
	if res.IsError || !ran {
		t.Fatalf("an unconfined server refused a project because of a link: %+v", res)
	}
}

func TestSandboxRefusesAnOversizedTree(t *testing.T) {
	oldFiles, oldBytes := maxSandboxFiles, maxSandboxBytes
	t.Cleanup(func() { maxSandboxFiles, maxSandboxBytes = oldFiles, oldBytes })

	src := t.TempDir()
	for _, name := range []string{"a", "b", "c", "d", "e", "f"} {
		writeFileT(t, filepath.Join(src, name+".txt"), "0123456789")
	}

	maxSandboxFiles = 5
	if err := copyTree(src, filepath.Join(t.TempDir(), "x")); err == nil || !strings.Contains(err.Error(), "files") {
		t.Errorf("6 files were copied under a 5-file limit: %v", err)
	}

	maxSandboxFiles, maxSandboxBytes = 100, 25
	if err := copyTree(src, filepath.Join(t.TempDir(), "y")); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("60 bytes were copied under a 25-byte limit: %v", err)
	}

	maxSandboxFiles, maxSandboxBytes = 100, 1<<20
	if err := copyTree(src, filepath.Join(t.TempDir(), "z")); err != nil {
		t.Errorf("a small tree was refused: %v", err)
	}
}
