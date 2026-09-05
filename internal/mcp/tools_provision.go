package mcp

// tools_provision.go — Category H: Docker-aware fleet provisioning.
//
// # What is new here, and what is reused
//
// The only genuinely new work in this file is orchestration: allocate ports, build
// an image, start a container, record it, report the three addresses. Everything
// that produces a *project* is reused verbatim:
//
//   - generation goes through planProjectArgs.scaffold → generator.New, the exact
//     path breeze_new takes. There is no second scaffolder;
//   - Fleet wiring goes through generator.ApplyConfig with FleetConfig set, which is
//     the same call breeze_add's fleet feature makes. There is no second wiring.
//
// That reuse is the point rather than a convenience. A parallel implementation
// would drift: the moment `breeze add fleet` gained a field, a provisioned service
// would quietly stop matching a hand-wired one, and the failure would appear as
// traces that do not join up rather than as a build error.
//
// # Why the tools are named without the breeze_ prefix
//
// Every other tool here is breeze_*. These four are provision_service,
// list_provisioned_services, deprovision_service and provision_fleet, because that
// is what the design names them and because a tool name is API — a client
// configuration or an agent's learned habit refers to it. Renaming them to fit a
// convention would break callers to make a list tidier.
//
// # Addresses
//
// Nothing in this file returns a field called "port". Every address is one of
// control_port, app_port or aggregator_port, and the aggregator's belongs to the
// Fleet Aggregator rather than to the service that happens to host it. The three
// are allocated from one allocator (ports.go) so that no two of them can ever be
// the same number within one orchestrator's lifetime.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nelthaarion/breeze/client"
	"github.com/nelthaarion/breeze/internal/generator"
)

// registerProvisioningTools wires Category H.
//
// The orchestrator state — one allocator, one registry, one Docker client — is
// created once and shared by the four tools, because it has to be: two allocators
// would hand out the same port, and two registries would each authorise removing
// what the other created.
func registerProvisioningTools(s *Server) {
	orch := newOrchestrator(s.version)

	s.addTool(provisionServiceTool(orch))
	s.addTool(listProvisionedServicesTool(orch))
	s.addTool(deprovisionServiceTool(orch))
	s.addTool(provisionFleetTool(orch))
}

// orchestrator holds the provisioning state.
type orchestrator struct {
	version string

	ports *portAllocator

	// registry and docker are resolved lazily, on the first call that needs them,
	// and the reason is different for each. The registry touches the filesystem, and
	// NewServer must not fail because a file is unreadable — every other tool would
	// become unavailable over a problem none of them have. Docker may be absent
	// entirely, which is the normal case for the many non-orchestrator instances.
	registry *provisionRegistry
	docker   *dockerClient

	// loadErr is remembered so a repeated call reports the same reason rather than
	// retrying a filesystem read that will fail again. Docker gets no equivalent:
	// a daemon that was down a minute ago may be up now, and refusing to retry
	// would make a transient outage permanent for the life of the process.
	loadErr error
}

func newOrchestrator(version string) *orchestrator {
	return &orchestrator{version: version, ports: newPortAllocator()}
}

// store returns the registry, loading it on first use.
func (o *orchestrator) store() (*provisionRegistry, error) {
	if o.registry != nil {
		return o.registry, nil
	}
	if o.loadErr != nil {
		return nil, o.loadErr
	}

	reg, err := loadProvisionRegistry("", o.ports)
	if err != nil {
		o.loadErr = err
		return nil, err
	}
	o.registry = reg
	return reg, nil
}

// dockerOrError returns a Docker client, or the reason there is none.
//
// Both checks run: the CLI must exist and the daemon must answer. They are separate
// failures with separate remedies, and reporting them together as "docker
// unavailable" would send an operator looking for an install when the socket is
// simply not mounted.
func (o *orchestrator) dockerOrError() (*dockerClient, error) {
	if o.docker != nil {
		return o.docker, nil
	}

	client, err := newDockerClient()
	if err != nil {
		return nil, err
	}
	if err := client.available(); err != nil {
		return nil, err
	}
	o.docker = client
	return client, nil
}

// ─── shared argument and result shapes ───────────────────────────────────────

// dockerOptions is the docker half of a provisioning request.
type dockerOptions struct {
	// Host is where the published ports will be reachable. It defaults to
	// 127.0.0.1, matching what runContainer publishes on, and is recorded in the
	// registry so a returned address is usable verbatim rather than needing the
	// caller to know where the daemon lives.
	Host string `json:"host"`

	// ImageTag overrides the generated tag. Provided because an operator with a
	// registry naming convention should not have to work around ours; when empty a
	// tag is derived from the service name.
	ImageTag string `json:"image_tag"`

	// ContainerName overrides the container's name, for the same reason.
	ContainerName string `json:"container_name"`

	// Env adds environment variables to the container. BREEZE_MCP_TOKEN cannot be
	// set here: the orchestrator mints that one, and letting a caller supply it
	// would mean the token in the return value was not necessarily the token the
	// container is using.
	Env map[string]string `json:"env"`

	// SkipBuild starts from an existing ImageTag instead of building. It exists for
	// re-provisioning a known-good image quickly, and it is refused without an
	// explicit ImageTag — "skip the build and use whichever image you would have
	// built" has no meaning.
	SkipBuild bool `json:"skip_build"`

	// WaitForHealthy bounds how long provisioning waits for the app port to answer
	// before reporting. Zero means the default; a negative value means do not wait.
	//
	// Waiting is the default because the alternative is a tool that returns an
	// app_port nothing is listening on yet, and a caller that immediately calls
	// breeze_get_routes against it gets "nothing is listening" — a true statement
	// about a service that is merely still starting.
	WaitSeconds int `json:"wait_seconds"`

	// EnableAppMCP asks the provisioned service to also expose its own app-runtime
	// MCP endpoint, on a fourth published port.
	//
	// Off by default, because it is a second listener and a second thing to secure,
	// and because a service that nobody intends to introspect this way should not have
	// one. Turning it on is what an agent wants when the question is "what is this
	// instance doing right now" rather than "change this project": the app-runtime
	// endpoint answers the former and, by construction, cannot answer the latter — it
	// has no mutating tool registered at all.
	//
	// The container's generator-level control port is unaffected either way. These are
	// separate servers with separate capabilities, which is exactly why this needs its
	// own port rather than sharing one.
	EnableAppMCP bool `json:"enable_app_mcp"`
}

