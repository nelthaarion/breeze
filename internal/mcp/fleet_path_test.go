package mcp

// fleet_path_test.go — the aggregator_url a caller actually pastes.
//
// package mcp rather than mcp_test because this exercises fleetArgs directly. The
// end-to-end half, against a real aggregator, is in tools_fleet_test.go, which has to be
// an external test package to import fleet/aggregator.

import "testing"

// TestFleetRequestPathDoesNotDoubleTheBasePath covers the most likely aggregator_url
// mistake, and the reason it was worth fixing in code rather than in documentation.
//
// Every other place in this codebase that names an aggregator names its *mount*:
// fleet.TracerConfig.AggregatorURL, dashboard.Config.FleetAggregatorURL, and
// provision_fleet's own returned aggregator_url are all "http://host:9000/fleet". These
// tools take an origin plus a separate base_path. So the value a caller already has
// produced /fleet/fleet/api/... — and the 404 that followed reported "the fleet
// aggregator feature is not installed", which is false and points somewhere else
// entirely.
func TestFleetRequestPathDoesNotDoubleTheBasePath(t *testing.T) {
	cases := []struct {
		aggregatorURL string
		basePath      string
		want          string
	}{
		// An origin gets the base path added, exactly as before.
		{"http://127.0.0.1:9000", "", "/fleet/api/topology"},
		{"http://127.0.0.1:9000/", "", "/fleet/api/topology"},
		// A mount URL already carries it, so it is not added twice.
		{"http://127.0.0.1:9000/fleet", "", "/api/topology"},
		{"http://127.0.0.1:9000/fleet/", "", "/api/topology"},
		// A custom mount behaves identically, in both spellings and with or without
		// the leading slash on base_path.
		{"http://127.0.0.1:9000", "/tracing", "/tracing/api/topology"},
		{"http://127.0.0.1:9000/tracing", "/tracing", "/api/topology"},
		{"http://127.0.0.1:9000/tracing", "tracing", "/api/topology"},
		// A path that merely contains the base path is not a mount ending in it.
		{"http://127.0.0.1:9000/fleet-proxy", "", "/fleet/api/topology"},
	}

	for _, tc := range cases {
		args := fleetArgs{AggregatorURL: tc.aggregatorURL, BasePath: tc.basePath}
		if got := args.request("/topology", "test").path; got != tc.want {
			t.Errorf("aggregator_url %q with base_path %q built %q, want %q",
				tc.aggregatorURL, tc.basePath, got, tc.want)
		}
	}
}
