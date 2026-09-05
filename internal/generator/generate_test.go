package generator

import (
	"bytes"
	"flag"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func testFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String("plural", "", "")
	fs.Bool("force", false, "")
	return fs
}

func TestSplitFlagsEqualsForm(t *testing.T) {
	flagArgs, positional := splitFlagsAndPositional(
		testFlagSet(),
		[]string{"name:string", "--plural=people", "--force", "email:string"},
	)
	if want := []string{"--plural=people", "--force"}; !reflect.DeepEqual(flagArgs, want) {
		t.Errorf("flagArgs = %v, want %v", flagArgs, want)
	}
	if want := []string{"name:string", "email:string"}; !reflect.DeepEqual(positional, want) {
		t.Errorf("positional = %v, want %v", positional, want)
	}
}

func TestSplitFlagsSpaceSeparatedValue(t *testing.T) {
	flagArgs, positional := splitFlagsAndPositional(
		testFlagSet(),
		[]string{"name:string", "--plural", "people", "email:string"},
	)
	if want := []string{"--plural", "people"}; !reflect.DeepEqual(flagArgs, want) {
		t.Errorf("flagArgs = %v, want %v", flagArgs, want)
	}
	if want := []string{"name:string", "email:string"}; !reflect.DeepEqual(positional, want) {
		t.Errorf("positional = %v, want %v", positional, want)
	}
}

func TestSplitFlagsBoolDoesNotConsumeValue(t *testing.T) {
	flagArgs, positional := splitFlagsAndPositional(
		testFlagSet(),
		[]string{"--force", "name:string"},
	)
	if want := []string{"--force"}; !reflect.DeepEqual(flagArgs, want) {
		t.Errorf("flagArgs = %v, want %v", flagArgs, want)
	}
	if want := []string{"name:string"}; !reflect.DeepEqual(positional, want) {
		t.Errorf("positional = %v, want %v", positional, want)
	}
}

func TestSplitFlagsUnknownFlagKeptForParseError(t *testing.T) {
	flagArgs, _ := splitFlagsAndPositional(testFlagSet(), []string{"--bogus", "name:string"})
	if want := []string{"--bogus"}; !reflect.DeepEqual(flagArgs, want) {
		t.Errorf("flagArgs = %v, want %v", flagArgs, want)
	}
}

func TestUnknownFlagReturnsError(t *testing.T) {
	if err := runNew([]string{"--bogus", "myapp"}); err == nil {
		t.Error("runNew with unknown flag: expected error, got nil")
	}
	if err := generateResource(
		"example.com/x",
		"User",
		[]string{"--bogus", "name:string"},
	); err == nil {
		t.Error("generateResource with unknown flag: expected error, got nil")
	}
	if err := generateHandler("example.com/x", "User", []string{"--bogus"}); err == nil {
		t.Error("generateHandler with unknown flag: expected error, got nil")
	}
}

// TestGenerateHelpDocumentsEveryRule keeps the help and the parser in step in
// both directions.
//
// The direction that matters is a rule the parser accepts but the help omits:
// validation is only reachable by typing a rule name, so an undocumented rule is
// one nobody can use. The reverse â€” a documented rule the parser rejects â€” is
// the mistake this whole area started as, help advertising behaviour the code
// did not have.
func TestGenerateHelpDocumentsEveryRule(t *testing.T) {
	var buf bytes.Buffer
	printGenerateHelp(&buf)
	help := buf.String()

	for _, set := range []map[string]bool{validateRulesBare, validateRulesNeedingArg} {
		for name := range set {
			if !regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`).MatchString(help) {
				t.Errorf("rule %q is accepted but not documented in `breeze help generate`", name)
			}
		}
	}

	// The help spells the rules out on one line; every entry on it must be a
	// rule the parser accepts, so a typo there cannot advertise behaviour the
	// code does not have. Comma-separated because oneof's argument is itself
	// space-separated.
	m := regexp.MustCompile(`(?m)^  Rules:\s+(.*)$`).FindStringSubmatch(help)
	if m == nil {
		t.Fatal("help no longer carries a `Rules:` line; update this test or restore it")
	}
	for _, entry := range strings.Split(m[1], ",") {
		name := strings.TrimSpace(entry)
		if i := strings.IndexByte(name, '='); i >= 0 {
			name = name[:i]
		}
		if !validateRulesBare[name] && !validateRulesNeedingArg[name] {
			t.Errorf("help documents rule %q, which the parser does not accept", name)
		}
	}

	// --no-validate exists only to turn inference off, so it is useless to
	// anyone who cannot find it.
	if !strings.Contains(help, "--no-validate") {
		t.Error("help does not mention --no-validate")
	}
}
