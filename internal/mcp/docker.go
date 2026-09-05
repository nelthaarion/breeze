package mcp

// docker.go — the orchestrator's Docker access, via the docker CLI.
//
// # Why the CLI and not the Engine API
//
// The Engine API would mean either a new dependency (the official client pulls in a
// large tree) or hand-rolling HTTP over a Unix socket and a Windows named pipe,
// including the API version negotiation that goes with it. The CLI is already
// installed wherever Docker is usable at all, it is the same interface an operator
// would use to check this orchestrator's work by hand, and shelling out costs one
// process per provisioning step — a rounding error next to an image build.
//
// The objection to shelling out, made in protocol.go about the generator, does not
// apply here. That argument was about losing a diagnostic: a generator failure
// arriving as "exit status 1" with the real error mixed into stdout. Docker's CLI
// does the opposite — a specific, quotable message on stderr and a non-zero exit —
// so the error survives rather than being flattened. What would be lost by going
// the other way is the ability to run at all on a machine without a Go Docker
// client's transitive dependencies.
//
// # This is the distinguished instance
//
// Only the orchestrator uses this file. Most breeze-mcp instances — one per
// service, per Parts 1 and 2 of the design — have no Docker socket and no need for
// one: they generate and verify code inside their own container. Exactly one (or a
// small controlled number) is given Docker access, and that instance is the only
// thing that can create or destroy another.
//
// "Distinguished" is a deployment fact, not a build-time one. The Category H tools
// are registered by every instance, and each refuses at call time when Docker is
// not reachable, naming what is missing. The alternative — registering them only
// when a socket is present — would make tools/list depend on the machine, and a
// client's cached tool list would then be wrong in a way nothing reports.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Docker timeouts, per operation. They differ because the operations do: a build
// compiles a Go binary from scratch on a cold cache, while an inspect reads local
// state.
//
// Each exists so a hung daemon is reported as a hung daemon rather than as an MCP
// client that stopped answering.
const (
	dockerQuickTimeout = 30 * time.Second
	dockerRunTimeout   = 2 * time.Minute
	dockerBuildTimeout = 15 * time.Minute
	dockerStopTimeout  = 60 * time.Second
)

// dockerClient runs docker commands.
//
// The binary path is resolved once, at construction, so a missing docker produces
// one clear answer at registration time instead of an exec error per tool call.
type dockerClient struct {
	binary string

	// run is the command runner. It is a field so the provisioning tests can drive
	// the whole tool path — argument construction, registry writes, port allocation,
	// token handling — on a machine with no Docker daemon, without any of that logic
	// carrying a test-only branch.
	run func(ctx context.Context, binary string, args ...string) (string, error)
}

// newDockerClient returns a client, or an error naming what is missing.
func newDockerClient() (*dockerClient, error) {
	binary, err := exec.LookPath("docker")
	if err != nil {
		return nil, fmt.Errorf("docker is not on PATH, so this instance cannot provision containers. "+
			"Only an orchestrator instance needs it: %w", err)
	}
	return &dockerClient{binary: binary, run: runDockerCommand}, nil
}

// available reports whether the daemon answers, not merely whether the CLI exists.
//
// The distinction is worth a round trip: `docker` is installed on plenty of machines
// where the daemon is stopped or the socket is not mounted into this container, and
// "command not found" and "cannot connect to the Docker daemon" send an operator to
// completely different places.
func (d *dockerClient) available() error {
	ctx, cancel := context.WithTimeout(context.Background(), dockerQuickTimeout)
	defer cancel()

	if _, err := d.exec(ctx, "version", "--format", "{{.Server.Version}}"); err != nil {
		return fmt.Errorf("the Docker daemon is not reachable: %w. An orchestrator instance needs the "+
			"socket mounted (-v /var/run/docker.sock:/var/run/docker.sock) or DOCKER_HOST set", err)
	}
	return nil
}

