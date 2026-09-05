package aggregator

// Registry tests.
//
// The registry is what the topology view's node colours and the "is this service
// even running" question resolve to, so the properties that matter most are the
// ones that would mislead someone mid-incident: an instance count that counts dead
// replicas, an error rate skewed by an idle pod, or a service that vanishes from
// the UI because it restarted.

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/nelthaarion/breeze/v2/fleet"
)

func hb(service, instance string, rps, errRate float64) fleet.Heartbeat {
	return fleet.Heartbeat{
		Service:    service,
		InstanceID: instance,
		RPS:        rps,
		ErrorRate:  errRate,
	}
}

func TestRegistryObserveAndSnapshot(t *testing.T) {
	r := NewServiceRegistry(Config{})
	now := time.Now()

	r.Observe(hb("gateway", "i1", 100, 0), now)
	r.Observe(hb("orders", "i1", 50, 0), now)

	got := r.Snapshot(now)
	if len(got) != 2 {
		t.Fatalf("Snapshot returned %d services, want 2", len(got))
	}
	// Sorted, so a reader's table does not reshuffle between polls.
	if got[0].Name != "gateway" || got[1].Name != "orders" {
		t.Errorf("services = %q, %q; want gateway, orders sorted", got[0].Name, got[1].Name)
	}
	if got[0].Status != StatusUp {
		t.Errorf("status = %q, want %q", got[0].Status, StatusUp)
	}
	if got[0].Instances != 1 {
		t.Errorf("instances = %d, want 1", got[0].Instances)
	}
}

// TestRegistryCountsInstancesNotHeartbeats is the replica-count property: three
// pods must read as three instances, and repeated beats from the same pod must not
// inflate that.
func TestRegistryCountsInstancesNotHeartbeats(t *testing.T) {
	r := NewServiceRegistry(Config{})
	now := time.Now()

	for _, inst := range []string{"pod-a", "pod-b", "pod-c"} {
		for beat := 0; beat < 5; beat++ {
			r.Observe(hb("orders", inst, 10, 0), now)
		}
	}

	info, ok := r.Service("orders", now)
	if !ok {
		t.Fatal("orders not in registry")
	}
	if info.Instances != 3 {
		t.Errorf("instances = %d, want 3", info.Instances)
	}
	// RPS sums across replicas: the service as a whole is serving 30/s.
	if info.RPS != 30 {
		t.Errorf("rps = %v, want 30", info.RPS)
	}
}

// TestRegistryMissingInstanceIDCountsAsOne covers the small-fleet default. A
// service that never set InstanceID is still one running instance, and reporting
// zero would make a live service look absent.
func TestRegistryMissingInstanceIDCountsAsOne(t *testing.T) {
	r := NewServiceRegistry(Config{})
	now := time.Now()

	r.Observe(fleet.Heartbeat{Service: "orders", RPS: 5}, now)
	r.Observe(fleet.Heartbeat{Service: "orders", RPS: 5}, now)

	info, _ := r.Service("orders", now)
	if info.Instances != 1 {
		t.Errorf("instances = %d, want 1", info.Instances)
	}
}

// TestRegistryIgnoresUnnamedHeartbeats — an entry under "" would be a permanent
// mystery row in the UI attributable to nothing.
func TestRegistryIgnoresUnnamedHeartbeats(t *testing.T) {
	r := NewServiceRegistry(Config{})
	r.Observe(fleet.Heartbeat{InstanceID: "i1", RPS: 10}, time.Now())

	if got := r.Count(); got != 0 {
		t.Errorf("Count = %d, want 0", got)
	}
}

// TestRegistryMarksSilentServiceDown is the core liveness property.
func TestRegistryMarksSilentServiceDown(t *testing.T) {
	r := NewServiceRegistry(Config{ServiceTTL: 15 * time.Second})
	start := time.Now()

	r.Observe(hb("orders", "i1", 10, 0), start)

	if info, _ := r.Service("orders", start); info.Status != StatusUp {
		t.Errorf("status right after a beat = %q, want up", info.Status)
	}
	// Past the TTL with no further beat.
	later := start.Add(20 * time.Second)
	info, ok := r.Service("orders", later)
	if !ok {
		t.Fatal("service disappeared from the registry — down is not the same as gone")
	}
	if info.Status != StatusDown {
		t.Errorf("status after TTL = %q, want down", info.Status)
	}
	if info.Instances != 0 {
		t.Errorf("instances = %d, want 0 — a silent replica must not be counted live", info.Instances)
	}
	// The last-seen time must survive, since "when did it stop" is the first
	// question anyone asks about a down service.
	if info.LastHeartbeatUnix != start.Unix() {
		t.Errorf("LastHeartbeatUnix = %d, want %d", info.LastHeartbeatUnix, start.Unix())
	}
}

