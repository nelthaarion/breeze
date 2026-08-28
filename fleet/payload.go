package fleet

import (
	"bytes"
	"encoding/json"
	"strings"
)

const MaxCapturedPayloadBytes = 64 << 10

var sensitivePayloadFields = map[string]struct{}{
	"authorization": {}, "cookie": {}, "password": {}, "passwd": {},
	"secret": {}, "ssn": {}, "token": {}, "access_token": {},
	"refresh_token": {}, "api_key": {}, "apikey": {}, "credit_card": {},
}

// CaptureJSONPayload returns a bounded, source-redacted JSON body. Invalid,
// non-JSON, empty, or oversized bodies are deliberately omitted: a contract
// payload must never turn tracing into an unbounded data-export channel.
func CaptureJSONPayload(body []byte) json.RawMessage {
	if len(body) == 0 || len(body) > MaxCapturedPayloadBytes || !json.Valid(body) {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil
	}
	redactPayload(value)
	out, err := json.Marshal(value)
	if err != nil || len(out) > MaxCapturedPayloadBytes {
		return nil
	}
	return out
}

func redactPayload(value any) {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			if _, sensitive := sensitivePayloadFields[strings.ToLower(key)]; sensitive {
				v[key] = "••••••"
				continue
			}
			redactPayload(child)
		}
	case []any:
		for _, child := range v {
			redactPayload(child)
		}
	}
}
