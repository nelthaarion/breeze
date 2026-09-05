package mcp

// tools_knowledge.go — what an agent needs to know before it generates anything.
//
// The generation tools take a configuration. Nothing so far told a caller what a
// configuration looks like, so the only way to find out was to send one and read
// the validation error — which works, slowly, and only for the fields a caller
// already suspected existed.
//
// describe_schema answers that from the struct itself, list_examples answers
// "what does a real one look like", and the llms tools produce the orientation
// document a model reads before touching a project at all. suggest_next_steps is
// the small one that matters most in practice: it reads a project and names the
// gap between what has been generated and what a working service needs.
//
// Nothing here writes into a project except generate_llms_txt, and that one only
// when asked.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/nelthaarion/breeze/internal/generator"
	"github.com/nelthaarion/breeze/scalar"
)

func registerKnowledgeTools(s *Server) {
	s.addTool(describeSchemaTool())
	s.addTool(listExamplesTool())
	s.addTool(generateLLMSTool())
	s.addTool(checkLLMSFreshnessTool())
	s.addTool(searchLLMSTool())
	s.addTool(suggestNextStepsTool())
	s.addTool(explainIdiomTool())
}

// ─── describe_schema ─────────────────────────────────────────────────────────

type describeSchemaArgs struct {
	Section string `json:"section"`
}

func describeSchemaTool() *tool {
	return &tool{
		name: "breeze_describe_schema",
		description: "The JSON Schema for a Breeze project configuration, generated from the " +
			"generator's own config struct. Every leaf carries the --flag that sets it " +
			"under x-flag, its default, and its accepted values where those are a closed " +
			"set. Pass section for one top-level key. Read this before building a config " +
			"for breeze_plan_project, breeze_new or breeze_diff_config.",
		schema: objectSchema(map[string]any{
			"section": map[string]any{
				"type":        "string",
				"enum":        generator.ConfigSections(),
				"description": "One top-level configuration key. Omit for the whole schema.",
			},
		}),
		run: func(raw json.RawMessage) toolCallResult {
			var a describeSchemaArgs
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}
			return describeSchema(a.Section)
		},
	}
}

// schemaResult is what describe_schema answers.
type schemaResult struct {
	Section string `json:"section,omitempty"`
	// Schema is the document itself, inlined rather than stringified so a caller
	// does not have to parse a string out of JSON it already parsed.
	Schema json.RawMessage `json:"schema"`
	// Sections lists the alternatives, so one call is enough to find out what
	// else could have been asked for.
	Sections []string `json:"sections"`
	// FlagPaths is every settable dotted path. It is the same list the CLI
	// accepts, and it is included because "which fields exist" is the question
	// most often asked of a schema and answering it should not require walking
	// the tree.
	FlagPaths []string `json:"flag_paths"`
}

func describeSchema(section string) toolCallResult {
	doc, err := generator.ConfigJSONSchema(section)
	if err != nil {
		return errorResult(err.Error())
	}

	paths, err := generator.SchemaFlagPaths(doc)
	if err != nil {
		return errorResult("the generated schema could not be read back: " + err.Error())
	}

	label := section
	if label == "" {
		label = "the whole configuration"
	}
	return structuredResult(
		fmt.Sprintf("schema for %s, %d settable path(s)", label, len(paths)),
		schemaResult{
			Section:   section,
			Schema:    doc,
			Sections:  generator.ConfigSections(),
			FlagPaths: paths,
		})
}

// ─── list_examples ───────────────────────────────────────────────────────────

type listExamplesArgs struct {
	Section string `json:"section"`
}

func listExamplesTool() *tool {
	return &tool{
		name: "breeze_list_examples",
		description: "Working configuration examples, each one validated before it is returned. " +
			"Pass section to filter to the examples that exercise it. Use these as the " +
			"starting point for a config rather than composing one from the schema.",
		schema: objectSchema(map[string]any{
			"section": stringProp("Optional. Only return examples that set this top-level key."),
		}),
		run: func(raw json.RawMessage) toolCallResult {
			var a listExamplesArgs
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}
			return listExamples(a.Section)
		},
	}
}

// configExample is one worked example.
type configExample struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Sections is the top-level keys this example sets, so filtering does not
	// have to guess from the YAML.
	Sections []string `json:"sections"`
	// YAML is the example itself, ready to pass as config_yaml.
	YAML string `json:"yaml"`
	// Valid and Errors report the result of running the real validator over it.
	// An example that does not validate is worse than no example, and the only
	// way to be sure is to check every time rather than at review time.
	Valid  bool     `json:"valid"`
	Errors []string `json:"errors,omitempty"`
	// Notes carry the caveats — a transport that is specified but not
	// generatable, say.
	Notes []string `json:"notes,omitempty"`
}

