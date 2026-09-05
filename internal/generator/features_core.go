package generator

// Core subsystem features: the ones that stand up long-lived infrastructure
// rather than wrapping requests. Their generated blocks declare the package
// handles â€” EventBus, ObsCollector, DashboardCollector, WorkflowEngine â€” that
// other features and the user's own code reference by name.
//
// Priorities put the handle producers before their consumers: events (5) and
// observability (8) run ahead of dashboard (80) and workflow (200), because
// those read the bus and collector the earlier blocks created. Neither events
// nor observability calls router.Use, so sitting first costs the middleware
// order nothing.

import (
	"flag"
	"fmt"
	"sort"
	"strings"
)

func registerCoreFeatures() {
	registerEvents()
	registerObservability()
	registerDashboard()
	registerWorkflow()
	registerTuning()
	registerMigrator()
}

func registerEvents() {
	register(&feature{
		Name:     "events",
		Summary:  "typed event bus shared by the framework and your own code",
		Priority: 5,
		Imports:  []string{eventsImport},
		Build: func(fs *flag.FlagSet) func(featureCtx) (featureOutput, error) {
			async := fs.Bool("async", false, "dispatch to listeners on worker goroutines instead of inline")
			workers := fs.Int("workers", 0, "async worker count (0 = one per CPU)")
			queue := fs.Int("queue-size", 1024, "async queue depth")
			metrics := fs.Bool("metrics", true, "count emits, errors and panics per event type")
			continueOnError := fs.Bool("continue-on-error", true, "keep calling remaining listeners after one fails")

			return func(ctx featureCtx) (featureOutput, error) {
				var cfg strings.Builder
				fmt.Fprintf(&cfg, "\t\tContinueOnError: %t,\n", *continueOnError)
				// Metrics is on for the zero-value Config, so the off switch is
				// DisableMetrics â€” Config.normalize resolves the pair as
				// `Metrics = !DisableMetrics` and would overwrite `Metrics:
				// false` right back to true.
				if !*metrics {
					cfg.WriteString("\t\tDisableMetrics:  true,\n")
				}

				var imports []string
				if *async {
					// Async is an AsyncMode, not a bool. AsyncWorkerPool is the
					// bounded one: AsyncGoroutine would spawn a goroutine per
					// listener per emit with no ceiling.
					cfg.WriteString("\t\tAsync:           events.AsyncWorkerPool,\n")
					if *workers > 0 {
						fmt.Fprintf(&cfg, "\t\tWorkers:         %d,\n", *workers)
					} else {
						cfg.WriteString("\t\tWorkers:         runtime.NumCPU(),\n")
						imports = append(imports, runtimeImport)
					}
					fmt.Fprintf(&cfg, "\t\tQueueSize:       %d,\n", *queue)
				}

				body := `// EventBus is this application's event bus.
//
// The framework publishes here too â€” requests, WebSocket lifecycle, workflow
// steps â€” so keeping one bus for your own events means every subscriber and
// the dashboard's Events page see a single stream.
//
// Publish and subscribe with the generic helpers, which take the bus as their
// first argument:
//
//	events.OnTypeBus[UserCreated](EventBus, func(ctx *events.Context, e UserCreated) error {
//		return mailer.Welcome(e.Email)
//	})
//	events.EmitBus(EventBus, UserCreated{Email: "a@example.com"})
var EventBus *events.Bus

func setupEvents(app *breeze.Breeze, router *breeze.Router) {
	EventBus = events.New(events.Config{
` + cfg.String() + `	})
}`

				notes := []string{
					"Subscribe with events.OnTypeBus[T](EventBus, handler); publish with events.EmitBus(EventBus, value).",
					"Run `breeze generate event <Name> [field:type...]` to scaffold an event type.",
				}
				if *async {
					notes = append(notes, "Async dispatch means Emit returns before listeners finish â€” use EmitAsyncWaitBus where you need to observe their errors.")
				}
				return featureOutput{Body: body, Imports: imports, Notes: notes}, nil
			}
		},
	})
}

