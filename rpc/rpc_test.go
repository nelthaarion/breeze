package rpc

import (
	"reflect"
	"strings"
	"testing"

	json "github.com/goccy/go-json"
)

// rpc_test.go — conformance against the JSON-RPC 2.0 specification.
//
// The assertions are made against decoded JSON rather than exact byte strings.
// A response is a JSON object, and JSON objects are unordered, so comparing
// serialized text would couple every test to this implementation's member order
// and fail on a refactor that changed nothing observable. Where the ordering of
// an array matters — a batch — that is asserted explicitly, because there the
// specification does constrain it.

// testServer builds a server with the arithmetic methods used by the
// specification's own examples in §7, so the fixtures below can be quoted
// verbatim from the document.
func testServer(t *testing.T) *Server {
	t.Helper()

	reg := NewRegistry()

	// subtract accepts both by-position and by-name parameters, which is the
	// case §4.2 calls out and the one the examples exercise both ways.
	reg.Register("subtract", func(ctx *Context) {
		var byName struct {
			Minuend    *int `json:"minuend"`
			Subtrahend *int `json:"subtrahend"`
		}
		if err := json.Unmarshal(ctx.Params, &byName); err == nil &&
			byName.Minuend != nil && byName.Subtrahend != nil {
			ctx.Result(*byName.Minuend - *byName.Subtrahend)
			return
		}
		var byPos []int
		if err := ctx.Bind(&byPos); err != nil {
			return
		}
		if len(byPos) != 2 {
			ctx.Error(ErrInvalidParams())
			return
		}
		ctx.Result(byPos[0] - byPos[1])
	})

	reg.Register("sum", func(ctx *Context) {
		var nums []int
		if err := ctx.Bind(&nums); err != nil {
			return
		}
		total := 0
		for _, n := range nums {
			total += n
		}
		ctx.Result(total)
	})

	reg.Register("get_data", func(ctx *Context) {
		ctx.Result([]any{"hello", 5})
	})

	// update and notify_hello exist only to be notified; they record nothing,
	// matching the specification's examples where the server's reply is the
	// only thing under test.
	reg.Register("update", func(ctx *Context) {})
	reg.Register("notify_hello", func(ctx *Context) {})

	reg.Register("boom", func(ctx *Context) {
		panic("handler exploded")
	})

	reg.Register("needs_object", func(ctx *Context) {
		var p struct {
			Name string `json:"name"`
		}
		if err := ctx.Bind(&p); err != nil {
			return
		}
		ctx.Result(p.Name)
	})

	reg.Register("app_error", func(ctx *Context) {
		ctx.ErrorData(-32001, "insufficient funds", map[string]any{"balance": 12})
	})

	return NewServer(reg)
}

// decode unmarshals a response into a generic map, failing the test if the bytes
// are not a JSON object. Decoding is itself part of the assertion: a response
// this implementation cannot round-trip through a standard decoder is not a
// conforming response, whatever its contents.
func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	if len(b) == 0 {
		t.Fatal("expected a response, got none")
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("response is not a JSON object: %v\nbytes: %s", err, b)
	}
	return m
}

// decodeBatch unmarshals a response into an array of objects.
func decodeBatch(t *testing.T, b []byte) []map[string]any {
	t.Helper()
	if len(b) == 0 {
		t.Fatal("expected a batch response, got none")
	}
	var a []map[string]any
	if err := json.Unmarshal(b, &a); err != nil {
		t.Fatalf("response is not a JSON array: %v\nbytes: %s", err, b)
	}
	return a
}

// assertVersion checks the mandatory jsonrpc member (§5).
func assertVersion(t *testing.T, m map[string]any) {
	t.Helper()
	if m["jsonrpc"] != "2.0" {
		t.Errorf("jsonrpc = %v, want \"2.0\"", m["jsonrpc"])
	}
}

// assertErrorCode checks that m is an error response carrying code.
//
// It also asserts the absence of a result member, which §5 requires: "Either the
// result member or error member MUST be included, but both members MUST NOT be
// included." A response with both would satisfy a naive code check while being
// unusable by a conforming client.
func assertErrorCode(t *testing.T, m map[string]any, code int) {
	t.Helper()
	assertVersion(t, m)
	if _, ok := m["result"]; ok {
		t.Error("error response must not contain a result member")
	}
	raw, ok := m["error"]
	if !ok {
		t.Fatalf("expected an error member, got %v", m)
	}
	errObj, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("error member is %T, want an object", raw)
	}
	got, ok := errObj["code"].(float64)
	if !ok {
		t.Fatalf("error code is %T, want a number", errObj["code"])
	}
	if int(got) != code {
		t.Errorf("error code = %d, want %d", int(got), code)
	}
	if msg, ok := errObj["message"].(string); !ok || msg == "" {
		t.Errorf("error message = %v, want a non-empty string", errObj["message"])
	}
}

// assertResult checks that m is a success response and returns its result.
func assertResult(t *testing.T, m map[string]any) any {
	t.Helper()
	assertVersion(t, m)
	if _, ok := m["error"]; ok {
		t.Fatalf("expected a result, got error %v", m["error"])
	}
	res, ok := m["result"]
	if !ok {
		t.Fatalf("success response must contain a result member, got %v", m)
	}
	return res
}

// ─── §4 Request object ───────────────────────────────────────────────────────

// TestSingleRequestPositionalParams is the specification's first §7 example.
func TestSingleRequestPositionalParams(t *testing.T) {
	s := testServer(t)

	out := s.Handle([]byte(`{"jsonrpc": "2.0", "method": "subtract", "params": [42, 23], "id": 1}`))
	m := decode(t, out)

	if got := assertResult(t, m); got != float64(19) {
		t.Errorf("result = %v, want 19", got)
	}
	if m["id"] != float64(1) {
		t.Errorf("id = %v, want 1", m["id"])
	}
}

