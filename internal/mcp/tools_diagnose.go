package mcp

// tools_diagnose.go — the tool that asks a running service what every one of its
// subsystems is doing.
//
// # What this is for
//
// The other live tools each answer one question: which routes exist, what the
// heap looks like, what failed recently. None of them answers the question an
// agent actually starts with, which is "what is wrong with this service". Before
// this tool the only way to approach that was to call five tools and infer, and
// the inference was wrong in the common case — because the thing wrong is usually
// a subsystem that is not running at all, and a tool that reads a subsystem's
// output cannot distinguish "off" from "quiet".
//
// The diag registry answers it directly. Every framework subsystem registers a
// probe; the dashboard serves them all at one endpoint; this tool reads it.
//
// # Why the shape is not just a passthrough
//
// The endpoint returns a dozen-plus reports. Handing all of them back verbatim
// would make the caller do the triage, so this tool sorts degraded first,
// summarises the statuses, and — the part that matters — carries every subsystem's
// notes through verbatim. Those notes are where the framework says things like
// "DevMode is on in production" and "spans are being dropped", written for exactly
// this reader.
//
// A status filter exists because the follow-up call after "three things are
// degraded" is "show me those three".

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/nelthaarion/breeze/v2/diag"
)

// diagnoseReport is what the tool returns.
type diagnoseReport struct {
	ServiceURL string `json:"service_url"`

	// Summary counts, so a caller can decide whether to read the list.
	Total    int `json:"total"`
	OK       int `json:"ok"`
	Degraded int `json:"degraded"`
	Off      int `json:"off"`
	Unknown  int `json:"unknown"`

	// Counting reports whether the service had counted diagnostics enabled. It
	// travels with the numbers because without it every zero is ambiguous
	// between "nothing happened" and "nothing was measured".
	Counting      bool   `json:"counting"`
	CountingSince string `json:"counting_since,omitempty"`

	// Subsystems is the reports, degraded first.
	Subsystems []diagSubsystem `json:"subsystems"`

	Notes []string `json:"notes,omitempty"`
}

// diagSubsystem mirrors diag.Report's JSON.
//
// Declared here rather than imported from the diag package for the same reason
// every other live type is: package dashboard imports the root breeze package,
// which imports this one, so this package cannot import dashboard. diag itself
// would be importable — it is a leaf — but declaring the wire shape locally keeps
// the coupling to the JSON, which is the actual interface, and means a field added
// to diag.Report does not silently change this tool's output contract.
type diagSubsystem struct {
	Subsystem string         `json:"subsystem"`
	Status    string         `json:"status"`
	Summary   string         `json:"summary"`
	Detail    map[string]any `json:"detail,omitempty"`
	Notes     []string       `json:"notes,omitempty"`
}

// diagnosticsEnvelope is the dashboard endpoint's own wrapper.
type diagnosticsEnvelope struct {
	Subsystems    []diagSubsystem `json:"subsystems"`
	Total         int             `json:"total"`
	OK            int             `json:"ok"`
	Degraded      int             `json:"degraded"`
	Off           int             `json:"off"`
	Unknown       int             `json:"unknown"`
	Counting      bool            `json:"counting"`
	CountingSince string          `json:"counting_since"`
	Notes         []string        `json:"notes"`
}

type diagnoseArgs struct {
	liveArgs

	// Subsystem reads one probe instead of all of them.
	Subsystem string `json:"subsystem"`

	// Status filters the returned list. One of ok, degraded, off, unknown.
	Status string `json:"status"`
}

// diagnoseProps are the arguments the tool takes beyond how to reach the service.
func diagnoseProps() map[string]any {
	return map[string]any{
		"subsystem": stringProp("Read one subsystem instead of all of them, e.g. fleet, " +
			"workflow, templates. Omit for everything."),
		"status": map[string]any{
			"type":        "string",
			"enum":        []string{"ok", "degraded", "off", "unknown"},
			"description": "Return only subsystems in this state. Use degraded to see just the problems.",
		},
	}
}

func diagnoseServiceTool() *tool {
	return &tool{
		name: "breeze_diagnose_service",
		description: "Ask a running service what every one of its subsystems is doing: the event " +
			"bus, workflow engine, fleet tracer, template engine, i18n bundle, OpenAPI registry, " +
			"router, worker pool, WebSocket hub, video mounts, and every installed middleware. " +
			"Each reports ok, degraded or off, with the numbers behind it and notes explaining " +
			"what a reader should do. This is the tool to call first when something is wrong and " +
			"it is not yet clear what: a subsystem that is off reports so explicitly, which no " +
			"other tool can distinguish from one that is merely idle. Requires the dashboard " +
			"feature.",
		schema: objectSchema(liveProps(diagnoseProps()), "service_url"),
		run: func(raw json.RawMessage) toolCallResult {
			var a diagnoseArgs
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}
			return diagnoseService(a)
		},
	}
}

