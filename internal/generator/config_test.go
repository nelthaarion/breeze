package generator

// Tests for the canonical configuration schema.
//
// The claim this file has to substantiate is the one the design rests on: that
// YAML and CLI flags are two input paths into a single schema rather than two
// implementations that happen to agree today. A test that only checked "YAML
// works" and "flags work" separately would pass just as happily if they were
// maintained independently and had already drifted, so the parity tests compare
// the two resolved configurations against each other rather than against a
// hand-written expectation.

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeConfig writes a YAML file into a temp dir and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "project.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return path
}

func mustLoad(t *testing.T, yamlBody string, args ...string) ProjectConfig {
	t.Helper()
	path := ""
	if yamlBody != "" {
		path = writeConfig(t, yamlBody)
	}
	cfg, _, err := Load(path, args)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

// â”€â”€â”€ Precedence â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

// TestDefaultsAreValid guards the claim that an empty configuration still
// describes a working project.
func TestDefaultsAreValid(t *testing.T) {
	cfg := Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("defaults do not validate: %v", err)
	}
	if cfg.Server.Port != 8080 || cfg.Server.Host != "0.0.0.0" {
		t.Errorf("unexpected server defaults: %+v", cfg.Server)
	}
	// The spec route must keep its path, since existing projects depend on it.
	if cfg.Docs.SpecPath != "/openapi.json" {
		t.Errorf("docs.spec_path default = %q, want /openapi.json", cfg.Docs.SpecPath)
	}
}

// TestYAMLOverridesDefaults is the middle layer of the precedence chain.
func TestYAMLOverridesDefaults(t *testing.T) {
	cfg := mustLoad(t, `
server:
  host: 127.0.0.1
  port: 3000
`)
	if cfg.Server.Host != "127.0.0.1" || cfg.Server.Port != 3000 {
		t.Errorf("YAML did not override defaults: %+v", cfg.Server)
	}
	// A field YAML did not mention keeps its default rather than being zeroed.
	if !cfg.Server.Multicore {
		t.Error("server.multicore was reset by an unrelated YAML key")
	}
}

// TestFlagsOverrideYAML is the documented precedence rule:
// defaults < YAML < CLI flags.
func TestFlagsOverrideYAML(t *testing.T) {
	cfg := mustLoad(t, `
server:
  port: 3000
fleet:
  enabled: true
  transport: http
`, "--server.port=9999")

	if cfg.Server.Port != 9999 {
		t.Errorf("flag did not override YAML: port = %d, want 9999", cfg.Server.Port)
	}
	// The flag must not disturb neighbouring YAML values.
	if !cfg.Fleet.Enabled || cfg.Fleet.Transport != "http" {
		t.Errorf("flag disturbed unrelated YAML: %+v", cfg.Fleet)
	}
}

// TestFlagWithoutYAMLOverridesDefaults is the two-layer case.
func TestFlagWithoutYAMLOverridesDefaults(t *testing.T) {
	cfg := mustLoad(t, "", "--server.host=10.0.0.1")
	if cfg.Server.Host != "10.0.0.1" {
		t.Errorf("host = %q", cfg.Server.Host)
	}
	if cfg.Server.Port != 8080 {
		t.Errorf("unrelated default lost: port = %d", cfg.Server.Port)
	}
}

// â”€â”€â”€ YAML / CLI parity â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

