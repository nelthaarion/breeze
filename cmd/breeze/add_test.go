package main

// Tests for the `breeze add` registry and its dispatch.
//
// The failure mode these guard against is drift between three lists that must
// agree but live apart: the registry itself, the feature names printUsage
// advertises, and the alias table. Nothing about a mismatch is a compile error
// — `breeze add workerpool` just reports "unknown feature" for a subsystem the
// help text promised.

import (
	"bytes"
	"flag"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestFeatureRegistryInvariants(t *testing.T) {
	for name, f := range features {
		if name != f.Name {
			t.Errorf("feature registered under %q but names itself %q", name, f.Name)
		}
		if name != strings.ToLower(name) {
			t.Errorf("feature %q is not lowercase — args[0] is lowercased before lookup, so it is unreachable", name)
		}
		if strings.TrimSpace(f.Summary) == "" {
			t.Errorf("feature %q has no Summary — `add --list` would show a blank line", name)
		}
		if f.Build == nil {
			t.Errorf("feature %q has a nil Build — add would panic", name)
		}
		if f.Priority <= 0 {
			t.Errorf("feature %q has priority %d; priorities order the dispatcher and must be positive", name, f.Priority)
		}
		for _, dep := range f.DependsOn {
			if _, ok := features[dep]; !ok {
				t.Errorf("feature %q depends on %q, which is not a registered feature — the stale-block warning would name a command that fails", name, dep)
			}
			if dep == name {
				t.Errorf("feature %q lists itself in DependsOn", name)
			}
		}
	}
}

// generateFeature runs a feature's generator with default flags. Flags are
// beside the point for the caller below: what is being measured is whether the
// generator consults featureCtx, and no flag changes that.
func generateFeature(t *testing.T, f *feature, ctx featureCtx) featureOutput {
	t.Helper()

	fs := flag.NewFlagSet("add "+f.Name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	generate := f.Build(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parsing default flags for %q: %v", f.Name, err)
	}

	out, err := generate(ctx)
	if err != nil {
		t.Fatalf("generating %q: %v", f.Name, err)
	}
	return out
}

// TestDependsOnMatchesWhatGeneratorsRead derives each feature's dependencies
// instead of trusting the table: generate its block with a composition input off
// and then on, and if the code moved, the feature reads that input.
//
// Both directions of a mismatch are worth failing on. A dependency the
// generator has but the table omits means `add events` stays silent about a
// block it just invalidated — the case that leaves a dashboard's Events page
// permanently empty with no signal anywhere. A dependency in the table the
// generator does not have means add prints a re-run instruction that would
// change nothing.
func TestDependsOnMatchesWhatGeneratorsRead(t *testing.T) {
	// Generation builds strings and does not read the project, but chdir into a
	// scratch directory anyway so a generator that ever starts stat-ing cannot
	// pick up this repo's own files.
	t.Chdir(t.TempDir())

	inputs := []struct {
		name string
		set  func(*featureCtx)
	}{
		{"events", func(c *featureCtx) { c.HasEvents = true }},
		{"observability", func(c *featureCtx) { c.HasObservability = true }},
	}

	for _, name := range featureNames() {
		f := features[name]
		for _, in := range inputs {
			if in.name == name {
				continue
			}

			without := featureCtx{ModulePath: "example.com/proj"}
			with := without
			in.set(&with)

			off := generateFeature(t, f, without)
			on := generateFeature(t, f, with)

			// Notes are deliberately excluded: a feature whose only difference is
			// the advice it prints generates the same block either way, so there
			// is nothing to re-run.
			reads := off.Body != on.Body || !slices.Equal(off.Imports, on.Imports)
			declared := slices.Contains(f.DependsOn, in.name)

			switch {
			case reads && !declared:
				t.Errorf("feature %q generates different code when %q is installed but does not list it in DependsOn — "+
					"`breeze add %s` will not report that %q needs re-running", name, in.name, in.name, name)
			case declared && !reads:
				t.Errorf("feature %q lists %q in DependsOn but generates identical code either way — "+
					"the stale-block warning would be noise", name, in.name)
			}
		}
	}
}

// TestFeatureAliasesResolve checks the alias table cannot point anywhere
// useless. An alias naming a feature that does not exist is worse than no
// alias: it reports "unknown feature" naming the canonical name the user did
// not type.
func TestFeatureAliasesResolve(t *testing.T) {
	for alias, canonical := range featureAliases {
		if _, ok := features[canonical]; !ok {
			t.Errorf("alias %q resolves to %q, which is not a registered feature", alias, canonical)
		}
		if _, ok := features[alias]; ok {
			t.Errorf("alias %q is also a real feature name — the alias is dead code and the note it prints would be wrong", alias)
		}
		if alias != strings.ToLower(alias) {
			t.Errorf("alias %q is not lowercase, so it can never match the lowercased input", alias)
		}
	}
}

func TestResolveFeatureName(t *testing.T) {
	// workerpool is the case that motivated the table: the framework file is
	// workerpool.go but the feature is called tuning.
	if got := resolveFeatureName("workerpool"); got != "tuning" {
		t.Errorf("resolveFeatureName(\"workerpool\") = %q, want \"tuning\"", got)
	}
	// A real feature name must pass through untouched rather than be rewritten.
	if got := resolveFeatureName("dashboard"); got != "dashboard" {
		t.Errorf("resolveFeatureName(\"dashboard\") = %q, want \"dashboard\"", got)
	}
	// An unknown name is returned as typed, so the error names what the user
	// actually wrote.
	if got := resolveFeatureName("nonsense"); got != "nonsense" {
		t.Errorf("resolveFeatureName(\"nonsense\") = %q, want it unchanged", got)
	}
}

func TestSuggestFeature(t *testing.T) {
	// Within the two-edit cutoff.
	if s := suggestFeature("dashboad"); !strings.Contains(s, `"dashboard"`) {
		t.Errorf("suggestFeature(\"dashboad\") = %q, want a dashboard suggestion", s)
	}
	// Beyond it: proposing a random feature for an unrelated word is noise.
	if s := suggestFeature("kubernetes"); s != "" {
		t.Errorf("suggestFeature(\"kubernetes\") = %q, want no suggestion", s)
	}
	// A near-miss on an alias suggests the canonical name, not the alias, so
	// the advice is a command that also teaches the real name.
	if s := suggestFeature("workerpol"); !strings.Contains(s, `"tuning"`) {
		t.Errorf("suggestFeature(\"workerpol\") = %q, want a tuning suggestion", s)
	}
}

// TestSuggestFeatureIsDeterministic guards the sort in suggestionCandidates.
// Both maps it draws from have randomised iteration order, so without it a tie
// between two equally-distant names produced a different suggestion per run —
// and an intermittently-failing assertion above.
func TestSuggestFeatureIsDeterministic(t *testing.T) {
	first := suggestFeature("dashboad")
	for i := 0; i < 50; i++ {
		if got := suggestFeature("dashboad"); got != first {
			t.Fatalf("suggestion varies between runs: %q then %q", first, got)
		}
	}
}

// TestUsageAdvertisesEveryFeature keeps the hand-written feature list in
// printUsage honest. It is the list a user reads before typing anything, so a
// name missing from it is a feature nobody finds, and a name in it that the
// registry lacks is a command that fails.
func TestUsageAdvertisesEveryFeature(t *testing.T) {
	var buf bytes.Buffer
	printUsage(&buf)
	usage := buf.String()

	for name := range features {
		if !regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`).MatchString(usage) {
			t.Errorf("feature %q is registered but not named in printUsage", name)
		}
	}

	// The count is written out as prose ("for all 21"), so it goes stale
	// silently.
	m := regexp.MustCompile(`for all (\d+)`).FindStringSubmatch(usage)
	if m == nil {
		t.Fatal("printUsage no longer states a feature count; update this test or restore it")
	}
	if want := len(features); m[1] != strconv.Itoa(want) {
		t.Errorf("printUsage advertises %s features, registry has %d", m[1], want)
	}
}

// TestFeatureListRendersEveryFeature covers `breeze add --list`, the other
// place the full set is presented.
func TestFeatureListRendersEveryFeature(t *testing.T) {
	var buf bytes.Buffer
	printFeatureList(&buf)
	out := buf.String()

	for name, f := range features {
		if !strings.Contains(out, name) {
			t.Errorf("feature %q missing from `add --list` output", name)
		}
		if !strings.Contains(out, f.Summary) {
			t.Errorf("feature %q summary missing from `add --list` output", name)
		}
	}
}
