package generator

// Validation and Go type resolution for the generator configuration.
//
// The rule this file exists to enforce is that generation either produces a
// project that compiles or fails with an error naming exactly what was wrong.
// Emitting Go source that the compiler will reject is the worst outcome
// available to a generator: the error surfaces far from its cause, in code the
// developer did not write.
//
// So every model field's type is parsed and resolved here, before a single file
// is written, and an unresolvable reference is reported as
//
//	model Order field shipping_address references unknown type AddressBook
//
// which names the model, the field and the type â€” the three things needed to
// find it in the config.

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Validate checks the whole configuration.
//
// All errors are collected rather than returned one at a time. A developer
// fixing a config file wants the whole list, not a dozen re-runs.
func (c *ProjectConfig) Validate() error {
	var errs []string

	errs = append(errs, c.validateServer()...)
	errs = append(errs, c.validateFleet()...)
	errs = append(errs, c.validateJSONRPC()...)
	errs = append(errs, c.validateMiddleware()...)
	errs = append(errs, c.validateModels()...)
	errs = append(errs, c.validateRoutes()...)

	if len(errs) == 0 {
		return nil
	}
	if len(errs) == 1 {
		return fmt.Errorf("%s", errs[0])
	}
	return fmt.Errorf("%d configuration problems:\n  - %s", len(errs), strings.Join(errs, "\n  - "))
}

func (c *ProjectConfig) validateServer() []string {
	var errs []string
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		errs = append(errs, fmt.Sprintf("server.port %d is not a valid port", c.Server.Port))
	}
	if strings.TrimSpace(c.Server.Host) == "" {
		errs = append(errs, "server.host is empty")
	}
	return errs
}

// validateFleet checks the transport and backend against what is actually
// implemented.
//
// The distinction between "named by the spec" and "implemented" is deliberate
// and is reported differently: an unknown value is a typo, while a known but
// unimplemented one is a real feature that is not ready. Conflating them would
// send someone hunting for a spelling mistake that is not there.
func (c *ProjectConfig) validateFleet() []string {
	if !c.Fleet.Enabled {
		return nil
	}
	var errs []string

	switch {
	case !slices.Contains(fleetTransports, c.Fleet.Transport):
		errs = append(errs, fmt.Sprintf("fleet.transport %q is not one of %s",
			c.Fleet.Transport, strings.Join(fleetTransports, ", ")))
	case !slices.Contains(fleetImplementedTransports, c.Fleet.Transport):
		errs = append(errs, fmt.Sprintf(
			"fleet.transport %q is defined by the Fleet specification but has no transport implementation in the fleet package yet, so it cannot be generated â€” currently generatable: %s",
			c.Fleet.Transport,
			strings.Join(fleetImplementedTransports, ", "),
		))
	}

	// Backend is meaningful only for the events transport. Setting it elsewhere
	// is a no-op rather than an error, but a *wrong* value is still reported so
	// a typo does not lie dormant until the transport is switched.
	if !slices.Contains(fleetBackends, c.Fleet.Backend) {
		errs = append(errs, fmt.Sprintf("fleet.backend %q is not one of %s",
			c.Fleet.Backend, strings.Join(fleetBackends, ", ")))
	} else if c.Fleet.Transport == "events" && !slices.Contains(fleetImplementedBackends, c.Fleet.Backend) {
		errs = append(errs, fmt.Sprintf(
			"fleet.backend %q has no implementation in the events package yet â€” currently generatable: %s",
			c.Fleet.Backend,
			strings.Join(fleetImplementedBackends, ", "),
		))
	}

	if c.Fleet.SampleRate < 0 || c.Fleet.SampleRate > 1 {
		errs = append(errs, fmt.Sprintf("fleet.sample_rate %v is outside 0..1", c.Fleet.SampleRate))
	}
	if strings.TrimSpace(c.Fleet.AggregatorURL) == "" {
		errs = append(errs, "fleet.aggregator_url is empty")
	}

	// The ws transport dials aggregator_ws_url and falls back to HTTP if it
	// cannot. A wrong scheme here would therefore not fail loudly at startup â€”
	// it would silently export over the fallback for the life of the process,
	// which is the failure mode most likely to be mistaken for working.
	if c.Fleet.Transport == "ws" {
		wsURL := strings.TrimSpace(c.Fleet.AggregatorWSURL)
		switch {
		case wsURL == "":
			errs = append(errs, "fleet.transport is \"ws\" but fleet.aggregator_ws_url is empty")
		case !strings.HasPrefix(wsURL, "ws://") && !strings.HasPrefix(wsURL, "wss://"):
			errs = append(errs, fmt.Sprintf(
				"fleet.aggregator_ws_url %q must begin with ws:// or wss://", wsURL))
		}
	}
	return errs
}

