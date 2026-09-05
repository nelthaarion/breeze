package generator

// JSON Schema for the generator configuration.
//
// This file adds no schema. It publishes the one that already exists.
//
// ProjectConfig is the declaration of every generatable setting, and config.go
// already walks it — by yaml tag, in FlagPaths — to enumerate the flags the
// setters accept. A hand-written JSON Schema alongside that would be a second
// declaration of the same thing, and the two would drift the first time a field
// was added: the flag would work, the schema would not mention it, and an agent
// reading the schema would have no way to find out. So the schema is walked out
// of the same struct, through the same yamlTagName, and every leaf carries the
// flag path that FlagPaths would have printed for it. A field that is added
// appears in both at once, or in neither.
//
// The enumerations are borrowed rather than restated for the same reason. The
// values Fleet accepts are the package-level vars validateFleet checks against,
// and the HTTP verbs are the list validateRoutes checks against, so a schema
// cannot advertise a value that validation will reject a moment later.
//
// Reflection here runs at generation time, over the generator's own config
// struct, and nothing it produces reaches a generated project — the rule about
// keeping reflection out of generated accessors is untouched.

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// jsonSchemaDialect is the dialect the emitted schema declares.
//
// 2020-12 is named because that is what the MCP tool descriptors in this
// repository already use, and a schema that travels next to them should not
// answer a different dialect.
const jsonSchemaDialect = "https://json-schema.org/draft/2020-12/schema"

// ConfigJSONSchema returns the JSON Schema for a configuration section, or for
// the whole of ProjectConfig when section is empty.
//
// section is a top-level yaml key: "server", "fleet", "models". An unknown one
// is an error listing the keys that exist, since a caller that guessed wrong
// needs the alternatives more than it needs the failure.
func ConfigJSONSchema(section string) (json.RawMessage, error) {
	t := reflect.TypeOf(ProjectConfig{})
	defaults := reflect.ValueOf(Defaults())

	var node map[string]any
	switch section = strings.TrimSpace(section); section {
	case "":
		node = schemaForType(t, nil, defaults)
		node["title"] = "breeze.yaml"
		node["description"] = "The complete generator configuration. Every leaf is also settable as a --flag, given under x-flag."

	default:
		field, ok := fieldByTagName(t, section)
		if !ok {
			return nil, fmt.Errorf("unknown configuration section %q — one of: %s",
				section, strings.Join(ConfigSections(), ", "))
		}
		var value reflect.Value
		if v, ok := valueByTagName(defaults, section); ok {
			value = v
		}
		node = schemaForType(field.Type, []string{section}, value)
		node["title"] = section
	}

	node["$schema"] = jsonSchemaDialect

	out, err := json.Marshal(node)
	if err != nil {
		// Unreachable in practice: every value put in the tree above is a
		// map, slice, string, bool or number.
		return nil, fmt.Errorf("encoding schema for %q: %w", section, err)
	}
	return out, nil
}

// ConfigSections lists the top-level configuration keys, in schema order.
//
// The order is the field order of ProjectConfig rather than alphabetical,
// because that order is itself informative: it runs from what a project is
// (module, server) through the features it enables to the code it declares.
func ConfigSections() []string {
	t := reflect.TypeOf(ProjectConfig{})
	out := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		out = append(out, yamlTagName(t.Field(i)))
	}
	return out
}

// schemaForType builds the schema node for one Go type.
//
// prefix is the flag path walked so far, and is what makes the emitted x-flag
// values identical to FlagPaths' output — including the "<name>" placeholder
// that stands in for a keyed slice element, since an element name is chosen by
// the user and cannot be enumerated.
//
// defaults is the corresponding value from Defaults(), or the zero Value when
// there is none — inside a slice element, for instance, where no default entry
// exists to read.
func schemaForType(t reflect.Type, prefix []string, defaults reflect.Value) map[string]any {
	switch t.Kind() {
	case reflect.Struct:
		props := make(map[string]any, t.NumField())
		order := make([]string, 0, t.NumField())

		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name := yamlTagName(f)
			path := append(append([]string{}, prefix...), name)

			var fieldDefault reflect.Value
			if defaults.IsValid() && defaults.Kind() == reflect.Struct {
				fieldDefault = defaults.Field(i)
			}

			props[name] = schemaForType(f.Type, path, fieldDefault)
			order = append(order, name)
		}

		node := map[string]any{
			"type":       "object",
			"properties": props,
			// Closed by construction: the YAML decoder is run with
			// KnownFields(true), so an unknown key is already an error. A
			// schema that permitted extras would describe a laxer parser than
			// the one that exists.
			"additionalProperties": false,
			"x-field-order":        order,
		}
		if req := identifyingFields(t); len(req) > 0 {
			node["required"] = req
		}
		return node

	case reflect.Slice:
		elem := t.Elem()
		if elem.Kind() == reflect.Struct {
			// A keyed section. Its elements are addressed by name in a flag
			// path, which is where the "<name>" segment comes from.
			return map[string]any{
				"type":  "array",
				"items": schemaForType(elem, append(append([]string{}, prefix...), "<name>"), reflect.Value{}),
			}
		}
		node := map[string]any{
			"type":  "array",
			"items": schemaForType(elem, prefix, reflect.Value{}),
			// Set on the array rather than the element: a list is set from one
			// flag as a comma-separated value, not one flag per item.
			"x-flag":        flagPath(prefix),
			"x-flag-format": "comma-separated",
		}
		applyEnum(node, prefix, true)
		applyDefault(node, defaults)
		return node

	default:
		node := map[string]any{"type": jsonTypeOf(t.Kind())}
		if format := jsonFormatOf(t.Kind()); format != "" {
			node["format"] = format
		}
		if len(prefix) > 0 {
			node["x-flag"] = flagPath(prefix)
		}
		applyEnum(node, prefix, false)
		applyDefault(node, defaults)
		return node
	}
}

