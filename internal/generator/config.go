package generator

// The canonical generator configuration schema.
//
// This is the one place a generatable setting is declared. Both input paths â€”
// `--config project.yaml` and `--section.field=value` CLI flags â€” resolve into
// the same ProjectConfig value, and neither maintains its own list of fields.
//
// The mechanism that makes that true, rather than merely intended, is that the
// `yaml` tag on each field is the single source of truth for both names. The
// YAML key and the CLI flag segment are the same string because they are read
// from the same tag: there is no second table mapping one to the other, so
// there is nothing to drift. Adding a field here makes it settable both ways at
// once, and a field that is renamed is renamed in both at once.
//
// Precedence is a consequence of the order the three sources are applied, not
// of any comparison logic:
//
//	defaults < YAML < CLI flags
//
// Load() starts from Defaults(), unmarshals YAML on top of that value, and only
// then registers flags whose registered default *is the value already there*.
// Parsing the command line therefore overwrites whatever YAML or the defaults
// put in the field, and a flag the user did not pass leaves it untouched. No
// "was this set?" bookkeeping is needed, which is the usual source of bugs in
// layered configuration.
//
// Reflection is used here, over the config struct, at generation time. That is
// unrelated to â€” and does not weaken â€” the rule that generated model accessors
// must not use runtime reflection: nothing in this file ends up in a generated
// project.

import (
	"fmt"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ProjectConfig is the complete, resolved description of a project to generate.
//
// Every generatable section hangs off this struct. The sections mirror the
// feature registry in features.go rather than inventing a second vocabulary:
// what `breeze add ws` wires by hand is what the WebSocket section describes
// declaratively, and both end up calling the same feature generator.
type ProjectConfig struct {
	// Module is the Go module path of the project being generated. It has no
	// default that could be guessed correctly, so an empty value means "read it
	// from the go.mod already on disk".
	Module string `yaml:"module"`

	Server    ServerConfig  `yaml:"server"`
	WebSocket WSConfig      `yaml:"websocket"`
	Fleet     FleetConfig   `yaml:"fleet"`
	JSONRPC   JSONRPCConfig `yaml:"jsonrpc"`
	Docs      DocsConfig    `yaml:"docs"`

	// The keyed sections. These are sequences in YAML because their elements
	// are user-named and open-ended; a flag addresses one element by that name
	// (--middleware.rate-limit.rps=100). See bindKeyedFlags.
	Middleware []MiddlewareConfig `yaml:"middleware"`
	Models     []ModelConfig      `yaml:"models"`
	Routes     []RouteConfig      `yaml:"routes"`
}

// ServerConfig is the listener the generated main.go binds.
type ServerConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
	// Multicore maps to the gnet option of the same meaning, exposed because it
	// is the one server setting whose right value depends on deployment rather
	// than on the code.
	Multicore bool `yaml:"multicore"`
}

// WSConfig describes the generated websocket.go.
type WSConfig struct {
	Enabled bool `yaml:"enabled"`
	// Rooms selects the room-aware hub over the flat broadcast hub.
	Rooms bool   `yaml:"rooms"`
	Path  string `yaml:"path"`
}

// FleetConfig describes Fleet tracing wiring.
//
// Transport and Backend are validated against what the fleet package actually
// implements, not against the wish list: a generator that emits a call to a
// transport constructor that does not exist has produced a project that cannot
// build, which is worse than refusing to generate. See validateFleet.
type FleetConfig struct {
	Enabled     bool   `yaml:"enabled"`
	ServiceName string `yaml:"service_name"`
	// Transport is one of fleetTransports.
	Transport string `yaml:"transport"`
	// Backend applies only when Transport is "events", and is one of
	// fleetBackends.
	Backend string `yaml:"backend"`
	// AggregatorURL is the HTTP write endpoint, used by the http transport and
	// as the ws transport's fallback.
	AggregatorURL string `yaml:"aggregator_url"`
	// AggregatorWSURL is the WebSocket ingest endpoint (ws:// or wss://), used
	// only by the ws transport.
	//
	// It is a separate field rather than a scheme swap on AggregatorURL because
	// the ws transport needs both: it falls back to HTTP when the WebSocket
	// cannot be established, so a project configured for ws must carry a usable
	// HTTP endpoint too.
	AggregatorWSURL string  `yaml:"aggregator_ws_url"`
	SampleRate      float64 `yaml:"sample_rate"`
}

