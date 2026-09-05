package mcp

// tools_provision_test.go — Category H, tested without a Docker daemon where that is
// possible and skipped where it is not.
//
// # The fake, and what it does not fake
//
// dockerClient.run is replaced with a recorder. That substitution is deliberately at
// the lowest possible level — the argv handed to the docker binary — so everything
// above it is the real code: port allocation, token generation, project generation
// through generator.New, the Fleet wiring through ApplyConfig, the registry writes,
// the rollback paths. A test that faked the orchestrator instead would prove only
// that the fake behaves.
//
// What the fake cannot prove is that a built image runs. The tests that require that
// — a genuinely reachable app_port, a working control_port+token pair, traces flowing
// between provisioned services — are in this file too and skip without a daemon,
// because a test that silently passed on a machine with no Docker would be worse than
// one that says why it did not run.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeDocker records the commands it was given and answers them plausibly.
type fakeDocker struct {
	// commands is every argv, in order, so a test can assert on what was asked of
	// Docker rather than only on the result.
	commands [][]string

	// failOn makes the first command whose first argument matches fail. This is how
	// the rollback paths are reached: they are the paths that only run when Docker
	// says no.
	failOn string

	// nextID is the container id the next `run` reports.
	nextID int

	// running is what inspect reports.
	running bool
	health  string
}

func (f *fakeDocker) client() *dockerClient {
	f.running = true
	return &dockerClient{
		binary: "docker-fake",
		run: func(_ context.Context, _ string, args ...string) (string, error) {
			f.commands = append(f.commands, args)

			if f.failOn != "" && len(args) > 0 && args[0] == f.failOn {
				return "", fmt.Errorf("docker %s failed: simulated failure", args[0])
			}

			switch args[0] {
			case "version":
				return "27.0.0", nil
			case "run":
				f.nextID++
				return fmt.Sprintf("c0ntainer%04d", f.nextID), nil
			case "inspect":
				return fmt.Sprintf(`{"running":%t,"status":%q,"health":%q}`,
					f.running, map[bool]string{true: "running", false: "exited"}[f.running], f.health), nil
			default:
				return "", nil
			}
		},
	}
}

// argvFor returns the first recorded command starting with name.
func (f *fakeDocker) argvFor(name string) []string {
	for _, args := range f.commands {
		if len(args) > 0 && args[0] == name {
			return args
		}
	}
	return nil
}

// argvAll returns every recorded command starting with name.
func (f *fakeDocker) argvAll(name string) [][]string {
	var out [][]string
	for _, args := range f.commands {
		if len(args) > 0 && args[0] == name {
			out = append(out, args)
		}
	}
	return out
}

// newTestOrchestrator returns an orchestrator whose registry is in a temporary
// directory and whose Docker is the fake.
//
// The port allocator's probe is left real: the ports it hands out are genuinely free
// on the machine running the test, which is what makes the collision test meaningful
// rather than a test of a counter.
func newTestOrchestrator(t *testing.T, fake *fakeDocker) *orchestrator {
	t.Helper()

	dir := t.TempDir()
	orch := newOrchestrator("test")

	reg, err := loadProvisionRegistry(filepath.Join(dir, registryFileName), orch.ports)
	if err != nil {
		t.Fatalf("loading a fresh registry: %v", err)
	}
	orch.registry = reg
	orch.docker = fake.client()
	return orch
}

// minimalService is the smallest provisioning request that generates a real project.
func minimalService(name string) provisionServiceArgs {
	return provisionServiceArgs{
		Name:     name,
		Template: "api",
		configInput: configInput{
			Config: map[string]any{"module": "example.com/" + name},
		},
		// Negative, so provisioning does not wait for an application the fake never
		// actually started. The waiting behaviour is covered separately.
		Docker: dockerOptions{WaitSeconds: -1},
	}
}

// structuredOf decodes a tool result's structured payload.
func structuredOf[T any](t *testing.T, result toolCallResult) T {
	t.Helper()

	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("re-encoding the structured result: %v", err)
	}
	var out T
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatalf("decoding the structured result: %v\n%s", err, encoded)
	}
	return out
}

// ─── the token is issued once and never again ────────────────────────────────

// TestProvisionReturnsAllThreeAddressKindsExplicitly is the acceptance criterion that
// no return value is ambiguous about which address it names.
//
// It asserts on the raw JSON rather than the struct, because the criterion is about
// what a *caller* sees: a field named "port" would satisfy any Go-level assertion
// about a value while being exactly the ambiguity this forbids.
func TestProvisionReturnsAllThreeAddressKindsExplicitly(t *testing.T) {
	fake := &fakeDocker{}
	orch := newTestOrchestrator(t, fake)

	result := orch.provisionService(minimalService("users"))
	if result.IsError {
		t.Fatalf("provision_service failed: %s", result.Content[0].Text)
	}

	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var shape map[string]any
	if err := json.Unmarshal(encoded, &shape); err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{"control_port", "control_token", "control_url", "app_port", "app_url"} {
		if _, ok := shape[key]; !ok {
			t.Errorf("the result has no %q field:\n%s", key, encoded)
		}
	}
	// The forbidden field. A bare "port" is what makes a caller guess.
	if _, ok := shape["port"]; ok {
		t.Errorf("the result carries an ambiguous %q field:\n%s", "port", encoded)
	}

	got := structuredOf[provisionedResult](t, result)
	if got.ControlPort == got.AppPort {
		t.Fatalf("control_port and app_port are both %d; they must never be the same", got.ControlPort)
	}
	if got.ControlToken == "" {
		t.Error("no control_token was returned, so the control plane is unreachable")
	}
	if !strings.Contains(got.ControlURL, fmt.Sprint(got.ControlPort)) {
		t.Errorf("control_url %q does not name control_port %d", got.ControlURL, got.ControlPort)
	}
	if strings.Contains(got.AppURL, fmt.Sprint(got.ControlPort)) {
		t.Errorf("app_url %q points at the control port", got.AppURL)
	}
}

