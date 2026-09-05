package generator

import (
	"flag"
	"fmt"
	"strings"
)

// generateEvent writes an event type into the project's own events package.
//
// The framework's events package is imported as `evt` here, not under its own
// name: the file lives in a package the user's project calls `events`, and
// `import "github.com/nelthaarion/breeze/events"` inside a package named
// `events` is legal but reads as a shadow and collides the moment the same file
// wants to name both. The alias makes every reference unambiguous.
//
// The emit/subscribe helpers take the bus as a parameter rather than reaching
// for main's EventBus var. Package events cannot import package main, so a
// parameter is the only direction the dependency can run.
func generateEvent(modulePath, name string, args []string) error {
	fs := flag.NewFlagSet("generate event", flag.ContinueOnError)
	force := fs.Bool("force", false, "overwrite an existing event file")
	out := registerOutputFlags(fs)

	flagArgs, positional := splitFlagsAndPositional(fs, args)
	if err := parseFlags(fs, flagArgs); err != nil {
		return err
	}

	fields, err := parseFieldsNoRules("event", positional)
	if err != nil {
		return err
	}

	target, err := out.target("events", fileSlug(name))
	if err != nil {
		return err
	}

	imports := []string{`evt "github.com/nelthaarion/breeze/events"`}
	if usesTime(fields) {
		imports = append(imports, timeImport)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "// %s is emitted when ... (describe the trigger).\n", name)
	fmt.Fprintf(&b, "type %s struct {\n", name)
	if len(fields) == 0 {
		b.WriteString("\t// TODO: add payload fields, or keep it empty as a pure signal.\n")
	}
	for _, f := range fields {
		fmt.Fprintf(&b, "\t%s %s `json:\"%s\"`\n", f.Name, f.Type, f.JSON)
	}
	b.WriteString("}\n\n")

	fmt.Fprintf(&b, "// Emit%s publishes the event on bus. It returns the error from the\n", name)
	b.WriteString("// bus rather than swallowing it: a full queue or a closed bus is a\n")
	b.WriteString("// real failure and the caller is the only one who knows whether it\n")
	b.WriteString("// should be fatal.\n")
	fmt.Fprintf(&b, "func Emit%s(bus *evt.Bus, e %s) error {\n", name, name)
	fmt.Fprintf(&b, "\treturn evt.EmitBus(bus, e)\n}\n\n")

	fmt.Fprintf(&b, "// On%s subscribes fn to %s. Cancel the returned subscription to\n", name, name)
	b.WriteString("// stop receiving events.\n")
	fmt.Fprintf(&b, "func On%s(bus *evt.Bus, fn func(ctx *evt.Context, e %s) error) *evt.Subscription[%s] {\n", name, name, name)
	fmt.Fprintf(&b, "\treturn evt.OnTypeBus[%s](bus, fn)\n}\n", name)

	if err := writeGeneratedGoFile(generatedFile{
		Target:     target,
		Owner:      generateOwner("event"),
		Imports:    imports,
		Body:       b.String(),
		ModulePath: modulePath,
		Force:      *force,
	}); err != nil {
		return err
	}

	notes := []string{
		fmt.Sprintf("Emit it:      events.Emit%s(EventBus, events.%s{...})", name, name),
		fmt.Sprintf("Subscribe:    breeze generate listener %s", name),
		fmt.Sprintf("Import as:    \"%s/events\"", modulePath),
	}
	if !hasBlock(featuresFileName, featureMarkerPrefix, "events") {
		notes = append(notes, "Run `breeze add events` â€” the EventBus this needs is declared by that block.")
	}
	printNotes(notes)
	return nil
}