// TestRegistryRecovers checks a flapping pod comes back up rather than staying
// marked down until swept.
func TestRegistryRecovers(t *testing.T) {
	r := NewServiceRegistry(Config{ServiceTTL: 15 * time.Second})
	start := time.Now()

	r.Observe(hb("orders", "i1", 10, 0), start)
	gone := start.Add(30 * time.Second)
	if info, _ := r.Service("orders", gone); info.Status != StatusDown {
		t.Fatalf("status = %q, want down", info.Status)
	}

	back := gone.Add(time.Second)
	r.Observe(hb("orders", "i1", 10, 0), back)
	if info, _ := r.Service("orders", back); info.Status != StatusUp {
		t.Errorf("status after recovery = %q, want up", info.Status)
	}
}

func TestRegistryDegradedOnErrorRate(t *testing.T) {
	r := NewServiceRegistry(Config{})
	now := time.Now()

	r.Observe(hb("orders", "i1", 100, 0.2), now)

	info, _ := r.Service("orders", now)
	if info.Status != StatusDegraded {
		t.Errorf("status = %q, want degraded at a 20%% error rate", info.Status)
	}
}

// TestRegistryErrorRateIsTrafficWeighted is the one piece of arithmetic here that
// is easy to get wrong and actively misleading when wrong: one idle replica
// failing everything must not drag a busy healthy service to 50%.
func TestRegistryErrorRateIsTrafficWeighted(t *testing.T) {
	r := NewServiceRegistry(Config{})
	now := time.Now()

	// 1000 rps clean, plus an idle replica at 1 rps failing everything.
	r.Observe(hb("orders", "busy", 1000, 0), now)
	r.Observe(hb("orders", "idle", 1, 1.0), now)

	info, _ := r.Service("orders", now)

	// Flat mean would be 0.5. Weighted is ~0.001.
	if info.ErrorRate > 0.01 {
		t.Errorf("ErrorRate = %v, want ~0.001 — a flat mean over replicas misreports service health", info.ErrorRate)
	}
	if info.Status != StatusUp {
		t.Errorf("status = %q, want up: 1 failing request in 1001 is not a degraded service", info.Status)
	}
}

// TestRegistryErrorRateWithoutTraffic covers the fallback: with no rps to weight
// by, an idle service reporting errors must not read as perfectly healthy.
func TestRegistryErrorRateWithoutTraffic(t *testing.T) {
	r := NewServiceRegistry(Config{})
	now := time.Now()

	r.Observe(hb("orders", "i1", 0, 0.5), now)

	info, _ := r.Service("orders", now)
	if info.ErrorRate != 0.5 {
		t.Errorf("ErrorRate = %v, want 0.5", info.ErrorRate)
	}
}

// TestRegistryTracksVersions makes a half-finished deploy visible — a common cause
// of the §9A contract violations, so worth surfacing rather than collapsing.
func TestRegistryTracksVersions(t *testing.T) {
	r := NewServiceRegistry(Config{})
	now := time.Now()

	a := hb("orders", "old", 10, 0)
	a.Version = "v1.0.0"
	b := hb("orders", "new", 10, 0)
	b.Version = "v1.1.0"
	r.Observe(a, now)
	r.Observe(b, now)

	info, _ := r.Service("orders", now)
	if len(info.Versions) != 2 {
		t.Fatalf("Versions = %v, want two entries during a rollout", info.Versions)
	}
	if info.Versions[0] != "v1.0.0" || info.Versions[1] != "v1.1.0" {
		t.Errorf("Versions = %v, want sorted [v1.0.0 v1.1.0]", info.Versions)
	}
}

// TestRegistryCarriesSchemaFieldsFromNewestInstance — §9A.3/§9C.3 both key schema
// refresh off this hash, and mid-deploy the newer instance is the one the fleet is
// converging on.
func TestRegistryCarriesSchemaFieldsFromNewestInstance(t *testing.T) {
	r := NewServiceRegistry(Config{})
	start := time.Now()

	old := hb("orders", "old", 10, 0)
	old.OpenAPIHash = "hash-old"
	old.OpenAPIURL = "http://old/openapi.json"
	r.Observe(old, start)

	newer := hb("orders", "new", 10, 0)
	newer.OpenAPIHash = "hash-new"
	newer.OpenAPIURL = "http://new/openapi.json"
	r.Observe(newer, start.Add(time.Second))

	info, _ := r.Service("orders", start.Add(time.Second))
	if info.OpenAPIHash != "hash-new" {
		t.Errorf("OpenAPIHash = %q, want hash-new", info.OpenAPIHash)
	}
	if info.OpenAPIURL != "http://new/openapi.json" {
		t.Errorf("OpenAPIURL = %q", info.OpenAPIURL)
	}
}

func TestRegistryUnknownService(t *testing.T) {
	r := NewServiceRegistry(Config{})
	if _, ok := r.Service("nope", time.Now()); ok {
		t.Error("Service reported an unknown name as present")
	}
}

