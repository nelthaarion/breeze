package dashboard

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// maskHeaders returns a copy of headers with sensitive values replaced by
// "••••••". The comparison is case-insensitive against cfg.MaskedHeaders.
//
// Used by the dashboard middleware before persisting a RequestRecord so
// secrets never enter the rolling buffer.
func maskHeaders(cfg Config, headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	masked := make(map[string]string, len(headers))
	sensitive := make(map[string]struct{}, len(cfg.MaskedHeaders))
	for _, h := range cfg.MaskedHeaders {
		sensitive[strings.ToLower(h)] = struct{}{}
	}
	for k, v := range headers {
		if _, ok := sensitive[strings.ToLower(k)]; ok {
			masked[k] = "••••••"
		} else {
			masked[k] = v
		}
	}
	return masked
}

// maskKeys is the set of sensitive keys, lower-cased, without repeats or empties. A collector
// computes it once: maskLine used to build it again, as a map, for every line it was given.
func maskKeys(cfg Config) []string {
	keys := make([]string, 0, len(cfg.MaskedHeaders))
	seen := make(map[string]struct{}, len(cfg.MaskedHeaders))
	for _, h := range cfg.MaskedHeaders {
		k := strings.ToLower(h)
		if _, dup := seen[k]; dup || k == "" {
			continue
		}
		seen[k] = struct{}{}
		keys = append(keys, k)
	}
	return keys
}

// maskLine scans s for tokens that look like secrets (key=value, key:value,
// "key":"value") and replaces their value with "••••••". Used by the log
// recorder so application logs that accidentally print tokens are still
// surfaced but redacted.
func maskLine(cfg Config, s string) string { return maskLineWith(maskKeys(cfg), s) }

// maskLineWith is maskLine for keys that maskKeys already produced.
//
// Almost every line a service logs has nothing to mask, and masking one used to cost a map, a
// split into fields, a lower-cased copy of every token and a join: about three microseconds and
// eight allocations, on every request line the logging block feeds the dashboard. A line that is
// already what the join would produce, and in which no token can be a secret, is returned as it
// is. Anything the check is not sure of goes through the same code as before.
func maskLineWith(keys []string, s string) string {
	if s == "" {
		return s
	}
	if nothingToMask(keys, s) {
		return s
	}
	sensitive := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		sensitive[k] = struct{}{}
	}
	parts := strings.Fields(s)
	for i, p := range parts {
		key, sep, ok := sensitiveKeyIn(p, sensitive)
		if !ok {
			continue
		}
		head, _ := splitAfterValueStart(p, key, sep)
		parts[i] = head + "••••••" + trailingPunct(p, sep)
	}
	return strings.Join(parts, " ")
}

// sensitiveKeyIn finds a sensitive key at the start of the token (ignoring
// leading JSON punctuation like `{`, `[`, `,`, or a leading quote) and
// reports the separator that followed it ('=' or ':').
func sensitiveKeyIn(token string, sensitive map[string]struct{}) (key string, sep byte, ok bool) {
	lower := strings.ToLower(token)
	start := strings.IndexFunc(lower, func(r rune) bool {
		return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-'
	})
	if start < 0 {
		return "", 0, false
	}
	rest := lower[start:]
	for k := range sensitive {
		if !strings.HasPrefix(rest, k) {
			continue
		}
		after := rest[len(k):]
		after = strings.TrimPrefix(after, `"`)
		if strings.HasPrefix(after, "=") {
			return k, '=', true
		}
		if strings.HasPrefix(after, ":") {
			return k, ':', true
		}
	}
	return "", 0, false
}

// splitAfterValueStart returns the token content up through the separator
// (and an opening quote for JSON-style values), so the caller can splice in
// the redacted placeholder.
func splitAfterValueStart(token, key string, sep byte) (head, tail string) {
	lower := strings.ToLower(token)
	idx := strings.Index(lower, key)
	head = token[:idx+len(key)]
	rest := token[idx+len(key):]
	if strings.HasPrefix(rest, `"`) {
		head += `"`
		rest = rest[1:]
	}
	head += string(sep)
	rest = rest[1:]
	if strings.HasPrefix(rest, `"`) {
		head += `"`
		rest = rest[1:]
	}
	return head, rest
}

// trailingPunct returns any trailing quote/brace/comma characters from the
// original token so JSON structure around the redacted value is preserved.
func trailingPunct(token string, sep byte) string {
	if sep != ':' {
		return ""
	}
	i := len(token)
	for i > 0 {
		c := token[i-1]
		if c == '"' || c == '}' || c == ']' || c == ',' {
			i--
			continue
		}
		break
	}
	return token[i:]
}

// nothingToMask reports whether maskLineWith would return s unchanged, and says no when it is not sure.
//
// Two things have to hold. The line must already be what strings.Join(strings.Fields(s), " ") makes of
// it, because that normalises whitespace even when nothing is masked: no leading or trailing space,
// exactly one ASCII space between tokens, and no other space character. And no token may be one the
// full path would mask. Masking needs a '=' or a ':' in the token, so a token with neither is clear. A
// token with one is checked here only when it is ASCII, with the same rule sensitiveKeyIn applies; a
// non-ASCII token that has one is left to the full path, since lower-casing can turn a non-ASCII letter
// into an ASCII one (the Kelvin sign is a k) and the full path would then find a key this check cannot.
func nothingToMask(keys []string, s string) bool {
	for _, k := range keys {
		if !isASCII(k) {
			return false
		}
	}
	prevSpace := true // a leading space is not normalised
	start := 0
	for i := 0; i <= len(s); i++ {
		if i < len(s) {
			c := s[i]
			if c < utf8.RuneSelf {
				if c != ' ' && !isASCIISpace(c) {
					prevSpace = false
					continue
				}
				if c != ' ' || prevSpace {
					return false // a tab, a newline, a second space, or a leading one
				}
			} else {
				r, size := utf8.DecodeRuneInString(s[i:])
				if !unicode.IsSpace(r) {
					prevSpace = false
					i += size - 1
					continue
				}
				return false // a non-ASCII space is replaced by the join
			}
		}
		// i is at a space or at the end: s[start:i] is a token.
		if i == len(s) && prevSpace {
			return false // a trailing space (or the empty string)
		}
		if tokenMayBeMasked(keys, s[start:i]) {
			return false
		}
		prevSpace = true
		start = i + 1
	}
	return true
}

// tokenMayBeMasked is false only when sensitiveKeyIn would find no key in the token.
func tokenMayBeMasked(keys []string, token string) bool {
	if !strings.ContainsAny(token, "=:") {
		return false
	}
	if !isASCII(token) {
		return true
	}
	begin := strings.IndexFunc(token, func(r rune) bool {
		return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-'
	})
	if begin < 0 {
		return false
	}
	rest := token[begin:]
	for _, k := range keys {
		if len(rest) < len(k) || !strings.EqualFold(rest[:len(k)], k) {
			continue
		}
		after := strings.TrimPrefix(rest[len(k):], `"`)
		if strings.HasPrefix(after, "=") || strings.HasPrefix(after, ":") {
			return true
		}
	}
	return false
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// isASCIISpace is unicode.IsSpace for the ASCII range.
func isASCIISpace(c byte) bool {
	return c == '\t' || c == '\n' || c == '\v' || c == '\f' || c == '\r' || c == ' '
}
