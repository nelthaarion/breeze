package generator

import (
	"strings"
	"testing"
)

// The workflow engine publishes one observability signal per run, with its steps as spans, directly
// into a collector and not over the bus. The dashboard reads the collector it created when it attached
// to the bus, not the one the observability block builds, so an engine pointed at the latter in a
// project with a dashboard published every run into a collector nothing reads. It was found by running a
// generated application: breeze_get_signals source=workflow answered zero while workflows ran, and the
// bus events of the same runs were recorded under source events. The generator has to follow what
// dashboard.Collector.Observability documents.
func TestWorkflowEnginePublishesIntoTheCollectorTheDashboardReads(t *testing.T) {
	const viaDashboard = "obs := DashboardCollector.Observability()"
	const fallback = "obs = ObsCollector"
	const direct = "Collector:       ObsCollector,"
	const engineUsesObs = "Collector:       obs,"

	for _, tc := range []struct {
		name string
		ctx  featureCtx
		// which of the lines below the generated block must contain
		wantDashboard, wantFallback, wantDirect bool
	}{
		{"dashboard, events and observability: the dashboard's collector, the app's as the fallback",
			featureCtx{HasEvents: true, HasObservability: true, HasDashboard: true}, true, true, false},
		{"a dashboard with no bus has no collector yet, and the engine is built when the block runs",
			featureCtx{HasObservability: true, HasDashboard: true}, true, true, false},
		{"dashboard and events, no observability block: nothing to fall back to",
			featureCtx{HasEvents: true, HasDashboard: true}, true, false, false},
		{"observability and events, no dashboard: the app's own collector, as before",
			featureCtx{HasEvents: true, HasObservability: true}, false, false, true},
		{"neither", featureCtx{HasEvents: true}, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := generateFeature(t, features["workflow"], tc.ctx)
			check := func(substr string, want bool) {
				t.Helper()
				if has := strings.Contains(out.Body, substr); has != want {
					t.Errorf("contains %q = %v, want %v:\n%s", substr, has, want, out.Body)
				}
			}
			check(viaDashboard, tc.wantDashboard)
			check(fallback, tc.wantFallback)
			check(direct, tc.wantDirect)
			check(engineUsesObs, tc.wantDashboard)
		})
	}
}
