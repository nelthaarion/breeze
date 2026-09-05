package mcp

// testhelp_test.go — assertions shared by the tool tests.
//
// The one that needs explaining is looseEqual. Configuration values reach the
// tests after a YAML round-trip (a ProjectConfig is marshalled so the keys come
// out as the yaml tags the schema and the flags both use, then unmarshalled into
// a map), and that round-trip does not preserve Go's numeric types: a port
// declared as an int can come back as an int, a uint64, or a float64 depending on
// which decoder saw it, and a sample rate written as 0.25 comes back as a
// float64 even though 1 would come back as an int. Comparing with == would make
// these tests assert the decoder's typing choices rather than the values, so
// numbers are compared as numbers.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// mustJSON encodes tool arguments the way the JSON-RPC layer would deliver them.
// The tool tests invoke run directly rather than going through tools/call, so
// this is the only place the arguments become bytes.
func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()

	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encoding tool arguments: %v", err)
	}
	return b
}

// changedPathsOf names the files in a plan, for failure messages. It is the
// planFile counterpart of changedPaths, which reports fileChange values.
func changedPathsOf(files []planFile) []string {
	paths := make([]string, 0, len(files))
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	return paths
}

// assertConfig walks a dotted path into a decoded configuration and compares the
// value it finds. A missing path fails with the path that ran out, rather than
// with a nil comparison, because "fleet.sample_rate is nil" and "fleet is nil"
// are different bugs.
func assertConfig(t *testing.T, cfg map[string]any, path string, want any) {
	t.Helper()

	got, err := lookupPath(cfg, path)
	if err != nil {
		t.Errorf("%s: %v", path, err)
		return
	}
	if !looseEqual(got, want) {
		t.Errorf("%s = %#v (%T), want %#v (%T)", path, got, got, want, want)
	}
}

// lookupPath resolves a dotted path through nested maps.
func lookupPath(cfg map[string]any, path string) (any, error) {
	keys := strings.Split(path, ".")
	var current any = cfg

	for i, key := range keys {
		node, ok := current.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s is %T, not an object", strings.Join(keys[:i], "."), current)
		}
		value, ok := node[key]
		if !ok {
			return nil, fmt.Errorf("%s is absent", strings.Join(keys[:i+1], "."))
		}
		current = value
	}
	return current, nil
}

// hasChange reports whether a diff contains a specific transition. Both ends are
// compared, because a report that a field changed is only useful if it says what
// it changed from.
func hasChange(changes []configChange, path string, from, to any) bool {
	for _, c := range changes {
		if c.Path == path && looseEqual(c.From, from) && looseEqual(c.To, to) {
			return true
		}
	}
	return false
}

// looseEqual compares two decoded values, treating all numeric types as numbers.
func looseEqual(got, want any) bool {
	if gotNum, ok := asFloat(got); ok {
		if wantNum, ok := asFloat(want); ok {
			return gotNum == wantNum
		}
		return false
	}
	return got == want
}

// asFloat widens any of the numeric types a YAML or JSON decoder can produce.
func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint8:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	case float32:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

// replaceFirst edits generated source, failing the test if the text it is asked
// to replace is not there. A silent no-op would turn a test about edited blocks
// into a test that passes because nothing was edited.
func replaceFirst(t *testing.T, source, old, replacement string) string {
	t.Helper()

	if !strings.Contains(source, old) {
		t.Fatalf("the generated source does not contain %q, so the fixture edit would be a no-op:\n%s", old, source)
	}
	return strings.Replace(source, old, replacement, 1)
}
