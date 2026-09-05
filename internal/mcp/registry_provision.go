package mcp

// registry_provision.go — the orchestrator's record of what it created.
//
// # Why this is persisted
//
// Everything else in this package is stateless: a tool call reads a tree or runs a
// generator and forgets. Provisioning is not, because it creates something that
// outlives the process. An orchestrator that lost its registry on restart would
// still have running containers it could no longer name, could no longer report,
// and — critically — could no longer safely remove, since deprovision_service
// refuses to touch anything the registry does not list. The containers would have
// to be cleaned up by hand, by someone reading `docker ps` and guessing which
// entries were ours.
//
// So the registry is a file. A JSON file, written whole on every change, because
// the alternatives all cost more than they are worth here: an embedded key-value
// store is a new dependency for a table with tens of rows, and an append-only log
// needs compaction to avoid growing forever. Whole-file writes are O(registry) per
// change and the registry is small by construction — one entry per container this
// orchestrator manages.
//
// The write is atomic: a temporary file in the same directory, then a rename. A
// half-written registry is the one failure that would be worse than no registry at
// all, because it would parse as a shorter list and quietly orphan whatever fell
// off the end.
//
// # What is deliberately absent
//
// There is no token field. Not an empty one, not an omitempty one — none. A
// control token is returned exactly once, by the call that provisioned the
// service, and the way to guarantee it is never re-exposed is for it never to be
// stored. That makes "lose the token and you must deprovision" a property of the
// design rather than a policy someone has to remember to enforce.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// registryFileName is the file, kept next to the orchestrator's own binary.
//
// Next to the binary rather than in the working directory: an MCP client starts
// this process from wherever it happens to be, and a registry that moved with the
// working directory would be a different registry on every launch — which is the
// same failure as having no registry, arriving unpredictably.
const registryFileName = "breeze-mcp-provisioned.json"

// provisionedService is one entry: what was created, where it listens, and nothing
// else.
//
// The three addresses are named separately and none of them is called "port". A
// single ambiguous field is how an agent ends up sending a tool call to an
// application or an HTTP request to a control plane.
type provisionedService struct {
	ServiceName string `json:"service_name"`
	ContainerID string `json:"container_id"`

	// Host is where all of this service's published ports are reachable, from the
	// orchestrator's point of view. It is recorded rather than assumed because a
	// remote Docker daemon publishes on its own host, not on this one.
	Host string `json:"host"`

	// ControlPort is the service's own breeze-mcp instance: the address an agent
	// adds to its client configuration as a named server.
	ControlPort int `json:"control_port"`

	// AppPort is the Breeze application's own bind address, as published on Host.
	// This is what a Category C/D tool's service_url argument points at.
	AppPort int `json:"app_port"`

	// AggregatorPort is set only on the service hosting a Fleet Aggregator, and is
	// that aggregator's own endpoint — not this service's control port and not its
	// app port. Zero means this service hosts no aggregator.
	AggregatorPort int `json:"aggregator_port,omitempty"`

	// AppMCPPort is the application's own embedded app-runtime MCP endpoint, when it
	// has one. Zero means it does not.
	//
	// A fourth distinct address, and the distinction from ControlPort is the whole
	// reason it exists: ControlPort serves the generator-level toolchain over this
	// container's source tree, so whoever holds its token can rewrite the project.
	// This one serves read-only introspection of the running process, with no
	// mutating tool registered at all. An agent that wants to know what the service
	// is doing right now should be pointed here; one that wants to change the project
	// needs ControlPort. Conflating them hands out far more capability than intended.
	AppMCPPort int `json:"app_mcp_port,omitempty"`

	// Image is the tag that was built, so a caller can identify the container
	// outside this registry, and so deprovisioning can remove what it created.
	Image string `json:"image"`

	// CreatedAt orders the registry and answers "which of these is stale".
	CreatedAt time.Time `json:"created_at"`
}

// controlURL renders the control address as a client configuration would name it.
func (s provisionedService) controlURL() string {
	return fmt.Sprintf("http://%s:%d%s", s.Host, s.ControlPort, DefaultEndpointPath)
}

// appURL renders the app address as a service_url argument would name it.
func (s provisionedService) appURL() string {
	return fmt.Sprintf("http://%s:%d", s.Host, s.AppPort)
}

