package mcp

// docker_names_test.go — the adversarial tests for the provisioning identifiers.
//
// These are written as the attacker's inputs, not as the validator's cases. Each one is
// a value that could appear in a provision_service or provision_fleet argument, and the
// assertion is that it is refused — with, where it matters, a message that names the
// actual hazard rather than a character class.
//
// Nothing here concerns shell escaping. runDockerCommand builds an argument array and
// calls exec.CommandContext, so `; rm -rf /` as a container name reaches Docker as one
// literal argument. The three hazards that survive that are what these tests cover:
// argument smuggling, registry redirection, and settings the request must not choose
// for the container.

import (
	"fmt"
	"strings"
	"testing"
)

// TestDockerNamesRefuseArgumentSmuggling is the case with the worst consequence and the
// smallest input.
//
// `docker run -d --name <value> <image>` with a value of "--privileged" produces
// `docker run -d --name --privileged <image>`, and Docker's flag parser takes
// --privileged as the flag it looks like: a container with host-level capabilities,
// obtained through a package that never offered a privileged option. The same shape
// applies to every operand Docker takes.
func TestDockerNamesRefuseArgumentSmuggling(t *testing.T) {
	smuggled := []string{
		"--privileged",
		"-v",
		"--network=host",
		"--pid=host",
		"-v=/:/host",
		"--user=0:0",
		"--cap-add=SYS_ADMIN",
		"-",
	}

	for _, value := range smuggled {
		if err := validateContainerName(value); err == nil {
			t.Errorf("validateContainerName(%q) was accepted; Docker reads a leading dash as a "+
				"flag, so this lands in argv as one", value)
		}
		if err := validateServiceName(value); err == nil {
			t.Errorf("validateServiceName(%q) was accepted", value)
		}
		if err := validateImageTag(value); err == nil {
			t.Errorf("validateImageTag(%q) was accepted", value)
		}
	}

	// The message has to name the hazard. An operator told "invalid character" looks
	// for a typo; one told about flag parsing understands why a legal-looking string
	// was refused.
	err := validateContainerName("--privileged")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "flag") {
		t.Errorf("the refusal does not explain that Docker would read it as a flag: %v", err)
	}
}

// TestImageTagRefusesARegistryHost covers the redirection case.
//
// Docker treats the first path segment as a registry host when it contains "." or ":",
// or when it is exactly "localhost". Combined with skip_build — which skips the build
// and runs the tag as given — a tag naming a registry makes the orchestrator pull and
// execute an image this host never built.
func TestImageTagRefusesARegistryHost(t *testing.T) {
	hostile := []string{
		"evil.example/app",
		"evil.example/app:latest",
		"registry.internal:5000/app:1",
		"localhost/app",
		"localhost:5000/app:dev",
		"1.2.3.4/app",
		"ghcr.io/someone/thing:main",
	}

	for _, tag := range hostile {
		if err := validateImageTag(tag); err == nil {
			t.Errorf("validateImageTag(%q) was accepted; with skip_build this runs an image "+
				"pulled from a host named in a tool argument", tag)
		}
	}

	err := validateImageTag("evil.example/app:latest")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "registry") {
		t.Errorf("the refusal does not name registry redirection as the reason: %v", err)
	}
}

// TestImageTagAcceptsALocalReference is the necessary other half: the orchestrator's own
// default is "breeze-provisioned/<name>", so a check that refused every slash would
// refuse the values this package itself produces.
func TestImageTagAcceptsALocalReference(t *testing.T) {
	for _, tag := range []string{
		"breeze-provisioned/orders",
		"breeze-provisioned/orders:latest",
		"orders",
		"orders:v1.2.3",
		"team-a/service_b/orders:2026-01-01",
	} {
		if err := validateImageTag(tag); err != nil {
			t.Errorf("validateImageTag(%q) was refused: %v — this is the shape of a locally "+
				"built tag, including the orchestrator's own default", tag, err)
		}
	}
}

// TestServiceNameRefusesPathTraversal covers the second life of a service name.
//
// It is not only a container name: generateProject creates a directory called it, so a
// name of "../../etc" would place a generated project outside the workspace by a path
// the confinement check never sees — the name is joined onto an already-approved root.
func TestServiceNameRefusesPathTraversal(t *testing.T) {
	for _, name := range []string{
		"../escape",
		"..",
		"../../etc",
		"a/../..",
		`..\escape`,
		"orders/../../../tmp",
		"orders/sub",
		`orders\sub`,
		".hidden",
	} {
		if err := validateServiceName(name); err == nil {
			t.Errorf("validateServiceName(%q) was accepted; the name is also the directory the "+
				"project is generated into", name)
		}
	}
}

