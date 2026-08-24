package main

// Marker-block code generation, shared by every generated file the CLI
// manages: routes_generated.go (written by `breeze generate`) and
// features_generated.go (written by `breeze add`).
//
// A managed file is ordinary Go source carrying paired marker comments:
//
//	// breeze:feature:dashboard:start
//	...generated code...
//	// breeze:feature:dashboard:end
//
// Re-running a generator replaces only the text between its own markers, so
// blocks for other names — and any hand-written code outside the markers —
// survive untouched. That is what makes `breeze add` idempotent and safe to
// run against a project that has already been edited.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/format"
	"os"
	"regexp"
	"strings"
)

// blockPlacement decides where a block goes when the target file does not
// already contain one under that name. Replacement of an existing block
// always happens in place, so placement only matters the first time.
type blockPlacement int

const (
	// placeInLastBrace inserts just before the file's final closing brace.
	// Used by routes_generated.go, where every block is a run of statements
	// inside RegisterGeneratedRoutes and that function is the last
	// declaration in the file.
	placeInLastBrace blockPlacement = iota

	// placeAtEOF appends at package scope. Used by features_generated.go,
	// where a block declares its own setup function and any package-level
	// vars (the event bus, the workflow engine, the dashboard collector)
	// that other features and the user's own code reference.
	placeAtEOF

	// placeReplaceOnly refuses to insert: the block must already exist. Used
	// for the dispatcher's call list, which the file template always
	// contains — if it is missing, the file has been damaged and silently
	// appending a second copy would be worse than reporting it.
	placeReplaceOnly
)

// blockRequest is one upsert against a marker-managed file.
type blockRequest struct {
	FileName  string   // e.g. "features_generated.go"
	Initial   string   // full file content to create when FileName is absent
	Prefix    string   // marker prefix, e.g. "breeze:feature"
	Name      string   // block name, e.g. "dashboard"
	Body      string   // generated Go source for the block, without markers
	Imports   []string // import lines to ensure are present
	Placement blockPlacement
	// Stamp records a checksum of Body on the start marker, so a later run can
	// tell a block it wrote itself from one that has been edited since.
	//
	// Only `breeze add` sets this, because only add refuses to overwrite: the
	// route blocks and the dispatcher's call list are rewritten unconditionally,
	// and a checksum there would advertise a protection that does not exist.
	Stamp bool
}

// markersFor builds the start/end comment pair for a block.
func markersFor(prefix, name string) (start, end string) {
	return fmt.Sprintf("// %s:%s:start", prefix, name),
		fmt.Sprintf("// %s:%s:end", prefix, name)
}

// blockPattern matches a whole marker block, markers included. (?s) lets .
// span newlines and the lazy quantifier stops at this block's own end marker
// rather than running on to a later one.
func blockPattern(start, end string) *regexp.Regexp {
	return regexp.MustCompile(regexp.QuoteMeta(start) + `(?s).*?` + regexp.QuoteMeta(end))
}

// upsertBlock inserts or replaces req's marker block, ensures its imports,
// gofmts the result, and writes it back. The file is created from req.Initial
// when it does not exist.
func upsertBlock(req blockRequest) error {
	src, err := os.ReadFile(req.FileName)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		src = []byte(req.Initial)
	}

	start, end := markersFor(req.Prefix, req.Name)
	startLine := start
	if req.Stamp {
		startLine += " " + stampFor(req.Body)
	}
	block := startLine + "\n" + strings.TrimRight(req.Body, "\n") + "\n" + end

	content := ensureImports(string(src), req.Imports)
	// Matched on the unstamped start marker, which is a prefix of the stamped
	// one — so a block written with a checksum is still found, and replacing it
	// swaps in the new checksum along with the new body.
	re := blockPattern(start, end)

	switch {
	case re.MatchString(content):
		// ReplaceAllLiteralString, not ReplaceAllString: generated bodies
		// contain "$" in struct tags and printf verbs, which the expansion
		// form would eat.
		content = re.ReplaceAllLiteralString(content, block)

	case req.Placement == placeReplaceOnly:
		return fmt.Errorf("%s is missing its %s:%s block — delete the file and re-run to regenerate it",
			req.FileName, req.Prefix, req.Name)

	case req.Placement == placeAtEOF:
		content = strings.TrimRight(content, "\n") + "\n\n" + block + "\n"

	default: // placeInLastBrace
		braceIdx := strings.LastIndex(content, "}")
		if braceIdx == -1 {
			return fmt.Errorf("%s is malformed: no closing brace to insert into", req.FileName)
		}
		indented := "\t" + strings.ReplaceAll(block, "\n", "\n\t") + "\n"
		content = content[:braceIdx] + indented + content[braceIdx:]
	}

	formatted, err := format.Source([]byte(content))
	if err != nil {
		return fmt.Errorf("formatting %s: %w", req.FileName, err)
	}

	return os.WriteFile(req.FileName, formatted, 0o644)
}