// JSONRPCConfig describes the generated JSON-RPC wiring.
//
// It configures the rpc package rather than reimplementing any of it: Methods
// names the handlers to stub out, and the generated registration calls
// rpc.Register for each.
type JSONRPCConfig struct {
	Enabled bool `yaml:"enabled"`
	// Port is the JSON-RPC listener. It is separate from Server.Port because
	// JSON-RPC is a peer protocol on its own gnet listener, not a route on the
	// HTTP router.
	Port    int      `yaml:"port"`
	Methods []string `yaml:"methods"`
	// BlockingMethods are the methods registered with RegisterBlocking rather
	// than Register, because they perform I/O.
	//
	// This is a separate list rather than a flag on each method because the
	// distinction is not cosmetic: a blocking handler left on an event loop
	// stalls every connection that loop is serving, and the failure appears
	// only under concurrency. Naming them explicitly makes the choice visible
	// in the config file, and generating the worker pool only when this list is
	// non-empty means projects that do not need one do not carry one.
	BlockingMethods []string `yaml:"blocking_methods"`
	// MaxMessageBytes bounds a single framed message; 0 keeps the package
	// default.
	MaxMessageBytes int `yaml:"max_message_bytes"`
	// Multicore runs one event loop per core, mirroring Server.Multicore.
	Multicore bool `yaml:"multicore"`
}

// DocsConfig describes the OpenAPI documentation viewer.
//
// SpecPath is the route the spec is served from and is deliberately settable
// but defaulted to /openapi.json, because the spec-serving route's behaviour is
// fixed by existing projects and must not change.
type DocsConfig struct {
	Enabled  bool   `yaml:"enabled"`
	UIPath   string `yaml:"ui_path"`
	SpecPath string `yaml:"spec_path"`
	Title    string `yaml:"title"`
}

// MiddlewareConfig is one middleware to generate into middleware/<name>.go.
//
// Name is the registry name (cors, rate-limit, jwt...). The remaining fields
// are the union of what the middlewares take; a field that does not apply to
// the named middleware is ignored by that middleware's generator rather than
// being an error, which keeps one YAML shape usable for all of them.
type MiddlewareConfig struct {
	Name string `yaml:"name"`
	// RPS is the rate limiter's requests-per-second budget.
	RPS int `yaml:"rps"`
	// Origins is the CORS allow-list.
	Origins []string `yaml:"origins"`
	// Secret is the JWT signing secret. A generated project gets a placeholder
	// and a note telling the developer to move it out of source.
	Secret string `yaml:"secret"`
}

// ModelConfig is one model to generate into models/<name>.go.
type ModelConfig struct {
	Name   string        `yaml:"name"`
	Table  string        `yaml:"table"`
	Fields []FieldConfig `yaml:"fields"`
}

// FieldConfig is one field of a model.
//
// Type is a Go type expression as written in source â€” "string", "[]OrderItem",
// "map[string]*Address" â€” and is parsed and resolved at generation time so an
// unresolvable reference fails loudly instead of emitting a project that does
// not compile.
type FieldConfig struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"`
	// Column overrides the derived database column name.
	Column string `yaml:"column"`
	// PrimaryKey marks this field as the model's key.
	PrimaryKey bool `yaml:"primary_key"`
}

// RouteConfig is one resource's routes, generated into routes/<resource>.go.
type RouteConfig struct {
	Resource string `yaml:"resource"`
	Path     string `yaml:"path"`
	// Methods are the HTTP verbs to scaffold handlers for.
	Methods []string `yaml:"methods"`
	// Model optionally ties the resource to a model, so the generated handlers
	// can reference the real type instead of a placeholder.
	Model string `yaml:"model"`
}

// The values Fleet accepts.
//
// fleetTransports is the full set named by the specification. Only the ones in
// fleetImplementedTransports can currently be generated; the rest are accepted
// by the schema and rejected by validation with a message that says so,
// because silently generating a project that cannot build is the one outcome
// worth ruling out.
var (
	fleetTransports = []string{"events", "ws", "gnet", "grpc", "http"}
	// http, ws and events have real transport implementations in
	// fleet/transport and so can be generated.
	//
	// gnet and grpc do not. The package named gnettransport exists, but every
	// method delegates to httptransport â€” so generating it would produce a
	// project that builds, runs, and silently exports over HTTP under a name
	// that says otherwise. Refusing is the honest outcome until it is real.
	fleetImplementedTransports = []string{"http", "ws", "events"}
	fleetBackends              = []string{"memory", "nats", "kafka", "rabbitmq"}
	fleetImplementedBackends   = []string{"memory"}
)