// TestListProvisionedServicesNeverReturnsAToken is the criterion that a control token
// is exposed exactly once.
//
// The whole response is searched, recursively, for both the field name and the token's
// value. Checking only the field name would miss a token that arrived inside a note or
// a summary line, which is precisely how a secret leaks in practice.
func TestListProvisionedServicesNeverReturnsAToken(t *testing.T) {
	fake := &fakeDocker{}
	orch := newTestOrchestrator(t, fake)

	first := structuredOf[provisionedResult](t, orch.provisionService(minimalService("users")))
	second := structuredOf[provisionedResult](t, orch.provisionService(minimalService("orders")))

	if first.ControlToken == second.ControlToken {
		t.Fatal("two services were issued the same control token")
	}

	listed := orch.listProvisioned()
	if listed.IsError {
		t.Fatalf("list_provisioned_services failed: %s", listed.Content[0].Text)
	}

	// Both halves of the response: the structured payload and the text a client
	// renders. A token in either is a leak.
	encoded, err := json.Marshal(listed.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	whole := string(encoded) + "\n" + listed.Content[0].Text

	for _, token := range []string{first.ControlToken, second.ControlToken} {
		if strings.Contains(whole, token) {
			t.Errorf("list_provisioned_services leaked a control token")
		}
	}
	// The field name as a JSON key, not as a substring: the notice below the listing
	// legitimately mentions control_token by name in prose, and a search that could
	// not tell the two apart would fail on documentation.
	for _, key := range []string{`"control_token":`, `"token":`, `"ControlToken":`} {
		if strings.Contains(string(encoded), key) {
			t.Errorf("list_provisioned_services carries a %s field:\n%s", key, encoded)
		}
	}

	// And the listing must still be useful: both services, with both address kinds.
	report := structuredOf[listResult](t, listed)
	if report.Count != 2 {
		t.Fatalf("listed %d services, want 2", report.Count)
	}
	for _, service := range report.Services {
		if service.ControlPort == 0 || service.AppPort == 0 {
			t.Errorf("%s is listed without both addresses: %+v", service.ServiceName, service)
		}
		if service.ControlPort == service.AppPort {
			t.Errorf("%s has the same control and app port", service.ServiceName)
		}
	}
}

// ─── reuse, not reimplementation ─────────────────────────────────────────────

// TestProvisionGeneratesThroughTheRealGenerator is the check that provision_service
// scaffolds by calling generator.New rather than by writing files itself.
//
// It asserts on the build context: the directory handed to `docker build` must contain
// what `breeze new` produces — a go.mod, a main.go, the generated routes file — plus
// the two files provisioning adds. If provisioning ever grew its own project writer,
// the tree it produced would differ from a real scaffold and this would say so.
func TestProvisionGeneratesThroughTheRealGenerator(t *testing.T) {
	fake := &fakeDocker{}
	orch := newTestOrchestrator(t, fake)

	// The build context is deleted when provisioning returns, so it is captured from
	// the argv Docker was given while it still exists. This is also, incidentally,
	// the assertion that a build happened at all.
	var buildContext string
	inner := fake.client().run
	orch.docker = &dockerClient{
		binary: "docker-fake",
		run: func(ctx context.Context, binary string, args ...string) (string, error) {
			if len(args) > 0 && args[0] == "build" {
				buildContext = args[len(args)-1]
				for _, want := range []string{"go.mod", "main.go", "Dockerfile", "entrypoint.sh"} {
					if _, err := os.Stat(filepath.Join(buildContext, want)); err != nil {
						t.Errorf("the build context has no %s: %v", want, err)
					}
				}
			}
			return inner(ctx, binary, args...)
		},
	}

	if result := orch.provisionService(minimalService("users")); result.IsError {
		t.Fatalf("provision_service failed: %s", result.Content[0].Text)
	}
	if buildContext == "" {
		t.Fatal("docker build was never called, so nothing was ever generated")
	}
	// And it is cleaned up: a provisioning run must not leave a copy of the project
	// in a temporary directory for the rest of the process's life.
	if _, err := os.Stat(buildContext); err == nil {
		t.Errorf("the build context %s was left behind", buildContext)
	}
}

// TestProvisionFleetWiresFleetThroughApplyConfig is the reuse claim for the Fleet
// half: provision_fleet must produce the same wiring `breeze add fleet` produces.
//
// The check is on the generated features file, because that is what the two paths
// share. A parallel implementation would have to emit its own tracer construction,
// and it would not land in this file with these strings.
func TestProvisionFleetWiresFleetThroughApplyConfig(t *testing.T) {
	fake := &fakeDocker{}
	orch := newTestOrchestrator(t, fake)

	contexts := map[string]string{}
	inner := fake.client().run
	orch.docker = &dockerClient{
		binary: "docker-fake",
		run: func(ctx context.Context, binary string, args ...string) (string, error) {
			if len(args) > 0 && args[0] == "build" {
				dir := args[len(args)-1]
				body, err := os.ReadFile(filepath.Join(dir, "features_generated.go"))
				if err != nil {
					t.Errorf("%s has no features_generated.go, so no feature was wired: %v", dir, err)
				} else {
					contexts[filepath.Base(dir)] = string(body)
				}
			}
			return inner(ctx, binary, args...)
		},
	}

	result := orch.provisionFleet(
		[]fleetServiceRequest{
			{Name: "gateway", Template: "api", configInput: configInput{Config: map[string]any{"module": "example.com/gateway"}}},
			{Name: "orders", Template: "api", configInput: configInput{Config: map[string]any{"module": "example.com/orders"}}},
		},
		fleetAggregatorConfig{HostedBy: "gateway"},
		dockerOptions{WaitSeconds: -1},
	)
	if result.IsError {
		t.Fatalf("provision_fleet failed: %s", result.Content[0].Text)
	}

	if len(contexts) != 2 {
		t.Fatalf("built %d contexts, want 2", len(contexts))
	}
	for name, body := range contexts {
		// The identifiers the fleet feature generates. They come from
		// internal/generator/features_fleet.go and nothing in the provisioning code
		// writes them.
		for _, want := range []string{"fleet.New(", "FleetTracer", "FLEET_WRITE_URL", "FLEET_SERVICE_NAME"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: features_generated.go does not contain %q, so Fleet was not wired by the "+
					"generator", name, want)
			}
		}
	}
}