// provisionedResult is what provision_service returns.
//
// Every address is named. There is no "port" field, and control_token appears here
// and nowhere else in this package's output.
type provisionedResult struct {
	ServiceName string `json:"service_name"`
	ContainerID string `json:"container_id"`
	Host        string `json:"host"`

	// ControlPort and ControlToken are the control plane: add these to an MCP
	// client as a named server. ControlURL is the same information pre-assembled,
	// because the endpoint path is not something a caller should have to know.
	ControlPort  int    `json:"control_port"`
	ControlToken string `json:"control_token"`
	ControlURL   string `json:"control_url"`

	// AppPort is the running application. AppURL is what a Category C/D tool's
	// service_url argument takes.
	AppPort int    `json:"app_port"`
	AppURL  string `json:"app_url"`

	// AggregatorPort and AggregatorURL are set only when this service hosts a Fleet
	// Aggregator, and describe the aggregator's own endpoint — not this service's
	// control plane and not its application. AggregatorURL is what a Fleet tool's
	// aggregator_url argument takes.
	AggregatorPort int    `json:"aggregator_port,omitempty"`
	AggregatorURL  string `json:"aggregator_url,omitempty"`

	// AppMCPPort and AppMCPURL are set only when enable_app_mcp was requested, and
	// describe the application's own read-only introspection endpoint.
	//
	// Not interchangeable with ControlURL above: that one can generate and rewrite
	// this project, this one cannot — it serves no mutating tool. Point an agent here
	// to ask what the running instance is doing.
	AppMCPPort int    `json:"app_mcp_port,omitempty"`
	AppMCPURL  string `json:"app_mcp_url,omitempty"`

	Image  string `json:"image"`
	Status string `json:"status"`

	// TokenNotice is returned with every provision, because the one-time nature of
	// the token above is the single fact a caller most needs and is least likely to
	// be told anywhere else at the moment it matters.
	TokenNotice string `json:"token_notice"`

	// Notes carry anything that went right enough to return but is worth saying —
	// a health check that timed out, an image left behind.
	Notes []string `json:"notes,omitempty"`
}

// tokenNotice is that warning, spelled once so every provisioning path says the
// same thing.
const tokenNotice = "control_token is issued exactly once, at provision time. No tool reports it " +
	"afterwards — not list_provisioned_services, not any other. If it is lost, the only recovery is " +
	"deprovision_service followed by provisioning again."

// resultFor builds a return value from a registry entry and its one-time token.
func resultFor(service provisionedService, token, status string, notes []string) provisionedResult {
	return provisionedResult{
		ServiceName:    service.ServiceName,
		ContainerID:    service.ContainerID,
		Host:           service.Host,
		ControlPort:    service.ControlPort,
		ControlToken:   token,
		ControlURL:     service.controlURL(),
		AppPort:        service.AppPort,
		AppURL:         service.appURL(),
		AggregatorPort: service.AggregatorPort,
		AggregatorURL:  service.aggregatorURL(),
		AppMCPPort:     service.AppMCPPort,
		AppMCPURL:      service.appMCPURL(),
		Image:          service.Image,
		Status:         status,
		TokenNotice:    tokenNotice,
		Notes:          notes,
	}
}

// ─── provision_service ───────────────────────────────────────────────────────

// provisionServiceArgs is the tool's argument set.
//
// The config half embeds configInput, so a ProjectConfig is spelled here exactly as
// breeze_plan_project and breeze_diff_config spell it — object or YAML, one decoder,
// one meaning.
type provisionServiceArgs struct {
	configInput

	// Name is the service name and the project directory. It is the registry key, so
	// it is required: an unnamed service could be provisioned and then never found,
	// reported or removed.
	Name string `json:"name"`

	Template string        `json:"template"`
	Docker   dockerOptions `json:"docker"`
}

func provisionServiceTool(orch *orchestrator) *tool {
	return &tool{
		name: "provision_service",
		description: "Generate a Breeze project, build its container image, and start it. " +
			"Returns the service's control_port and one-time control_token (its own breeze-mcp " +
			"control plane, for an MCP client to connect to) and its app_port (the running " +
			"application's own address, for service_url arguments). These are always different " +
			"ports. Requires an orchestrator instance with Docker access.",
		schema: objectSchema(map[string]any{
			"name":        stringProp("Service name, also the project directory and the registry key."),
			"config":      map[string]any{"type": "object", "description": "Project configuration, in the shape breeze_describe_schema documents."},
			"config_yaml": stringProp("Project configuration as breeze.yaml text. Use this or config, not both."),
			"template":    map[string]any{"type": "string", "enum": []string{"api", "views"}, "description": "api for a JSON service, views for server-rendered HTML. Defaults to api."},
			"docker": map[string]any{
				"type": "object",
				"description": "Docker options: host, image_tag, container_name, env, skip_build, wait_seconds, " +
					"enable_app_mcp. enable_app_mcp publishes a fourth port serving the application's own " +
					"read-only app-runtime MCP endpoint, which is what to point an agent at to ask what the " +
					"running instance is doing; the control port remains the generator-level server.",
			},
		}, "name"),
		run: func(raw json.RawMessage) toolCallResult {
			var a provisionServiceArgs
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}
			return orch.provisionService(a)
		},
	}
}

// provisionService is the whole sequence, in the order that leaves the least behind
// when a step fails.
func (o *orchestrator) provisionService(a provisionServiceArgs) toolCallResult {
	name := strings.TrimSpace(a.Name)
	if name == "" {
		return errorResult("name is required: it is the registry key this service is managed by")
	}
	// Validated before anything is allocated, generated or built. The name becomes
	// a directory, a container name and an image tag; the Docker options become
	// arguments to `docker run`. See docker_names.go for what each check is for.
	if err := validateServiceName(name); err != nil {
		return errorResult(err.Error())
	}
	if err := validateDockerOptions(a.Docker); err != nil {
		return errorResult(err.Error())
	}

	reg, err := o.store()
	if err != nil {
		return errorResult(err.Error())
	}
	if _, exists := reg.get(name); exists == nil {
		return errorResult(fmt.Sprintf("%q is already provisioned; deprovision_service it first, "+
			"or provision under a different name", name))
	}

	docker, err := o.dockerOrError()
	if err != nil {
		return errorResult(err.Error())
	}

	cfg, err := a.resolve()
	if err != nil {
		return errorResult("configuration: " + err.Error())
	}

	plan, err := o.prepare(name, a.Template, cfg, a.Docker, 0)
	if err != nil {
		return errorResult(err.Error())
	}
	defer plan.cleanup()

	service, notes, err := o.launch(context.Background(), docker, plan)
	if err != nil {
		plan.releasePorts(o.ports)
		return errorResult(err.Error())
	}

	if err := reg.add(service); err != nil {
		// The container is running but unrecorded, which is the one state this
		// orchestrator must never leave behind: nothing would authorise removing it
		// later. So it is removed now, and the registry failure is the reported one.
		if rmErr := docker.removeContainer(context.Background(), service.ContainerID); rmErr != nil {
			return errorResult(fmt.Sprintf("the container started but could not be recorded (%v), and "+
				"removing it also failed (%v). Container %s must be removed by hand.",
				err, rmErr, service.ContainerID))
		}
		plan.releasePorts(o.ports)
		return errorResult(fmt.Sprintf("the container started but could not be recorded, so it was "+
			"removed again rather than left unmanaged: %v", err))
	}

	status, healthNote := o.waitForApp(service, a.Docker.WaitSeconds)
	if healthNote != "" {
		notes = append(notes, healthNote)
	}

	return structuredResult(fmt.Sprintf(
		"%s is running: control plane on %s (token returned once, below), application on %s",
		name, service.controlURL(), service.appURL()),
		resultFor(service, plan.token, status, notes))
}