type examplesResult struct {
	Section  string          `json:"section,omitempty"`
	Examples []configExample `json:"examples"`
	Count    int             `json:"count"`
	// Note explains the filter when it matched nothing, rather than returning an
	// empty list that reads as "there are none".
	Note string `json:"note,omitempty"`
}

// exampleSources are the examples, as YAML.
//
// They are written as YAML text rather than built as structs on purpose: this is
// the form a caller pastes into a breeze.yaml, so it is the form that should be
// checked. Every combination below is one the generators actually implement —
// http, ws and events transports with the memory backend — because an example
// naming grpc would validate against the specification and then be refused by
// the feature generator, which is a worse outcome than not offering it.
var exampleSources = []struct {
	name        string
	description string
	yaml        string
	notes       []string
}{
	{
		name:        "minimal-api",
		description: "The smallest useful JSON service: docs and nothing else.",
		yaml: `module: example.com/minimal
server:
  port: 8080
docs:
  enabled: true
  title: Minimal API
`,
	},
	{
		name:        "fleet-http",
		description: "Distributed tracing over the HTTP transport, which posts spans to an aggregator's write endpoint.",
		yaml: `module: example.com/checkout
server:
  port: 8080
docs:
  enabled: true
fleet:
  enabled: true
  service_name: checkout
  transport: http
  aggregator_url: http://localhost:9000/fleet
  sample_rate: 1.0
`,
		notes: []string{
			"http is the transport to start with: it needs nothing running except the aggregator's HTTP endpoint.",
		},
	},
	{
		name:        "fleet-ws",
		description: "Tracing over the WebSocket transport, for a service that emits spans continuously and wants one long-lived connection instead of a request per batch.",
		yaml: `module: example.com/pricing
server:
  port: 8081
fleet:
  enabled: true
  service_name: pricing
  transport: ws
  aggregator_url: http://localhost:9000/fleet
  aggregator_ws_url: ws://localhost:9000/fleet/ws
  sample_rate: 0.25
`,
		notes: []string{
			"aggregator_ws_url is set as well as aggregator_url: the WebSocket transport still needs the HTTP endpoint for the calls that are not span writes.",
			"sample_rate below 1.0 is the normal setting for a high-traffic service; the aggregator stitches whatever it receives.",
		},
	},
	{
		name:        "fleet-events-memory",
		description: "Tracing over the in-process events bus with the memory backend — the combination to use when the aggregator runs in the same binary.",
		yaml: `module: example.com/monolith
server:
  port: 8080
fleet:
  enabled: true
  service_name: monolith
  transport: events
  backend: memory
  sample_rate: 1.0
`,
		notes: []string{
			"memory is the only backend with an implementation in the events package; nats, kafka and rabbitmq are named by the specification and refused by the generator.",
		},
	},
	{
		name:        "websocket-and-rpc",
		description: "A service that also speaks WebSocket and JSON-RPC, with CORS and rate limiting in front.",
		yaml: `module: example.com/realtime
server:
  port: 8080
  multicore: true
websocket:
  enabled: true
  path: /ws
  rooms: true
jsonrpc:
  enabled: true
  port: 9100
  methods:
    - ping
    - stats
  blocking_methods:
    - stats
middleware:
  - name: cors
    origins:
      - https://app.example.com
  - name: ratelimit
    rps: 100
`,
		notes: []string{
			"blocking_methods is a subset of methods: a method that does I/O is registered blocking so it does not hold the event loop.",
		},
	},
	{
		name:        "nested-models",
		description: "Models with a relationship between them, and the routes that expose them. The order of the models does not matter — the generator resolves dependencies itself.",
		yaml: `module: example.com/shop
server:
  port: 8080
docs:
  enabled: true
models:
  - name: Order
    table: orders
    fields:
      - name: ID
        type: int64
        column: id
        primary_key: true
      - name: Customer
        type: Customer
        column: customer_id
      - name: Total
        type: float64
        column: total
  - name: Customer
    table: customers
    fields:
      - name: ID
        type: int64
        column: id
        primary_key: true
      - name: Email
        type: string
        column: email
routes:
  - resource: orders
    path: /orders
    methods:
      - GET
      - POST
    model: Order
  - resource: customers
    path: /customers
    methods:
      - GET
    model: Customer
`,
		notes: []string{
			"Order.Customer has a model name as its type, which is what makes it a relationship: the generator emits Customer before Order so the reference compiles.",
			"A model referenced by another must exist in the same configuration; a type that resolves to nothing is a validation error naming the field.",
		},
	},
}

