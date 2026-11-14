package main

import (
	"strings"
	"testing"
	"time"

	"github.com/nelthaarion/breeze/v2/fleet/aggregator"
)

func TestFleetServiceTTLComesFromTheEnvironment(t *testing.T) {
	get := func(v string) func(string) string {
		return func(key string) string {
			if key == "FLEET_SERVICE_TTL" {
				return v
			}
			return ""
		}
	}

	for _, tc := range []struct {
		name    string
		value   string
		want    time.Duration
		wantErr string
	}{
		{"unset keeps the default", "", aggregator.DefaultServiceTTL, ""},
		{"seconds", "45s", 45 * time.Second, ""},
		{"minutes", "2m", 2 * time.Minute, ""},
		{"spaces are ignored", "  90s ", 90 * time.Second, ""},
		{"not a duration stops the process rather than reverting", "banana", 0, "not a duration"},
		{"a bare number is ambiguous and refused", "30", 0, "not a duration"},
		{"zero is refused", "0s", 0, "must be positive"},
		{"negative is refused", "-5s", 0, "must be positive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := aggregator.DefaultConfig()
			err := applyServiceTTL(&cfg, get(tc.value))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
				if cfg.ServiceTTL != aggregator.DefaultServiceTTL {
					t.Errorf("a refused value changed the TTL to %v", cfg.ServiceTTL)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.ServiceTTL != tc.want {
				t.Errorf("ServiceTTL = %v, want %v", cfg.ServiceTTL, tc.want)
			}
		})
	}
}
