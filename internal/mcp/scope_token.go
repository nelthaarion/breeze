package mcp

// scope_token.go — the set of capabilities one token carries.

import (
	"fmt"
	"sort"
	"strings"
)

// Scope is the set of capabilities a token was minted with.
//
// The zero value means unscoped: every capability, which is the documented default and
// today's behaviour. That is why this is a struct rather than a bare slice — a nil
// slice has to mean *something*, and "nothing is granted" would turn an omitted field
// into a server no client can use, while "everything is granted" hidden inside a nil
// check is the kind of implicit rule this codebase avoids stating twice.
type Scope struct {
	// granted is nil for an unscoped token, and non-nil for a scoped one. It is never
	// non-nil and empty: NewScope refuses an empty capability list, so a scoped token
	// always grants at least one thing.
	granted map[Capability]bool

	// scoped records that a scope was set at all, so an unscoped token is
	// distinguishable from one deliberately minted with every capability — a
	// distinction an operator auditing a deployment cares about.
	scoped bool
}

// UnscopedScope is a token with every capability. Named so a call site reads as a
// decision rather than as a zero value someone forgot to fill in.
func UnscopedScope() Scope { return Scope{} }

// NewScope builds a scope granting exactly the given capabilities.
//
// Duplicates are harmless. An empty list is refused at construction rather than
// accepted: it would mint a credential that authenticates and then rejects every call,
// which is indistinguishable from a broken server. It is also the shape a caller gets
// when a dynamically built list came back empty, and the two possible silent readings —
// "grant nothing" and "grant everything" — are both worse than an error.
func NewScope(caps ...Capability) (Scope, error) {
	if len(caps) == 0 {
		return Scope{}, fmt.Errorf("mcp: a scoped token needs at least one capability; " +
			"omit the scope entirely for an unrestricted token")
	}

	granted := make(map[Capability]bool, len(caps))
	for _, c := range caps {
		if _, err := ParseCapability(string(c)); err != nil {
			return Scope{}, err
		}
		granted[c] = true
	}
	return Scope{granted: granted, scoped: true}, nil
}

// ParseScope builds a scope from a comma-separated list, as a flag supplies it.
//
// An empty or whitespace-only string is an unscoped token, matching the flag's default:
// not passing --scope and passing --scope="" are the same intent.
func ParseScope(raw string) (Scope, error) {
	var caps []Capability
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		c, err := ParseCapability(part)
		if err != nil {
			return Scope{}, err
		}
		caps = append(caps, c)
	}
	if len(caps) == 0 {
		return UnscopedScope(), nil
	}
	return NewScope(caps...)
}

// IsScoped reports whether this token was narrowed at all.
func (s Scope) IsScoped() bool { return s.scoped }

// Allows reports whether this scope grants a capability.
func (s Scope) Allows(c Capability) bool {
	if !s.scoped {
		return true
	}
	return s.granted[c]
}

// AllowsTool reports whether this scope permits calling a named tool.
//
// An unclassified tool is refused for a scoped token and permitted for an unscoped
// one. That asymmetry is deliberate: an unscoped token already has everything, so
// refusing it would be a regression for a tool that merely has not been categorised
// yet, while granting it to a narrow token would silently widen that token.
func (s Scope) AllowsTool(name string) bool {
	if !s.scoped {
		return true
	}
	c, classified := capabilityOf(name)
	if !classified {
		return false
	}
	return s.granted[c]
}

// Granted returns the capabilities in force, sorted.
//
// For an unscoped token this is every known capability rather than an empty list,
// because that is what the token can actually do — and the initialize payload has to
// report what is true, not what was typed.
func (s Scope) Granted() []Capability {
	if !s.scoped {
		return KnownCapabilities()
	}
	out := make([]Capability, 0, len(s.granted))
	for c := range s.granted {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