func listExamples(section string) toolCallResult {
	section = strings.TrimSpace(section)

	result := examplesResult{Section: section, Examples: []configExample{}}

	for _, src := range exampleSources {
		sections, err := yamlTopLevelKeys(src.yaml)
		if err != nil {
			// A malformed example is a bug in this file, and reporting it is
			// more useful than dropping it silently.
			result.Examples = append(result.Examples, configExample{
				Name:        src.name,
				Description: src.description,
				YAML:        src.yaml,
				Errors:      []string{"this example could not be parsed: " + err.Error()},
			})
			continue
		}

		if section != "" && !containsString(sections, section) {
			continue
		}

		example := configExample{
			Name:        src.name,
			Description: src.description,
			Sections:    sections,
			YAML:        src.yaml,
			Notes:       src.notes,
		}

		// Validated through the same path a caller would take, so an example
		// cannot rot into one that no longer works.
		cfg, err := (configInput{ConfigYAML: src.yaml}).resolve()
		if err != nil {
			example.Errors = []string{err.Error()}
		} else if err := cfg.Validate(); err != nil {
			example.Errors = splitValidationError(err)
		} else {
			example.Valid = true
			if unsupported := generator.UnsupportedConfigKeys(cfg); len(unsupported) > 0 {
				example.Notes = append(example.Notes,
					"configured but not reached by any generator: "+strings.Join(unsupported, ", "))
			}
		}

		result.Examples = append(result.Examples, example)
	}

	result.Count = len(result.Examples)
	if result.Count == 0 {
		result.Note = fmt.Sprintf("no example sets %q; sections with examples: %s",
			section, strings.Join(exampleSectionsCovered(), ", "))
		return structuredResult("no examples matched", result)
	}

	return structuredResult(fmt.Sprintf("%d example(s)", result.Count), result)
}

// yamlTopLevelKeys reports the top-level keys a YAML document sets.
func yamlTopLevelKeys(source string) ([]string, error) {
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(source), &doc); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(doc))
	for key := range doc {
		out = append(out, key)
	}
	sort.Strings(out)
	return out, nil
}

func exampleSectionsCovered() []string {
	seen := map[string]bool{}
	var out []string
	for _, src := range exampleSources {
		keys, err := yamlTopLevelKeys(src.yaml)
		if err != nil {
			continue
		}
		for _, key := range keys {
			if !seen[key] {
				seen[key] = true
				out = append(out, key)
			}
		}
	}
	sort.Strings(out)
	return out
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// ─── llms.txt ────────────────────────────────────────────────────────────────

// llmsShortName and llmsFullName are the conventional filenames.
const (
	llmsShortName = "llms.txt"
	llmsFullName  = "llms-full.txt"
)

type generateLLMSArgs struct {
	Path string `json:"path"`
	// Write asks for the files to be written into the project. It defaults to
	// false so that the tool a model reaches for first cannot modify a
	// repository by accident; the content is returned either way.
	Write bool `json:"write"`
}

func generateLLMSTool() *tool {
	return &tool{
		name: "breeze_generate_llms_txt",
		description: "Build llms.txt and llms-full.txt for a project from its generated route " +
			"registry, its models and the framework's conventions. Returns the content; " +
			"pass write=true to also write the two files into the project root.",
		schema: objectSchema(map[string]any{
			"path":  stringProp("Project root. Defaults to the server's working directory."),
			"write": boolProp("Write the files into the project. Defaults to false, which returns the content without touching the project."),
		}),
		run: func(raw json.RawMessage) toolCallResult {
			var a generateLLMSArgs
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}
			return generateLLMS(a)
		},
	}
}

// llmsFile is one produced document.
type llmsFile struct {
	Name  string `json:"name"`
	Bytes int    `json:"bytes"`
	// Digest is the content stamp, which is what a freshness check compares.
	Digest  string `json:"digest"`
	Content string `json:"content"`
	Written bool   `json:"written"`
}

type llmsResult struct {
	Path   string     `json:"path"`
	Files  []llmsFile `json:"files"`
	Routes int        `json:"routes"`
	Models int        `json:"models"`
	Notes  []string   `json:"notes,omitempty"`
}

