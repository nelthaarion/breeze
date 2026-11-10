package mcp

// tools_fleet_services.go — what the Fleet aggregator knows that no tool read.
//
// Two things were missing, and the gap between them is the shape of the commonest
// microservice failure.
//
// A service that is down cannot report that it is. It exports no spans, so a trace that
// reaches it stops at its caller, and the aggregator's own explanation of the failure
// names the caller: "auth-service returned 502". The aggregator does know the truth. Its
// service registry marks the dead service down within seconds, because heartbeats stopped
// arriving. But no tool read the registry, so the one fact that turns "auth-service is
// failing" into "auth-service's dependency is gone" was reachable through the dashboard
// and not through an agent.
//
// And the aggregator can read every service's logs for a trace on its own, from the
// addresses services heartbeat. Without a tool for that, an agent had to know each
// service's URL and ask them one at a time, which is the thing a fleet-level view is for
// avoiding.
//
// Both tools are reads of endpoints the dashboard already uses.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// fleetNow is the clock silence is measured against. A variable so a test can fix it.
var fleetNow = time.Now

// The registry's statuses, mirrored from the aggregator for the same reason the types
// are: this package reads the aggregator's JSON and does not import it.
const (
	fleetStatusUp       = "up"
	fleetStatusDegraded = "degraded"
	fleetStatusDown     = "down"
)

// ── mirrors of the aggregator's JSON ─────────────────────────────────────────

// fleetService mirrors aggregator.ServiceInfo.
type fleetService struct {
	Name              string   `json:"name"`
	Instances         int      `json:"instances"`
	Versions          []string `json:"versions,omitempty"`
	RPS               float64  `json:"rps"`
	ErrorRate         float64  `json:"error_rate"`
	Status            string   `json:"status"`
	LastHeartbeatUnix int64    `json:"last_heartbeat_unix"`
	OpenAPIURL        string   `json:"openapi_url,omitempty"`
}

// fleetTraceLogLine mirrors aggregator.TraceLog: a dashboard log line, attributed.
type fleetTraceLogLine struct {
	liveLogEntry
	Service string `json:"service"`
}

// fleetLogSource mirrors aggregator.LogSource.
type fleetLogSource struct {
	Service   string `json:"service"`
	Available bool   `json:"available"`
	Error     string `json:"error,omitempty"`
	Count     int    `json:"count"`
}

// fleetTraceLogs mirrors aggregator.TraceLogs.
type fleetTraceLogs struct {
	TraceID   string              `json:"trace_id"`
	Logs      []fleetTraceLogLine `json:"logs"`
	Sources   []fleetLogSource    `json:"sources"`
	Truncated bool                `json:"truncated,omitempty"`
	Disabled  string              `json:"disabled,omitempty"`
}

// ── breeze_get_services ──────────────────────────────────────────────────────

type serviceRow struct {
	fleetService
	// SilentSeconds is how long ago the last heartbeat arrived, measured on this
	// machine's clock against the aggregator's timestamp. It is context for a status the
	// aggregator computed, not a second opinion on it.
	SilentSeconds int64 `json:"silent_seconds,omitempty"`
}

type servicesReport struct {
	AggregatorURL string       `json:"aggregator_url"`
	Total         int          `json:"total"`
	Up            int          `json:"up"`
	Degraded      int          `json:"degraded"`
	Down          int          `json:"down"`
	Services      []serviceRow `json:"services"`
	Notes         []string     `json:"notes,omitempty"`
}

func getServicesTool() *tool {
	return &tool{
		name: "breeze_get_services",
		description: "Read the Fleet aggregator's service registry: every service that is " +
			"heartbeating, whether the aggregator considers it up, degraded or down, when it last " +
			"reported, how many instances and which versions are running. Down services are listed " +
			"first. This is the tool that shows a service is dead: a down service exports no spans, " +
			"so a failing trace stops at its caller and breeze_get_topology and breeze_explain_incident " +
			"can only name the caller. The registry forgets a service after it has been silent for " +
			"twenty times the aggregator's service TTL (five minutes by default), so a service missing " +
			"from this list may have died long ago, not never existed.",
		schema: objectSchema(fleetProps(nil), "aggregator_url"),
		run: func(raw json.RawMessage) toolCallResult {
			var a fleetArgs
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}
			return getServices(a)
		},
	}
}