// TestSingleRequestNamedParams covers by-name parameters (§4.2).
func TestSingleRequestNamedParams(t *testing.T) {
	s := testServer(t)

	out := s.Handle([]byte(
		`{"jsonrpc": "2.0", "method": "subtract", "params": {"subtrahend": 23, "minuend": 42}, "id": 3}`,
	))
	m := decode(t, out)

	if got := assertResult(t, m); got != float64(19) {
		t.Errorf("result = %v, want 19", got)
	}
	if m["id"] != float64(3) {
		t.Errorf("id = %v, want 3", m["id"])
	}
}

// TestIDIsEchoedExactly checks that the id member comes back byte-identical.
//
// This is the reason Request.ID is raw JSON rather than an any. Decoding an id
// of 1 into an interface produces a float64, and re-encoding that can yield 1
// or 1.0 depending on the encoder — a different value than the client sent, for
// a member whose entire purpose is correlation. A string id must likewise stay
// a string and not acquire or lose quoting.
func TestIDIsEchoedExactly(t *testing.T) {
	s := testServer(t)

	for _, id := range []string{`1`, `"abc"`, `"1"`, `0`, `-7`, `4294967296`, `null`} {
		t.Run(id, func(t *testing.T) {
			req := `{"jsonrpc":"2.0","method":"sum","params":[1],"id":` + id + `}`
			out := s.Handle([]byte(req))
			if len(out) == 0 {
				t.Fatal("expected a response")
			}
			// Assert on the raw bytes here rather than the decoded value: the
			// decode is exactly the lossy step being guarded against.
			want := `"id":` + id
			if !strings.Contains(string(out), want) {
				t.Errorf("response %s does not contain %s", out, want)
			}
		})
	}
}

// TestLargeIntegerIDPrecision is the concrete failure the raw-id design avoids.
//
// An id beyond 2^53 cannot survive a float64 round trip. Held as bytes it comes
// back intact; decoded into an any it would come back as 9007199254740993 →
// 9007199254740992, silently answering a different call than the one made.
func TestLargeIntegerIDPrecision(t *testing.T) {
	s := testServer(t)

	const bigID = "9007199254740993"
	out := s.Handle([]byte(`{"jsonrpc":"2.0","method":"sum","params":[1],"id":` + bigID + `}`))

	if !strings.Contains(string(out), `"id":`+bigID) {
		t.Errorf("large id lost precision: %s", out)
	}
}

// ─── §4.1 Notification ───────────────────────────────────────────────────────

// TestNotificationProducesNoResponse covers §4.1: a request without an id is a
// notification and "the Server MUST NOT reply".
func TestNotificationProducesNoResponse(t *testing.T) {
	s := testServer(t)

	for _, req := range []string{
		`{"jsonrpc": "2.0", "method": "update", "params": [1,2,3,4,5]}`,
		`{"jsonrpc": "2.0", "method": "notify_hello", "params": [7]}`,
	} {
		if out := s.Handle([]byte(req)); len(out) != 0 {
			t.Errorf("notification produced a response: %s", out)
		}
	}
}

// TestNotificationHandlerStillRuns checks that a notification is dispatched and
// only its reply is suppressed.
//
// The distinction matters: a server that skipped the handler entirely would also
// pass a test that only asserted the absence of a response, while silently
// dropping every fire-and-forget call the client made.
func TestNotificationHandlerStillRuns(t *testing.T) {
	reg := NewRegistry()
	called := 0
	reg.Register("touch", func(ctx *Context) { called++ })
	s := NewServer(reg)

	if out := s.Handle([]byte(`{"jsonrpc":"2.0","method":"touch"}`)); len(out) != 0 {
		t.Fatalf("expected no response, got %s", out)
	}
	if called != 1 {
		t.Errorf("handler ran %d times, want 1", called)
	}
}

// TestNotificationToUnknownMethodIsSilent covers the intersection of §4.1 and
// §5.1: a notification gets no reply even when it names a method that does not
// exist, because the no-reply rule is about the absent id, not about success.
func TestNotificationToUnknownMethodIsSilent(t *testing.T) {
	s := testServer(t)

	if out := s.Handle([]byte(`{"jsonrpc":"2.0","method":"nonexistent"}`)); len(out) != 0 {
		t.Errorf("notification to unknown method produced a response: %s", out)
	}
}

// TestNullIDIsARequestNotANotification distinguishes an absent id from an id of
// literal null.
//
// §4.1 makes the *absence* of the member the definition of a notification. A
// null id is discouraged by §4 but is still a present member, so the call must
// be answered. Conflating the two — which any implementation decoding the id
// into a nil-able type will do unless it is careful — turns an answerable
// request into silence.
func TestNullIDIsARequestNotANotification(t *testing.T) {
	s := testServer(t)

	out := s.Handle([]byte(`{"jsonrpc":"2.0","method":"sum","params":[1,2],"id":null}`))
	if len(out) == 0 {
		t.Fatal("a request with id null must be answered")
	}
	m := decode(t, out)
	if got := assertResult(t, m); got != float64(3) {
		t.Errorf("result = %v, want 3", got)
	}
	if m["id"] != nil {
		t.Errorf("id = %v, want null", m["id"])
	}
}

// ─── §5.1 Error codes ────────────────────────────────────────────────────────

