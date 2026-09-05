package generator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateHandlerPluralOverride(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := generateHandler("example.com/x", "Person", []string{"--plural=People", "--methods=list"}); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(filepath.Join("handlers", "person.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "func ListPeople(") {
		t.Errorf("expected ListPeople handler with --plural=People, got:\n%s", src)
	}
	routes, err := os.ReadFile(registryFileName)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(routes), `"/people"`) {
		t.Errorf("expected /people route with --plural=People, got:\n%s", routes)
	}
}
