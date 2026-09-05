package scalar

// llms.go — the llms.txt rendering, over the route registry that already exists.
//
// llms.txt is a convention for handing a language model the same orientation a
// new contributor would get: what this service is, what it exposes, and the
// house rules. Two files, by convention — a short index and a full reference.
//
// # Why this lives next to the OpenAPI generator
//
// A route's method, path, summary and payload shape are already recorded, once,
// by RegisterRoute. Rendering llms.txt from a second walk of the router would
// mean two collections of the same facts, and they would disagree the first time
// a route was documented in one place and not the other — which is exactly the
// failure llms.txt is supposed to prevent. So this file adds a renderer and no
// new source: Routes() reads the same slice Generate() reads, through the same
// lock, and produces the same paths because it goes through the same breezePath.
//
// # Why the renderer takes a document rather than reading globals
//
// The registry is populated at startup by the process that owns the routes. A
// tool that generates llms.txt for a project on disk is not that process, and it
// cannot be: importing a project to enumerate its routes would mean running its
// main. So the renderer is given an LLMSDoc and does not care where the facts
// came from — the running service fills one from its own registry, and the
// generator tooling fills one by parsing the project's generated registry file.
// One renderer, one output format, two ways of learning the same facts.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// StampPrefix marks the line that records what a generated llms file was built
// from.
//
// Freshness is the whole reason it exists. A file checked into a repository is a
// snapshot, and the useful question about a snapshot is not "does it parse" but
// "is it still true" — which can only be answered by rebuilding the document and
// comparing. The stamp is what makes that comparison cheap, and it is a comment
// rather than a header so it does not read as content.
const StampPrefix = "<!-- breeze-llms-stamp: "

// RouteInfo is one documented route, as llms rendering needs it.
//
// It is the exported form of the unexported registry entry. The registry type
// stays unexported because its fields are the OpenAPI generator's business;
// this is the read-only projection callers outside the package are allowed.
type RouteInfo struct {
	Method string
	Path   string
	Doc    RouteDoc
}

// Routes returns the documented routes, sorted by path then method.
//
// Registration order is arrival order, which depends on which file registered
// first — so rendering in that order would make the output churn on unrelated
// refactors and turn every regeneration into a diff. Sorting makes the document
// a function of its content, which is what the stamp needs to mean anything.
func Routes() []RouteInfo {
	entries := allRoutes()
	out := make([]RouteInfo, 0, len(entries))
	for _, e := range entries {
		out = append(out, RouteInfo{Method: strings.ToUpper(e.method), Path: e.path, Doc: e.doc})
	}
	sortRoutes(out)
	return out
}

func sortRoutes(routes []RouteInfo) {
	sort.SliceStable(routes, func(i, j int) bool {
		if routes[i].Path != routes[j].Path {
			return routes[i].Path < routes[j].Path
		}
		return routes[i].Method < routes[j].Method
	})
}

// APIInfo reports the title, version and description the docs middleware was
// configured with, so a caller rendering llms.txt in-process does not have to be
// told again what it already registered.
//
// The name avoids Info, which is already the OpenAPI document's info object in
// types.go. A getter sharing a name with the type it describes would compile
// only until someone wrote scalar.Info{} in this package.
func APIInfo() (title, version, description string) {
	mu.RLock()
	defer mu.RUnlock()
	return apiTitle, apiVersion, apiDesc
}

// LLMSModel is one persisted type and its fields.
//
// Relationships are carried as their own field kind rather than folded into the
// type string, because "belongs to User" and "a column called user_id" are
// different facts and a model reading the second one has to guess the first.
type LLMSModel struct {
	Name   string
	Table  string
	Fields []LLMSField
}

// LLMSField is one field of a model.
type LLMSField struct {
	Name       string
	Type       string
	Column     string
	PrimaryKey bool
	// RelatedTo names the model this field points at, when it does.
	RelatedTo string
}

