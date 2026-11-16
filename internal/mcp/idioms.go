package mcp

// idioms.go — the framework's conventions, written down once.
//
// Three things need this list: llms-full.txt, which teaches it; explain_idiom,
// which answers questions about one entry; and check_idioms, which enforces the
// ones a machine can check. Those three had better agree. If the essay says
// middleware order matters and the checker does not look at it, an agent reads
// the essay, gets it wrong anyway, and learns to distrust the document — and if
// the checker flags something the essay never mentioned, the finding is a
// mystery rather than a correction.
//
// So an idiom is one value with a rule attached, and every consumer is a
// projection of this slice. A new convention is added here, and it appears in
// the prose, in the explanations, and in the checker's vocabulary at once.
//
// # What is deliberately not here
//
// Style. gofmt already owns that argument, and a rule an agent cannot act on is
// noise. Everything below changes what the code does, not how it looks.

import (
	"sort"
	"strings"
)

// severity levels for a finding.
//
// Only two, because a third would be a way to avoid deciding. An idiom is either
// something that will hurt — wrong output, a hot-path allocation, a trace that
// cannot be stitched — or it is advice. warning is the honest label for the
// second kind and stops it from being filtered out as an error that was not.
const (
	severityError   = "error"
	severityWarning = "warning"
)

// idiom is one convention.
type idiom struct {
	// Rule is the stable identifier a finding reports and explain_idiom takes.
	// It is kebab-case and never changes, because it ends up in suppression
	// comments and commit messages.
	Rule string

	// Topic is the short human name.
	Topic string

	// Severity is what a violation costs.
	Severity string

	// Summary is the one-line form, used in a finding message.
	Summary string

	// Why is the reasoning. It is the part that makes the rule followable
	// rather than obeyed: an agent that knows why reflection is banned from a
	// handler can tell a hot path from a startup path on its own.
	Why string

	// Do and Dont are the concrete forms.
	Do   string
	Dont string

	// Checked reports whether check_idioms can detect this statically. An idiom
	// that cannot be is still worth explaining, and pretending otherwise would
	// make a clean report mean more than it does.
	Checked bool
}

