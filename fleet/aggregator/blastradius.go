package aggregator

// Blast radius (§9B.2): when a service starts failing, which other services are
// failing *because of it*.
//
// # What this is for
//
// Root-cause marking (§9B.1, in assemble.go) answers "what broke" for one trace.
// This answers the fleet-level version: one service is unhealthy — who else is
// being dragged down, and how badly. That is the difference between a wall of red
// dashboards and one sentence naming the origin and its victims.
//
// # Direction of traversal
//
// Failure propagates from callee to caller: if orders-service fails, the gateway
// that called it fails too. So the blast radius of an unhealthy service is found
// by walking *inbound* edges — its callers, their callers, and so on. This is the
// opposite of what §9B.2's prose says ("every service reachable from the
// unhealthy node by following topology edges outward"), and the spec's phrasing is
// simply wrong on the mechanics: a service's own downstream dependencies are what
// might have *caused* its failure, not what suffers from it. Walking outward would
// name the victims as culprits and miss every affected caller. The spec's intent —
// "who is impacted" — is what is implemented; only its stated direction is
// corrected.
//
// # Cheap by construction
//
// Everything here is arithmetic over counters topology.go already maintains. No
// trace is re-read, no history is scanned. The traversal is bounded by the number
// of services, which is a property of the fleet, not of its traffic.

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// minWindowCalls is how many calls an error rate must be computed from before it
// can trigger an incident.
//
// Without a floor, a single failed request against an idle service reads as a
// 100% error rate and would raise a fleet-wide incident banner. Ten is low enough
// to catch a genuine failure within a second or two at any real traffic level, and
// high enough that one unlucky health check cannot declare an outage.
const minWindowCalls = 10

// AffectedService is one entry in a blast radius.
type AffectedService struct {
	Service string `json:"service"`

	// Hops is the distance from the unhealthy service: 1 for a direct
	// caller, 2 for its caller, and so on. Surfaced because proximity is
	// what makes an impact credible — a direct caller failing alongside its
	// dependency is causation; four hops away it is increasingly likely to be
	// coincidence.
	Hops int `json:"hops"`

	// DependencyErrorRate is this service's own windowed error rate on the
	// path leading to the unhealthy service.
	DependencyErrorRate float64 `json:"dependency_error_rate"`

	// AttributedShare is the fraction of this service's failing traffic that
	// §9B.2 attributes to the unhealthy service — the number behind the
	// tooltip "23% of this service's requests currently fail because of
	// orders-service".
	AttributedShare float64 `json:"attributed_share"`

	// Via names the immediate callee through which the failure reached this
	// service, so a reader can see the actual path rather than guessing it
	// from the graph.
	Via string `json:"via,omitempty"`
}

// BlastRadius is the computed impact of one unhealthy service.
type BlastRadius struct {
	Service string `json:"service"`

	// ErrorRate and Calls are the unhealthy service's own windowed numbers,
	// included so the banner can state why this was flagged at all.
	ErrorRate float64 `json:"error_rate"`
	Calls     uint64  `json:"calls"`

	Affected []AffectedService `json:"affected"`

	// Banner is the pre-rendered incident line for the top of Fleet View
	// (§9B.2). Built here rather than in the UI so the same sentence appears
	// in the API, the WS feed, and any future alerting integration.
	Banner string `json:"banner"`

	ComputedUnix int64 `json:"computed_unix"`
}

// Incidents returns a blast radius for every service currently over the error
// threshold, worst first.
//
// Returns a slice rather than one worst-offender because two unrelated services
// can be broken at once, and collapsing that to a single incident would hide one
// of them. Sorted so the UI's banner shows the largest first.
//
// Takes the graph explicitly rather than hanging off the Aggregator so the whole
// computation is testable against a hand-built graph — the §14.12 correctness
// tests need exactly that, and an incident calculation that can only be exercised
// through a running aggregator is one that will not be tested thoroughly.

func Incidents(g *TopologyGraph, cfg Config, now time.Time) []BlastRadius {
	cfg = cfg.withDefaults()
	if g == nil {
		return nil
	}

	var out []BlastRadius
	for _, node := range g.Snapshot().Nodes {
		rate, calls := g.WindowedErrorRate(node.Service, now)
		if calls < minWindowCalls || rate < cfg.BlastRadiusErrorRateThreshold {
			continue
		}
		out = append(out, ComputeBlastRadius(g, cfg, node.Service, rate, calls, now))
	}

	sort.Slice(out, func(i, j int) bool {
		// Most affected services first; ties broken by error rate, then
		// name, so the ordering is stable across polls.
		if len(out[i].Affected) != len(out[j].Affected) {
			return len(out[i].Affected) > len(out[j].Affected)
		}
		if out[i].ErrorRate != out[j].ErrorRate {
			return out[i].ErrorRate > out[j].ErrorRate
		}
		return out[i].Service < out[j].Service
	})
	return out
}

