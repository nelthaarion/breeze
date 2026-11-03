package generator

// Tests for how the fleet block wires the tracer's Logger.
//
// The general guard is TestNoSetupFunctionUsesAHandleAssignedByOneDispatchedAfterIt,
// which reads the generated file as a whole. These pin the specific shape the fix
// takes, so a well-meaning "simplification" back to the method value fails here with
// the reason beside it instead of in a service that exits on its first traced request.

import (
	"strings"
	"testing"
)

// loggerLine returns the Logger field of the TracerConfig literal, as fields.
func loggerLine(t *testing.T, body string) []string {
	t.Helper()

	for _, line := range strings.Split(body, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 && fields[0] == "Logger:" {
			return fields
		}
	}
	t.Fatalf("the fleet block has no Logger field:\n%s", body)
	return nil
}

func TestFleetLoggerForwardsThroughAFunctionWhenADashboardIsPresent(t *testing.T) {
	out := generateFeature(t, features["fleet"], featureCtx{HasDashboard: true})

	// The field must name the function, not the method value. DashboardCollector is
	// nil when the tracer is built, because setupFleet is dispatched first, and a
	// method value is bound to its receiver at that moment.
	if got := loggerLine(t, out.Body); len(got) != 2 || got[1] != "fleetLog," {
		t.Errorf("Logger = %q, want fleetLog; a method value on DashboardCollector would be bound to nil", got)
	}

	for _, want := range []string{
		"func fleetLog(level, message, source string) {",
		// Read when a line is logged, and guarded: before the dashboard is installed
		// there is nowhere for the line to go, and dropping it is correct.
		"if c := DashboardCollector; c != nil {",
		"c.PushLog(level, message, source)",
	} {
		if !strings.Contains(out.Body, want) {
			t.Errorf("the block does not contain %q:\n%s", want, out.Body)
		}
	}
}

func TestFleetLoggerIsNilAndNothingIsEmittedWithoutADashboard(t *testing.T) {
	out := generateFeature(t, features["fleet"], featureCtx{})

	if got := loggerLine(t, out.Body); len(got) != 2 || got[1] != "nil," {
		t.Errorf("Logger = %q, want nil: with no dashboard block there is nowhere for the lines to go", got)
	}
	// Dead code in a generated file is read by the person who has to maintain it.
	if strings.Contains(out.Body, "fleetLog") {
		t.Errorf("fleetLog is emitted although nothing uses it:\n%s", out.Body)
	}
}
