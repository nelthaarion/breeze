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
		schema: objectSchema(liveProps(map[string]any{
			"subsystem": stringProp("Read one subsystem instead of all of them, e.g. fleet, " +
				"workflow, templates. Omit for everything."),
			"status": map[string]any{
				"type":        "string",
				"enum":        []string{"ok", "degraded", "off", "unknown"},
				"description": "Return only subsystems in this state. Use degraded to see just the problems.",
			},
		}), "service_url"),
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
		req := a.liveArgs.request("/diagnostics", "dashboard")
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
	if err := fetchLiveJSON(a.liveArgs.request("/diagnostics", "dashboard"), &env); err != nil {
		return liveResultError("reading diagnostics", err)
	}

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