// TestParseError covers -32700: "Invalid JSON was received by the server."
func TestParseError(t *testing.T) {
	s := testServer(t)

	// The specification's own malformed example (§7 "rpc call with invalid
	// JSON"), plus shapes that close structurally but are not valid JSON, which
	// is what separates the framer's job from the decoder's.
	for _, bad := range []string{
		`{"jsonrpc": "2.0", "method": "foobar, "params": "bar", "baz]`,
		`{"jsonrpc":"2.0",}`,
		`{"method"}`,
		`{"jsonrpc":"2.0" "method":"sum"}`,
		`{`,
	} {
		t.Run(bad, func(t *testing.T) {
			m := decode(t, s.Handle([]byte(bad)))
			assertErrorCode(t, m, CodeParseError)
			if m["id"] != nil {
				t.Errorf("id = %v, want null for a parse error", m["id"])
			}
		})
	}
}

// TestInvalidRequest covers -32600 for a non-object and for a malformed member
// set, which is the specification's §7 "rpc call with invalid Request object".
func TestInvalidRequest(t *testing.T) {
	s := testServer(t)

	for name, req := range map[string]string{
		"method is not a string": `{"jsonrpc": "2.0", "method": 1, "params": "bar"}`,
		"bare number":            `1`,
		"bare string":            `"hello"`,
		"bare true":              `true`,
		"bare null":              `null`,
		"params is a string":     `{"jsonrpc":"2.0","method":"sum","params":"bar","id":1}`,
		"params is a number":     `{"jsonrpc":"2.0","method":"sum","params":5,"id":1}`,
		"missing method":         `{"jsonrpc":"2.0","id":1}`,
		"empty method":           `{"jsonrpc":"2.0","method":"","id":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			assertErrorCode(t, decode(t, s.Handle([]byte(req))), CodeInvalidRequest)
		})
	}
}

// TestMissingOrInvalidVersion covers the jsonrpc member requirement in §4.
//
// A 1.0 request has no jsonrpc member at all, and this is the check that stops a
// 2.0 server from silently accepting one and then applying 2.0 semantics — such
// as the notification rule — to a message that never agreed to them.
func TestMissingOrInvalidVersion(t *testing.T) {
	s := testServer(t)

	for name, req := range map[string]string{
		"absent":        `{"method":"sum","params":[1,2],"id":1}`,
		"one point oh":  `{"jsonrpc":"1.0","method":"sum","params":[1,2],"id":1}`,
		"two":           `{"jsonrpc":"2","method":"sum","params":[1,2],"id":1}`,
		"two point one": `{"jsonrpc":"2.1","method":"sum","params":[1,2],"id":1}`,
		"numeric":       `{"jsonrpc":2.0,"method":"sum","params":[1,2],"id":1}`,
		"empty":         `{"jsonrpc":"","method":"sum","params":[1,2],"id":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			assertErrorCode(t, decode(t, s.Handle([]byte(req))), CodeInvalidRequest)
		})
	}
}

// TestVersionCheckAppliesToNotifications makes sure a versionless notification
// is answered rather than silently dropped.
//
// This is the case an implementation gets wrong by checking the id before the
// version: the message has no id, so it looks like a notification and gets
// silence, and the client never learns its request was rejected. §4 makes the
// version member mandatory for the object to be a valid Request at all, so the
// version check has to come first.
func TestVersionCheckAppliesToNotifications(t *testing.T) {
	s := testServer(t)

	out := s.Handle([]byte(`{"method":"update","params":[1]}`))
	if len(out) == 0 {
		t.Fatal(
			"a versionless message is an invalid Request, not a notification; it must be answered",
		)
	}
	assertErrorCode(t, decode(t, out), CodeInvalidRequest)
}

// TestMethodNotFound covers -32601, the specification's §7 example.
func TestMethodNotFound(t *testing.T) {
	s := testServer(t)

	m := decode(t, s.Handle([]byte(`{"jsonrpc": "2.0", "method": "foobar", "id": "1"}`)))
	assertErrorCode(t, m, CodeMethodNotFound)
	if m["id"] != "1" {
		t.Errorf("id = %v, want \"1\"", m["id"])
	}
}

// TestInvalidParams covers -32602, raised by Bind when the params do not fit the
// handler's expected shape.
func TestInvalidParams(t *testing.T) {
	s := testServer(t)

	for name, req := range map[string]string{
		"array where object expected": `{"jsonrpc":"2.0","method":"needs_object","params":[1,2],"id":1}`,
		"string element in int array": `{"jsonrpc":"2.0","method":"sum","params":["a","b"],"id":2}`,
		"object where array expected": `{"jsonrpc":"2.0","method":"sum","params":{"a":1},"id":3}`,
	} {
		t.Run(name, func(t *testing.T) {
			assertErrorCode(t, decode(t, s.Handle([]byte(req))), CodeInvalidParams)
		})
	}
}

// TestInternalError covers -32603 for a panicking handler.
//
// A panic is the server's fault, so it must become a response rather than
// escaping to kill the event-loop goroutine and every connection pinned to it.
func TestInternalError(t *testing.T) {
	s := testServer(t)

	m := decode(t, s.Handle([]byte(`{"jsonrpc":"2.0","method":"boom","id":9}`)))
	assertErrorCode(t, m, CodeInternalError)
	if m["id"] != float64(9) {
		t.Errorf("id = %v, want 9", m["id"])
	}
}

// TestPanicInNotificationIsContained checks that a panicking notification is
// swallowed rather than crashing, and still produces no reply.
func TestPanicInNotificationIsContained(t *testing.T) {
	s := testServer(t)

	if out := s.Handle([]byte(`{"jsonrpc":"2.0","method":"boom"}`)); len(out) != 0 {
		t.Errorf("panicking notification produced a response: %s", out)
	}
}

