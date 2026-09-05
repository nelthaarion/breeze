package fleet

// Tests for the Span type's own logic.
//
// Span is mostly a data carrier, so there are only three behaviours worth
// testing — but each one is load-bearing in a way that makes a wrong answer
// expensive rather than merely wrong:
//
//   - Valid guards the ingestion path (§8.1.1). It is the last check before a
//     span enters the store, and the store groups spans by trace-id *string*.
//   - Failed drives root-cause marking (§9B.1), so missing a failure shape
//     mis-attributes an entire cascade to the wrong service.
//   - The JSON shape is the wire format every transport shares, so a changed tag
//     silently breaks cross-language ingest.

import (
	"strings"
	"testing"

	jsonx "github.com/goccy/go-json"
	"github.com/nelthaarion/breeze/v2/dashboard"
)

func validSpan() Span {
	return Span{
		TraceID:      "4bf92f3577b34da6a3ce929d0e0e4736",
		SpanID:       "00f067aa0ba902b7",
		ParentSpanID: "0123456789abcdef",
		Service:      "orders-service",
		Route:        "/orders/:id",
		Method:       "POST",
		Status:       200,
		StartNanoUTC: 1700000000000000000,
		DurationMs:   12.5,
	}
}

// --- Valid -----------------------------------------------------------------

func TestSpanValid(t *testing.T) {
	if !validSpan().Valid() {
		t.Error("a well-formed span was rejected")
	}
	// A root span has no parent, which is the normal edge-service case and
	// must not be mistaken for a missing field.
	s := validSpan()
	s.ParentSpanID = ""
	if !s.Valid() {
		t.Error("a root span (no parent) was rejected")
	}
}

// TestSpanValidRejects is the important half. Each case here, if accepted, does
// something worse than losing one span:
//
//   - uppercase splits one trace into two, because the store keys on the string;
//   - all-zero merges every span carrying it into one fictional trace;
//   - wrong length or non-hex means the id did not come from a real tracer, so
//     nothing else in the fleet will ever match it.
func TestSpanValidRejects(t *testing.T) {
	cases := map[string]func(*Span){
		"empty trace id":       func(s *Span) { s.TraceID = "" },
		"empty span id":        func(s *Span) { s.SpanID = "" },
		"short trace id":       func(s *Span) { s.TraceID = "4bf92f3577b34da6a3ce929d0e0e473" },
		"long trace id":        func(s *Span) { s.TraceID = "4bf92f3577b34da6a3ce929d0e0e47360" },
		"short span id":        func(s *Span) { s.SpanID = "00f067aa0ba902b" },
		"long span id":         func(s *Span) { s.SpanID = "00f067aa0ba902b70" },
		"uppercase trace id":   func(s *Span) { s.TraceID = "4BF92F3577B34DA6A3CE929D0E0E4736" },
		"uppercase span id":    func(s *Span) { s.SpanID = "00F067AA0BA902B7" },
		"non-hex trace id":     func(s *Span) { s.TraceID = "zzf92f3577b34da6a3ce929d0e0e4736" },
		"non-hex span id":      func(s *Span) { s.SpanID = "zzf067aa0ba902b7" },
		"zero trace id":        func(s *Span) { s.TraceID = strings.Repeat("0", 32) },
		"zero span id":         func(s *Span) { s.SpanID = strings.Repeat("0", 16) },
		"bad parent span id":   func(s *Span) { s.ParentSpanID = "nothex" },
		"zero parent span id":  func(s *Span) { s.ParentSpanID = strings.Repeat("0", 16) },
		"upper parent span id": func(s *Span) { s.ParentSpanID = "0123456789ABCDEF" },
	}
	for desc, mutate := range cases {
		t.Run(desc, func(t *testing.T) {
			s := validSpan()
			mutate(&s)
			if s.Valid() {
				t.Errorf("accepted a span with %s: trace=%q span=%q parent=%q",
					desc, s.TraceID, s.SpanID, s.ParentSpanID)
			}
		})
	}
}

// TestSpanValidIgnoresNonIdentityFields pins the deliberate narrowness of the
// check. A span with a blank route or a negative duration is odd but still
// useful, and rejecting it would throw away the record of a real request over a
// cosmetic problem. Only identity is worth a 400.
func TestSpanValidIgnoresNonIdentityFields(t *testing.T) {
	s := validSpan()
	s.Route = ""
	s.Method = ""
	s.Service = ""
	s.Status = 0
	s.DurationMs = -1
	s.StartNanoUTC = 0

	if !s.Valid() {
		t.Error("Valid rejected a span over non-identity fields; it should only check ids")
	}
}

func TestSpanZeroValueIsNotValid(t *testing.T) {
	var s Span
	if s.Valid() {
		t.Error("the zero span reports itself as storable")
	}
}

// --- Failed ----------------------------------------------------------------