func getServices(a fleetArgs) toolCallResult {
	var services []fleetService
	if err := fetchLiveJSON(a.request("/services", "fleet aggregator"), &services); err != nil {
		return liveResultError("reading the service registry", err)
	}

	report := servicesReport{
		AggregatorURL: a.AggregatorURL,
		Total:         len(services),
		Services:      make([]serviceRow, 0, len(services)),
	}

	now := fleetNow()
	var down []string
	for _, s := range services {
		row := serviceRow{fleetService: s}
		if s.LastHeartbeatUnix > 0 {
			if silent := now.Unix() - s.LastHeartbeatUnix; silent > 0 {
				row.SilentSeconds = silent
			}
		}
		switch s.Status {
		case fleetStatusUp:
			report.Up++
		case fleetStatusDegraded:
			report.Degraded++
		case fleetStatusDown:
			report.Down++
			down = append(down, s.Name)
		}
		report.Services = append(report.Services, row)
	}

	// Worst first: this is read to find what is wrong, so what is wrong goes on top.
	rank := map[string]int{fleetStatusDown: 0, fleetStatusDegraded: 1, fleetStatusUp: 2}
	sort.SliceStable(report.Services, func(i, j int) bool {
		ri, rj := rank[report.Services[i].Status], rank[report.Services[j].Status]
		if ri != rj {
			return ri < rj
		}
		return report.Services[i].Name < report.Services[j].Name
	})

	switch {
	case report.Total == 0:
		report.Notes = append(report.Notes, "The registry is empty: no service has sent a heartbeat recently. "+
			"A silent service is forgotten after twenty times the aggregator's service TTL, so this means "+
			"either nothing has reported for several minutes or the services are not configured with this "+
			"aggregator's URL. Services that export spans without heartbeating never appear here at all; "+
			"breeze_get_topology lists every service seen in spans.")
	case report.Down > 0:
		report.Notes = append(report.Notes, "down is inferred from silence: no heartbeat within the aggregator's "+
			"service TTL. It cannot tell a crashed process from a network partition between that service and "+
			"the aggregator, or from a heartbeat that stopped for another reason. A down service exports no "+
			"spans, so a trace that ends at its caller is the expected shape of its failure: read the service "+
			"itself, or the host it runs on, rather than the caller.")
	}
	if report.Degraded > 0 {
		report.Notes = append(report.Notes, "degraded means the service is heartbeating and reporting an error "+
			"rate above the aggregator's threshold: it is alive and failing, which is read from its own logs.")
	}

	summary := fmt.Sprintf("%d service(s): %d up, %d degraded, %d down", report.Total, report.Up, report.Degraded, report.Down)
	if len(down) > 0 {
		shown := down
		if len(shown) > 5 {
			shown = append(append([]string{}, shown[:5]...), "...")
		}
		summary += " (down: " + strings.Join(shown, ", ") + ")"
	}
	return structuredResult(summary, report)
}

// ── breeze_get_trace_logs ────────────────────────────────────────────────────

type traceLogsArgs struct {
	fleetArgs
	TraceID string `json:"trace_id"`
}

type traceLogsReport struct {
	AggregatorURL string              `json:"aggregator_url"`
	TraceID       string              `json:"trace_id"`
	Count         int                 `json:"count"`
	Logs          []fleetTraceLogLine `json:"logs"`
	Sources       []fleetLogSource    `json:"sources"`
	Truncated     bool                `json:"truncated,omitempty"`
	Disabled      string              `json:"disabled,omitempty"`
	Notes         []string            `json:"notes,omitempty"`
}

func getTraceLogsTool() *tool {
	return &tool{
		name: "breeze_get_trace_logs",
		description: "Read what every service in one distributed trace logged for it, merged into a single " +
			"timeline oldest first and attributed to the service that wrote each line. The aggregator asks " +
			"each service itself, from the address it heartbeats, so the caller does not need to know any " +
			"service's URL. Every log level is read, so the failure (filed under error) appears beside the " +
			"work that led to it. A service that could not be read is listed with the reason: its evidence " +
			"is missing, not empty, and a service the registry marks down (breeze_get_services) cannot be " +
			"read at all. Needs log stitching enabled: the aggregator's and each service's dashboard " +
			"service_token set to the same value.",
		schema: objectSchema(fleetProps(map[string]any{
			"trace_id": stringProp("The 32-character hex id of the trace. breeze_get_traces lists them."),
		}), "aggregator_url", "trace_id"),
		run: func(raw json.RawMessage) toolCallResult {
			var a traceLogsArgs
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}
			return getTraceLogs(a)
		},
	}
}

