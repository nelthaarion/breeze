package aggregator

// The service registry (§8.1.2): who is in the fleet, how many instances each
// service is running, and which of them have stopped reporting.
//
// # Why heartbeats instead of polling
//
// The aggregator could poll every service for health, but then it would need to
// know where they all are — a static list to maintain, and a wrong answer every
// time something is deployed, rescheduled, or scaled. Heartbeats invert that: a
// service announces itself, so the registry's membership is whatever is actually
// running right now. The cost is that "down" can only ever be inferred from
// silence, which is what ServiceTTL encodes.
//
// # Down means "stopped reporting", not "broken"
//
// A service marked down might be dead, or merely unable to reach the aggregator
// while happily serving traffic. The registry cannot tell those apart and does
// not pretend to — it reports the last time it heard from something, and the UI
// says so. Overstating this ("orders-service is DOWN") would send someone
// debugging the wrong process during an incident.

import (
	"sort"
	"sync"
	"time"

	"github.com/nelthaarion/breeze/v2/fleet"
)

// Service statuses, matching the dashboard Health page's existing
// green/yellow/red convention (§9.2) rather than inventing new vocabulary.
const (
	StatusUp       = "up"
	StatusDegraded = "degraded"
	StatusDown     = "down"
)

// degradedErrorRate is the error rate at which a reporting service is shown
// amber rather than green.
//
// Distinct from Config.BlastRadiusErrorRateThreshold on purpose: this one is
// cosmetic (what colour a node is), while that one triggers an incident banner
// and a graph traversal. Tying them together would mean any tuning of the
// incident threshold silently restyled the whole topology view.
const degradedErrorRate = 0.05

// ServiceInfo is one service's registry entry, as returned by
// GET /fleet/api/services.
type ServiceInfo struct {
	Name string `json:"name"`

	// Instances is how many distinct instance ids reported within
	// ServiceTTL. This is the number that makes a scaled deployment legible:
	// one service, six reporters.
	Instances int `json:"instances"`

	// Versions lists the distinct versions currently reporting, sorted. More
	// than one entry means a deploy is in progress — or stuck half-done,
	// which is worth seeing during an incident since it is a common cause of
	// the contract violations in §9A.
	Versions []string `json:"versions,omitempty"`

	// RPS and ErrorRate are summed/averaged across live instances, so they
	// describe the service as a whole rather than whichever replica reported
	// most recently.
	RPS       float64 `json:"rps"`
	ErrorRate float64 `json:"error_rate"`

	Status            string `json:"status"`
	LastHeartbeatUnix int64  `json:"last_heartbeat_unix"`

	// OpenAPIHash/OpenAPIURL are carried through from the heartbeat so the
	// schema registry (§9A.3) and catalog (§9C.3) can both notice a change
	// from one place, without either polling services themselves.
	OpenAPIHash string `json:"openapi_hash,omitempty"`
	OpenAPIURL  string `json:"openapi_url,omitempty"`
}

// instanceState is what the registry remembers about one reporter.
type instanceState struct {
	id          string
	version     string
	rps         float64
	errorRate   float64
	openAPIHash string
	openAPIURL  string
	lastSeen    time.Time
}

// serviceState holds every instance of one service.
type serviceState struct {
	name      string
	instances map[string]*instanceState
}

// ServiceRegistry tracks fleet membership from heartbeats.
//
// One mutex rather than sharding, unlike the span store: heartbeats arrive once
// per service instance per five seconds, so even a thousand-instance fleet is a
// few hundred writes a second against a map — orders of magnitude below where
// lock contention is measurable, and sharding would only add a way to get the
// instance-count arithmetic wrong.
type ServiceRegistry struct {
	cfg Config

	mu       sync.RWMutex
	services map[string]*serviceState

	// changed is set by any observation that alters what a reader would see,
	// so the WS hub can push a service_event only when something actually
	// happened (§8.1.6) instead of every tick.
	changed bool
}

func NewServiceRegistry(cfg Config) *ServiceRegistry {
	return &ServiceRegistry{
		cfg:      cfg.withDefaults(),
		services: make(map[string]*serviceState),
	}
}

