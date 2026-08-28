package generator

// The `breeze add` command: wire an existing framework feature into the
// project.
//
// The division of labour against `generate` is that generate writes code you
// are expected to edit â€” a handler, a workflow definition, an event struct â€”
// while add wires up something the framework already implements. That is why
// add's output lives in one generated file under markers and generate's does
// not.

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func runAdd(args []string) error {
	if len(args) == 0 {
		printFeatureList(os.Stdout)
		return fmt.Errorf("no feature given")
	}

	switch args[0] {
	case "--list", "-list", "list":
		printFeatureList(os.Stdout)
		return nil
	case "-h", "--help", "help":
		printAddHelp(os.Stdout)
		return nil
	}

	name := resolveFeatureName(strings.ToLower(args[0]))
	f, ok := features[name]
	if !ok {
		return fmt.Errorf("unknown feature %q%s\n\nRun `breeze add --list` to see all %d features",
			args[0], suggestFeature(name), len(features))
	}

	fs := flag.NewFlagSet("add "+name, flag.ContinueOnError)
	force := fs.Bool("force", false, "overwrite an existing block or file that differs from what would be generated")
	generate := f.Build(fs)

	flagArgs, positional := splitFlagsAndPositional(fs, args[1:])
	if len(positional) > 0 {
		return fmt.Errorf("`breeze add %s` takes no positional arguments, got %q â€” did you mean --%s?",
			name, positional[0], positional[0])
	}
	if err := parseFlags(fs, flagArgs); err != nil {
		return err
	}

	modulePath, err := currentModulePath()
	if err != nil {
		return err
	}

	out, err := generate(featureCtx{
		ModulePath: modulePath,
		// Probed rather than remembered: the file on disk is the only record
		// of what has been added, which is what lets add stay stateless.
		HasEvents:        hasBlock(featuresFileName, featureMarkerPrefix, "events"),
		HasObservability: hasBlock(featuresFileName, featureMarkerPrefix, "observability"),
		HasDashboard:     hasBlock(featuresFileName, featureMarkerPrefix, "dashboard"),
	})

	if err != nil {
		return err
	}

	written, skipped, err := writeFeatureFiles(out, *force)
	if err != nil {
		return err
	}

	result := blockCreated
	if !f.Standalone {
		result, err = applyFeatureBlock(f, out, *force)
		if err != nil {
			return err
		}
		if err := rebuildFeatureCalls(); err != nil {
			return err
		}
	}

	report(f, out, result, written, skipped)
	return nil
}

// blockResult is what applyFeatureBlock did, which decides both what is
// reported and whether dependent features need regenerating.
type blockResult int

const (
	// blockUnchanged: the block was already exactly what would be generated.
	blockUnchanged blockResult = iota
	// blockCreated: there was no block for this feature before.
	blockCreated
	// blockUpdated: a block existed and was replaced, because the flags or the
	// surrounding project changed what the feature generates.
	blockUpdated
)

// applyFeatureBlock writes the feature's block, reporting what it did.
//
// An unchanged re-run is a no-op rather than an error: `breeze add cors` twice
// should be as harmless as running it once. A block that differs splits two
// ways, and telling them apart is the whole reason blocks carry a checksum:
//
//   - Still pristine â€” the flags changed, or a sibling feature appeared and this
//     feature now composes with it. Replacing is exactly what the user asked
//     for, so it happens without ceremony.
//   - Edited since it was generated â€” refuse without --force. Discarding
//     someone's edit is the one outcome worth a confirmation step.
//
// Before the checksum both cases looked identical, so every legitimate change
// demanded --force under a warning about losing edits that did not exist.
func applyFeatureBlock(f *feature, out featureOutput, force bool) (blockResult, error) {
	start, end := markersFor(featureMarkerPrefix, f.Name)

	result := blockCreated
	if existing, ok := readBlock(featuresFileName, featureMarkerPrefix, f.Name); ok {
		stored, stamp := splitBlock(existing, start, end)
		switch {
		case sameBlockBody(stored, out.Body):
			return blockUnchanged, nil
		case blockIsPristine(stored, stamp), force:
			result = blockUpdated
		default:
			return blockUnchanged, fmt.Errorf("the %s block in %s has been edited since it was generated, "+
				"and differs from what would be generated now\n"+
				"  re-run with --force to replace it, losing those edits",
				f.Name, featuresFileName)
		}
	}

	imports := append(append([]string{}, f.Imports...), out.Imports...)
	err := upsertBlock(blockRequest{
		FileName:  featuresFileName,
		Initial:   featuresTemplate(),
		Prefix:    featureMarkerPrefix,
		Name:      f.Name,
		Body:      out.Body,
		Imports:   imports,
		Placement: placeAtEOF,
		Stamp:     true,
	})
	return result, err
}