func generateLLMS(a generateLLMSArgs) toolCallResult {
	// Resolved once, before the document is built, because the same root is both
	// what gets read and — with write set — what gets written to. Two calls could
	// not disagree today, but a write target that was validated separately from
	// the read target is exactly the shape a later refactor gets wrong.
	root, err := orWorkingDir(a.Path)
	if err != nil {
		return errorResult(err.Error())
	}

	doc, notes, err := buildLLMSDoc(root)
	if err != nil {
		return errorResult(err.Error())
	}

	short := doc.LLMS()
	full := doc.LLMSFull()

	result := llmsResult{
		Path:   root,
		Routes: len(doc.Routes),
		Models: len(doc.Models),
		Notes:  notes,
		Files: []llmsFile{
			{Name: llmsShortName, Bytes: len(short), Digest: scalar.BodyDigest(string(short)), Content: string(short)},
			{Name: llmsFullName, Bytes: len(full), Digest: scalar.BodyDigest(string(full)), Content: string(full)},
		},
	}

	if a.Write {
		for i := range result.Files {
			target := filepath.Join(root, result.Files[i].Name)
			if err := os.WriteFile(target, []byte(result.Files[i].Content), 0o644); err != nil {
				result.Notes = append(result.Notes, "could not write "+result.Files[i].Name+": "+err.Error())
				continue
			}
			result.Files[i].Written = true
		}
	}

	summary := fmt.Sprintf("%d route(s) and %d model(s) documented", len(doc.Routes), len(doc.Models))
	if a.Write {
		summary += "; files written"
	} else {
		summary += "; nothing written"
	}
	return structuredResult(summary, result)
}

// buildLLMSDoc assembles the document for a project on disk.
//
// The routes come from the project's own generated registry, parsed by the same
// ParseRoutes the routes tool uses. That is the only source available from
// outside the project's process: the scalar registry is filled at the project's
// startup, which is not happening here. A registry entry has no payload schema —
// the RouteDoc lives in the project's source — so what is reported is the method,
// path and handler, and a note says that the shapes come from the running
// service. Claiming more would mean inventing it.
func buildLLMSDoc(path string) (scalar.LLMSDoc, []string, error) {
	facts, cfg, err := gatherProjectFacts(path)
	if err != nil {
		return scalar.LLMSDoc{}, nil, err
	}

	doc := scalar.LLMSDoc{
		Title:   cfg.Docs.Title,
		Module:  facts.Module,
		Version: "1.0.0",
	}
	if doc.Title == "" {
		doc.Title = lastPathElement(facts.Module)
	}
	if doc.Title == "" {
		doc.Title = "Breeze service"
	}

	for _, r := range facts.Routes {
		title := r.Handler
		if r.Blocking {
			title += " (blocking)"
		}
		doc.Routes = append(doc.Routes, scalar.RouteInfo{
			Method: strings.ToUpper(r.Method),
			Path:   r.Path,
			Doc: scalar.RouteDoc{
				Title: title,
				Tags:  []string{r.Block},
			},
		})
	}
	doc.Routes = sortedRouteInfos(doc.Routes)

	doc.Models = modelsForLLMS(facts.Models)

	for _, f := range facts.Features {
		doc.Features = append(doc.Features, f.Name)
	}

	doc.Sections = []scalar.LLMSSection{
		{Title: "Conventions", Body: idiomProse, Items: idiomSectionItems()},
	}
	if len(facts.Edited) > 0 {
		doc.Sections = append(doc.Sections, scalar.LLMSSection{
			Title: "Hand-edited generated blocks",
			Body: "These generated blocks no longer match their recorded checksum, so " +
				"regenerating them needs --force and would discard the edit.",
			Items: facts.Edited,
		})
	}

	notes := []string{
		"Routes are read from " + generator.RegistryFileName + ", which records the method, path and handler. " +
			"Request and response shapes are declared in the project's source with middleware.Doc and are " +
			"served by the running service at its OpenAPI path; breeze_query_openapi reads them from a live service.",
	}
	doc.Notes = notes

	return doc, nil, nil
}

func sortedRouteInfos(routes []scalar.RouteInfo) []scalar.RouteInfo {
	sort.SliceStable(routes, func(i, j int) bool {
		if routes[i].Path != routes[j].Path {
			return routes[i].Path < routes[j].Path
		}
		return routes[i].Method < routes[j].Method
	})
	return routes
}

// modelsForLLMS converts the models found in the project's models directory,
// marking the fields that point at another model.
//
// The input is what is on disk rather than what a rebuilt configuration says: a
// model is a whole generated file and not a marker block, so a configuration
// recovered from the blocks contains no models at all, however many the project
// has.
//
// The relationship test is ModelRefs, which is the generator's own — the same
// function that decides which model has to be emitted first. Re-deriving it here
// from "does the type start with a capital" would be a second rule, and it would
// disagree the first time a custom non-model type appeared.
func modelsForLLMS(models []sourceModel) []scalar.LLMSModel {
	configs := asModelConfigs(models)
	names := modelNameSet(configs)

	out := make([]scalar.LLMSModel, 0, len(configs))
	for _, m := range configs {
		refs := map[string]bool{}
		for _, ref := range m.ModelRefs(names) {
			refs[ref] = true
		}

		model := scalar.LLMSModel{Name: m.Name, Table: m.Table}
		for _, f := range m.Fields {
			field := scalar.LLMSField{
				Name:       f.Name,
				Type:       f.Type,
				Column:     f.Column,
				PrimaryKey: f.PrimaryKey,
			}
			if base := baseTypeName(f.Type); refs[base] {
				field.RelatedTo = base
			}
			model.Fields = append(model.Fields, field)
		}
		out = append(out, model)
	}
	return out
}

