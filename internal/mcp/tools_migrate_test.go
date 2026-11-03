package mcp

// Tests for breeze_makemigration and breeze_migrate.
//
// The two properties that justify migrate being its own implementation rather than a
// call into the CLI's are tested by doing the thing: a runner that fails must come
// back as a result and leave the process alive (generator.Migrate would have called
// os.Exit inside this test binary), and a connection string in the environment must
// not reach the caller however the runner chooses to print it.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nelthaarion/breeze/v2/internal/generator"
)

// ── breeze_makemigration ─────────────────────────────────────────────────────

func TestMakeMigrationWritesTheNumberedPair(t *testing.T) {
	root := t.TempDir()
	withWorkspace(t, root)
	srv := NewServer("test")

	first := callTool(t, srv, "breeze_makemigration", map[string]any{"name": "AddUsers", "dir": root})
	if first.IsError {
		t.Fatalf("makemigration failed: %s", first.Content[0].Text)
	}
	for _, want := range []string{"0001_add_users.up.sql", "0001_add_users.down.sql"} {
		if _, err := os.Stat(filepath.Join(root, "migrations", want)); err != nil {
			t.Errorf("%s was not written: %v", want, err)
		}
	}

	// A second call numbers past the first. This is the property an agent relies on
	// when it adds several migrations in a row without counting them.
	second := callTool(t, srv, "breeze_makemigration", map[string]any{"name": "AddIndexToUsers", "dir": root})
	if second.IsError {
		t.Fatalf("second makemigration failed: %s", second.Content[0].Text)
	}
	if _, err := os.Stat(filepath.Join(root, "migrations", "0002_add_index_to_users.up.sql")); err != nil {
		t.Errorf("the second migration was not numbered 0002: %v", err)
	}
}

func TestMakeMigrationHonoursMigrationsDirAndRefusesToEscapeTheProject(t *testing.T) {
	root := t.TempDir()
	withWorkspace(t, root)
	srv := NewServer("test")

	ok := callTool(t, srv, "breeze_makemigration",
		map[string]any{"name": "Init", "dir": root, "migrations_dir": "db/migrations"})
	if ok.IsError {
		t.Fatalf("makemigration with migrations_dir failed: %s", ok.Content[0].Text)
	}
	if _, err := os.Stat(filepath.Join(root, "db", "migrations", "0001_init.up.sql")); err != nil {
		t.Errorf("the pair was not written under migrations_dir: %v", err)
	}

	// Inside the workspace is not the same as inside the project. The workspace root
	// here is the project's parent, so "../" stays within what the server may touch
	// and is still refused, because the flag names a place in the project.
	for _, escape := range []string{"../elsewhere", "/etc", "a/../../b"} {
		res := callTool(t, srv, "breeze_makemigration",
			map[string]any{"name": "Evil", "dir": root, "migrations_dir": escape})
		if !res.IsError {
			t.Errorf("migrations_dir %q was accepted", escape)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "elsewhere")); err == nil {
		t.Error("a refused migrations_dir still created a directory outside the project")
	}
}

func TestMakeMigrationRequiresAName(t *testing.T) {
	srv := NewServer("test")
	res := callTool(t, srv, "breeze_makemigration", map[string]any{"name": "  "})
	if !res.IsError || !strings.Contains(res.Content[0].Text, "name is required") {
		t.Errorf("a blank name was not refused: %+v", res)
	}
}

// ── breeze_migrate: arguments ────────────────────────────────────────────────