// LLMSSection is a titled block of prose in the full document.
//
// Conventions are passed in as sections rather than written here because they
// are not facts about OpenAPI. The tooling that knows the framework's idioms
// owns them, and it owns exactly one copy — the same one its explain-idiom tool
// answers from, so the essay and the advice cannot drift.
type LLMSSection struct {
	Title string
	Body  string
	// Items are rendered as a list under Body, for a section whose content is
	// enumerable rather than prose.
	Items []string
}

// LLMSDoc is everything the renderer needs.
type LLMSDoc struct {
	Title       string
	Version     string
	Description string

	// Module is the Go module path, when known. It tells a model what to write
	// in an import statement, which is otherwise a guess.
	Module string

	Routes   []RouteInfo
	Models   []LLMSModel
	Features []string
	Sections []LLMSSection

	// Notes record what could not be determined. A document that silently
	// omitted a section a reader expected would be read as "this project has
	// none of that", which is a different claim.
	Notes []string
}

// LLMS renders the short index file.
//
// Short means an index: what this is, and a line per route. A model that needs
// the payload shape reads llms-full.txt, and the split exists so the common case
// — "which endpoint do I want" — costs a few hundred tokens instead of tens of
// thousands.
func (d LLMSDoc) LLMS() []byte {
	var b strings.Builder
	d.writeHeader(&b, false)

	if len(d.Routes) > 0 {
		fmt.Fprintf(&b, "\n## Endpoints (%d)\n\n", len(d.Routes))
		for _, r := range d.Routes {
			fmt.Fprintf(&b, "- `%s %s`", r.Method, r.Path)
			if summary := firstLine(r.Doc.Title); summary != "" {
				fmt.Fprintf(&b, " — %s", summary)
			}
			b.WriteString("\n")
		}
	}

	if len(d.Models) > 0 {
		fmt.Fprintf(&b, "\n## Models (%d)\n\n", len(d.Models))
		for _, m := range d.Models {
			fmt.Fprintf(&b, "- `%s`", m.Name)
			if m.Table != "" {
				fmt.Fprintf(&b, " → table `%s`", m.Table)
			}
			if rel := relatedNames(m); len(rel) > 0 {
				fmt.Fprintf(&b, ", references %s", strings.Join(rel, ", "))
			}
			b.WriteString("\n")
		}
	}

	if len(d.Features) > 0 {
		b.WriteString("\n## Enabled features\n\n")
		for _, f := range d.Features {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	}

	b.WriteString(
		"\nSee llms-full.txt for request and response shapes and the framework conventions.\n",
	)
	d.writeNotes(&b)
	return stamped(b.String())
}

// LLMSFull renders the full reference.
func (d LLMSDoc) LLMSFull() []byte {
	var b strings.Builder
	d.writeHeader(&b, true)

	if len(d.Routes) > 0 {
		fmt.Fprintf(&b, "\n## Endpoints (%d)\n", len(d.Routes))
		for _, r := range d.Routes {
			d.writeRoute(&b, r)
		}
	}

	if len(d.Models) > 0 {
		fmt.Fprintf(&b, "\n## Models (%d)\n", len(d.Models))
		for _, m := range d.Models {
			writeModel(&b, m)
		}
	}

	if len(d.Features) > 0 {
		b.WriteString("\n## Enabled features\n\n")
		for _, f := range d.Features {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	}

	for _, s := range d.Sections {
		fmt.Fprintf(&b, "\n## %s\n", s.Title)
		if body := strings.TrimSpace(s.Body); body != "" {
			fmt.Fprintf(&b, "\n%s\n", body)
		}
		if len(s.Items) > 0 {
			b.WriteString("\n")
			for _, item := range s.Items {
				fmt.Fprintf(&b, "- %s\n", item)
			}
		}
	}

	d.writeNotes(&b)
	return stamped(b.String())
}

func (d LLMSDoc) writeHeader(b *strings.Builder, full bool) {
	title := d.Title
	if title == "" {
		title = "Breeze API"
	}
	fmt.Fprintf(b, "# %s\n\n", title)

	if d.Version != "" {
		fmt.Fprintf(b, "> Version %s\n", d.Version)
	}
	if d.Module != "" {
		fmt.Fprintf(b, "> Go module: `%s`\n", d.Module)
	}
	if desc := strings.TrimSpace(d.Description); desc != "" {
		fmt.Fprintf(b, "\n%s\n", desc)
	}

	kind := "index"
	if full {
		kind = "full reference"
	}
	fmt.Fprintf(b, "\nThis is the %s, generated from the service's own route registry.\n", kind)
}

func (d LLMSDoc) writeRoute(b *strings.Builder, r RouteInfo) {
	fmt.Fprintf(b, "\n### `%s %s`\n", r.Method, r.Path)

	if title := strings.TrimSpace(r.Doc.Title); title != "" {
		fmt.Fprintf(b, "\n%s\n", title)
	}
	if desc := strings.TrimSpace(r.Doc.Description); desc != "" {
		fmt.Fprintf(b, "\n%s\n", desc)
	}
	if len(r.Doc.Tags) > 0 {
		fmt.Fprintf(b, "\nTags: %s\n", strings.Join(r.Doc.Tags, ", "))
	}

	for _, group := range r.Doc.Input {
		fields := describeShape(group.Fields)
		if fields == "" {
			continue
		}
		required := ""
		if group.Required {
			required = " (required)"
		}
		fmt.Fprintf(b, "\n%s%s: %s\n", group.Type, required, fields)
		if note := strings.TrimSpace(group.Description); note != "" {
			fmt.Fprintf(b, "  %s\n", note)
		}
	}

	status := r.Doc.OutputStatus
	if status == 0 {
		status = 200
	}
	description := r.Doc.OutputDescription
	if description == "" {
		description = "OK"
	}
	if shape := describeShape(r.Doc.Output); shape != "" {
		fmt.Fprintf(b, "\nresponse %d %s: %s\n", status, description, shape)
	} else {
		fmt.Fprintf(b, "\nresponse %d %s\n", status, description)
	}
}

func writeModel(b *strings.Builder, m LLMSModel) {
	fmt.Fprintf(b, "\n### `%s`\n", m.Name)
	if m.Table != "" {
		fmt.Fprintf(b, "\nTable: `%s`\n", m.Table)
	}
	if len(m.Fields) == 0 {
		return
	}
	b.WriteString("\n")
	for _, f := range m.Fields {
		fmt.Fprintf(b, "- `%s` %s", f.Name, f.Type)
		if f.Column != "" && f.Column != f.Name {
			fmt.Fprintf(b, ", column `%s`", f.Column)
		}
		if f.PrimaryKey {
			b.WriteString(", primary key")
		}
		if f.RelatedTo != "" {
			fmt.Fprintf(b, ", references `%s`", f.RelatedTo)
		}
		b.WriteString("\n")
	}
}

func (d LLMSDoc) writeNotes(b *strings.Builder) {
	if len(d.Notes) == 0 {
		return
	}
	b.WriteString("\n## Notes\n\n")
	for _, note := range d.Notes {
		fmt.Fprintf(b, "- %s\n", note)
	}
}

// describeShape summarises a Go value's JSON shape in one line.
//
// It goes through InferSchema rather than reflecting again, so what a document
// says a route accepts is what the OpenAPI spec says it accepts. Nesting is
// rendered inline to a bounded depth: the point is to let a model recognise the
// shape, and a fully expanded tree of a deeply nested type would crowd out the
// routes around it.
func describeShape(v any) string {
	schema := InferSchema(v)
	if schema == nil {
		return ""
	}
	return renderSchema(schema, 0)
}

// maxShapeDepth bounds the inline rendering. Three levels covers a response
// wrapping a list of records with an embedded object, which is where real
// payloads stop being self-explanatory anyway.
const maxShapeDepth = 3

func renderSchema(s *Schema, depth int) string {
	if s == nil {
		return ""
	}
	switch {
	case s.Type == "array":
		if s.Items == nil {
			return "[]"
		}
		return "[" + renderSchema(s.Items, depth) + "]"

	case len(s.Properties) > 0:
		if depth >= maxShapeDepth {
			return "{…}"
		}
		required := make(map[string]bool, len(s.Required))
		for _, name := range s.Required {
			required[name] = true
		}
		names := make([]string, 0, len(s.Properties))
		for name := range s.Properties {
			names = append(names, name)
		}
		sort.Strings(names)

		parts := make([]string, 0, len(names))
		for _, name := range names {
			label := name
			if required[name] {
				label += "!"
			}
			parts = append(parts, label+": "+renderSchema(s.Properties[name], depth+1))
		}
		return "{" + strings.Join(parts, ", ") + "}"

	case s.Type == "":
		return "any"

	case s.Format != "":
		return s.Type + "(" + s.Format + ")"

	default:
		return s.Type
	}
}

func relatedNames(m LLMSModel) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range m.Fields {
		if f.RelatedTo == "" || seen[f.RelatedTo] {
			continue
		}
		seen[f.RelatedTo] = true
		out = append(out, "`"+f.RelatedTo+"`")
	}
	return out
}

// firstLine returns the first line of s, trimmed.
//
// No truncation, deliberately. This renders a route's doc Title into the llms.txt
// index, and a Title is authored prose — cutting it at an arbitrary column would
// produce a half-sentence in a file a model reads to decide which endpoint it
// wants. A long title is the author's choice; a mangled one is not.
//
// (internal/mcp has a bodyExcerpt that does truncate. That one summarises an HTTP
// error body, where the input is untrusted and can be a whole HTML page.)
func firstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		s = s[:idx]
	}
	return strings.TrimSpace(s)
}

