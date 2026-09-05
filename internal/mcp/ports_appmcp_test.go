package mcp

// ports_appmcp_test.go — the fourth port purpose (Part 9).

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// TestFourDistinctPortsWhenAppMCPEnabled — Part 9's port requirement: a service with
// app-level MCP enabled gets four ports, all different, all from the one allocator.
//
// Four separate purposes rather than four numbers is the point: the failure this
// guards against is a service whose app-runtime endpoint shares a port with its
// generator-level control plane, which would silently hand every agent the ability to
// rewrite the project when it only asked to read.
func TestFourDistinctPortsWhenAppMCPEnabled(t *testing.T) {
	alloc := newPortAllocator()
	// A deterministic probe: nothing on this machine is consulted, so the test cannot
	// fail because some unrelated process holds a port in the range.
	alloc.probe = func(int) bool { return true }

	ports, err := alloc.allocateN(
		portPurposeControl, portPurposeApp, portPurposeAppMCP, portPurposeAggregator)
	if err != nil {
		t.Fatal(err)
	}
	if len(ports) != 4 {
		t.Fatalf("allocated %d ports, want 4", len(ports))
	}

	seen := map[int]string{}
	for i, port := range ports {
		if prior, dup := seen[port]; dup {
			t.Fatalf("port %d was handed out twice (%s and index %d)", port, prior, i)
		}
		seen[port] = fmt.Sprint(i)
	}

	// And each is recorded under its own purpose, which is what makes a registry
	// readable during an incident.
	wantPurposes := []string{
		portPurposeControl, portPurposeApp, portPurposeAppMCP, portPurposeAggregator,
	}
	for i, want := range wantPurposes {
		if got := alloc.purposeOf(ports[i]); got != want {
			t.Errorf("port %d purpose = %q, want %q", ports[i], got, want)
		}
	}
}

// TestAppMCPPortIsNotAllocatedByDefault — the fourth port is opt-in. A service that
// did not ask for an app-runtime endpoint must not have one, and must not burn a port
// reserving it.
func TestAppMCPPortIsNotAllocatedByDefault(t *testing.T) {
	alloc := newPortAllocator()
	alloc.probe = func(int) bool { return true }

	ports, err := alloc.allocateN(portPurposeControl, portPurposeApp)
	if err != nil {
		t.Fatal(err)
	}
	if len(ports) != 2 {
		t.Fatalf("allocated %d ports for a default provision, want 2", len(ports))
	}
	for _, port := range ports {
		if got := alloc.purposeOf(port); got == portPurposeAppMCP {
			t.Errorf("port %d was allocated as %q without being asked for", port, got)
		}
	}
}

// TestAppMCPPortRoundTripsThroughTheRegistry covers persistence: a restart must not
// hand a running container's app-runtime port to the next provision.
func TestAppMCPPortRoundTripsThroughTheRegistry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")

	// One allocator across both loads, as the orchestrator has: the point of the
	// reload is that reservations are re-derived from the file, so the allocator passed
	// in must learn about a port it never handed out itself.
	firstAlloc := newPortAllocator()
	firstAlloc.probe = func(int) bool { return true }

	first, err := loadProvisionRegistry(path, firstAlloc)
	if err != nil {
		t.Fatal(err)
	}
	service := provisionedService{
		ServiceName: "orders",
		ContainerID: "abc123",
		Host:        "127.0.0.1",
		ControlPort: 50001,
		AppPort:     50002,
		AppMCPPort:  50003,
		Image:       "breeze-provisioned/orders:latest",
		CreatedAt:   time.Now().UTC(),
	}
	if err := first.add(service); err != nil {
		t.Fatal(err)
	}

	// Reload, as a restarted orchestrator does, with a fresh allocator that has never
	// seen these numbers.
	secondAlloc := newPortAllocator()
	secondAlloc.probe = func(int) bool { return true }

	second, err := loadProvisionRegistry(path, secondAlloc)
	if err != nil {
		t.Fatal(err)
	}

	restored, err := second.get("orders")
	if err != nil {
		t.Fatal(err)
	}
	if restored.AppMCPPort != 50003 {
		t.Errorf("app_mcp_port survived as %d, want 50003", restored.AppMCPPort)
	}
	if got := second.ports.purposeOf(50003); got != portPurposeAppMCP {
		t.Errorf("after reload, port 50003 is held as %q, want %q", got, portPurposeAppMCP)
	}

	// And the URL is distinct from the control URL — the same port would make them
	// identical, which is the confusion the fourth purpose exists to prevent.
	if restored.appMCPURL() == restored.controlURL() {
		t.Error("app_mcp_url and control_url are the same address")
	}

	// Deprovisioning releases it.
	if _, err := second.remove("orders"); err != nil {
		t.Fatal(err)
	}
	if got := second.ports.purposeOf(50003); got != "" {
		t.Errorf("port 50003 is still held as %q after removal", got)
	}
}
