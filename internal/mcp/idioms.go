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
