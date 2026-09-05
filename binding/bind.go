package binding

import (
	"fmt"
	"net/url"
	"reflect"
	"strconv"
	"strings"

	"github.com/goccy/go-json"
)

// Source is a function that populates a destination struct from a data source
// (e.g., JSON body, query parameters, path parameters, form data).
type Source func(dst any) error

// Bind decodes data from one or more sources into dst (a pointer to a struct),
// then validates the result using struct tags. Sources are applied in order;
// later sources override earlier ones for overlapping fields.
//
// dst must be a pointer to a struct. Each source attempts to populate fields
// based on source-specific tags (json for JSONBody, form/query for Query/Form,
// param for Path). After all sources succeed, validation rules in the
// validate:"..." tags are checked.
//
// If validation fails, Bind returns a *ValidationError. All other errors are
// returned as-is (e.g., json.SyntaxError for malformed JSON).
func Bind(dst any, sources ...Source) error {
	// Validate that dst is a pointer to a struct.
	ptrVal := reflect.ValueOf(dst)
	if ptrVal.Kind() != reflect.Ptr || ptrVal.IsNil() {
		return fmt.Errorf("binding: dst must be a non-nil pointer, got %T", dst)
	}

	val := ptrVal.Elem()
	if val.Kind() != reflect.Struct {
		return fmt.Errorf("binding: dst must point to a struct, got %T", dst)
	}

	// Apply each source in order.
	for _, source := range sources {
		if err := source(dst); err != nil {
			return err
		}
	}

	// Compile validation plan and validate.
	plan, err := compilePlan(val.Type())
	if err != nil {
		return err
	}

	v := &validator{plan: plan, val: val}
	return v.validate()
}

// JSONBody returns a Source that unmarshals JSON from body into dst.
// The body slice must not be retained after the source function returns.
func JSONBody(body []byte) Source {
	return func(dst any) error {
		if len(body) == 0 {
			return nil
		}
		return json.Unmarshal(body, dst)
	}
}

// Query returns a Source that populates dst from query parameters.
// Fields are matched using the "form" or "query" struct tag (form is checked first).
// Supported types: string, int/int64, float64, bool, and their pointer variants.
func Query(values url.Values) Source {
	return func(dst any) error {
		if len(values) == 0 {
			return nil
		}
		return populateFromValues(dst, values, "form", "query")
	}
}

// Form returns a Source that populates dst from form-encoded parameters.
// This is identical to Query, accepting url.Values from a parsed form body.
func Form(values url.Values) Source {
	return func(dst any) error {
		if len(values) == 0 {
			return nil
		}
		return populateFromValues(dst, values, "form", "query")
	}
}

// Path returns a Source that populates dst from path parameters.
// Fields are matched using the "param" struct tag.
func Path(params map[string]string) Source {
	return func(dst any) error {
		if len(params) == 0 {
			return nil
		}
		return populateFromMap(dst, params, "param")
	}
}

// populateFromValues assigns string values from url.Values to struct fields
// using the specified tag names (checked in order).
func populateFromValues(dst any, values url.Values, tagNames ...string) error {
	ptrVal := reflect.ValueOf(dst)
	if ptrVal.Kind() != reflect.Ptr || ptrVal.IsNil() {
		return nil
	}

	val := ptrVal.Elem()
	if val.Kind() != reflect.Struct {
		return nil
	}

	typ := val.Type()
	for i := 0; i < typ.NumField(); i++ {
		fld := typ.Field(i)
		if !fld.IsExported() {
			continue
		}

		// Try each tag name in order; use the first one that matches.
		var paramName string
		for _, tagName := range tagNames {
			if n := fld.Tag.Get(tagName); n != "" {
				paramName = n
				break
			}
		}

		if paramName == "" {
			continue
		}

		// Get the value from the URL values (first value only).
		strVal := values.Get(paramName)
		if strVal == "" {
			continue
		}

		// Assign to the struct field, converting type as needed.
		if err := assignString(val.Field(i), strVal); err != nil {
			return err
		}
	}

	return nil
}

// populateFromMap assigns string values from a map to struct fields
// using the specified tag name.
func populateFromMap(dst any, params map[string]string, tagName string) error {
	ptrVal := reflect.ValueOf(dst)
	if ptrVal.Kind() != reflect.Ptr || ptrVal.IsNil() {
		return nil
	}

	val := ptrVal.Elem()
	if val.Kind() != reflect.Struct {
		return nil
	}

	typ := val.Type()
	for i := 0; i < typ.NumField(); i++ {
		fld := typ.Field(i)
		if !fld.IsExported() {
			continue
		}

		paramName := fld.Tag.Get(tagName)
		if paramName == "" {
			continue
		}

		strVal, ok := params[paramName]
		if !ok || strVal == "" {
			continue
		}

		// Assign to the struct field, converting type as needed.
		if err := assignString(val.Field(i), strVal); err != nil {
			return err
		}
	}

	return nil
}

// assignString converts a string to the field's type and assigns it.
// Supports: string, int, int64, float64, bool, and their pointer variants.
func assignString(fld reflect.Value, strVal string) error {
	// Handle pointer types: allocate if nil.
	if fld.Kind() == reflect.Ptr {
		if fld.IsNil() {
			fld.Set(reflect.New(fld.Type().Elem()))
		}
		fld = fld.Elem()
	}

	switch fld.Kind() {
	case reflect.String:
		fld.SetString(strVal)

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		i, err := strconv.ParseInt(strVal, 10, 64)
		if err != nil {
			return fmt.Errorf("binding: invalid integer value: %q", strVal)
		}
		fld.SetInt(i)

	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		u, err := strconv.ParseUint(strVal, 10, 64)
		if err != nil {
			return fmt.Errorf("binding: invalid unsigned integer value: %q", strVal)
		}
		fld.SetUint(u)

	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(strVal, 64)
		if err != nil {
			return fmt.Errorf("binding: invalid float value: %q", strVal)
		}
		fld.SetFloat(f)

	case reflect.Bool:
		b, err := parseBool(strVal)
		if err != nil {
			return err
		}
		fld.SetBool(b)

	default:
		// Unsupported type — skip it.
	}

	return nil
}

// parseBool converts a string to a boolean, accepting common true/false values.
func parseBool(s string) (bool, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "true", "1", "yes", "on":
		return true, nil
	case "false", "0", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("binding: invalid boolean value: %q", s)
	}
}
