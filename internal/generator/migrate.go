package generator

// `breeze migrate` and `breeze makemigration`.
//
// migrate does not open the database itself. It cannot: database/sql resolves a
// driver by name from the drivers registered in the running binary, and this
// binary registers none â€” breeze depends on no SQL driver, so that every
// project does not inherit Postgres, MySQL and SQLite whether it uses them or
// not. The previous version of this file called sql.Open anyway and failed with
// `sql: unknown driver "postgres"` on every invocation.
//
// So the driver is chosen in the project instead. `breeze add migrator
// --driver=<name>` writes cmd/migrate/main.go there, blank-importing that
// driver, and the subcommands here forward to it. Flags are passed through
// untouched, so the runner's own --dsn/--driver/--dir/--timeout work without
// this side needing to know about them.

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// migratorPkg is the project-relative package the generated runner lives in.
const migratorPkg = "cmd/migrate"

var migrateSubcommands = map[string]bool{"up": true, "down": true, "status": true}

func runMigrate(args []string) error {
	// Bare `breeze migrate` means up, matching the old behaviour.
	forward := args
	sub := "up"
	if len(args) == 0 {
		forward = []string{"up"}
	} else if !strings.HasPrefix(args[0], "-") {
		sub = args[0]
	}

	if !migrateSubcommands[sub] {
		return fmt.Errorf("unknown migrate subcommand %q â€” must be up, down, or status", sub)
	}

	// `down 2` is validated here as well as in the runner so a typo costs a
	// message rather than a compile-and-run round trip.
	if sub == "down" && len(args) > 1 && !strings.HasPrefix(args[1], "-") {
		if n, err := strconv.Atoi(args[1]); err != nil || n < 1 {
			return fmt.Errorf("invalid step count %q â€” expected a positive integer, as in `breeze migrate down 2`", args[1])
		}
	}

	if err := requireMigrator(); err != nil {
		return err
	}
	return execMigrator(forward)
}

// migratorPresent reports whether the project has a generated migration runner.
//
// The runner is a standalone main package, not a block in
// features_generated.go â€” so this file is the only record that `add migrator`
// has been run. Asking hasBlock about "migrator" is always false and always
// will be, which is how `generate model` came to recommend `add migrator` to
// people who already had one.
func migratorPresent() bool {
	info, err := os.Stat(filepath.Join(migratorPkg, "main.go"))
	return err == nil && !info.IsDir()
}

// requireMigrator checks the generated runner is present, and explains how to
// get it when it is not. Reporting the missing file is far more useful than
// letting `go run` fail on a package that does not exist.
func requireMigrator() error {
	if _, err := os.Stat("go.mod"); err != nil {
		if os.IsNotExist(err) {
			return errors.New("no go.mod in the current directory â€” run this from the root of a Breeze project")
		}
		return err
	}

	if migratorPresent() {
		return nil
	}

	return fmt.Errorf(`no migration runner in this project (%s/main.go is missing)

The breeze binary has no SQL driver compiled into it, so it cannot connect to
your database â€” database/sql only knows the drivers registered in the process
that calls sql.Open. Generate a runner that does:

  breeze add migrator --driver=postgres     # or pgx, mysql, sqlite, sqlite3
  go mod tidy

Then set the connection string and re-run:

  export BREEZE_DATABASE_URL="postgres://user:pass@localhost:5432/db?sslmode=disable"
  breeze migrate status`, migratorPkg)
}

// execMigrator runs the project's migration binary, passing stdio through so
// its output appears as if it were this command's own.
func execMigrator(args []string) error {
	cmd := exec.Command("go", append([]string{"run", "./" + migratorPkg}, args...)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	err := cmd.Run()
	if err == nil {
		return nil
	}

	// The runner prints its own diagnostics before exiting non-zero. Returning
	// the error here would stack a "breeze: exit status 1" on top of a message
	// that already said what went wrong, so adopt its exit code instead.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		os.Exit(exitErr.ExitCode())
	}
	return fmt.Errorf("running ./%s: %w", migratorPkg, err)
}

