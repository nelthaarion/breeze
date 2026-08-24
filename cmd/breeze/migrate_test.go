package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMigratorPresentTracksTheRunner pins the predicate that decides whether the
// project has a migration runner.
//
// It has to look at the file, because `add migrator` writes a standalone main
// package and no block in features_generated.go. The check it replaced asked
// hasBlock about "migrator", which is false in every project that will ever
// exist — so `generate model` recommended `breeze add migrator` to people who
// had already run it.
func TestMigratorPresentTracksTheRunner(t *testing.T) {
	t.Chdir(t.TempDir())

	if migratorPresent() {
		t.Error("migratorPresent() is true in an empty directory")
	}

	if err := os.MkdirAll(migratorPkg, 0o755); err != nil {
		t.Fatal(err)
	}
	// The directory alone is not a runner — `add migrator` creates migrations/
	// and cmd/migrate/, and a half-finished run must not read as success.
	if migratorPresent() {
		t.Error("migratorPresent() is true with only the directory present")
	}

	if err := os.WriteFile(filepath.Join(migratorPkg, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !migratorPresent() {
		t.Error("migratorPresent() is false with cmd/migrate/main.go in place")
	}
}

// TestStandaloneFeaturesHaveNoBlock records why migratorPresent exists.
//
// A standalone feature writes files and nothing else, so no marker block appears
// for it and hasBlock can never find one. Anything that gates on "has this
// feature been added" must check the filesystem instead. If a feature stops being
// standalone this fails, which is the moment to revisit those gates.
func TestStandaloneFeaturesHaveNoBlock(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := runNew([]string{"sa", "--module=example.com/sa"}); err != nil {
		t.Fatalf("breeze new: %v", err)
	}
	t.Chdir("sa")

	for name, f := range features {
		if !f.Standalone {
			continue
		}
		if err := runAdd([]string{name}); err != nil {
			t.Fatalf("breeze add %s: %v", name, err)
		}
		if hasBlock(featuresFileName, featureMarkerPrefix, name) {
			t.Errorf("standalone feature %q wrote a block to %s — it has no dispatcher call, "+
				"so the block would never run", name, featuresFileName)
		}
	}
}

func TestMakeMigration(t *testing.T) {
	t.Chdir(t.TempDir())

	// Create migrations directory first
	if err := os.Mkdir("migrations", 0o755); err != nil {
		t.Fatalf("failed to create migrations dir: %v", err)
	}

	err := runMakeMigration([]string{"CreateUsersTable"})
	if err != nil {
		t.Fatalf("runMakeMigration() error = %v", err)
	}

	// Check that up and down files were created
	upFile := filepath.Join("migrations", "0001_create_users_table.up.sql")
	downFile := filepath.Join("migrations", "0001_create_users_table.down.sql")

	if _, err := os.Stat(upFile); os.IsNotExist(err) {
		t.Errorf("up file not created: %s", upFile)
	}
	if _, err := os.Stat(downFile); os.IsNotExist(err) {
		t.Errorf("down file not created: %s", downFile)
	}
}

func TestMakeMigrationCreatesDir(t *testing.T) {
	t.Chdir(t.TempDir())

	// Don't create migrations directory; runMakeMigration should create it
	err := runMakeMigration([]string{"AddEmailColumn"})
	if err != nil {
		t.Fatalf("runMakeMigration() error = %v", err)
	}

	if _, err := os.Stat("migrations"); os.IsNotExist(err) {
		t.Error("migrations directory was not created")
	}
}

func TestMakeMigrationMultiple(t *testing.T) {
	t.Chdir(t.TempDir())

	if err := os.Mkdir("migrations", 0o755); err != nil {
		t.Fatalf("failed to create migrations dir: %v", err)
	}

	// Create first migration
	if err := runMakeMigration([]string{"CreateUsers"}); err != nil {
		t.Fatalf("first migration failed: %v", err)
	}

	// Create second migration
	if err := runMakeMigration([]string{"AddEmail"}); err != nil {
		t.Fatalf("second migration failed: %v", err)
	}

	// Check version numbers
	upFile1 := filepath.Join("migrations", "0001_create_users.up.sql")
	upFile2 := filepath.Join("migrations", "0002_add_email.up.sql")

	if _, err := os.Stat(upFile1); os.IsNotExist(err) {
		t.Errorf("first migration not created")
	}
	if _, err := os.Stat(upFile2); os.IsNotExist(err) {
		t.Errorf("second migration not created")
	}
}

func TestMakeMigrationEmptyName(t *testing.T) {
	err := runMakeMigration([]string{""})
	if err == nil {
		t.Fatal("expected error for empty name")
	}
	if !strings.Contains(err.Error(), "cannot be empty") {
		t.Errorf("error = %v, want error containing 'cannot be empty'", err)
	}
}

func TestMakeMigrationNoArgs(t *testing.T) {
	err := runMakeMigration([]string{})
	if err == nil {
		t.Fatal("expected error for no arguments")
	}
}

func TestToSlug(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"CreateUsersTable", "create_users_table"},
		{"AddEmail", "add_email"},
		{"user", "user"},
		{"User", "user"},
		// Runs of capitals stay one word: these name generated files, table
		// names and db columns, where "request_i_d" is wrong in every one.
		{"RequestID", "request_id"},
		{"UserID", "user_id"},
		{"ID", "id"},
		{"HTTPServer", "http_server"},
		{"OAuth2Token", "o_auth2_token"},
		{"Step2Verify", "step2_verify"},
		// Already-snake_case input is left alone, which is what makes it safe to
		// run over a field name typed as "signed_up_at".
		{"signed_up_at", "signed_up_at"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := toSlug(tt.input)
			if got != tt.want {
				t.Errorf("toSlug(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestMakeMigrationFileContent(t *testing.T) {
	t.Chdir(t.TempDir())

	if err := os.Mkdir("migrations", 0o755); err != nil {
		t.Fatalf("failed to create migrations dir: %v", err)
	}

	if err := runMakeMigration([]string{"TestMigration"}); err != nil {
		t.Fatalf("runMakeMigration() error = %v", err)
	}

	upFile := filepath.Join("migrations", "0001_test_migration.up.sql")
	content, err := os.ReadFile(upFile)
	if err != nil {
		t.Fatalf("failed to read up file: %v", err)
	}

	// Check that file contains comment header
	contentStr := string(content)
	if !strings.Contains(contentStr, "Migration 0001") {
		t.Errorf("up file missing migration header comment")
	}
	if !strings.Contains(contentStr, "test_migration") {
		t.Errorf("up file missing name in header comment")
	}
}