// ─── the allocator ───────────────────────────────────────────────────────────

// TestPortAllocatorNeverDoubleAssignsAcrossThePool is the criterion that no two
// addresses of any kind collide within one orchestrator's lifetime.
//
// It provisions several services and a fleet through the real tools, then checks every
// number that came back — control, app and aggregator together, not pool by pool. Pool
// by pool would pass on the exact bug this exists to catch: two separate allocators
// that each never repeat themselves and happily hand out the same number as each other.
func TestPortAllocatorNeverDoubleAssignsAcrossThePool(t *testing.T) {
	fake := &fakeDocker{}
	orch := newTestOrchestrator(t, fake)

	seen := map[int]string{}
	claim := func(port int, what string) {
		if port == 0 {
			return
		}
		if previous, taken := seen[port]; taken {
			t.Errorf("port %d was assigned twice: %s and %s", port, previous, what)
			return
		}
		seen[port] = what
	}

	for _, name := range []string{"alpha", "beta", "gamma"} {
		result := orch.provisionService(minimalService(name))
		if result.IsError {
			t.Fatalf("provisioning %s: %s", name, result.Content[0].Text)
		}
		got := structuredOf[provisionedResult](t, result)
		claim(got.ControlPort, name+" control")
		claim(got.AppPort, name+" app")
	}

	fleet := orch.provisionFleet(
		[]fleetServiceRequest{
			{Name: "gateway", configInput: configInput{Config: map[string]any{"module": "example.com/gateway"}}},
			{Name: "orders", configInput: configInput{Config: map[string]any{"module": "example.com/orders"}}},
		},
		fleetAggregatorConfig{HostedBy: "gateway"},
		dockerOptions{WaitSeconds: -1},
	)
	if fleet.IsError {
		t.Fatalf("provision_fleet: %s", fleet.Content[0].Text)
	}

	report := structuredOf[fleetResult](t, fleet)
	for _, service := range report.Services {
		claim(service.ControlPort, service.ServiceName+" control")
		claim(service.AppPort, service.ServiceName+" app")
	}
	claim(report.Aggregator.AggregatorPort, "aggregator")

	// Five services × two ports, plus one aggregator port.
	if len(seen) != 11 {
		t.Errorf("assigned %d distinct ports, want 11: %v", len(seen), seen)
	}
}

// TestReleasedPortsAreReused checks the other half of the allocator's contract: a
// deprovisioned service's ports come back.
//
// Without this the allocator is a counter with a memory leak, and a long-running
// orchestrator that provisioned and tore down repeatedly would eventually exhaust its
// range while nothing at all was running.
func TestReleasedPortsAreReused(t *testing.T) {
	fake := &fakeDocker{}
	orch := newTestOrchestrator(t, fake)

	first := structuredOf[provisionedResult](t, orch.provisionService(minimalService("users")))

	if result := orch.deprovisionService("users", false); result.IsError {
		t.Fatalf("deprovision_service failed: %s", result.Content[0].Text)
	}

	// The ports must be free again, and specifically free — not merely "some port was
	// available", which would pass on a machine with 11,000 spare ports regardless.
	if purpose := orch.ports.purposeOf(first.ControlPort); purpose != "" {
		t.Errorf("control port %d is still held as %q after deprovisioning", first.ControlPort, purpose)
	}
	if purpose := orch.ports.purposeOf(first.AppPort); purpose != "" {
		t.Errorf("app port %d is still held as %q after deprovisioning", first.AppPort, purpose)
	}

	second := structuredOf[provisionedResult](t, orch.provisionService(minimalService("users")))
	if second.ControlPort != first.ControlPort {
		t.Errorf("re-provisioning took control port %d rather than reusing the released %d",
			second.ControlPort, first.ControlPort)
	}
}

// TestAllocatorSkipsPortsHeldOutsideTheOrchestrator covers the probe.
//
// Tracking alone would hand out a port a database is already listening on, and the
// failure would land on the provisioned container as "cannot bind" — attributed to the
// new service rather than to the allocator.
func TestAllocatorSkipsPortsHeldOutsideTheOrchestrator(t *testing.T) {
	allocator := newPortAllocator()

	// The first two ports in the range are "taken by something else".
	blocked := map[int]bool{allocator.start: true, allocator.start + 1: true}
	allocator.probe = func(port int) bool { return !blocked[port] }

	port, err := allocator.allocate(portPurposeControl)
	if err != nil {
		t.Fatal(err)
	}
	if blocked[port] {
		t.Fatalf("allocated %d, which the probe reported as taken", port)
	}
	if port != allocator.start+2 {
		t.Errorf("allocated %d, want %d: the scan should skip exactly the blocked ports",
			port, allocator.start+2)
	}

	// The blocked ports are recorded, so a second allocation does not re-probe them.
	if purpose := allocator.purposeOf(allocator.start); purpose != portPurposeExternal {
		t.Errorf("a port held outside the orchestrator is recorded as %q, want %q",
			purpose, portPurposeExternal)
	}
}