func runMakeMigration(args []string) error {
	fs := flag.NewFlagSet("makemigration", flag.ContinueOnError)
	dir := fs.String("dir", "migrations", "directory to write the migration pair into")

	flagArgs, positional := splitFlagsAndPositional(fs, args)
	if err := parseFlags(fs, flagArgs); err != nil {
		return err
	}
	if len(positional) != 1 {
		return errors.New("usage: breeze makemigration <Name> [--dir=<path>]")
	}

	name := strings.TrimSpace(positional[0])
	if name == "" {
		return errors.New("migration name cannot be empty")
	}

	migrationsDir := *dir
	if err := validatePathFlag("dir", migrationsDir); err != nil {
		return err
	}
	if err := os.MkdirAll(migrationsDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", migrationsDir, err)
	}

	version, err := nextMigrationVersion(migrationsDir)
	if err != nil {
		return err
	}

	versionStr := fmt.Sprintf("%04d", version)
	slug := toSlug(name)
	stamp := time.Now().Format(time.RFC3339)

	upFile := filepath.Join(migrationsDir, fmt.Sprintf("%s_%s.up.sql", versionStr, slug))
	downFile := filepath.Join(migrationsDir, fmt.Sprintf("%s_%s.down.sql", versionStr, slug))

	// Refuse rather than overwrite: a migration that has already been applied
	// is recorded by checksum, so silently replacing its contents produces a
	// mismatch that is confusing to diagnose later.
	for _, path := range []string{upFile, downFile} {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s already exists", path)
		}
	}

	upContent := fmt.Sprintf("-- Migration %s: %s\n-- Created at %s\n\n", versionStr, slug, stamp)
	if err := os.WriteFile(upFile, []byte(upContent), 0o644); err != nil {
		return fmt.Errorf("creating %s: %w", upFile, err)
	}

	downContent := fmt.Sprintf("-- Rollback for migration %s: %s\n-- Created at %s\n\n", versionStr, slug, stamp)
	if err := os.WriteFile(downFile, []byte(downContent), 0o644); err != nil {
		os.Remove(upFile)
		return fmt.Errorf("creating %s: %w", downFile, err)
	}

	fmt.Printf("Created migration %s:\n  %s\n  %s\n", versionStr, upFile, downFile)
	return nil
}

// nextMigrationVersion returns one past the highest NNNN_ prefix in dir.
func nextMigrationVersion(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", dir, err)
	}

	highest := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".up.sql") {
			continue
		}
		underscore := strings.Index(entry.Name(), "_")
		if underscore <= 0 {
			continue
		}
		if v, err := strconv.Atoi(entry.Name()[:underscore]); err == nil && v > highest {
			highest = v
		}
	}
	return highest + 1, nil
}

// toSlug converts CamelCase to snake_case.
//
// Runs of capitals are kept together, so "RequestID" becomes "request_id" and
// "HTTPServer" becomes "http_server". Treating every capital as a word boundary
// produced "request_i_d" â€” which reached further than it looks, because this
// function names generated files, migration files, default table names, and the
// db column for every field of a generated model.
func toSlug(name string) string {
	runes := []rune(name)
	var buf strings.Builder
	for i, r := range runes {
		if isASCIIUpper(r) && i > 0 {
			// A boundary before a capital that follows a non-capital
			// ("userID" -> user_id), and before the last capital of a run when
			// a lowercase follows it, since that capital starts a new word
			// ("HTTPServer" -> http_server).
			prevIsUpper := isASCIIUpper(runes[i-1])
			nextIsLower := i+1 < len(runes) && runes[i+1] >= 'a' && runes[i+1] <= 'z'
			if !prevIsUpper || nextIsLower {
				buf.WriteRune('_')
			}
		}
		buf.WriteString(strings.ToLower(string(r)))
	}
	return buf.String()
}

func isASCIIUpper(r rune) bool { return r >= 'A' && r <= 'Z' }

func printMigrateHelp(w io.Writer) {
	fmt.Fprintf(w, `Usage: breeze migrate [up|down [n]|status] [flags]

Subcommands:
  up             apply every pending migration (the default)
  down [n]       roll back the last n migrations (default 1)
  status         show each migration and whether it is applied

Flags are forwarded to the project's runner:
  --dir=<path>   migrations directory (default "migrations")
  --dsn=<url>    connection string (default $BREEZE_DATABASE_URL)
  --driver=<n>   database/sql driver name (default $BREEZE_DATABASE_DRIVER)
  --timeout=<d>  overall deadline (default 5m)

This command runs ./%s, which `+"`breeze add migrator`"+` generates. The
breeze binary has no SQL driver compiled in, so it cannot open your database
itself â€” the generated runner is where the driver gets chosen, and it lands in
your go.mod rather than the framework's.

Setup:
  breeze add migrator --driver=postgres
  go mod tidy
  export BREEZE_DATABASE_URL="postgres://user:pass@localhost:5432/db?sslmode=disable"

Examples:
  breeze migrate status
  breeze migrate up
  breeze migrate down 2
  breeze migrate up --dir=db/migrations
`, migratorPkg)
}
