package generator

import (
	"strings"
	"testing"
)

// The mcp feature's notes say what the dashboard-reading tools do, and the answer depends on whether the
// process has a dashboard. With none they answer that there is nothing to read; with one they read its
// collector in memory, which needs no credentials and is not behind the application's rate limiter.
func TestMcpNotesSayWhatTheLiveToolsDoWithAndWithoutADashboard(t *testing.T) {
	without := generateFeature(t, features["mcp"], featureCtx{})
	with := generateFeature(t, features["mcp"], featureCtx{HasDashboard: true})

	joined := func(out featureOutput) string { return strings.Join(out.Notes, "\n") }

	for _, want := range []string{"nothing to read", "breeze_diagnose_service", "breeze add dashboard"} {
		if !strings.Contains(joined(without), want) {
			t.Errorf("without a dashboard the notes do not say %q:\n%s", want, joined(without))
		}
	}
	if strings.Contains(joined(without), "rate limiter") {
		t.Errorf("without a dashboard the notes promise an in-memory read there is nothing to read from:\n%s", joined(without))
	}

	for _, want := range []string{"in memory", "no credentials", "rate limiter"} {
		if !strings.Contains(joined(with), want) {
			t.Errorf("with a dashboard the notes do not say %q:\n%s", want, joined(with))
		}
	}
	if strings.Contains(joined(with), "nothing to read") {
		t.Errorf("with a dashboard the notes still say there is nothing to read:\n%s", joined(with))
	}

	// The generated block carries the same fact, where someone reading setupMcp will find it.
	if !strings.Contains(with.Body, "read it in memory") {
		t.Errorf("the generated block does not say the tools read the dashboard in memory:\n%s", with.Body)
	}
}
