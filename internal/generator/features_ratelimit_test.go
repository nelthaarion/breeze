package generator

import (
	"io"
	"os"
	"strings"
	"testing"
)

// The dashboard's HTTP API sits behind the rate limiter, and what the limiter rejects never reaches
// the dashboard's middleware. A service rejecting traffic therefore refuses reads of its dashboard at
// the moment they are wanted, which cost an agent a failed tool call before anyone wrote it down. The
// generated code says it where someone debugging a 429 will read it, and the notes say it to whoever
// is running the command.

func TestRatelimitBlockSaysTheDashboardIsBehindItOnlyWhenThereIsOne(t *testing.T) {
	with := generateFeature(t, features["ratelimit"], featureCtx{HasDashboard: true})
	without := generateFeature(t, features["ratelimit"], featureCtx{})

	for _, want := range []string{
		"The dashboard's HTTP API sits behind this limiter too",
		"breeze add mcp",
		"counted by the ratelimit diagnostic",
	} {
		if !strings.Contains(with.Body, want) {
			t.Errorf("the block generated beside a dashboard does not say %q:\n%s", want, with.Body)
		}
	}
	if strings.Contains(without.Body, "dashboard") {
		t.Errorf("the block mentions a dashboard that is not there:\n%s", without.Body)
	}

	if !strings.Contains(strings.Join(with.Notes, "\n"), "dashboard's HTTP API is behind this limiter") {
		t.Errorf("the notes do not say it: %v", with.Notes)
	}
	if strings.Contains(strings.Join(without.Notes, "\n"), "dashboard") {
		t.Errorf("the notes mention a dashboard that is not there: %v", without.Notes)
	}
}

func TestDashboardNotesSaySoWhenALimiterIsAlreadyThere(t *testing.T) {
	with := generateFeature(t, features["dashboard"], featureCtx{HasEvents: true, HasRatelimit: true})
	without := generateFeature(t, features["dashboard"], featureCtx{HasEvents: true})

	if !strings.Contains(strings.Join(with.Notes, "\n"), "rate limiter is registered before the dashboard") {
		t.Errorf("the dashboard's notes do not mention the limiter: %v", with.Notes)
	}
	if strings.Contains(strings.Join(without.Notes, "\n"), "rate limiter") {
		t.Errorf("the dashboard's notes mention a limiter that is not there: %v", without.Notes)
	}
}

// Both notes say the dashboard is registered after the limiter. That is the priority order, so the
// sentence is true only while the order is.
func TestTheLimiterIsOrderedBeforeTheDashboardAsTheNotesSay(t *testing.T) {
	limiter, dash := features["ratelimit"].Priority, features["dashboard"].Priority
	if limiter >= dash {
		t.Fatalf("ratelimit has priority %d and dashboard %d: the generated comments claim the dashboard comes after the limiter, "+
			"so they are wrong, or the order was changed on purpose and the comments, notes and docs need to follow", limiter, dash)
	}
}

// addAndCapture runs the real `breeze add` and returns what it printed, since the notes go to
// stdout and nowhere else.
func addAndCapture(t *testing.T, args ...string) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	runErr := runAdd(args)
	os.Stdout = stdout
	_ = w.Close()

	out, _ := io.ReadAll(r)
	if runErr != nil {
		t.Fatalf("add %v: %v", args, runErr)
	}
	return string(out)
}

// TestAddGivesTheDashboardAndTheLimiterEachOthersContext goes through runAdd, the path the command
// takes, because the context is built there from what is on disk: a feature generator that reads a
// context field nobody fills in passes every test that builds the context by hand.
func TestAddGivesTheDashboardAndTheLimiterEachOthersContext(t *testing.T) {
	t.Run("limiter first, then dashboard", func(t *testing.T) {
		projectDir(t)
		if err := os.WriteFile("main.go", []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		first := addAndCapture(t, "ratelimit", "--requests=30")
		if strings.Contains(first, "dashboard") {
			t.Errorf("a limiter added to a project with no dashboard mentions one:\n%s", first)
		}
		second := addAndCapture(t, "dashboard", "--no-auth")
		if !strings.Contains(second, "rate limiter is registered before the dashboard") {
			t.Errorf("a dashboard added after a limiter does not say it is behind it:\n%s", second)
		}
	})

	t.Run("dashboard first, then limiter", func(t *testing.T) {
		projectDir(t)
		if err := os.WriteFile("main.go", []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		addAndCapture(t, "dashboard", "--no-auth")
		out := addAndCapture(t, "ratelimit", "--requests=30")
		if !strings.Contains(out, "dashboard's HTTP API is behind this limiter") {
			t.Errorf("a limiter added after a dashboard does not say it is in front of it:\n%s", out)
		}
		src, err := os.ReadFile(featuresFileName)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(src), "The dashboard's HTTP API sits behind this limiter too") {
			t.Errorf("the generated limiter block does not carry the comment:\n%s", src)
		}
	})
}
