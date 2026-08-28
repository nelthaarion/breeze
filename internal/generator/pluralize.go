package generator

import "strings"

// pluralize applies simple English pluralization rules. It intentionally
// does not handle irregular plurals (e.g. "person" -> "people"); callers
// needing that should pass an explicit override.
func pluralize(name string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, "y") && !endsInVowelY(lower):
		return name[:len(name)-1] + "ies"
	case strings.HasSuffix(lower, "s"),
		strings.HasSuffix(lower, "x"),
		strings.HasSuffix(lower, "ch"),
		strings.HasSuffix(lower, "sh"):
		return name + "es"
	default:
		return name + "s"
	}
}

func endsInVowelY(lower string) bool {
	if len(lower) < 2 {
		return false
	}
	switch lower[len(lower)-2] {
	case 'a', 'e', 'i', 'o', 'u':
		return true
	}
	return false
}