func registerObservability() {
	register(&feature{
		Name:      "observability",
		Summary:   "in-process signal collector for requests, queries and jobs",
		Priority:  8,
		Imports:   []string{observabilityImport, eventsImport},
		DependsOn: []string{"events"},
		Build: func(fs *flag.FlagSet) func(featureCtx) (featureOutput, error) {
			capacity := fs.Int("capacity", 1000, "ring-buffer capacity per signal kind")
			metrics := fs.Bool("metrics", true, "aggregate counters and latency histograms")

			return func(ctx featureCtx) (featureOutput, error) {
				// The collector is fed by the event bus. With an events block
				// present we share that bus; without one, events.Default is
				// the bus the framework publishes to by default, so attaching
				// to it still collects framework signals.
				bus := "events.Default"
				if ctx.HasEvents {
					bus = "EventBus"
				}

				body := fmt.Sprintf(`// ObsCollector holds the recent-signal ring buffers and aggregate metrics.
// Read from it with the accessor methods; it is safe for concurrent use.
var ObsCollector *observability.Collector

func setupObservability(app *breeze.Breeze, router *breeze.Router) {
	ObsCollector = observability.NewCollector(observability.Config{
		Capacity: %d,
		Metrics:  %t,
	})

	// Framework signals arrive as events, so collection is a subscription
	// rather than a hook. The returned detach func is ignored here: this
	// collector lives as long as the process.
	observability.AttachEvents(%s, ObsCollector)
}`, *capacity, *metrics, bus)

				notes := []string{}
				if !ctx.HasEvents {
					notes = append(notes, "Attached to events.Default. Run `breeze add events` for a bus you own, then re-run `breeze add observability` to point the collector at it.")
				}
				return featureOutput{Body: body, Notes: notes}, nil
			}
		},
	})
}

func registerDashboard() {
	register(&feature{
		Name:      "dashboard",
		Summary:   "developer dashboard: requests, queries, timeline, metrics",
		Priority:  80,
		Imports:   []string{dashboardImport},
		DependsOn: []string{"events"},
		Build: func(fs *flag.FlagSet) func(featureCtx) (featureOutput, error) {
			basePath := fs.String("basepath", "/dashboard", "URL prefix the dashboard is served under")
			user := fs.String("user", "admin", "basic-auth username")
			pass := fs.String("pass", "admin", "basic-auth password")
			noAuth := fs.Bool("no-auth", false, "serve without basic auth (local development only)")
			allowWrites := fs.Bool("allow-writes", false, "allow the dashboard's query console to run writes")

			return func(ctx featureCtx) (featureOutput, error) {
				var extra strings.Builder
				if *noAuth {
					extra.WriteString("\tcfg.DisableAuth = true\n")
				} else {
					fmt.Fprintf(&extra, "\tcfg.Username = %q\n", *user)
					fmt.Fprintf(&extra, "\tcfg.Password = %q\n", *pass)
				}
				if *allowWrites {
					extra.WriteString("\tcfg.AllowWrites = true\n")
				}

				attach := ""
				if ctx.HasEvents {
					attach = `
	// Bridge the bus onto the dashboard's live Events page.
	DashboardCollector.AttachEvents(EventBus)`
				}

				body := fmt.Sprintf(`// DashboardCollector is the dashboard's data sink. Register your own
// connections, jobs and health checks on it â€” RegisterConnection,
// RegisterJob, RegisterTask, RegisterHealthCheck, SetDBInspector â€” and they
// show up alongside the framework's own panels.
var DashboardCollector *dashboard.Collector

func setupDashboard(app *breeze.Breeze, router *breeze.Router) {
	cfg := dashboard.DefaultConfig()
	cfg.BasePath = %q
%s
	DashboardCollector = dashboard.Install(app, router, cfg)

	// The middleware is what records requests. It has to wrap the routes it
	// measures, so it must be registered before they run â€” which is what this
	// feature's ordering guarantees.
	router.Use(dashboard.Middleware(DashboardCollector))%s
}`, *basePath, strings.TrimRight(extra.String(), "\n"), attach)

				notes := []string{fmt.Sprintf("Dashboard at %s", *basePath)}
				switch {
				case *noAuth:
					notes = append(notes, "Auth is disabled â€” do not expose this build publicly.")
				case *user == "admin" && *pass == "admin":
					notes = append(notes, "Credentials are the admin/admin default. Change them before this reaches a shared environment.")
				}
				if *allowWrites {
					notes = append(notes, "The query console can execute writes against your database.")
				}
				if !ctx.HasEvents {
					notes = append(notes, "No events block found, so the live Events page stays empty. Run `breeze add events`, then re-run `breeze add dashboard` to wire them together.")
				}
				return featureOutput{Body: body, Notes: notes}, nil
			}
		},
	})
}