// exec runs a docker subcommand after checking the argv.
//
// Every call in this file goes through it rather than through d.run directly, so
// refuseHostAccess sees the complete argument list of every docker invocation this
// package makes. See docker_args.go for why the check is here and not at the point
// each argv is assembled.
//
// It wraps d.run rather than replacing it so the provisioning tests keep their
// recorder: the check runs in the real code path, and what the fake receives is what
// the docker binary would have received.
func (d *dockerClient) exec(ctx context.Context, args ...string) (string, error) {
	if err := refuseHostAccess(args); err != nil {
		return "", err
	}
	return d.run(ctx, d.binary, args...)
}

// runDockerCommand is the real runner.
//
// stdout and stderr are combined, because Docker splits progress and diagnostics
// across them differently per subcommand, and a build's failure is only intelligible
// with both in the order they were written.
func runDockerCommand(ctx context.Context, binary string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))

	if ctx.Err() != nil {
		return text, fmt.Errorf("docker %s timed out: %w", args[0], ctx.Err())
	}
	if err != nil {
		if text == "" {
			return "", fmt.Errorf("docker %s failed: %w", args[0], err)
		}
		// Docker's own message first: it is the specific one, and the exit status adds
		// nothing a reader needs.
		return text, fmt.Errorf("docker %s failed: %s", args[0], dockerFirstLine(text))
	}
	return text, nil
}

// dockerFirstLine returns the first non-empty line of an output, for an error
// message that has to fit in a sentence.
//
// Named apart from live.go's firstLine deliberately: that one truncates an HTTP
// body to a readable excerpt, this one picks the line Docker put its diagnostic on.
// Sharing an implementation would mean one of the two callers getting behaviour
// tuned for the other.
func dockerFirstLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return text
}

// buildImage builds a context directory into a tagged image.
func (d *dockerClient) buildImage(ctx context.Context, dir, tag string, buildArgs map[string]string) (string, error) {
	args := []string{"build", "-t", tag}
	for key, value := range buildArgs {
		args = append(args, "--build-arg", key+"="+value)
	}
	args = append(args, dir)

	buildCtx, cancel := context.WithTimeout(ctx, dockerBuildTimeout)
	defer cancel()

	return d.exec(buildCtx, args...)
}

// dockerHostAlias is the hostname a container uses to reach the host.
//
// Docker Desktop resolves it natively; on Linux it does not exist, so runContainer maps
// it to host-gateway explicitly. One name, both platforms.
//
// A provisioned fleet depends on this. Each service's tracer exports to the aggregator
// through a published *host* port, and inside a container 127.0.0.1 is that container —
// so a tracer aimed at the host loopback writes into its own closed port. Export is
// asynchronous and best-effort, so nothing errors: the fleet works and records nothing.
const dockerHostAlias = "host.docker.internal"

// runOptions describes one container to start.
type runOptions struct {
	Name  string
	Image string

	// Env is passed with -e KEY=VALUE. This is how a control token reaches a
	// provisioned instance: never as a CLI argument, which every process listing on
	// the host would show. `docker inspect` can still read the environment, which is
	// why each instance gets its own token rather than sharing one — access to the
	// host is already total, and this keeps a leaked token's blast radius to a single
	// service.
	Env map[string]string

	// Ports maps hostPort → containerPort. Both are explicit; nothing here publishes
	// a port Docker chose, because a port this orchestrator did not allocate is a
	// port its allocator does not know about.
	Ports map[int]int
}