// TestApplicationDefinedErrorCode covers the room §5.1 leaves outside the
// reserved range, including the data member the specification leaves to the
// server to define.
func TestApplicationDefinedErrorCode(t *testing.T) {
	s := testServer(t)

	m := decode(t, s.Handle([]byte(`{"jsonrpc":"2.0","method":"app_error","id":4}`)))
	assertErrorCode(t, m, -32001)

	errObj := m["error"].(map[string]any)
	data, ok := errObj["data"].(map[string]any)
	if !ok {
		t.Fatalf("data = %v, want an object", errObj["data"])
	}
	if data["balance"] != float64(12) {
		t.Errorf("data.balance = %v, want 12", data["balance"])
	}
}

// TestErrorCodeReserved documents where an application may safely put its own
// codes.
func TestErrorCodeReserved(t *testing.T) {
	reserved := []int{-32768, -32700, -32603, -32602, -32601, -32600, -32099, -32000}
	for _, c := range reserved {
		if !ErrorCodeReserved(c) {
			t.Errorf("ErrorCodeReserved(%d) = false, want true", c)
		}
	}
	free := []int{-31999, -1, 0, 1, 100, -32769}
	for _, c := range free {
		if ErrorCodeReserved(c) {
			t.Errorf("ErrorCodeReserved(%d) = true, want false", c)
		}
	}
}

// ─── §6 Batch ────────────────────────────────────────────────────────────────

// TestBatchAllSucceed checks a batch of ordinary calls.
func TestBatchAllSucceed(t *testing.T) {
	s := testServer(t)

	out := s.Handle([]byte(`[
		{"jsonrpc":"2.0","method":"sum","params":[1,2,4],"id":"1"},
		{"jsonrpc":"2.0","method":"subtract","params":[42,23],"id":"2"},
		{"jsonrpc":"2.0","method":"get_data","id":"9"}
	]`))

	got := decodeBatch(t, out)
	if len(got) != 3 {
		t.Fatalf("batch returned %d responses, want 3", len(got))
	}
	if r := assertResult(t, got[0]); r != float64(7) {
		t.Errorf("responses[0].result = %v, want 7", r)
	}
	if r := assertResult(t, got[1]); r != float64(19) {
		t.Errorf("responses[1].result = %v, want 19", r)
	}
	if r := assertResult(t, got[2]); !reflect.DeepEqual(r, []any{"hello", float64(5)}) {
		t.Errorf("responses[2].result = %v, want [hello 5]", r)
	}
}

// TestBatchMixedIsTheSpecExample is the §7 "rpc call Batch" example, which is
// the single most demanding fixture in the document: it mixes successes, a
// notification, an unknown method, an invalid request and a valid call, and
// pins the exact set of responses expected back.
func TestBatchMixedIsTheSpecExample(t *testing.T) {
	s := testServer(t)

	out := s.Handle([]byte(`[
		{"jsonrpc": "2.0", "method": "sum", "params": [1,2,4], "id": "1"},
		{"jsonrpc": "2.0", "method": "notify_hello", "params": [7]},
		{"jsonrpc": "2.0", "method": "subtract", "params": [42,23], "id": "2"},
		{"foo": "boo"},
		{"jsonrpc": "2.0", "method": "foo.get", "params": {"name": "myself"}, "id": "5"},
		{"jsonrpc": "2.0", "method": "get_data", "id": "9"}
	]`))

	got := decodeBatch(t, out)

	// Five responses, not six: the notification contributes nothing (§6).
	if len(got) != 5 {
		t.Fatalf(
			"batch returned %d responses, want 5 (the notification must not produce one)",
			len(got),
		)
	}

	if r := assertResult(t, got[0]); r != float64(7) {
		t.Errorf("responses[0].result = %v, want 7", r)
	}
	if got[0]["id"] != "1" {
		t.Errorf("responses[0].id = %v, want \"1\"", got[0]["id"])
	}

	if r := assertResult(t, got[1]); r != float64(19) {
		t.Errorf("responses[1].result = %v, want 19", r)
	}
	if got[1]["id"] != "2" {
		t.Errorf("responses[1].id = %v, want \"2\"", got[1]["id"])
	}

	// {"foo":"boo"} is valid JSON but not a valid Request.
	assertErrorCode(t, got[2], CodeInvalidRequest)
	if got[2]["id"] != nil {
		t.Errorf("responses[2].id = %v, want null", got[2]["id"])
	}

	assertErrorCode(t, got[3], CodeMethodNotFound)
	if got[3]["id"] != "5" {
		t.Errorf("responses[3].id = %v, want \"5\"", got[3]["id"])
	}

	if r := assertResult(t, got[4]); !reflect.DeepEqual(r, []any{"hello", float64(5)}) {
		t.Errorf("responses[4].result = %v, want [hello 5]", r)
	}
}

// TestBatchOfAllNotificationsReturnsNothing covers the §6 rule that a batch
// producing no responses must return nothing at all, not an empty array.
func TestBatchOfAllNotificationsReturnsNothing(t *testing.T) {
	s := testServer(t)

	out := s.Handle([]byte(`[
		{"jsonrpc": "2.0", "method": "notify_hello", "params": [1,2,4]},
		{"jsonrpc": "2.0", "method": "update", "params": [7]}
	]`))

	if len(out) != 0 {
		t.Errorf("batch of notifications returned %s, want nothing", out)
	}
	if string(out) == "[]" {
		t.Error("an empty array is explicitly forbidden by §6")
	}
}

