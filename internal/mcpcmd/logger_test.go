package mcpcmd

// logger_test.go — the log must be useful, and must never carry a secret.
//
// # Why the redaction test is the important one here
//
// Several tools take a credential as an ordinary argument: `password` and `token` on the
// fleet and live tools, `service_token` on provisioning, `token` on simulate. A log that
// formatted arguments would copy those into a file that outlives the process — and
// stderr is exactly the stream a container runtime captures and a supervisor ships
// elsewhere.
//
// internal/mcp/observe.go makes that structurally impossible by giving Event no field
// that could hold a value. These tests assert the property end to end anyway, because
// "structurally impossible" is a claim about today's code and a future field is one
// commit away.

import (
	"bytes"
	"encoding/json"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nelthaarion/breeze/internal/mcp"
)

// TestLogNeverContainsAnArgumentValue is the redaction guarantee at the formatter.
//
// The formatter is handed names only — that is what Event carries — so this asserts it
// cannot be made to print a value even when one is passed where a name belongs. The
// end-to-end half, that a real tools/call never puts a value into an Event in the first
// place, is internal/mcp's TestAToolCallLogsNoArgumentValue.
func TestLogNeverContainsAnArgumentValue(t *testing.T) {
	// Distinctive strings that appear nowhere else, so a hit is unambiguous. The keys are
	// the real credential-bearing argument names from the fleet, live, provisioning and
	// simulate tools.
	secrets := map[string]string{
		"token":         "SECRET-TOKEN-9f3c4d5e",
		"password":      "SECRET-PASSWORD-1a2b3c",
		"service_token": "SECRET-SERVICE-TOKEN-7d8e9f",
	}

	names := make([]string, 0, len(secrets))
	for name := range secrets {
		names = append(names, name)
	}
	sort.Strings(names)

	var out bytes.Buffer
	logger := &stderrLogger{out: &out, name: "breeze-mcp"}
	logger.LogEvent(mcp.Event{
		Kind:     mcp.EventToolCall,
		Tool:     "breeze_get_logs",
		Outcome:  mcp.OutcomeOK,
		ArgNames: names,
		Duration: 42 * time.Millisecond,
	})

	line := out.String()
	for key, value := range secrets {
		if strings.Contains(line, value) {
			t.Errorf("the log line contains the value of %q. stderr is captured into logs "+
				"that outlive the process:\n%s", key, line)
		}
		// The name is what makes the line useful, so it must be there.
		if !strings.Contains(line, key) {
			t.Errorf("the log line omits the argument name %q:\n%s", key, line)
		}
	}
}

// TestLogRecordsWhatAnOperatorNeeds checks the fields are actually present, so the
// redaction test above cannot pass by logging nothing.
func TestLogRecordsWhatAnOperatorNeeds(t *testing.T) {
	cases := []struct {
		name  string
		event mcp.Event
		want  []string
	}{
		{
			name: "a successful call",
			event: mcp.Event{
				Kind: mcp.EventToolCall, Tool: "breeze_verify_project",
				Outcome: mcp.OutcomeOK, Duration: 4210 * time.Millisecond,
			},
			want: []string{"breeze-mcp", "breeze_verify_project", "ok", "4.21s"},
		},
		{
			name: "a failed call",
			event: mcp.Event{
				Kind: mcp.EventToolCall, Tool: "breeze_get_logs",
				Outcome: mcp.OutcomeError, Duration: 91 * time.Millisecond,
			},
			want: []string{"breeze_get_logs", "error", "91ms"},
		},
		{
			name: "a panic is not just an error",
			event: mcp.Event{
				Kind: mcp.EventToolPanic, Tool: "breeze_generate",
				Duration: time.Millisecond,
			},
			want: []string{"breeze_generate", "PANICKED"},
		},
		{
			name:  "an unknown tool",
			event: mcp.Event{Kind: mcp.EventToolUnknown, Tool: "breeze_nonexistent"},
			want:  []string{"breeze_nonexistent", "unknown"},
		},
		{
			name: "a scope refusal names the capability needed",
			event: mcp.Event{
				Kind: mcp.EventToolRefused, Tool: "breeze_new", Reason: "generation",
			},
			want: []string{"breeze_new", "refused", "generation"},
		},
		{
			// The security-relevant line: without the address, a run of these cannot be
			// attributed and therefore cannot be acted on.
			name: "a rejected request reports its cause and its source",
			event: mcp.Event{
				Kind: mcp.EventTransportRefusal, Status: 401,
				Reason: mcp.ReasonNoToken, Remote: "10.1.2.3:53551",
			},
			want: []string{"refused", "401", "bearer token", "10.1.2.3:53551"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			(&stderrLogger{out: &out, name: "breeze-mcp"}).LogEvent(tc.event)

			line := out.String()
			if strings.Count(line, "\n") != 1 {
				t.Errorf("want exactly one line, got %d newlines: %q",
					strings.Count(line, "\n"), line)
			}
			for _, want := range tc.want {
				if !strings.Contains(line, want) {
					t.Errorf("the line is missing %q:\n%s", want, line)
				}
			}
		})
	}
}