// runContainer starts a container detached and returns its id.
func (d *dockerClient) runContainer(ctx context.Context, opts runOptions) (string, error) {
	args := []string{"run", "-d", "--name", opts.Name}
	for key, value := range opts.Env {
		args = append(args, "-e", key+"="+value)
	}
	for host, container := range opts.Ports {
		// Bound to 127.0.0.1 on the host, for the same reason breeze-mcp's own
		// listener is: a provisioned service should not become reachable from the
		// network merely because someone provisioned it.
		args = append(args, "-p", fmt.Sprintf("127.0.0.1:%d:%d", host, container))
	}
	// The host as seen from inside the container.
	//
	// Native on Docker Desktop and absent on Linux, where this maps it to the default
	// gateway — so one address works everywhere. A fleet needs it: every service's
	// tracer reaches the aggregator through a published host port, and 127.0.0.1 inside
	// a container is that container, not the host.
	//
	// Harmless when the daemon already provides the name; Docker accepts the explicit
	// mapping and the container ends up with one entry either way.
	args = append(args, "--add-host", dockerHostAlias+":host-gateway")
	args = append(args, opts.Image)

	runCtx, cancel := context.WithTimeout(ctx, dockerRunTimeout)
	defer cancel()

	out, err := d.exec(runCtx, args...)
	if err != nil {
		return "", err
	}
	id := dockerFirstLine(out)
	if id == "" {
		return "", errors.New("docker run reported no container id")
	}
	return id, nil
}

// containerState is the part of `docker inspect` this package reads.
type containerState struct {
	Running bool   `json:"running"`
	Status  string `json:"status"`
	Health  string `json:"health"`
}

// inspectState reports a container's state.
//
// The format string asks for exactly the three fields used rather than parsing the
// full inspect document: that document is large, version-dependent, and every field
// of it this package does not read is a field that could change shape and break the
// decode for no reason.
//
// Health is empty for an image with no HEALTHCHECK, which is different from
// unhealthy and is reported as such — calling a container unhealthy when it never
// claimed to have a health check would be a fabricated finding.
func (d *dockerClient) inspectState(ctx context.Context, id string) (containerState, error) {
	const format = `{"running":{{.State.Running}},"status":{{printf "%q" .State.Status}},` +
		`"health":{{if .State.Health}}{{printf "%q" .State.Health.Status}}{{else}}""{{end}}}`

	inspectCtx, cancel := context.WithTimeout(ctx, dockerQuickTimeout)
	defer cancel()

	out, err := d.exec(inspectCtx, "inspect", "--format", format, id)
	if err != nil {
		return containerState{}, err
	}

	var state containerState
	if err := json.Unmarshal([]byte(dockerFirstLine(out)), &state); err != nil {
		return containerState{}, fmt.Errorf("docker inspect returned an unreadable state for %s: %w", id, err)
	}
	return state, nil
}

// removeContainer stops and removes a container.
//
// Stop then remove, rather than `rm -f`: a forced removal kills the process
// immediately, and a Breeze service with a Fleet tracer buffers spans that are only
// flushed by Close on shutdown. A graceful stop means the last traces of a
// deprovisioned service are not the ones lost.
func (d *dockerClient) removeContainer(ctx context.Context, id string) error {
	stopCtx, cancelStop := context.WithTimeout(ctx, dockerStopTimeout)
	defer cancelStop()

	// A stop failure is not fatal on its own — the container may already be stopped,
	// which is the common case after a crash — so the removal is attempted regardless
	// and its error is the one that decides the outcome.
	_, _ = d.exec(stopCtx, "stop", id)

	rmCtx, cancelRm := context.WithTimeout(ctx, dockerQuickTimeout)
	defer cancelRm()

	_, err := d.exec(rmCtx, "rm", id)
	return err
}

// removeImage deletes an image this orchestrator built.
//
// Failure is returned but callers treat it as a note rather than an error: an image
// still referenced by a stopped container, or shared with another tag, legitimately
// refuses removal, and a deprovision that succeeded in every other respect should
// not report failure over a leftover layer.
func (d *dockerClient) removeImage(ctx context.Context, tag string) error {
	rmCtx, cancel := context.WithTimeout(ctx, dockerQuickTimeout)
	defer cancel()

	_, err := d.exec(rmCtx, "rmi", tag)
	return err
}
