package binding

import (
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// fieldPlan describes how to validate a single struct field.
type fieldPlan struct {
	index int // field index in the struct
	kind  reflect.Kind
	rules []rule
}

// rule defines a single validation rule for a field.
type rule struct {
	name string
	arg  string
}

// planCache caches compiled field plans by type to avoid repeated reflection.
var planCache sync.Map

// compilePlan builds a validation plan for the given struct type.
// It parses the validate:"..." tags and caches the result.
func compilePlan(t reflect.Type) (map[string]*fieldPlan, error) {
	// Check cache first.
	if cached, ok := planCache.Load(t); ok {
		return cached.(map[string]*fieldPlan), nil
	}

	// Ensure we're dealing with a struct.
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("binding: type must be a struct, got %s", t.Kind())
	}

	plan := make(map[string]*fieldPlan)

	for i := 0; i < t.NumField(); i++ {
		fld := t.Field(i)
		if !fld.IsExported() {
			continue
		}

		validateTag := fld.Tag.Get("validate")
		if validateTag == "" {
			continue
		}

		// Parse validate tag: "required,min=5,max=100,email"
		rules := parseValidateTags(validateTag)

		plan[fld.Name] = &fieldPlan{
			index: i,
			kind:  fld.Type.Kind(),
			rules: rules,
		}
	}

	// Store in cache.
	planCache.Store(t, plan)
	return plan, nil
}

// parseValidateTags splits a comma-separated validate tag into rules.
func parseValidateTags(tag string) []rule {
	var rules []rule
	parts := strings.Split(tag, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		// Split "min=5" into "min" and "5".
		var ruleName, ruleArg string
		if idx := strings.IndexByte(part, '='); idx >= 0 {
			ruleName = part[:idx]
			ruleArg = part[idx+1:]
		} else {
			ruleName = part
		}

		rules = append(rules, rule{name: ruleName, arg: ruleArg})
	}
	return rules
}

// validator holds the validation state for a single Bind call.
type validator struct {
	plan   map[string]*fieldPlan
	val    reflect.Value
	errors []FieldError
}

// validate runs all rules in the plan against the struct value.
func (v *validator) validate() error {
	for fieldName, fp := range v.plan {
		if err := v.validateField(fieldName, fp); err != nil {
			return err
		}
	}

	if len(v.errors) > 0 {
		return &ValidationError{Errors: v.errors}
	}
	return nil
}

// validateField runs all rules for a single field.
func (v *validator) validateField(fieldName string, fp *fieldPlan) error {
	fld := v.val.Type().Field(fp.index)
	fldVal := v.val.Field(fp.index)

	// Handle pointer types: if the field is a pointer and is nil, skip validation
	// for most rules (nil is only invalid if "required" is present).
	isPtr := fldVal.Kind() == reflect.Ptr
	if isPtr && fldVal.IsNil() {
		// Check for required rule.
		for _, r := range fp.rules {
			if r.name == "required" {
				v.errors = append(v.errors, FieldError{
					Field:   fieldName,
					Rule:    "required",
					Message: fld.Name + " is required",
				})
			}
		}
		return nil
	}

	// Dereference pointer if needed.
	if isPtr {
		fldVal = fldVal.Elem()
	}

	// Check if this field is required.
	isRequired := false
	for _, r := range fp.rules {
		if r.name == "required" {
			isRequired = true
			break
		}
	}

	// If field is not required and has zero value, skip validation
	// (except for the required check itself).
	if !isRequired && !isFieldSet(fldVal) {
		return nil
	}

	for _, r := range fp.rules {
		if err := v.applyRule(fieldName, fld.Name, fldVal, r); err != nil {
			return err
		}
	}
	return nil
}