// TestServiceNameAcceptsARealName is the other half. A check that refused every
// plausible service name would be discovered in production rather than here.
func TestServiceNameAcceptsARealName(t *testing.T) {
	for _, name := range []string{"orders", "orders-api", "orders_api", "gateway2", "a", "svc.v1"} {
		if err := validateServiceName(name); err != nil {
			t.Errorf("validateServiceName(%q) was refused: %v", name, err)
		}
	}
}

// TestDockerEnvRefusesTheScopeVariable is a privilege question rather than an injection
// one.
//
// The provisioned entrypoint honours BREEZE_MCP_SCOPE, so it decides what the new
// container's control plane will serve. A caller who sets it through docker.env is
// choosing that container's capability set — which is the orchestrator's decision, and
// the reason BREEZE_MCP_TOKEN is already dropped in prepare.
func TestDockerEnvRefusesTheScopeVariable(t *testing.T) {
	for _, key := range []string{"BREEZE_MCP_SCOPE", "breeze_mcp_scope", "Breeze_Mcp_Scope"} {
		err := validateDockerEnv(map[string]string{key: "generation,provisioning"})
		if err == nil {
			t.Errorf("docker.env accepted %q; it decides the provisioned container's capability "+
				"set, which the request must not choose", key)
			continue
		}
		if !strings.Contains(err.Error(), "BREEZE_MCP_SCOPE") {
			t.Errorf("the refusal for %q does not name the variable: %v", key, err)
		}
	}
}

// TestFleetServiceCountIsBounded is a denial-of-service bound, not a naming rule.
//
// Each service in a fleet is a generated project, a `docker build` with a 15-minute
// timeout, three or four allocated ports and a running container — and provisioning is
// sequential. A request for two hundred services is a request to exhaust the port range,
// the disk and the Docker daemon from one tool call while holding the orchestrator for
// hours.
//
// The refusal has to happen before the first side effect, which is why this test uses an
// orchestrator with no Docker available: if the bound were checked inside the loop that
// provisions, the error returned would be Docker's rather than this one, and the first
// services would already have been built by the time the request was refused.
func TestFleetServiceCountIsBounded(t *testing.T) {
	orch := &orchestrator{ports: newPortAllocator()}

	services := make([]fleetServiceRequest, maxFleetServices+1)
	for i := range services {
		services[i] = fleetServiceRequest{Name: fmt.Sprintf("svc%d", i)}
	}

	result := orch.provisionFleet(services, fleetAggregatorConfig{}, dockerOptions{WaitSeconds: -1})
	if !result.IsError {
		t.Fatalf("provision_fleet accepted %d services; the limit is %d",
			len(services), maxFleetServices)
	}

	message := result.Content[0].Text
	if !strings.Contains(message, "limit") {
		t.Errorf("provision_fleet refused %d services but not for the documented reason: %s",
			len(services), message)
	}
	// The count check must come before the registry and Docker are consulted, or the
	// bound is not a bound: the refusal would arrive after work had already started.
	if strings.Contains(strings.ToLower(message), "docker") &&
		!strings.Contains(message, "docker build") {
		t.Errorf("the refusal came from the Docker check rather than the count bound, so the "+
			"limit is enforced after provisioning begins: %s", message)
	}
}

// TestFleetRequiresAtLeastOneService is the other end of the same argument. An empty
// list is a caller error rather than a no-op fleet, and reporting it says so.
func TestFleetRequiresAtLeastOneService(t *testing.T) {
	orch := &orchestrator{ports: newPortAllocator()}

	result := orch.provisionFleet(nil, fleetAggregatorConfig{}, dockerOptions{WaitSeconds: -1})
	if !result.IsError {
		t.Fatal("provision_fleet accepted an empty services list")
	}
	if !strings.Contains(result.Content[0].Text, "services") {
		t.Errorf("the refusal does not name the argument: %s", result.Content[0].Text)
	}
}

// TestDockerEnvRefusesMalformedNames covers the two shapes that change what Docker
// receives rather than merely being ugly.
//
// A name containing "=" redefines a different variable, because Docker splits the pair
// on the first "=": key `FOO=BAR` with value `BAZ` becomes FOO=BAR=BAZ. A name starting
// with "-" is argument smuggling again, in the -e operand this time.
func TestDockerEnvRefusesMalformedNames(t *testing.T) {
	cases := map[string]string{
		"FOO=BAR":      "an = in the name redefines a different variable",
		"-e":           "a leading dash is read as a flag",
		"--privileged": "a leading dash is read as a flag",
		"":             "an empty name is not a variable",
		"   ":          "a blank name is not a variable",
		"9LIVES":       "a leading digit is not a legal variable name",
		"HAS SPACE":    "a space is not legal in a variable name",
		"HAS\nNEWLINE": "a newline is not legal in a variable name",
	}

	for key, why := range cases {
		if err := validateDockerEnv(map[string]string{key: "x"}); err == nil {
			t.Errorf("docker.env accepted the name %q; %s", key, why)
		}
	}
}