// TestYAMLAndFlagParity is the core test of the schema's single-source claim.
//
// Each case gives the same setting in both spellings. They must produce
// identical ProjectConfig values â€” not merely both non-default â€” because that is
// what proves the two paths write the same field.
func TestYAMLAndFlagParity(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		args []string
	}{
		{
			name: "server",
			yaml: "server:\n  host: 1.2.3.4\n  port: 4321\n  multicore: false\n",
			args: []string{"--server.host=1.2.3.4", "--server.port=4321", "--server.multicore=false"},
		},
		{
			name: "websocket",
			yaml: "websocket:\n  enabled: true\n  rooms: true\n  path: /socket\n",
			args: []string{"--websocket.enabled=true", "--websocket.rooms=true", "--websocket.path=/socket"},
		},
		{
			name: "fleet",
			yaml: "fleet:\n  enabled: true\n  service_name: gateway\n  transport: http\n  sample_rate: 0.5\n",
			args: []string{"--fleet.enabled=true", "--fleet.service_name=gateway", "--fleet.transport=http", "--fleet.sample_rate=0.5"},
		},
		{
			name: "jsonrpc",
			yaml: "jsonrpc:\n  enabled: true\n  port: 7000\n  methods: [sum, echo]\n",
			args: []string{"--jsonrpc.enabled=true", "--jsonrpc.port=7000", "--jsonrpc.methods=sum,echo"},
		},
		{
			name: "docs",
			yaml: "docs:\n  enabled: true\n  ui_path: /reference\n  title: My API\n",
			args: []string{"--docs.enabled=true", "--docs.ui_path=/reference", "--docs.title=My API"},
		},
		{
			// The keyed-section case: a flag addresses one element of a
			// sequence by its name.
			name: "middleware",
			yaml: "middleware:\n  - name: rate-limit\n    rps: 100\n",
			args: []string{"--middleware.rate-limit.rps=100"},
		},
		{
			name: "middleware list value",
			yaml: "middleware:\n  - name: cors\n    origins: [a.com, b.com]\n",
			args: []string{"--middleware.cors.origins=a.com,b.com"},
		},
		{
			name: "routes",
			yaml: "routes:\n  - resource: users\n    path: /users\n    methods: [GET, POST]\n",
			args: []string{"--routes.users.path=/users", "--routes.users.methods=GET,POST"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fromYAML := mustLoad(t, tc.yaml)
			fromFlags := mustLoad(t, "", tc.args...)

			if !reflect.DeepEqual(fromYAML, fromFlags) {
				t.Errorf("YAML and flags produced different configs\n YAML: %+v\nflags: %+v", fromYAML, fromFlags)
			}
			// Guard against the test passing because neither path did anything.
			if reflect.DeepEqual(fromYAML, Defaults()) {
				t.Error("neither input path changed anything, so parity is vacuous")
			}
		})
	}
}

// TestEveryFlagPathIsSettable walks the schema and sets every scalar path,
// proving the derivation covers the whole struct rather than the fields the
// other tests happen to mention.
//
// This is what makes the flag surface self-maintaining: a field added to
// ProjectConfig is covered by this test the moment it is declared, and a field
// the walker cannot set fails here rather than in a user's terminal.
func TestEveryFlagPathIsSettable(t *testing.T) {
	for _, path := range FlagPaths() {
		// Keyed paths need a user-chosen element name substituted in.
		concrete := strings.Replace(path, "<name>", "probe", 1)

		value := "1"
		cfg := Defaults()
		v, err := resolvePath(reflect.ValueOf(&cfg).Elem(), strings.Split(concrete, "."), concrete)
		if err != nil {
			t.Errorf("FlagPaths lists --%s but it does not resolve: %v", path, err)
			continue
		}
		switch v.Kind() {
		case reflect.Bool:
			value = "true"
		case reflect.String:
			value = "probe"
		case reflect.Slice:
			value = "a,b"
		case reflect.Float32, reflect.Float64:
			value = "0.5"
		}

		if err := setPath(&cfg, concrete, value); err != nil {
			t.Errorf("--%s=%s failed: %v", concrete, value, err)
		}
	}
}

// TestUnknownFlagPathIsRejected â€” a mistyped path must not be silently ignored.
func TestUnknownFlagPathIsRejected(t *testing.T) {
	for _, arg := range []string{
		"--server.prot=80",
		"--nosuchsection.field=1",
		"--server.port.deeper=1",
	} {
		if _, _, err := Load("", []string{arg}); err == nil {
			t.Errorf("%s was accepted", arg)
		}
	}
}