// aggregatorURL renders the aggregator address as an aggregator_url argument would
// name it, or "" when this service hosts none.
//
// The /fleet suffix matches the aggregator's own default base path, which is what
// cmd/fleet-aggregator serves and what the Fleet tools expect.
func (s provisionedService) aggregatorURL() string {
	if s.AggregatorPort == 0 {
		return ""
	}
	return fmt.Sprintf("http://%s:%d/fleet", s.Host, s.AggregatorPort)
}

// appMCPURL renders the embedded app-runtime endpoint's URL, or "" when the service
// has none.
//
// Same endpoint path as controlURL because it is the same protocol on the same
// framework — what differs is which server answers and what it is allowed to do.
func (s provisionedService) appMCPURL() string {
	if s.AppMCPPort == 0 {
		return ""
	}
	return fmt.Sprintf("http://%s:%d%s", s.Host, s.AppMCPPort, DefaultEndpointPath)
}

// provisionRegistry is the persisted table.
type provisionRegistry struct {
	mu sync.Mutex

	path     string
	services map[string]provisionedService

	// ports is the allocator whose memory this registry restores on load. They are
	// held together because a registry entry and a port reservation must appear and
	// disappear as one thing: an entry without its reservation would let the next
	// provision take a running container's port.
	ports *portAllocator
}

// registryPath returns the registry file's location: beside the running binary,
// falling back to the working directory when the executable's path cannot be
// determined.
//
// Beside the binary rather than in the working directory because an MCP client
// starts this process from wherever it happens to be, and a registry that moved
// with the working directory would be a different registry on every launch — the
// same failure as having no registry, arriving unpredictably.
func registryPath() string {
	exe, err := os.Executable()
	if err != nil {
		return registryFileName
	}
	return filepath.Join(filepath.Dir(exe), registryFileName)
}

// loadProvisionRegistry reads the registry, or returns an empty one when the file
// does not exist yet.
//
// A missing file is the first-run case and not an error. A file that exists but
// cannot be parsed *is* an error, and a loud one: the safe-looking alternative —
// start empty — would abandon every container the previous process created, which
// is exactly what persistence exists to prevent.
func loadProvisionRegistry(path string, ports *portAllocator) (*provisionRegistry, error) {
	if path == "" {
		path = registryPath()
	}
	if ports == nil {
		ports = newPortAllocator()
	}

	reg := &provisionRegistry{
		path:     path,
		services: make(map[string]provisionedService, 8),
		ports:    ports,
	}

	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return reg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("mcp: cannot read the provisioning registry at %s: %w", path, err)
	}

	var entries []provisionedService
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf(
			"mcp: the provisioning registry at %s is unreadable (%w). It lists the "+
				"containers this orchestrator created and is the only thing that authorises removing them; "+
				"repair or move it rather than deleting it, or those containers must be cleaned up by hand",
			path,
			err,
		)
	}

	for _, entry := range entries {
		if entry.ServiceName == "" {
			// An entry with no name cannot be looked up, deprovisioned, or reported.
			// Skipping it would hide it; refusing makes the corruption visible while
			// the rest of the file is still intact.
			return nil, fmt.Errorf(
				"mcp: the provisioning registry at %s contains an entry with no "+
					"service_name, so it cannot be managed",
				path,
			)
		}
		reg.services[entry.ServiceName] = entry

		// These reservations are what make the allocator's memory survive a restart.
		// Without them the next provision could hand out a port a running container
		// is already publishing on.
		reg.ports.reserve(entry.ControlPort, portPurposeControl)
		reg.ports.reserve(entry.AppPort, portPurposeApp)
		reg.ports.reserve(entry.AggregatorPort, portPurposeAggregator)
		reg.ports.reserve(entry.AppMCPPort, portPurposeAppMCP)
	}
	return reg, nil
}

// add records a service and persists the registry.
//
// The port reservations are not made here: the caller allocated them from the same
// allocator before starting the container, so they are already held. Reserving them
// again would be harmless but would obscure where a port's lifetime begins.
func (r *provisionRegistry) add(service provisionedService) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if service.ServiceName == "" {
		return errors.New("mcp: a provisioned service needs a name")
	}
	if _, exists := r.services[service.ServiceName]; exists {
		return fmt.Errorf(
			"mcp: %q is already provisioned by this orchestrator; deprovision it first",
			service.ServiceName,
		)
	}

	r.services[service.ServiceName] = service
	if err := r.persistLocked(); err != nil {
		// Roll back the in-memory half so the two cannot disagree. A registry that
		// remembers a service its file does not would forget it on the next restart,
		// which is the orphaned-container case again.
		delete(r.services, service.ServiceName)
		return err
	}
	return nil
}

