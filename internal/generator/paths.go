package generator

// paths.go — the one check every path-like flag goes through.
//
// # Why the generators need this at all
//
// The generators resolve paths relative to the working directory, which is what
// makes them reusable across commands. That is fine when a person is typing them: a
// developer who passes `--dir=../../etc` meant it, and it is their own machine.
//
// It stops being fine when an agent supplies the value. `breeze_generate` forwards
// its `flags` object to the generator's FlagSet verbatim — deliberately, so this
// package stays the authority on which flags a kind accepts — so
// `{"flags": {"dir": "../../../../etc"}}` reaches `--dir` and the generator writes
// there. internal/mcp confines the *working directory* the generator runs in; only
// this package can confine what the generator does with a path once it is there.
//
// # Why the rule is "at or below the working directory"
//
// Every one of these flags names a place inside the project: migrations/, locales/,
// public/, the routes registry file. None of them has a legitimate value outside the
// project root, so the check does not have to reason about intent — it only has to
// establish that the resolved path is still inside the tree the command is operating
// on.
//
// An absolute path is refused for the same reason rather than resolved-and-compared.
// An absolute path that happens to point inside the project is a value nobody types
// and no configuration file needs, so accepting it would add a second shape to
// validate for no benefit.

import (
	"fmt"
	"path/filepath"
	"strings"
)

// validatePathFlag checks that a path-like flag value stays inside the project.
//
// flag is the flag's name, for the message: someone told "invalid path" has to guess
// which of a command's several path arguments was refused.
//
// The empty string is allowed through: it means "the default", and every caller
// substitutes its own before using it.
func validatePathFlag(flag, value string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}

	if filepath.IsAbs(trimmed) || strings.HasPrefix(trimmed, `\`) || strings.HasPrefix(trimmed, "/") {
		return fmt.Errorf("--%s %q is an absolute path. This flag names a location inside the "+
			"project, so it must be relative to the project root", flag, value)
	}
	// A Windows volume-relative path ("C:foo") is neither absolute nor safely
	// relative: it resolves against that volume's own current directory, which is
	// not this process's working directory.
	if len(trimmed) >= 2 && trimmed[1] == ':' {
		return fmt.Errorf("--%s %q names a volume, which does not resolve against the project "+
			"root", flag, value)
	}

	// Both separators are normalised before cleaning, and that is load-bearing rather
	// than tidiness. filepath.Clean is per-platform: on Linux `\` is an ordinary
	// filename character, so `..\windows` is one atom that Clean leaves untouched and
	// the leading-".." test below never fires. The same value on Windows is a
	// traversal. A path flag is refused or accepted on what it means, not on which
	// machine the check happens to run — and the caller here may be an agent, which is
	// exactly the caller that would send the spelling the host does not police.
	normalised := strings.ReplaceAll(trimmed, `\`, "/")

	// Clean collapses "a/../b" to "b" and reduces any escape to a leading "..",
	// which is the whole test. A textual search for ".." would reject "..foo" and
	// miss "a/b/../../..".
	cleaned := filepath.Clean(filepath.FromSlash(normalised))
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return fmt.Errorf("--%s %q resolves outside the project (%s). This flag names a location "+
			"inside the project, and a path that escapes it is refused rather than followed",
			flag, value, cleaned)
	}
	return nil
}