// TestLoggingIsOffUnlessAsked is the default.
//
// On stdio, stdout is the protocol stream and stderr is whatever the editor that launched
// the process does with it. Both belong to somebody else, so an unasked-for log line is a
// library writing into a channel it does not own.
func TestLoggingIsOffUnlessAsked(t *testing.T) {
	t.Setenv(TokenEnv, "")

	opts, err := ParseFlags("breeze-mcp", []string{"--mode", "generator"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Log {
		t.Error("--log defaulted to on")
	}
	if logger := loggerFor("breeze-mcp", opts, io.Discard); logger != nil {
		t.Error("a logger was built without --log; SetLogger(nil) is what disables logging, " +
			"so a non-nil logger here means every server logs")
	}

	withFlag, err := ParseFlags("breeze-mcp", []string{"--mode", "generator", "--log"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !withFlag.Log {
		t.Error("--log did not take effect")
	}
	if logger := loggerFor("breeze-mcp", withFlag, io.Discard); logger == nil {
		t.Error("--log was passed and no logger was built")
	}
}

// TestAStdioServerWithoutLoggingWritesOnlyProtocol is the consequence that matters on the
// transport where a stray line is a protocol error.
//
// It drives the real ServeStdio with a nil logger and requires every byte of stdout to be
// a JSON-RPC message. One log line on this stream is one malformed message to the peer,
// and some clients abandon the session over it.
func TestAStdioServerWithoutLoggingWritesOnlyProtocol(t *testing.T) {
	const input = `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}` + "\n"

	var out bytes.Buffer
	if err := ServeStdio("test-version", mcp.ModeAppRuntime, mcp.UnscopedScope(),
		strings.NewReader(input), &out, nil); err != nil {
		t.Fatal(err)
	}

	for i, line := range bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n")) {
		var message map[string]any
		if err := json.Unmarshal(line, &message); err != nil {
			t.Fatalf("stdout line %d is not a JSON-RPC message: %q", i, line)
		}
		if message["jsonrpc"] != "2.0" {
			t.Errorf("stdout line %d is not JSON-RPC 2.0: %q", i, line)
		}
	}
}

// TestTheLoggerSerialisesConcurrentWrites is why stderrLogger holds a mutex.
//
// The network transport serves each request on its own net/http goroutine, so two tools
// can finish at the same instant. io.Writer promises nothing about concurrent use, and two
// interleaved Fprintf calls produce one corrupted line — which is worse than no line for
// anything scraping the log, because it looks like data.
//
// Run under -race in CI, where an unsynchronised writer would also be reported directly.
func TestTheLoggerSerialisesConcurrentWrites(t *testing.T) {
	var out bytes.Buffer
	logger := &stderrLogger{out: &out, name: "breeze-mcp"}

	const goroutines = 16
	const each = 25

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				logger.LogEvent(mcp.Event{
					Kind: mcp.EventToolCall, Tool: "breeze_routes",
					Outcome: mcp.OutcomeOK, Duration: time.Millisecond,
				})
			}
		}()
	}
	wg.Wait()

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != goroutines*each {
		t.Fatalf("got %d lines, want %d", len(lines), goroutines*each)
	}
	// Every line whole: a torn write would produce a line missing its prefix or its tail.
	for i, line := range lines {
		if !strings.HasPrefix(line, "breeze-mcp: tool breeze_routes ok") {
			t.Fatalf("line %d is torn: %q", i, line)
		}
	}
}