// remove deletes an entry, releases its ports, and persists.
//
// The order matters: the file is written before the ports are released, so a crash
// between the two leaves ports held by nothing — recoverable by restarting — rather
// than a container still in the file whose ports have been handed to someone else.
func (r *provisionRegistry) remove(name string) (provisionedService, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	service, ok := r.services[name]
	if !ok {
		return provisionedService{}, r.notInRegistryLocked(name)
	}

	delete(r.services, name)
	if err := r.persistLocked(); err != nil {
		r.services[name] = service
		return provisionedService{}, err
	}

	r.ports.release(service.ControlPort)
	r.ports.release(service.AppPort)
	if service.AggregatorPort != 0 {
		r.ports.release(service.AggregatorPort)
	}
	if service.AppMCPPort != 0 {
		r.ports.release(service.AppMCPPort)
	}
	return service, nil
}

// get returns one entry.
func (r *provisionRegistry) get(name string) (provisionedService, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	service, ok := r.services[name]
	if !ok {
		return provisionedService{}, r.notInRegistryLocked(name)
	}
	return service, nil
}

// list returns every entry, oldest first.
//
// Sorted by creation time rather than by name, because the question a caller
// usually has is "what did I start, and in what order" — and a name sort would put
// a fleet's aggregator in the middle of its services.
func (r *provisionRegistry) list() []provisionedService {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]provisionedService, 0, len(r.services))
	for _, service := range r.services {
		out = append(out, service)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ServiceName < out[j].ServiceName
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// names reports the registered service names, sorted, for error messages.
func (r *provisionRegistry) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.namesLocked()
}

func (r *provisionRegistry) namesLocked() []string {
	out := make([]string, 0, len(r.services))
	for name := range r.services {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// notInRegistryLocked is the refusal that makes this registry the sole authority
// over what may be torn down.
//
// It names what *is* registered, because the common cause is a caller working from
// its own notes rather than from list_provisioned_services — and because the other
// reading of the failure ("the container is gone") is the one that leads to someone
// removing a container by hand.
func (r *provisionRegistry) notInRegistryLocked(name string) error {
	known := r.namesLocked()
	if len(known) == 0 {
		return fmt.Errorf(
			"this orchestrator has not provisioned %q — its registry is empty, so it "+
				"will not stop or remove any container",
			name,
		)
	}
	return fmt.Errorf("this orchestrator has not provisioned %q, so it will not touch it; "+
		"it manages: %s", name, strings.Join(known, ", "))
}

// persistLocked writes the registry atomically.
//
// Temporary file in the same directory, sync, rename. A half-written registry is
// the one failure worse than no registry at all: it would parse as a shorter list
// and quietly orphan whatever fell off the end.
func (r *provisionRegistry) persistLocked() error {
	entries := make([]provisionedService, 0, len(r.services))
	for _, service := range r.services {
		entries = append(entries, service)
	}
	sort.Slice(
		entries,
		func(i, j int) bool { return entries[i].ServiceName < entries[j].ServiceName },
	)

	// Indented, because reading this file by hand is a supported recovery path and
	// one long line is not readable.
	payload, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("mcp: cannot encode the provisioning registry: %w", err)
	}

	dir := filepath.Dir(r.path)
	tmp, err := os.CreateTemp(dir, ".breeze-mcp-registry-*")
	if err != nil {
		return fmt.Errorf("mcp: cannot write the provisioning registry in %s: %w", dir, err)
	}
	name := tmp.Name()

	if _, err := tmp.Write(payload); err != nil {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("mcp: cannot write the provisioning registry: %w", err)
	}
	// Sync before rename: the rename is atomic with respect to readers, but the
	// bytes it points at still have to have reached the disk for the guarantee to
	// survive a power loss rather than only a process crash.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("mcp: cannot flush the provisioning registry: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return fmt.Errorf("mcp: cannot close the provisioning registry: %w", err)
	}

	if err := os.Rename(name, r.path); err != nil {
		os.Remove(name)
		return fmt.Errorf("mcp: cannot replace the provisioning registry at %s: %w", r.path, err)
	}
	return nil
}