// ─── the steps provisioning is made of ───────────────────────────────────────

// provisionPlan is everything decided before Docker is touched: the ports, the
// token, the built project directory, the names.
//
// It exists as a value so that the failure path is one call — releasePorts and
// cleanup — rather than an unwinding sequence repeated at each step. Provisioning a
// fleet performs this several times before starting anything, and a partial fleet
// that leaked three ports and two temporary directories is the kind of mess that
// only shows up as a range slowly filling.
type provisionPlan struct {
	name      string
	dir       string
	token     string
	image     string
	container string

	host           string
	controlPort    int
	appPort        int
	aggregatorPort int
	// appMCPPort is the embedded app-runtime endpoint's host port, or zero when the
	// service does not enable one.
	appMCPPort int

	skipBuild bool
	env       map[string]string

	// buildNotes are anything worth reporting about how the image will be built —
	// most usefully whether the framework is compiled from local source or fetched
	// from the proxy. Carried on the plan rather than logged, because provision_service
	// returns its notes to the caller and a note nobody sees is not a note.
	buildNotes []string

	// remove deletes the generated project directory. The image was built from it,
	// so it is not needed once the build is done — and it holds a copy of the
	// project, which the container now also holds.
	remove func()
}

func (p provisionPlan) cleanup() {
	if p.remove != nil {
		p.remove()
	}
}

// releasePorts returns this plan's ports to the allocator. Called only when the plan
// did not become a registry entry; once it has, the registry owns their lifetime.
func (p provisionPlan) releasePorts(ports *portAllocator) {
	ports.release(p.controlPort)
	ports.release(p.appPort)
	if p.aggregatorPort != 0 {
		ports.release(p.aggregatorPort)
	}
	if p.appMCPPort != 0 {
		ports.release(p.appMCPPort)
	}
}

// prepare generates the project and allocates the addresses.
//
// aggregatorPort is passed in rather than allocated here because only provision_fleet
// knows which of its services hosts the aggregator, and allocating one for every
// service would burn two ports per service for no reason.
func (o *orchestrator) prepare(
	name, template string,
	cfg generator.ProjectConfig,
	opts dockerOptions,
	aggregatorPort int,
) (provisionPlan, error) {
	// Every port from one allocator, in one call, so a failure part-way releases what
	// it already took. The app-level MCP port joins the same call rather than being
	// allocated afterwards: a second allocation could fail on its own and would then
	// have to unwind the first two by hand.
	purposes := []string{portPurposeControl, portPurposeApp}
	if opts.EnableAppMCP {
		purposes = append(purposes, portPurposeAppMCP)
	}
	ports, err := o.ports.allocateN(purposes...)
	if err != nil {
		return provisionPlan{}, err
	}

	token, err := NewToken()
	if err != nil {
		for _, allocated := range ports {
			o.ports.release(allocated)
		}
		return provisionPlan{}, err
	}

	plan := provisionPlan{
		name:           name,
		token:          token,
		host:           orDefault(opts.Host, "127.0.0.1"),
		controlPort:    ports[0],
		appPort:        ports[1],
		aggregatorPort: aggregatorPort,
		image:          orDefault(opts.ImageTag, "breeze-provisioned/"+name+":latest"),
		container:      orDefault(opts.ContainerName, "breeze-"+name),
		skipBuild:      opts.SkipBuild,
	}
	if opts.EnableAppMCP {
		// Index 2 by construction: it is appended above only in this case.
		plan.appMCPPort = ports[2]
	}

	if plan.skipBuild && strings.TrimSpace(opts.ImageTag) == "" {
		plan.releasePorts(o.ports)
		return provisionPlan{}, fmt.Errorf("skip_build needs an explicit image_tag: there is no " +
			"previously built image to reuse without one")
	}

	// BREEZE_MCP_TOKEN last, so a caller cannot override the token this call is
	// about to return. Everything else the caller asked for is honoured.
	plan.env = map[string]string{}
	for key, value := range opts.Env {
		if strings.EqualFold(key, tokenEnvVar) {
			continue
		}
		plan.env[key] = value
	}
	plan.env[tokenEnvVar] = token

	// The aggregator is started by the entrypoint only when this variable is present,
	// which is how one image serves both a fleet's host service and its plain ones.
	//
	// The value is the *container* port, not the host port the allocator handed out:
	// runContainer maps hostPort → containerAggregatorPort, so a process listening on
	// the host number inside the container would be behind a mapping to a port nothing
	// bound. The host side is already recorded on the plan and returned as
	// aggregator_url.
	if aggregatorPort != 0 {
		plan.env[fleetAggregatorPortEnvVar] = strconv.Itoa(containerAggregatorPort)
	}

	if !plan.skipBuild {
		dir, remove, notes, err := o.generateProject(name, template, cfg)
		if err != nil {
			plan.releasePorts(o.ports)
			return provisionPlan{}, err
		}
		plan.dir, plan.remove = dir, remove
		plan.buildNotes = notes
	}
	return plan, nil
}

// tokenEnvVar is the variable a provisioned container's control instance reads its
// token from. It is the same name cmd/breeze-mcp accepts, which is what makes a
// provisioned instance and a hand-started one configured identically.
const tokenEnvVar = "BREEZE_MCP_TOKEN"