// TestBatchInvalidJSON covers the §7 "rpc call Batch, invalid JSON" example: the
// reply is a single error object, not an array.
func TestBatchInvalidJSON(t *testing.T) {
	s := testServer(t)

	out := s.Handle([]byte(`[
		{"jsonrpc": "2.0", "method": "sum", "params": [1,2,4], "id": "1"},
		{"jsonrpc": "2.0", "method"
	]`))

	assertErrorCode(t, decode(t, out), CodeParseError)
}

// TestBatchEmptyArray covers the §7 "rpc call with an empty Array" example.
//
// The reply is a single object rather than an array, because the batch itself was
// not recognised as an array with at least one value (§6).
func TestBatchEmptyArray(t *testing.T) {
	s := testServer(t)

	out := s.Handle([]byte(`[]`))
	assertErrorCode(t, decode(t, out), CodeInvalidRequest)

	if len(out) > 0 && out[0] == '[' {
		t.Errorf("empty batch answered with an array %s, want a single object", out)
	}
}

// TestBatchInvalidNonEmpty covers the §7 "rpc call with an invalid Batch (but
// not empty)" example: [1] gets an array containing one error.
func TestBatchInvalidNonEmpty(t *testing.T) {
	s := testServer(t)

	got := decodeBatch(t, s.Handle([]byte(`[1]`)))
	if len(got) != 1 {
		t.Fatalf("got %d responses, want 1", len(got))
	}
	assertErrorCode(t, got[0], CodeInvalidRequest)
}

// TestBatchInvalidMultiple covers the §7 "rpc call with invalid Batch" example:
// [1,2,3] gets three separate invalid-request errors, one per element.
func TestBatchInvalidMultiple(t *testing.T) {
	s := testServer(t)

	got := decodeBatch(t, s.Handle([]byte(`[1,2,3]`)))
	if len(got) != 3 {
		t.Fatalf("got %d responses, want 3", len(got))
	}
	for i, m := range got {
		assertErrorCode(t, m, CodeInvalidRequest)
		if m["id"] != nil {
			t.Errorf("responses[%d].id = %v, want null", i, m["id"])
		}
	}
}

// TestBatchPreservesOrder checks that responses come back in request order.
//
// §6 permits any order and requires the client to correlate by id, so this is
// not a conformance requirement — but a client that does correlate by position
// is common, and silently reordering would break it. The property is asserted so
// a future change to batch execution has to be deliberate about giving it up.
func TestBatchPreservesOrder(t *testing.T) {
	s := testServer(t)

	out := s.Handle([]byte(`[
		{"jsonrpc":"2.0","method":"sum","params":[1],"id":1},
		{"jsonrpc":"2.0","method":"sum","params":[2],"id":2},
		{"jsonrpc":"2.0","method":"sum","params":[3],"id":3},
		{"jsonrpc":"2.0","method":"sum","params":[4],"id":4}
	]`))

	got := decodeBatch(t, out)
	if len(got) != 4 {
		t.Fatalf("got %d responses, want 4", len(got))
	}
	for i, m := range got {
		wantID := float64(i + 1)
		if m["id"] != wantID {
			t.Errorf("responses[%d].id = %v, want %v", i, m["id"], wantID)
		}
		if r := assertResult(t, m); r != wantID {
			t.Errorf("responses[%d].result = %v, want %v", i, r, wantID)
		}
	}
}

// TestBatchWithNotificationInMiddleHasNoHole is a regression guard on the
// separator bookkeeping.
//
// Skipping a notification means not writing a response *and* not writing the
// comma that would have preceded it. Getting that wrong produces `[{...},,{...}]`
// or a trailing comma — output that no client can parse, from a batch where
// every individual call succeeded.
func TestBatchWithNotificationInMiddleHasNoHole(t *testing.T) {
	s := testServer(t)

	for name, req := range map[string]string{
		"notification first": `[
			{"jsonrpc":"2.0","method":"update","params":[0]},
			{"jsonrpc":"2.0","method":"sum","params":[1],"id":1},
			{"jsonrpc":"2.0","method":"sum","params":[2],"id":2}]`,
		"notification middle": `[
			{"jsonrpc":"2.0","method":"sum","params":[1],"id":1},
			{"jsonrpc":"2.0","method":"update","params":[0]},
			{"jsonrpc":"2.0","method":"sum","params":[2],"id":2}]`,
		"notification last": `[
			{"jsonrpc":"2.0","method":"sum","params":[1],"id":1},
			{"jsonrpc":"2.0","method":"sum","params":[2],"id":2},
			{"jsonrpc":"2.0","method":"update","params":[0]}]`,
		"notifications around": `[
			{"jsonrpc":"2.0","method":"update","params":[0]},
			{"jsonrpc":"2.0","method":"sum","params":[1],"id":1},
			{"jsonrpc":"2.0","method":"update","params":[0]},
			{"jsonrpc":"2.0","method":"sum","params":[2],"id":2},
			{"jsonrpc":"2.0","method":"update","params":[0]}]`,
	} {
		t.Run(name, func(t *testing.T) {
			out := s.Handle([]byte(req))
			got := decodeBatch(t, out) // fails loudly on `,,` or a trailing comma
			if len(got) != 2 {
				t.Fatalf("got %d responses, want 2: %s", len(got), out)
			}
			if got[0]["id"] != float64(1) || got[1]["id"] != float64(2) {
				t.Errorf("ids = %v, %v; want 1, 2", got[0]["id"], got[1]["id"])
			}
		})
	}
}