// Observe records one heartbeat.
//
// Unnamed heartbeats are dropped rather than filed under "": a service that
// cannot say who it is contributes nothing to a registry whose entire purpose is
// attribution, and an empty-named entry would show up as a permanent mystery row
// in the UI.
func (r *ServiceRegistry) Observe(hb fleet.Heartbeat, now time.Time) {
	if hb.Service == "" {
		return
	}
	// An instance that does not identify itself still counts as one
	// reporter. Falling back to the service name means a single-instance
	// deployment that never set InstanceID reports as one instance rather
	// than zero — the common case for a small fleet, and misreporting it as
	// zero instances would make a live service look absent.
	instanceID := hb.InstanceID
	if instanceID == "" {
		instanceID = hb.Service
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	svc, ok := r.services[hb.Service]
	if !ok {
		svc = &serviceState{name: hb.Service, instances: make(map[string]*instanceState, 1)}
		r.services[hb.Service] = svc
		r.changed = true
	}
	inst, ok := svc.instances[instanceID]
	if !ok {
		inst = &instanceState{id: instanceID}
		svc.instances[instanceID] = inst
		r.changed = true
	}

	// A version or schema change is a state change worth pushing; a routine
	// rps update on an already-known instance is not, or the WS feed would
	// fire on every heartbeat from every service forever.
	if inst.version != hb.Version || inst.openAPIHash != hb.OpenAPIHash {
		r.changed = true
	}

	inst.version = hb.Version
	inst.rps = hb.RPS
	inst.errorRate = hb.ErrorRate
	inst.openAPIHash = hb.OpenAPIHash
	inst.openAPIURL = hb.OpenAPIURL
	inst.lastSeen = now
}

// Snapshot returns every known service, sorted by name.
//
// Sorted so the UI's service list does not reshuffle on every poll — Go map
// iteration order is randomized, and a table that reorders itself while someone
// is reading it is worse than useless during an incident.
func (r *ServiceRegistry) Snapshot(now time.Time) []ServiceInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]ServiceInfo, 0, len(r.services))
	for _, svc := range r.services {
		out = append(out, svc.info(now, r.cfg.ServiceTTL))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Service returns one service's entry.
func (r *ServiceRegistry) Service(name string, now time.Time) (ServiceInfo, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	svc, ok := r.services[name]
	if !ok {
		return ServiceInfo{}, false
	}
	return svc.info(now, r.cfg.ServiceTTL), true
}

// info aggregates one service's live instances. Caller holds at least RLock.
func (s *serviceState) info(now time.Time, ttl time.Duration) ServiceInfo {
	info := ServiceInfo{Name: s.name, Status: StatusDown}
	cutoff := now.Add(-ttl)

	var (
		versions      = make([]string, 0, 2)
		seenVersion   = make(map[string]struct{}, 2)
		weightedError float64
		lastSeen      time.Time
	)
	for _, inst := range s.instances {
		if inst.lastSeen.After(lastSeen) {
			lastSeen = inst.lastSeen
			// Schema fields come from the most recent reporter: mid-deploy
			// two instances disagree, and the newer one is the one whose
			// schema the fleet is converging on.
			info.OpenAPIHash = inst.openAPIHash
			info.OpenAPIURL = inst.openAPIURL
		}
		if !inst.lastSeen.After(cutoff) {
			// Stale instance: remembered (so a flapping pod is not
			// forgotten between beats) but excluded from every live
			// number, since counting a dead replica's last-known rps
			// would inflate the service's reported load indefinitely.
			continue
		}
		info.Instances++
		info.RPS += inst.rps

		// Error rate is weighted by each instance's rps, not averaged
		// flat: one idle replica at 100% errors must not drag a service
		// serving thousands of clean requests per second to a 50% error
		// rate, which a flat mean would do.
		weightedError += inst.errorRate * inst.rps
		if _, dup := seenVersion[inst.version]; !dup && inst.version != "" {
			seenVersion[inst.version] = struct{}{}
			versions = append(versions, inst.version)
		}
	}

	if info.RPS > 0 {
		info.ErrorRate = weightedError / info.RPS
	} else if info.Instances > 0 {
		// No traffic to weight by: fall back to the flat mean so an idle
		// service reporting errors is not shown as perfectly healthy.
		var sum float64
		for _, inst := range s.instances {
			if inst.lastSeen.After(cutoff) {
				sum += inst.errorRate
			}
		}
		info.ErrorRate = sum / float64(info.Instances)
	}

	sort.Strings(versions)
	info.Versions = versions
	if !lastSeen.IsZero() {
		info.LastHeartbeatUnix = lastSeen.Unix()
	}

	switch {
	case info.Instances == 0:
		info.Status = StatusDown
	case info.ErrorRate >= degradedErrorRate:
		info.Status = StatusDegraded
	default:
		info.Status = StatusUp
	}
	return info
}

// Sweep forgets instances that have been silent far longer than ServiceTTL, and
// services left with none.
//
// Two different cutoffs are at work, and conflating them would be a bug. Status
// uses ServiceTTL, so a service goes amber/red within seconds of going quiet and
// recovers the moment it returns. Sweep uses a much longer grace period, because
// deleting an entry is what makes a service *vanish from the UI* — and a
// restarting pod should be shown as down, not silently removed from the fleet
// while someone is watching it.
func (r *ServiceRegistry) Sweep(now time.Time) int {
	cutoff := now.Add(-r.forgetAfter())

	r.mu.Lock()
	defer r.mu.Unlock()

	removed := 0
	for name, svc := range r.services {
		for id, inst := range svc.instances {
			if !inst.lastSeen.After(cutoff) {
				delete(svc.instances, id)
				removed++
				r.changed = true
			}
		}
		if len(svc.instances) == 0 {
			delete(r.services, name)
			r.changed = true
		}
	}
	return removed
}

// forgetAfter is how long a silent instance is remembered before being deleted.
//
// Derived from ServiceTTL rather than configured separately: it is not an
// independent tuning knob, it is "long enough that a restart is visible as
// downtime", and one fewer config field is one fewer way to set up a registry
// that forgets services faster than it marks them down.
func (r *ServiceRegistry) forgetAfter() time.Duration {
	const multiple = 20 // 15s ServiceTTL → 5m memory
	d := r.cfg.ServiceTTL * multiple
	if min := time.Minute; d < min {
		d = min
	}
	return d
}

// TakeChanged reports whether the registry changed since the last call, and
// resets the flag.
//
// Read-and-clear in one locked operation so a change landing between a caller's
// read and its reset cannot be lost — that would mean a service appearing or
// disappearing without the UI ever being told.
func (r *ServiceRegistry) TakeChanged() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	was := r.changed
	r.changed = false
	return was
}

// Count returns the number of known services, live or not.
func (r *ServiceRegistry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.services)
}