// Defaults is the base configuration, before YAML or flags.
//
// Every field that has a sensible default gets one here, so that a project
// generated from an empty configuration is still a working project.
func Defaults() ProjectConfig {
	return ProjectConfig{
		Server: ServerConfig{
			Host:      "0.0.0.0",
			Port:      8080,
			Multicore: true,
		},
		WebSocket: WSConfig{
			Path: "/ws",
		},
		Fleet: FleetConfig{
			Transport:       "http",
			Backend:         "memory",
			AggregatorURL:   "http://localhost:9000/fleet",
			AggregatorWSURL: "ws://localhost:9000/fleet/ws",
			SampleRate:      1,
		},

		JSONRPC: JSONRPCConfig{
			Port: 9090,
		},
		Docs: DocsConfig{
			Enabled:  true,
			UIPath:   "/docs",
			SpecPath: "/openapi.json",
			Title:    "Breeze API",
		},
	}
}

// Load resolves the configuration from a YAML file and command-line arguments.
//
// The three layers are applied in the order that defines precedence, and the
// returned config is validated before it is handed to any generator: a
// generator should never have to ask whether its input makes sense.
func Load(configPath string, args []string) (ProjectConfig, []string, error) {
	cfg := Defaults()

	if configPath != "" {
		data, err := os.ReadFile(configPath)
		if err != nil {
			return cfg, nil, fmt.Errorf("reading --config %s: %w", configPath, err)
		}
		// KnownFields makes a misspelled key an error rather than a silently
		// ignored line. A typo in a config file that changes nothing is the
		// hardest kind of configuration bug to notice.
		dec := yaml.NewDecoder(strings.NewReader(string(data)))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil {
			return cfg, nil, fmt.Errorf("parsing --config %s: %w", configPath, err)
		}
	}

	rest, err := applyFlags(&cfg, args)
	if err != nil {
		return cfg, nil, err
	}

	if err := cfg.Validate(); err != nil {
		return cfg, nil, err
	}
	return cfg, rest, nil
}

// applyFlags overwrites cfg from --path.to.field=value arguments.
//
// The flag names are not declared anywhere: they are walked out of the struct's
// yaml tags at the moment they are matched, which is what keeps them identical
// to the YAML keys by construction rather than by convention.
func applyFlags(cfg *ProjectConfig, args []string) ([]string, error) {
	var rest []string

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "--") {
			rest = append(rest, arg)
			continue
		}
		name, value, hasValue := strings.Cut(strings.TrimPrefix(arg, "--"), "=")

		// A dotted name is a config path; anything else belongs to the command.
		if !strings.Contains(name, ".") {
			rest = append(rest, arg)
			continue
		}

		if !hasValue {
			// Booleans may be written bare (--websocket.enabled). Anything else
			// takes the next argument, matching flag package behaviour.
			if isBoolPath(cfg, name) {
				value, hasValue = "true", true
			} else if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				i++
				value, hasValue = args[i], true
			}
		}
		if !hasValue {
			return nil, fmt.Errorf("flag --%s needs a value", name)
		}

		if err := setPath(cfg, name, value); err != nil {
			return nil, err
		}
	}
	return rest, nil
}

// setPath assigns value at the dotted yaml path.
func setPath(cfg *ProjectConfig, path, value string) error {
	v, err := resolvePath(reflect.ValueOf(cfg).Elem(), strings.Split(path, "."), path)
	if err != nil {
		return err
	}
	return assign(v, value, path)
}

// isBoolPath reports whether a path names a bool, so a bare --flag can be
// accepted for it.
func isBoolPath(cfg *ProjectConfig, path string) bool {
	v, err := resolvePath(reflect.ValueOf(cfg).Elem(), strings.Split(path, "."), path)
	return err == nil && v.Kind() == reflect.Bool
}

// resolvePath walks a dotted path to the addressable field it names.
//
// Two shapes are walked. A struct is matched on its yaml tags. A slice of
// named elements is matched on the element's own Name (or Resource) value â€”
// which is what makes --middleware.rate-limit.rps address the rate-limit entry
// specifically. An element named in a flag but absent from the slice is
// appended, so a middleware can be configured entirely from the command line
// without a YAML file to declare it first.
func resolvePath(v reflect.Value, path []string, full string) (reflect.Value, error) {
	for len(path) > 0 {
		switch v.Kind() {
		case reflect.Struct:
			f, ok := fieldByYAMLTag(v, path[0])
			if !ok {
				return reflect.Value{}, fmt.Errorf("unknown configuration path --%s (no field %q)", full, path[0])
			}
			v, path = f, path[1:]

		case reflect.Slice:
			// The remaining path is <element-name>.<field...>, so a bare
			// element name with nothing after it is not addressable.
			if len(path) < 2 {
				return reflect.Value{}, fmt.Errorf("--%s must name a field of %q, not the entry itself", full, path[0])
			}
			elem, err := sliceElemByName(v, path[0])
			if err != nil {
				return reflect.Value{}, fmt.Errorf("in --%s: %w", full, err)
			}
			v, path = elem, path[1:]

		default:
			return reflect.Value{}, fmt.Errorf("--%s goes past the end of the configuration at %q", full, path[0])
		}
	}
	return v, nil
}

