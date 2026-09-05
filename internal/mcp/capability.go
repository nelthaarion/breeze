package mcp

// capability.go — what a token is allowed to do (Part 8).
//
// # Why scope is a second layer, not the only one
//
// mode.go already decides what is *registered*: an app-runtime server has no
// generating or provisioning tool at all. That is structural and cannot be
// misconfigured. This file is the layer above it: given the tools a server does
// have, which of them may this particular token reach.
//
// The two answer different questions and neither replaces the other. Mode is a
// property of the deployment ("this process must never rewrite its own source").
// Scope is a property of a credential ("this token was minted for the CI job that
// only needs to read traces"). A token scoped to {fleet} on a generator server still
// cannot provision; an unscoped token on an app-runtime server still cannot generate,
// because there is nothing there to reach.
//
// # Why a category rather than a per-tool list
//
// A token minted with 39 tool names would be stale the moment a tool is added, and
// whoever minted it would have to know the whole inventory. Categories are stable and
// small enough to reason about: an operator granting "fleet" does not have to be told
// that breeze_explain_incident exists.
//
// # Why the default is everything
//
// An unscoped token keeps today's behaviour exactly. Scoping is a hardening step an
// operator takes deliberately; making it mandatory would break every existing
// configuration to protect a loopback default that is already the trust boundary.
// What is *not* silent is the risky combination — an unscoped token on a non-loopback
// bind prints a warning, the same way a widened bind already does.

import (
	"fmt"
	"sort"
	"strings"
)

// Capability is one category of tool a token may be granted.
type Capability string

const (
	// CapGeneration creates and modifies project files.
	CapGeneration Capability = "generation"

	// CapIntrospection answers questions about what exists, from a project tree or
	// from constants compiled in. Reads only.
	CapIntrospection Capability = "introspection"

	// CapPlanning previews changes and holds change sets open. It writes only to
	// sandbox copies until a change set is committed.
	CapPlanning Capability = "planning"

	// CapKnowledge maintains and searches llms.txt and suggests next steps.
	CapKnowledge Capability = "knowledge"

	// CapVerification runs the Go toolchain — build, vet, test, benchmarks — against a
	// project. It executes project code, which is why it is separable.
	CapVerification Capability = "verification"

	// CapRuntime reads live state from a running service over HTTP.
	CapRuntime Capability = "runtime"

	// CapFleet reads a Fleet Aggregator: traces, topology, contract violations.
	CapFleet Capability = "fleet"

	// CapProvisioning drives Docker: builds images, starts and removes containers.
	// The most consequential category, and the one most worth withholding.
	CapProvisioning Capability = "provisioning"
)

// KnownCapabilities lists every category, sorted.
//
// Reported in the initialize payload alongside the granted set, so an agent learns
// what it was *not* given as well as what it was — a list of granted categories with
// nothing to compare it against cannot tell an agent whether a tool is missing
// because it does not exist or because the token was narrowed.
func KnownCapabilities() []Capability {
	all := []Capability{
		CapGeneration, CapIntrospection, CapPlanning, CapKnowledge,
		CapVerification, CapRuntime, CapFleet, CapProvisioning,
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	return all
}

// ParseCapability converts a configuration or flag value to a Capability.
func ParseCapability(raw string) (Capability, error) {
	candidate := Capability(strings.ToLower(strings.TrimSpace(raw)))
	for _, known := range KnownCapabilities() {
		if candidate == known {
			return known, nil
		}
	}
	return "", fmt.Errorf("mcp: unknown capability %q; valid values are %s",
		raw, strings.Join(capabilityNames(KnownCapabilities()), ", "))
}

// capabilityNames renders capabilities as strings, for a message or a JSON payload.
func capabilityNames(caps []Capability) []string {
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		out = append(out, string(c))
	}
	return out
}
