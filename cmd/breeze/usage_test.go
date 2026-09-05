package main

// The usage text is the only substantive thing left in this package after the
// generators moved to internal/generator, and it is hand-written prose that
// names features the registry owns. That split is exactly why this test exists
// here: the two halves can now drift independently, and nothing about a
// mismatch is a compile error. `breeze add workerpool` just reports "unknown
// feature" for a subsystem the help text promised.

import (
	"bytes"
	"regexp"
	"strconv"
	"testing"

	"github.com/nelthaarion/breeze/v2/internal/generator"
)

// TestUsageAdvertisesEveryFeature keeps the hand-written feature list in
// printUsage honest. It is the list a user reads before typing anything, so a
// name missing from it is a feature nobody finds.
func TestUsageAdvertisesEveryFeature(t *testing.T) {
	var buf bytes.Buffer
	printUsage(&buf)
	usage := buf.String()

	names := generator.FeatureNames()
	if len(names) == 0 {
		t.Fatal("the feature registry is empty; the facade is not reporting it")
	}

	for _, name := range names {
		if !regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`).MatchString(usage) {
			t.Errorf("feature %q is registered but not named in printUsage", name)
		}
	}

	// The count is written out as prose ("for all 23"), so it goes stale
	// silently.
	m := regexp.MustCompile(`for all (\d+)`).FindStringSubmatch(usage)
	if m == nil {
		t.Fatal("printUsage no longer states a feature count; update this test or restore it")
	}
	if want := len(names); m[1] != strconv.Itoa(want) {
		t.Errorf("printUsage advertises %s features, registry has %d", m[1], want)
	}
}

// TestUsageNamesOnlyRealFeatures is the other direction, and it could not be
// written before the split without listing the prose names by hand.
//
// The feature block is matched by position rather than by scanning the whole
// text, because the usage string mentions plenty of words that are not features
// (commands, flags, generator kinds) and a whole-text scan would either pass
// vacuously or drown in false positives.
func TestUsageNamesOnlyRealFeatures(t *testing.T) {
	var buf bytes.Buffer
	printUsage(&buf)

	block := regexp.MustCompile(`(?s)Features \(breeze add --list for all \d+\):\n(.*?)\n\n`).
		FindStringSubmatch(buf.String())
	if block == nil {
		t.Fatal("could not locate the feature block in printUsage; update this test if the layout changed")
	}

	known := make(map[string]bool, 32)
	for _, name := range generator.FeatureNames() {
		known[name] = true
	}

	for _, word := range regexp.MustCompile(`[a-z0-9]+`).FindAllString(block[1], -1) {
		if !known[word] && generator.CanonicalFeatureName(word) == "" {
			t.Errorf("printUsage's feature list names %q, which is not a registered feature or alias — "+
				"`breeze add %s` would fail", word, word)
		}
	}
}