// idiomList is the source of truth.
var idiomList = []idiom{
	{
		Rule:     "no-reflection-in-handlers",
		Topic:    "reflection in request handling",
		Severity: severityError,
		Summary:  "handlers and the middleware around them must not use reflect",
		Why: "A handler runs on the event loop, once per request, and reflect is " +
			"the one thing in the standard library that is guaranteed to allocate " +
			"and to defeat inlining. The framework's answer is to pay that cost " +
			"once at generation time instead: generated model accessors are plain " +
			"field reads, and scalar.InferSchema reflects at startup while routes " +
			"are being registered, not while they are being served. Reflection in " +
			"a handler moves a startup cost into the hot path, where it is paid " +
			"again on every request forever.",
		Do: "Reflect at startup or in a generator, and hand the handler a typed " +
			"struct or a generated accessor.",
		Dont:    "Call reflect.TypeOf, reflect.ValueOf or any reflect helper inside a HandlerFunc.",
		Checked: true,
	},
	{
		Rule:     "fleet-before-dashboard",
		Topic:    "middleware order for Fleet and the dashboard",
		Severity: severityError,
		Summary:  "fleet.Middleware must be registered before dashboard.Middleware",
		Why: "Fleet starts the span and puts its trace ID on the context; the " +
			"dashboard reads that ID to attach a request to a trace. Middleware " +
			"runs in registration order, so registering the dashboard first means " +
			"it looks for a trace ID that has not been set yet. Nothing fails " +
			"loudly — the dashboard simply records requests with no trace, and the " +
			"symptom appears much later as a timeline that cannot be correlated " +
			"with a distributed trace. That is the worst kind of bug this framework " +
			"can have, because the tool you would use to investigate it is the " +
			"tool that is broken.",
		Do:      "router.Use(fleet.Middleware(tracer)) first, then router.Use(dashboard.Middleware(...)).",
		Dont:    "Register dashboard.Middleware before fleet.Middleware.",
		Checked: true,
	},
	{
		Rule:     "scalar-is-the-viewer",
		Topic:    "the documentation viewer",
		Severity: severityWarning,
		Summary:  "documentation is served by Scalar; Swagger UI is not shipped",
		Why: "This framework ships one viewer. The Swagger-prefixed names in " +
			"middlewares are aliases kept so existing code compiles, and " +
			"SwaggerMiddleware serves Scalar — it does not serve Swagger UI. Code " +
			"written against the old name works, but a reader who believes the name " +
			"is describing the viewer will look for a Swagger page that does not " +
			"exist. The decision and its migration note are recorded in " +
			"CHANGELOG.md under \"Documentation viewer: Scalar only\".",
		Do:      "Use middleware.ScalarMiddleware and middleware.ScalarOptions.",
		Dont:    "Use SwaggerMiddleware or SwaggerOptions in new code, or expect a Swagger UI page.",
		Checked: true,
	},
	{
		Rule:     "generated-model-accessors",
		Topic:    "reading and writing model fields",
		Severity: severityWarning,
		Summary:  "use the generated accessors for a model's table, columns and scan targets",
		Why: "`breeze generate model` emits <Model>Table, <Model>Columns and " +
			"ScanDest alongside the struct. They exist so a query does not have to " +
			"restate the column list, and so that adding a field updates every " +
			"query that selects it. A hand-written column list is a second " +
			"declaration of the schema: it compiles after a field is added, selects " +
			"the wrong set of columns, and the mismatch surfaces as a scan error at " +
			"runtime rather than a compile error.",
		Do:      "Select strings.Join(UserColumns, \", \") from UserTable and scan into user.ScanDest().",
		Dont:    "Write the table name or the column list out by hand in a query.",
		Checked: true,
	},
	{
		Rule:     "blocking-for-slow-work",
		Topic:    "blocking versus event-loop handlers",
		Severity: severityWarning,
		Summary:  "anything that waits on I/O belongs on HandleBlocking, not Handle",
		Why: "Handle runs on a gnet event loop, and an event loop is shared. A " +
			"handler that waits on a database or an outbound HTTP call holds the " +
			"loop for the whole wait, so it does not slow down one request, it " +
			"stalls every connection that loop owns. HandleBlocking moves the work " +
			"to the worker pool, which is what the pool is for. The docs endpoints " +
			"in this repository are registered blocking for exactly this reason.",
		Do:      "router.HandleBlocking for database access, file reads and outbound calls.",
		Dont:    "Do blocking I/O inside a router.Handle handler.",
		Checked: false,
	},
	{
		Rule:     "marker-blocks-are-generated",
		Topic:    "editing generated code",
		Severity: severityWarning,
		Summary:  "code inside a generated marker block is checksummed and will be refused on regeneration",
		Why: "Each generated block records a hash of its own body in its start " +
			"marker. The generator compares that hash before rewriting, so an " +
			"edited block is not silently overwritten — it is refused, and needs " +
			"--force. That makes editing one safe but temporary: the next " +
			"regeneration either fails or discards the edit. Configuration belongs " +
			"in breeze.yaml, where regeneration reproduces it.",
		Do:      "Change the configuration and regenerate, or write the code outside the markers.",
		Dont:    "Edit inside a // breeze:feature block and expect it to survive.",
		Checked: false,
	},
	{
		Rule:     "mcp-reads-the-dashboard",
		Topic:    "the in-process MCP endpoint and the dashboard",
		Severity: severityWarning,
		Summary:  "breeze add mcp serves a read-only view of the dashboard's collector; with no dashboard it has nothing to read",
		Why: "The in-process endpoint is read-only by decision: it answers from the running service's own memory, " +
			"so a tool called without service_url reads the dashboard's collector directly, with no HTTP hop and no " +
			"second set of credentials, and a tool that would change state is not offered at all (actions are exposed " +
			"through Auto-MCP by the application's author). Every live tool (breeze_get_routes, breeze_get_performance, " +
			"breeze_get_recent_errors, breeze_get_logs, breeze_get_signals, breeze_get_health, breeze_get_timeline, " +
			"breeze_get_queries) reads the collector, so a service that has the mcp feature and not the dashboard answers " +
			"each call that it has no dashboard. The endpoint is a separate port (BREEZE_MCP_PORT) behind BREEZE_MCP_TOKEN, " +
			"and what it exposes is bounded by BREEZE_MCP_SCOPE. One cost is worth knowing: the mcp package links the " +
			"generator, which adds about 5 MB to the service's binary, so a service that is size-sensitive should read " +
			"its dashboard over HTTP from a separate MCP server instead of embedding the endpoint.",
		Do: "breeze add dashboard, then breeze add mcp, and call the live tools with no service_url. Re-run breeze add " +
			"mcp if the dashboard was added afterwards.",
		Dont:    "Add mcp to a service that has no dashboard and expect an agent to find faults through it.",
		Checked: false,
	},
	{
		Rule:     "fleet-advertises-an-address",
		Topic:    "how the Fleet aggregator finds a service's logs",
		Severity: severityWarning,
		Summary:  "a service that advertises no dashboard address is traced but its logs are missing from the stitched trace",
		Why: "A heartbeat carries the address the aggregator reads a service's logs from, and a service that sends " +
			"none is traced and cannot be log-stitched: breeze_get_trace_logs and breeze_explain_incident are silent " +
			"about it, and breeze_get_services names it. FLEET_DASHBOARD_URL is that address, base path included " +
			"(http://orders:3000/dashboard). FLEET_OPENAPI_URL is the address of the OpenAPI document, used for " +
			"contract checks; the aggregator still derives a dashboard address from its host when no dashboard " +
			"address is sent, assuming the default /dashboard base path. FLEET_SERVICE_TTL is the aggregator's setting " +
			"for how long a silent service stays up: it is called down after one TTL and forgotten after twenty, " +
			"and a forgotten service is listed by breeze_get_services as a tombstone for a day. The aggregator " +
			"sends its ServiceToken to the address a heartbeat gives, so it does so only for an authenticated fleet " +
			"or a host on FetchAllowedHosts.",
		Do:      "Set FLEET_DASHBOARD_URL on every service in the fleet, and FLEET_SERVICE_TTL on the aggregator if five minutes of memory is wrong for the deployment.",
		Dont:    "Rely on FLEET_OPENAPI_URL alone for a service that moved its dashboard, or leave both empty and expect its logs in a trace.",
		Checked: false,
	},
	{
		Rule:     "the-dashboard-counts-before-it-keeps",
		Topic:    "what the dashboard keeps and what it only counts",
		Severity: severityWarning,
		Summary:  "request counts are exact; full records, timelines and queries are bounded samples with their own capture policies",
		Why: "The dashboard counts every request per route and status class, and keeps a full record only for a bounded " +
			"sample of failures (a budget of client errors and of server errors per route in each window) and for slow " +
			"requests. So breeze_get_routes is the answer to how many, and breeze_get_recent_errors is a sample of which, " +
			"and the number of records is not the number of errors. /dashboard/api/traffic carries the capture policy " +
			"and a counting field (exact, partial or not_measured) that says whether the numbers can be trusted: " +
			"not_measured means the dashboard middleware is not installed, and zero then means nothing. A timeline is " +
			"recorded only for a request that ran while a browser had the dashboard open, and the framework itself " +
			"records one step, 'Request Received'. Queries are in the monitor only if the application's data layer " +
			"pushed them with Collector.PushQuery, and are not database signals. Of the observability sources, only " +
			"events, workflow and http have a producer in the framework.",
		Do:      "Read counts for how many, samples for which, and read the notes each tool returns before treating an empty answer as a clean one.",
		Dont:    "Take the length of an error list as the error count, or an empty timeline or query list as proof that nothing was slow.",
		Checked: false,
	},
}