// TestUnknownYAMLKeyIsRejected â€” a mistyped key in a config file is the
// hardest kind of configuration bug to notice, so it is an error.
func TestUnknownYAMLKeyIsRejected(t *testing.T) {
	_, _, err := Load(writeConfig(t, "server:\n  prot: 8080\n"), nil)
	if err == nil {
		t.Fatal("misspelled YAML key was accepted")
	}
	if !strings.Contains(err.Error(), "prot") {
		t.Errorf("error does not name the offending key: %v", err)
	}
}

// TestBareBooleanFlag â€” --websocket.enabled with no value means true.
func TestBareBooleanFlag(t *testing.T) {
	cfg := mustLoad(t, "", "--websocket.enabled")
	if !cfg.WebSocket.Enabled {
		t.Error("bare boolean flag did not set true")
	}
}

// TestNonConfigArgsArePassedThrough â€” Load must not swallow the command's own
// arguments and flags.
func TestNonConfigArgsArePassedThrough(t *testing.T) {
	_, rest, err := Load("", []string{"User", "--force", "--server.port=1234", "name:string"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"User", "--force", "name:string"}
	if !reflect.DeepEqual(rest, want) {
		t.Errorf("rest = %q, want %q", rest, want)
	}
}

// â”€â”€â”€ Type parsing (Â§11) â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

func TestParseTypeExprSupportedForms(t *testing.T) {
	models := map[string]bool{"Address": true, "OrderItem": true}

	for _, expr := range []string{
		"string", "int", "bool", "float64", "byte", "[]byte",
		"time.Time", "json.RawMessage",
		"Address", "*Address", "[]Address", "[]*Address", "*[]Address",
		"map[string]string", "map[string]Address", "map[string]*Address",
		"[]OrderItem", "[]*OrderItem",
		"map[string][]*Address", "[][]Address", "*map[string]int",
	} {
		refs, err := parseTypeExpr(expr)
		if err != nil {
			t.Errorf("%s: unexpected error %v", expr, err)
			continue
		}
		for _, r := range refs {
			if !primitives[r] && !models[r] {
				t.Errorf("%s: ref %q resolves to nothing", expr, r)
			}
		}
	}
}

func TestParseTypeExprRejectsMalformed(t *testing.T) {
	for _, expr := range []string{
		"", "  ",
		"map[string", "map[]int", "map[string]",
		"[]", "*",
		"1bad", "has space",
		"map[Address]string", // model as map key
	} {
		if _, err := parseTypeExpr(expr); err == nil {
			t.Errorf("%q was accepted", expr)
		}
	}
}

// TestUnresolvedTypeErrorNamesModelFieldAndType checks the exact error the
// specification asks for, because a vague message here is what sends someone
// reading generated code to find a config typo.
func TestUnresolvedTypeErrorNamesModelFieldAndType(t *testing.T) {
	cfg := Defaults()
	cfg.Models = []ModelConfig{{
		Name: "Order",
		Fields: []FieldConfig{
			{Name: "id", Type: "string"},
			{Name: "shipping_address", Type: "AddressBook"},
		},
	}}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("unknown type reference was accepted")
	}
	want := "model Order field shipping_address references unknown type AddressBook"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error was:\n  %v\nwant it to contain:\n  %s", err, want)
	}
}

// TestUnresolvedTypeInsideCompositeIsCaught â€” the reference must be resolved
// through the composite, not just at the top level.
func TestUnresolvedTypeInsideCompositeIsCaught(t *testing.T) {
	for _, typ := range []string{"[]Ghost", "*Ghost", "map[string]Ghost", "[]*Ghost", "map[string][]*Ghost"} {
		cfg := Defaults()
		cfg.Models = []ModelConfig{{
			Name:   "Order",
			Fields: []FieldConfig{{Name: "f", Type: typ}},
		}}
		err := cfg.Validate()
		if err == nil {
			t.Errorf("%s: unresolved reference inside composite was accepted", typ)
			continue
		}
		if !strings.Contains(err.Error(), "unknown type Ghost") {
			t.Errorf("%s: error does not name Ghost: %v", typ, err)
		}
	}
}