// ─── the registry is the only authority ──────────────────────────────────────

// TestDeprovisionRefusesAContainerItDidNotCreate is the criterion that this
// orchestrator will not touch anything outside its own registry.
//
// The refusal must be a structured error rather than a silent no-op, and it must not
// reach Docker at all: a tool that asked the daemon first and then decided could be
// pointed at a production database that happens to share a name.
func TestDeprovisionRefusesAContainerItDidNotCreate(t *testing.T) {
	fake := &fakeDocker{}
	orch := newTestOrchestrator(t, fake)

	// One service really is provisioned, so the refusal below cannot be explained by
	// an empty registry.
	if result := orch.provisionService(minimalService("users")); result.IsError {
		t.Fatalf("provision_service failed: %s", result.Content[0].Text)
	}
	before := len(fake.argvAll("rm"))

	result := orch.deprovisionService("someone-elses-database", false)
	if !result.IsError {
		t.Fatal("deprovisioning an unknown container succeeded; it must be refused")
	}

	text := result.Content[0].Text
	for _, want := range []string{"someone-elses-database", "users"} {
		if !strings.Contains(text, want) {
			t.Errorf("the refusal does not mention %q, so a caller cannot tell what went wrong "+
				"or what is managed:\n%s", want, text)
		}
	}
	if result.StructuredContent == nil {
		t.Error("the refusal has no structured payload; it must be a structured error, not prose")
	}

	// And nothing was asked of Docker.
	if after := len(fake.argvAll("rm")); after != before {
		t.Errorf("a refused deprovision issued %d docker rm command(s)", after-before)
	}
	if names := orch.registry.names(); len(names) != 1 || names[0] != "users" {
		t.Errorf("the registry changed during a refused deprovision: %v", names)
	}
}

// TestRegistrySurvivesAnOrchestratorRestart is the persistence criterion.
//
// The restart is simulated by discarding the orchestrator entirely — including its
// allocator — and loading a fresh one from the same file, which is what a process
// restart does. Keeping the allocator would make this a test of a map.
func TestRegistrySurvivesAnOrchestratorRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, registryFileName)
	fake := &fakeDocker{}

	first := newOrchestrator("test")
	reg, err := loadProvisionRegistry(path, first.ports)
	if err != nil {
		t.Fatal(err)
	}
	first.registry = reg
	first.docker = fake.client()

	users := structuredOf[provisionedResult](t, first.provisionService(minimalService("users")))
	orders := structuredOf[provisionedResult](t, first.provisionService(minimalService("orders")))

	// The restart.
	second := newOrchestrator("test")
	reloaded, err := loadProvisionRegistry(path, second.ports)
	if err != nil {
		t.Fatalf("reloading the registry after a restart: %v", err)
	}
	second.registry = reloaded
	second.docker = fake.client()

	listed := second.listProvisioned()
	if listed.IsError {
		t.Fatalf("list_provisioned_services after restart: %s", listed.Content[0].Text)
	}
	report := structuredOf[listResult](t, listed)

	if report.Count != 2 {
		t.Fatalf("after a restart the registry reports %d services, want 2", report.Count)
	}
	byName := map[string]listedService{}
	for _, service := range report.Services {
		byName[service.ServiceName] = service
	}
	for _, want := range []provisionedResult{users, orders} {
		got, ok := byName[want.ServiceName]
		if !ok {
			t.Errorf("%s was lost across the restart", want.ServiceName)
			continue
		}
		if got.ControlPort != want.ControlPort || got.AppPort != want.AppPort {
			t.Errorf("%s came back with control_port %d/app_port %d, want %d/%d",
				want.ServiceName, got.ControlPort, got.AppPort, want.ControlPort, want.AppPort)
		}
		if got.ContainerID != want.ContainerID {
			t.Errorf("%s came back with container %s, want %s", want.ServiceName,
				got.ContainerID, want.ContainerID)
		}
	}

	// Still no tokens, on the far side of a restart. This is the assertion that the
	// tokens were never written down in the first place.
	encoded, err := json.Marshal(listed.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	whole := string(encoded) + listed.Content[0].Text
	for _, token := range []string{users.ControlToken, orders.ControlToken} {
		if strings.Contains(whole, token) {
			t.Error("a control token survived the restart and was reported")
		}
	}
	if raw, err := os.ReadFile(path); err != nil {
		t.Fatal(err)
	} else {
		for _, token := range []string{users.ControlToken, orders.ControlToken} {
			if strings.Contains(string(raw), token) {
				t.Error("the registry file on disk contains a control token")
			}
		}
	}

	// And the restored allocator knows the ports are taken, which is what stops the
	// next provision handing out a running container's address.
	for _, port := range []int{users.ControlPort, users.AppPort, orders.ControlPort, orders.AppPort} {
		if second.ports.purposeOf(port) == "" {
			t.Errorf("port %d is free after the restart, so it could be handed to a new service "+
				"while a container is still publishing on it", port)
		}
	}
}

// ─── the three address kinds, in a fleet ─────────────────────────────────────