func registerWorkflow() {
	register(&feature{
		Name:      "workflow",
		Summary:   "durable multi-step workflow engine with retries and compensation",
		Priority:  200,
		Imports:   []string{workflowImport, timeImport, contextImport, logImport},
		DependsOn: []string{"events", "observability"},
		Build: func(fs *flag.FlagSet) func(featureCtx) (featureOutput, error) {
			workers := fs.Int("workers", 0, "max concurrent step workers (0 = one per CPU)")
			shutdown := fs.Duration("shutdown-timeout", 0, "how long Shutdown waits for in-flight runs (default 30s)")

			return func(ctx featureCtx) (featureOutput, error) {
				var imports []string

				workersLine := "\t\tMaxWorkers:      runtime.NumCPU(),\n"
				if *workers > 0 {
					workersLine = fmt.Sprintf("\t\tMaxWorkers:      %d,\n", *workers)
				} else {
					imports = append(imports, runtimeImport)
				}

				timeout := "30 * time.Second"
				if *shutdown > 0 {
					timeout = fmt.Sprintf("%d * time.Millisecond", shutdown.Milliseconds())
				}

				var cfg strings.Builder
				if ctx.HasEvents {
					cfg.WriteString("\t\tBus:             EventBus,\n")
				}
				if ctx.HasObservability {
					cfg.WriteString("\t\tCollector:       ObsCollector,\n")
				}
				cfg.WriteString(workersLine)
				fmt.Fprintf(&cfg, "\t\tShutdownTimeout: %s,\n", timeout)

				body := fmt.Sprintf(`// WorkflowEngine runs the definitions registered below. A workflow is a
// named sequence of steps with per-step timeout, retry and compensation; the
// engine persists progress so a run survives a step failure rather than
// unwinding the whole thing in memory.
var WorkflowEngine *workflow.Engine

func setupWorkflow(app *breeze.Breeze, router *breeze.Router) {
	WorkflowEngine = workflow.NewEngine(workflow.Config{
%s	})

	// Register your definitions here. Register returns an error for a
	// malformed definition â€” a duplicate step name, a dependency on a step
	// that does not exist â€” so it is worth failing loudly at boot:
	//
	//	if err := WorkflowEngine.Register(workflows.Signup()); err != nil {
	//		log.Fatalf("workflow: %%v", err)
	//	}
	//
	// Scaffold one with: breeze generate workflow Signup --steps=validate,create,notify
}

// ShutdownWorkflow drains in-flight runs.
//
// The engine's workers are independent of the HTTP server, so they outlive
// app.Run returning. Killing the process without this leaves runs stopped
// mid-step: not completed, and not compensated either.
func ShutdownWorkflow() {
	ctx, cancel := context.WithTimeout(context.Background(), %s)
	defer cancel()
	if err := WorkflowEngine.Shutdown(ctx); err != nil {
		log.Printf("workflow: shutdown: %%v", err)
	}
}`, cfg.String(), timeout)

				notes := []string{
					"Call ShutdownWorkflow() on your shutdown path â€” in-flight runs are dropped otherwise.",
					"Scaffold a definition with `breeze generate workflow <Name> --steps=a,b,c`.",
				}
				if !ctx.HasEvents {
					notes = append(notes, "Without an events block the engine emits no step events, so nothing reaches the dashboard timeline. `breeze add events` then re-running `breeze add workflow` connects them.")
				}
				return featureOutput{Body: body, Imports: imports, Notes: notes}, nil
			}
		},
	})
}