// idiomsByRule indexes the list once.
var idiomsByRule = func() map[string]idiom {
	out := make(map[string]idiom, len(idiomList))
	for _, i := range idiomList {
		out[i.Rule] = i
	}
	return out
}()

// findIdiom resolves a rule name or topic to an idiom.
//
// Both spellings are accepted because a caller asking about a convention knows
// the phrase before it knows the identifier: "middleware order" should find the
// rule, not return "unknown topic" and leave the caller to guess kebab-case.
func findIdiom(query string) (idiom, bool) {
	needle := strings.ToLower(strings.TrimSpace(query))
	if needle == "" {
		return idiom{}, false
	}
	if found, ok := idiomsByRule[needle]; ok {
		return found, true
	}

	// Exact topic, then substring, so "reflection" finds the reflection rule
	// without "no" matching everything that contains it.
	for _, i := range idiomList {
		if strings.ToLower(i.Topic) == needle {
			return i, true
		}
	}
	if len(needle) < 4 {
		// Too short to be a meaningful substring; matching on it would return an
		// arbitrary entry and read as an answer.
		return idiom{}, false
	}
	for _, i := range idiomList {
		haystack := strings.ToLower(i.Rule + " " + i.Topic + " " + i.Summary)
		if strings.Contains(haystack, needle) {
			return i, true
		}
	}
	return idiom{}, false
}

// idiomRules lists the rule names, sorted.
func idiomRules() []string {
	out := make([]string, 0, len(idiomList))
	for _, i := range idiomList {
		out = append(out, i.Rule)
	}
	sort.Strings(out)
	return out
}

// idiomSectionItems renders the conventions as the lines llms-full.txt carries.
//
// This is the projection that keeps the essay honest: it is built from the same
// values explain_idiom answers from, so a convention cannot be documented in one
// and missing from the other.
func idiomSectionItems() []string {
	out := make([]string, 0, len(idiomList))
	for _, i := range idiomList {
		line := "**" + i.Topic + "** (`" + i.Rule + "`, " + i.Severity + "): " + i.Summary +
			" Do: " + i.Do + " Not: " + i.Dont
		if i.Checked {
			line += " Checked by breeze_check_idioms."
		}
		out = append(out, line)
	}
	return out
}

// idiomProse is the paragraph above that list.
const idiomProse = "These are the conventions this framework is built around. They are not style " +
	"preferences: each one describes a way to get wrong behaviour, a hot-path cost, or a " +
	"silently broken diagnostic. The rules marked as checked are enforced statically by " +
	"breeze_check_idioms; ask breeze_explain_idiom about any rule name for the full reasoning."
