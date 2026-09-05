package mcp

// observe_test.go — the events a running server emits, and what they must not carry.
//
// # The claim under test
//
// observe.go asserts that no Event can hold a secret, on the grounds that no field has a
// shape that would hold one. That is a structural argument, and structural arguments are
// worth testing precisely because they are about today's code: a future field, or a future
// call site that puts a value where a name belongs, would break it silently.
//
// So these drive real dispatch — a real tools/call, a real refused request — and inspect
// what a logger actually received.

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorder is a Logger that keeps what it was given.
//
// Mutex-guarded because the network transport hands events over from a net/http goroutine
// per request, and a test that raced here would fail under -race for its own reasons
// rather than the code's.
type recorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *recorder) LogEvent(ev Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *recorder) all() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Event, len(r.events))
	copy(out, r.events)
	return out
}

// firstOfKind returns the first event of a kind, or reports that none arrived.
func (r *recorder) firstOfKind(t *testing.T, kind EventKind) Event {
	t.Helper()
	for _, ev := range r.all() {
		if ev.Kind == kind {
			return ev
		}
	}
	t.Fatalf("no %s event was recorded; got %+v", kind, r.all())
	return Event{}
}

// TestAToolCallLogsNoArgumentValue is the guarantee that matters.
//
// A real tools/call, with arguments named exactly as the credential-bearing ones on the
// fleet, live, provisioning and simulate tools, and values that appear nowhere else in the
// process. Nothing in the recorded event may contain any of them — not in ArgNames, not in
// Reason, not anywhere.
//
// breeze_features is used because it is read-only, needs no network and no filesystem, and
// ignores unknown arguments, so the call completes and the assertion is about logging
// rather than about the tool.
func TestAToolCallLogsNoArgumentValue(t *testing.T) {
	secrets := map[string]string{
		"token":         "SECRET-TOKEN-3f9a2b1c",
		"password":      "SECRET-PASSWORD-8e7d6c",
		"service_token": "SECRET-SERVICE-4b5a6f",
		"control_token": "SECRET-CONTROL-2d3e4f",
	}

	rec := &recorder{}
	srv := NewServer("test")
	srv.SetLogger(rec)

	args, err := json.Marshal(secrets)
	if err != nil {
		t.Fatal(err)
	}
	call, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "breeze_features", "arguments": json.RawMessage(args)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out := srv.RPCServer().Handle(call); len(out) == 0 {
		t.Fatal("tools/call produced no response")
	}

	ev := rec.firstOfKind(t, EventToolCall)

	// The whole event, rendered, must not contain a value. Rendered rather than
	// field-by-field so a field added later is covered without this test being updated —
	// which is the failure mode the structural argument in observe.go is exposed to.
	rendered, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range secrets {
		if strings.Contains(string(rendered), value) {
			t.Errorf("the event carries the value of %q:\n%s", name, rendered)
		}
	}

	// And the names are there, because a log that records nothing is not the answer.
	for name := range secrets {
		if !slices.Contains(ev.ArgNames, name) {
			t.Errorf("ArgNames omits %q: %v", name, ev.ArgNames)
		}
	}
	if ev.Tool != "breeze_features" {
		t.Errorf("Tool = %q, want breeze_features", ev.Tool)
	}
	if ev.Outcome != OutcomeOK {
		t.Errorf("Outcome = %q, want %q (body was a successful call)", ev.Outcome, OutcomeOK)
	}
	// Not asserted as > 0. breeze_features reads an in-memory table, and Windows'
	// monotonic clock granularity is coarse enough that a sub-millisecond call genuinely
	// measures zero — an assertion that it did not would fail on the fast machine and pass
	// on the slow one. What matters is that the field is set from a measurement rather than
	// left as garbage, so the bound is the useful one: non-negative, and not absurd.
	if ev.Duration < 0 || ev.Duration > time.Minute {
		t.Errorf("Duration = %v, which is not a measurement of this call", ev.Duration)
	}
}

// TestARejectedRequestIsRecordedWithItsSource is the security-relevant half.
//
// A wrong token never reaches a tool, so dispatch cannot see it. Without a transport-level
// event, a token-guessing run against the control port leaves no trace at all — and this is
// a port whose tools write files and start containers.
//
// The address must be there: a series of 401s nobody can attribute is a series nobody can
// act on. The presented token must not be, because a log is not the place to record what
// somebody guessed.
func TestARejectedRequestIsRecordedWithItsSource(t *testing.T) {
	rec := &recorder{}
	ns, srv, token := newTestNetwork(t)
	ns.SetLogger(rec)

	const guessed = "GUESSED-TOKEN-0d1e2f3a"
	resp := post(t, srv, guessed, "", initializeRequest(1))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}

	ev := rec.firstOfKind(t, EventTransportRefusal)
	if ev.Status != http.StatusUnauthorized {
		t.Errorf("Status = %d, want 401", ev.Status)
	}
	if ev.Reason != ReasonNoToken {
		t.Errorf("Reason = %q, want %q", ev.Reason, ReasonNoToken)
	}
	if ev.Remote == "" {
		t.Error("Remote is empty; an unauthenticated caller's address is the only identity it " +
			"has, and a refusal nobody can attribute is one nobody can act on")
	}

	rendered, err := json.Marshal(rec.all())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rendered), guessed) {
		t.Errorf("the presented token was recorded:\n%s", rendered)
	}
	// The real token is worse still: it would put the credential in the log by way of a
	// failed guess at it.
	if strings.Contains(string(rendered), token) {
		t.Errorf("the server's own token reached the log:\n%s", rendered)
	}
}

