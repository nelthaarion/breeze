package main

// This is deliberately an executable-boundary test rather than another MCP
// handler test. internal/mcp proves the vocabulary; this proves cmd/breeze-mcp
// actually composes that vocabulary with rpc.StdioServer and preserves framing.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestRunHandshakeListAndNotification(t *testing.T) {
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	if err := run(strings.NewReader(input), &out); err != nil {
		t.Fatalf("run: %v", err)
	}

	lines := bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("got %d response lines, want 2 (the notification must be silent):\n%s", len(lines), out.Bytes())
	}

	for i, line := range lines {
		var message map[string]any
		if err := json.Unmarshal(line, &message); err != nil {
			t.Fatalf("line %d is not one JSON-RPC message: %q (%v)", i, line, err)
		}
		if message["jsonrpc"] != "2.0" {
			t.Errorf("line %d jsonrpc = %v", i, message["jsonrpc"])
		}
		if _, hasError := message["error"]; hasError {
			t.Errorf("line %d is an error: %s", i, line)
		}
	}

	var initialized struct {
		Result struct {
			ServerInfo struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal(lines[0], &initialized); err != nil {
		t.Fatal(err)
	}
	if initialized.Result.ServerInfo.Name != "breeze" || initialized.Result.ServerInfo.Version != version {
		t.Errorf("serverInfo = %+v", initialized.Result.ServerInfo)
	}

	var listed struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(lines[1], &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Result.Tools) != 5 {
		t.Fatalf("tools/list returned %d tools, want 5", len(listed.Result.Tools))
	}
}
