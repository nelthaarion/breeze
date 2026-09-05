// Command linkcheck verifies that every relative Markdown link in a directory
// tree resolves to a file that exists.
//
// It exists because the reorganisation this repository just went through moves
// documents, and a moved document leaves behind links that still look fine in
// review. A human reading a diff cannot tell that `](./events/README.md)` is
// still correct; this can.
//
// Usage:
//
//	go run ./internal/tools/linkcheck .
//
// Exit status is 1 if any link is broken, so CI fails on it.
//
// # Why fenced code blocks are skipped
//
// Go generics look exactly like Markdown links:
//
//	events.Inspect[UserCreated](bus)
//	workflow.Payload[Order](ctx)
//
// `[UserCreated](bus)` is a well-formed link to a file named "bus". Four such
// false positives exist in events/README.md and workflow/README.md alone. A
// checker that reports them gets ignored, which is worse than no checker, so
// this one tracks fence state and skips fenced content entirely. Inline code
// spans are stripped for the same reason.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// linkPattern captures the target of a Markdown inline link: the text between
// "](" and the closing paren, with no whitespace or paren inside it.
var linkPattern = regexp.MustCompile(`\]\(([^)\s]+)\)`)

// codeSpan matches an inline `code span`, which is replaced before link
// extraction so that code samples cannot produce findings.
var codeSpan = regexp.MustCompile("`[^`]*`")

// fenceStart matches an opening or closing fence, ``` or ~~~, with optional
// leading whitespace and an optional language tag.
var fenceStart = regexp.MustCompile("^\\s*(```|~~~)")

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}

	broken, checked, err := run(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "linkcheck: %v\n", err)
		os.Exit(2)
	}

	for _, b := range broken {
		fmt.Printf("BROKEN %s:%d -> %s\n", b.File, b.Line, b.Target)
	}
	fmt.Printf("linkcheck: %d internal links checked, %d broken\n", checked, len(broken))
	if len(broken) > 0 {
		os.Exit(1)
	}
}

// finding is one unresolvable link.
type finding struct {
	File   string
	Line   int
	Target string
}

// run walks root for Markdown files and returns every broken link, plus the
// number of internal links examined.
func run(root string) (broken []finding, checked int, err error) {
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skipped wholesale: .git holds packed objects, node_modules holds
			// third-party docs this repository does not own, and .migration is
			// gitignored one-shot tooling.
			switch d.Name() {
			case ".git", "node_modules", ".migration":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.EqualFold(filepath.Ext(path), ".md") {
			return nil
		}

		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		found, n := checkFile(path, string(body))
		broken = append(broken, found...)
		checked += n
		return nil
	})
	return broken, checked, err
}

// checkFile reports the broken links in one file's content, and how many
// internal links it examined. Relative targets resolve against the containing
// file's directory, which is how Markdown renderers resolve them.
func checkFile(path, content string) (broken []finding, checked int) {
	dir := filepath.Dir(path)
	inFence := false

	for i, raw := range strings.Split(content, "\n") {
		line := strings.TrimSuffix(raw, "\r")

		if fenceStart.MatchString(line) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}

		for _, m := range linkPattern.FindAllStringSubmatch(codeSpan.ReplaceAllString(line, "CODE"), -1) {
			target := m[1]
			if isExternal(target) {
				continue
			}
			// Strip a fragment: a link may point at a heading inside a file,
			// and only the file part is checkable here.
			file := target
			if idx := strings.IndexByte(file, '#'); idx >= 0 {
				file = file[:idx]
			}
			if file == "" {
				continue // pure #anchor, same document
			}
			checked++
			if _, err := os.Stat(filepath.Join(dir, file)); err != nil {
				broken = append(broken, finding{File: path, Line: i + 1, Target: target})
			}
		}
	}
	return broken, checked
}

// isExternal reports whether a link target points outside the repository and so
// cannot be resolved against the filesystem.
func isExternal(target string) bool {
	switch {
	case strings.HasPrefix(target, "http://"),
		strings.HasPrefix(target, "https://"),
		strings.HasPrefix(target, "mailto:"),
		strings.HasPrefix(target, "#"):
		return true
	}
	return false
}
