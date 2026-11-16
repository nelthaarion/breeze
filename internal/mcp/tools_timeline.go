package mcp

// tools_timeline.go — where the time went inside one request.
//
// The dashboard keeps a timeline for a request it recorded while a browser had the dashboard
// open: the steps the request passed through, each with a start, an end and what the code that
// recorded it attached. That is the one place a service says how a request's time was spent, as
// opposed to how long the request took, and an agent chasing a slow route needs exactly that.
//
// # What a timeline can and cannot tell
//
// Two facts about how timelines are made decide what this tool may claim, and both are stated in
// the notes of every answer rather than left to be rediscovered.
//
//   - A timeline exists only for a request that ran while a browser had the dashboard open. The
//     recorder has to be attached before the handler runs, when it is not yet known whether the
//     request will be slow, and attaching one to every request would allocate on the path the
//     dashboard keeps free of allocation. So a slow request that nobody was watching has no
//     timeline, and an empty list is not evidence that nothing was slow. A request record
//     carries a timeline_id whether or not a timeline was recorded for it, so a missing
//     timeline for an id taken from a request is the ordinary case.
//   - The framework records one step itself, "Request Received", which spans the whole request.
//     A finer breakdown exists only where application code records it with the dashboard's
//     TimelineRecorder. A timeline of one step says how long the request took and nothing
//     about where.
//
// # What is not carried
//
// A step's metadata is whatever the code that recorded the step put there: a row count, and just
// as easily a SQL statement, a token or an address. The dashboard serves it to its own UI and
// masks none of it. This tool carries a value only when it is a number or a boolean, which is
// what a measurement is, and lists the keys of the rest as omitted. That is the rule
// breeze_get_signals applies to an event's payload, for the same reason: reading a service's
// state must not be a way to read its data.
//
// # The tree
//
// The recorder returns every step in one flat list in the order they began, and also nests each
// step inside its parent when it closes, so a nested step appears twice: as a child and as an
// entry of the list. Read as it comes, the list counts a step's time again beside the parent
// that contains it, and the parent looks twice as busy as it was. The children are the
// structure; an entry of the list that is also somebody's child is dropped here.

import (
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-json"
)

const (
	// timelineDefaultLimit is how many timelines a list returns when no limit is given.
	timelineDefaultLimit = 20
	// timelineMaxLimit bounds a list, which carries a summary of each and is otherwise limited
	// only by the dashboard's request buffer.
	timelineMaxLimit = 200
	// timelineRootStep is the one step the framework records itself.
	timelineRootStep = "Request Received"
)

// liveTimelineStep mirrors dashboard.TimelineStep's JSON.
type liveTimelineStep struct {
	Name       string             `json:"name"`
	Start      string             `json:"start"`
	End        string             `json:"end"`
	DurationUS int64              `json:"duration_us"`
	Metadata   map[string]any     `json:"metadata"`
	Children   []liveTimelineStep `json:"children"`
}

// liveTimeline mirrors dashboard.Timeline's JSON.
type liveTimeline struct {
	ID      string             `json:"id"`
	Time    string             `json:"time"`
	Method  string             `json:"method"`
	Path    string             `json:"path"`
	Status  int                `json:"status"`
	TotalUS int64              `json:"total_us"`
	Steps   []liveTimelineStep `json:"steps"`
}

// timelineStep is one step of the answer: its own time, and what part of the request that was.
type timelineStep struct {
	Name string `json:"name"`
	// OffsetMS is how long after the request began the step started.
	OffsetMS   float64 `json:"offset_ms"`
	DurationMS float64 `json:"duration_ms"`
	// SelfMS is the step's duration less its children's, never below zero: the time spent in this
	// step and not in a step inside it, which is where the time went.
	SelfMS float64 `json:"self_ms"`
	// SharePercent is SelfMS as a share of the whole request.
	SharePercent float64 `json:"share_percent"`
	// Open is true for a step that never ended: the code that began it did not call the function
	// Step returned, so its duration is unknown and reads as zero.
	Open bool `json:"open,omitempty"`
	// Metadata holds the numbers and booleans the step recorded. MetadataOmitted names the keys
	// whose values were left out because they were not one of those.
	Metadata        map[string]any `json:"metadata,omitempty"`
	MetadataOmitted []string       `json:"metadata_omitted,omitempty"`
	Children        []timelineStep `json:"children,omitempty"`
}

// timelineSlowest names the step a request spent the most time in itself.
type timelineSlowest struct {
	// Step is the path from the top step, joined with " > ".
	Step         string  `json:"step"`
	SelfMS       float64 `json:"self_ms"`
	SharePercent float64 `json:"share_percent"`
}