func diagnoseService(a diagnoseArgs) toolCallResult {
	// One subsystem is a different response shape — a single report, not a
	// document — so it is handled separately rather than by filtering a full read
	// down to one entry. Filtering would work but would make the service run every
	// probe to answer a question about one.
	if name := strings.TrimSpace(a.Subsystem); name != "" {
		req := a.request("/diagnostics", "dashboard")
		req.path += "?subsystem=" + name
		// A 404 here means the subsystem is not registered, not that the
		// dashboard is missing — the default reading would send a caller to
		// reinstall a working feature.
		req.notFound = fmt.Sprintf("this service has no subsystem named %q. Call this tool "+
			"without a subsystem argument to see the names it does report.", name)

		var report diagSubsystem
		if err := fetchLiveJSON(req, &report); err != nil {
			return liveResultError("reading diagnostics", err)
		}
		return structuredResult(fmt.Sprintf("%s: %s — %s", report.Subsystem, report.Status,
			report.Summary), report)
	}

	var env diagnosticsEnvelope
	if err := fetchLiveJSON(a.request("/diagnostics", "dashboard"), &env); err != nil {
		return liveResultError("reading diagnostics", err)
	}

	return shapeDiagnostics(a, env)
}

// shapeDiagnostics turns the diagnostics envelope into the tool's result: filtered,
// ordered the way someone debugging reads, and summarised. It does not care where the
// envelope came from, which is the point of it being separate: the dashboard's endpoint
// and a process reading its own registry produce the same thing, and answer the same way.
func shapeDiagnostics(a diagnoseArgs, env diagnosticsEnvelope) toolCallResult {
	report := diagnoseReport{
		ServiceURL:    a.ServiceURL,
		Total:         env.Total,
		OK:            env.OK,
		Degraded:      env.Degraded,
		Off:           env.Off,
		Unknown:       env.Unknown,
		Counting:      env.Counting,
		CountingSince: env.CountingSince,
		Subsystems:    env.Subsystems,
		Notes:         env.Notes,
	}

	if want := strings.TrimSpace(strings.ToLower(a.Status)); want != "" {
		filtered := make([]diagSubsystem, 0, len(report.Subsystems))
		for _, s := range report.Subsystems {
			if s.Status == want {
				filtered = append(filtered, s)
			}
		}
		report.Subsystems = filtered
		report.Notes = append(report.Notes, fmt.Sprintf("Filtered to status %q: %d of %d "+
			"subsystem(s) shown. The counts above are for all of them.", want, len(filtered), env.Total))
	}

	// Degraded first, then unknown, then ok, then off — the order someone
	// debugging reads in. Off last because it is the largest group in a typical
	// service and the least likely to be the problem.
	sort.SliceStable(report.Subsystems, func(i, j int) bool {
		li, lj := statusRank(report.Subsystems[i].Status), statusRank(report.Subsystems[j].Status)
		if li != lj {
			return li < lj
		}
		return report.Subsystems[i].Subsystem < report.Subsystems[j].Subsystem
	})

	summary := fmt.Sprintf("%d subsystem(s): %d ok, %d degraded, %d off",
		env.Total, env.OK, env.Degraded, env.Off)
	if names := degradedNames(report.Subsystems); len(names) > 0 {
		summary += " — " + strings.Join(names, ", ") + " need attention"
	}
	return structuredResult(summary, report)
}

// statusRank orders statuses by how much they want reading.
func statusRank(status string) int {
	switch status {
	case "degraded":
		return 0
	case "unknown":
		return 1
	case "ok":
		return 2
	default:
		return 3
	}
}

// degradedNames lists the degraded subsystems, for the summary line.
func degradedNames(subsystems []diagSubsystem) []string {
	var out []string
	for _, s := range subsystems {
		if s.Status == "degraded" {
			out = append(out, s.Subsystem)
		}
	}
	return out
}

// ── an embedded server reading its own process ───────────────────────────────

// thisProcess labels a result that was read from the process the endpoint is embedded in,
// where there is no URL to show.
const thisProcess = "(this process)"

// NewEmbeddedServer builds the server an embedded endpoint serves: the mode's server, plus
// what only a server living inside the process it describes can do.
//
// NewInProcess serves it. It is a constructor of its own, and exported, because the first
// version of reading the process's own diagnostics was written into NewInProcessServer,
// which the real endpoint does not use: its tests passed and a generated application still
// answered "a service URL is required". Whatever makes a server embedded belongs in the
// one function the endpoint and its tests both call.
func NewEmbeddedServer(version string, mode ServerMode) (*Server, error) {
	s, err := NewServerForMode(version, mode)
	if err != nil {
		return nil, err
	}
	s.useLocalDiagnostics()
	s.useLocalDashboard()
	return s, nil
}

