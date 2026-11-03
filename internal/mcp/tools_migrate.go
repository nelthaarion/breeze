package mcp

// tools_migrate.go — the CLI's makemigration and migrate, as tools.
//
// makemigration is an ordinary generator call and is wired the way breeze_generate
// is: build the argv, run the generator with stdout captured, report what it said.
//
// migrate is not, and the difference is why it does not go through
// generator.Migrate. That function finishes by calling os.Exit with the runner's own
// exit status, which is right for a CLI that is about to exit anyway and wrong
// inside a server: the first migration that failed would end the MCP process, and
// every call after it with it. So this runs the same command the CLI does —
// `go run ./cmd/migrate <args>` — itself, through the runGo the verification tools
// use, and reports the exit status as a result instead of acting on it.
//
// The connection string is deliberately not an argument. A DSN passed as a tool
// argument is a database password in an agent's transcript. The runner reads
// BREEZE_DATABASE_URL from its environment, so whoever starts the server decides
// which database an agent can reach, and the agent only chooses what to do to it.
// For the same reason the runner's output is scrubbed of that value before it is
// returned: a driver error is exactly the kind of message that echoes it.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nelthaarion/breeze/v2/internal/generator"
)

const (
	// migrationRunnerPkg is where `breeze add migrator` writes the runner. The
	// generator keeps the same value unexported; a test pins the two together
	// against what the generator really writes.
	migrationRunnerPkg = "cmd/migrate"

	// migrationDSNEnv is the variable the generated runner reads its connection
	// string from when it is not given --dsn.
	migrationDSNEnv = "BREEZE_DATABASE_URL"

	// migrateTimeout bounds the whole call: compiling the runner, then the runner's
	// own --timeout, which defaults to five minutes. It is longer than that default
	// so a runner that hits its own deadline reports it, instead of being killed
	// from outside with nothing said.
	migrateTimeout = 8 * time.Minute
)

func registerMigrationTools(s *Server) {
	s.addTool(makeMigrationTool())
	s.addTool(migrateTool())
}

// ── breeze_makemigration ─────────────────────────────────────────────────────

type makeMigrationArgs struct {
	Name          string `json:"name"`
	MigrationsDir string `json:"migrations_dir"`
	Dir           string `json:"dir"`
}

func makeMigrationTool() *tool {
	return &tool{
		name: "breeze_makemigration",
		description: "Create an empty, numbered up/down SQL migration pair in a Breeze project " +
			"(NNNN_name.up.sql and NNNN_name.down.sql). The files hold only a header comment: " +
			"write the SQL into them, then apply it with breeze_migrate. Refuses to overwrite " +
			"an existing migration. breeze_generate with kind=model already writes a pair for " +
			"a new table; use this for every other schema change.",
		schema: objectSchema(map[string]any{
			"name":           stringProp("Migration name, e.g. AddIndexToUsers. CamelCase becomes snake_case in the file names."),
			"migrations_dir": stringProp("Directory for the pair, relative to the project root. Defaults to migrations."),
			"dir":            stringProp("Project root. Defaults to the server's working directory."),
		}, "name"),
		run: func(raw json.RawMessage) toolCallResult {
			var a makeMigrationArgs
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}
			argv, err := a.argv()
			if err != nil {
				return errorResult(err.Error())
			}

			return runGenerator(a.Dir, func() error { return generator.MakeMigration(argv) })
		},
	}
}

func (a makeMigrationArgs) argv() ([]string, error) {
	if strings.TrimSpace(a.Name) == "" {
		return nil, errors.New("name is required")
	}

	argv := []string{a.Name}
	if a.MigrationsDir != "" {
		// The generator checks this too. Checking here as well costs nothing and
		// keeps the refusal's wording the same as breeze_migrate's.
		if err := generator.ValidatePathFlag("migrations_dir", a.MigrationsDir); err != nil {
			return nil, err
		}
		argv = append(argv, "--dir="+a.MigrationsDir)
	}
	return argv, nil
}

// ── breeze_migrate ───────────────────────────────────────────────────────────

type migrateArgs struct {
	Action        string `json:"action"`
	Steps         int    `json:"steps"`
	MigrationsDir string `json:"migrations_dir"`
	Driver        string `json:"driver"`
	Timeout       string `json:"timeout"`
	Dir           string `json:"dir"`
}

func migrateTool() *tool {
	return &tool{
		name: "breeze_migrate",
		description: "Apply, roll back or report database migrations by running the project's own " +
			"migration runner (cmd/migrate, written by breeze_add with feature=migrator). " +
			"action=status is read-only; action=up applies every pending migration; action=down " +
			"rolls back the last `steps` (default 1) and destroys whatever those migrations created. " +
			"There is no default action. The database is whichever BREEZE_DATABASE_URL names in the " +
			"environment of the server process: no connection string is accepted as an argument, " +
			"and none is echoed back.",
		schema: objectSchema(map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"status", "up", "down"},
				"description": "status lists each migration and whether it is applied. up applies the pending ones. down rolls back.",
			},
			"steps": map[string]any{
				"type":        "integer",
				"minimum":     1,
				"description": "For action=down: how many migrations to roll back. Defaults to 1.",
			},
			"migrations_dir": stringProp("Migrations directory, relative to the project root. Defaults to migrations."),
			"driver":         stringProp("database/sql driver name, when it is not the runner's default (BREEZE_DATABASE_DRIVER)."),
			"timeout":        stringProp("Deadline for the runner, as a duration such as 30s or 2m. Defaults to 5m."),
			"dir":            stringProp("Project root. Defaults to the server's working directory."),
		}, "action"),
		run: func(raw json.RawMessage) toolCallResult {
			var a migrateArgs
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}
			argv, err := a.argv()
			if err != nil {
				return errorResult(err.Error())
			}

			// Confined like every other caller-supplied path, and before anything
			// is run: the runner is project code, so where it runs is the decision
			// that matters.
			root, err := resolvePath(a.Dir)
			if err != nil {
				return errorResult(err.Error())
			}
			if err := requireMigrationRunner(root); err != nil {
				return errorResult(err.Error())
			}

			run := runGo(root, migrateTimeout, append([]string{"run", "./" + migrationRunnerPkg}, argv...)...)
			return migrateResult(run)
		},
	}
}

