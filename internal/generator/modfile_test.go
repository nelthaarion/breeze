package generator

import (
	"os"
	"path/filepath"
	"testing"
)

func modulePathFrom(t *testing.T, gomod string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	return currentModulePath()
}

func TestCurrentModulePath(t *testing.T) {
	cases := []struct {
		name  string
		gomod string
		want  string
	}{
		{"plain", "module example.com/foo\n\ngo 1.24.3\n", "example.com/foo"},
		{"trailing comment", "module example.com/foo // my module\n", "example.com/foo"},
		{"quoted", "module \"example.com/foo\"\n", "example.com/foo"},
		{"leading comment lines", "// a comment\nmodule example.com/foo\n", "example.com/foo"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := modulePathFrom(t, c.gomod)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestCurrentModulePathMissingDirective(t *testing.T) {
	if _, err := modulePathFrom(t, "go 1.24.3\n"); err == nil {
		t.Error("expected error for go.mod without module directive, got nil")
	}
}
