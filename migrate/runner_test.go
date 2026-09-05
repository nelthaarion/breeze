package migrate

import (
	"fmt"
	"strings"
	"testing"
)

func TestSplitStatements(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want int
	}{
		{
			name: "single statement",
			sql:  "CREATE TABLE users (id INT);",
			want: 1,
		},
		{
			name: "multiple statements",
			sql:  "CREATE TABLE users (id INT); CREATE TABLE posts (id INT);",
			want: 2,
		},
		{
			name: "no trailing semicolon",
			sql:  "CREATE TABLE users (id INT)",
			want: 1,
		},
		{
			name: "empty statements ignored",
			sql:  "CREATE TABLE users (id INT);;",
			want: 1,
		},
		{
			name: "whitespace handling",
			sql:  "\n  CREATE TABLE users (id INT);\n  ",
			want: 1,
		},
		{
			name: "semicolon in string literal",
			sql:  "INSERT INTO users (name) VALUES ('John;Doe'); SELECT 1;",
			want: 2,
		},
		{
			name: "double-quoted strings",
			sql:  `INSERT INTO users (name) VALUES ("John;Doe"); SELECT 1;`,
			want: 2,
		},
		{
			name: "empty input",
			sql:  "",
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitStatements(tt.sql)
			if len(got) != tt.want {
				t.Errorf("splitStatements() got %d statements, want %d", len(got), tt.want)
				if len(got) > 0 {
					t.Logf("statements: %v", got)
				}
			}
		})
	}
}

func TestSplitStatementsContent(t *testing.T) {
	sql := "CREATE TABLE users (id INT); DROP TABLE users;"
	got := splitStatements(sql)

	if len(got) != 2 {
		t.Fatalf("splitStatements() got %d statements, want 2", len(got))
	}

	if got[0] != "CREATE TABLE users (id INT)" {
		t.Errorf("first statement = %q", got[0])
	}
	if got[1] != "DROP TABLE users" {
		t.Errorf("second statement = %q", got[1])
	}
}

func TestComputeChecksum(t *testing.T) {
	content := "CREATE TABLE users (id INT)"
	checksum1 := computeChecksum(content)
	checksum2 := computeChecksum(content)

	if checksum1 != checksum2 {
		t.Errorf("checksum not deterministic: %q vs %q", checksum1, checksum2)
	}

	// Different content should produce different checksum
	checksum3 := computeChecksum("CREATE TABLE posts (id INT)")
	if checksum1 == checksum3 {
		t.Errorf("different content produced same checksum")
	}

	// Checksum should be valid hex
	if len(checksum1) != 64 {
		t.Errorf("checksum length = %d, want 64 (SHA-256 hex)", len(checksum1))
	}
	if !isHex(checksum1) {
		t.Errorf("checksum is not valid hex: %q", checksum1)
	}
}

func isHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// TestDescendingByVersion pins the order Down rolls back in.
//
// This is the whole of `migrate down N`'s contract: N means "the last N", so the
// first record out of here must be the highest version. The implementation this
// replaced sorted ascending under a comment claiming descending, which made
// `down 1` roll back the project's *oldest* migration — and gave no sign of it,
// because rolling back 0001 succeeds exactly as quietly as rolling back 0003.
//
// The input is a map, so the pre-sort order is randomised by the runtime. Any
// case that passes only for one iteration order will flake rather than pass.
func TestDescendingByVersion(t *testing.T) {
	tests := []struct {
		name  string
		input []int
		want  []int
	}{
		{"empty", nil, nil},
		{"single", []int{1}, []int{1}},
		{"already descending", []int{3, 2, 1}, []int{3, 2, 1}},
		{"ascending", []int{1, 2, 3}, []int{3, 2, 1}},
		{"unordered", []int{2, 5, 1, 4, 3}, []int{5, 4, 3, 2, 1}},
		// Versions need not be contiguous: a branch merge can leave gaps.
		{"gaps", []int{1, 7, 3}, []int{7, 3, 1}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			applied := make(map[int]appliedRecord, len(tt.input))
			for _, v := range tt.input {
				applied[v] = appliedRecord{Version: v, Name: fmt.Sprintf("m%d", v)}
			}

			got := descendingByVersion(applied)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d records, want %d", len(got), len(tt.want))
			}
			for i, rec := range got {
				if rec.Version != tt.want[i] {
					versions := make([]int, len(got))
					for j, r := range got {
						versions[j] = r.Version
					}
					t.Fatalf("descendingByVersion() = %v, want %v", versions, tt.want)
				}
			}
		})
	}
}

// TestDescendingByVersionIsStableAcrossRuns is the guard against the map. Go
// randomises iteration order per run, so a sort that happened to be a no-op on
// one ordering would pass intermittently.
func TestDescendingByVersionIsStableAcrossRuns(t *testing.T) {
	applied := map[int]appliedRecord{}
	for v := 1; v <= 12; v++ {
		applied[v] = appliedRecord{Version: v}
	}

	for i := 0; i < 100; i++ {
		got := descendingByVersion(applied)
		for j, rec := range got {
			if want := 12 - j; rec.Version != want {
				t.Fatalf("run %d: position %d is version %d, want %d", i, j, rec.Version, want)
			}
		}
	}
}

func BenchmarkSplitStatements(b *testing.B) {
	sql := strings.Repeat("INSERT INTO users (id) VALUES (1);", 100)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		splitStatements(sql)
	}
}