// timelineSummary is one timeline in a list.
type timelineSummary struct {
	ID        string           `json:"id"`
	Time      string           `json:"time"`
	Method    string           `json:"method"`
	Path      string           `json:"path"`
	Status    int              `json:"status"`
	TotalMS   float64          `json:"total_ms"`
	StepCount int              `json:"step_count"`
	Slowest   *timelineSlowest `json:"slowest_step,omitempty"`
}

// timelineListReport is the answer when no id is given.
type timelineListReport struct {
	ServiceURL string `json:"service_url"`
	Count      int    `json:"count"`
	// Timelines are the newest first.
	Timelines []timelineSummary `json:"timelines"`
	Notes     []string          `json:"notes,omitempty"`
}

// timelineReport is the answer for one timeline.
type timelineReport struct {
	ServiceURL string           `json:"service_url"`
	ID         string           `json:"id"`
	Time       string           `json:"time"`
	Method     string           `json:"method"`
	Path       string           `json:"path"`
	Status     int              `json:"status"`
	TotalMS    float64          `json:"total_ms"`
	StepCount  int              `json:"step_count"`
	Slowest    *timelineSlowest `json:"slowest_step,omitempty"`
	Steps      []timelineStep   `json:"steps"`
	Notes      []string         `json:"notes,omitempty"`
}

type timelineArgs struct {
	liveArgs
	ID    string `json:"id"`
	Limit int    `json:"limit"`
}

func getTimelineTool() *tool {
	return &tool{
		name: "breeze_get_timeline",
		description: "Read where the time went inside a request on a running service. Without an id, " +
			"lists the timelines the dashboard holds, newest first, each with its total, its step count and the " +
			"step that took the longest itself. With an id (a request record's timeline_id), returns that " +
			"request's steps as a tree: each step's duration, its own time less its children's, and its share of the " +
			"request. Two limits shape every answer. A timeline is recorded only for a request that ran while a " +
			"browser had the dashboard open, so a slow request nobody was watching has none and an empty list is " +
			"not evidence that nothing was slow. And the framework records one step itself, 'Request Received', " +
			"spanning the whole request; a finer breakdown exists only where application code records one. A " +
			"step's metadata is carried only where it is a number or a boolean; the keys of the rest are listed. " +
			"Requires the dashboard feature.",
		schema: objectSchema(liveProps(map[string]any{
			"id": stringProp("A request's timeline id, as a request record carries it. Omit to list the " +
				"timelines the dashboard holds."),
			"limit": map[string]any{
				"type": "integer",
				"description": fmt.Sprintf("How many timelines to list. Defaults to %d, at most %d. Ignored when an id is given.",
					timelineDefaultLimit, timelineMaxLimit),
			},
		}), "service_url"),
		run: func(raw json.RawMessage) toolCallResult {
			var a timelineArgs
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}
			return getTimeline(a)
		},
	}
}

func getTimeline(a timelineArgs) toolCallResult {
	if id := strings.TrimSpace(a.ID); id != "" {
		return getOneTimeline(a, id)
	}
	return listTimelines(a)
}

// timelineCaptureNote is the first limit above, in the dashboard's own terms and, when the
// dashboard counts requests and can say so, about this moment.
func timelineCaptureNote(t *liveTraffic) string {
	note := "A timeline is recorded only for a request that ran while a browser had the dashboard open " +
		"(a WebSocket client connected): the recorder is attached before the handler runs, when nobody can know " +
		"the request will be slow. A request that was not watched has no timeline, whatever its duration, so " +
		"a missing timeline is not evidence that the request was fast."
	if countingOf(t) == countingExact {
		if t.Capture.Watching {
			note += " A browser is connected now, so requests made from this moment are timed."
		} else {
			note += " No browser is connected now, so requests made at this moment are not being timed."
		}
	}
	return note
}

// timelineNotTimedNote says why no request is timed at all, which is a different answer from "nobody
// was watching": the dashboard is installed and nothing is measuring, so a browser would change
// nothing. It is empty when something is.
func timelineNotTimedNote(t *liveTraffic) string {
	if countingOf(t) != countingNotMeasured {
		return ""
	}
	if !t.Enabled {
		return "The dashboard is installed but Config.Enabled is false, so no request is timed, whoever has the " +
			"dashboard open."
	}
	return "The dashboard is installed but its middleware is not: dashboard.Middleware(coll) was never created, so " +
		"no request is timed, whoever has the dashboard open. Add router.Use(dashboard.Middleware(coll))."
}

// timelineGranularityNote is the second limit above.
const timelineGranularityNote = "The framework records one step itself, 'Request Received', which spans the whole " +
	"request. A step inside it exists only where application code records one with the dashboard's " +
	"TimelineRecorder (the context value 'breeze.dashboard.timeline'). Where a timeline has the one step, it " +
	"says how long the request took and nothing about where the time went."