// TestARejectedOriginDoesNotPutTheOriginInTheLog is the injection case.
//
// The Origin header is caller-supplied and echoed back to the caller deliberately — an
// operator needs to see which string to allowlist. It must not reach the log, because that
// would let whoever is being refused choose what gets written into a file somebody else
// reads.
//
// The forged value below has no newline in it, deliberately: net/http refuses to send a
// header value containing one, so a literal injected line cannot be tested through a real
// client. That makes the newline the *transport's* guarantee and this test's subject the
// one that remains ours — that the value is not written at all, whatever it contains.
func TestARejectedOriginDoesNotPutTheOriginInTheLog(t *testing.T) {
	rec := &recorder{}
	ns, srv, token := newTestNetwork(t)
	ns.SetLogger(rec)

	const hostile = "http://evil.example/;breeze-mcp:+tool+breeze_new+ok"
	resp := postWithOrigin(t, srv, token, hostile)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}

	ev := rec.firstOfKind(t, EventTransportRefusal)
	if ev.Reason != ReasonOriginRejected {
		t.Errorf("Reason = %q, want %q", ev.Reason, ReasonOriginRejected)
	}

	rendered, err := json.Marshal(rec.all())
	if err != nil {
		t.Fatal(err)
	}
	// Either fragment appearing means the origin reached the log.
	for _, fragment := range []string{"evil.example", "breeze_new"} {
		if strings.Contains(string(rendered), fragment) {
			t.Errorf("a caller-supplied Origin reached the log, so a refused caller chooses "+
				"what is written to it (%q):\n%s", fragment, rendered)
		}
	}
}

// TestAnUnknownToolAndAScopeRefusalAreDistinguishable is why they are separate kinds.
//
// On the wire the two are deliberately close: a refusal should not enumerate what a caller
// cannot have. In a *log* the opposite is wanted — an operator debugging "the agent says the
// tool is missing" needs to know whether the answer is "wrong mode" or "wrong token".
func TestAnUnknownToolAndAScopeRefusalAreDistinguishable(t *testing.T) {
	scope, err := NewScope(CapIntrospection)
	if err != nil {
		t.Fatal(err)
	}

	rec := &recorder{}
	srv := NewServer("test")
	srv.SetScope(scope)
	srv.SetLogger(rec)

	// breeze_new exists, and this token's scope excludes it.
	dispatchToolCall(t, srv, "breeze_new")
	// This one does not exist at all.
	dispatchToolCall(t, srv, "breeze_definitely_not_a_tool")

	refused := rec.firstOfKind(t, EventToolRefused)
	if refused.Tool != "breeze_new" {
		t.Errorf("the refused event names %q, want breeze_new", refused.Tool)
	}
	// The capability is what makes the line actionable: it says which scope to grant.
	if refused.Reason != string(CapGeneration) {
		t.Errorf("Reason = %q, want the capability needed (%q)", refused.Reason, CapGeneration)
	}

	unknown := rec.firstOfKind(t, EventToolUnknown)
	if unknown.Tool != "breeze_definitely_not_a_tool" {
		t.Errorf("the unknown event names %q", unknown.Tool)
	}
}

// dispatchToolCall sends one tools/call, ignoring the result.
func dispatchToolCall(t *testing.T, srv *Server, name string) {
	t.Helper()

	message, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out := srv.RPCServer().Handle(message); len(out) == 0 {
		t.Fatalf("tools/call %s produced no response", name)
	}
}

// TestNoLoggerIsTheDefaultAndStillWorks is the path every existing caller takes.
//
// SetLogger is never called, so every emit hits its nil check and returns. The assertion is
// that the server still answers — the point being that logging is genuinely optional rather
// than a nil dereference waiting for the first call.
func TestNoLoggerIsTheDefaultAndStillWorks(t *testing.T) {
	srv := NewServer("test")

	message, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "breeze_features"},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := srv.RPCServer().Handle(message)
	if len(out) == 0 {
		t.Fatal("a server with no logger produced no response")
	}
	var reply map[string]any
	if err := json.Unmarshal(out, &reply); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if _, isError := reply["error"]; isError {
		t.Errorf("a server with no logger answered with an error: %s", out)
	}
}

// TestSetLoggerOnTheTransportAlsoCoversDispatch is the wiring.
//
// The transport sees refusals; the Server behind it sees calls. An operator asking for a log
// wants both, so NetworkServer.SetLogger installs on both — otherwise a caller that wired
// only the transport would get refusals and, silently, no tool calls at all.
func TestSetLoggerOnTheTransportAlsoCoversDispatch(t *testing.T) {
	rec := &recorder{}
	ns, srv, token := newTestNetwork(t)
	ns.SetLogger(rec)

	session := handshake(t, srv, token)
	resp := post(t, srv, token, session, map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": "breeze_features"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tools/call status = %d, want 200", resp.StatusCode)
	}

	// A dispatch-side event, reached through a transport-side SetLogger.
	ev := rec.firstOfKind(t, EventToolCall)
	if ev.Tool != "breeze_features" {
		t.Errorf("Tool = %q, want breeze_features", ev.Tool)
	}
	// And the handshake that preceded it.
	rec.firstOfKind(t, EventHandshake)
}
