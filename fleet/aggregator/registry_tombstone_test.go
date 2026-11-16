package aggregator

// Tests of what the registry remembers about a service it has swept. A service the registry has
// forgotten is indistinguishable, in the list of services, from one that never existed, and "never
// existed" and "died twenty minutes ago" are opposite findings in an incident.

import (
	"fmt"
	"testing"
	"time"

	"github.com/nelthaarion/breeze/v2/fleet"
)

func TestSweepLeavesATombstoneAndAReturningServiceClearsIt(t *testing.T) {
	r := NewServiceRegistry(Config{})
	t0 := time.Now().Add(-time.Hour)

	r.Observe(fleet.Heartbeat{Service: "orders", InstanceID: "i1", Version: "v1.2", OpenAPIURL: "http://orders:3000/openapi.json",
		DashboardURL: "http://orders:3000/dashboard"}, t0)
	// Alive: heard from within the forgetting window, so not swept and not entombed.
	sweepAt := t0.Add(10 * time.Minute)
	r.Observe(hb("gateway", "g1", 10, 0), sweepAt.Add(-time.Second))

	if removed := r.Sweep(sweepAt); removed != 1 {
		t.Fatalf("Sweep removed %d instances, want only orders' one", removed)
	}
	if got := r.Snapshot(sweepAt); len(got) != 1 || got[0].Name != "gateway" {
		t.Fatalf("Snapshot = %+v, want only the live gateway: a tombstone is not a live service", got)
	}

	gone := r.Forgotten(sweepAt)
	if len(gone) != 1 || gone[0].Name != "orders" {
		t.Fatalf("Forgotten = %+v, want orders alone", gone)
	}
	o := gone[0]
	if o.Status != StatusForgotten || o.Instances != 0 || o.LastHeartbeatUnix != t0.Unix() || o.ForgottenAtUnix != sweepAt.Unix() {
		t.Errorf("tombstone = %+v, want status forgotten, no instances, last seen %d and forgotten at %d", o, t0.Unix(), sweepAt.Unix())
	}
	if len(o.Versions) != 1 || o.Versions[0] != "v1.2" || o.DashboardURL != "http://orders:3000/dashboard" || o.OpenAPIURL == "" {
		t.Errorf("the tombstone lost what the service said about itself: %+v", o)
	}

	// It is back: the tombstone is no longer true.
	r.Observe(hb("orders", "i1", 5, 0), sweepAt.Add(time.Minute))
	if got := r.Forgotten(sweepAt.Add(time.Minute)); len(got) != 0 {
		t.Errorf("Forgotten = %+v after the service reported again", got)
	}
}

func TestTombstonesAreBoundedAndExpire(t *testing.T) {
	r := NewServiceRegistry(Config{})
	base := time.Now().Add(-48 * time.Hour)

	// A fleet that churns names: each service is swept the second after it goes quiet.
	for i := 0; i < maxTombstones+44; i++ {
		seen := base.Add(time.Duration(i) * time.Second)
		r.Observe(hb(fmt.Sprintf("svc-%03d", i), "i", 1, 0), seen)
		r.Sweep(seen.Add(10 * time.Minute))
	}
	at := base.Add(time.Duration(maxTombstones+44) * time.Second).Add(10 * time.Minute)
	got := r.Forgotten(at)
	if len(got) != maxTombstones {
		t.Fatalf("%d tombstones kept, want the bound of %d", len(got), maxTombstones)
	}
	names := map[string]bool{}
	for _, g := range got {
		names[g.Name] = true
	}
	if names["svc-000"] || !names[fmt.Sprintf("svc-%03d", maxTombstones+43)] {
		t.Errorf("the bound dropped the newest and kept the oldest: has svc-000=%v", names["svc-000"])
	}

	// Remembered for a day, and no longer.
	if n := len(r.Forgotten(at.Add(tombstoneRetention - time.Hour))); n == 0 {
		t.Errorf("tombstones are gone before their retention is up")
	}
	if n := len(r.Forgotten(at.Add(tombstoneRetention + time.Hour))); n != 0 {
		t.Errorf("%d tombstones outlived the retention", n)
	}
}
