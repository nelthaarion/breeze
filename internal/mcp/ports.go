package mcp

// ports.go — one allocator for every port this orchestrator hands out.
//
// There are four pools in play — control ports, app ports, app-level MCP ports, and
// the Fleet Aggregator's own port — and the reason they share one allocator is that
// nothing else keeps them apart. A per-pool allocator with per-pool ranges would work
// right up until someone widened a range, and the failure would be a container
// that starts, binds nothing, and reports itself unhealthy for a reason nothing in
// the registry explains.
//
// So: one allocator, one set of handed-out numbers, and a release path that
// returns a number to the pool only when the thing holding it is genuinely gone.
//
// # Why a port is probed rather than only tracked
//
// Tracking alone is not enough, because this orchestrator is not the only thing on
// the machine. A port it has never used may still be held by a database, another
// orchestrator, or a container someone started by hand. So a candidate must be
// both unseen *and* bindable before it is handed out.
//
// The probe is a real bind on the loopback interface, immediately closed. That is
// racy in principle — something else could take the port between the close and the
// container's own bind — and it is the same race every port allocator has,
// including Docker's own. What makes it tolerable is that the loser finds out
// immediately: the container fails to start, and provision_service reports that
// rather than returning an address that does not work.

import (
	"fmt"
	"net"
	"sync"
)

// The default range is the lower half of the IANA dynamic/private range, which is
// where an ephemeral service port belongs and which does not collide with the
// registered ports a developer's own services tend to use.
//
// It is deliberately far from 2000 (the control port in every example) and from
// 3000/8080 (the app ports), so a hand-started instance and a provisioned one do
// not compete for the same numbers.
const (
	defaultPortRangeStart = 49152
	defaultPortRangeEnd   = 60999
)

// Port purposes, spelled once. They are labels for diagnostics, not behaviour: the
// allocator treats every pool identically, which is the property that makes a
// collision between pools impossible rather than merely unlikely.
const (
	portPurposeControl    = "control"
	portPurposeApp        = "app"
	portPurposeAggregator = "aggregator"
	portPurposeExternal   = "external"

	// portPurposeAppMCP is the in-process app-level MCP endpoint a provisioned
	// service may also expose.
	//
	// It is a fourth distinct purpose rather than a reuse of portPurposeControl,
	// because the two answer different questions and carry different capabilities.
	// The control port serves a generator-level breeze-mcp for that container: it can
	// scaffold and rewrite the project. This one serves the app-runtime endpoint
	// embedded in the running service: read-only introspection of that instance, with
	// no mutating tool registered (see mode.go).
	//
	// Naming them apart matters when something goes wrong. "Which port can generate
	// code" is the first question in an incident, and a registry that spelled both
	// "control" could not answer it.
	portPurposeAppMCP = "app-mcp"
)

// portAllocator hands out ports and remembers every one it has handed out.
type portAllocator struct {
	mu sync.Mutex

	start, end int

	// held is every port currently assigned, whatever pool it belongs to. The
	// value is the purpose, kept only so a diagnostic can say what a port is for —
	// the first thing anyone asks when a port turns up in a registry they did not
	// expect.
	held map[int]string

	// probe reports whether a port can be bound. It is a field so a test can
	// simulate a machine where a port is taken without actually taking one, and so
	// the production path has exactly one implementation of the bind.
	probe func(port int) bool
}

// newPortAllocator returns an allocator over the default range.
func newPortAllocator() *portAllocator {
	return &portAllocator{
		start: defaultPortRangeStart,
		end:   defaultPortRangeEnd,
		held:  make(map[int]string, 16),
		probe: portIsFree,
	}
}

// reserve marks a port as held without allocating it.
//
// This is what makes registry persistence safe: on restart the orchestrator reads
// back the ports its containers are already using and reserves them, so the
// allocator's memory survives even though the allocator itself does not.
func (a *portAllocator) reserve(port int, purpose string) {
	if port <= 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.held[port] = purpose
}

// allocate returns a free port, marked with what it is for.
//
// The scan is linear from the start of the range rather than random. A random probe
// would spread allocations across the range and make two consecutive provisions
// produce unrelated numbers; sequential means a fleet's ports are adjacent and
// readable, which matters when a human is reading a registry.
func (a *portAllocator) allocate(purpose string) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	for port := a.start; port <= a.end; port++ {
		if _, taken := a.held[port]; taken {
			continue
		}
		if !a.probe(port) {
			// Held by something outside this orchestrator. Recording it prevents
			// re-probing the same occupied port on every later allocation, which on
			// a busy machine turns an O(1) allocation into an O(range) one.
			a.held[port] = portPurposeExternal
			continue
		}
		a.held[port] = purpose
		return port, nil
	}
	return 0, fmt.Errorf("mcp: no free port in %d-%d; %d are in use",
		a.start, a.end, len(a.held))
}

// allocateN allocates several ports at once, releasing all of them if any fails.
//
// Provisioning needs a control port and an app port together, and a fleet needs
// three at a time. Allocating them one at a time and failing half-way would leak
// the ones already taken, and the leak would only ever be visible as a range that
// slowly fills up over a long-running orchestrator's lifetime.
func (a *portAllocator) allocateN(purposes ...string) ([]int, error) {
	ports := make([]int, 0, len(purposes))
	for _, purpose := range purposes {
		port, err := a.allocate(purpose)
		if err != nil {
			for _, allocated := range ports {
				a.release(allocated)
			}
			return nil, err
		}
		ports = append(ports, port)
	}
	return ports, nil
}

// release returns a port to the pool.
//
// Only deprovisioning calls this, and only after the container is gone. Releasing a
// port whose container is still running would hand it to the next provision, which
// would then fail to bind — and the failure would be attributed to the new service
// rather than to the stale one holding its port.
func (a *portAllocator) release(port int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.held, port)
}

// purposeOf reports what a port was allocated for, or "" when it is not held.
func (a *portAllocator) purposeOf(port int) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.held[port]
}

// portIsFree reports whether a port can be bound right now.
//
// Loopback rather than every interface: binding 0.0.0.0 to test a port would fail
// on a machine where something holds the port on one interface only, and would
// report a usable port as taken. A container's published port is bound by Docker on
// the host, and Docker's bind is what this is predicting.
func portIsFree(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	// The close can fail in principle; if it does, the port is not usable and
	// reporting it free would hand out something unbindable.
	return ln.Close() == nil
}