// driverName is what a database/sql driver is called: letters, digits, dash and
// underscore. The value reaches the runner as one argv element, so this is not
// about shell injection; it keeps a free-form string out of a flag whose only valid
// values are identifiers.
var driverName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)

// argv renders the arguments as the runner accepts them.
//
// action is required and has no default. The CLI's bare `breeze migrate` means up,
// which is a reasonable thing to type and an unreasonable thing for an omitted
// argument to do.
func (a migrateArgs) argv() ([]string, error) {
	switch a.Action {
	case "status", "up", "down":
	case "":
		return nil, errors.New("action is required: status, up or down. There is no default; " +
			"the CLI's default is up, and an omitted argument should not apply migrations")
	default:
		return nil, fmt.Errorf("unknown action %q; must be status, up or down", a.Action)
	}

	if a.Steps < 0 {
		return nil, errors.New("steps must be a positive integer")
	}
	if a.Steps > 0 && a.Action != "down" {
		return nil, fmt.Errorf("steps applies only to action=down, not %s", a.Action)
	}

	argv := []string{a.Action}
	if a.Steps > 0 {
		argv = append(argv, strconv.Itoa(a.Steps))
	}

	if a.MigrationsDir != "" {
		if err := generator.ValidatePathFlag("migrations_dir", a.MigrationsDir); err != nil {
			return nil, err
		}
		argv = append(argv, "--dir="+a.MigrationsDir)
	}
	if a.Driver != "" {
		if !driverName.MatchString(a.Driver) {
			return nil, fmt.Errorf("driver %q is not a database/sql driver name such as postgres, pgx, mysql or sqlite", a.Driver)
		}
		argv = append(argv, "--driver="+a.Driver)
	}
	if a.Timeout != "" {
		if d, err := time.ParseDuration(a.Timeout); err != nil || d <= 0 {
			return nil, fmt.Errorf("timeout %q is not a positive duration such as 30s or 2m", a.Timeout)
		}
		argv = append(argv, "--timeout="+a.Timeout)
	}
	return argv, nil
}

// requireMigrationRunner explains how to get the runner when the project has none.
// Reporting the missing file is far more useful than letting `go run` fail on a
// package that does not exist, and the cause is never the agent's arguments.
func requireMigrationRunner(root string) error {
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no go.mod in %s; point dir at the root of a Breeze project", root)
		}
		return err
	}

	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(migrationRunnerPkg), "main.go"))
	if err != nil || info.IsDir() {
		return fmt.Errorf("this project has no migration runner (%s/main.go is missing). "+
			"The breeze binary has no SQL driver compiled in, so the runner is generated into the project: "+
			"call breeze_add with feature=migrator and flags {\"driver\":\"postgres\"} (or pgx, mysql, sqlite, sqlite3), "+
			"then run `go mod tidy` in the project", migrationRunnerPkg)
	}
	return nil
}

// migrateResult shapes a finished runner into a result.
//
// A non-zero exit is an error result carrying the runner's own output, which is
// where it says what went wrong and which migration it stopped at. The exit status
// is reported rather than acted on; see the file comment for why.
//
// The status reported is go run's, and that is not the runner's: go run exits 1 for
// any program that exits non-zero and prints the program's own status as an
// "exit status N" line, which is in the output. The message says whose status it is
// so a reader does not take a 1 for the runner's.
func migrateResult(run goRun) toolCallResult {
	output := strings.TrimSpace(redactSecret(run.Output, os.Getenv(migrationDSNEnv)))

	switch {
	case run.StartErr != nil:
		return errorResult(run.StartErr.Error())

	case run.TimedOut:
		msg := fmt.Sprintf("`%s` did not finish within %s and was stopped. A migration stopped part-way "+
			"can leave the schema half-applied, so call breeze_migrate with action=status before doing anything else.",
			run.Command, migrateTimeout)
		if output != "" {
			msg += "\n\n" + output
		}
		return errorResult(msg)

	case run.ExitCode != 0:
		msg := fmt.Sprintf("the migration runner failed (go run exited with status %d)", run.ExitCode)
		if output != "" {
			msg = output + "\n\n" + msg
		}
		return errorResult(msg)
	}

	if output == "" {
		return textResult("done (the migration runner reported no output)")
	}
	return textResult(output)
}

// redactSecret replaces every occurrence of secret in text.
//
// A value too short to be a connection string is left alone: masking a three
// character "secret" would blank out ordinary words in the output, and a DSN that
// short is not a DSN.
func redactSecret(text, secret string) string {
	if len(secret) < 8 {
		return text
	}
	return strings.ReplaceAll(text, secret, "["+migrationDSNEnv+"]")
}
