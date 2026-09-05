package generator

import (
	"strings"
	"testing"
)

func TestParseFieldsValid(t *testing.T) {
	fields, err := parseFields([]string{"name:string", "age:int", "signedUpAt:time.Time"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []field{
		{Name: "Name", JSON: "name", Type: "string"},
		{Name: "Age", JSON: "age", Type: "int"},
		{Name: "SignedUpAt", JSON: "signedUpAt", Type: "time.Time"},
	}
	if len(fields) != len(want) {
		t.Fatalf("got %d fields, want %d", len(fields), len(want))
	}
	for i := range want {
		if fields[i] != want[i] {
			t.Errorf("field %d = %+v, want %+v", i, fields[i], want[i])
		}
	}
}

func TestParseFieldsErrors(t *testing.T) {
	cases := []string{
		"noColon",
		"1invalid:string",
		"name:unsupportedtype",
		"name:string:extra",
		":string",
		"name:",
	}
	for _, c := range cases {
		if _, err := parseFields([]string{c}); err == nil {
			t.Errorf("parseFields(%q) expected error, got nil", c)
		}
	}
}

func TestParseFieldsDuplicate(t *testing.T) {
	if _, err := parseFields([]string{"name:string", "Name:int"}); err == nil {
		t.Error("expected error for duplicate field, got nil")
	}
}

// TestParseFieldsNoRulesRejectsRules covers the generators whose structs are
// never bound from a request. Accepting the segment and dropping it is the
// failure this whole path exists to avoid: the user would have typed a rule and
// been given a struct that does not enforce it.
func TestParseFieldsNoRulesRejectsRules(t *testing.T) {
	if _, err := parseFieldsNoRules(
		"model",
		[]string{"name:string", "email:string:required,email"},
	); err == nil {
		t.Error("parseFieldsNoRules accepted a rules segment")
	} else if !strings.Contains(
		err.Error(),
		"generate resource",
	) {
		t.Errorf("error should point at the generator that does validate, got: %v", err)
	}

	// Without rules it is the ordinary parser, and no tag is inferred either â€”
	// inference is generateResource's step, not the parser's.
	fields, err := parseFieldsNoRules("event", []string{"email:string", "age:int"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, f := range fields {
		if f.Validate != "" {
			t.Errorf("field %q got validate tag %q from the parser", f.JSON, f.Validate)
		}
	}
}
