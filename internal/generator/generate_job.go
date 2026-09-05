package generator

import (
	"flag"
	"fmt"
	"strings"
)

// generateJob writes a ticker-driven background job plus the block that starts
// it.
//
// Status reporting goes through *dashboard.Collector, held as an optional field
// rather than reached for globally: package jobs cannot import package main
// where the collector lives, and a nil field is the no-dashboard case. The
// generated report method checks for nil, so the job runs identically with or
// without `breeze add dashboard`.
func generateJob(modulePath, name string, args []string) error {
	fs := flag.NewFlagSet("generate job", flag.ContinueOnError)
	every := fs.String("every", "1m", "run interval (e.g. --every=30s)")
	force := fs.Bool("force", false, "overwrite an existing job file")
	out := registerOutputFlags(fs)

	flagArgs, _ := splitFlagsAndPositional(fs, args)
	if err := parseFlags(fs, flagArgs); err != nil {
		return err
	}

	everyExpr, err := parseDurationFlag("--every", *every)
	if err != nil {
		return err
	}

	target, err := out.target("jobs", fileSlug(name))
	if err != nil {
		return err
	}

	jobName := toSlug(name)
	lower := lowerFirst(name)
	hasDashboard := hasBlock(featuresFileName, featureMarkerPrefix, "dashboard")

	var b strings.Builder
	fmt.Fprintf(&b, "// %sName identifies the job in the dashboard's scheduler panel.\n", name)
	fmt.Fprintf(&b, "const %sName = %q\n\n", name, jobName)

	fmt.Fprintf(&b, "// %sInterval is how often Run is called.\n", name)
	fmt.Fprintf(&b, "const %sInterval = %s\n\n", name, everyExpr)

	fmt.Fprintf(&b, "// %s is a periodic background job.\n", name)
	fmt.Fprintf(&b, "type %s struct {\n", name)
	b.WriteString("\t// Interval overrides how often Run is called. Zero uses the constant.\n")
	b.WriteString("\tInterval time.Duration\n\n")
	b.WriteString("\t// Reporter publishes run status to the developer dashboard. Nil is\n")
	b.WriteString("\t// valid and means \"do not report\" â€” the job still runs.\n")
	b.WriteString("\tReporter *dashboard.Collector\n\n")
	b.WriteString("\t// Counters are touched only by the Start goroutine.\n")
	b.WriteString("\truns  int64\n")
	b.WriteString("\tfails int64\n")
	b.WriteString("}\n\n")

	fmt.Fprintf(&b, "// New%s returns the job with its default interval.\n", name)
	fmt.Fprintf(&b, "func New%s() *%s {\n\treturn &%s{Interval: %sInterval}\n}\n\n", name, name, name, name)

	fmt.Fprintf(&b, "// Run performs one unit of work. Returning an error marks the run failed\n")
	b.WriteString("// and reports it, but does not stop the schedule.\n")
	fmt.Fprintf(&b, "func (j *%s) Run(ctx context.Context) error {\n", name)
	b.WriteString("\t// TODO: implement\n")
	b.WriteString("\treturn nil\n}\n\n")

	fmt.Fprintf(&b, "// Start runs the job on a ticker until ctx is cancelled. It blocks, so\n")
	b.WriteString("// call it in a goroutine.\n")
	fmt.Fprintf(&b, "func (j *%s) Start(ctx context.Context) {\n", name)
	fmt.Fprintf(&b, "\tif j.Interval <= 0 {\n\t\tj.Interval = %sInterval\n\t}\n\n", name)
	b.WriteString("\tticker := time.NewTicker(j.Interval)\n")
	b.WriteString("\tdefer ticker.Stop()\n\n")
	b.WriteString("\tj.report(\"idle\", 0, nil)\n\n")
	b.WriteString("\tfor {\n")
	b.WriteString("\t\tselect {\n")
	b.WriteString("\t\tcase <-ctx.Done():\n")
	fmt.Fprintf(&b, "\t\t\tlog.Printf(%q)\n", jobName+": stopped")
	b.WriteString("\t\t\treturn\n\n")
	b.WriteString("\t\tcase <-ticker.C:\n")
	b.WriteString("\t\t\tj.report(\"running\", 0, nil)\n")
	b.WriteString("\t\t\tstart := time.Now()\n")
	b.WriteString("\t\t\terr := j.Run(ctx)\n")
	b.WriteString("\t\t\telapsed := time.Since(start)\n")
	b.WriteString("\t\t\tj.runs++\n\n")
	b.WriteString("\t\t\tif err != nil {\n")
	b.WriteString("\t\t\t\tj.fails++\n")
	fmt.Fprintf(&b, "\t\t\t\tlog.Printf(%q, err)\n", jobName+": %v")
	b.WriteString("\t\t\t\tj.report(\"failed\", elapsed, err)\n")
	b.WriteString("\t\t\t\tcontinue\n")
	b.WriteString("\t\t\t}\n\n")
	b.WriteString("\t\t\tj.report(\"idle\", elapsed, nil)\n")
	b.WriteString("\t\t}\n")
	b.WriteString("\t}\n}\n\n")

	// report: RegisterTask keys by Name and replaces, so it is both register
	// and update â€” there is no separate UpdateTask to call.
	b.WriteString("// report publishes status to the dashboard when one is attached.\n")
	fmt.Fprintf(&b, "func (j *%s) report(status string, elapsed time.Duration, runErr error) {\n", name)
	b.WriteString("\tif j.Reporter == nil {\n\t\treturn\n\t}\n\n")
	b.WriteString("\tnow := time.Now()\n")
	b.WriteString("\ttask := dashboard.SchedulerTask{\n")
	fmt.Fprintf(&b, "\t\tName:      %sName,\n", name)
	b.WriteString("\t\tCron:      \"every \" + j.Interval.String(),\n")
	b.WriteString("\t\tStatus:    status,\n")
	b.WriteString("\t\tNextRun:   now.Add(j.Interval),\n")
	b.WriteString("\t\tRunCount:  j.runs,\n")
	b.WriteString("\t\tFailCount: j.fails,\n")
	b.WriteString("\t}\n\n")
	b.WriteString("\t// Leave LastRun zero until there has actually been one, so the\n")
	b.WriteString("\t// dashboard does not show a run that never happened.\n")
	b.WriteString("\tif j.runs > 0 {\n")
	b.WriteString("\t\ttask.LastRun = now\n")
	b.WriteString("\t\ttask.LastRunMS = float64(elapsed.Microseconds()) / 1000\n")
	b.WriteString("\t}\n")
	b.WriteString("\tif runErr != nil {\n\t\ttask.LastError = runErr.Error()\n\t}\n\n")
	b.WriteString("\t// RegisterTask keys on Name and replaces, so it doubles as the update.\n")
	b.WriteString("\tj.Reporter.RegisterTask(task)\n}\n")

	if err := writeGeneratedGoFile(generatedFile{
		Target: target,
		Owner:  generateOwner("job"),
		Imports: []string{
			contextImport, logImport, timeImport,
			`"github.com/nelthaarion/breeze/v2/dashboard"`,
		},
		Body:       b.String(),
		ModulePath: modulePath,
		Force:      *force,
	}); err != nil {
		return err
	}

	// â”€â”€ the features block â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	var body strings.Builder
	fmt.Fprintf(&body, "// %sJob runs every %s, started by %s.\n", lower, *every, featureSetupFunc("job"+name))
	fmt.Fprintf(&body, "var %sJob = %s.New%s()\n\n", lower, target.Package, name)
	fmt.Fprintf(&body, "func %s(app *breeze.Breeze, router *breeze.Router) {\n", featureSetupFunc("job"+name))
	if hasDashboard {
		body.WriteString("\t// The dashboard block runs first, so the collector is already built.\n")
		body.WriteString("\t")
		body.WriteString(lower)
		body.WriteString("Job.Reporter = DashboardCollector\n\n")
	}
	body.WriteString("\t// Background: Start blocks on its ticker for the life of the process.\n")
	fmt.Fprintf(&body, "\tgo %sJob.Start(context.Background())\n", lower)
	body.WriteString("}\n")

	imports := []string{
		contextImport,
		`"github.com/nelthaarion/breeze/v2"`,
		// Aliased for the same reason `generate ws` aliases its own: the package
		// the generated file declares is target.Package, which --package may have
		// made something other than the directory name.
		fmt.Sprintf("%s %q", target.Package, modulePath+"/jobs"),
	}
	if err := upsertGeneratedFeature("job"+name, body.String(), imports); err != nil {
		return err
	}

	notes := []string{
		fmt.Sprintf("Implement:    %s.%s.Run in %s", target.Package, name, target.Path),
		fmt.Sprintf("Interval:     %s (change %sJob.Interval before it starts)", *every, lower),
	}
	if hasDashboard {
		notes = append(notes, "Reporting to the dashboard's scheduler panel at /dashboard.")
	} else {
		notes = append(notes, "Run `breeze add dashboard` to see run counts and failures in the scheduler panel.")
	}
	notes = append(notes,
		"Started with context.Background() â€” swap in a cancellable context to stop it on shutdown.")
	printNotes(notes)
	return nil
}