// fieldByYAMLTag finds a struct field by its yaml tag name.
//
// The tag is the only thing consulted. Matching on the Go field name as a
// fallback would let a flag name and a YAML key diverge for the same field,
// which is the exact failure this schema exists to prevent.
func fieldByYAMLTag(v reflect.Value, name string) (reflect.Value, bool) {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		if yamlTagName(t.Field(i)) == name {
			return v.Field(i), true
		}
	}
	return reflect.Value{}, false
}

// yamlTagName is the tag's name portion, ignoring options like ",omitempty".
func yamlTagName(f reflect.StructField) string {
	tag := f.Tag.Get("yaml")
	if tag == "" {
		return strings.ToLower(f.Name)
	}
	name, _, _ := strings.Cut(tag, ",")
	return name
}

// sliceElemByName returns the element whose identifying field equals name,
// appending a new element if there is none.
func sliceElemByName(slice reflect.Value, name string) (reflect.Value, error) {
	for i := 0; i < slice.Len(); i++ {
		if elemName(slice.Index(i)) == name {
			return slice.Index(i), nil
		}
	}
	if !slice.CanSet() {
		return reflect.Value{}, fmt.Errorf("cannot add entry %q", name)
	}

	elem := reflect.New(slice.Type().Elem()).Elem()
	if !setElemName(elem, name) {
		return reflect.Value{}, fmt.Errorf("entry %q has no name field to set", name)
	}
	slice.Set(reflect.Append(slice, elem))
	return slice.Index(slice.Len() - 1), nil
}

// elemNameFields are the fields that identify a keyed slice element, in the
// order they are tried.
var elemNameFields = []string{"name", "resource"}

func elemName(elem reflect.Value) string {
	if elem.Kind() != reflect.Struct {
		return ""
	}
	for _, key := range elemNameFields {
		if f, ok := fieldByYAMLTag(elem, key); ok && f.Kind() == reflect.String {
			return f.String()
		}
	}
	return ""
}

func setElemName(elem reflect.Value, name string) bool {
	for _, key := range elemNameFields {
		if f, ok := fieldByYAMLTag(elem, key); ok && f.Kind() == reflect.String && f.CanSet() {
			f.SetString(name)
			return true
		}
	}
	return false
}

// assign parses value into the field according to its Go kind.
func assign(v reflect.Value, value, path string) error {
	if !v.CanSet() {
		return fmt.Errorf("--%s is not settable", path)
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(value)

	case reflect.Bool:
		b, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("--%s: %q is not a boolean", path, value)
		}
		v.SetBool(b)

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return fmt.Errorf("--%s: %q is not an integer", path, value)
		}
		v.SetInt(n)

	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return fmt.Errorf("--%s: %q is not a number", path, value)
		}
		v.SetFloat(f)

	case reflect.Slice:
		// A comma-separated list, matching splitList's behaviour for the
		// existing feature flags so the two spellings agree.
		if v.Type().Elem().Kind() != reflect.String {
			return fmt.Errorf("--%s cannot be set as a list", path)
		}
		items := splitList(value)
		out := reflect.MakeSlice(v.Type(), len(items), len(items))
		for i, s := range items {
			out.Index(i).SetString(s)
		}
		v.Set(out)

	default:
		return fmt.Errorf("--%s has unsupported type %s", path, v.Kind())
	}
	return nil
}

// FlagPaths lists every settable configuration path, for help output and for
// the test that holds the schema and its documentation to each other.
//
// It is generated by walking the same tags the setters read, so it cannot list
// a flag that does not work or omit one that does.
func FlagPaths() []string {
	var out []string
	walkPaths(reflect.TypeOf(ProjectConfig{}), nil, &out)
	sort.Strings(out)
	return out
}

func walkPaths(t reflect.Type, prefix []string, out *[]string) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name := yamlTagName(f)
		path := append(append([]string{}, prefix...), name)

		switch f.Type.Kind() {
		case reflect.Struct:
			walkPaths(f.Type, path, out)

		case reflect.Slice:
			if f.Type.Elem().Kind() == reflect.Struct {
				// Keyed section: the addressable paths carry a user-chosen name
				// in the middle, shown as <name> since it cannot be enumerated.
				walkPaths(f.Type.Elem(), append(path, "<name>"), out)
				continue
			}
			*out = append(*out, strings.Join(path, "."))

		default:
			*out = append(*out, strings.Join(path, "."))
		}
	}
}