// writeFeatureFiles creates the feature's directories and side files. Existing
// files are left alone unless force is set: a locale catalog or a view template
// is a starting point the user is meant to edit, and re-running add must not
// undo that.
func writeFeatureFiles(out featureOutput, force bool) (written, skipped []string, err error) {
	for _, dir := range out.Dirs {
		if dir == "" {
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, nil, err
		}
	}

	// Sorted so the output is stable across runs â€” map iteration order is not.
	paths := make([]string, 0, len(out.Files))
	for path := range out.Files {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	for _, path := range paths {
		if dir := filepath.Dir(path); dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, nil, err
			}
		}
		if _, statErr := os.Stat(path); statErr == nil && !force {
			skipped = append(skipped, path)
			continue
		}
		if err := os.WriteFile(path, []byte(out.Files[path]), 0o644); err != nil {
			return nil, nil, err
		}
		written = append(written, path)
	}
	return written, skipped, nil
}

func report(f *feature, out featureOutput, result blockResult, written, skipped []string) {
	switch {
	case f.Standalone:
		fmt.Printf("Added %s\n", f.Name)
	case result == blockCreated:
		fmt.Printf("Added %s to %s\n", f.Name, featuresFileName)
	case result == blockUpdated:
		fmt.Printf("Updated %s in %s\n", f.Name, featuresFileName)
	default:
		fmt.Printf("%s is already up to date\n", f.Name)
	}

	for _, path := range written {
		fmt.Printf("  wrote %s\n", path)
	}
	for _, path := range skipped {
		fmt.Printf("  kept  %s (already exists â€” use --force to overwrite)\n", path)
	}

	for _, note := range out.Notes {
		fmt.Printf("  - %s\n", note)
	}

	if !f.Standalone {
		warnStaleDependents(f, result)
		warnIfDispatcherUncalled()
	}
}

// warnStaleDependents reports the features whose wiring this add just made
// out of date.
//
// Composition is one-directional: `add dashboard` consults whether an events
// block exists and bridges the bus onto the live Events page if it does. Add
// them the other way round and the dashboard block was generated before the bus
// existed, so the bridge is absent â€” the project compiles, the dashboard serves,
// and its Events page is permanently empty with nothing anywhere reporting why.
//
// The feature being added already warns about the ordering it can see ("no
// events block found, so..."), but that note is printed before the user has any
// reason to care about it. This is the other half: say so at the moment it
// becomes true, and name the command that fixes it.
func warnStaleDependents(added *feature, result blockResult) {
	// Only a brand-new block invalidates anything. A re-run that changed flags
	// does not move a dependent, and an unchanged one moves nothing at all.
	if result != blockCreated {
		return
	}

	var stale []string
	for _, name := range listBlocks(featuresFileName, featureMarkerPrefix) {
		f, ok := features[name]
		if !ok || name == added.Name {
			continue
		}
		for _, dep := range f.DependsOn {
			if dep == added.Name {
				stale = append(stale, name)
				break
			}
		}
	}
	if len(stale) == 0 {
		return
	}
	sort.Strings(stale)

	fmt.Printf("\n  ! %s was added after %s, which reads it at generation time.\n",
		added.Name, strings.Join(stale, " and "))
	for _, name := range stale {
		fmt.Printf("      breeze add %s\n", name)
	}
	fmt.Printf("    Re-run those to pick it up â€” the blocks are untouched, so no --force is needed.\n")
}

// warnIfDispatcherUncalled checks that something actually calls the generated
// dispatcher.
//
// Projects from `breeze new` already do, because the scaffold's main.go carries
// the call. A project that predates this command, or one assembled by hand,
// will not â€” and without it every add is silently inert: the code compiles,
// the setup functions exist, and none of them ever run. That failure gives no
// signal at all at runtime, so it is worth checking for.
func warnIfDispatcherUncalled() {
	entries, err := os.ReadDir(".")
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || e.Name() == featuresFileName {
			continue
		}
		src, err := os.ReadFile(e.Name())
		if err != nil {
			continue
		}
		if strings.Contains(string(src), "RegisterGeneratedFeatures(") {
			return
		}
	}

	fmt.Printf(`
  ! Nothing calls RegisterGeneratedFeatures, so none of this runs yet.
    Add it to main.go after breeze.New and before app.Run:

        app := breeze.New(router, pool)
        RegisterGeneratedFeatures(app, router)
        app.Run(3000, true)
`)
}

// suggestFeature offers the closest registered name for a typo, so `add
// dashbord` points at `dashboard` instead of just listing everything.
// featureAliases maps names people actually type to the canonical feature.
//
// This solves a different problem from the Levenshtein suggestion below. That
// one catches typos; this one catches knowing a subsystem by a name the
// registry does not use. `tuning` is the case that motivated it: the framework
// file is `workerpool.go`, so `breeze add workerpool` is the obvious command,
// but the feature is called `tuning` because it also covers zero-copy headers
// â€” and no edit distance connects those two words.
//
// Aliases only ever point at a real feature, and the resolution is announced,
// so the canonical name is what the user learns for next time.
var featureAliases = map[string]string{
	"workerpool":   "tuning",
	"pool":         "tuning",
	"performance":  "tuning",
	"perf":         "tuning",
	"eventbus":     "events",
	"bus":          "events",
	"metrics":      "observability",
	"prometheus":   "observability",
	"openapi":      "docs",
	"swagger":      "docs",
	"scalar":       "docs",
	"ws":           "websocket",
	"gzip":         "compression",
	"headers":      "security",
	"views":        "templates",
	"html":         "templates",
	"migrations":   "migrator",
	"migrate":      "migrator",
	"locale":       "i18n",
	"translations": "i18n",
	"streaming":    "video",
	"cache":        "etag",
	"panics":       "recovery",

	// Hyphenated spellings. The registry uses single words, but a hyphen is
	// the natural way to write a two-word name in a YAML file, and the
	// configuration schema takes these names as data rather than as something
	// the user was prompted for. Accepting both spellings costs one line each
	// and removes a class of "not a known feature" errors that teach nothing.
	"rate-limit":  "ratelimit",
	"rate_limit":  "ratelimit",
	"worker-pool": "tuning",
	"event-bus":   "events",
	"web-socket":  "websocket",
	"open-api":    "docs",
}