// useLocalDiagnostics lets an embedded server answer breeze_diagnose_service about the
// process it lives in, without a service_url.
//
// The diagnostic registry is process-wide, and an embedded endpoint is in the process it
// describes. Going through the dashboard's HTTP endpoint to read what is already in memory
// cost two things that had nothing to do with the question: the dashboard feature had to be
// installed, and its credentials had to be passed. A service that left the dashboard out, or
// turned it off in production because it is a surface, could not be diagnosed by its own
// control plane. Reading the registry directly needs neither, and the endpoint's own token
// and scope are what authorise the call.
//
// Only an embedded server may do this. A standalone breeze-mcp has its own registry, which
// describes nothing the caller cares about, so it keeps the tool as it was.
//
// With a service_url the tool behaves exactly as before: another service is still reached
// over HTTP.
func (s *Server) useLocalDiagnostics() {
	t, ok := s.tools["breeze_diagnose_service"]
	if !ok {
		return
	}
	remote := t.run

	t.description = "Ask a running service what every one of its subsystems is doing: the event bus, " +
		"workflow engine, fleet tracer, HTTP client, template engine, i18n bundle, OpenAPI registry, router, " +
		"worker pool, WebSocket hub, video mounts, and every installed middleware. Each reports ok, degraded " +
		"or off, with the numbers behind it and notes explaining what a reader should do. This is the tool " +
		"to call first when something is wrong and it is not yet clear what. On this endpoint, omit " +
		"service_url to diagnose the process the endpoint is embedded in: the diagnostic registry is read " +
		"directly, so neither the dashboard feature nor its credentials are needed. Pass service_url to " +
		"diagnose another service over HTTP."
	t.schema = objectSchema(liveProps(diagnoseProps()))
	t.run = func(raw json.RawMessage) toolCallResult {
		var a diagnoseArgs
		if err := decodeArgs(raw, &a); err != nil {
			return errorResult("arguments: " + err.Error())
		}
		if strings.TrimSpace(a.ServiceURL) != "" {
			return remote(raw)
		}
		return diagnoseLocal(a)
	}
}

// diagnoseLocal answers from this process's own diagnostic registry.
func diagnoseLocal(a diagnoseArgs) toolCallResult {
	if name := strings.TrimSpace(a.Subsystem); name != "" {
		r, found := diag.Get(name)
		if !found {
			return errorResult(fmt.Sprintf("this service has no subsystem named %q. Call this tool without a "+
				"subsystem argument to see the names it does report: %s.", name, strings.Join(diag.Registered(), ", ")))
		}
		report := localSubsystem(r)
		return structuredResult(fmt.Sprintf("%s: %s  %s", report.Subsystem, report.Status, report.Summary), report)
	}

	a.ServiceURL = thisProcess
	return shapeDiagnostics(a, localEnvelope())
}

func localSubsystem(r diag.Report) diagSubsystem {
	return diagSubsystem{
		Subsystem: r.Subsystem,
		Status:    string(r.Status),
		Summary:   r.Summary,
		Detail:    r.Detail,
		Notes:     r.Notes,
	}
}

// localEnvelope builds what the dashboard's diagnostics endpoint builds from the same
// registry: the reports, how many are in each state, and what a reader needs to know about
// counting. A test compares the two, because a second copy of the rules is only safe while
// something fails when they drift.
func localEnvelope() diagnosticsEnvelope {
	reports := diag.Snapshot()

	env := diagnosticsEnvelope{Subsystems: make([]diagSubsystem, 0, len(reports)), Total: len(reports)}
	for _, r := range reports {
		env.Subsystems = append(env.Subsystems, localSubsystem(r))
		switch r.Status {
		case diag.StatusOK:
			env.OK++
		case diag.StatusDegraded:
			env.Degraded++
		case diag.StatusOff:
			env.Off++
		default:
			env.Unknown++
		}
	}

	since, counting := diag.CountersSince()
	env.Counting = counting
	if counting && !since.IsZero() {
		env.CountingSince = since.UTC().Format(time.RFC3339)
	}
	if !counting {
		env.Notes = append(env.Notes, "Counted diagnostics are off, so any hit/miss numbers below are not "+
			"measurements of an idle subsystem  nothing was counted. Call diag.EnableCounters() to turn them "+
			"on without a restart.")
	}
	if env.Off > 0 {
		env.Notes = append(env.Notes, fmt.Sprintf("%d subsystem(s) report status \"off\". That is not a fault: "+
			"it means not installed, or installed and deliberately disabled. Each one's summary names what "+
			"would turn it on.", env.Off))
	}
	return env
}