func listTimelines(a timelineArgs) toolCallResult {
	limit := a.Limit
	if limit <= 0 {
		limit = timelineDefaultLimit
	}
	if limit > timelineMaxLimit {
		limit = timelineMaxLimit
	}

	req := a.request("/timeline", "dashboard")
	req.path += fmt.Sprintf("?limit=%d", limit)
	var held []liveTimeline
	if err := fetchLiveJSON(req, &held); err != nil {
		return liveResultError("reading timelines", err)
	}

	report := timelineListReport{
		ServiceURL: a.ServiceURL,
		Count:      len(held),
		Timelines:  make([]timelineSummary, 0, len(held)),
	}
	oneStep := 0
	slowest := 0.0
	// The endpoint answers oldest first; the newest is what is asked about.
	for i := len(held) - 1; i >= 0; i-- {
		t := held[i]
		steps := rebuildTimelineSteps(t)
		count := countTimelineSteps(steps)
		if count <= 1 {
			oneStep++
		}
		total := msOf(t.TotalUS)
		if total > slowest {
			slowest = total
		}
		report.Timelines = append(report.Timelines, timelineSummary{
			ID: t.ID, Time: t.Time, Method: t.Method, Path: t.Path, Status: t.Status,
			TotalMS: total, StepCount: count, Slowest: slowestStep(steps),
		})
	}

	traffic := readTraffic(a.liveArgs)
	report.Notes = append(report.Notes, timelineCaptureNote(traffic))
	if note := timelineNotTimedNote(traffic); note != "" {
		report.Notes = append(report.Notes, note)
	}
	switch {
	case len(held) == 0:
		report.Notes = append(report.Notes, "The dashboard holds no timelines. Either no browser has had the dashboard open "+
			"since this process started, or the dashboard's Timeline setting is off. This is not evidence that no "+
			"request was slow: breeze_get_routes gives each route's average and maximum latency from the exact counts.")
	case oneStep == len(held):
		report.Notes = append(report.Notes, timelineGranularityNote)
	case oneStep > 0:
		report.Notes = append(report.Notes, fmt.Sprintf("%d of the %d timelines have only the one step the framework records, "+
			"which spans the whole request.", oneStep, len(held)))
	}

	summary := fmt.Sprintf("%d timeline(s) held, newest first", len(held))
	if len(held) > 0 {
		summary += fmt.Sprintf("; the slowest took %sms", fmtMS(slowest))
	}
	return structuredResult(summary, report)
}

func getOneTimeline(a timelineArgs, id string) toolCallResult {
	var t liveTimeline
	if err := fetchLiveJSON(a.request("/timeline/"+url.PathEscape(id), "timeline"), &t); err != nil {
		// A 404 for one id says the timeline is not held only if the dashboard is there to say
		// it. Where the list cannot be read either, the dashboard or its base path is the finding,
		// and the transport's own message for that is the right one.
		if err.Kind == "missing" && timelineListReadable(a.liveArgs) {
			err = &liveError{Kind: "missing", Message: fmt.Sprintf("no timeline with id %q is held. A request record "+
				"carries a timeline_id whether or not a timeline was recorded for it: a timeline exists only for a "+
				"request that ran while a browser had the dashboard open, and the dashboard keeps the most recent ones "+
				"only, so a request that was not watched or has since been evicted has none. Call this tool without "+
				"an id to list the timelines that are held.", id)}
		}
		return liveResultError("reading timeline "+id, err)
	}

	total := msOf(t.TotalUS)
	steps := rebuildTimelineSteps(t)
	count := countTimelineSteps(steps)
	report := timelineReport{
		ServiceURL: a.ServiceURL,
		ID:         t.ID, Time: t.Time, Method: t.Method, Path: t.Path, Status: t.Status,
		TotalMS: total, StepCount: count, Slowest: slowestStep(steps), Steps: steps,
	}
	if report.Steps == nil {
		report.Steps = []timelineStep{}
	}

	traffic := readTraffic(a.liveArgs)
	report.Notes = append(report.Notes, timelineCaptureNote(traffic))
	if note := timelineNotTimedNote(traffic); note != "" {
		report.Notes = append(report.Notes, note)
	}
	if count <= 1 {
		report.Notes = append(report.Notes, timelineGranularityNote)
	}
	if open := openTimelineSteps(steps); len(open) > 0 {
		report.Notes = append(report.Notes, fmt.Sprintf("%d step(s) never ended (%s): the code that began them did not call "+
			"the function Step returned, so their duration is unknown and reads as zero.", len(open), strings.Join(open, ", ")))
	}

	summary := fmt.Sprintf("%s %s took %sms (HTTP %d) across %d step(s)", t.Method, t.Path, fmtMS(total), t.Status, count)
	if report.Slowest != nil && count > 1 {
		summary += fmt.Sprintf("; %.0f%% of it in %s", report.Slowest.SharePercent, report.Slowest.Step)
	}
	return structuredResult(summary, report)
}