func lastPathElement(module string) string {
	if module == "" {
		return ""
	}
	parts := strings.Split(module, "/")
	return parts[len(parts)-1]
}

// ─── check_llms_txt_freshness ────────────────────────────────────────────────

type checkLLMSArgs struct {
	Path string `json:"path"`
}

func checkLLMSFreshnessTool() *tool {
	return &tool{
		name: "breeze_check_llms_txt_freshness",
		description: "Check whether a project's llms.txt and llms-full.txt still describe it, by " +
			"rebuilding them and comparing content digests. Reports stale, missing or " +
			"up-to-date per file, and what changed. Writes nothing.",
		schema: objectSchema(map[string]any{
			"path": stringProp("Project root. Defaults to the server's working directory."),
		}),
		run: func(raw json.RawMessage) toolCallResult {
			var a checkLLMSArgs
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}
			return checkLLMSFreshness(a.Path)
		},
	}
}

// freshnessEntry is one file's verdict.
type freshnessEntry struct {
	Name string `json:"name"`
	// Status is one of missing, stale, current, unstamped.
	Status string `json:"status"`
	// RecordedDigest is what the file says it was built from; ExpectedDigest is
	// what rebuilding it now produces. Both are reported so a caller can see
	// that a check ran rather than trusting a boolean.
	RecordedDigest string `json:"recorded_digest,omitempty"`
	ExpectedDigest string `json:"expected_digest"`
	Reason         string `json:"reason"`
}

type freshnessResult struct {
	Path    string           `json:"path"`
	Fresh   bool             `json:"fresh"`
	Files   []freshnessEntry `json:"files"`
	Regen   string           `json:"regenerate_with,omitempty"`
	Changes []string         `json:"changes,omitempty"`
}

func checkLLMSFreshness(path string) toolCallResult {
	root, err := orWorkingDir(path)
	if err != nil {
		return errorResult(err.Error())
	}

	doc, _, err := buildLLMSDoc(root)
	if err != nil {
		return errorResult(err.Error())
	}

	expected := map[string]string{
		llmsShortName: string(doc.LLMS()),
		llmsFullName:  string(doc.LLMSFull()),
	}

	result := freshnessResult{Path: root, Fresh: true}

	for _, name := range []string{llmsShortName, llmsFullName} {
		want := scalar.BodyDigest(expected[name])
		entry := freshnessEntry{Name: name, ExpectedDigest: want}

		raw, readErr := os.ReadFile(filepath.Join(root, name))
		switch {
		case readErr != nil:
			entry.Status = "missing"
			entry.Reason = "the file does not exist, so nothing describes this project yet"
			result.Fresh = false

		default:
			content := string(raw)
			recorded, hasStamp := scalar.ReadStamp(content)
			entry.RecordedDigest = recorded

			switch {
			case !hasStamp:
				// A file without a stamp was not produced by this tool. It is
				// reported as unstamped rather than stale because "somebody
				// wrote this by hand" and "this is out of date" call for
				// different responses — overwriting the first would discard
				// work.
				entry.Status = "unstamped"
				entry.Reason = "the file carries no breeze-llms-stamp, so it was not generated by this tool and cannot be compared; regenerating would overwrite it"
				result.Fresh = false

			case recorded == want:
				entry.Status = "current"
				entry.Reason = "the recorded digest matches a freshly built document"

			case scalar.BodyDigest(content) != recorded:
				// The stamp does not describe the file it is in: somebody edited
				// the content and left the stamp. That is a different fault from
				// the project having moved on.
				entry.Status = "stale"
				entry.Reason = "the file has been edited since it was generated — its own content no longer matches the digest it records"
				result.Fresh = false

			default:
				entry.Status = "stale"
				entry.Reason = "the project has changed since this file was generated"
				result.Fresh = false
			}
		}

		result.Files = append(result.Files, entry)
	}

	if !result.Fresh {
		result.Regen = "breeze_generate_llms_txt with write=true"
		result.Changes = []string{
			fmt.Sprintf("%d route(s) and %d model(s) are in the project now", len(doc.Routes), len(doc.Models)),
		}
	}

	summary := "llms.txt and llms-full.txt are up to date"
	if !result.Fresh {
		summary = "at least one llms file is missing or out of date"
	}
	return structuredResult(summary, result)
}