func TestMigrateArgv(t *testing.T) {
	cases := []struct {
		name    string
		args    migrateArgs
		want    []string
		wantErr string
	}{
		{name: "status", args: migrateArgs{Action: "status"}, want: []string{"status"}},
		{name: "up", args: migrateArgs{Action: "up"}, want: []string{"up"}},
		{name: "down defaults to one step in the runner", args: migrateArgs{Action: "down"}, want: []string{"down"}},
		{name: "down with steps", args: migrateArgs{Action: "down", Steps: 2}, want: []string{"down", "2"}},
		{
			name: "every flag, in the order the runner documents",
			args: migrateArgs{Action: "up", MigrationsDir: "db/migrations", Driver: "pgx", Timeout: "30s"},
			want: []string{"up", "--dir=db/migrations", "--driver=pgx", "--timeout=30s"},
		},

		// There is deliberately no default action.
		{name: "no action", args: migrateArgs{}, wantErr: "action is required"},
		{name: "unknown action", args: migrateArgs{Action: "drop"}, wantErr: "unknown action"},

		{name: "steps on up", args: migrateArgs{Action: "up", Steps: 1}, wantErr: "only to action=down"},
		{name: "negative steps", args: migrateArgs{Action: "down", Steps: -1}, wantErr: "positive integer"},

		{name: "absolute migrations_dir", args: migrateArgs{Action: "up", MigrationsDir: "/etc"}, wantErr: "absolute"},
		{name: "escaping migrations_dir", args: migrateArgs{Action: "up", MigrationsDir: "../x"}, wantErr: "outside the project"},
		{name: "driver that is not a name", args: migrateArgs{Action: "up", Driver: "pg; drop"}, wantErr: "not a database/sql driver name"},
		{name: "timeout that is not a duration", args: migrateArgs{Action: "up", Timeout: "soon"}, wantErr: "not a positive duration"},
		{name: "zero timeout", args: migrateArgs{Action: "up", Timeout: "0s"}, wantErr: "not a positive duration"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.args.argv()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q (argv %v)", err, tc.wantErr, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
				t.Errorf("argv = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestMigrateAcceptsNoConnectionString is the design point the file comment makes:
// a DSN as a tool argument is a database password in an agent's transcript.
func TestMigrateAcceptsNoConnectionString(t *testing.T) {
	srv := NewServer("test")
	tl, ok := srv.tools["breeze_migrate"]
	if !ok {
		t.Fatal("breeze_migrate is not registered")
	}

	schema := strings.ToLower(string(tl.schema))
	for _, forbidden := range []string{"dsn", "password", "secret", "connection", "\"url\""} {
		if strings.Contains(schema, forbidden) {
			t.Errorf("the schema mentions %q, so a credential can be passed as an argument: %s", forbidden, tl.schema)
		}
	}
}

// ── breeze_migrate: running a runner ─────────────────────────────────────────

// fakeRunnerSource stands in for the generated cmd/migrate/main.go. It echoes what
// it was given and what it was configured with, and fails when told to, which is
// every behaviour migrate has to get right without a database.
const fakeRunnerSource = `package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	fmt.Println("args:", strings.Join(os.Args[1:], " "))
	fmt.Println("connecting to", os.Getenv("BREEZE_DATABASE_URL"))
	if os.Getenv("FAKE_RUNNER_EXIT") == "3" {
		fmt.Fprintln(os.Stderr, "migration 0002 failed: relation already exists")
		os.Exit(3)
	}
}
`

func fakeRunnerProject(t *testing.T) string {
	t.Helper()

	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("the Go toolchain is not on PATH; migrate runs the runner with go run")
	}
	// The developer's GOFLAGS belong to their own module, not to this fixture. A
	// -modfile or -mod there would make `go run` resolve against the wrong tree.
	t.Setenv("GOFLAGS", "")

	root := t.TempDir()
	withWorkspace(t, root)
	writeGo(t, root, "go.mod", "module example.com/fakerunner\n\ngo 1.24\n")
	writeGo(t, root, "cmd/migrate/main.go", fakeRunnerSource)
	return root
}

func TestMigrateRunsTheRunnerAndPassesItsArguments(t *testing.T) {
	root := fakeRunnerProject(t)
	srv := NewServer("test")

	status := callTool(t, srv, "breeze_migrate", map[string]any{"action": "status", "dir": root})
	if status.IsError {
		t.Fatalf("status failed: %s", status.Content[0].Text)
	}
	if !strings.Contains(status.Content[0].Text, "args: status") {
		t.Errorf("the runner was not given the action: %s", status.Content[0].Text)
	}

	down := callTool(t, srv, "breeze_migrate", map[string]any{"action": "down", "steps": 2, "dir": root})
	if !strings.Contains(down.Content[0].Text, "args: down 2") {
		t.Errorf("steps did not reach the runner: %s", down.Content[0].Text)
	}

	up := callTool(t, srv, "breeze_migrate", map[string]any{
		"action": "up", "dir": root, "migrations_dir": "db/migrations", "driver": "pgx", "timeout": "30s",
	})
	if !strings.Contains(up.Content[0].Text, "args: up --dir=db/migrations --driver=pgx --timeout=30s") {
		t.Errorf("flags did not reach the runner in order: %s", up.Content[0].Text)
	}
}

// TestMigrateNeverReturnsTheConnectionString: the runner is free to print whatever
// it likes, and a driver error that echoes the DSN is the ordinary way a password
// ends up in a log. The server holds the secret, so the server scrubs it.
func TestMigrateNeverReturnsTheConnectionString(t *testing.T) {
	root := fakeRunnerProject(t)
	t.Setenv("BREEZE_DATABASE_URL", "postgres://app:s3cret-pass@db.internal:5432/app")
	srv := NewServer("test")

	res := callTool(t, srv, "breeze_migrate", map[string]any{"action": "status", "dir": root})
	if res.IsError {
		t.Fatalf("status failed: %s", res.Content[0].Text)
	}
	text := res.Content[0].Text
	if strings.Contains(text, "s3cret-pass") || strings.Contains(text, "db.internal") {
		t.Errorf("the connection string reached the caller: %s", text)
	}
	if !strings.Contains(text, "[BREEZE_DATABASE_URL]") {
		t.Errorf("the redaction is not visible, so the caller cannot tell output was altered: %s", text)
	}
}

// TestMigrateReportsAFailingRunnerAndSurvivesIt is the reason migrate does not call
// generator.Migrate. That function ends in os.Exit with the runner's status; run
// that way, this test would terminate the test binary instead of failing.
func TestMigrateReportsAFailingRunnerAndSurvivesIt(t *testing.T) {
	root := fakeRunnerProject(t)
	srv := NewServer("test")

	t.Setenv("FAKE_RUNNER_EXIT", "3")
	failed := callTool(t, srv, "breeze_migrate", map[string]any{"action": "up", "dir": root})
	if !failed.IsError {
		t.Fatalf("a runner that exited 3 was reported as success: %s", failed.Content[0].Text)
	}
	text := failed.Content[0].Text
	if !strings.Contains(text, "migration 0002 failed: relation already exists") {
		t.Errorf("the runner's own diagnostic was dropped: %s", text)
	}
	// go run prints the runner's own status and exits 1 itself, so the status that
	// matters reaches the caller through the output, and the message must not
	// present go run's 1 as if it were the runner's.
	if !strings.Contains(text, "exit status 3") {
		t.Errorf("the runner's own exit status was lost: %s", text)
	}
	if !strings.Contains(text, "the migration runner failed (go run exited with status 1)") {
		t.Errorf("the failure is not reported as a failure of the run: %s", text)
	}

	// Still alive, and still usable.
	t.Setenv("FAKE_RUNNER_EXIT", "")
	again := callTool(t, srv, "breeze_migrate", map[string]any{"action": "status", "dir": root})
	if again.IsError {
		t.Errorf("the next call failed after a failing runner: %s", again.Content[0].Text)
	}
}

func TestMigrateExplainsAMissingRunner(t *testing.T) {
	root := t.TempDir()
	withWorkspace(t, root)
	srv := NewServer("test")

	noModule := callTool(t, srv, "breeze_migrate", map[string]any{"action": "status", "dir": root})
	if !noModule.IsError || !strings.Contains(noModule.Content[0].Text, "no go.mod") {
		t.Errorf("a directory that is not a module was not named as such: %+v", noModule)
	}

	writeGo(t, root, "go.mod", "module example.com/norunner\n\ngo 1.24\n")
	noRunner := callTool(t, srv, "breeze_migrate", map[string]any{"action": "status", "dir": root})
	text := noRunner.Content[0].Text
	if !noRunner.IsError || !strings.Contains(text, "breeze_add") || !strings.Contains(text, "migrator") {
		t.Errorf("a missing runner did not say how to get one: %s", text)
	}
}

func TestMigrateRequiresAnActionBeforeTouchingAnything(t *testing.T) {
	srv := NewServer("test")
	res := callTool(t, srv, "breeze_migrate", map[string]any{})
	if !res.IsError || !strings.Contains(res.Content[0].Text, "action is required") {
		t.Errorf("a call with no action was not refused: %+v", res)
	}
}

// migrateResult is also exercised directly for the outcomes a test cannot provoke
// cheaply: a stuck runner would take eight minutes to time out for real.
func TestMigrateResultShapesEveryOutcome(t *testing.T) {
	t.Setenv("BREEZE_DATABASE_URL", "")

	if res := migrateResult(goRun{ExitCode: 0, Output: "applied 2 migrations\n"}); res.IsError ||
		res.Content[0].Text != "applied 2 migrations" {
		t.Errorf("success: %+v", res)
	}
	if res := migrateResult(goRun{ExitCode: 0}); res.IsError || !strings.Contains(res.Content[0].Text, "no output") {
		t.Errorf("silent success: %+v", res)
	}
	if res := migrateResult(goRun{ExitCode: -1, StartErr: errNoGo}); !res.IsError ||
		!strings.Contains(res.Content[0].Text, "toolchain") {
		t.Errorf("a runner that could not start: %+v", res)
	}

	stuck := migrateResult(goRun{Command: "go run ./cmd/migrate up", TimedOut: true, ExitCode: -1, Output: "applying 0003\n"})
	if !stuck.IsError {
		t.Fatalf("a timed-out runner was reported as success: %+v", stuck)
	}
	for _, want := range []string{"did not finish", "half-applied", "action=status", "applying 0003"} {
		if !strings.Contains(stuck.Content[0].Text, want) {
			t.Errorf("a timed-out run's message lacks %q: %s", want, stuck.Content[0].Text)
		}
	}
}

var errNoGo = &pathError{"the Go toolchain is not on PATH"}

type pathError struct{ msg string }

func (e *pathError) Error() string { return e.msg }

func TestRedactSecret(t *testing.T) {
	const dsn = "postgres://app:hunter2@db:5432/app"

	if got := redactSecret("dial "+dsn+" failed; retrying "+dsn, dsn); strings.Contains(got, "hunter2") ||
		strings.Count(got, "[BREEZE_DATABASE_URL]") != 2 {
		t.Errorf("every occurrence must be replaced: %q", got)
	}
	// Too short to be a connection string: masking it would blank ordinary words.
	if got := redactSecret("the app connected", "app"); got != "the app connected" {
		t.Errorf("a short value was masked: %q", got)
	}
	if got := redactSecret("nothing to hide", ""); got != "nothing to hide" {
		t.Errorf("an unset secret changed the text: %q", got)
	}
}

// ── the runner's location ────────────────────────────────────────────────────

// TestMigrationRunnerPackageMatchesWhatBreezeAddWrites pins migrationRunnerPkg to
// the real generator. The constant is a copy of one the generator keeps unexported,
// and a copy is only safe while something fails when the two drift.
func TestMigrationRunnerPackageMatchesWhatBreezeAddWrites(t *testing.T) {
	root := t.TempDir()
	writeGo(t, root, "go.mod", "module example.com/runnerpath\n\ngo 1.24\n")
	t.Chdir(root)

	if err := generator.Add([]string{"migrator", "--driver=sqlite"}); err != nil {
		t.Fatalf("breeze add migrator: %v", err)
	}
	if err := requireMigrationRunner(root); err != nil {
		t.Errorf("migrate does not find the runner breeze add just wrote: %v", err)
	}
}