// flagPath renders a walked path the way the CLI accepts it.
func flagPath(prefix []string) string { return strings.Join(prefix, ".") }

// jsonTypeOf maps a Go kind to its JSON Schema type.
//
// Kinds that assign cannot handle are reported as "string": that is what the
// CLI would try to parse the value as before failing, so it is the honest
// answer for a field the generator cannot really set.
func jsonTypeOf(k reflect.Kind) string {
	switch k {
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "integer"
	case reflect.Float32, reflect.Float64:
		return "number"
	default:
		return "string"
	}
}

// jsonFormatOf annotates the numeric widths a consumer might round-trip badly.
func jsonFormatOf(k reflect.Kind) string {
	switch k {
	case reflect.Int64, reflect.Uint64:
		return "int64"
	case reflect.Int32, reflect.Uint32:
		return "int32"
	default:
		return ""
	}
}

// applyDefault records the value Defaults() puts in a field.
//
// A zero value is left out. Reporting `"default": ""` would tell a reader that
// empty is a deliberate default when it only means the field has none, and the
// difference matters for fields like module, where empty means "read go.mod".
func applyDefault(node map[string]any, v reflect.Value) {
	if !v.IsValid() || v.IsZero() {
		return
	}
	switch v.Kind() {
	case reflect.String:
		node["default"] = v.String()
	case reflect.Bool:
		node["default"] = v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		node["default"] = v.Int()
	case reflect.Float32, reflect.Float64:
		node["default"] = v.Float()
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.String {
			node["default"] = v.Interface()
		}
	}
}

// configEnums are the fields whose accepted values are a closed set.
//
// The slices are the ones validation reads — not copies of them — so an enum
// here cannot list a value config_validate.go will reject. Where the schema
// admits more than the generators implement, the implemented subset is carried
// alongside as x-implemented rather than narrowing the enum: the wider set is
// what the specification names, and an agent is better served by "this exists
// but is not generatable yet" than by being told it does not exist.
var configEnums = map[string]struct {
	values      []string
	implemented []string
}{
	"fleet.transport":       {fleetTransports, fleetImplementedTransports},
	"fleet.backend":         {fleetBackends, fleetImplementedBackends},
	"routes.<name>.methods": {httpMethods, httpMethods},
}

// applyEnum attaches the accepted values for a path, if it has a closed set.
//
// forItems puts the enum on the array's items, where a list of strings is
// constrained per element rather than as a whole.
func applyEnum(node map[string]any, prefix []string, forItems bool) {
	spec, ok := configEnums[flagPath(prefix)]
	if !ok {
		return
	}

	target := node
	if forItems {
		items, ok := node["items"].(map[string]any)
		if !ok {
			return
		}
		target = items
	}

	target["enum"] = spec.values
	if len(spec.implemented) != len(spec.values) {
		target["x-implemented"] = spec.implemented
	}
}

// identifyingFields returns the yaml names that must be present for a keyed
// slice element to be addressable, which is what resolvePath needs to find or
// append one.
func identifyingFields(t reflect.Type) []string {
	for _, key := range elemNameFields {
		if f, ok := fieldByTagName(t, key); ok && f.Type.Kind() == reflect.String {
			return []string{key}
		}
	}
	return nil
}

// fieldByTagName is fieldByYAMLTag on a type rather than a value, for the
// schema walk, which has no value to look at.
func fieldByTagName(t reflect.Type, name string) (reflect.StructField, bool) {
	for i := 0; i < t.NumField(); i++ {
		if f := t.Field(i); yamlTagName(f) == name {
			return f, true
		}
	}
	return reflect.StructField{}, false
}

// valueByTagName is the value half of the same lookup.
func valueByTagName(v reflect.Value, name string) (reflect.Value, bool) {
	if !v.IsValid() || v.Kind() != reflect.Struct {
		return reflect.Value{}, false
	}
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		if yamlTagName(t.Field(i)) == name {
			return v.Field(i), true
		}
	}
	return reflect.Value{}, false
}

// SchemaFlagPaths lists the x-flag values the schema carries, sorted.
//
// It exists so the schema can be held to FlagPaths by a test rather than by
// review: the two are produced by separate walks of the same struct, and if
// they ever disagree one of them is wrong.
func SchemaFlagPaths(raw json.RawMessage) ([]string, error) {
	var node any
	if err := json.Unmarshal(raw, &node); err != nil {
		return nil, err
	}
	var out []string
	collectFlagPaths(node, &out)
	sort.Strings(out)
	return out, nil
}

func collectFlagPaths(node any, out *[]string) {
	switch n := node.(type) {
	case map[string]any:
		if flag, ok := n["x-flag"].(string); ok && flag != "" {
			*out = append(*out, flag)
		}
		for key, child := range n {
			// x-field-order is a list of names, not of schemas, and
			// descending into it would find nothing.
			if key == "x-field-order" {
				continue
			}
			collectFlagPaths(child, out)
		}
	case []any:
		for _, child := range n {
			collectFlagPaths(child, out)
		}
	}
}