// ─── search_llms_txt ─────────────────────────────────────────────────────────

type searchLLMSArgs struct {
	Path  string `json:"path"`
	Query string `json:"query"`
	Limit int    `json:"limit"`
}

func searchLLMSTool() *tool {
	return &tool{
		name: "breeze_search_llms_txt",
		description: "Search a project's llms-full.txt for a term and return the matching sections. " +
			"Use this instead of reading the whole file when the question is about one " +
			"endpoint, model or convention. Falls back to a freshly built document if the " +
			"file is absent.",
		schema: objectSchema(map[string]any{
			"path":  stringProp("Project root. Defaults to the server's working directory."),
			"query": stringProp("What to look for: a path, a method, a model name, a rule name."),
			"limit": map[string]any{"type": "integer", "description": "Maximum sections to return. Defaults to 10."},
		}, "query"),
		run: func(raw json.RawMessage) toolCallResult {
			var a searchLLMSArgs
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}
			return searchLLMS(a)
		},
	}
}

// llmsMatch is one matching section.
type llmsMatch struct {
	// Heading is the section the match is in, which is what makes a snippet
	// interpretable: "response 200 OK" means nothing without the endpoint above
	// it.
	Heading string `json:"heading"`
	Line    int    `json:"line"`
	Excerpt string `json:"excerpt"`
}

type searchResult struct {
	Path    string      `json:"path"`
	Query   string      `json:"query"`
	Source  string      `json:"source"`
	Matches []llmsMatch `json:"matches"`
	Count   int         `json:"count"`
	// Truncated says the limit cut the list, so a caller knows to narrow the
	// query rather than concluding it has seen everything.
	Truncated bool     `json:"truncated"`
	Notes     []string `json:"notes,omitempty"`
}

func searchLLMS(a searchLLMSArgs) toolCallResult {
	query := strings.TrimSpace(a.Query)
	if query == "" {
		return errorResult("query is required")
	}
	limit := a.Limit
	if limit <= 0 {
		limit = 10
	}

	root, err := orWorkingDir(a.Path)
	if err != nil {
		return errorResult(err.Error())
	}
	result := searchResult{Path: root, Query: query, Matches: []llmsMatch{}}

	var content string
	if raw, err := os.ReadFile(filepath.Join(root, llmsFullName)); err == nil {
		content = string(raw)
		result.Source = llmsFullName
	} else {
		doc, _, buildErr := buildLLMSDoc(a.Path)
		if buildErr != nil {
			return errorResult(buildErr.Error())
		}
		content = string(doc.LLMSFull())
		result.Source = "freshly built (the project has no " + llmsFullName + ")"
		result.Notes = append(result.Notes,
			"generate the file with breeze_generate_llms_txt so this search reads what the repository ships")
	}

	needle := strings.ToLower(query)
	heading := ""
	total := 0

	for i, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "#") {
			heading = strings.TrimSpace(strings.TrimLeft(line, "# "))
		}
		if !strings.Contains(strings.ToLower(line), needle) {
			continue
		}
		total++
		if len(result.Matches) >= limit {
			continue
		}
		result.Matches = append(result.Matches, llmsMatch{
			Heading: heading,
			Line:    i + 1,
			Excerpt: strings.TrimSpace(line),
		})
	}

	result.Count = total
	result.Truncated = total > len(result.Matches)

	if total == 0 {
		return structuredResult(fmt.Sprintf("%q is not mentioned in %s", query, result.Source), result)
	}
	return structuredResult(fmt.Sprintf("%d match(es) for %q", total, query), result)
}

// ─── suggest_next_steps ──────────────────────────────────────────────────────

type suggestNextStepsArgs struct {
	Path string `json:"path"`
}

func suggestNextStepsTool() *tool {
	return &tool{
		name: "breeze_suggest_next_steps",
		description: "Read a project and report what is missing or inconsistent: models with no " +
			"routes, WebSocket without auth, Fleet without tagged routes, and similar gaps. " +
			"Each finding carries the reason and the tool call that would address it. " +
			"Writes nothing.",
		schema: objectSchema(map[string]any{
			"path": stringProp("Project root. Defaults to the server's working directory."),
		}),
		run: func(raw json.RawMessage) toolCallResult {
			var a suggestNextStepsArgs
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}
			return suggestNextSteps(a.Path)
		},
	}
}

// suggestion is one recommended next step.
type suggestion struct {
	// Kind is a stable identifier for the rule that produced this, so a caller
	// can suppress or count them without matching on prose.
	Kind string `json:"kind"`
	// Subject is what the finding is about — a model name, a feature.
	Subject string `json:"subject,omitempty"`
	// Rationale is the one line explaining why this matters. It is required by
	// construction: a suggestion without a reason is an instruction, and an
	// agent cannot judge whether to follow it.
	Rationale string `json:"rationale"`
	// Action is the concrete call.
	Action   string `json:"action"`
	Severity string `json:"severity"`
}

