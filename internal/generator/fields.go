package generator

import (
	"fmt"
	"go/token"
	"sort"
	"strings"
)

// field is a single name:type[:rules] triple parsed from `breeze generate`
// arguments, e.g. "email:string" or "email:string:required,email".
type field struct {
	Name string // Go-exported field name, e.g. "Email"
	JSON string // json tag, e.g. "email"
	Type string // Go type, e.g. "string"

	// Validate is the contents of the validate:"â€¦" struct tag, empty when the
	// field carries no rules. binding.Bind reads that tag, so this is the whole
	// mechanism by which a generated request struct gets validated.
	Validate string
}

var supportedFieldTypes = map[string]bool{
	"string":        true,
	"int":           true,
	"int64":         true,
	"uint":          true,
	"uint64":        true,
	"float32":       true,
	"float64":       true,
	"bool":          true,
	"[]string":      true,
	"time.Time":     true,
	"time.Duration": true,
}

func supportedFieldTypeList() string {
	types := make([]string, 0, len(supportedFieldTypes))
	for t := range supportedFieldTypes {
		types = append(types, t)
	}
	sort.Strings(types)
	return strings.Join(types, ", ")
}

// parseFields parses a list of "name:type[:rules]" arguments into fields,
// validating that each name is a legal Go identifier, each type is supported,
// and each validation rule is one the binding package actually implements.
//
// The rules segment is optional and lands in the validate:"â€¦" tag verbatim:
//
//	breeze generate resource User name:string:required,min=2 email:string:required,email
//
// It is checked here rather than passed through because binding's validator
// ignores a rule name it does not recognise (see applyRule's switch, which has
// no default), so a typo like "requird" would silently validate nothing at all.
func parseFields(args []string) ([]field, error) {
	fields := make([]field, 0, len(args))
	seen := make(map[string]bool, len(args))

	for _, arg := range args {
		// Three-way split: the type may itself contain no colon, so anything
		// after the second is the rules segment.
		parts := strings.SplitN(arg, ":", 3)
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("invalid field %q â€” expected name:type[:rules] (e.g. email:string or email:string:required,email)", arg)
		}
		name, typ := parts[0], parts[1]
		rules := ""
		if len(parts) == 3 {
			rules = parts[2]
		}

		if !token.IsIdentifier(name) {
			return nil, fmt.Errorf("invalid field name %q â€” must be a valid Go identifier", name)
		}
		if !supportedFieldTypes[typ] {
			return nil, fmt.Errorf("unsupported field type %q for %q â€” supported types: %s", typ, name, supportedFieldTypeList())
		}

		validate, err := parseValidateSpec(rules, name, typ)
		if err != nil {
			return nil, err
		}

		exported := exportedFieldName(name)
		if seen[exported] {
			return nil, fmt.Errorf("duplicate field %q", name)
		}
		seen[exported] = true

		fields = append(fields, field{Name: exported, JSON: name, Type: typ, Validate: validate})
	}

	return fields, nil
}

// parseFieldsNoRules parses fields for a generator whose struct is never bound
// from a request â€” a model is filled by Scan, an event is constructed in
// process â€” and so rejects a rules segment instead of accepting one it would
// then drop.
//
// The alternative was to emit the validate tag anyway. That is worse: on a model
// the tag is live only if the user binds a request straight into it, and a model
// carries ID and the timestamp columns, so the one path that would make the tag
// mean anything is also mass assignment.
func parseFieldsNoRules(kind string, args []string) ([]field, error) {
	for _, arg := range args {
		// The supported types contain no colon, so a second one is a rules
		// segment. Caught before parseFields so the message names the real
		// problem rather than reporting an unknown rule.
		if strings.Count(arg, ":") >= 2 {
			return nil, fmt.Errorf("field %q carries validation rules, which `generate %s` cannot use â€” only `generate resource` binds a request through binding.Bind, which is what reads a validate tag", arg, kind)
		}
	}
	return parseFields(args)
}

// validateRulesNeedingArg / validateRulesBare are the rules binding implements,
// split by whether they take an "=arg". Anything outside these two sets is a
// typo, and binding would ignore it silently.
var (
	validateRulesBare       = map[string]bool{"required": true, "email": true}
	validateRulesNeedingArg = map[string]bool{"min": true, "max": true, "oneof": true}
)

