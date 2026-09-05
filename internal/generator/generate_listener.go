package generator

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// generateListener writes a subscriber for an event produced by
// `breeze generate event`.
//
// It lands in its own `listeners` package rather than alongside the event: a
// listener is where application logic goes, and keeping it out of the events
// package means the events package stays a leaf that anything can import
// without picking up a dependency cycle.
func generateListener(modulePath, name string, args []string) error {
	fs := flag.NewFlagSet("generate listener", flag.ContinueOnError)
	nameOverride := fs.String(
		"name",
		"",
		"listener name (default On<Event>, e.g. --name=SendWelcomeEmail)",
	)
	force := fs.Bool("force", false, "overwrite an existing listener file")
	out := registerOutputFlags(fs)

	flagArgs, _ := splitFlagsAndPositional(fs, args)
	if err := parseFlags(fs, flagArgs); err != nil {
		return err
	}

	event := name
	listener := *nameOverride
	if listener == "" {
		listener = "On" + event
	} else {
		listener = identFrom(listener)
		if err := requireIdent("--name", *nameOverride); err != nil {
			return err
		}
	}

	target, err := out.target("listeners", fileSlug(listener))
	if err != nil {
		return err
	}

	imports := []string{
		logImport,
		`evt "github.com/nelthaarion/breeze/v2/events"`,
		fmt.Sprintf("%q", modulePath+"/events"),
	}

	var b strings.Builder
	fmt.Fprintf(&b, "// %s handles the %s event.\n//\n", listener, event)
	b.WriteString("// Returning an error marks the delivery as failed; whether that is retried\n")
	b.WriteString("// depends on the bus configuration. Do not block here for long â€” a\n")
	b.WriteString("// synchronous bus runs this on the emitting goroutine.\n")
	fmt.Fprintf(&b, "func %s(ctx *evt.Context, e events.%s) error {\n", listener, event)
	fmt.Fprintf(&b, "\tlog.Printf(\"%s: %%+v\", e)\n", event)
	b.WriteString("\t// TODO: implement\n")
	b.WriteString("\treturn nil\n}\n\n")

	fmt.Fprintf(
		&b,
		"// Register%s subscribes %s to bus. Call it once during startup.\n",
		listener,
		listener,
	)
	fmt.Fprintf(&b, "func Register%s(bus *evt.Bus) {\n", listener)
	fmt.Fprintf(&b, "\tevents.On%s(bus, %s)\n}\n", event, listener)

	if err := writeGeneratedGoFile(generatedFile{
		Target:     target,
		Owner:      generateOwner("listener"),
		Imports:    imports,
		Body:       b.String(),
		ModulePath: modulePath,
		Force:      *force,
	}); err != nil {
		return err
	}

	notes := []string{
		fmt.Sprintf("Register it in main: listeners.Register%s(EventBus)", listener),
	}
	// Without the event type this file does not compile, and the error the user
	// would get points at the listener rather than the missing declaration.
	eventFile := filepath.Join("events", fileSlug(event)+".go")
	if _, err := os.Stat(eventFile); err != nil {
		notes = append(
			notes,
			fmt.Sprintf(
				"%s does not exist yet â€” run `breeze generate event %s` or this will not compile.",
				eventFile,
				event,
			),
		)
	}
	if !hasBlock(featuresFileName, featureMarkerPrefix, "events") {
		notes = append(
			notes,
			"Run `breeze add events` â€” the EventBus this needs is declared by that block.",
		)
	}
	printNotes(notes)
	return nil
}