// â”€â”€â”€ Dependency resolution (Â§10) â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

// orderFixture is the Order/OrderItem/Address example from the specification.
func orderFixture() ProjectConfig {
	cfg := Defaults()
	cfg.Models = []ModelConfig{
		{Name: "Order", Fields: []FieldConfig{
			{Name: "id", Type: "string", PrimaryKey: true},
			{Name: "items", Type: "[]OrderItem"},
			{Name: "shipping_address", Type: "Address"},
			{Name: "metadata", Type: "map[string]string"},
		}},
		{Name: "OrderItem", Fields: []FieldConfig{
			{Name: "sku", Type: "string"},
			{Name: "quantity", Type: "int"},
		}},
		{Name: "Address", Fields: []FieldConfig{
			{Name: "street", Type: "string"},
			{Name: "city", Type: "string"},
		}},
	}
	return cfg
}

func TestOrderFixtureValidates(t *testing.T) {
	cfg := orderFixture()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the specification's own example does not validate: %v", err)
	}
}

// TestResolveModelOrderEmitsDependenciesFirst â€” a referenced model must be
// generated before the model referencing it, and exactly once.
func TestResolveModelOrderEmitsDependenciesFirst(t *testing.T) {
	cfg := orderFixture()
	ordered, err := cfg.ResolveModelOrder()
	if err != nil {
		t.Fatalf("ResolveModelOrder: %v", err)
	}

	if len(ordered) != 3 {
		t.Fatalf("got %d models, want 3", len(ordered))
	}

	pos := map[string]int{}
	for i, m := range ordered {
		if _, dup := pos[m.Name]; dup {
			t.Fatalf("model %s generated twice", m.Name)
		}
		pos[m.Name] = i
	}
	if pos["OrderItem"] > pos["Order"] {
		t.Error("OrderItem must come before Order")
	}
	if pos["Address"] > pos["Order"] {
		t.Error("Address must come before Order")
	}
}

// TestSharedDependencyGeneratedOnce â€” two models referencing the same type must
// not produce it twice.
func TestSharedDependencyGeneratedOnce(t *testing.T) {
	cfg := Defaults()
	cfg.Models = []ModelConfig{
		{Name: "Order", Fields: []FieldConfig{{Name: "addr", Type: "Address"}}},
		{Name: "Customer", Fields: []FieldConfig{{Name: "addr", Type: "*Address"}}},
		{Name: "Address", Fields: []FieldConfig{{Name: "city", Type: "string"}}},
	}
	ordered, err := cfg.ResolveModelOrder()
	if err != nil {
		t.Fatalf("ResolveModelOrder: %v", err)
	}

	count := 0
	for _, m := range ordered {
		if m.Name == "Address" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("Address generated %d times, want 1", count)
	}
	if len(ordered) != 3 {
		t.Errorf("got %d models, want 3", len(ordered))
	}
}

// TestResolveModelOrderIsDeterministic â€” output order must not depend on map
// iteration, or regeneration would produce spurious diffs.
func TestResolveModelOrderIsDeterministic(t *testing.T) {
	cfg := orderFixture()
	first, err := cfg.ResolveModelOrder()
	if err != nil {
		t.Fatalf("ResolveModelOrder: %v", err)
	}
	for i := 0; i < 50; i++ {
		again, err := cfg.ResolveModelOrder()
		if err != nil {
			t.Fatalf("ResolveModelOrder: %v", err)
		}
		if !reflect.DeepEqual(first, again) {
			t.Fatalf("order changed between runs:\n%v\n%v", names(first), names(again))
		}
	}
}

