package dashboard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// dashboardJSPath is the shipped SPA bundle the Fleet View ships inside.
const dashboardJSPath = "templates/public/dashboard.js"

// TestFleetPlaybackBehaviour runs the Fleet View's trace-playback and topology
// code under Node against a stub DOM, a controllable clock, and a controllable
// requestAnimationFrame queue.
//
// This is browser code, so Go can only reach it through a harness. Source-text
// assertions were not an option for what needs pinning here: every one of these
// is a timing or state property, invisible in the text and only observable by
// driving the code.
//
//   - one Play press walks the whole trace, rather than stalling on span two
//     because advancing recursed through the toggle that means "stop"
//   - 0.0x actually holds, rather than a `speed||1` fallback silently
//     treating the legal value 0 as unset and running at 1x
//   - the speed <select> is bound to `change`, not `click`, so a pick applies
//     now instead of one step late (and works from the keyboard at all)
//   - overlapping renders keep exactly one animation frame in flight, rather
//     than a poll tick landing mid-playback and compounding into two
//     self-scheduling loops that speed up and burn CPU the longer they run
//
// The harness is mutation-tested: reintroducing any of the four bugs above into
// a copy of dashboard.js makes it fail, each with a message naming the original
// symptom. Skipped when Node is unavailable, matching the existing SPA runtime
// tests in the root package.
func TestFleetPlaybackBehaviour(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping fleet playback behaviour tests")
	}

	harness := filepath.Join("testdata", "fleet_playback_harness.js")
	if _, err := os.Stat(harness); err != nil {
		t.Fatalf("harness missing: %v", err)
	}
	if _, err := os.Stat(dashboardJSPath); err != nil {
		t.Fatalf("dashboard.js missing: %v", err)
	}

	out, err := exec.Command(node, harness, dashboardJSPath).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "PASS") {
		t.Fatalf("fleet playback harness failed:\n%s", out)
	}
}

// TestFleetSPASyntax catches a malformed bundle early. Go never parses this
// file, so a syntax error would otherwise surface only as a blank dashboard in
// a browser — and the Fleet View is the newest, largest block of code in it.
func TestFleetSPASyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping dashboard.js syntax check")
	}

	if out, err := exec.Command(node, "--check", dashboardJSPath).CombinedOutput(); err != nil {
		t.Fatalf("dashboard.js has a syntax error:\n%s", out)
	}
}