// TestNestedBatchIsNotFlattened checks that an array inside a batch is rejected
// as an invalid request rather than recursed into.
//
// §6 defines a batch as an array of Request objects. Treating a nested array as
// a sub-batch would let a client nest arbitrarily deep and make the server's
// stack depth a function of its input.
func TestNestedBatchIsNotFlattened(t *testing.T) {
	s := testServer(t)

	got := decodeBatch(
		t,
		s.Handle([]byte(`[[{"jsonrpc":"2.0","method":"sum","params":[1],"id":1}]]`)),
	)
	if len(got) != 1 {
		t.Fatalf("got %d responses, want 1", len(got))
	}
	assertErrorCode(t, got[0], CodeInvalidRequest)
}

// ─── Response invariants ─────────────────────────────────────────────────────

// TestHandlerSettingNothingProducesNullResult checks that the mandatory result
// member is present even when the handler set nothing.
//
// §5 requires result on a success response. Omitting it yields an object that is
// neither a valid success nor a valid error, which a strict client rejects
// outright.
func TestHandlerSettingNothingProducesNullResult(t *testing.T) {
	reg := NewRegistry()
	reg.Register("silent", func(ctx *Context) {})
	s := NewServer(reg)

	out := s.Handle([]byte(`{"jsonrpc":"2.0","method":"silent","id":1}`))
	m := decode(t, out)
	assertVersion(t, m)

	res, ok := m["result"]
	if !ok {
		t.Fatalf("result member is missing: %s", out)
	}
	if res != nil {
		t.Errorf("result = %v, want null", res)
	}
}

// TestErrorWinsOverResult pins the tie-break when a handler sets both.
//
// Reporting a partially-built result as success would hide a failure the handler
// explicitly signalled, so the error is what goes out.
func TestErrorWinsOverResult(t *testing.T) {
	reg := NewRegistry()
	reg.Register("both", func(ctx *Context) {
		ctx.Result("partial")
		ctx.Errorf(-32050, "failed after partial work")
	})
	s := NewServer(reg)

	m := decode(t, s.Handle([]byte(`{"jsonrpc":"2.0","method":"both","id":1}`)))
	assertErrorCode(t, m, -32050)
}

// TestResultRawIsForwardedVerbatim covers the proxy path.
func TestResultRawIsForwardedVerbatim(t *testing.T) {
	reg := NewRegistry()
	reg.Register("cached", func(ctx *Context) {
		ctx.ResultRaw(json.RawMessage(`{"cached":true,"n":9007199254740993}`))
	})
	s := NewServer(reg)

	out := s.Handle([]byte(`{"jsonrpc":"2.0","method":"cached","id":1}`))
	if !strings.Contains(string(out), `"n":9007199254740993`) {
		t.Errorf("raw result was not forwarded verbatim: %s", out)
	}
}

// TestErrorMessageIsEscaped checks that a message containing JSON metacharacters
// cannot break the envelope.
//
// The serializer writes the envelope as literal bytes, so it — not the encoder —
// is responsible for escaping. A quote or a newline passed through unescaped
// would produce a response that fails to parse, turning an application error
// into a protocol error.
func TestErrorMessageIsEscaped(t *testing.T) {
	reg := NewRegistry()
	reg.Register("nasty", func(ctx *Context) {
		ctx.Errorf(-32050, "he said \"stop\"\n\tand\\left \x01")
	})
	s := NewServer(reg)

	out := s.Handle([]byte(`{"jsonrpc":"2.0","method":"nasty","id":1}`))
	m := decode(t, out) // fails if the escaping is wrong
	errObj := m["error"].(map[string]any)

	want := "he said \"stop\"\n\tand\\left \x01"
	if errObj["message"] != want {
		t.Errorf("message = %q, want %q", errObj["message"], want)
	}
}

// TestUnencodableResultBecomesInternalError checks the rollback path in the
// serializer.
//
// A channel cannot be marshalled. The result member has already been opened by
// the time that is discovered, so the response has to be rewound rather than
// having an error member appended after a half-written result.
func TestUnencodableResultBecomesInternalError(t *testing.T) {
	reg := NewRegistry()
	reg.Register("unencodable", func(ctx *Context) {
		ctx.Result(make(chan int))
	})
	s := NewServer(reg)

	out := s.Handle([]byte(`{"jsonrpc":"2.0","method":"unencodable","id":1}`))
	m := decode(t, out) // fails if the rollback left torn bytes
	assertErrorCode(t, m, CodeInternalError)
	if m["id"] != float64(1) {
		t.Errorf("id = %v, want 1", m["id"])
	}
}

// TestUnencodableErrorDataKeepsTheError checks that a diagnostic attachment
// which cannot be encoded is dropped rather than replacing the real error.
//
// The code and message are what the client acts on. Escalating to -32603 because
// the optional data member failed to marshal would discard the actual cause.
func TestUnencodableErrorDataKeepsTheError(t *testing.T) {
	reg := NewRegistry()
	reg.Register("bad_data", func(ctx *Context) {
		ctx.ErrorData(-32055, "real cause", make(chan int))
	})
	s := NewServer(reg)

	m := decode(t, s.Handle([]byte(`{"jsonrpc":"2.0","method":"bad_data","id":1}`)))
	assertErrorCode(t, m, -32055)

	errObj := m["error"].(map[string]any)
	if errObj["message"] != "real cause" {
		t.Errorf("message = %v, want \"real cause\"", errObj["message"])
	}
	if _, ok := errObj["data"]; ok {
		t.Errorf("unencodable data should be omitted, got %v", errObj["data"])
	}
}

// ─── Registration API ────────────────────────────────────────────────────────