// timelineListReadable says whether the dashboard answers the timeline list.
func timelineListReadable(a liveArgs) bool {
	req := a.request("/timeline", "dashboard")
	req.path += "?limit=1"
	var held []liveTimeline
	return fetchLiveJSON(req, &held) == nil
}

// stepKey identifies a step well enough to tell a nested step from its entry in the flat list: the
// recorder copies a step into its parent when it closes, so the two carry the same name and times.
type stepKey struct{ name, start, end string }

func keyOf(s liveTimelineStep) stepKey { return stepKey{s.Name, s.Start, s.End} }

// rebuildTimelineSteps turns a timeline's flat list into the tree it describes, with each step's
// own time worked out.
func rebuildTimelineSteps(t liveTimeline) []timelineStep {
	// Every nested step is the direct child of exactly one entry of the list, so counting direct
	// children over the whole list, and not recursively, counts each nested step once.
	nested := map[stepKey]int{}
	for _, s := range t.Steps {
		for _, c := range s.Children {
			nested[keyOf(c)]++
		}
	}
	var top []liveTimelineStep
	for _, s := range t.Steps {
		if k := keyOf(s); nested[k] > 0 {
			nested[k]--
			continue
		}
		top = append(top, s)
	}

	origin, originOK := parseStepTime(t.Time)
	total := msOf(t.TotalUS)
	out := make([]timelineStep, 0, len(top))
	for _, s := range top {
		out = append(out, shapeTimelineStep(s, origin, originOK, total))
	}
	return out
}

func shapeTimelineStep(s liveTimelineStep, origin time.Time, originOK bool, total float64) timelineStep {
	step := timelineStep{
		Name:       s.Name,
		DurationMS: msOf(s.DurationUS),
		Open:       isZeroStepTime(s.End),
	}
	if start, ok := parseStepTime(s.Start); ok && originOK {
		step.OffsetMS = round3(float64(start.Sub(origin).Microseconds()) / 1000)
	}

	children := 0.0
	for _, c := range s.Children {
		shaped := shapeTimelineStep(c, origin, originOK, total)
		children += shaped.DurationMS
		step.Children = append(step.Children, shaped)
	}
	step.SelfMS = round3(math.Max(0, step.DurationMS-children))
	if total > 0 {
		step.SharePercent = round1(step.SelfMS / total * 100)
	}

	for k, v := range s.Metadata {
		switch v.(type) {
		case float64, bool:
			if step.Metadata == nil {
				step.Metadata = map[string]any{}
			}
			step.Metadata[k] = v
		default:
			step.MetadataOmitted = append(step.MetadataOmitted, k)
		}
	}
	sort.Strings(step.MetadataOmitted)
	return step
}

func countTimelineSteps(steps []timelineStep) int {
	n := 0
	for _, s := range steps {
		n += 1 + countTimelineSteps(s.Children)
	}
	return n
}

// slowestStep finds the step with the largest time of its own, and where it sits.
func slowestStep(steps []timelineStep) *timelineSlowest {
	var best *timelineSlowest
	var walk func(prefix string, steps []timelineStep)
	walk = func(prefix string, steps []timelineStep) {
		for _, s := range steps {
			path := s.Name
			if prefix != "" {
				path = prefix + " > " + s.Name
			}
			if best == nil || s.SelfMS > best.SelfMS {
				best = &timelineSlowest{Step: path, SelfMS: s.SelfMS, SharePercent: s.SharePercent}
			}
			walk(path, s.Children)
		}
	}
	walk("", steps)
	return best
}

func openTimelineSteps(steps []timelineStep) []string {
	var names []string
	for _, s := range steps {
		if s.Open {
			names = append(names, s.Name)
		}
		names = append(names, openTimelineSteps(s.Children)...)
	}
	return names
}

// msOf converts the dashboard's microseconds to milliseconds. It keeps the microsecond: a request
// that took eighty of them is 0.08ms, and rounding that to a tenth would make the fast case look
// like the slow one.
func msOf(us int64) float64 { return float64(us) / 1000 }

// round3 rounds milliseconds to the microsecond, and round1 a percentage to a tenth.
func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

func round1(v float64) float64 { return math.Round(v*10) / 10 }

// fmtMS writes milliseconds the shortest way that loses nothing at the microsecond.
func fmtMS(v float64) string { return strconv.FormatFloat(round3(v), 'f', -1, 64) }

func parseStepTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	return t, err == nil
}

// isZeroStepTime is true for the time a step that has not ended carries: Go's zero time, which
// the dashboard serialises as the year 1.
func isZeroStepTime(s string) bool {
	t, ok := parseStepTime(s)
	return s == "" || (ok && t.IsZero())
}