func (c *ProjectConfig) validateJSONRPC() []string {
	if !c.JSONRPC.Enabled {
		return nil
	}
	var errs []string

	if c.JSONRPC.Port < 1 || c.JSONRPC.Port > 65535 {
		errs = append(errs, fmt.Sprintf("jsonrpc.port %d is not a valid port", c.JSONRPC.Port))
	}
	if c.JSONRPC.Port == c.Server.Port {
		errs = append(errs, fmt.Sprintf(
			"jsonrpc.port %d is the same as server.port â€” JSON-RPC listens on its own gnet listener and cannot share the HTTP port",
			c.JSONRPC.Port,
		))
	}
	if c.JSONRPC.MaxMessageBytes < 0 {
		errs = append(errs, "jsonrpc.max_message_bytes is negative")
	}

	seen := map[string]bool{}
	for _, m := range c.JSONRPC.Methods {
		if strings.TrimSpace(m) == "" {
			errs = append(errs, "jsonrpc.methods contains an empty method name")
			continue
		}
		// The spec reserves the rpc. prefix for internal methods (Â§4).
		if strings.HasPrefix(m, "rpc.") {
			errs = append(errs, fmt.Sprintf(
				"jsonrpc method %q uses the rpc. prefix, which JSON-RPC 2.0 Â§4 reserves for internal methods",
				m,
			))
		}
		if seen[m] {
			errs = append(errs, fmt.Sprintf("jsonrpc method %q is listed twice", m))
		}
		seen[m] = true
	}

	// A blocking method must be one of the declared methods. Otherwise the
	// generated code would register a handler function that was never
	// scaffolded, and the project would not compile â€” or, if the name merely
	// differs in case or spelling from a real one, that method would quietly
	// stay on the event loop, which is the failure this list exists to prevent.
	for _, b := range c.JSONRPC.BlockingMethods {
		if !seen[b] {
			errs = append(errs, fmt.Sprintf(
				"jsonrpc.blocking_methods names %q, which is not in jsonrpc.methods", b))
		}
	}
	return errs
}

func (c *ProjectConfig) validateMiddleware() []string {
	var errs []string
	seen := map[string]bool{}

	for _, m := range c.Middleware {
		if strings.TrimSpace(m.Name) == "" {
			errs = append(errs, "a middleware entry has no name")
			continue
		}
		if seen[m.Name] {
			errs = append(errs, fmt.Sprintf("middleware %q is configured twice", m.Name))
		}
		seen[m.Name] = true

		// The middleware must be one the framework provides, since the
		// generator wires it rather than writing it.
		//
		// The silent resolution matters here: validation runs over the whole
		// config before anything is generated, and resolveFeatureName announces
		// each alias it maps. Using it would print those notes during a
		// validation pass that may end in an error and generate nothing, and
		// print them twice for a name that is both aliased and unknown.
		if _, ok := features[canonicalFeatureName(m.Name)]; !ok {
			errs = append(errs, fmt.Sprintf("middleware %q is not a known feature%s",
				m.Name, suggestFeature(canonicalFeatureName(m.Name))))
		}

		if m.RPS < 0 {
			errs = append(errs, fmt.Sprintf("middleware %s rps is negative", m.Name))
		}
	}
	return errs
}

func (c *ProjectConfig) validateRoutes() []string {
	var errs []string
	seen := map[string]bool{}
	models := c.modelNames()

	for _, r := range c.Routes {
		if strings.TrimSpace(r.Resource) == "" {
			errs = append(errs, "a route entry has no resource name")
			continue
		}
		if seen[r.Resource] {
			errs = append(errs, fmt.Sprintf("route resource %q is configured twice", r.Resource))
		}
		seen[r.Resource] = true

		for _, m := range r.Methods {
			if !slices.Contains(httpMethods, strings.ToUpper(m)) {
				errs = append(
					errs,
					fmt.Sprintf("route %s method %q is not an HTTP method", r.Resource, m),
				)
			}
		}
		// A route may name a model, and that reference is checked here for the
		// same reason field types are: the generated handler would not compile.
		if r.Model != "" && !models[r.Model] {
			errs = append(
				errs,
				fmt.Sprintf("route %s references unknown model %s", r.Resource, r.Model),
			)
		}
	}
	return errs
}

var httpMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"}

// modelNames is the set of model names declared in the config, which is the
// universe a custom type reference can resolve against.
func (c *ProjectConfig) modelNames() map[string]bool {
	out := make(map[string]bool, len(c.Models))
	for _, m := range c.Models {
		out[m.Name] = true
	}
	return out
}

// validateModels checks names, duplicates and every field type.
func (c *ProjectConfig) validateModels() []string {
	var errs []string
	seen := map[string]bool{}
	models := c.modelNames()

	for _, m := range c.Models {
		if strings.TrimSpace(m.Name) == "" {
			errs = append(errs, "a model entry has no name")
			continue
		}
		// Generated as a Go type name, so it has to be usable as one.
		if !isExportableIdent(m.Name) {
			errs = append(errs, fmt.Sprintf("model name %q is not a usable Go type name", m.Name))
		}
		if seen[m.Name] {
			errs = append(errs, fmt.Sprintf("model %q is defined twice", m.Name))
		}
		seen[m.Name] = true

		if len(m.Fields) == 0 {
			errs = append(errs, fmt.Sprintf("model %s has no fields", m.Name))
		}

		fieldSeen := map[string]bool{}
		keys := 0
		for _, f := range m.Fields {
			if strings.TrimSpace(f.Name) == "" {
				errs = append(errs, fmt.Sprintf("model %s has a field with no name", m.Name))
				continue
			}
			if fieldSeen[f.Name] {
				errs = append(
					errs,
					fmt.Sprintf("model %s field %s is defined twice", m.Name, f.Name),
				)
			}
			fieldSeen[f.Name] = true
			if f.PrimaryKey {
				keys++
			}

			if err := validateFieldType(m.Name, f, models); err != nil {
				errs = append(errs, err.Error())
			}
		}
		if keys > 1 {
			errs = append(
				errs,
				fmt.Sprintf("model %s marks %d fields as primary_key", m.Name, keys),
			)
		}
	}
	return errs
}

// primitives are the non-model types a field may use.
//
// This is an allowlist rather than a rejection list. A generator that accepts
// any identifier it does not recognise as "probably a type" will emit code
// referencing types that do not exist; refusing the unknown is what makes the
// unresolved-reference error possible at all.
var primitives = map[string]bool{
	"string": true, "bool": true,
	"int": true, "int8": true, "int16": true, "int32": true, "int64": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true,
	"float32": true, "float64": true,
	"byte": true, "rune": true,
	// Types whose imports the model generator knows how to add.
	"time.Time": true, "time.Duration": true,
	"json.RawMessage": true,
	"[]byte":          true,
}

// validateFieldType parses a field's type expression and resolves every named
// type in it.
func validateFieldType(model string, f FieldConfig, models map[string]bool) error {
	expr := strings.TrimSpace(f.Type)
	if expr == "" {
		return fmt.Errorf("model %s field %s has no type", model, f.Name)
	}
	refs, err := parseTypeExpr(expr)
	if err != nil {
		return fmt.Errorf("model %s field %s type %q: %w", model, f.Name, f.Type, err)
	}
	for _, ref := range refs {
		if primitives[ref] || models[ref] {
			continue
		}
		// The wording is fixed by the specification for this error, because it
		// is the one a developer sees most often.
		return fmt.Errorf("model %s field %s references unknown type %s", model, f.Name, ref)
	}
	return nil
}