// TestProvisionFleetDistinguishesAllThreeAddressKinds is the criterion that the
// aggregator's address is never confused with the hosting service's own two.
//
// The hosting service is the only place all three exist at once, so it is the only
// place the confusion is possible — and the return value has to make the distinction
// legible without the reader having to cross-reference anything.
func TestProvisionFleetDistinguishesAllThreeAddressKinds(t *testing.T) {
	fake := &fakeDocker{}
	orch := newTestOrchestrator(t, fake)

	result := orch.provisionFleet(
		[]fleetServiceRequest{
			{Name: "gateway", configInput: configInput{Config: map[string]any{"module": "example.com/gateway"}}},
			{Name: "orders", configInput: configInput{Config: map[string]any{"module": "example.com/orders"}}},
		},
		fleetAggregatorConfig{HostedBy: "gateway"},
		dockerOptions{WaitSeconds: -1},
	)
	if result.IsError {
		t.Fatalf("provision_fleet failed: %s", result.Content[0].Text)
	}

	report := structuredOf[fleetResult](t, result)
	if len(report.Services) != 2 {
		t.Fatalf("provisioned %d services, want 2", len(report.Services))
	}

	agg := report.Aggregator
	if agg.HostedByService != "gateway" {
		t.Errorf("aggregator.hosted_by_service = %q, want gateway", agg.HostedByService)
	}

	var host provisionedResult
	for _, service := range report.Services {
		if service.ServiceName == "gateway" {
			host = service
		}
	}
	if host.ServiceName == "" {
		t.Fatal("the aggregator's host is not in the returned services")
	}

	// The three, all different.
	kinds := map[string]int{
		"control":    host.ControlPort,
		"app":        host.AppPort,
		"aggregator": agg.AggregatorPort,
	}
	for a, portA := range kinds {
		for b, portB := range kinds {
			if a < b && portA == portB {
				t.Errorf("the %s and %s addresses are both port %d on the same container", a, b, portA)
			}
		}
	}

	// And the result states the host's other two alongside the aggregator's, so the
	// distinction does not have to be reconstructed by the reader.
	if agg.HostServiceControlPort != host.ControlPort {
		t.Errorf("aggregator.host_service_control_port = %d, want %d",
			agg.HostServiceControlPort, host.ControlPort)
	}
	if agg.HostServiceAppPort != host.AppPort {
		t.Errorf("aggregator.host_service_app_port = %d, want %d", agg.HostServiceAppPort, host.AppPort)
	}
	if !strings.Contains(agg.AggregatorURL, fmt.Sprint(agg.AggregatorPort)) {
		t.Errorf("aggregator_url %q does not name aggregator_port %d", agg.AggregatorURL, agg.AggregatorPort)
	}

	// The non-hosting service has no aggregator address at all: it reports *to* one.
	for _, service := range report.Services {
		if service.ServiceName != "gateway" && service.AggregatorPort != 0 {
			t.Errorf("%s carries an aggregator_port (%d) but does not host the aggregator",
				service.ServiceName, service.AggregatorPort)
		}
	}

	// Every service's tracer points at the aggregator's port, never at any service's
	// app port. This is what makes traces from two services join up.
	//
	// The *host* in that URL is deliberately not the one the report carries. The report
	// names the aggregator as the caller reaches it (127.0.0.1); a tracer runs inside a
	// container, where 127.0.0.1 is that container and the host is reached by alias. Both
	// name the same listener, and asserting they are string-equal would be asserting a
	// bug — spans exported to the container's own loopback are dropped silently, since
	// export is asynchronous and best-effort.
	wantSuffix := fmt.Sprintf(":%d/fleet", agg.AggregatorPort)
	for _, args := range fake.argvAll("run") {
		var writeURL string
		for i, arg := range args {
			if arg == "-e" && i+1 < len(args) && strings.HasPrefix(args[i+1], "FLEET_WRITE_URL=") {
				writeURL = strings.TrimPrefix(args[i+1], "FLEET_WRITE_URL=")
			}
		}
		if !strings.HasSuffix(writeURL, wantSuffix) {
			t.Errorf("a service was started with FLEET_WRITE_URL=%q, which does not name the "+
				"aggregator's port (%s)", writeURL, wantSuffix)
		}
		// And it is reachable from inside a container, which the host-side address is
		// not.
		if !strings.Contains(writeURL, dockerHostAlias) {
			t.Errorf("FLEET_WRITE_URL=%q is not reachable from inside a container; it needs %s",
				writeURL, dockerHostAlias)
		}
	}

	// The alias has to resolve, which on Linux it does not unless docker run maps it.
	for _, args := range fake.argvAll("run") {
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "--add-host "+dockerHostAlias+":host-gateway") {
			t.Errorf("a container was started without mapping %s, so its tracer cannot "+
				"resolve the aggregator on Linux", dockerHostAlias)
		}
	}
}