// stamped appends the content stamp.
//
// The digest covers the body without the stamp, which is what makes it
// verifiable: hashing a document that contains its own hash is not something a
// checker could reproduce.
func stamped(body string) []byte {
	body = strings.TrimRight(body, "\n") + "\n"
	return []byte(body + "\n" + StampPrefix + "sha256=" + BodyDigest(body) + " -->\n")
}

// BodyDigest hashes a document body, ignoring the stamp line and trailing
// whitespace.
//
// Editors and version control disagree about final newlines, and a freshness
// check that failed because of one would be noise that trains a reader to
// ignore it.
func BodyDigest(document string) string {
	var b strings.Builder
	for _, line := range strings.Split(document, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), StampPrefix) {
			continue
		}
		b.WriteString(strings.TrimRight(line, " \t"))
		b.WriteString("\n")
	}
	sum := sha256.Sum256([]byte(strings.TrimRight(b.String(), "\n")))
	return hex.EncodeToString(sum[:])
}

// ReadStamp recovers the digest recorded in a document, if it has one.
func ReadStamp(document string) (string, bool) {
	for _, line := range strings.Split(document, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, StampPrefix) {
			continue
		}
		rest := strings.TrimPrefix(line, StampPrefix)
		rest = strings.TrimSuffix(strings.TrimSpace(rest), "-->")
		rest = strings.TrimSpace(rest)
		if digest, ok := strings.CutPrefix(rest, "sha256="); ok {
			return strings.TrimSpace(digest), true
		}
	}
	return "", false
}

// LLMSFromRegistry builds a document from the live registry.
//
// This is the in-process path: a running service describing itself. It reads the
// same routes and the same title Generate() reads, so a service whose OpenAPI
// document is right cannot have an llms.txt that is wrong.
func LLMSFromRegistry() LLMSDoc {
	title, version, description := APIInfo()
	return LLMSDoc{
		Title:       title,
		Version:     version,
		Description: description,
		Routes:      Routes(),
	}
}