type suggestionsResult struct {
	Path        string       `json:"path"`
	Generated   bool         `json:"generated"`
	Suggestions []suggestion `json:"suggestions"`
	Count       int          `json:"count"`
	// Checked lists the rules that ran, so an empty result is distinguishable
	// from no rules having run at all.
	Checked []string `json:"checked"`
	Notes   []string `json:"notes,omitempty"`
}

// nextStepRules are the checks, named so the result can report what ran.
var nextStepRules = []string{
	"model-without-routes",
	"model-references-unknown-model",
	"websocket-without-auth",
	"fleet-without-tagged-routes",
	"fleet-without-aggregator",
	"no-docs",
	"edited-blocks",
	"llms-missing",
}

func suggestNextSteps(path string) toolCallResult {
	facts, cfg, err := gatherProjectFacts(path)
	if err != nil {
		return errorResult(err.Error())
	}

	result := suggestionsResult{
		Path:        facts.Path,
		Generated:   facts.Generated,
		Checked:     nextStepRules,
		Suggestions: []suggestion{},
	}

	if !facts.Generated {
		result.Notes = append(result.Notes,
			"this directory has no generated features file, so there is nothing to advise on yet; scaffold with breeze_new or breeze_plan_project first")
		return structuredResult("not a generated project", result)
	}

	installed := map[string]bool{}
	for _, f := range facts.Features {
		installed[f.Name] = true
	}

	// Models and routes are both read from what is on disk — the models
	// directory and the generated route registry — rather than from the
	// reconstructed configuration. A model is a whole file rather than a marker
	// block, so the configuration rebuilt from blocks contains none of them, and
	// every rule below would silently never fire if it looked there.
	for _, m := range facts.Models {
		if routeServesModel(facts.Routes, m.Name) {
			continue
		}
		result.Suggestions = append(result.Suggestions, suggestion{
			Kind:    "model-without-routes",
			Subject: m.Name,
			Rationale: "the model and its table exist but no route mentions it, so nothing can reach it — " +
				"a model with no route is either unfinished or dead code. " +
				"This is matched by name against route paths, handlers and blocks, so a handler written by hand under an unrelated path will be missed.",
			Action:   fmt.Sprintf("breeze_generate with kind=resource and name=%s, or write a handler and register it", m.Name),
			Severity: severityWarning,
		})
	}

	// A field whose type is a bare exported identifier can only be naming
	// another model: everything the generator emits is either a builtin or
	// qualified by a package, so a name that resolves to neither is a reference
	// to a type the models package does not contain and will not compile.
	known := modelNameSet(asModelConfigs(facts.Models))
	for _, m := range facts.Models {
		for _, f := range m.Fields {
			typeName := baseTypeName(f.Type)
			if strings.Contains(typeName, ".") || typeName == "" {
				continue
			}
			if r := rune(typeName[0]); r < 'A' || r > 'Z' {
				continue
			}
			if known[typeName] {
				continue
			}
			result.Suggestions = append(result.Suggestions, suggestion{
				Kind:    "model-references-unknown-model",
				Subject: m.Name + "." + f.Name,
				Rationale: "the field's type is " + typeName + ", which no model in this project declares, so the " +
					"models package does not compile — a relationship can only point at a model that exists.",
				Action:   fmt.Sprintf("breeze_generate with kind=model and name=%s, or change %s.%s to a type that exists", typeName, m.Name, f.Name),
				Severity: severityError,
			})
		}
	}

	if installed["websocket"] && !installed["jwt"] {
		result.Suggestions = append(result.Suggestions, suggestion{
			Kind:    "websocket-without-auth",
			Subject: "websocket",
			Rationale: "a WebSocket upgrade is a long-lived connection, so an unauthenticated one is not a single " +
				"unauthorised request but an open channel — and the handshake is the only point at which it can " +
				"still be refused cheaply.",
			Action:   "breeze_add with feature=jwt, then apply it to the WebSocket route",
			Severity: severityWarning,
		})
	}

	if installed["fleet"] {
		if !installed["docs"] {
			result.Suggestions = append(result.Suggestions, suggestion{
				Kind:    "fleet-without-tagged-routes",
				Subject: "fleet",
				Rationale: "Fleet reports spans per route, and a route with no documentation entry shows up in the " +
					"aggregator as a bare path — so a trace can be read but not understood, which defeats the " +
					"reason for collecting it.",
				Action:   "breeze_add with feature=docs, and annotate handlers with middleware.Doc so routes are named in traces and in OpenAPI",
				Severity: severityWarning,
			})
		}
		if cfg.Fleet.AggregatorURL == "" && cfg.Fleet.Transport != "events" {
			result.Suggestions = append(result.Suggestions, suggestion{
				Kind:    "fleet-without-aggregator",
				Subject: cfg.Fleet.Transport,
				Rationale: "the " + cfg.Fleet.Transport + " transport posts spans to an aggregator, and with no URL " +
					"configured every span is produced and then dropped — the cost is paid and nothing is collected.",
				Action:   "set fleet.aggregator_url, or switch to the events transport for in-process collection",
				Severity: severityError,
			})
		}
	}

	if !installed["docs"] && !installed["fleet"] {
		result.Suggestions = append(result.Suggestions, suggestion{
			Kind:    "no-docs",
			Subject: "docs",
			Rationale: "without the docs feature there is no OpenAPI document, so neither a client generator nor " +
				"breeze_query_openapi can discover what this service accepts.",
			Action:   "breeze_add with feature=docs",
			Severity: severityWarning,
		})
	}

	for _, name := range facts.Edited {
		result.Suggestions = append(result.Suggestions, suggestion{
			Kind:    "edited-blocks",
			Subject: name,
			Rationale: "this generated block has been edited, so the next regeneration will refuse it or discard " +
				"the edit; whichever happens, the change is not reproducible from the configuration.",
			Action:   "move the change into breeze.yaml and regenerate, or move the code outside the marker block",
			Severity: severityWarning,
		})
	}

	if _, err := os.Stat(filepath.Join(facts.Path, llmsShortName)); err != nil {
		result.Suggestions = append(result.Suggestions, suggestion{
			Kind:    "llms-missing",
			Subject: llmsShortName,
			Rationale: "there is no llms.txt, so an agent working on this project has to rediscover its routes and " +
				"conventions by reading the source every time.",
			Action:   "breeze_generate_llms_txt with write=true",
			Severity: severityWarning,
		})
	}

	result.Count = len(result.Suggestions)
	if result.Count == 0 {
		return structuredResult("nothing to suggest: every check passed", result)
	}
	return structuredResult(fmt.Sprintf("%d suggestion(s)", result.Count), result)
}

