package main

// This is deliberately an executable-boundary test rather than another MCP
// handler test. internal/mcp proves the vocabulary; this proves cmd/breeze-mcp
// actually composes that vocabulary with rpc.StdioServer and preserves framing.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nelthaarion/breeze/v2/internal/mcp"
	"github.com/nelthaarion/breeze/v2/internal/mcpcmd"
)

func TestRunHandshakeListAndNotification(t *testing.T) {
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	if err := mcpcmd.ServeStdio(version, mcp.ModeGenerator, mcp.UnscopedScope(),
		strings.NewReader(input), &out, nil); err != nil {
		t.Fatalf("ServeStdio: %v", err)
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
	// The exact inventory is asserted in internal/mcp, by a test that fails in
	// both directions when a tool is added or removed. Repeating the number here
	// would mean every new tool broke this test for a reason that has nothing to
	// do with what it covers, which is framing: that tools/list survives the
	// stdio transport as one well-formed message carrying real entries.
	if len(listed.Result.Tools) == 0 {
		t.Fatal("tools/list came back empty, so the registry did not survive the transport")
	}
	names := map[string]bool{}
	for _, tool := range listed.Result.Tools {
		if tool.Name == "" {
			t.Error("a listed tool has no name, so a client could not call it")
		}
		names[tool.Name] = true
	}
	// One representative tool, to prove the entries are the real registry rather
	// than placeholders that happen to be well-formed.
	if !names["breeze_new"] {
		t.Errorf("breeze_new is not in the listing; got %d tools", len(listed.Result.Tools))
	}
}

// ─── the no-flag case, unchanged ─────────────────────────────────────────────

// TestStdioIsTheDefaultTransportAndWritesNothingExtra is the regression test for the
// network transport's central promise: adding it changed nothing about the default.
//
// It goes through main1 — the full executable including flag parsing — rather than
// through the shared stdio helper, because the helper was already the stdio path and
// testing it would prove only that the function still exists. What could actually
// break is the dispatch in main1: a wrong default for --port, or a startup banner
// printed unconditionally. Both would be invisible to a test that called the helper
// directly and fatal to an editor reading this process's stdout.
//
// --mode is passed because Part 9 made it required; --port deliberately is not,
// because its absence is the thing under test.
func TestStdioIsTheDefaultTransportAndWritesNothingExtra(t *testing.T) {
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
	}, "\n") + "\n"

	var out, errOut bytes.Buffer
	if err := main1([]string{"--mode", "generator"}, strings.NewReader(input), &out, &errOut); err != nil {
		t.Fatalf("main1 with no --port: %v", err)
	}

	// Every line on stdout must be one JSON-RPC message. This is the assertion a
	// banner, a token, or a "listening on" line would fail.
	lines := bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("got %d lines on stdout, want 2:\n%s", len(lines), out.Bytes())
	}
	for i, line := range lines {
		var message map[string]any
		if err := json.Unmarshal(line, &message); err != nil {
			t.Fatalf("stdout line %d is not a JSON-RPC message: %q (%v)", i, line, err)
		}
		if message["jsonrpc"] != "2.0" {
			t.Errorf("stdout line %d jsonrpc = %v", i, message["jsonrpc"])
		}
	}

	// And stderr must be silent. A diagnostic here would be harmless to the
	// protocol but would appear in an editor's log on every launch, which is how a
	// "why is Breeze printing this" issue gets filed.
	if errOut.Len() != 0 {
		t.Errorf("stdio mode wrote to stderr: %q", errOut.String())
	}
}

// TestStdioDispatchMatchesTheSharedPathByteForByte compares main1's output with the
// shared stdio path's.
//
// The two are the same code path today: main1 parses flags and, for the no-port case,
// calls mcpcmd.ServeStdio. This is what fails if they ever stop being so — main1
// gaining a "print the mode" line, or the dispatch acquiring a wrapper that reorders
// or reframes anything.
func TestStdioDispatchMatchesTheSharedPathByteForByte(t *testing.T) {
	const input = `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}` + "\n"

	var viaMain, viaShared, errOut bytes.Buffer
	if err := main1([]string{"--mode", "generator"}, strings.NewReader(input), &viaMain, &errOut); err != nil {
		t.Fatalf("main1: %v", err)
	}
	if err := mcpcmd.ServeStdio(version, mcp.ModeGenerator, mcp.UnscopedScope(),
		strings.NewReader(input), &viaShared, nil); err != nil {
		t.Fatalf("ServeStdio: %v", err)
	}

	if viaMain.String() != viaShared.String() {
		t.Errorf("main1 and ServeStdio disagree\nmain1:      %q\nServeStdio: %q",
			viaMain.String(), viaShared.String())
	}
}

// TestModeIsRequiredByTheBinary is Part 9 at the executable's own boundary: no argv,
// no mode, no server. Checked through main1 rather than ParseFlags because a default
// reintroduced in the dispatch would not be caught by a parser test.
func TestModeIsRequiredByTheBinary(t *testing.T) {
	var out, errOut bytes.Buffer
	err := main1(nil, strings.NewReader(""), &out, &errOut)
	if err == nil {
		t.Fatal("breeze-mcp started with no --mode")
	}
	if !strings.Contains(err.Error(), "--mode is required") {
		t.Errorf("the refusal does not name --mode: %v", err)
	}
	// Nothing may reach stdout: a client reading this stream must not receive a
	// diagnostic where a JSON-RPC message belongs.
	if out.Len() != 0 {
		t.Errorf("the refusal wrote to stdout: %q", out.String())
	}
}

// ─── flags ───────────────────────────────────────────────────────────────────
//
// The flag surface itself is tested in internal/mcpcmd, where it lives: defaults,
// the token environment variable, rejections and the origin list are one
// implementation shared with `breeze start mcp-server`, so testing them here would
// be testing the same function under a second name.
//
// What remains here is the part specific to this executable: that -h is an
// exit-zero path through main1 and that its usage reaches stderr rather than the
// protocol stream.

// TestHelpIsNotAFailure keeps -h an exit-zero path. A usage message that exits
// non-zero shows up as a failed command in every wrapper script, and it must go to
// stderr so it cannot be mistaken for protocol output.
func TestHelpIsNotAFailure(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := main1([]string{"-h"}, strings.NewReader(""), &out, &errOut); err != nil {
		t.Fatalf("-h returned %v, want nil", err)
	}
	if out.Len() != 0 {
		t.Errorf("-h wrote to stdout: %q", out.String())
	}
	if !strings.Contains(errOut.String(), "--port") {
		t.Errorf("usage does not mention --port:\n%s", errOut.String())
	}
}
