package mcpcmd

// usage_secret_test.go — the control token must not reach stderr through the flag set,
// and the log must stay one line per event whatever a caller sends.

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/nelthaarion/breeze/v2/internal/mcp"
)

const envTokenForTest = "env-token-that-must-never-be-printed-0123456789abcdef"

func TestUsageDoesNotPrintTheTokenFromTheEnvironment(t *testing.T) {
	// flag.PrintDefaults renders "(default <value>)" for any non-empty default. When the
	// flag's default was $BREEZE_MCP_TOKEN, -h — or any mistyped flag, which prints usage
	// too — wrote the live control token to stderr.
	t.Setenv(TokenEnv, envTokenForTest)

	for name, args := range map[string][]string{
		"help":            {"-h"},
		"unknown-flag":    {"--no-such-flag"},
		"bad-port":        {"--port", "notanumber"},
		"help-with-other": {"--mode", "generator", "--help"},
	} {
		var errOut bytes.Buffer
		_, err := ParseFlags("breeze-mcp", args, &errOut)
		if err == nil {
			t.Errorf("%s: expected an error or help, got none", name)
		}
		if strings.Contains(errOut.String(), envTokenForTest) {
			t.Errorf("%s: usage output contains the token from $%s:\n%s", name, TokenEnv, errOut.String())
		}
	}

	var errOut bytes.Buffer
	if _, err := ParseFlags("breeze-mcp", []string{"-h"}, &errOut); !errors.Is(err, ErrHelp) {
		t.Fatalf("-h returned %v, want ErrHelp", err)
	}
	if !strings.Contains(errOut.String(), "-token") {
		t.Error("the usage no longer documents --token")
	}
}

func TestTokenStillComesFromTheEnvironmentAndTheFlagWins(t *testing.T) {
	t.Setenv(TokenEnv, envTokenForTest)

	opts, err := ParseFlags("breeze-mcp", []string{"--mode", "generator"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Token != envTokenForTest {
		t.Errorf("the token from $%s was not applied: got %q", TokenEnv, opts.Token)
	}

	opts, err = ParseFlags("breeze-mcp", []string{"--mode", "generator", "--token", "flag-token-wins-0123456789"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Token != "flag-token-wins-0123456789" {
		t.Errorf("an explicit --token did not override the environment: got %q", opts.Token)
	}

	t.Setenv(TokenEnv, "")
	opts, err = ParseFlags("breeze-mcp", []string{"--mode", "generator"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Token != "" {
		t.Errorf("with nothing set the token should be empty so one is generated, got %q", opts.Token)
	}
}

func TestLogLinesCannotBeForged(t *testing.T) {
	// A caller holding the token chooses the name of an unknown tool and every argument
	// name. Verbatim, a newline in one of them writes a second, fabricated log line.
	var buf bytes.Buffer
	l := &stderrLogger{out: &buf, name: "breeze-mcp"}

	forged := "x\nbreeze-mcp: refused 401 missing or invalid bearer token from 10.0.0.9:1"
	l.LogEvent(mcp.Event{Kind: mcp.EventToolUnknown, Tool: forged})
	l.LogEvent(mcp.Event{Kind: mcp.EventToolCall, Tool: "breeze_new", Outcome: "ok",
		ArgNames: []string{"name", "a\r\nforged", "\x1b[2J\x1b[Hscreen-wipe"}})
	l.LogEvent(mcp.Event{Kind: mcp.EventToolRefused, Tool: "t\nu", Reason: "generation"})
	l.LogEvent(mcp.Event{Kind: mcp.EventTransportRefusal, Status: 401, Reason: "missing token", Remote: "10.1.2.3:5\n6"})

	out := buf.String()
	if got := strings.Count(out, "\n"); got != 4 {
		t.Fatalf("4 events produced %d lines; a caller-controlled field broke a line:\n%q", got, out)
	}
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if !strings.HasPrefix(line, "breeze-mcp: ") {
			t.Errorf("a line does not start with the command prefix — it was forged: %q", line)
		}
	}
	if strings.ContainsAny(out, "\r\x1b") {
		t.Errorf("a control character reached the log: %q", out)
	}
	// The forged text is still there — the operator should see what was sent — but inside
	// quotes, on the line of the event that carried it, never at the start of a line.
	first := strings.SplitN(out, "\n", 2)[0]
	if !strings.HasPrefix(first, `breeze-mcp: tool "x\n`) {
		t.Errorf("the unknown-tool line does not quote what the caller sent: %q", first)
	}
}

func TestLogLeavesOrdinaryNamesUntouched(t *testing.T) {
	var buf bytes.Buffer
	l := &stderrLogger{out: &buf, name: "breeze-mcp"}
	l.LogEvent(mcp.Event{Kind: mcp.EventToolCall, Tool: "breeze_verify_project", Outcome: "ok",
		ArgNames: []string{"path", "run_tests", "service_url"}})
	l.LogEvent(mcp.Event{Kind: mcp.EventTransportRefusal, Status: 401, Reason: "missing or invalid bearer token",
		Remote: "[::1]:53551"})

	out := buf.String()
	for _, want := range []string{
		"tool breeze_verify_project ok",
		"args=path,run_tests,service_url",
		"from [::1]:53551",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("an ordinary field was altered; %q missing from:\n%s", want, out)
		}
	}
}

func TestLogBoundsWhatACallerCanMakeALineHold(t *testing.T) {
	var buf bytes.Buffer
	l := &stderrLogger{out: &buf, name: "breeze-mcp"}

	names := make([]string, 5000)
	for i := range names {
		names[i] = strings.Repeat("a", 500)
	}
	l.LogEvent(mcp.Event{Kind: mcp.EventToolCall, Tool: strings.Repeat("t", 100_000), Outcome: "ok", ArgNames: names})

	if n := buf.Len(); n > 8<<10 {
		t.Errorf("one event wrote %d bytes; a caller can flood a log line", n)
	}
	if !strings.Contains(buf.String(), "_more") {
		t.Error("truncation of the argument list is not indicated")
	}
}
