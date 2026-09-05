package generator

// Helpers shared by the `breeze generate` stub writers.
//
// Every generator here follows the same shape: build source into a
// strings.Builder, hand it to writeGeneratedGoFile, and â€” where the stub needs
// wiring â€” upsert a marker block into one of the two generated files. Keeping
// that in one place means a new generator is a template plus a flag set.
//
// A caution that applies to all of them: format.Source validates syntax, not
// semantics. A stub that calls a function which does not exist formats
// perfectly and fails at `go build` in the user's project. The generated code
// here is written against APIs verified in the framework source, and the
// end-to-end test in generate_stubs_test.go is what keeps it that way.

import (
	"fmt"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// generatedFile is one standalone Go file a generator writes.
//
// It is a struct rather than a parameter list because the fields are not
// independent: Target's package clause has to be the one written into the file,
// Owner has to be the command a later run will claim the file back under, and
// ModulePath decides how Imports are grouped. Passing them separately made it
// possible to resolve a target and then write a different package, which is the
// mistake this shape rules out.
type generatedFile struct {
	// Target is where the file goes and what package it declares, from
	// outputFlags.target — so a default-derived destination and an overridden
	// one arrive here identically.
	Target outputTarget
	// Owner is the command that generates this file, from generateOwner. It is
	// written into the header and is what a later run reads back to tell its own
	// file from another feature's.
	Owner string
	// Imports are candidate import lines, in any order and any grouping: the
	// writer groups them and drops the ones Body does not use. A generator may
	// list an import its own conditionals might not end up needing.
	Imports []string
	// Body is everything after the imports.
	Body string
	// ModulePath is the generated project's module path, which is what
	// distinguishes its own packages from third-party ones.
	ModulePath string
	Force      bool
}

// writeGeneratedGoFile writes one generated Go file, creating its directory.
//
// Everything the clean-code guarantees promise happens here, which is what makes
// them generator-wide rather than per-template: the header that records
// ownership, the grouped imports, the pruning of unused ones, gofmt, and the
// package-name check. A generator gets all of it by calling this and cannot opt
// out by forgetting a step.
//
// An existing file is left alone unless Force is set, and a file another
// generator owns is refused regardless — see checkFileOwnership.
func writeGeneratedGoFile(f generatedFile) error {
	path := f.Target.Path
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if err := checkFileOwnership(path, f.Owner, f.Force); err != nil {
		return err
	}

	var b strings.Builder
	b.WriteString(generatedHeader(f.Owner))
	fmt.Fprintf(&b, "package %s\n\n", f.Target.Package)
	if len(f.Imports) > 0 {
		// Written factored and unsorted; canonicalGoFile regroups and sorts it,
		// so the order a generator lists its imports in does not matter.
		b.WriteString("import (\n")
		for _, line := range f.Imports {
			if line = strings.TrimSpace(line); line != "" {
				b.WriteString("\t")
				b.WriteString(line)
				b.WriteString("\n")
			}
		}
		b.WriteString(")\n\n")
	}
	b.WriteString(f.Body)

	content, err := canonicalGoFile(path, b.String(), f.ModulePath)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
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
		return fmt.Errorf("%s already exists â€” pass --force to overwrite", path)
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

// identFrom turns a free-form token â€” a workflow step name, a job label â€” into
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
		return fmt.Errorf(
			"invalid %s %q â€” it must contain at least one letter or digit",
			kind,
			name,
		)
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
		fmt.Printf("  â€¢ %s\n", n)
	}
}
