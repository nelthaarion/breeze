package migrate

import (
	"io/fs"
	"strings"
	"testing"
)

// mockFS is a simple in-memory filesystem for testing.
type mockFS struct {
	files map[string]string
}

func (m *mockFS) Open(name string) (fs.File, error) {
	panic("not implemented")
}

func (m *mockFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name != "." {
		return nil, nil
	}
	var entries []fs.DirEntry
	for filename := range m.files {
		entries = append(entries, &mockDirEntry{name: filename})
	}
	return entries, nil
}

func (m *mockFS) ReadFile(name string) ([]byte, error) {
	content, ok := m.files[name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return []byte(content), nil
}

type mockDirEntry struct {
	name string
}

func (m *mockDirEntry) Name() string      { return m.name }
func (m *mockDirEntry) IsDir() bool       { return false }
func (m *mockDirEntry) Type() fs.FileMode { return 0 }
func (m *mockDirEntry) Info() (fs.FileInfo, error) {
	panic("not implemented")
}

func TestDiscoverMigrations(t *testing.T) {
	tests := []struct {
		name    string
		files   map[string]string
		want    int
		wantErr bool
		errMsg  string
	}{
		{
			name:  "empty directory",
			files: map[string]string{},
			want:  0,
		},
		{
			name: "single migration pair",
			files: map[string]string{
				"0001_create_users.up.sql":   "CREATE TABLE users (id INT)",
				"0001_create_users.down.sql": "DROP TABLE users",
			},
			want: 1,
		},
		{
			name: "multiple migrations sorted by version",
			files: map[string]string{
				"0002_add_email.up.sql":      "ALTER TABLE users ADD email",
				"0002_add_email.down.sql":    "ALTER TABLE users DROP email",
				"0001_create_users.up.sql":   "CREATE TABLE users (id INT)",
				"0001_create_users.down.sql": "DROP TABLE users",
			},
			want: 2,
		},
		{
			name: "missing down file",
			files: map[string]string{
				"0001_create_users.up.sql": "CREATE TABLE users (id INT)",
			},
			wantErr: true,
			errMsg:  "no .down.sql",
		},
		{
			name: "missing up file",
			files: map[string]string{
				"0001_create_users.down.sql": "DROP TABLE users",
			},
			wantErr: true,
			errMsg:  "no .up.sql",
		},
		{
			name: "duplicate versions",
			files: map[string]string{
				"0001_create_users.up.sql":   "CREATE TABLE users (id INT)",
				"0001_create_users.down.sql": "DROP TABLE users",
				"0001_create_posts.up.sql":   "CREATE TABLE posts (id INT)",
				"0001_create_posts.down.sql": "DROP TABLE posts",
			},
			wantErr: true,
			errMsg:  "duplicate version",
		},
		{
			name: "non-migration files ignored",
			files: map[string]string{
				"0001_create_users.up.sql":   "CREATE TABLE users (id INT)",
				"0001_create_users.down.sql": "DROP TABLE users",
				"README.md":                  "# Migrations",
				"schema.sql":                 "SELECT 1",
			},
			want: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mfs := &mockFS{files: tt.files}
			got, err := DiscoverMigrations(mfs)

			if (err != nil) != tt.wantErr {
				t.Errorf("DiscoverMigrations() error = %v, wantErr %v", err, tt.wantErr)
			}

			if tt.wantErr {
				if !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf(
						"DiscoverMigrations() error = %v, want error containing %q",
						err,
						tt.errMsg,
					)
				}
				return
			}

			if len(got) != tt.want {
				t.Errorf("DiscoverMigrations() got %d migrations, want %d", len(got), tt.want)
			}

			// Verify sorted order
			for i := 1; i < len(got); i++ {
				if got[i].Version <= got[i-1].Version {
					t.Errorf(
						"migrations not sorted: version %d after %d",
						got[i].Version,
						got[i-1].Version,
					)
				}
			}
		})
	}
}

func TestDiscoverMigrationsContent(t *testing.T) {
	mfs := &mockFS{
		files: map[string]string{
			"0001_create_users.up.sql":   "CREATE TABLE users (id INT PRIMARY KEY)",
			"0001_create_users.down.sql": "DROP TABLE users",
		},
	}

	migrations, err := DiscoverMigrations(mfs)
	if err != nil {
		t.Fatalf("DiscoverMigrations() error = %v", err)
	}

	if len(migrations) != 1 {
		t.Fatalf("expected 1 migration, got %d", len(migrations))
	}

	m := migrations[0]
	if m.Version != 1 {
		t.Errorf("Version = %d, want 1", m.Version)
	}
	if m.Name != "create_users" {
		t.Errorf("Name = %q, want %q", m.Name, "create_users")
	}
	if m.UpSQL != "CREATE TABLE users (id INT PRIMARY KEY)" {
		t.Errorf("UpSQL = %q, want %q", m.UpSQL, "CREATE TABLE users (id INT PRIMARY KEY)")
	}
	if m.DownSQL != "DROP TABLE users" {
		t.Errorf("DownSQL = %q, want %q", m.DownSQL, "DROP TABLE users")
	}
}