// TestSpanFailed covers both failure shapes, because §9B.1's root-cause walk
// keys off this. The 200-with-error case is the one an implementation is likely
// to miss: a handler that caught an error, recovered, and still returned a body
// has failed in a way that matters to a trace, even though its status says
// otherwise.
func TestSpanFailed(t *testing.T) {
	cases := []struct {
		desc   string
		status int
		errMsg string
		want   bool
	}{
		{"200 no error", 200, "", false},
		{"404 is a client problem, not a failure", 404, "", false},
		{"499 is below the 5xx line", 499, "", false},
		{"500", 500, "", true},
		{"503", 503, "", true},
		{"500 with text", 500, "upstream timeout", true},
		{"200 with recorded error", 200, "recovered panic", true},
		{"302 with recorded error", 302, "redirect loop detected", true},
		{"status unset but error recorded", 0, "connection refused", true},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			s := validSpan()
			s.Status = c.status
			s.Error = c.errMsg
			if got := s.Failed(); got != c.want {
				t.Errorf("Failed() = %v, want %v (status=%d error=%q)",
					got, c.want, c.status, c.errMsg)
			}
		})
	}
}

func TestSpanIsRoot(t *testing.T) {
	s := validSpan()
	if s.IsRoot() {
		t.Error("a span with a parent reports itself as root")
	}
	s.ParentSpanID = ""
	if !s.IsRoot() {
		t.Error("a span with no parent does not report itself as root")
	}
}

// --- Wire format -----------------------------------------------------------

// TestSpanJSONTags pins the wire contract. §5A.5.2 requires one shared schema
// across every transport, and §5A.8 promises a non-Go service can POST spans by
// hand — both of which make these field names an API. Renaming one is a silent
// break for every cross-language reporter in a fleet, so it should fail here
// first.
func TestSpanJSONTags(t *testing.T) {
	raw, err := jsonx.Marshal(validSpan())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{
		`"trace_id"`, `"span_id"`, `"parent_span_id"`, `"service"`,
		`"route"`, `"method"`, `"status"`, `"start_ns"`, `"duration_ms"`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("marshalled span is missing %s\ngot: %s", want, raw)
		}
	}
}

// TestSpanJSONOmitsEmptyOptionals matters at fleet scale: spans are exported
// continuously, and emitting null timelines, empty tag objects, and blank parent
// ids on every unsampled span is bandwidth spent to say nothing.
func TestSpanJSONOmitsEmptyOptionals(t *testing.T) {
	s := validSpan()
	s.ParentSpanID = ""

	raw, err := jsonx.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, absent := range []string{
		"parent_span_id", "error", "timeline", "tags",
		"request_payload", "response_payload",
	} {
		if strings.Contains(string(raw), `"`+absent+`"`) {
			t.Errorf("empty field %q was serialized anyway\ngot: %s", absent, raw)
		}
	}
}

func TestSpanRoundTrip(t *testing.T) {
	in := validSpan()
	in.Error = "upstream timeout"
	in.Tags = map[string]string{"order_id": "123"}

	raw, err := jsonx.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Span
	if err := jsonx.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if out.TraceID != in.TraceID || out.SpanID != in.SpanID ||
		out.ParentSpanID != in.ParentSpanID || out.Service != in.Service ||
		out.Route != in.Route || out.Method != in.Method ||
		out.Status != in.Status || out.StartNanoUTC != in.StartNanoUTC ||
		out.DurationMs != in.DurationMs || out.Error != in.Error {
		t.Errorf("round trip changed the span:\n got %+v\nwant %+v", out, in)
	}
	if out.Tags["order_id"] != "123" {
		t.Errorf("tags did not survive the round trip: %v", out.Tags)
	}
	if !out.Valid() {
		t.Error("a span that survived a round trip is no longer valid")
	}
}

// TestSpanTimelineUsesDashboardType is the "one source of truth" requirement from
// §6.2 expressed as a compile-time check: the timeline field must be the
// dashboard's own step type, not a parallel struct that drifts from it.
func TestSpanTimelineUsesDashboardType(t *testing.T) {
	s := validSpan()
	// Nested children on purpose: §9.2's merged waterfall renders each
	// service's existing nested steps as sub-rows, so the nesting has to
	// survive export — a flattened timeline would still round-trip and still
	// be wrong.
	s.Timeline = []dashboard.TimelineStep{{
		ID:       "step-1",
		Name:     "handler",
		Duration: 4200, // microseconds, per the dashboard's own field
		Children: []dashboard.TimelineStep{{
			ID:       "step-2",
			Name:     "db.query",
			Duration: 1800,
		}},
	}}

	raw, err := jsonx.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Span
	if err := jsonx.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Timeline) != 1 {
		t.Fatalf("timeline has %d steps, want 1: %+v", len(out.Timeline), out.Timeline)
	}
	if out.Timeline[0].Name != "handler" || out.Timeline[0].Duration != 4200 {
		t.Errorf("step = %+v, want name=handler duration=4200", out.Timeline[0])
	}
	if len(out.Timeline[0].Children) != 1 || out.Timeline[0].Children[0].Name != "db.query" {
		t.Errorf("nested children did not survive export: %+v", out.Timeline[0].Children)
	}
}