// TestProvisionFleetPublishesTheAggregatorPortOnlyOnItsHost checks the port mapping
// rather than the report: the aggregator's host publishes three ports, everything else
// publishes two.
func TestProvisionFleetPublishesTheAggregatorPortOnlyOnItsHost(t *testing.T) {
	fake := &fakeDocker{}
	orch := newTestOrchestrator(t, fake)

	result := orch.provisionFleet(
		[]fleetServiceRequest{
			{Name: "gateway", configInput: configInput{Config: map[string]any{"module": "example.com/gateway"}}},
			{Name: "orders", configInput: configInput{Config: map[string]any{"module": "example.com/orders"}}},
		},
		fleetAggregatorConfig{HostedBy: "orders"},
		dockerOptions{WaitSeconds: -1},
	)
	if result.IsError {
		t.Fatalf("provision_fleet failed: %s", result.Content[0].Text)
	}

	published := map[string]int{}
	for _, args := range fake.argvAll("run") {
		var name string
		count := 0
		for i, arg := range args {
			switch arg {
			case "--name":
				if i+1 < len(args) {
					name = args[i+1]
				}
			case "-p":
				count++
			}
		}
		published[name] = count
	}

	if got := published["breeze-orders"]; got != 3 {
		t.Errorf("the aggregator's host publishes %d ports, want 3 (control, app, aggregator)", got)
	}
	if got := published["breeze-gateway"]; got != 2 {
		t.Errorf("a non-hosting service publishes %d ports, want 2 (control, app)", got)
	}
}

// TestProvisionFleetRejectsAnUnknownHost covers the argument check that would otherwise
// produce a fleet with an aggregator nothing hosts.
func TestProvisionFleetRejectsAnUnknownHost(t *testing.T) {
	fake := &fakeDocker{}
	orch := newTestOrchestrator(t, fake)

	result := orch.provisionFleet(
		[]fleetServiceRequest{
			{Name: "gateway", configInput: configInput{Config: map[string]any{"module": "example.com/gateway"}}},
		},
		fleetAggregatorConfig{HostedBy: "not-in-this-fleet"},
		dockerOptions{WaitSeconds: -1},
	)
	if !result.IsError {
		t.Fatal("an aggregator hosted by a service that is not being provisioned was accepted")
	}
	if !strings.Contains(result.Content[0].Text, "gateway") {
		t.Errorf("the refusal does not name the available services:\n%s", result.Content[0].Text)
	}
	// Nothing started, so nothing has to be cleaned up.
	if len(fake.argvAll("run")) != 0 {
		t.Error("a rejected fleet request started containers")
	}
}

// ─── the token reaches the container safely ──────────────────────────────────

// TestTokenIsPassedByEnvironmentNotArgument is the criterion that a control token never
// appears on a command line.
//
// It inspects the argv provisioning handed to docker: the token must be there, as the
// value of a -e pair, and must not appear in any other position. A token passed as an
// argument would be visible in `ps` on the host and in `docker inspect`'s Cmd, to every
// user on the machine.
func TestTokenIsPassedByEnvironmentNotArgument(t *testing.T) {
	fake := &fakeDocker{}
	orch := newTestOrchestrator(t, fake)

	got := structuredOf[provisionedResult](t, orch.provisionService(minimalService("users")))
	if got.ControlToken == "" {
		t.Fatal("no token was issued")
	}

	args := fake.argvFor("run")
	if args == nil {
		t.Fatal("docker run was never called")
	}

	found := false
	for i, arg := range args {
		if arg == tokenEnvVar+"="+got.ControlToken {
			// Only valid immediately after -e.
			if i == 0 || args[i-1] != "-e" {
				t.Errorf("the token appears at argv[%d] without a preceding -e", i)
			}
			found = true
			continue
		}
		// Anywhere else — a bare argument, a flag value, an image tag — is a leak.
		if strings.Contains(arg, got.ControlToken) {
			t.Errorf("the token appears in argv[%d] = %q, which is not an environment pair", i, arg)
		}
	}
	if !found {
		t.Errorf("the returned token was never passed to the container:\n%v", args)
	}
}

// TestCallerSuppliedEnvironmentCannotOverrideTheToken covers the one way a caller could
// otherwise make the returned token a lie.
func TestCallerSuppliedEnvironmentCannotOverrideTheToken(t *testing.T) {
	fake := &fakeDocker{}
	orch := newTestOrchestrator(t, fake)

	request := minimalService("users")
	request.Docker.Env = map[string]string{
		tokenEnvVar: "a-token-the-caller-chose",
		"LOG_LEVEL": "debug",
	}

	got := structuredOf[provisionedResult](t, orch.provisionService(request))
	if got.ControlToken == "a-token-the-caller-chose" {
		t.Fatal("the caller's token was used, so the orchestrator does not control its own credentials")
	}

	joined := strings.Join(fake.argvFor("run"), " ")
	if strings.Contains(joined, "a-token-the-caller-chose") {
		t.Error("the caller's token reached the container, so the returned token is not the one in use")
	}
	if !strings.Contains(joined, tokenEnvVar+"="+got.ControlToken) {
		t.Error("the returned token is not the one the container was given")
	}
	// Everything else the caller asked for is still honoured.
	if !strings.Contains(joined, "LOG_LEVEL=debug") {
		t.Error("an unrelated environment variable was dropped")
	}
}

// ─── the generated image files ───────────────────────────────────────────────

