package fleet

import (
	"strings"
	"testing"
)

func TestCaptureJSONPayloadRedactsNestedSensitiveFields(t *testing.T) {
	got := string(CaptureJSONPayload([]byte(`{"user":"n","nested":{"token":"abc"},"items":[{"password":"pw"}]}`)))
	if strings.Contains(got, "abc") || strings.Contains(got, "pw") || !strings.Contains(got, "••••••") {
		t.Fatalf("payload was not redacted at source: %s", got)
	}
}

func TestCaptureJSONPayloadRejectsInvalidAndOversized(t *testing.T) {
	if got := CaptureJSONPayload([]byte(`{`)); got != nil {
		t.Fatalf("invalid JSON captured: %s", got)
	}
	if got := CaptureJSONPayload(make([]byte, MaxCapturedPayloadBytes+1)); got != nil {
		t.Fatal("oversized payload captured")
	}
}