// TestSelfReferenceAndCyclesDoNotHang â€” mutually recursive models are legal Go
// through pointers, so they must be emitted rather than rejected or looped on.
func TestSelfReferenceAndCyclesDoNotHang(t *testing.T) {
	cfg := Defaults()
	cfg.Models = []ModelConfig{
		{Name: "Node", Fields: []FieldConfig{
			{Name: "parent", Type: "*Node"},
			{Name: "peer", Type: "*Other"},
		}},
		{Name: "Other", Fields: []FieldConfig{
			{Name: "back", Type: "*Node"},
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("recursive models rejected: %v", err)
	}
	ordered, err := cfg.ResolveModelOrder()
	if err != nil {
		t.Fatalf("ResolveModelOrder: %v", err)
	}
	if len(ordered) != 2 {
		t.Errorf("got %d models, want 2: %v", len(ordered), names(ordered))
	}
}

func names(ms []ModelConfig) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Name
	}
	return out
}

// â”€â”€â”€ Validation â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

func TestValidateRejects(t *testing.T) {
	cases := map[string]func(*ProjectConfig){
		"port 0":        func(c *ProjectConfig) { c.Server.Port = 0 },
		"port too high": func(c *ProjectConfig) { c.Server.Port = 70000 },
		"empty host":    func(c *ProjectConfig) { c.Server.Host = "" },
		"duplicate model": func(c *ProjectConfig) {
			c.Models = []ModelConfig{{Name: "A", Fields: []FieldConfig{{Name: "x", Type: "int"}}}, {Name: "A", Fields: []FieldConfig{{Name: "y", Type: "int"}}}}
		},
		"model with no field": func(c *ProjectConfig) { c.Models = []ModelConfig{{Name: "A"}} },
		"lowercase model": func(c *ProjectConfig) {
			c.Models = []ModelConfig{{Name: "order", Fields: []FieldConfig{{Name: "x", Type: "int"}}}}
		},
		"two primary keys": func(c *ProjectConfig) {
			c.Models = []ModelConfig{{Name: "A", Fields: []FieldConfig{
				{Name: "x", Type: "int", PrimaryKey: true},
				{Name: "y", Type: "int", PrimaryKey: true},
			}}}
		},
		"duplicate field": func(c *ProjectConfig) {
			c.Models = []ModelConfig{{Name: "A", Fields: []FieldConfig{{Name: "x", Type: "int"}, {Name: "x", Type: "int"}}}}
		},
		"unknown middleware": func(c *ProjectConfig) { c.Middleware = []MiddlewareConfig{{Name: "not-a-middleware"}} },
		"bad http method":    func(c *ProjectConfig) { c.Routes = []RouteConfig{{Resource: "u", Methods: []string{"FETCH"}}} },
		"route unknown model": func(c *ProjectConfig) {
			c.Routes = []RouteConfig{{Resource: "u", Model: "Ghost"}}
		},
		"rpc reserved prefix": func(c *ProjectConfig) {
			c.JSONRPC.Enabled = true
			c.JSONRPC.Methods = []string{"rpc.internal"}
		},
		"rpc port clash": func(c *ProjectConfig) {
			c.JSONRPC.Enabled = true
			c.JSONRPC.Port = c.Server.Port
		},
		"duplicate rpc method": func(c *ProjectConfig) {
			c.JSONRPC.Enabled = true
			c.JSONRPC.Methods = []string{"sum", "sum"}
		},
		"bad fleet transport": func(c *ProjectConfig) {
			c.Fleet.Enabled = true
			c.Fleet.Transport = "carrier-pigeon"
		},
		"bad fleet backend": func(c *ProjectConfig) {
			c.Fleet.Enabled = true
			c.Fleet.Backend = "postgres"
		},
		"sample rate out of range": func(c *ProjectConfig) {
			c.Fleet.Enabled = true
			c.Fleet.SampleRate = 2
		},
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := Defaults()
			mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Error("accepted an invalid configuration")
			}
		})
	}
}

