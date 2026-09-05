package mcp

// docker_args_test.go — the adversarial tests for the host-access boundary.
//
// The claim under test is narrow and worth stating exactly: nothing an MCP caller can
// send makes this orchestrator hand a container a host mount, a privileged flag, a
// shared namespace, host networking, or a chosen entrypoint. That claim has two
// halves, and they fail differently.
//
// The request half — a JSON field asking for it — is refused at decode. The emission
// half — this package building such an argv for any reason — is refused at exec. A
// test for only the first would pass a version of this package that mounted the host
// on every provision, because no caller asked for it.

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestProvisionRefusesEveryHostAccessOption is the request half.
//
// Each of these is a field a caller might plausibly write, because each is a real
// Docker capability with an obvious name. None is a field on dockerOptions, and the
// point of the test is that the *absence* is enforced rather than incidental: a JSON
// decoder that ignored unknown fields would drop all of them silently and this test
// would fail, which is exactly the behaviour it exists to prevent.
func TestProvisionRefusesEveryHostAccessOption(t *testing.T) {
	for _, tc := range []struct {
		name string
		json string
	}{
		{"privileged", `{"privileged": true}`},
		{"volumes", `{"volumes": ["/:/host"]}`},
		{"volume", `{"volume": "/etc:/etc:ro"}`},
		{"mounts", `{"mounts": [{"source": "/", "target": "/host"}]}`},
		{"binds", `{"binds": ["/var/run/docker.sock:/var/run/docker.sock"]}`},
		{"network_mode", `{"network_mode": "host"}`},
		{"network", `{"network": "host"}`},
		{"cap_add", `{"cap_add": ["SYS_ADMIN"]}`},
		{"devices", `{"devices": ["/dev/sda"]}`},
		{"security_opt", `{"security_opt": ["seccomp=unconfined"]}`},
		{"pid_mode", `{"pid_mode": "host"}`},
		{"ipc_mode", `{"ipc_mode": "host"}`},
		{"user", `{"user": "0:0"}`},
		{"entrypoint", `{"entrypoint": "/bin/sh"}`},
		{"command", `{"command": ["sh", "-c", "id"]}`},
		{"docker_host", `{"docker_host": "tcp://evil.example:2375"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var opts dockerOptions
			err := json.Unmarshal([]byte(tc.json), &opts)
			if err == nil {
				t.Fatalf("docker %s was accepted. Either this is now a real option — in which "+
					"case it grants a container access to this host and needs its own review — "+
					"or unknown fields are being dropped silently, which makes every request of "+
					"this shape look honoured", tc.json)
			}
			// The message has to say the capability does not exist, not merely that
			// the field is unknown. An agent told "unknown field" retries with a
			// synonym; one told this orchestrator grants no host access stops.
			if !strings.Contains(err.Error(), "access to this host") {
				t.Errorf("%s was refused, but not as a host-access request, so a caller learns "+
					"only that this spelling is wrong: %v", tc.name, err)
			}
		})
	}
}

// TestDockerOptionsStillAcceptsItsRealFields is the counterpart.
//
// A strict decoder that refused everything would pass the test above and break
// provisioning. This is what keeps the strictness honest — and it fails by name,
// so a field removed from dockerOptions is reported as that rather than as a
// provisioning test failing somewhere downstream.
func TestDockerOptionsStillAcceptsItsRealFields(t *testing.T) {
	const request = `{
		"host": "127.0.0.1",
		"image_tag": "breeze-provisioned/users:latest",
		"container_name": "breeze-users",
		"env": {"LOG_LEVEL": "debug"},
		"skip_build": true,
		"wait_seconds": -1,
		"enable_app_mcp": true
	}`

	var opts dockerOptions
	if err := json.Unmarshal([]byte(request), &opts); err != nil {
		t.Fatalf("a request using only documented fields was refused: %v", err)
	}

	if opts.Host != "127.0.0.1" || opts.ImageTag != "breeze-provisioned/users:latest" ||
		opts.ContainerName != "breeze-users" || opts.Env["LOG_LEVEL"] != "debug" ||
		!opts.SkipBuild || opts.WaitSeconds != -1 || !opts.EnableAppMCP {
		t.Errorf("a field decoded to the wrong value: %+v", opts)
	}
}

// TestATypoInADockerOptionIsRefusedRatherThanIgnored is the non-security half of the
// same decision, and the reason strictness is worth its cost here.
//
// wait_seconds: -1 means "do not wait". Spelled wait_second it was previously dropped,
// and provisioning then waited thirty seconds for an application the caller had said
// not to wait for — a silent contradiction of an explicit argument.
func TestATypoInADockerOptionIsRefusedRatherThanIgnored(t *testing.T) {
	var opts dockerOptions
	err := json.Unmarshal([]byte(`{"wait_second": -1}`), &opts)
	if err == nil {
		t.Fatal("a misspelled option was accepted, so the caller's intent was silently dropped")
	}
	if !strings.Contains(err.Error(), "wait_seconds") {
		t.Errorf("the refusal does not name the field that was meant, so it does not help: %v", err)
	}
}

// TestNoDockerCommandRequestsHostAccess is the emission half.
//
// A full provision runs against the fake, and every argv it produced is checked. The
// fake records at the lowest level — the argument list handed to the docker binary —
// so this sees exactly what a real daemon would have been asked for, including flags
// no tool argument could have influenced.
//
// This is the test that catches the case the decode test cannot: a future field, or a
// line added to runContainer for a plausible reason, that mounts the host on every
// provision without any caller asking.
func TestNoDockerCommandRequestsHostAccess(t *testing.T) {
	fake := &fakeDocker{}
	orch := newTestOrchestrator(t, fake)

	result := orch.provisionService(minimalService("users"))
	if result.IsError {
		t.Fatalf("provisioning failed, so there is nothing to inspect: %s", result.Content[0].Text)
	}
	if len(fake.commands) == 0 {
		t.Fatal("no docker command was recorded, so this test proved nothing")
	}

	for _, args := range fake.commands {
		if err := refuseHostAccess(args); err != nil {
			t.Errorf("docker %s\n%v", strings.Join(args, " "), err)
		}
	}
}

// TestRefuseHostAccessCatchesEveryFlagItLists checks the guard itself.
//
// Each entry is tried in three shapes: as `--flag value`, as `--flag=value`, and in
// operand position. The third matters most — it is the argument-smuggling case, where
// a caller-supplied name lands where Docker's parser reads a flag, and it is the shape
// a check that only looked at even-numbered arguments would miss.
func TestRefuseHostAccessCatchesEveryFlagItLists(t *testing.T) {
	for flag := range hostAccessFlags {
		t.Run(flag, func(t *testing.T) {
			for _, args := range [][]string{
				{"run", "-d", flag, "host", "image"},
				{"run", "-d", flag + "=host", "image"},
				{"run", "-d", "--name", flag, "image"},
			} {
				if err := refuseHostAccess(args); err == nil {
					t.Errorf("docker %s was allowed", strings.Join(args, " "))
				}
			}
		})
	}
}

// TestRefuseHostAccessAllowsWhatProvisioningActuallyNeeds is the false-positive guard.
//
// A denylist that refused the orchestrator's own commands would be found immediately;
// one that refused only an uncommon path — `docker rmi` during a deprovision with
// remove_image — would be found by an operator, in production, as a service that
// cannot be cleaned up. The argvs here are the real shapes from docker.go.
func TestRefuseHostAccessAllowsWhatProvisioningActuallyNeeds(t *testing.T) {
	for _, args := range [][]string{
		{"version", "--format", "{{.Server.Version}}"},
		{"build", "-t", "breeze-provisioned/users:latest", "/tmp/breeze-provision-x/users"},
		{"build", "-t", "img", "--build-arg", "VERSION=1.2.3", "/ctx"},
		{"run", "-d", "--name", "breeze-users",
			"-e", "BREEZE_MCP_TOKEN=abc", "-e", "LOG_LEVEL=debug",
			"-p", "127.0.0.1:40001:2000", "-p", "127.0.0.1:40002:8080",
			"--add-host", "host.docker.internal:host-gateway", "breeze-provisioned/users:latest"},
		{"inspect", "--format", `{"running":true}`, "c0ntainer0001"},
		{"stop", "c0ntainer0001"},
		{"rm", "c0ntainer0001"},
		{"rmi", "breeze-provisioned/users:latest"},
	} {
		if err := refuseHostAccess(args); err != nil {
			t.Errorf("a command provisioning genuinely issues was refused:\ndocker %s\n%v",
				strings.Join(args, " "), err)
		}
	}
}

// TestDockerExecIsTheOnlyWayThisPackageRunsDocker pins the choke point.
//
// refuseHostAccess is only a boundary if every docker invocation passes through it.
// dockerClient.exec is that one door; a call to d.run elsewhere in the package would
// bypass the check entirely and nothing else here would notice — which is the same
// failure mode, and the same fix, as resolvePath in confine.go.
//
// The package's own source is read rather than the behaviour being inferred, because
// the property is about which function is called, and there is no runtime observation
// that distinguishes "checked" from "not checked" for a command that happens to be
// harmless.
func TestDockerExecIsTheOnlyWayThisPackageRunsDocker(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}

		for i, line := range strings.Split(string(source), "\n") {
			if !strings.Contains(line, "d.run(") {
				continue
			}
			// The one legitimate call: exec's own, which performs the check first.
			if name == "docker.go" && strings.Contains(line, "return d.run(ctx, d.binary, args...)") {
				continue
			}
			t.Errorf("%s:%d calls d.run directly, bypassing refuseHostAccess. Use d.exec:\n\t%s",
				name, i+1, strings.TrimSpace(line))
		}
	}
}