// fleetAggregatorPortEnvVar tells a provisioned container to run the Fleet Aggregator.
//
// Set on exactly one service per fleet — the one aggregator.hosted_by names. Its absence
// is what keeps the other services from each starting an aggregator nothing reports to.
//
// Named for the aggregator rather than reusing the binary's own FLEET_PORT because a
// container already carries an app port and a control port; a variable called FLEET_PORT
// in that company invites being set to the wrong one. The entrypoint translates.
const fleetAggregatorPortEnvVar = "FLEET_AGGREGATOR_PORT"

// orDefault returns value when it is non-blank, else fallback.
func orDefault(value, fallback string) string {
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		return trimmed
	}
	return fallback
}

// generateProject scaffolds into a temporary directory and applies the
// configuration.
//
// Both halves are the existing paths. generator.New is what breeze_new calls;
// generator.ApplyConfig is what wires every feature a configuration enables — Fleet
// included — and is the same call `breeze new` makes when a breeze.yaml is present.
// Nothing about a provisioned project's code is produced here.
func (o *orchestrator) generateProject(name, template string, cfg generator.ProjectConfig) (string, func(), []string, error) {
	if strings.TrimSpace(template) == "" {
		template = "api"
	}
	if err := cfg.Validate(); err != nil {
		return "", nil, nil, fmt.Errorf("the configuration is not valid, so nothing was provisioned: %w", err)
	}

	root, err := os.MkdirTemp("", "breeze-provision-*")
	if err != nil {
		return "", nil, nil, err
	}
	remove := func() { _ = os.RemoveAll(root) }

	configPath, cleanupConfig, err := writeTempConfig(cfg)
	if err != nil {
		remove()
		return "", nil, nil, err
	}
	defer cleanupConfig()

	// The generator's output is captured rather than printed, for the reason
	// capture.go exists: on stdio, os.Stdout is the protocol stream. Provisioning
	// runs on the network transport, but the capture is not conditional on the
	// transport — a tool that behaved differently depending on how it was reached
	// would be the one thing this whole feature promises not to build.
	argv := []string{name, "--template=" + template, "--config=" + configPath}
	out, capErr := captureStdout(func() error {
		return runInDir(root, func() error { return generator.New(argv) })
	})
	if capErr != nil {
		remove()
		return "", nil, nil, capErr
	}

	projectDir := filepath.Join(root, name)
	if _, err := os.Stat(projectDir); err != nil {
		remove()
		return "", nil, nil, fmt.Errorf("the generator did not produce %s: %w\n%s", name, err, strings.TrimSpace(out))
	}

	notes, err := writeProvisionBuildFiles(projectDir, o.version)
	if err != nil {
		remove()
		return "", nil, nil, err
	}
	return projectDir, remove, notes, nil
}

// launch builds the image if needed, starts the container, and returns the registry
// entry describing it.
func (o *orchestrator) launch(ctx context.Context, docker *dockerClient, plan provisionPlan) (provisionedService, []string, error) {
	var notes []string
	// Carried forward first, so how the image was built is reported even when the
	// build itself then fails and the caller only sees the error's context.
	notes = append(notes, plan.buildNotes...)

	if !plan.skipBuild {
		if _, err := docker.buildImage(ctx, plan.dir, plan.image, nil); err != nil {
			return provisionedService{}, nil, fmt.Errorf("building %s: %w", plan.image, err)
		}
	} else {
		notes = append(notes, "skip_build was set, so "+plan.image+" was started as it already existed")
	}

	ports := map[int]int{
		plan.controlPort: containerControlPort,
		plan.appPort:     containerAppPort,
	}
	if plan.aggregatorPort != 0 {
		ports[plan.aggregatorPort] = containerAggregatorPort
	}
	if plan.appMCPPort != 0 {
		ports[plan.appMCPPort] = containerAppMCPPort
	}

	id, err := docker.runContainer(ctx, runOptions{
		Name:  plan.container,
		Image: plan.image,
		Env:   plan.env,
		Ports: ports,
	})
	if err != nil {
		return provisionedService{}, nil, fmt.Errorf("starting %s: %w", plan.container, err)
	}

	return provisionedService{
		ServiceName:    plan.name,
		ContainerID:    id,
		Host:           plan.host,
		ControlPort:    plan.controlPort,
		AppPort:        plan.appPort,
		AggregatorPort: plan.aggregatorPort,
		AppMCPPort:     plan.appMCPPort,
		Image:          plan.image,
		CreatedAt:      time.Now().UTC(),
	}, notes, nil
}

// defaultProvisionWait is how long provisioning waits for the app port to answer.
//
// Generous, because the wait covers a Go binary's first start inside a fresh
// container on a machine that has just finished building it. Too short and every
// provision reports a false "not answering yet"; too long and a genuinely broken
// service holds the caller for minutes. Thirty seconds is past the former and well
// short of the latter.
const defaultProvisionWait = 30 * time.Second