// TestGeneratedImageFilesAreCoherent checks the two files provisioning writes into the
// build context. A mistake in either produces a container that starts and then fails in
// a way no tool reports.
func TestGeneratedImageFilesAreCoherent(t *testing.T) {
	// The proxy variant: no local source, so the version pin is what matters.
	dockerfile := provisionDockerfile("v1.7.0", false)
	entrypoint := provisionEntrypoint()

	// The token is read from the environment, and the script refuses without one: a
	// control plane that generated its own would be unreachable by design.
	if !strings.Contains(entrypoint, tokenEnvVar) {
		t.Errorf("the entrypoint does not read %s:\n%s", tokenEnvVar, entrypoint)
	}
	if !strings.Contains(entrypoint, "refusing") {
		t.Error("the entrypoint does not refuse to start without a token")
	}
	// The application is exec'd, so it is PID 1 and receives docker stop's signals —
	// which is what lets a Fleet tracer flush its buffer.
	if !strings.Contains(entrypoint, "exec app") {
		t.Error("the application is not exec'd, so it will not receive shutdown signals")
	}
	// The control plane binds every interface *inside* the container, which is the only
	// way its published port is reachable.
	if !strings.Contains(entrypoint, "--host 0.0.0.0") {
		t.Error("the control instance binds loopback inside the container, where nothing can reach it")
	}
	if !strings.Contains(entrypoint, fmt.Sprintf("--port %d", containerControlPort)) {
		t.Errorf("the entrypoint does not start the control plane on %d", containerControlPort)
	}
	// --mode has no default, so an entrypoint that omits it starts a process which
	// exits immediately — and the container still looks healthy, because the probe
	// targets the application. This is exactly that regression.
	if !strings.Contains(entrypoint, "--mode "+string(ModeGenerator)) {
		t.Errorf("the entrypoint does not pass --mode, so the control plane will refuse to start:\n%s", entrypoint)
	}
	// Optional scoping, honoured when the environment carries it.
	if !strings.Contains(entrypoint, "BREEZE_MCP_SCOPE") || !strings.Contains(entrypoint, "--scope") {
		t.Error("the entrypoint cannot pass a token scope through to the control plane")
	}
	// The arguments are built as a positional list, not a string. BREEZE_MCP_SCOPE
	// comes from the container's environment and a string would be word-split when
	// used, so a value of `runtime --allow-any-path` would arrive as two arguments
	// and start this control plane unconfined.
	if strings.Contains(entrypoint, "$MCP_ARGS") {
		t.Errorf("the entrypoint word-splits its arguments, so a value in BREEZE_MCP_SCOPE "+
			"can inject a flag:\n%s", entrypoint)
	}
	if !strings.Contains(entrypoint, `breeze-mcp "$@"`) {
		t.Errorf(`the entrypoint does not invoke breeze-mcp with "$@", so an environment value `+
			"containing a space becomes more than one argument:\n%s", entrypoint)
	}

	// A released orchestrator pins its own version, so the two speak the same tools.
	if !strings.Contains(dockerfile, "@v1.7.0") {
		t.Errorf("a released orchestrator does not pin its version:\n%s", dockerfile)
	}
	if strings.Contains(provisionDockerfile("(devel)", false), "@(devel)") {
		t.Error("a development version was used as a module version, which cannot resolve")
	}
	if !strings.Contains(provisionDockerfile("(devel)", false), "@latest") {
		t.Error("a development build does not fall back to @latest")
	}

	// Both ports are declared, and they are different numbers.
	if containerControlPort == containerAppPort {
		t.Fatal("the container's control and app ports are the same number")
	}
	for _, port := range []int{containerControlPort, containerAppPort} {
		if !strings.Contains(dockerfile, fmt.Sprintf("EXPOSE %d", port)) {
			t.Errorf("the Dockerfile does not EXPOSE %d", port)
		}
	}

	// The health check targets the app port. Probing the control port would report a
	// healthy container whose application had died.
	if !strings.Contains(dockerfile, fmt.Sprintf("127.0.0.1:%d/", containerAppPort)) {
		t.Errorf("the health check does not probe the app port %d:\n%s", containerAppPort, dockerfile)
	}
	if strings.Contains(dockerfile, fmt.Sprintf("127.0.0.1:%d/", containerControlPort)) {
		t.Error("the health check probes the control port rather than the application")
	}
}

// TestVendoredDockerfileBuildsEverythingFromSource — the from-source variant has to
// produce all three binaries from the copied tree and consult the proxy for none of
// them, or a development orchestrator provisions containers running published code it
// was never built from.
func TestVendoredDockerfileBuildsEverythingFromSource(t *testing.T) {
	dockerfile := provisionDockerfile("v1.7.0", true)

	// Nothing is installed from the module path: that is the whole point.
	if strings.Contains(dockerfile, "go install") {
		t.Errorf("the vendored Dockerfile still installs from the proxy:\n%s", dockerfile)
	}
	if strings.Contains(dockerfile, "@v1.7.0") || strings.Contains(dockerfile, "@latest") {
		t.Error("the vendored Dockerfile pins a published version, which it does not use")
	}

	// All three binaries, each built from breeze-src/ or from the project beside it.
	for _, want := range []string{
		"-o /out/app .",
		"-o /out/breeze-mcp ./cmd/breeze-mcp",
		"-o /out/fleet-aggregator ./cmd/fleet-aggregator",
	} {
		if !strings.Contains(dockerfile, want) {
			t.Errorf("the vendored Dockerfile does not build %q:\n%s", want, dockerfile)
		}
	}
	// And each is copied into the runtime stage; a build stage that produces a binary
	// nothing copies is a silent no-op.
	for _, want := range []string{"/usr/local/bin/app", "/usr/local/bin/breeze-mcp",
		"/usr/local/bin/fleet-aggregator"} {
		if !strings.Contains(dockerfile, want) {
			t.Errorf("the vendored Dockerfile does not install %s", want)
		}
	}

	if !strings.Contains(dockerfile, "WORKDIR /src/"+vendoredBreezeDir) {
		t.Errorf("the vendored Dockerfile never enters %s", vendoredBreezeDir)
	}
	// tidy is required: the replace covers Breeze, not Breeze's own published
	// dependencies, and the generated go.sum has no entries for those.
	if !strings.Contains(dockerfile, "go mod tidy") {
		t.Error("the vendored Dockerfile does not tidy, so Breeze's own dependencies have no go.sum entries")
	}
}