// TestUnimplementedFleetTransportIsRefusedWithAClearReason.
//
// A transport the specification names but the fleet package does not implement
// must not generate a project that cannot build. The error has to distinguish
// that case from a typo, or someone will go looking for a spelling mistake that
// is not there.
func TestUnimplementedFleetTransportIsRefusedWithAClearReason(t *testing.T) {
	// gnet and grpc only. ws and events were on this list until their
	// transports were implemented; gnettransport exists as a package but
	// delegates every method to httptransport, so generating it would produce a
	// service that reports gnet and speaks HTTP.
	for _, transport := range []string{"gnet", "grpc"} {
		cfg := Defaults()
		cfg.Fleet.Enabled = true
		cfg.Fleet.Transport = transport

		err := cfg.Validate()
		if err == nil {
			t.Errorf("%s: accepted despite having no implementation", transport)
			continue
		}
		if !strings.Contains(err.Error(), "no transport implementation") &&
			!strings.Contains(err.Error(), "no implementation") {
			t.Errorf("%s: error reads like a typo rather than a missing feature: %v", transport, err)
		}
	}

	// Every implemented transport must pass on the defaults alone. This is the
	// half of the test that would have caught the allowlist going stale in the
	// other direction: a transport that became real while validation still
	// refused it.
	for _, transport := range fleetImplementedTransports {
		cfg := Defaults()
		cfg.Fleet.Enabled = true
		cfg.Fleet.Transport = transport
		if err := cfg.Validate(); err != nil {
			t.Errorf("%s transport is in fleetImplementedTransports but was rejected: %v", transport, err)
		}
	}
}

// TestFleetWSURLIsValidated covers the failure that is invisible at runtime:
// the ws transport falls back to HTTP when it cannot dial, so a bad WebSocket
// URL does not crash â€” it silently exports over the fallback forever.
func TestFleetWSURLIsValidated(t *testing.T) {
	cases := map[string]string{
		"empty":       "",
		"http scheme": "http://localhost:9000/fleet",
		"no scheme":   "localhost:9000/fleet",
	}
	for name, wsURL := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Fleet.Enabled = true
			cfg.Fleet.Transport = "ws"
			cfg.Fleet.AggregatorWSURL = wsURL

			if err := cfg.Validate(); err == nil {
				t.Errorf("accepted aggregator_ws_url %q", wsURL)
			}
		})
	}

	// wss must be accepted, not just ws â€” refusing TLS would be worse than the
	// bug this validation exists to prevent.
	cfg := Defaults()
	cfg.Fleet.Enabled = true
	cfg.Fleet.Transport = "ws"
	cfg.Fleet.AggregatorWSURL = "wss://aggregator.example.com/fleet/ws"
	if err := cfg.Validate(); err != nil {
		t.Errorf("wss:// rejected: %v", err)
	}

	// The check is scoped to the ws transport: an http-transport project has no
	// reason to carry a WebSocket URL, and demanding one would be noise.
	cfg = Defaults()
	cfg.Fleet.Enabled = true
	cfg.Fleet.Transport = "http"
	cfg.Fleet.AggregatorWSURL = ""
	if err := cfg.Validate(); err != nil {
		t.Errorf("http transport rejected for an empty ws url: %v", err)
	}
}

// TestValidateReportsEveryProblem â€” fixing a config file should not require
// one re-run per mistake.
func TestValidateReportsEveryProblem(t *testing.T) {
	cfg := Defaults()
	cfg.Server.Port = 0
	cfg.Server.Host = ""
	cfg.Models = []ModelConfig{{Name: "A", Fields: []FieldConfig{{Name: "f", Type: "Ghost"}}}}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected errors")
	}
	msg := err.Error()
	for _, want := range []string{"server.port", "server.host", "Ghost"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error omits %q:\n%v", want, msg)
		}
	}
}