// TestRegistrySweepKeepsRecentlyDownServices is the distinction between "down" and
// "forgotten". A restarting pod must remain visible as down; deleting it would make
// it silently vanish from the UI while someone is watching it.
func TestRegistrySweepKeepsRecentlyDownServices(t *testing.T) {
	r := NewServiceRegistry(Config{ServiceTTL: 15 * time.Second})
	start := time.Now()

	r.Observe(hb("orders", "i1", 10, 0), start)

	// Well past ServiceTTL, nowhere near the forget threshold.
	justDown := start.Add(time.Minute)
	if removed := r.Sweep(justDown); removed != 0 {
		t.Errorf("Sweep removed %d instances one minute in; a down service must stay visible", removed)
	}
	info, ok := r.Service("orders", justDown)
	if !ok {
		t.Fatal("service was forgotten instead of marked down")
	}
	if info.Status != StatusDown {
		t.Errorf("status = %q, want down", info.Status)
	}
}

func TestRegistrySweepForgetsLongGoneServices(t *testing.T) {
	r := NewServiceRegistry(Config{ServiceTTL: 15 * time.Second})
	start := time.Now()

	r.Observe(hb("orders", "i1", 10, 0), start)

	// 15s TTL × 20 = 5m memory; an hour later it should be gone.
	if removed := r.Sweep(start.Add(time.Hour)); removed != 1 {
		t.Errorf("Sweep removed %d, want 1", removed)
	}
	if _, ok := r.Service("orders", start.Add(time.Hour)); ok {
		t.Error("a long-dead service is still in the registry")
	}
	if got := r.Count(); got != 0 {
		t.Errorf("Count = %d, want 0 — an empty service entry must be removed too", got)
	}
}

// TestRegistryForgetAfterHasFloor guards a small-TTL misconfiguration from
// producing a registry that forgets services faster than it marks them down.
func TestRegistryForgetAfterHasFloor(t *testing.T) {
	r := NewServiceRegistry(Config{ServiceTTL: time.Millisecond})
	if got := r.forgetAfter(); got < time.Minute {
		t.Errorf("forgetAfter = %v, want at least a minute", got)
	}
}

func TestRegistrySweepPartialInstances(t *testing.T) {
	r := NewServiceRegistry(Config{ServiceTTL: 15 * time.Second})
	start := time.Now()

	r.Observe(hb("orders", "dead", 10, 0), start)
	alive := start.Add(time.Hour)
	r.Observe(hb("orders", "alive", 10, 0), alive)

	if removed := r.Sweep(alive); removed != 1 {
		t.Errorf("Sweep removed %d, want 1 (only the dead replica)", removed)
	}
	info, ok := r.Service("orders", alive)
	if !ok {
		t.Fatal("service removed despite a live instance")
	}
	if info.Instances != 1 {
		t.Errorf("instances = %d, want 1", info.Instances)
	}
}

// --- Change tracking -------------------------------------------------------

// TestRegistryTakeChanged pins the WS-push trigger. Routine rps updates must not
// fire an event, or the feed would push on every heartbeat from every service
// forever; membership and schema changes must.
func TestRegistryTakeChanged(t *testing.T) {
	r := NewServiceRegistry(Config{})
	now := time.Now()

	r.Observe(hb("orders", "i1", 10, 0), now)
	if !r.TakeChanged() {
		t.Error("a new service did not register as a change")
	}
	// Read-and-clear.
	if r.TakeChanged() {
		t.Error("TakeChanged did not reset")
	}

	r.Observe(hb("orders", "i1", 99, 0.5), now)
	if r.TakeChanged() {
		t.Error("a routine rps/error update fired a change event")
	}

	r.Observe(hb("orders", "i2", 10, 0), now)
	if !r.TakeChanged() {
		t.Error("a new instance did not register as a change")
	}

	withSchema := hb("orders", "i1", 10, 0)
	withSchema.OpenAPIHash = "new-hash"
	r.Observe(withSchema, now)
	if !r.TakeChanged() {
		t.Error("a schema-hash change did not register — §9A.3 depends on noticing this")
	}
}

func TestRegistrySweepMarksChanged(t *testing.T) {
	r := NewServiceRegistry(Config{ServiceTTL: time.Second})
	start := time.Now()

	r.Observe(hb("orders", "i1", 10, 0), start)
	r.TakeChanged()

	r.Sweep(start.Add(time.Hour))
	if !r.TakeChanged() {
		t.Error("forgetting a service did not register as a change")
	}
}

// --- Concurrency -----------------------------------------------------------

// TestRegistryConcurrentAccess is the production shape: many services beating
// while the dashboard polls. Meaningful under -race (§14.6).
func TestRegistryConcurrentAccess(t *testing.T) {
	r := NewServiceRegistry(Config{})
	now := time.Now()

	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			service := fmt.Sprintf("svc-%d", w%3)
			for i := 0; i < 100; i++ {
				r.Observe(hb(service, fmt.Sprintf("i%d", w), float64(i), 0), now)
			}
		}(w)
	}
	for reader := 0; reader < 4; reader++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				_ = r.Snapshot(now)
				_, _ = r.Service("svc-0", now)
				_ = r.Count()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			r.Sweep(now)
			r.TakeChanged()
		}
	}()
	wg.Wait()

	if got := r.Count(); got != 3 {
		t.Errorf("Count = %d, want 3", got)
	}
}