// waitForApp polls the app address until it answers, and reports the status the
// registry listing would report.
//
// It polls the *app* port, not the control port. A control plane that answers proves
// only that breeze-mcp started; the question provisioning has to answer is whether
// the application is serving, and those two fail independently — a project that
// compiles and panics on startup has a working control plane and a dead app.
func (o *orchestrator) waitForApp(service provisionedService, waitSeconds int) (string, string) {
	wait := defaultProvisionWait
	switch {
	case waitSeconds < 0:
		return "starting", "wait_seconds was negative, so the application was not waited for; " +
			"its app_port may not answer yet"
	case waitSeconds > 0:
		wait = time.Duration(waitSeconds) * time.Second
	}

	deadline := time.Now().Add(wait)
	for {
		if httpAnswers(service.appURL()) {
			return "running", ""
		}
		if time.Now().After(deadline) {
			return "unhealthy", fmt.Sprintf("the application did not answer on %s within %s. "+
				"The container is running and recorded; use breeze_get_logs or docker logs %s to see why.",
				service.appURL(), wait, service.ContainerID)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// httpAnswers reports whether anything serves HTTP at a URL.
//
// Any status counts, including 404: the question is whether the process is listening
// and speaking HTTP, and a generated project whose "/" was replaced by the author
// still answers 404 from a perfectly healthy server. Requiring 200 would report
// those as broken.
//
// It does not go through fetchLiveJSON, which decodes a body and would report a
// healthy service serving HTML as malformed. The framework's own client is used for
// the reason live.go gives: this repository's tooling should exercise the client it
// ships.
func httpAnswers(url string) bool {
	c := client.New(client.Config{Timeout: liveTimeout})
	defer c.Close()

	resp, err := c.Do(client.NewRequest("GET", strings.TrimSuffix(url, "/")+"/", nil))
	return err == nil && resp != nil
}

// ─── list_provisioned_services ───────────────────────────────────────────────

// listedService is one entry in the listing.
//
// There is deliberately no token field of any kind. Not omitted-when-empty: absent
// from the type, so no code path and no future edit can populate one.
type listedService struct {
	ServiceName string `json:"service_name"`
	ContainerID string `json:"container_id"`
	Host        string `json:"host"`

	ControlPort int    `json:"control_port"`
	ControlURL  string `json:"control_url"`

	AppPort int    `json:"app_port"`
	AppURL  string `json:"app_url"`

	AggregatorPort int    `json:"aggregator_port,omitempty"`
	AggregatorURL  string `json:"aggregator_url,omitempty"`

	// AppMCPPort and AppMCPURL describe the embedded app-runtime endpoint, when the
	// service has one. Absent means it does not.
	AppMCPPort int    `json:"app_mcp_port,omitempty"`
	AppMCPURL  string `json:"app_mcp_url,omitempty"`

	Image  string `json:"image"`
	Status string `json:"status"`

	// Health is Docker's own health status when the image declares a health check,
	// and empty when it does not. Empty is not "unhealthy" — reporting a container
	// with no health check as unhealthy would be a finding this tool invented.
	Health string `json:"health,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}

// listResult is the whole listing.
type listResult struct {
	Services []listedService `json:"services"`
	Count    int             `json:"count"`

	// TokenNotice restates why no token appears here, at the exact moment a caller
	// is looking for one.
	TokenNotice string `json:"token_notice"`

	Notes []string `json:"notes,omitempty"`
}

func listProvisionedServicesTool(orch *orchestrator) *tool {
	return &tool{
		name: "list_provisioned_services",
		description: "List the services this orchestrator provisioned: name, container id, host, " +
			"control_port (its breeze-mcp control plane), app_port (the running application), " +
			"the aggregator address where one is hosted, and status. Never returns control_token — " +
			"that is issued once, by provision_service or provision_fleet.",
		schema: objectSchema(map[string]any{}),
		run: func(json.RawMessage) toolCallResult {
			return orch.listProvisioned()
		},
	}
}

func (o *orchestrator) listProvisioned() toolCallResult {
	reg, err := o.store()
	if err != nil {
		return errorResult(err.Error())
	}

	services := reg.list()
	result := listResult{
		Services:    make([]listedService, 0, len(services)),
		Count:       len(services),
		TokenNotice: tokenNotice,
	}

	// Docker is consulted for status, but its absence is a note rather than a
	// failure: the registry is still the answer to "what did I provision", and
	// refusing to report it because a daemon is down would lose the only record of
	// what needs cleaning up.
	docker, dockerErr := o.dockerOrError()
	if dockerErr != nil {
		result.Notes = append(result.Notes,
			"container status is unavailable ("+dockerErr.Error()+"); the addresses below are from the registry")
	}

	for _, service := range services {
		entry := listedService{
			ServiceName:    service.ServiceName,
			ContainerID:    service.ContainerID,
			Host:           service.Host,
			ControlPort:    service.ControlPort,
			ControlURL:     service.controlURL(),
			AppPort:        service.AppPort,
			AppURL:         service.appURL(),
			AggregatorPort: service.AggregatorPort,
			AggregatorURL:  service.aggregatorURL(),
			AppMCPPort:     service.AppMCPPort,
			AppMCPURL:      service.appMCPURL(),
			Image:          service.Image,
			Status:         "unknown",
			CreatedAt:      service.CreatedAt,
		}

		if dockerErr == nil {
			state, err := docker.inspectState(context.Background(), service.ContainerID)
			switch {
			case err != nil:
				// A container the registry lists and Docker does not know is worth
				// saying out loud: it was removed by something other than
				// deprovision_service, and its ports are still reserved.
				entry.Status = "missing"
				result.Notes = append(result.Notes, fmt.Sprintf(
					"%s is in the registry but Docker does not report it; deprovision_service will "+
						"clear the entry and release its ports", service.ServiceName))
			case state.Running:
				entry.Status = "running"
				entry.Health = state.Health
				if state.Health == "unhealthy" {
					entry.Status = "unhealthy"
				}
			default:
				entry.Status = "stopped"
				entry.Health = state.Health
			}
		}
		result.Services = append(result.Services, entry)
	}

	return structuredResult(fmt.Sprintf("%d provisioned service(s); control_token is not included",
		result.Count), result)
}

// ─── deprovision_service ─────────────────────────────────────────────────────

// deprovisionResult is what deprovision_service returns.
type deprovisionResult struct {
	ServiceName string `json:"service_name"`
	ContainerID string `json:"container_id"`

	// The released addresses are reported so a caller can see that the ports really
	// came back, and which ones.
	ReleasedControlPort    int `json:"released_control_port"`
	ReleasedAppPort        int `json:"released_app_port"`
	ReleasedAggregatorPort int `json:"released_aggregator_port,omitempty"`

	// ReleasedAppMCPPort is the app-runtime endpoint's port, when the service had
	// one. Reported for the same reason as the others: a port that came back needs to
	// be visible, or a caller cannot tell a released port from a leaked one.
	ReleasedAppMCPPort int `json:"released_app_mcp_port,omitempty"`

	ImageRemoved bool     `json:"image_removed"`
	Notes        []string `json:"notes,omitempty"`
}

func deprovisionServiceTool(orch *orchestrator) *tool {
	return &tool{
		name: "deprovision_service",
		description: "Stop and remove a container this orchestrator provisioned, and release its " +
			"control_port and app_port back to the port pool. Refuses, with an error, to touch any " +
			"container that is not in this orchestrator's own registry.",
		schema: objectSchema(map[string]any{
			"service_name": stringProp("The name provision_service or provision_fleet recorded."),
			"remove_image": boolProp("Also remove the image that was built for it. Defaults to false, " +
				"because an image is reusable and rebuilding one is the slowest part of provisioning."),
		}, "service_name"),
		run: func(raw json.RawMessage) toolCallResult {
			var a struct {
				ServiceName string `json:"service_name"`
				RemoveImage bool   `json:"remove_image"`
			}
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}
			return orch.deprovisionService(a.ServiceName, a.RemoveImage)
		},
	}
}

// deprovisionService removes one container this orchestrator created.
//
// The registry check comes first and is absolute. This orchestrator will not stop or
// remove a container it did not create, whatever the name resolves to on the daemon:
// a Docker daemon is shared, and a tool that removed anything by name could be
// pointed at a database that happens to be called "orders".
func (o *orchestrator) deprovisionService(name string, removeImage bool) toolCallResult {
	name = strings.TrimSpace(name)
	if name == "" {
		return errorResult("service_name is required")
	}

	reg, err := o.store()
	if err != nil {
		return errorResult(err.Error())
	}

	// Looked up before Docker is contacted, so a refusal costs nothing and cannot
	// be confused with a daemon problem.
	service, err := reg.get(name)
	if err != nil {
		return structuredErrorResult(err.Error(), map[string]any{
			"service_name": name,
			"refused":      true,
			"reason":       "not in this orchestrator's registry",
			"manages":      reg.names(),
		})
	}

	docker, err := o.dockerOrError()
	if err != nil {
		return errorResult(err.Error())
	}

	var notes []string
	ctx := context.Background()

	if err := docker.removeContainer(ctx, service.ContainerID); err != nil {
		// A container Docker cannot find is already gone — removed by hand, or by a
		// daemon restart with --rm. The registry entry still has to go, and its ports
		// still have to come back, or they stay reserved for the life of the process.
		if !strings.Contains(strings.ToLower(err.Error()), "no such container") {
			return errorResult(fmt.Sprintf("could not remove %s (%s): %v. The registry entry was kept, "+
				"so this can be retried.", name, service.ContainerID, err))
		}
		notes = append(notes, "Docker no longer had this container; the registry entry and its ports "+
			"were released anyway")
	}

	removed, err := reg.remove(name)
	if err != nil {
		return errorResult(fmt.Sprintf("the container was removed but the registry could not be "+
			"updated: %v. Its ports remain reserved until this is resolved.", err))
	}

	result := deprovisionResult{
		ServiceName:            removed.ServiceName,
		ContainerID:            removed.ContainerID,
		ReleasedControlPort:    removed.ControlPort,
		ReleasedAppPort:        removed.AppPort,
		ReleasedAggregatorPort: removed.AggregatorPort,
		ReleasedAppMCPPort:     removed.AppMCPPort,
		Notes:                  notes,
	}

	if removeImage {
		if err := docker.removeImage(ctx, removed.Image); err != nil {
			// Not a failure: an image shared with another tag or referenced by a
			// stopped container legitimately refuses removal, and the deprovision
			// itself succeeded.
			result.Notes = append(result.Notes,
				fmt.Sprintf("the image %s was left in place: %v", removed.Image, err))
		} else {
			result.ImageRemoved = true
		}
	}

	return structuredResult(fmt.Sprintf("%s removed; control port %d and app port %d released",
		name, removed.ControlPort, removed.AppPort), result)
}

// ─── provision_fleet ─────────────────────────────────────────────────────────

// fleetAggregatorConfig describes the aggregator to provision alongside the
// services.
type fleetAggregatorConfig struct {
	// HostedBy names which of the services runs the aggregator. Empty means the
	// first service in the list.
	//
	// The aggregator is hosted *by* a service rather than provisioned as a fourth
	// standalone container because that is what cmd/fleet-example does — the
	// aggregator is a Breeze app like any other — and because a hosted aggregator
	// makes the three-address distinction concrete: that one service has a control
	// port, an app port, and the aggregator's port, all different.
	HostedBy string `json:"hosted_by"`

	// IngestToken authenticates span writes. Generated when empty, and shared by
	// every service in the fleet: it is a write credential for one aggregator, which
	// is the same trust model cmd/fleet-example uses.
	IngestToken string `json:"ingest_token"`

	// ServiceToken lets the aggregator read each service's own logs when stitching a
	// trace's merged log panel. Generated when empty, and likewise shared.
	ServiceToken string `json:"service_token"`

	// SampleRate is passed through to each service's FleetConfig. Zero means 1.
	SampleRate float64 `json:"sample_rate"`
}

// fleetServiceRequest is one service in a fleet request.
type fleetServiceRequest struct {
	configInput
	Name     string `json:"name"`
	Template string `json:"template"`
}

// fleetResult is what provision_fleet returns.
type fleetResult struct {
	// Services carries a full entry per service, each with its own control_token.
	Services []provisionedResult `json:"services"`

	// Aggregator is stated separately and explicitly, because its address is the one
	// most easily confused with the hosting service's own two. It repeats which
	// service hosts it and names that service's other ports alongside, so the three
	// are visible together rather than having to be matched up by the reader.
	Aggregator fleetAggregatorAddress `json:"aggregator"`

	TokenNotice string   `json:"token_notice"`
	Notes       []string `json:"notes,omitempty"`
}

// fleetAggregatorAddress is the aggregator's own address, labelled against the
// hosting service's.
type fleetAggregatorAddress struct {
	HostedByService string `json:"hosted_by_service"`

	// AggregatorURL is what a Fleet tool's aggregator_url argument takes. The two
	// fields below it exist to make the distinction unmissable: they are the *same
	// container's* other two addresses, and neither of them is this one.
	AggregatorPort int    `json:"aggregator_port"`
	AggregatorURL  string `json:"aggregator_url"`

	HostServiceControlPort int `json:"host_service_control_port"`
	HostServiceAppPort     int `json:"host_service_app_port"`

	IngestToken string `json:"ingest_token"`
}

func provisionFleetTool(orch *orchestrator) *tool {
	return &tool{
		name: "provision_fleet",
		description: "Provision several services pre-wired for Fleet tracing, plus a Fleet Aggregator " +
			"hosted by one of them, in a single call. Returns every service's control_port, one-time " +
			"control_token and app_port, plus the aggregator's own separate address — which is neither " +
			"the hosting service's control port nor its app port.",
		schema: objectSchema(map[string]any{
			"services": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "object"},
				"description": "One object per service: name, plus config or config_yaml, plus optional template.",
			},
			"aggregator": map[string]any{
				"type":        "object",
				"description": "hosted_by (defaults to the first service), ingest_token, service_token, sample_rate.",
			},
			"docker": map[string]any{
				"type":        "object",
				"description": "Docker options applied to every service: host, env, wait_seconds.",
			},
		}, "services"),
		run: func(raw json.RawMessage) toolCallResult {
			var a struct {
				Services   []fleetServiceRequest `json:"services"`
				Aggregator fleetAggregatorConfig `json:"aggregator"`
				Docker     dockerOptions         `json:"docker"`
			}
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}
			return orch.provisionFleet(a.Services, a.Aggregator, a.Docker)
		},
	}
}

// maxFleetServices bounds how many services one provision_fleet call may ask for.
//
// This is a resource bound, not a naming rule. Each service in a fleet is a generated
// project, a `docker build` (up to 15 minutes each), three or four allocated ports, and
// a running container. A request for two hundred is not a fleet — it is a request to
// fill the port range, the disk and the Docker daemon from one tool call, and because
// provisioning is sequential the caller would be holding the orchestrator for hours
// while it happened.
//
// Twelve is above any fleet this repository's own examples or documentation describe
// (cmd/fleet-example has three) and far below the point where the call stops being
// something a person asked for. Someone who genuinely needs more can make a second
// call, which is the intended shape anyway: fleets are composed, not batch-submitted.
const maxFleetServices = 12

// provisionFleet provisions several services and an aggregator in one call.
//
// The ordering is the interesting part. The aggregator's port is allocated before
// any service is generated, because every service's Fleet configuration has to point
// at it — a service generated first and reconfigured later would be a second wiring
// path, which is exactly what this is meant not to have.
func (o *orchestrator) provisionFleet(
	requests []fleetServiceRequest,
	agg fleetAggregatorConfig,
	opts dockerOptions,
) toolCallResult {
	if len(requests) == 0 {
		return errorResult("services is required: provision_fleet provisions at least one service")
	}
	// Bounded before anything is allocated or built. Each service is a build, a
	// container and several ports, so the check belongs ahead of the first side
	// effect rather than in the loop that produces them.
	if len(requests) > maxFleetServices {
		return errorResult(fmt.Sprintf("services has %d entries; the limit is %d. Each service is "+
			"a generated project, a docker build and a running container, so one call asking for "+
			"more than this is a request to exhaust the port range and the daemon rather than to "+
			"provision a fleet. Split it across calls", len(requests), maxFleetServices))
	}

	reg, err := o.store()
	if err != nil {
		return errorResult(err.Error())
	}
	docker, err := o.dockerOrError()
	if err != nil {
		return errorResult(err.Error())
	}

	names := make([]string, 0, len(requests))
	for i, request := range requests {
		name := strings.TrimSpace(request.Name)
		if name == "" {
			return errorResult(fmt.Sprintf("services[%d] has no name", i))
		}
		// Same validation provision_service applies, on every service, before any
		// port is allocated. A fleet is the case where one bad name would otherwise
		// be discovered after several containers are already running.
		if err := validateServiceName(name); err != nil {
			return errorResult(fmt.Sprintf("services[%d]: %v", i, err))
		}
		if _, exists := reg.get(name); exists == nil {
			return errorResult(fmt.Sprintf("%q is already provisioned; deprovision it first", name))
		}
		names = append(names, name)
	}

	// The fleet's Docker options are shared by every service in it, so one check
	// covers all of them. ImageTag and ContainerName are per-service defaults here
	// rather than overrides, but the fields exist on the same struct and a caller
	// can set them — so they are checked rather than assumed unused.
	if err := validateDockerOptions(opts); err != nil {
		return errorResult(err.Error())
	}

	host := orDefault(opts.Host, "127.0.0.1")
	hostedBy := orDefault(agg.HostedBy, names[0])
	if !slices.Contains(names, hostedBy) {
		return errorResult(fmt.Sprintf("aggregator.hosted_by is %q, which is not one of the services "+
			"being provisioned (%s)", hostedBy, strings.Join(names, ", ")))
	}

	// From the same allocator as every control and app port, which is what makes a
	// collision between the three kinds impossible rather than unlikely.
	aggregatorPort, err := o.ports.allocate(portPurposeAggregator)
	if err != nil {
		return errorResult(err.Error())
	}

	ingestToken, serviceToken, err := fleetTokens(agg)
	if err != nil {
		o.ports.release(aggregatorPort)
		return errorResult(err.Error())
	}

	// Two spellings of one aggregator, because "where is it" has two answers.
	//
	// aggregatorURL is host-side: it is what the caller reads, and what a Fleet tool
	// running outside Docker connects to.
	//
	// writeURL is what the services use, and it cannot be the same value. Inside a
	// container, 127.0.0.1 is that container — a tracer pointed at the host loopback
	// exports spans into its own empty port and drops them, silently, since export is
	// asynchronous and a tracer does not fail a request because its buffer could not be
	// flushed. The symptom is a fleet that runs perfectly and records nothing.
	//
	// host.docker.internal is the host as seen from a container. It is native on Docker
	// Desktop and provided on Linux by the --add-host below, so one value works on both.
	aggregatorURL := fmt.Sprintf("http://%s:%d/fleet", host, aggregatorPort)
	writeURL := fmt.Sprintf("http://%s:%d/fleet", dockerHostAlias, aggregatorPort)

	plans := make([]provisionPlan, 0, len(requests))
	release := func() {
		o.ports.release(aggregatorPort)
		for _, plan := range plans {
			plan.releasePorts(o.ports)
			plan.cleanup()
		}
	}

	for i, request := range requests {
		cfg, err := request.resolve()
		if err != nil {
			release()
			return errorResult(fmt.Sprintf("services[%d] (%s) configuration: %v", i, names[i], err))
		}

		// The Fleet wiring, applied by setting the same FleetConfig `breeze add fleet`
		// sets and letting generator.ApplyConfig emit the block. Nothing here writes
		// tracer code.
		cfg.Fleet = fleetConfigFor(names[i], writeURL, agg.SampleRate, cfg.Fleet)

		aggregatorFor := 0
		if names[i] == hostedBy {
			aggregatorFor = aggregatorPort
		}

		serviceOpts := opts
		serviceOpts.Host = host
		serviceOpts.Env = fleetEnvFor(names[i], writeURL, ingestToken, serviceToken, opts.Env)

		plan, err := o.prepare(names[i], request.Template, cfg, serviceOpts, aggregatorFor)
		if err != nil {
			release()
			return errorResult(fmt.Sprintf("services[%d] (%s): %v", i, names[i], err))
		}
		plans = append(plans, plan)
	}

	result := fleetResult{
		Services:    make([]provisionedResult, 0, len(plans)),
		TokenNotice: tokenNotice,
	}

	// The aggregator's host is started first, so the other services have somewhere to
	// report to by the time they come up. A service whose first spans are dropped
	// because the aggregator was not listening yet produces a trace with holes in it,
	// which reads as a broken fleet rather than a startup order.
	sortHostFirst(plans, hostedBy)

	ctx := context.Background()
	for i := range plans {
		service, notes, err := o.launch(ctx, docker, plans[i])
		if err != nil {
			// Everything already started is torn down. A half-provisioned fleet is
			// worse than none: its traces are incomplete in a way that looks like a
			// tracing bug rather than a failed provision.
			result.Notes = append(result.Notes, o.rollbackFleet(ctx, docker, reg, result.Services)...)
			for j := i; j < len(plans); j++ {
				plans[j].releasePorts(o.ports)
				plans[j].cleanup()
			}
			o.ports.release(aggregatorPort)
			return structuredErrorResult(fmt.Sprintf("%s could not be started, so the fleet was rolled "+
				"back: %v", plans[i].name, err), result)
		}
		if err := reg.add(service); err != nil {
			_ = docker.removeContainer(ctx, service.ContainerID)
			result.Notes = append(result.Notes, o.rollbackFleet(ctx, docker, reg, result.Services)...)
			o.ports.release(aggregatorPort)
			return structuredErrorResult(fmt.Sprintf("%s started but could not be recorded, so the fleet "+
				"was rolled back: %v", plans[i].name, err), result)
		}

		status, healthNote := o.waitForApp(service, opts.WaitSeconds)
		if healthNote != "" {
			notes = append(notes, healthNote)
		}
		result.Services = append(result.Services, resultFor(service, plans[i].token, status, notes))
		plans[i].cleanup()
	}

	hostEntry, err := reg.get(hostedBy)
	if err != nil {
		return errorResult(err.Error())
	}
	result.Aggregator = fleetAggregatorAddress{
		HostedByService:        hostedBy,
		AggregatorPort:         aggregatorPort,
		AggregatorURL:          aggregatorURL,
		HostServiceControlPort: hostEntry.ControlPort,
		HostServiceAppPort:     hostEntry.AppPort,
		IngestToken:            ingestToken,
	}

	return structuredResult(fmt.Sprintf("%d service(s) provisioned; the Fleet Aggregator is hosted by "+
		"%s at %s, which is a different port from that service's control plane (%d) and its application (%d)",
		len(result.Services), hostedBy, aggregatorURL, hostEntry.ControlPort, hostEntry.AppPort), result)
}

// ─── fleet helpers ───────────────────────────────────────────────────────────

// fleetConfigFor builds the FleetConfig for one service in a fleet.
//
// It starts from whatever the caller configured and fills in what a fleet requires,
// rather than replacing the block: a caller that chose the ws transport or a specific
// service name keeps that choice, and only the aggregator address — which the caller
// cannot know, because it was allocated moments ago — is imposed.
//
// The http transport is the default because it is the one cmd/fleet-example's services
// use and the one that needs no additional endpoint.
func fleetConfigFor(name, aggregatorURL string, sampleRate float64, existing generator.FleetConfig) generator.FleetConfig {
	cfg := existing
	cfg.Enabled = true
	cfg.AggregatorURL = aggregatorURL

	if strings.TrimSpace(cfg.ServiceName) == "" {
		cfg.ServiceName = name
	}
	if strings.TrimSpace(cfg.Transport) == "" {
		cfg.Transport = "http"
	}
	if cfg.SampleRate == 0 {
		if sampleRate > 0 {
			cfg.SampleRate = sampleRate
		} else {
			// Everything, which is the right default for a fleet provisioned to be
			// looked at: a sampled trace that was dropped looks identical to a service
			// that never reported.
			cfg.SampleRate = 1
		}
	}
	return cfg
}

// fleetEnvFor builds the environment a fleet service needs.
//
// These are the same variable names cmd/fleet-example's services read and the same
// ones the generated Fleet block reads through fleetEnv, which is what makes a
// provisioned service configurable exactly like a hand-written one. Nothing here
// invents a variable.
func fleetEnvFor(name, aggregatorURL, ingestToken, serviceToken string, extra map[string]string) map[string]string {
	env := map[string]string{}
	for key, value := range extra {
		env[key] = value
	}

	env["FLEET_SERVICE_NAME"] = name
	env["FLEET_WRITE_URL"] = aggregatorURL
	env["FLEET_INGEST_TOKEN"] = ingestToken
	env["FLEET_SERVICE_TOKEN"] = serviceToken
	return env
}

// fleetTokens returns the fleet's shared ingest and service tokens, generating any
// the caller did not supply.
//
// Shared across the fleet, unlike a control token: they authenticate span writes and
// log reads against one aggregator, which is a fleet-wide relationship rather than a
// per-instance one. A control token is the opposite — it authorises changing one
// machine's code — which is why those are never shared.
func fleetTokens(agg fleetAggregatorConfig) (ingest, service string, err error) {
	ingest = strings.TrimSpace(agg.IngestToken)
	if ingest == "" {
		if ingest, err = NewToken(); err != nil {
			return "", "", err
		}
	}
	service = strings.TrimSpace(agg.ServiceToken)
	if service == "" {
		if service, err = NewToken(); err != nil {
			return "", "", err
		}
	}
	return ingest, service, nil
}

// sortHostFirst moves the aggregator's host to the front of the launch order.
//
// A stable single swap rather than a sort: the remaining services keep the order the
// caller listed them in, which is the order they will appear in the result and the
// order a reader expects.
func sortHostFirst(plans []provisionPlan, hostedBy string) {
	for i := range plans {
		if plans[i].name == hostedBy {
			plans[0], plans[i] = plans[i], plans[0]
			return
		}
	}
}

// rollbackFleet removes the services a failed fleet had already started.
//
// Failures during rollback are collected and returned as notes rather than replacing
// the original error: the caller needs to know why provisioning failed first, and
// which containers were left behind second.
func (o *orchestrator) rollbackFleet(
	ctx context.Context,
	docker *dockerClient,
	reg *provisionRegistry,
	started []provisionedResult,
) []string {
	var notes []string
	for _, service := range started {
		if err := docker.removeContainer(ctx, service.ContainerID); err != nil {
			notes = append(notes, fmt.Sprintf("rollback: %s (%s) could not be removed and is still "+
				"running: %v", service.ServiceName, service.ContainerID, err))
			continue
		}
		if _, err := reg.remove(service.ServiceName); err != nil {
			notes = append(notes, fmt.Sprintf("rollback: %s was removed but its registry entry was "+
				"not: %v", service.ServiceName, err))
		}
	}
	return notes
}