func registerTuning() {
	register(&feature{
		Name:     "tuning",
		Summary:  "event-loop execution and header copying knobs",
		Priority: 210,
		Build: func(fs *flag.FlagSet) func(featureCtx) (featureOutput, error) {
			inline := fs.Bool("inline", false, "run handlers on the event loop instead of the worker pool")
			zeroCopy := fs.Bool("zero-copy-headers", true, "parse headers in place instead of copying them out of the read buffer")

			return func(ctx featureCtx) (featureOutput, error) {
				body := fmt.Sprintf(`func setupTuning(app *breeze.Breeze, router *breeze.Router) {
	// Inline execution runs handlers directly on the gnet event loop, saving
	// the hand-off to the worker pool. That is a real latency win for handlers
	// that only touch memory, and a way to stall every connection on the same
	// loop for any handler that does I/O â€” a database call, an outbound HTTP
	// request, a file read. Leave it off unless you know every handler is
	// non-blocking.
	app.SetInlineExecution(%t)

	// Zero-copy headers hand handlers slices into the read buffer rather than
	// copies. Cheaper per request, but the buffer is reused once the handler
	// returns: anything you keep past that point must be copied first.
	app.SetZeroCopyHeaders(%t)
}`, *inline, *zeroCopy)

				notes := []string{
					"Worker-pool size is set in main.go where NewEventLoopWorkerPool is called â€” it is passed to breeze.New, so it cannot be changed from here.",
				}
				if *inline {
					notes = append(notes, "Inline execution is ON: a single blocking handler now stalls every connection sharing its event loop.")
				}
				if *zeroCopy {
					notes = append(notes, "Zero-copy headers are ON: copy any header value you retain beyond the handler's return.")
				}
				return featureOutput{Body: body, Notes: notes}, nil
			}
		},
	})
}

// sqlDrivers maps the --driver flag to the blank import and database/sql
// driver name. Breeze deliberately depends on no SQL driver, so the generated
// runner is where one gets chosen â€” and it lands in the user's go.mod, not
// breeze's.
var sqlDrivers = map[string]struct {
	Import string
	Name   string
}{
	"postgres": {"github.com/lib/pq", "postgres"},
	"pgx":      {"github.com/jackc/pgx/v5/stdlib", "pgx"},
	"mysql":    {"github.com/go-sql-driver/mysql", "mysql"},
	"sqlite":   {"modernc.org/sqlite", "sqlite"},
	"sqlite3":  {"github.com/mattn/go-sqlite3", "sqlite3"},
}

