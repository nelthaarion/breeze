package contracts

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/nelthaarion/breeze/v2/fleet"
	"github.com/nelthaarion/breeze/v2/scalar"
)

func testOperation(additional *bool) scalar.Operation {
	body := &scalar.Schema{Type: "object", Required: []string{"name", "price"}, AdditionalProperties: additional, Properties: map[string]*scalar.Schema{
		"name": {Type: "string"}, "price": {Type: "number"},
	}}
	return scalar.Operation{RequestBody: &scalar.RequestBody{Content: map[string]scalar.MediaType{"application/json": {Schema: body}}}, Responses: map[string]scalar.Response{"200": {Content: map[string]scalar.MediaType{"application/json": {Schema: body}}}}}
}

func testSpan() fleet.Span {
	return fleet.Span{TraceID: strings.Repeat("a", 32), SpanID: strings.Repeat("1", 16), Service: "orders", Route: "/orders", Method: "POST", Status: 200}
}

func TestValidateRequiredWrongTypeUnknownAndValid(t *testing.T) {
	deny := false
	tests := []struct {
		name, payload          string
		additional             *bool
		wantRule, wantSeverity string
		want                   int
	}{
		{"missing required", `{"name":"book"}`, nil, "required", "error", 1},
		{"wrong type", `{"name":"book","price":"nine"}`, nil, "type", "error", 1},
		{"unknown warning", `{"name":"book","price":9,"color":"blue"}`, nil, "additionalProperties", "warning", 1},
		{"unknown forbidden", `{"name":"book","price":9,"color":"blue"}`, &deny, "additionalProperties", "error", 1},
		{"valid", `{"name":"book","price":9}`, nil, "", "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := testSpan()
			s.RequestPayload = json.RawMessage(tt.payload)
			got := Validate(s, "gateway", testOperation(tt.additional), 10)
			if len(got) != tt.want {
				t.Fatalf("got %+v", got)
			}
			if tt.want > 0 && (got[0].Rule != tt.wantRule || got[0].Severity != tt.wantSeverity) {
				t.Fatalf("got %+v", got[0])
			}
		})
	}
}
