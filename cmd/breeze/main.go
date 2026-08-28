// Command breeze is the Breeze framework's project scaffolding and code
// generation CLI, in the spirit of `rails new` / `rails generate`.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/nelthaarion/breeze/internal/generator"
)

func main() {
	if len(os.Args) < 2 {
		printUsage(os.Stderr)
		os.Exit(1)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "new":
		err = generator.New(args)
	case "generate", "g":
		err = generator.Generate(args)
	case "add":
		err = generator.Add(args)
	case "routes":
		err = generator.Routes(args)
	case "migrate":
		err = generator.Migrate(args)
	case "makemigration":
		err = generator.MakeMigration(args)
	case "version", "--version", "-v":
		generator.PrintVersion(os.Stdout)
		return

	case "help", "-h", "--help":
		err = runHelp(args)
	default:
		fmt.Fprintf(os.Stderr, "breeze: unknown command %q\n\n", cmd)
		printUsage(os.Stderr)
		os.Exit(1)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "breeze: %v\n", err)
		os.Exit(1)
	}
}

// runHelp serves both `breeze help` and `breeze help <command>`. Per-command
// help exists because the flat usage block below cannot carry the detail each
// command needs — `add` alone has 22 features with their own flags — without
// becoming a wall nobody reads.
func runHelp(args []string) error {
	if len(args) == 0 {
		printUsage(os.Stdout)
		return nil
	}

	switch args[0] {
	case "new":
		fmt.Print(`Usage: breeze new <name> [flags]

Scaffolds a project: main.go, go.mod pinned to this CLI's breeze version,
routes_generated.go, features_generated.go, and a handlers package.

Flags:
  --template=api|views   api (default) is a JSON API; views adds the template
                         engine, layouts, components and static files
  --module=<path>        module path for go.mod (default: the project name)

Examples:
  breeze new myapp
  breeze new myapp --template=views --module=example.com/me/myapp
`)
	case "generate", "g":
		generator.PrintGenerateHelp(os.Stdout)
	case "add":
		generator.PrintAddHelp(os.Stdout)

	case "routes":
		fmt.Print(`Usage: breeze routes [--json]

Lists the routes in routes_generated.go by parsing the file — the app is never
built or started, so this works on a project that does not currently compile.

Routes registered by hand in main.go, and those a feature mounts at runtime
(the dashboard, video, the docs UI), are not in that file and so are not
listed.

Flags:
  --json   emit machine-readable JSON instead of a table
`)
	case "migrate":
		generator.PrintMigrateHelp(os.Stdout)

	case "makemigration":
		fmt.Print(`Usage: breeze makemigration <Name> [--dir=<path>]

Creates a paired NNNN_<name>.up.sql / .down.sql in the migrations directory,
numbered one past the highest existing version.

Flags:
  --dir=<path>   migrations directory (default "migrations")

Example:
  breeze makemigration CreateUsersTable
`)
	case "version":
		fmt.Print("Usage: breeze version\n\nPrints the CLI's breeze module version, Go toolchain and platform.\n")
	default:
		return fmt.Errorf("unknown command %q — run `breeze help` for the list", args[0])
	}
	return nil
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `breeze — scaffolding and code generation for the Breeze web framework

Usage:
  breeze new <name> [--template=api|views] [--module=<import-path>]
  breeze generate <kind> [Name] [args...]     scaffold code you then edit
  breeze add <feature> [flags]                wire a framework feature in
  breeze routes [--json]                      list generated routes
  breeze migrate [up|down [n]|status]         run migrations
  breeze makemigration <Name>                 create a migration pair
  breeze version
  breeze help [command]

Generators:
  handler <Name>       route group with CRUD stubs
  resource <Name>      handler + struct + OpenAPI docs + validation
  model <Name>         struct and a matching migration pair
  event <Name>         event type and its subscribe helper
  listener <Event>     subscriber for an existing event
  workflow <Name>      workflow definition with steps
  middleware <Name>    breeze.HandlerFunc stub
  ws <Name>            WebSocket handler
  view <Name>          HTML view and its route
  job <Name>           background job, registered with the dashboard
  grpc <Name>          gRPC service skeleton

Features (breeze add --list for all 23):
  events observability recovery logging security cors compression ratelimit
  dashboard i18n jwt oauth2 etag docs static video websocket jsonrpc
  templates workflow tuning migrator fleet


Aliases:
  g    generate

Examples:
  breeze new myapp
  breeze generate resource User name:string email:string age:int
  breeze generate workflow Signup --steps=validate,create,notify
  breeze add dashboard --allow-writes
  breeze add events --async
  breeze routes

Run "breeze help <command>" for detail on one command.
`)
}