// applyRule applies a single validation rule to a field value.
func (v *validator) applyRule(fieldName, displayName string, fldVal reflect.Value, r rule) error {
	switch r.name {
	case "required":
		if !isFieldSet(fldVal) {
			v.errors = append(v.errors, FieldError{
				Field:   fieldName,
				Rule:    "required",
				Message: displayName + " is required",
			})
		}

	case "min":
		if !checkMin(fldVal, r.arg) {
			msg := fmt.Sprintf("%s must be at least %s", displayName, r.arg)
			v.errors = append(v.errors, FieldError{
				Field:   fieldName,
				Rule:    "min",
				Message: msg,
			})
		}

	case "max":
		if !checkMax(fldVal, r.arg) {
			msg := fmt.Sprintf("%s must be at most %s", displayName, r.arg)
			v.errors = append(v.errors, FieldError{
				Field:   fieldName,
				Rule:    "max",
				Message: msg,
			})
		}

	case "email":
		if !checkEmail(fldVal) {
			msg := displayName + " must be a valid email"
			v.errors = append(v.errors, FieldError{
				Field:   fieldName,
				Rule:    "email",
				Message: msg,
			})
		}

	case "oneof":
		if !checkOneof(fldVal, r.arg) {
			msg := fmt.Sprintf("%s must be one of %s", displayName, r.arg)
			v.errors = append(v.errors, FieldError{
				Field:   fieldName,
				Rule:    "oneof",
				Message: msg,
			})
		}
	}

	return nil
}

// isFieldSet checks if a field has a non-zero value.
func isFieldSet(val reflect.Value) bool {
	switch val.Kind() {
	case reflect.String:
		return val.String() != ""
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return val.Int() != 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return val.Uint() != 0
	case reflect.Float32, reflect.Float64:
		return val.Float() != 0
	case reflect.Bool:
		return val.Bool()
	default:
		return !val.IsZero()
	}
}

// checkMin validates a minimum constraint.
// For strings, it checks length; for numbers, it checks the value.
func checkMin(val reflect.Value, argStr string) bool {
	minVal, err := strconv.ParseFloat(argStr, 64)
	if err != nil {
		return true // invalid arg, skip check
	}

	switch val.Kind() {
	case reflect.String:
		minLen, _ := strconv.ParseInt(argStr, 10, 64)
		return int64(len(val.String())) >= minLen
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(val.Int()) >= minVal
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return float64(val.Uint()) >= minVal
	case reflect.Float32, reflect.Float64:
		return val.Float() >= minVal
	}
	return true
}

// checkMax validates a maximum constraint.
// For strings, it checks length; for numbers, it checks the value.
func checkMax(val reflect.Value, argStr string) bool {
	maxVal, err := strconv.ParseFloat(argStr, 64)
	if err != nil {
		return true // invalid arg, skip check
	}

	switch val.Kind() {
	case reflect.String:
		maxLen, _ := strconv.ParseInt(argStr, 10, 64)
		return int64(len(val.String())) <= maxLen
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(val.Int()) <= maxVal
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return float64(val.Uint()) <= maxVal
	case reflect.Float32, reflect.Float64:
		return val.Float() <= maxVal
	}
	return true
}

// checkEmail validates a simple email format.
// This is a pragmatic check; for production, consider a more robust approach.
var emailRegex = regexp.MustCompile(`^[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}$`)

func checkEmail(val reflect.Value) bool {
	if val.Kind() != reflect.String {
		return false
	}
	email := val.String()
	if email == "" {
		return true // empty is valid (if required, a separate rule catches it)
	}
	return emailRegex.MatchString(email)
}

// checkOneof validates that a value is one of the space-separated allowed values.
func checkOneof(val reflect.Value, argStr string) bool {
	allowed := strings.Fields(argStr)
	if len(allowed) == 0 {
		return true // no values specified, skip
	}

	valStr := ""
	switch val.Kind() {
	case reflect.String:
		valStr = val.String()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		valStr = strconv.FormatInt(val.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		valStr = strconv.FormatUint(val.Uint(), 10)
	case reflect.Float32, reflect.Float64:
		valStr = strconv.FormatFloat(val.Float(), 'f', -1, 64)
	default:
		return false
	}

	for _, v := range allowed {
		if v == valStr {
			return true
		}
	}
	return false
}
