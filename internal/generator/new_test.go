package generator

import (
	"embed"
	"errors"
	"os"
	"testing"
)

func TestNewCleansUpOnFailure(t *testing.T) {
	t.Chdir(t.TempDir())
	orig := renderTree
	renderTree = func(embed.FS, string, string, newProjectData) error {
		return errors.New("render failed")
	}
	defer func() { renderTree = orig }()

	if err := runNew([]string{"myapp"}); err == nil {
		t.Fatal("expected error, got nil")
	}
	if _, err := os.Stat("myapp"); !os.IsNotExist(err) {
		t.Error("myapp directory was not cleaned up after a failed scaffold")
	}
}
