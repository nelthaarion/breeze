package scalar

import (
	"reflect"
	"strings"
)

// InferSchema derives an OpenAPI Schema from any Go value using reflection.
// Pass a zero-value struct, a pointer, a slice, or any primitive.
// Returns nil when v is nil.
func InferSchema(v any) *Schema {
	if v == nil {
		return nil
	}
	return walkType(reflect.TypeOf(v))
}

// walkType recurses through a reflect.Type to build a Schema.
func walkType(t reflect.Type) *Schema {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}

	switch t.Kind() {
	case reflect.Struct:
		return structSchema(t)
	case reflect.Slice, reflect.Array:
		return &Schema{Type: "array", Items: walkType(t.Elem())}
	case reflect.Map:
		return &Schema{Type: "object"}
	case reflect.Bool:
		return &Schema{Type: "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return &Schema{Type: "integer", Format: intFormat(t.Kind())}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return &Schema{Type: "integer", Format: "uint64"}
	case reflect.Float32:
		return &Schema{Type: "number", Format: "float"}
	case reflect.Float64:
		return &Schema{Type: "number", Format: "double"}
	case reflect.String:
		return &Schema{Type: "string"}
	default:
		return &Schema{Type: "string"}
	}
}

func structSchema(t reflect.Type) *Schema {
	props := map[string]*Schema{}
	required := []string{}

	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}

		name, omitempty := jsonFieldName(f)
		if name == "-" {
			continue
		}

		schema := walkType(f.Type)

		if desc := f.Tag.Get("description"); desc != "" {
			schema.Description = desc
		}
		if ex := f.Tag.Get("example"); ex != "" {
			schema.Example = ex
		}

		props[name] = schema
		if !omitempty {
			required = append(required, name)
		}
	}

	s := &Schema{Type: "object", Properties: props}
	if len(required) > 0 {
		s.Required = required
	}
	return s
}

// jsonFieldName returns the JSON key name and whether omitempty is set.
func jsonFieldName(f reflect.StructField) (string, bool) {
	tag := f.Tag.Get("json")
	if tag == "" {
		return f.Name, false
	}
	parts := strings.Split(tag, ",")
	name := parts[0]
	if name == "" {
		name = f.Name
	}
	omitempty := false
	for _, opt := range parts[1:] {
		if opt == "omitempty" {
			omitempty = true
		}
	}
	return name, omitempty
}

func intFormat(k reflect.Kind) string {
	switch k {
	case reflect.Int64:
		return "int64"
	case reflect.Int32:
		return "int32"
	default:
		return "integer"
	}
}