// canonicalFeatureName maps an alias to its canonical feature without saying
// so, for callers that are inspecting rather than acting.
//
// Validation is the caller that needs this: it resolves every configured
// middleware name to check it exists, before anything is generated. Announcing
// each alias there would print notes for a pass that may end in an error and
// generate nothing at all.
func canonicalFeatureName(name string) string {
	if canonical, ok := featureAliases[name]; ok {
		return canonical
	}
	return name
}

// resolveFeatureName maps an alias to its canonical feature, leaving anything
// else â€” real names and genuine mistakes alike â€” untouched for the caller to
// report.
//
// Deliberately not silent: an alias that quietly worked would leave the user
// with a name that does not appear in `add --list`, in the block markers, or in
// the file they are about to read.
func resolveFeatureName(name string) string {
	canonical := canonicalFeatureName(name)
	if canonical != name {
		fmt.Fprintf(os.Stderr, "note: %q is an alias for the %q feature\n", name, canonical)
	}
	return canonical
}

// featureCandidate is a name the user might have typed, paired with the
// feature it would resolve to.
type featureCandidate struct {
	typed     string
	canonical string
}

// suggestionCandidates is every name worth matching a mistake against â€”
// canonical features plus their aliases â€” sorted so a tie between two
// equally-distant candidates resolves the same way on every run rather than
// following Go's randomised map order.
func suggestionCandidates() []featureCandidate {
	out := make([]featureCandidate, 0, len(features)+len(featureAliases))
	for name := range features {
		out = append(out, featureCandidate{typed: name, canonical: name})
	}
	for alias, canonical := range featureAliases {
		out = append(out, featureCandidate{typed: alias, canonical: canonical})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].typed < out[j].typed })
	return out
}

func suggestFeature(name string) string {
	best, bestDist := "", 1<<30
	for _, c := range suggestionCandidates() {
		if d := editDistance(name, c.typed); d < bestDist {
			best, bestDist = c.canonical, d
		}
	}
	// Two edits is the useful cutoff: it catches transpositions and single
	// typos without proposing "etag" for an unrelated word.
	if best == "" || bestDist > 2 {
		return ""
	}
	return fmt.Sprintf(" â€” did you mean %q?", best)
}

// editDistance is Levenshtein distance over two rows.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min(prev[j]+1, min(curr[j-1]+1, prev[j-1]+cost))
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}

func printFeatureList(w io.Writer) {
	fmt.Fprintf(w, "Features (%d), in the order they are wired:\n\n", len(features))

	installed := map[string]bool{}
	for _, n := range listBlocks(featuresFileName, featureMarkerPrefix) {
		installed[n] = true
	}

	for _, name := range featureNames() {
		f := features[name]
		mark := " "
		if installed[name] {
			mark = "*"
		}
		kind := ""
		if f.Standalone {
			kind = " (standalone)"
		}
		fmt.Fprintf(w, "  %s %-14s %s%s\n", mark, name, f.Summary, kind)
	}

	fmt.Fprintf(w, "\n  * = already added to this project\n")
	fmt.Fprintf(w, "\nUsage: breeze add <feature> [flags]\n")
	fmt.Fprintf(w, "Per-feature flags: breeze add <feature> --help\n")
}

func printAddHelp(w io.Writer) {
	fmt.Fprint(w, `Usage: breeze add <feature> [flags]

Wires a framework feature into features_generated.go, under a marker block
named for the feature. Re-running replaces that block, so add is the way to
change a feature's flags; --force is only needed once you have edited inside
the markers by hand.

  breeze add --list              list every feature and whether it is installed
  breeze add <feature> --help    flags for one feature

Examples:
  breeze add dashboard --basepath=/_debug --user=dev --pass=hunter2
  breeze add events --async --workers=4
  breeze add video --root=./media --signed
  breeze add i18n --locales=en,fr,de
  breeze add migrator --driver=postgres

Ordering is by the feature's priority, not the order you add them, so
`)
	fmt.Fprint(w, "`add etag` before `add recovery` still leaves recovery outermost.\n")
	fmt.Fprint(w, `
A few features do read the order you added them in: dashboard, observability,
workflow and video bridge onto the event bus and the collector when those
blocks already exist. Add one of those later and add says which blocks to
re-run to pick it up.
`)
}