// parseValidateSpec checks a comma-separated rules segment and returns it
// normalized for the struct tag.
func parseValidateSpec(spec, fieldName, typ string) (string, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return "", nil
	}

	out := make([]string, 0, 4)
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		ruleName, arg := part, ""
		if i := strings.IndexByte(part, '='); i >= 0 {
			ruleName, arg = part[:i], part[i+1:]
		}

		switch {
		case validateRulesBare[ruleName]:
			if arg != "" {
				return "", fmt.Errorf("rule %q on field %q takes no argument", ruleName, fieldName)
			}
		case validateRulesNeedingArg[ruleName]:
			if strings.TrimSpace(arg) == "" {
				return "", fmt.Errorf("rule %q on field %q needs an argument, as in %s=â€¦", ruleName, fieldName, ruleName)
			}
		default:
			return "", fmt.Errorf("unknown validation rule %q on field %q â€” supported rules: email, max=N, min=N, oneof=a b c, required", ruleName, fieldName)
		}

		// email compares a string against a regexp; on any other kind
		// checkEmail returns false, so every request would be rejected.
		if ruleName == "email" && typ != "string" {
			return "", fmt.Errorf("rule \"email\" on field %q applies to string, not %s", fieldName, typ)
		}

		out = append(out, part)
	}

	return strings.Join(out, ","), nil
}

// withValidationDefaults fills in a validate tag for fields that were given
// none, so that a plain `generate resource User name:string email:string`
// produces a handler that actually rejects an empty body.
//
// Only strings get a default. "required" means non-zero to binding's validator,
// so defaulting it on a number would reject age=0 and on a bool would reject
// false â€” a generated bug rather than a generated convenience. Numeric and bool
// fields are therefore left open for the user to constrain explicitly.
func withValidationDefaults(fields []field) []field {
	out := make([]field, len(fields))
	copy(out, fields)
	for i := range out {
		if out[i].Validate != "" || out[i].Type != "string" {
			continue
		}
		if looksLikeEmail(out[i]) {
			out[i].Validate = "required,email"
			continue
		}
		out[i].Validate = "required"
	}
	return out
}

// looksLikeEmail reports whether a field is an email address by name, so the
// common case gets format checking without being asked for.
func looksLikeEmail(f field) bool {
	name := strings.ToLower(f.JSON)
	return name == "email" || strings.HasSuffix(name, "_email") || strings.HasSuffix(name, "email")
}

// usesValidation reports whether any field carries rules, which decides whether
// generated code needs its 422 branch at all.
func usesValidation(fields []field) bool {
	for _, f := range fields {
		if f.Validate != "" {
			return true
		}
	}
	return false
}

// exportedFieldName turns a field argument into a Go-exported name.
// "email" becomes Email and "placed_at" becomes PlacedAt â€” a snake_case
// argument is idiomatic to type on the command line but produces a field name
// that go vet's own style checks flag, so the underscores are folded here. A
// name that is already camelCase is only capitalized.
func exportedFieldName(name string) string {
	if !strings.Contains(name, "_") {
		return strings.ToUpper(name[:1]) + name[1:]
	}

	var buf strings.Builder
	for _, part := range strings.Split(name, "_") {
		if part == "" {
			continue
		}
		buf.WriteString(strings.ToUpper(part[:1]))
		buf.WriteString(part[1:])
	}
	return buf.String()
}

// usesTime reports whether any field requires the time package to be
// imported by generated code.
func usesTime(fields []field) bool {
	for _, f := range fields {
		if strings.HasPrefix(f.Type, "time.") {
			return true
		}
	}
	return false
}

// columnName is the snake_case SQL column for a field. The JSON name is the
// input rather than the exported name so that a field given as "placed_at"
// round-trips to the same column instead of being re-split from PlacedAt.
func columnName(f field) string {
	if strings.Contains(f.JSON, "_") {
		return strings.ToLower(f.JSON)
	}
	return toSlug(f.Name)
}

// sqlTypes maps Go field types onto portable SQL column types. TEXT for slices
// because a portable array type does not exist â€” Postgres has TEXT[], SQLite
// does not â€” and picking one would make the generated migration non-portable
// for the sake of a column the user is expected to edit anyway.
var sqlTypes = map[string]string{
	"string":        "TEXT",
	"int":           "INTEGER",
	"int64":         "BIGINT",
	"uint":          "INTEGER",
	"uint64":        "BIGINT",
	"float32":       "REAL",
	"float64":       "DOUBLE PRECISION",
	"bool":          "BOOLEAN",
	"[]string":      "TEXT",
	"time.Time":     "TIMESTAMP",
	"time.Duration": "BIGINT",
}

func sqlType(f field) string {
	if t, ok := sqlTypes[f.Type]; ok {
		return t
	}
	return "TEXT"
}

// zeroLiteral is a printable zero value for a field type, used by generated
// example code so the stub compiles before the user fills it in.
func zeroLiteral(f field) string {
	switch f.Type {
	case "string":
		return `""`
	case "bool":
		return "false"
	case "[]string":
		return "nil"
	case "time.Time":
		return "time.Time{}"
	case "time.Duration":
		return "0"
	default:
		return "0"
	}
}