// TestMiddlewareRunsInOrder checks the chain composition mirrors the router's.
func TestMiddlewareRunsInOrder(t *testing.T) {
	var order []string

	reg := NewRegistry()
	reg.Use(func(ctx *Context) {
		order = append(order, "global-before")
		ctx.Next()
		order = append(order, "global-after")
	})
	reg.Register("m", func(ctx *Context) {
		order = append(order, "handler")
		ctx.Result(true)
	}, func(ctx *Context) {
		order = append(order, "route-before")
		ctx.Next()
		order = append(order, "route-after")
	})

	s := NewServer(reg)
	s.Handle([]byte(`{"jsonrpc":"2.0","method":"m","id":1}`))

	want := []string{"global-before", "route-before", "handler", "route-after", "global-after"}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}
}

// TestUseAppliesToMethodsRegisteredBefore pins the chain-rebuild behaviour.
//
// Without the rebuild, middleware added after a Register call silently does not
// apply to it — which for an auth middleware means an unauthenticated method in
// production, discoverable only by reading registration order.
func TestUseAppliesToMethodsRegisteredBefore(t *testing.T) {
	var seen []string

	reg := NewRegistry()
	reg.Register("early", func(ctx *Context) {
		seen = append(seen, "handler")
		ctx.Result(true)
	})
	reg.Use(func(ctx *Context) {
		seen = append(seen, "mw")
		ctx.Next()
	})

	s := NewServer(reg)
	s.Handle([]byte(`{"jsonrpc":"2.0","method":"early","id":1}`))

	if !reflect.DeepEqual(seen, []string{"mw", "handler"}) {
		t.Errorf("seen = %v, want [mw handler]", seen)
	}
}

// TestUseTwiceKeepsBothAndPreservesMethodMiddleware guards the chain surgery in
// Use, which recovers a method's own middleware out of the flattened chain.
//
// A second Use call has to splice into a chain that already has a global prefix.
// Getting the slice bounds wrong drops the method's own middleware or duplicates
// a global one, and either way the count still looks plausible.
func TestUseTwiceKeepsBothAndPreservesMethodMiddleware(t *testing.T) {
	var order []string
	mark := func(s string) HandlerFunc {
		return func(ctx *Context) {
			order = append(order, s)
			ctx.Next()
		}
	}

	reg := NewRegistry()
	reg.Use(mark("g1"))
	reg.Register("m", func(ctx *Context) {
		order = append(order, "handler")
		ctx.Result(true)
	}, mark("own"))
	reg.Use(mark("g2"))

	s := NewServer(reg)
	s.Handle([]byte(`{"jsonrpc":"2.0","method":"m","id":1}`))

	want := []string{"g1", "g2", "own", "handler"}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}
}

// TestAbortStopsTheChain covers the rejection path a middleware uses.
func TestAbortStopsTheChain(t *testing.T) {
	handlerRan := false

	reg := NewRegistry()
	reg.Use(func(ctx *Context) {
		ctx.Errorf(-32000, "unauthorized")
		ctx.Abort()
	})
	reg.Register("guarded", func(ctx *Context) {
		handlerRan = true
		ctx.Result("secret")
	})

	s := NewServer(reg)
	m := decode(t, s.Handle([]byte(`{"jsonrpc":"2.0","method":"guarded","id":1}`)))

	if handlerRan {
		t.Error("handler ran despite Abort")
	}
	assertErrorCode(t, m, -32000)
}

// TestReRegisterReplaces documents the last-wins rule.
func TestReRegisterReplaces(t *testing.T) {
	reg := NewRegistry()
	reg.Register("m", func(ctx *Context) { ctx.Result("first") })
	reg.Register("m", func(ctx *Context) { ctx.Result("second") })

	s := NewServer(reg)
	m := decode(t, s.Handle([]byte(`{"jsonrpc":"2.0","method":"m","id":1}`)))

	if got := assertResult(t, m); got != "second" {
		t.Errorf("result = %v, want \"second\"", got)
	}
}

// TestRegisterRejectsEmptyNameAndNilHandler checks the guards.
//
// A nil handler would panic on first call, under traffic, far from the
// registration that caused it — so it is refused at registration instead.
func TestRegisterRejectsEmptyNameAndNilHandler(t *testing.T) {
	reg := NewRegistry()
	reg.Register("", func(ctx *Context) {})
	reg.Register("nilled", nil)

	if got := len(reg.Methods()); got != 0 {
		t.Errorf("registered %d methods, want 0: %v", got, reg.Methods())
	}

	s := NewServer(reg)
	assertErrorCode(
		t,
		decode(t, s.Handle([]byte(`{"jsonrpc":"2.0","method":"nilled","id":1}`))),
		CodeMethodNotFound,
	)
}

// TestContextParamsAndBind covers the params surface.
func TestContextParamsAndBind(t *testing.T) {
	reg := NewRegistry()
	reg.Register("echo", func(ctx *Context) {
		var p struct {
			Name string `json:"name"`
			N    int    `json:"n"`
		}
		if err := ctx.Bind(&p); err != nil {
			return
		}
		if ctx.Method != "echo" {
			t.Errorf("ctx.Method = %q, want \"echo\"", ctx.Method)
		}
		ctx.Result(map[string]any{"name": p.Name, "n": p.N})
	})

	s := NewServer(reg)
	m := decode(
		t,
		s.Handle([]byte(`{"jsonrpc":"2.0","method":"echo","params":{"name":"x","n":3},"id":1}`)),
	)

	res := assertResult(t, m).(map[string]any)
	if res["name"] != "x" || res["n"] != float64(3) {
		t.Errorf("result = %v, want name=x n=3", res)
	}
}