// parseTypeExpr reduces a Go type expression to the named types inside it.
//
// The composite forms are peeled off recursively â€” *T, []T, map[K]V and any
// nesting of them â€” leaving the identifiers that have to resolve. Returning the
// names rather than a tree is enough for validation and for deciding which
// models a model depends on, and avoids duplicating go/types for no gain.
//
// []byte is treated as a leaf before the slice case, since it is a primitive in
// its own right rather than a slice of a primitive.
func parseTypeExpr(expr string) ([]string, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, fmt.Errorf("empty type")
	}

	if primitives[expr] {
		return []string{expr}, nil
	}

	switch {
	case strings.HasPrefix(expr, "*"):
		return parseTypeExpr(expr[1:])

	case strings.HasPrefix(expr, "[]"):
		return parseTypeExpr(expr[2:])

	case strings.HasPrefix(expr, "map["):
		key, val, err := splitMap(expr)
		if err != nil {
			return nil, err
		}
		keyRefs, err := parseTypeExpr(key)
		if err != nil {
			return nil, err
		}
		// A map key must be comparable, and a model struct used as a key would
		// compile only by accident. Restricting keys to primitives keeps the
		// generated code honest.
		for _, k := range keyRefs {
			if !primitives[k] {
				return nil, fmt.Errorf("map key type %q must be a primitive", key)
			}
		}
		valRefs, err := parseTypeExpr(val)
		if err != nil {
			return nil, err
		}
		return append(keyRefs, valRefs...), nil
	}

	// A bare name: either a primitive (handled above), a qualified name, or a
	// model reference. Reject anything that is not a usable identifier here, so
	// malformed input is a parse error rather than an unresolved reference.
	if !isTypeName(expr) {
		return nil, fmt.Errorf("%q is not a valid type expression", expr)
	}
	return []string{expr}, nil
}

// splitMap splits "map[K]V" into K and V, honouring nesting so that
// map[string]map[string]int and map[string][]*T split at the right bracket.
func splitMap(expr string) (key, val string, err error) {
	rest := expr[len("map["):]
	depth := 1
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				key = rest[:i]
				val = rest[i+1:]
				if strings.TrimSpace(key) == "" {
					return "", "", fmt.Errorf("map has no key type")
				}
				if strings.TrimSpace(val) == "" {
					return "", "", fmt.Errorf("map has no value type")
				}
				return key, val, nil
			}
		}
	}
	return "", "", fmt.Errorf("unbalanced brackets in %q", expr)
}

// isTypeName reports whether s is an identifier or a package-qualified one.
func isTypeName(s string) bool {
	if pkg, name, ok := strings.Cut(s, "."); ok {
		return isIdent(pkg) && isIdent(name)
	}
	return isIdent(s)
}

func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_':
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// isExportableIdent reports whether s can be a generated exported type name.
func isExportableIdent(s string) bool {
	return isIdent(s) && s[0] >= 'A' && s[0] <= 'Z'
}

// ModelRefs returns the model names a model's fields refer to.
//
// Only model references are returned; primitives are dropped. This is the edge
// set the dependency ordering in ResolveModelOrder walks.
func (m ModelConfig) ModelRefs(models map[string]bool) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range m.Fields {
		refs, err := parseTypeExpr(strings.TrimSpace(f.Type))
		if err != nil {
			// Validation reports this; ordering just ignores it.
			continue
		}
		for _, r := range refs {
			if r == m.Name || !models[r] || seen[r] {
				continue
			}
			seen[r] = true
			out = append(out, r)
		}
	}
	sort.Strings(out)
	return out
}

// ResolveModelOrder returns the models in dependency order: a model appears
// after every model it references.
//
// Ordering matters for deterministic output, and determinism is why ties are
// broken by name rather than by map iteration order. Reference cycles are not
// an error â€” Go handles mutually recursive struct types through pointers, and
// refusing them would reject legal models â€” so a cycle is emitted in name order
// instead of failing.
func (c *ProjectConfig) ResolveModelOrder() ([]ModelConfig, error) {
	byName := make(map[string]ModelConfig, len(c.Models))
	names := make([]string, 0, len(c.Models))
	for _, m := range c.Models {
		byName[m.Name] = m
		names = append(names, m.Name)
	}
	sort.Strings(names)
	set := c.modelNames()

	var out []ModelConfig
	state := map[string]int{} // 0 unvisited, 1 in progress, 2 done

	var visit func(string)
	visit = func(name string) {
		switch state[name] {
		case 1, 2:
			// In progress means a cycle; emitting the node now and letting the
			// caller finish is what breaks it without dropping anything.
			return
		}
		state[name] = 1
		m := byName[name]
		for _, ref := range m.ModelRefs(set) {
			visit(ref)
		}
		state[name] = 2
		out = append(out, m)
	}

	for _, name := range names {
		visit(name)
	}
	return out, nil
}