// ─── explain_idiom ───────────────────────────────────────────────────────────

type explainIdiomArgs struct {
	Topic string `json:"topic"`
}

func explainIdiomTool() *tool {
	return &tool{
		name: "breeze_explain_idiom",
		description: "Explain one of the framework's conventions: what it is, why it exists, and what " +
			"to do instead of violating it. Takes a rule name from breeze_check_idioms or a " +
			"topic phrase. Omit topic to list every rule.",
		schema: objectSchema(map[string]any{
			"topic": stringProp("A rule name such as fleet-before-dashboard, or a phrase such as \"middleware order\". Omit to list all rules."),
		}),
		run: func(raw json.RawMessage) toolCallResult {
			var a explainIdiomArgs
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}
			return explainIdiom(a.Topic)
		},
	}
}

// idiomExplanation is one convention, explained.
type idiomExplanation struct {
	Rule     string `json:"rule"`
	Topic    string `json:"topic"`
	Severity string `json:"severity"`
	Summary  string `json:"summary"`
	Why      string `json:"why"`
	Do       string `json:"do"`
	Dont     string `json:"dont"`
	// StaticallyChecked says whether breeze_check_idioms can find violations of
	// this rule, so a caller knows whether a clean report is evidence.
	StaticallyChecked bool `json:"statically_checked"`
}

type idiomListResult struct {
	Rules       []idiomExplanation `json:"rules"`
	Count       int                `json:"count"`
	RequestedBy string             `json:"requested_topic,omitempty"`
}

func explainIdiom(topic string) toolCallResult {
	if strings.TrimSpace(topic) == "" {
		result := idiomListResult{Count: len(idiomList)}
		for _, i := range idiomList {
			result.Rules = append(result.Rules, toExplanation(i))
		}
		return structuredResult(fmt.Sprintf("%d convention(s)", len(idiomList)), result)
	}

	found, ok := findIdiom(topic)
	if !ok {
		return errorResult(fmt.Sprintf("no convention matches %q; rules: %s",
			topic, strings.Join(idiomRules(), ", ")))
	}
	return structuredResult(found.Topic, toExplanation(found))
}

func toExplanation(i idiom) idiomExplanation {
	return idiomExplanation{
		Rule:              i.Rule,
		Topic:             i.Topic,
		Severity:          i.Severity,
		Summary:           i.Summary,
		Why:               i.Why,
		Do:                i.Do,
		Dont:              i.Dont,
		StaticallyChecked: i.Checked,
	}
}