// TestBindWithAbsentParamsIsNotAnError covers a method whose parameters are all
// optional (§4.2 makes params itself optional).
func TestBindWithAbsentParamsIsNotAnError(t *testing.T) {
	reg := NewRegistry()
	reg.Register("optional", func(ctx *Context) {
		var p struct {
			N int `json:"n"`
		}
		if err := ctx.Bind(&p); err != nil {
			t.Errorf("Bind with absent params returned %v, want nil", err)
			return
		}
		ctx.Result(p.N)
	})

	s := NewServer(reg)
	m := decode(t, s.Handle([]byte(`{"jsonrpc":"2.0","method":"optional","id":1}`)))
	if got := assertResult(t, m); got != float64(0) {
		t.Errorf("result = %v, want 0", got)
	}
}

// TestNullParamsIsAccepted covers a client that serializes an absent argument
// list as null rather than omitting the member.
func TestNullParamsIsAccepted(t *testing.T) {
	s := testServer(t)

	out := s.Handle([]byte(`{"jsonrpc":"2.0","method":"get_data","params":null,"id":1}`))
	m := decode(t, out)
	if r := assertResult(t, m); !reflect.DeepEqual(r, []any{"hello", float64(5)}) {
		t.Errorf("result = %v, want [hello 5]", r)
	}
}

// TestContextStoreIsNotRetainedAcrossCalls guards the pooled context's reset.
//
// A value left in the store would be visible to a later, unrelated call — on a
// different connection, belonging to a different client. That is a cross-client
// data leak, not merely a stale read, which is why every field is cleared on
// release rather than only the ones that look live.
func TestContextStoreIsNotRetainedAcrossCalls(t *testing.T) {
	reg := NewRegistry()
	reg.Register("first", func(ctx *Context) {
		ctx.Set("secret", "tenant-a-token")
		ctx.Result(true)
	})
	reg.Register("second", func(ctx *Context) {
		if v, ok := ctx.Get("secret"); ok {
			t.Errorf("context leaked %v from a previous call", v)
		}
		ctx.Result(true)
	})

	s := NewServer(reg)
	// Enough iterations that the pool is certain to hand back a used context.
	for i := 0; i < 64; i++ {
		s.Handle([]byte(`{"jsonrpc":"2.0","method":"first","id":1}`))
		s.Handle([]byte(`{"jsonrpc":"2.0","method":"second","id":2}`))
	}
}

// TestMethodsIntrospection covers the introspection helper.
func TestMethodsIntrospection(t *testing.T) {
	reg := NewRegistry()
	reg.Register("a", func(ctx *Context) {})
	reg.Register("b", func(ctx *Context) {})
	reg.RegisterBlocking("c", func(ctx *Context) {})

	got := reg.Methods()
	if len(got) != 3 {
		t.Fatalf("Methods() = %v, want 3 entries", got)
	}
	seen := map[string]bool{}
	for _, n := range got {
		seen[n] = true
	}
	for _, want := range []string{"a", "b", "c"} {
		if !seen[want] {
			t.Errorf("Methods() missing %q: %v", want, got)
		}
	}
}

// TestBlockingCountTracksRegistration checks the fast-path gate.
//
// The pre-scan that decides whether a message needs a worker is skipped entirely
// when this counter is zero, so a miscount means either a blocking handler runs
// on the event loop or every message pays for a decode it does not need.
func TestBlockingCountTracksRegistration(t *testing.T) {
	reg := NewRegistry()
	reg.Register("inline", func(ctx *Context) {})
	s := NewServer(reg)

	if got := s.blockingCount.Load(); got != 0 {
		t.Errorf("blockingCount = %d with no blocking methods, want 0", got)
	}

	s.RegisterBlocking("blocking", func(ctx *Context) {})
	if got := s.blockingCount.Load(); got != 1 {
		t.Errorf("blockingCount = %d after RegisterBlocking, want 1", got)
	}

	// Registered straight on the registry, bypassing the server's wrapper.
	reg.RegisterBlocking("another", func(ctx *Context) {})
	s.RefreshBlocking()
	if got := s.blockingCount.Load(); got != 2 {
		t.Errorf("blockingCount = %d after RefreshBlocking, want 2", got)
	}
}

// TestMessageNeedsWorker checks the pre-scan's classification.
func TestMessageNeedsWorker(t *testing.T) {
	reg := NewRegistry()
	reg.Register("fast", func(ctx *Context) {})
	reg.RegisterBlocking("slow", func(ctx *Context) {})
	s := NewServer(reg)

	for name, tc := range map[string]struct {
		msg  string
		want bool
	}{
		"inline single":            {`{"jsonrpc":"2.0","method":"fast","id":1}`, false},
		"blocking single":          {`{"jsonrpc":"2.0","method":"slow","id":1}`, true},
		"unknown method":           {`{"jsonrpc":"2.0","method":"nope","id":1}`, false},
		"batch all inline":         {`[{"jsonrpc":"2.0","method":"fast","id":1}]`, false},
		"batch with one blocking":  {`[{"jsonrpc":"2.0","method":"fast","id":1},{"jsonrpc":"2.0","method":"slow","id":2}]`, true},
		"malformed":                {`{oops`, false},
		"blocking as notification": {`{"jsonrpc":"2.0","method":"slow"}`, true},
	} {
		t.Run(name, func(t *testing.T) {
			if got := s.messageNeedsWorker([]byte(tc.msg)); got != tc.want {
				t.Errorf("messageNeedsWorker(%s) = %v, want %v", tc.msg, got, tc.want)
			}
		})
	}
}
