package fleet

import (
	"bytes"
	"encoding/json"
	"strings"
)

const MaxCapturedPayloadBytes = 64 << 10

// sensitiveKeyExact are normalised key names that are too short or too common
// as substrings to match safely ("pin" would hit "shipping").
var sensitiveKeyExact = map[string]struct{}{
	"ssn": {}, "cvv": {}, "cvc": {}, "pin": {}, "otp": {}, "auth": {},
}

// sensitiveKeyParts are fragments that mark a key as secret wherever they appear
// in the normalised name, so accessToken, new_password, X-Api-Key, clientSecret
// and idToken are all caught — not just the dozen spellings an exact-match list
// happens to enumerate.
var sensitiveKeyParts = []string{
	"password", "passwd", "passphrase", "secret", "token", "apikey",
	"authorization", "cookie", "creditcard", "cardnumber", "privatekey",
	"credential", "sessionid", "signature",
}

// isSensitiveKey normalises a JSON key (lower-case, separators removed) and
// reports whether its value must not leave the process.
func isSensitiveKey(key string) bool {
	var buf [64]byte
	n := buf[:0]
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c >= 'A' && c <= 'Z':
			n = append(n, c+('a'-'A'))
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c >= 0x80:
			n = append(n, c)
		}
	}
	norm := string(n)
	if _, ok := sensitiveKeyExact[norm]; ok {
		return true
	}
	for _, part := range sensitiveKeyParts {
		if strings.Contains(norm, part) {
			return true
		}
	}
	return false
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
			if isSensitiveKey(key) {
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