func getTraceLogs(a traceLogsArgs) toolCallResult {
	id, idErr := validateTraceID(a.TraceID)
	if idErr != nil {
		return errorResult(idErr.Error())
	}

	var got fleetTraceLogs
	if err := fetchLiveJSON(a.request("/traces/"+id+"/logs", "fleet aggregator"), &got); err != nil {
		return liveResultError("reading the logs of a trace", err)
	}

	report := traceLogsReport{
		AggregatorURL: a.AggregatorURL,
		TraceID:       id,
		Count:         len(got.Logs),
		Logs:          got.Logs,
		Sources:       got.Sources,
		Truncated:     got.Truncated,
		Disabled:      got.Disabled,
	}
	if report.Logs == nil {
		report.Logs = []fleetTraceLogLine{}
	}
	if report.Sources == nil {
		report.Sources = []fleetLogSource{}
	}

	if got.Disabled != "" {
		report.Notes = append(report.Notes, "The aggregator did not read any service's logs: "+got.Disabled+". "+
			"Until that is configured, read each service directly with breeze_get_logs, which given a trace_id "+
			"and no level reads every level.")
		return structuredResult("log stitching is disabled on this aggregator", report)
	}

	var unreadable, silent []string
	for _, src := range got.Sources {
		switch {
		case !src.Available:
			reason := src.Error
			if reason == "" {
				reason = "no reason given"
			}
			unreadable = append(unreadable, src.Service+" ("+reason+")")
		case src.Count == 0:
			silent = append(silent, src.Service)
		}
	}
	if len(unreadable) > 0 {
		report.Notes = append(report.Notes, "Logs could not be read from "+strings.Join(unreadable, "; ")+
			". Their evidence is missing, not empty. A service the registry marks down cannot be asked at all "+
			"(breeze_get_services), which is a finding in itself; one the aggregator has no address for can be "+
			"read directly with breeze_get_logs and its own service_url.")
	}
	if len(silent) > 0 {
		report.Notes = append(report.Notes, strings.Join(silent, ", ")+" answered and logged nothing for this "+
			"trace. A service logs only what its code writes: a silent service may simply not log this path.")
	}
	if got.Truncated {
		report.Notes = append(report.Notes, "At least one service hit the aggregator's per-service line cap, so "+
			"these are not all the lines that exist.")
	}

	readable := 0
	for _, src := range got.Sources {
		if src.Available {
			readable++
		}
	}
	summary := fmt.Sprintf("%d log line(s) from %d of %d service(s)", report.Count, readable, len(got.Sources))
	return structuredResult(summary, report)
}

// ── a dead dependency, for breeze_explain_incident ───────────────────────────

// incidentSuspect is a service the failing service calls that the registry does not
// report as up.
type incidentSuspect struct {
	Service       string `json:"service"`
	Status        string `json:"status"`
	CalledBy      string `json:"called_by"`
	SilentSeconds int64  `json:"silent_seconds,omitempty"`
}

// findSuspects joins two things the aggregator reports separately: who the failing
// service calls (the topology) and whether services are alive (the registry).
//
// Neither fact is new, and this is not an inference about causes. It puts side by side
// two things the aggregator already knows and an explanation had no way to say together.
// A service that is down exports no spans, so a trace stops at its caller and the
// aggregator's own summary of it names the caller; the dependency that is gone shows up
// only here.
//
// There are two strengths of finding, and they are kept apart:
//
//   - suspects: the failing service calls it (a topology edge) and the registry does not
//     report it up. Both facts are on record.
//   - otherDown: the registry reports it down and the topology has no call from the
//     failing service to it. An edge is built from the callee's own spans, so a service
//     that died before it ever exported one has no edge however many times it was called.
//     This is a lead, and it is reported as one, because a fleet usually has something
//     unrelated down and the explanation must not blame it on the strength of timing.
//
// Best effort by the same rule as the rest of explain_incident: a failure to read either
// side is a note, because "the root cause was X, and whether its dependencies are up could
// not be read" is a useful answer and no answer is not.
//
// unregistered are callees the topology lists that the registry does not know. That is
// reported separately and not as a suspect: a service that exports spans without ever
// heartbeating is absent from the registry from the start, and calling it dead would be a
// guess. A service forgotten after five minutes of silence looks the same.
func findSuspects(a fleetArgs, culprit string) (suspects, otherDown []incidentSuspect, unregistered []string, note string) {
	var services []fleetService
	if err := fetchLiveJSON(a.request("/services", "fleet aggregator"), &services); err != nil {
		return nil, nil, nil, "The service registry could not be read (" + err.Message + "), so whether any service " +
			"is down is unknown, and a dead dependency of " + culprit + " cannot be ruled in or out."
	}

	// The topology is optional: without it the registry can still say what is down, but
	// not whether the failing service calls it.
	callees := map[string]bool{}
	var topo fleetTopology
	topoErr := fetchLiveJSON(a.request("/topology", "fleet aggregator"), &topo)
	if topoErr == nil {
		for _, e := range topo.Edges {
			if e.Caller == culprit && e.Callee != culprit {
				callees[e.Callee] = true
			}
		}
	} else {
		note = "The topology could not be read (" + topoErr.Message + "), so the services " + culprit +
			" calls are unknown: a service the registry reports down is a lead and cannot be tied to " + culprit + "."
	}

	now := fleetNow()
	registered := map[string]bool{}
	for _, s := range services {
		registered[s.Name] = true
		if s.Name == culprit || s.Status == fleetStatusUp {
			continue
		}

		sus := incidentSuspect{Service: s.Name, Status: s.Status}
		if s.LastHeartbeatUnix > 0 {
			if silent := now.Unix() - s.LastHeartbeatUnix; silent > 0 {
				sus.SilentSeconds = silent
			}
		}

		switch {
		case callees[s.Name]:
			sus.CalledBy = culprit
			suspects = append(suspects, sus)
		case s.Status == fleetStatusDown:
			// Alive and failing is not a lead: a degraded service unrelated to this trace
			// is the ordinary state of a fleet. Dead is different, because a dead service
			// is the one thing that cannot show up in a trace.
			otherDown = append(otherDown, sus)
		}
	}
	for callee := range callees {
		if !registered[callee] {
			unregistered = append(unregistered, callee)
		}
	}

	sort.Slice(suspects, func(i, j int) bool { return suspects[i].Service < suspects[j].Service })
	sort.Strings(unregistered)

	// The one that went quiet most recently is the likeliest to matter to a failure that
	// is happening now. Bounded, so a fleet with a dozen dead services does not bury the
	// explanation.
	sort.Slice(otherDown, func(i, j int) bool {
		if otherDown[i].SilentSeconds != otherDown[j].SilentSeconds {
			return otherDown[i].SilentSeconds < otherDown[j].SilentSeconds
		}
		return otherDown[i].Service < otherDown[j].Service
	})
	if len(otherDown) > 3 {
		otherDown = otherDown[:3]
	}
	return suspects, otherDown, unregistered, note
}