func sqlDriverNames() []string {
	names := make([]string, 0, len(sqlDrivers))
	for n := range sqlDrivers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func registerMigrator() {
	register(&feature{
		Name:    "migrator",
		Summary: "standalone migration runner binary (required by `breeze migrate`)",
		// Standalone: this writes a separate main package, so it has no block
		// in features_generated.go and nothing to call from the dispatcher.
		Standalone: true,
		Priority:   900,
		Build: func(fs *flag.FlagSet) func(featureCtx) (featureOutput, error) {
			driver := fs.String("driver", "postgres", "SQL driver: "+strings.Join(sqlDriverNames(), ", "))

			return func(ctx featureCtx) (featureOutput, error) {
				d, ok := sqlDrivers[*driver]
				if !ok {
					return featureOutput{}, fmt.Errorf("unknown driver %q â€” must be one of: %s",
						*driver, strings.Join(sqlDriverNames(), ", "))
				}

				content := migratorRunner(d.Import, d.Name)

				return featureOutput{
					Files: map[string]string{"cmd/migrate/main.go": content},
					Dirs:  []string{"migrations"},
					Notes: []string{
						fmt.Sprintf("Wrote cmd/migrate/main.go using the %s driver.", *driver),
						fmt.Sprintf("Run `go get %s` (or `go mod tidy`) to add the driver to go.mod.", d.Import),
						"Set BREEZE_DATABASE_URL, then `breeze migrate up` / `breeze migrate status` / `breeze migrate down 1`.",
						"`breeze migrate` shells out to this binary â€” it is what makes those subcommands work at all, since the CLI itself has no driver compiled in.",
					},
				}, nil
			}
		},
	})
}

// migratorRunner builds cmd/migrate/main.go. Placeholders are substituted
// rather than formatted with %s so the runner's own printf verbs stay literal.
func migratorRunner(driverImport, driverName string) string {
	const tmpl = `// Code generated by ` + "`breeze add migrator`" + `. Safe to edit â€” the CLI will not
// rewrite this file.
//
// Why this exists: database/sql resolves a driver by name from the drivers
// registered in the binary that opens the connection. Breeze depends on no SQL
// driver â€” pulling in Postgres, MySQL and SQLite so the CLI can open any of
// them would put all three in every project's dependency graph â€” so the
// ` + "`breeze`" + ` binary cannot talk to your database. This one can, because it
// blank-imports the driver you chose, and ` + "`breeze migrate`" + ` runs it for you.
//
// Usage (directly, or via the wrapper):
//
//	go run ./cmd/migrate up
//	go run ./cmd/migrate down 1
//	go run ./cmd/migrate status
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	_ "__DRIVER_IMPORT__"

	"github.com/nelthaarion/breeze/migrate"
)

func main() {
	dir := flag.String("dir", "migrations", "directory holding the .up.sql / .down.sql files")
	dsn := flag.String("dsn", os.Getenv("BREEZE_DATABASE_URL"), "connection string (default $BREEZE_DATABASE_URL)")
	driver := flag.String("driver", envOr("BREEZE_DATABASE_DRIVER", "__DRIVER_NAME__"), "database/sql driver name")
	timeout := flag.Duration("timeout", 5*time.Minute, "overall deadline")
	flag.Usage = usage
	flag.Parse()

	if err := run(flag.Args(), *dir, *driver, *dsn, *timeout); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, ` + "`" + `Usage: go run ./cmd/migrate <command> [flags]

Commands:
  up             apply every pending migration
  down [n]       roll back the last n migrations (default 1)
  status         show which migrations are applied

Flags:
  --dir=<path>   migrations directory (default "migrations")
  --dsn=<url>    connection string (default $BREEZE_DATABASE_URL)
  --driver=<n>   database/sql driver name (default $BREEZE_DATABASE_DRIVER)
  --timeout=<d>  overall deadline (default 5m)
` + "`" + `)
}

func run(args []string, dir, driver, dsn string, timeout time.Duration) error {
	if len(args) == 0 {
		usage()
		return errors.New("no command given")
	}
	if dsn == "" {
		return errors.New("no connection string: pass --dsn or set BREEZE_DATABASE_URL")
	}
	if info, err := os.Stat(dir); err != nil {
		return fmt.Errorf("migrations directory %q: %w", dir, err)
	} else if !info.IsDir() {
		return fmt.Errorf("%q is not a directory", dir)
	}

	db, err := sql.Open(driver, dsn)
	if err != nil {
		return fmt.Errorf("opening %s: %w", driver, err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Open is lazy, so a bad DSN or an unreachable server only surfaces on
	// first use. Ping here to fail with a connection error rather than one
	// blamed on a migration.
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("connecting to %s: %w", driver, err)
	}

	runner := migrate.New(db, os.DirFS(dir))

	switch cmd := args[0]; cmd {
	case "up":
		return runner.Up(ctx)

	case "down":
		steps := 1
		if len(args) > 1 {
			n, err := strconv.Atoi(args[1])
			if err != nil || n < 1 {
				return fmt.Errorf("invalid step count %q: expected a positive integer", args[1])
			}
			steps = n
		}
		return runner.Down(ctx, steps)

	case "status":
		entries, err := runner.Status(ctx)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			fmt.Printf("no migrations found in %s\n", dir)
			return nil
		}
		for _, e := range entries {
			mark := "pending"
			if e.Applied {
				mark = "applied"
				if e.AppliedAt != nil {
					mark += " " + e.AppliedAt.Format(time.RFC3339)
				}
			}
			// A checksum mismatch means the .sql file changed after it was
			// applied, so the schema no longer matches what is on disk.
			// Re-running will not fix it: the row is already recorded.
			if e.ChecksumMismatch {
				mark += "  !! checksum mismatch â€” file edited after apply"
			}
			fmt.Printf("  %04d  %-40s %s\n", e.Version, e.Name, mark)
		}
		return nil

	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
`
	out := strings.ReplaceAll(tmpl, "__DRIVER_IMPORT__", driverImport)
	return strings.ReplaceAll(out, "__DRIVER_NAME__", driverName)
}