// hasBlock reports whether fileName already contains a block for name. Used
// by features that compose: `add workflow` checks for the events block so it
// can share that bus instead of standing up a second one.
func hasBlock(fileName, prefix, name string) bool {
	_, ok := readBlock(fileName, prefix, name)
	return ok
}

// blockStampPrefix labels the checksum on a stamped start marker.
const blockStampPrefix = "hash="

// stampFor renders the marker suffix recording what a block was generated as.
//
// The checksum covers the gofmt-canonical body rather than the raw one, so it
// still matches after the file has been through format.Source — both as part of
// upsertBlock's own write and later, when the user runs gofmt over the project.
//
// Twelve hex digits is 48 bits, which is generous for what this detects: not a
// deliberate forgery, just whether a human has touched the block since it was
// written.
func stampFor(body string) string {
	sum := sha256.Sum256([]byte(canonicalGoSource(body)))
	return blockStampPrefix + hex.EncodeToString(sum[:])[:12]
}

// splitBlock separates what readBlock returned into the body between the
// markers and the checksum carried on the start marker, which is empty for a
// block written without one or edited to drop it.
func splitBlock(existing, start, end string) (body, stamp string) {
	rest := strings.TrimPrefix(existing, start)
	// Everything left on the start marker's own line is the stamp.
	firstLine, remainder, found := strings.Cut(rest, "\n")
	if !found {
		return "", ""
	}
	if trimmed := strings.TrimSpace(firstLine); strings.HasPrefix(trimmed, blockStampPrefix) {
		stamp = trimmed
	}
	return strings.TrimSuffix(remainder, end), stamp
}

// blockIsPristine reports whether a block still contains exactly what was
// generated into it.
//
// This is what lets `breeze add` honour its own promise that --force is only
// needed for hand edits. Without it, add cannot tell "this block was generated
// before `add events` existed, so it wires to the wrong bus" from "the user
// rewrote this block" — both are just "differs from what I would write now" —
// and so it had to refuse both.
func blockIsPristine(body, stamp string) bool {
	return stamp != "" && stamp == stampFor(body)
}

// sameBlockBody reports whether a block already on disk says the same thing as
// body, ignoring formatting.
//
// The comparison has to be canonical rather than byte-for-byte. What lands in
// the file has been through format.Source as part of the whole file, and gofmt
// puts a blank line between a declaration and a comment that follows it — so
// the stored block ends `}\n\n// breeze:feature:x:end` while a freshly built
// one ends `}\n// breeze:feature:x:end`. Comparing raw text made every block
// look modified the moment it was written, which turned every second
// `breeze add` into an error demanding --force for a block nobody had touched.
func sameBlockBody(stored, body string) bool {
	return canonicalGoSource(stored) == canonicalGoSource(body)
}

// canonicalGoSource reduces a list of declarations to one spelling, so two
// bodies that differ only in layout compare equal.
//
// The leading trim is what makes it independent of indentation. format.Source
// indents its output to match the first line of code it is handed, so a body
// arriving with a base indent would canonicalise differently from the same body
// at column 0 — and Go's parser ignores indentation entirely, so trimming first
// costs nothing and gofmt re-indents the rest from the syntax tree regardless.
//
// A body that does not parse falls back to its trimmed self: reporting that is
// upsertBlock's job, where the file name is in hand, not this comparison's.
func canonicalGoSource(src string) string {
	src = strings.TrimSpace(src)
	if formatted, err := format.Source([]byte(src)); err == nil {
		return strings.TrimSpace(string(formatted))
	}
	return src
}

// readBlock returns the full text of a block, markers included, and whether it
// was found.
//
// `breeze add` compares this against what it is about to write: identical means
// a re-run with the same flags, which should be a silent no-op, while different
// means either new flags or a hand edit inside the markers — and the second of
// those is worth refusing to clobber without --force.
func readBlock(fileName, prefix, name string) (string, bool) {
	src, err := os.ReadFile(fileName)
	if err != nil {
		return "", false
	}
	start, end := markersFor(prefix, name)
	match := blockPattern(start, end).Find(src)
	if match == nil {
		return "", false
	}
	return string(match), true
}

// listBlocks returns the names of every block in fileName carrying prefix, in
// the order they appear.
func listBlocks(fileName, prefix string) []string {
	src, err := os.ReadFile(fileName)
	if err != nil {
		return nil
	}
	re := regexp.MustCompile(`// ` + regexp.QuoteMeta(prefix) + `:([A-Za-z0-9_-]+):start`)
	matches := re.FindAllStringSubmatch(string(src), -1)
	names := make([]string, 0, len(matches))
	for _, m := range matches {
		names = append(names, m[1])
	}
	return names
}
