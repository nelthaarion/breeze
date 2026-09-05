package mcp

// autosync_test.go — the proof that a plain field addition never requires an edit
// under internal/mcp/.
//
// This is the concrete form of a claim made repeatedly about the schema tooling:
// describe_schema reflects over ProjectConfig, so a field added to that struct
// appears in the MCP schema on its own. A claim like that is worthless unless
// something fails when it stops being true, and nothing else in the suite would
// notice — the parity test next door compares two walks of the same struct
// against each other, so both would be equally wrong about a field neither saw.
//
// So this test names the two fields added to ModelConfig and asserts they reach
// an agent, through the tool rather than through the generator function the tool
// calls. Going through tools/call is the part that matters: the generator half
// could be right while the tool returned a cached or hand-written document, and
// only a real call rules that out.
//
// If this file is the only thing that changed when a field was added, the
// property holds.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/nelthaarion/breeze/v2/internal/generator"
)

// autoSyncFields are the fields whose presence is asserted end to end.
//
// They are written as literal strings on purpose. Deriving them from
// ModelConfig's tags would make the test pass by construction: it would assert
// that reflection sees what reflection sees, which is true of a field the schema
// walk skips as well.
var autoSyncFields = []struct {
	yamlKey  string
	flagPath string
}{
	{"filename", "models.<name>.filename"},
	{"package", "models.<name>.package"},
}

// TestNewConfigFieldsReachTheGeneratorSchemaWithoutMCPChanges is the generator
// half: ConfigJSONSchema must carry both fields, as properties and as flags.
func TestNewConfigFieldsReachTheGeneratorSchemaWithoutMCPChanges(t *testing.T) {
	doc, err := generator.ConfigJSONSchema("models")
	if err != nil {
		t.Fatalf("ConfigJSONSchema(\"models\"): %v", err)
	}

	flags, err := generator.SchemaFlagPaths(doc)
	if err != nil {
		t.Fatalf("reading flag paths out of the schema: %v", err)
	}
	inSchema := map[string]bool{}
	for _, f := range flags {
		inSchema[f] = true
	}

	for _, field := range autoSyncFields {
		if !inSchema[field.flagPath] {
			t.Errorf("the models schema does not carry --%s under x-flag; an agent reading the "+
				"schema cannot discover the field.\nflags found: %v", field.flagPath, flags)
		}
		// The property itself, not only its flag: a caller writing breeze.yaml
		// reads the properties, and a flag path with no property behind it would
		// describe a field YAML cannot set.
		if !schemaMentionsProperty(t, doc, field.yamlKey) {
			t.Errorf(
				"the models schema has no %q property, so it cannot be set from a config file",
				field.yamlKey,
			)
		}
	}
}

// TestNewConfigFieldsReachTheMCPSchemaWithoutMCPChanges is the half that makes
// the claim about internal/mcp/ rather than about the generator.
//
// The call goes through tools/call, the same path a client takes, so what is
// asserted is what an agent would actually receive.
func TestNewConfigFieldsReachTheMCPSchemaWithoutMCPChanges(t *testing.T) {
	srv := NewServer("test")

	for _, section := range []string{"models", ""} {
		args := map[string]any{}
		if section != "" {
			args["section"] = section
		}
		label := section
		if label == "" {
			label = "whole schema"
		}

		result := callTool(t, srv, "breeze_describe_schema", args)
		if result.IsError {
			t.Fatalf("breeze_describe_schema(%s) failed: %s", label, result.Content[0].Text)
		}

		// The text the tool renders is what a client without structured-content
		// support sees, and the document is what one with it parses. Both have to
		// mention the fields, so neither kind of client is left in the dark.
		rendered := result.Content[0].Text
		flags := schemaFlagsOf(t, result)

		for _, field := range autoSyncFields {
			if !containsString(flags, field.flagPath) {
				t.Errorf(
					"breeze_describe_schema(%s) does not advertise --%s, so adding a field to "+
						"ModelConfig did not reach MCP on its own",
					label,
					field.flagPath,
				)
			}
			if !strings.Contains(rendered, field.yamlKey) {
				t.Errorf("breeze_describe_schema(%s) does not mention %q in its rendered text",
					label, field.yamlKey)
			}
		}
	}
}

// schemaFlagsOf pulls the schema out of a describe_schema result and returns the
// flag paths it carries.
//
// The structured content arrives decoded rather than as the original type,
// because callTool goes through the JSON-RPC layer — which is the point of using
// it here, so the round trip is part of what the test covers.
func schemaFlagsOf(t *testing.T, result toolCallResult) []string {
	t.Helper()

	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("re-encoding the structured content: %v", err)
	}
	var decoded struct {
		Schema json.RawMessage `json:"schema"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decoding the structured content: %v", err)
	}
	if len(decoded.Schema) == 0 {
		t.Fatalf("the result carried no schema document: %s", raw)
	}

	flags, err := generator.SchemaFlagPaths(decoded.Schema)
	if err != nil {
		t.Fatalf("reading flag paths out of the returned schema: %v", err)
	}
	return flags
}

// schemaMentionsProperty reports whether the document declares a property under
// the given yaml key, at any depth.
func schemaMentionsProperty(t *testing.T, doc json.RawMessage, key string) bool {
	t.Helper()

	var node any
	if err := json.Unmarshal(doc, &node); err != nil {
		t.Fatalf("decoding the schema: %v", err)
	}
	return hasProperty(node, key)
}

func hasProperty(node any, key string) bool {
	switch n := node.(type) {
	case map[string]any:
		if props, ok := n["properties"].(map[string]any); ok {
			if _, ok := props[key]; ok {
				return true
			}
		}
		for _, child := range n {
			if hasProperty(child, key) {
				return true
			}
		}
	case []any:
		for _, child := range n {
			if hasProperty(child, key) {
				return true
			}
		}
	}
	return false
}
