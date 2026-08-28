package contracts

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/nelthaarion/breeze/fleet"
	"github.com/nelthaarion/breeze/scalar"
)

// Validate checks the focused JSON-Schema subset Breeze itself emits:
// object/array/scalar types, required, enum, and additionalProperties. It does
// not claim general JSON Schema compliance.
func Validate(span fleet.Span, caller string, op scalar.Operation, now int64) []Violation {
	var out []Violation
	if len(span.RequestPayload) > 0 && op.RequestBody != nil {
		if schema := mediaSchema(op.RequestBody.Content); schema != nil {
			out = append(out, validatePayload(span, caller, "request", span.RequestPayload, schema, now)...)
		}
	}
	if len(span.ResponsePayload) > 0 {
		resp, ok := responseFor(op, span.Status)
		if ok {
			if schema := mediaSchema(resp.Content); schema != nil {
				out = append(out, validatePayload(span, caller, "response", span.ResponsePayload, schema, now)...)
			}
		}
	}
	return out
}

func mediaSchema(content map[string]scalar.MediaType) *scalar.Schema {
	if mt, ok := content["application/json"]; ok {
		return mt.Schema
	}
	for _, mt := range content {
		return mt.Schema
	}
	return nil
}

func responseFor(op scalar.Operation, status int) (scalar.Response, bool) {
	if r, ok := op.Responses[strconv.Itoa(status)]; ok {
		return r, true
	}
	if r, ok := op.Responses[strconv.Itoa(status/100)+"XX"]; ok {
		return r, true
	}
	r, ok := op.Responses["default"]
	return r, ok
}

func validatePayload(span fleet.Span, caller, direction string, raw json.RawMessage, schema *scalar.Schema, now int64) []Violation {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return []Violation{makeViolation(span, caller, direction, "", "type", schema.Type, "invalid JSON", "error", now)}
	}
	var out []Violation
	walkSchema(span, caller, direction, "", value, schema, now, &out)
	return out
}

func walkSchema(span fleet.Span, caller, direction, path string, value any, schema *scalar.Schema, now int64, out *[]Violation) {
	if schema == nil {
		return
	}
	if !typeMatches(schema.Type, value) {
		*out = append(*out, makeViolation(span, caller, direction, path, "type", schema.Type, observedType(value), "error", now))
		return
	}
	if len(schema.Enum) > 0 {
		found := false
		for _, candidate := range schema.Enum {
			if fmt.Sprint(candidate) == fmt.Sprint(value) {
				found = true
				break
			}
		}
		if !found {
			*out = append(*out, makeViolation(span, caller, direction, path, "enum", fmt.Sprint(schema.Enum), fmt.Sprint(value), "error", now))
		}
	}
	switch typed := value.(type) {
	case map[string]any:
		for _, required := range schema.Required {
			if _, ok := typed[required]; !ok {
				*out = append(*out, makeViolation(span, caller, direction, pointer(path, required), "required", "present", "missing", "error", now))
			}
		}
		for key, child := range typed {
			childSchema, known := schema.Properties[key]
			if !known {
				severity := "warning"
				if schema.AdditionalProperties != nil && !*schema.AdditionalProperties {
					severity = "error"
				}
				*out = append(*out, makeViolation(span, caller, direction, pointer(path, key), "additionalProperties", "declared property", "unknown field", severity, now))
				continue
			}
			walkSchema(span, caller, direction, pointer(path, key), child, childSchema, now, out)
		}
	case []any:
		for i, child := range typed {
			walkSchema(span, caller, direction, pointer(path, strconv.Itoa(i)), child, schema.Items, now, out)
		}
	}
}

func typeMatches(expected string, v any) bool {
	if expected == "" {
		return true
	}
	switch expected {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "number":
		_, ok := v.(json.Number)
		return ok
	case "integer":
		n, ok := v.(json.Number)
		if !ok {
			return false
		}
		_, err := strconv.ParseInt(n.String(), 10, 64)
		return err == nil
	case "null":
		return v == nil
	}
	return true
}

func observedType(v any) string {
	switch v.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	case json.Number:
		return "number"
	case nil:
		return "null"
	}
	return fmt.Sprintf("%T", v)
}
func pointer(base, key string) string {
	key = strings.NewReplacer("~", "~0", "/", "~1").Replace(key)
	return base + "/" + key
}
func makeViolation(s fleet.Span, caller, direction, path, rule, expected, observed, severity string, now int64) Violation {
	return Violation{TraceID: s.TraceID, SpanID: s.SpanID, Caller: caller, Callee: s.Service, Route: s.Route, Direction: direction, Path: path, Rule: rule, Expected: expected, Observed: observed, Severity: severity, Timestamp: now}
}