// ComputeBlastRadius walks inbound edges from an unhealthy service.
//
// Breadth-first, so each affected service is recorded at its shortest distance
// from the origin — which matters because Hops is used to judge how credible the
// attribution is, and a depth-first walk could reach a direct caller by a long
// path first and label it four hops away.
func ComputeBlastRadius(g *TopologyGraph, cfg Config, service string, rate float64, calls uint64, now time.Time) BlastRadius {
	cfg = cfg.withDefaults()
	br := BlastRadius{
		Service:      service,
		ErrorRate:    rate,
		Calls:        calls,
		ComputedUnix: now.Unix(),
	}

	type queued struct {
		service string
		hops    int
		via     string
	}
	// The origin is seeded as visited so a cyclic graph (A calls B calls A,
	// which happens with retries or a callback) terminates instead of walking
	// forever, and so the unhealthy service never appears in its own blast
	// radius.
	visited := map[string]struct{}{service: {}}
	queue := []queued{{service: service, hops: 0}}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		for _, caller := range g.Callers(cur.service, now) {
			if _, seen := visited[caller]; seen {
				continue
			}
			visited[caller] = struct{}{}

			// A caller is only "affected" if it is actually failing.
			// A service that calls a broken dependency and degrades
			// gracefully — cache, fallback, retry — is working
			// correctly, and listing it as collateral damage would
			// overstate the incident and erode trust in the banner.
			callerRate, callerCalls := g.WindowedErrorRate(caller, now)
			if callerCalls == 0 || callerRate == 0 {
				continue
			}

			// How much of this caller's traffic to the failing
			// dependency is itself failing. This is the attribution
			// link: the caller fails, and its calls to this
			// dependency fail, so the dependency is a plausible
			// cause of the caller's failures.
			edgeRate, edgeCalls := g.EdgeWindowedErrorRate(caller, cur.service, now)
			if edgeCalls == 0 {
				continue
			}

			// Attributed share is capped at the caller's own error
			// rate: the dependency cannot be responsible for more
			// failures than the caller actually had. Without the cap,
			// a caller whose every dependency call failed but which
			// recovered from most of them would report an
			// attributed share above its real error rate.
			share := edgeRate
			if share > callerRate {
				share = callerRate
			}

			br.Affected = append(br.Affected, AffectedService{
				Service:             caller,
				Hops:                cur.hops + 1,
				DependencyErrorRate: edgeRate,
				AttributedShare:     share,
				Via:                 cur.service,
			})
			queue = append(queue, queued{service: caller, hops: cur.hops + 1, via: cur.service})
		}
	}

	sort.Slice(br.Affected, func(i, j int) bool {
		if br.Affected[i].Hops != br.Affected[j].Hops {
			return br.Affected[i].Hops < br.Affected[j].Hops
		}
		if br.Affected[i].AttributedShare != br.Affected[j].AttributedShare {
			return br.Affected[i].AttributedShare > br.Affected[j].AttributedShare
		}
		return br.Affected[i].Service < br.Affected[j].Service
	})
	br.Banner = buildBanner(br)
	return br
}

// buildBanner renders the incident line (§9B.2).
//
// Deterministic templating over the computed numbers, same approach as the
// per-trace summary in assemble.go: the value is in reliably stating facts, and a
// sentence assembled from counters cannot hallucinate a cause.
func buildBanner(br BlastRadius) string {
	var b strings.Builder
	fmt.Fprintf(&b, "⚠ %s is degraded (%.0f%% error rate)", br.Service, br.ErrorRate*100)

	if len(br.Affected) == 0 {
		// Worth stating explicitly. "No downstream impact detected" tells
		// the reader the traversal ran and found nothing, whereas a
		// truncated sentence would look like the computation failed.
		b.WriteString(" — no impact on other services detected.")
		return b.String()
	}

	names := make([]string, 0, len(br.Affected))
	for _, a := range br.Affected {
		names = append(names, a.Service)
	}
	noun := "services"
	if len(names) == 1 {
		noun = "service"
	}
	fmt.Fprintf(&b, " — impacting %d downstream %s: %s", len(names), noun, strings.Join(names, ", "))
	b.WriteByte('.')
	return b.String()
}