// describeSuspect renders one suspect for the explanation.
func describeSuspect(s incidentSuspect) string {
	switch s.Status {
	case fleetStatusDown:
		text := fmt.Sprintf("%s calls %s (per the topology), and the service registry reports %s down", s.CalledBy, s.Service, s.Service)
		if s.SilentSeconds > 0 {
			text += fmt.Sprintf(": no heartbeat for %ds", s.SilentSeconds)
		}
		return text + ". A service that is down exports no spans, so a trace that stops at its caller is what " +
			"its failure looks like from here. That is consistent with " + s.Service + " being the real cause, and " +
			"worth checking before " + s.CalledBy + "'s own code. "
	default:
		return fmt.Sprintf("%s calls %s (per the topology), and the service registry reports %s %s: it is "+
			"alive and failing, so its failures may be what %s is passing on. ",
			s.CalledBy, s.Service, s.Service, s.Status, s.CalledBy)
	}
}

// describeLead renders a down service the topology does not tie to the failing one.
func describeLead(culprit string, s incidentSuspect) string {
	text := fmt.Sprintf("The service registry also reports %s down", s.Service)
	if s.SilentSeconds > 0 {
		text += fmt.Sprintf(": no heartbeat for %ds", s.SilentSeconds)
	}
	return text + fmt.Sprintf(". The topology has no recorded call from %s to it, so this is a lead and not a "+
		"finding: a service that goes quiet exports no spans, and a call to one that never exported any leaves no "+
		"edge. %s's own log for this trace may say what it was calling. ", culprit, culprit)
}

// buildNextSteps says what to read next. Deterministic, from facts already gathered, and
// written as calls an agent can make, because an explanation that ends at a conclusion
// leaves the next step to be rediscovered each time.
func buildNextSteps(r incidentReport, unregistered []string) []string {
	var steps []string

	if r.Trace.HasError {
		steps = append(steps, fmt.Sprintf("Read what every service in this trace logged for it, including the failures "+
			"filed under error: breeze_get_trace_logs with trace_id %s. A service it cannot read can be read directly "+
			"with breeze_get_logs and its own service_url.", r.TraceID))
	}
	for _, s := range r.Suspects {
		steps = append(steps, fmt.Sprintf("Check %s directly: %s calls it and the registry reports it %s. "+
			"breeze_get_services shows every service's status and last heartbeat.", s.Service, s.CalledBy, s.Status))
	}
	for _, s := range r.OtherDown {
		culprit := "the failing service"
		if r.Trace.RootCause != nil {
			culprit = r.Trace.RootCause.Service
		}
		steps = append(steps, fmt.Sprintf("Check %s: the registry reports it down, though the topology does not show %s "+
			"calling it. If %s's log for this trace names it, that is the cause.", s.Service, culprit, culprit))
	}
	if len(unregistered) > 0 && r.Trace.RootCause != nil {
		steps = append(steps, fmt.Sprintf("%s calls %s, which the service registry does not know: it never sent a "+
			"heartbeat, or it has been silent long enough to be forgotten (twenty times the aggregator's service TTL). "+
			"Check it directly.", r.Trace.RootCause.Service, strings.Join(unregistered, ", ")))
	}
	return steps
}