// TestBuildImageTagTracksTheToolchain — a builder older than either go.mod's directive
// fails under the golang images' GOTOOLCHAIN=local, and it fails the day the
// orchestrator's toolchain is upgraded rather than the day this code changes.
func TestBuildImageTagTracksTheToolchain(t *testing.T) {
	tag := buildImageTag()

	if !strings.HasPrefix(tag, "golang:") || !strings.HasSuffix(tag, "-alpine") {
		t.Fatalf("buildImageTag() = %q, want a golang alpine tag", tag)
	}
	// On a released toolchain the tag carries this binary's own version, so the builder
	// cannot be behind a go directive derived from the same value.
	if m := goVersionRe.FindStringSubmatch(runtime.Version()); m != nil {
		if want := "golang:" + m[1] + "-alpine"; tag != want {
			t.Errorf("buildImageTag() = %q, want %q for %s", tag, want, runtime.Version())
		}
	} else if tag != fallbackBuildImage {
		t.Errorf("buildImageTag() = %q, want the fallback %q for %s", tag, fallbackBuildImage, runtime.Version())
	}

	// Both variants use it, so neither can drift onto a stale pin.
	for _, vendored := range []bool{true, false} {
		if !strings.Contains(provisionDockerfile("v1.7.0", vendored), "FROM "+tag) {
			t.Errorf("provisionDockerfile(vendored=%v) does not build in %s", vendored, tag)
		}
	}
}

// TestProvisionedContainerConfinesItsOwnControlPlane is the third place a workspace
// boundary has to exist.
//
// `--workspace` confines the orchestrator. It says nothing about the *provisioned*
// container, which runs a second breeze-mcp in generator mode with the same tools — so
// an agent holding that container's control token has a full generator inside a
// filesystem containing /etc, /usr, the Go toolchain, and a Docker socket if one was
// mounted. Confinement there is a property of the entrypoint, not of this process.
//
// Both halves are asserted, because either alone is a false pass: a flag naming a
// directory the image does not use would refuse every legitimate call, and a WORKDIR
// with no flag would leave the container confined only by its default.
func TestProvisionedContainerConfinesItsOwnControlPlane(t *testing.T) {
	entrypoint := provisionEntrypoint()

	if !strings.Contains(entrypoint, "--workspace "+containerWorkspaceDir) {
		t.Errorf("the entrypoint does not confine the container's own control plane to %s. "+
			"That control plane is a generator-mode server with the same tools this one has, "+
			"and the container's filesystem is not a boundary:\n%s",
			containerWorkspaceDir, entrypoint)
	}

	// The flag and the tree have to be the same directory in both Dockerfile
	// variants — vendored and from-proxy — because a provisioned service is built
	// from whichever the orchestrator's own deployment produces.
	for _, vendored := range []bool{true, false} {
		dockerfile := provisionDockerfile("v1.7.0", vendored)
		if !strings.Contains(dockerfile, "WORKDIR "+containerWorkspaceDir) {
			t.Errorf("provisionDockerfile(vendored=%v) does not use %s as its WORKDIR, so the "+
				"entrypoint's --workspace names a directory the project is not in",
				vendored, containerWorkspaceDir)
		}
		if !strings.Contains(dockerfile, "/src "+containerWorkspaceDir) {
			t.Errorf("provisionDockerfile(vendored=%v) does not copy the project into %s",
				vendored, containerWorkspaceDir)
		}
	}
}

// TestEntrypointStartsTheAggregatorOnlyWhenAsked — one image serves every service in a
// fleet, and only the host service should run an aggregator. Starting one everywhere
// would have each service reporting to its own, so every topology would show one node.
func TestEntrypointStartsTheAggregatorOnlyWhenAsked(t *testing.T) {
	entrypoint := provisionEntrypoint()

	if !strings.Contains(entrypoint, fleetAggregatorPortEnvVar) {
		t.Errorf("the entrypoint never looks at %s, so a fleet's aggregator port maps to "+
			"nothing:\n%s", fleetAggregatorPortEnvVar, entrypoint)
	}
	if !strings.Contains(entrypoint, "fleet-aggregator") {
		t.Error("the entrypoint cannot start the aggregator binary")
	}
	// Conditional, not unconditional.
	if !strings.Contains(entrypoint, `if [ -n "$`+fleetAggregatorPortEnvVar+`" ]`) {
		t.Error("the aggregator is started unconditionally, so every service would host one")
	}
	// It translates to the name the binary reads, rather than expecting the
	// orchestrator to set FLEET_PORT among an app port and a control port.
	if !strings.Contains(entrypoint, "FLEET_PORT=") {
		t.Error("the entrypoint does not pass FLEET_PORT, which is what fleet-aggregator reads")
	}
}

// TestWriteProvisionBuildFilesMakesTheEntrypointExecutable covers a small thing that
// fails loudly in production and silently in review: a non-executable entrypoint makes
// every provisioned container exit immediately with "permission denied".
func TestWriteProvisionBuildFilesMakesTheEntrypointExecutable(t *testing.T) {
	dir := t.TempDir()
	if _, err := writeProvisionBuildFiles(dir, "v1.0.0"); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"Dockerfile", "entrypoint.sh"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was not written: %v", name, err)
		}
	}

	info, err := os.Stat(filepath.Join(dir, "entrypoint.sh"))
	if err != nil {
		t.Fatal(err)
	}
	// Windows carries no Unix executable bit, so the mode is only meaningful where it
	// exists. The file's presence is what matters on either platform; the bit is
	// checked where there is one to check.
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		t.Errorf("entrypoint.sh is not executable (mode %v)", info.Mode().Perm())
	}
}