// TestDockerEnvBoundsValueSize is a denial-of-service bound, not a correctness one.
//
// Linux allows roughly 128 KiB for a process's whole environment. A value approaching
// that makes the container fail to exec with a message about argument lists — a failure
// that names neither the variable nor the request that caused it. Refusing above 32 KiB
// turns an unexplainable container failure into an answer.
func TestDockerEnvBoundsValueSize(t *testing.T) {
	oversized := strings.Repeat("x", maxDockerEnvValueLength+1)
	err := validateDockerEnv(map[string]string{"CONFIG": oversized})
	if err == nil {
		t.Fatalf("docker.env accepted a %d-byte value; the limit is %d",
			len(oversized), maxDockerEnvValueLength)
	}
	if !strings.Contains(err.Error(), "CONFIG") {
		t.Errorf("the refusal does not name the variable that caused it: %v", err)
	}

	// At the limit exactly, so the bound is a limit rather than an approximation.
	atLimit := strings.Repeat("x", maxDockerEnvValueLength)
	if err := validateDockerEnv(map[string]string{"CONFIG": atLimit}); err != nil {
		t.Errorf("docker.env refused a value of exactly the limit (%d bytes): %v",
			maxDockerEnvValueLength, err)
	}
}

// TestDockerEnvAcceptsOrdinaryConfiguration states what the check must not break:
// passing configuration to a provisioned service is the feature docker.env exists for.
func TestDockerEnvAcceptsOrdinaryConfiguration(t *testing.T) {
	env := map[string]string{
		"BREEZE_DATABASE_URL": "postgres://user:pass@db:5432/app?sslmode=disable",
		"LOG_LEVEL":           "debug",
		"_LEADING_UNDERSCORE": "ok",
		"WITH9DIGITS":         "ok",
		// Values are not name-checked, and must not be: a shell metacharacter in a
		// value is not a hazard here, because no shell sees it and the entrypoint
		// never expands an arbitrary variable.
		"MOTD": "hello; $(whoami) `id` && echo done",
		"JSON": `{"a": 1, "b": ["x"]}`,
	}
	if err := validateDockerEnv(env); err != nil {
		t.Errorf("docker.env refused an ordinary configuration map: %v", err)
	}
}

// TestDockerHostRefusesAValueThatWouldLie covers the field whose risk is not an escape.
//
// docker.host is never passed to Docker — runContainer always publishes on 127.0.0.1 —
// it is recorded in the registry and interpolated into the URLs these tools return. So a
// path or a URL here produces an app_url pointing somewhere the service is not, which
// sends every later tool call to a host the caller did not choose.
func TestDockerHostRefusesAValueThatWouldLie(t *testing.T) {
	for _, host := range []string{
		"http://evil.example",
		"127.0.0.1/../etc",
		"host with space",
		"host\nnewline",
		`host\path`,
		"-h",
	} {
		if err := validateDockerHost(host); err == nil {
			t.Errorf("docker.host accepted %q; it is interpolated into the URLs this tool "+
				"returns, so it must be a bare host", host)
		}
	}

	for _, host := range []string{"", "127.0.0.1", "localhost", "docker.internal", "10.0.0.4"} {
		if err := validateDockerHost(host); err != nil {
			t.Errorf("docker.host refused %q: %v", host, err)
		}
	}
}

// TestValidateDockerOptionsChecksEveryField is the aggregate.
//
// It exists because the risk is a *forgotten* field: provision_service and
// provision_fleet both call this one function, and if it skipped a field the individual
// tests above would still pass while the tools validated nothing. Each case sets exactly
// one hostile field, so a missing check fails here by name.
func TestValidateDockerOptionsChecksEveryField(t *testing.T) {
	cases := []struct {
		field string
		opts  dockerOptions
	}{
		{"image_tag", dockerOptions{ImageTag: "evil.example/app"}},
		{"image_tag (dash)", dockerOptions{ImageTag: "--privileged"}},
		{"container_name", dockerOptions{ContainerName: "--privileged"}},
		{"env", dockerOptions{Env: map[string]string{scopeEnvVar: "provisioning"}}},
		{"host", dockerOptions{Host: "http://evil.example"}},
	}

	for _, tc := range cases {
		if err := validateDockerOptions(tc.opts); err == nil {
			t.Errorf("validateDockerOptions did not check %s; provision_service and "+
				"provision_fleet both rely on this one call", tc.field)
		}
	}

	// An empty options struct is what a caller who set nothing sends, and every field
	// then means "use the default".
	if err := validateDockerOptions(dockerOptions{}); err != nil {
		t.Errorf("validateDockerOptions refused an empty options struct: %v", err)
	}
}
