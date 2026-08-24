package main

// Helpers shared by the `breeze generate` stub writers.
//
// Every generator here follows the same shape: build source into a
// strings.Builder, hand it to writeGeneratedGoFile, and — where the stub needs
// wiring — upsert a marker block into one of the two generated files. Keeping
// that in one place means a new generator is a template plus a flag set.
//
// A caution that applies to all of them: format.Source validates syntax, not
// semantics. A stub that calls a function which does not exist formats
// perfectly and fails at `go build` in the user's project. The generated code
// here is written against APIs verified in the framework source, and the
// end-to-end test in generate_stubs_test.go is what keeps it that way.

import (
	"fmt"
	"go/format"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// writeGeneratedGoFile formats src and writes it to path, creating the parent
// directory. An existing file is left alone unless force is set: these are
// stubs the user is expected to fill in, so silently replacing one would
// discard their implementation.
func writeGeneratedGoFile(path, src string, force bool) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	if _, err := os.Stat(path); err == nil && !force {
		return fmt.Errorf("%s already exists — pass --force to overwrite", path)
	}

	formatted, err := format.Source([]byte(src))
	if err != nil {
		return fmt.Errorf("formatting %s: %w", path, err)
	}
	if err := os.WriteFile(path, formatted, 0o644); err != nil {
		return err
	}

	fmt.Printf("Created %s\n", path)
	return nil
}

// writeGeneratedTextFile is the same for non-Go output (HTML views, SQL
// migrations), which must not go through gofmt.
func writeGeneratedTextFile(path, content string, force bool) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	if _, err := os.Stat(path); err == nil && !force {
		return fmt.Errorf("%s already exists — pass --force to overwrite", path)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return err
	}

	fmt.Printf("Created %s\n", path)
	return nil
}

// upsertGeneratedFeature writes a block into features_generated.go and rebuilds
// the dispatcher's call list.
//
// `generate` writing into `add`'s file looks like a layering violation but is
// the only place the wiring can go: app.WebSocket and the job starters need the
// *breeze.Breeze, and RegisterGeneratedRoutes only receives the router. The
// block name is namespaced (wsChat, jobCleanupSessions) so it can never collide
// with a feature name, and rebuildFeatureCalls sorts unrecognised names last,
// which is where route and goroutine registration belongs anyway.
func upsertGeneratedFeature(name, body string, imports []string) error {
	if err := upsertBlock(blockRequest{
		FileName:  featuresFileName,
		Initial:   featuresTemplate(),
		Prefix:    featureMarkerPrefix,
		Name:      name,
		Body:      body,
		Imports:   imports,
		Placement: placeAtEOF,
	}); err != nil {
		return err
	}
	if err := rebuildFeatureCalls(); err != nil {
		return err
	}
	fmt.Printf("Wired %s into %s\n", name, featuresFileName)
	return nil
}

// lowerFirst is the unexported counterpart of an exported name, used for
// generated helper functions that should not be part of a package's API.
func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// identFrom turns a free-form token — a workflow step name, a job label — into
// a Go identifier fragment. "charge-card" and "charge card" both become
// ChargeCard, so a step list can be typed the way it reads.
func identFrom(s string) string {
	var buf strings.Builder
	upper := true
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			if upper {
				buf.WriteString(strings.ToUpper(string(r)))
				upper = false
			} else {
				buf.WriteRune(r)
			}
		default:
			upper = true
		}
	}
	out := buf.String()
	// A leading digit is legal in the input but not in an identifier.
	if out != "" && out[0] >= '0' && out[0] <= '9' {
		out = "Step" + out
	}
	return out
}

// requireIdent validates a name that will become part of a Go identifier.
func requireIdent(kind, name string) error {
	if identFrom(name) == "" || !token.IsIdentifier(identFrom(name)) {
		return fmt.Errorf("invalid %s %q — it must contain at least one letter or digit", kind, name)
	}
	return nil
}

// fileSlug is the file name a generated stub for Name lands under: "UserCreated"
// becomes "user_created.go", matching the migration naming already in use.
func fileSlug(name string) string { return toSlug(name) }

// printNotes renders the follow-up instructions a generator wants to leave the
// user with, in the same shape `breeze add` uses.
func printNotes(notes []string) {
	if len(notes) == 0 {
		return
	}
	fmt.Println()
	for _, n := range notes {
		fmt.Printf("  • %s\n", n)
	}
}
