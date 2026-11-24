package dashboard

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// alwaysMasked are the names whose values are masked whatever Config.MaskedHeaders says.
//
// The list is a floor, not a default: selectHeaders always captures authorization and cookie,
// and logging always sees whatever the application prints, so an operator who replaced
// MaskedHeaders with a list of their own (or an empty one is replaced by the default, but a
// one-entry list is not) used to end up storing bearer tokens and session cookies in the
// requests table and sending them to every open dashboard. Config adds to this list; it cannot
// remove from it.
var alwaysMasked = []string{
	"authorization", "proxy-authorization", "cookie", "set-cookie",
	"x-api-key", "x-auth-token", "api-key", "apikey", "api_key",
	"password", "passwd", "secret", "token", "private-key", "private_key",
}

// maskedValue is what a masked value is replaced with.
const maskedValue = "••••••"

// maskHeaders returns a copy of headers with sensitive values replaced by
// "••••••". The comparison is case-insensitive against alwaysMasked and
// cfg.MaskedHeaders.
//
// Used by the dashboard middleware before persisting a RequestRecord so
// secrets never enter the rolling buffer.
func maskHeaders(cfg Config, headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	masked := make(map[string]string, len(headers))
	sensitive := make(map[string]struct{}, len(cfg.MaskedHeaders)+len(alwaysMasked))
	for _, h := range alwaysMasked {
		sensitive[h] = struct{}{}
	}
	for _, h := range cfg.MaskedHeaders {
		sensitive[strings.ToLower(h)] = struct{}{}
	}
	for k, v := range headers {
		if _, ok := sensitive[strings.ToLower(k)]; ok {
			masked[k] = maskedValue
		} else {
			masked[k] = v
		}
	}
	return masked
}

// maskKeys is the set of sensitive keys, lower-cased, without repeats or empties: alwaysMasked
// plus the configured ones. A collector computes it once: maskLine used to build it again, as a
// map, for every line it was given.
func maskKeys(cfg Config) []string {
	keys := make([]string, 0, len(cfg.MaskedHeaders)+len(alwaysMasked))
	seen := make(map[string]struct{}, len(cfg.MaskedHeaders)+len(alwaysMasked))
	add := func(h string) {
		k := strings.ToLower(h)
		if _, dup := seen[k]; dup || k == "" {
			return
		}
		seen[k] = struct{}{}
		keys = append(keys, k)
	}
	for _, h := range alwaysMasked {
		add(h)
	}
	for _, h := range cfg.MaskedHeaders {
		add(h)
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
	return strings.Join(maskFields(keys, strings.Fields(s)), " ")
}

// maskFields masks the secrets among the whitespace-separated fields of a line, in place.
func maskFields(keys []string, parts []string) []string {
	for i := 0; i < len(parts); i++ {
		masked, key, value, ok := maskToken(keys, parts[i])
		if !ok {
			continue
		}
		parts[i] = masked
		// "Authorization: Bearer eyJ…" has its key in one field and its value in the next two.
		// Masking only the key's own field left the credential in the clear, in the one line
		// shape that most often carries one. The field after the key is the scheme or the
		// credential itself; a scheme word is kept and the field after it is masked.
		if key == "authorization" || key == "proxy-authorization" {
			j := i + 1
			if value == "" && j < len(parts) && isAuthScheme(parts[j]) {
				j++
			}
			if (value == "" || isAuthScheme(value)) && j < len(parts) {
				parts[j] = maskedValue
				i = j
			}
		}
	}
	return parts
}

// isAuthScheme reports whether a field is the scheme word of an Authorization value.
func isAuthScheme(s string) bool {
	switch strings.ToLower(s) {
	case "bearer", "basic", "digest", "negotiate", "token", "apikey":
		return true
	}
	return false
}

// maskToken masks the first secret in one field. It returns the masked field, the key that
// matched, and the value that was in the field (without its quotes or closing punctuation).
func maskToken(keys []string, token string) (masked, key, value string, ok bool) {
	src := token
	if !isASCII(token) {
		// Lower-casing can turn a non-ASCII letter into an ASCII one (the Kelvin sign is a k),
		// so a token that is not plain ASCII is matched, and rebuilt, in its lower-cased form.
		src = strings.ToLower(token)
	}
	vs, sep, key, found := findSecret(keys, src)
	if !found {
		return token, "", "", false
	}
	head := src[:vs]
	if strings.HasPrefix(src[vs:], `"`) {
		head += `"` // a JSON value keeps its opening quote
	}
	value = strings.Trim(src[vs:], `",}]`)
	return head + maskedValue + trailingPunct(src, sep), key, value, true
}

// findSecret finds the first key=value or key:value pair in a field whose key is sensitive, and
// returns the index just past its separator.
//
// A key is sensitive when the identifier before the separator is one of the keys, or ends in one
// after a '_', '-' or '.': token, access_token, x-api-key, db.password. It used to have to be the
// field's first word, so access_token=…, refresh_token=…, client_secret=… and ?token=… went through
// unmasked while token=… did not. An identifier may be followed by a closing quote (the JSON form).
func findSecret(keys []string, token string) (valueStart int, sep byte, key string, ok bool) {
	for i := 0; i < len(token); i++ {
		c := token[i]
		if c != '=' && c != ':' {
			continue
		}
		end := i
		if end > 0 && token[end-1] == '"' {
			end--
		}
		begin := end
		for begin > 0 && isIdentByte(token[begin-1]) {
			begin--
		}
		ident := token[begin:end]
		if ident == "" {
			continue
		}
		for _, k := range keys {
			if identEndsWithKey(ident, k) {
				return i + 1, c, k, true
			}
		}
	}
	return 0, 0, "", false
}

func isIdentByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_' || b == '-' || b == '.'
}

// identEndsWithKey reports whether ident is key, or ends in key after a '_', '-' or '.'.
func identEndsWithKey(ident, key string) bool {
	n := len(key)
	if n == 0 || len(ident) < n || !strings.EqualFold(ident[len(ident)-n:], key) {
		return false
	}
	if len(ident) == n {
		return true
	}
	switch ident[len(ident)-n-1] {
	case '_', '-', '.':
		return true
	}
	return false
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
// token with one is checked here only when it is ASCII, with the same rule findSecret applies; a
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

// tokenMayBeMasked is false only when maskToken would find no key in the token.
func tokenMayBeMasked(keys []string, token string) bool {
	if !strings.ContainsAny(token, "=:") {
		return false
	}
	if !isASCII(token) {
		return true
	}
	_, _, _, ok := findSecret(keys, token)
	return ok
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
