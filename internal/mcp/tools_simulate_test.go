package mcp_test

// tools_simulate_test.go — breeze_simulate_request against a service that is
// actually serving.
//
// This tool is the one that closes the loop. Everything else in this package
// reads a project's files or a running service's telemetry; this one asks the
// service a question and reports what it said. A test that pointed it at a stub
// would prove that the client library works, which is not in doubt. What is in
// doubt is whether the readings this tool reports match what a real Breeze router
// really does — so these tests boot a real app and drive real requests.
//
// This is package mcp_test for the same reason tools_live_test.go is: the root
// breeze package is needed to boot an app, and Part 4 makes the root package
// import internal/mcp, so an in-package test would close an import cycle.
//
// The fixture is separate from the dashboard fixture in tools_live_test.go and
// deliberately has no dashboard installed. Two reasons. A dashboard would add
// middleware between the router and the handler, and the point here is what the
// router and handler do on their own. And the absence of a dashboard is itself
// worth covering: simulate_request is the only live tool that works without one,
// which is what makes it usable on a service that has not been instrumented yet.

import (
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nelthaarion/breeze"
)

const (
	simUser     = "simadmin"
	simPassword = "simpassword"
	simToken    = "sim-bearer-token"
)

type simFixture struct {
	url   string
	ready bool
}

var (
	simOnce    sync.Once
	simService simFixture
)

// startSimFixture boots one plain service for the whole package.
//
// Breeze has Run but no Stop, so a booted app owns its port for the rest of the
// process; every test below shares this one rather than leaking an event loop
// each.
func startSimFixture(t *testing.T) simFixture {
	t.Helper()

	simOnce.Do(func() {
		port := freeLivePort(t)

		router := breeze.NewRouter()
		app := breeze.New(router, breeze.NewEventLoopWorkerPool(runtime.NumCPU()))

		// A plain read.
		router.Handle(breeze.GET, "/api/ping", func(ctx *breeze.Context) error {
			return ctx.JSON(map[string]any{"pong": true})
		})

		// Echoes what it was sent, so a test can prove the body and the headers
		// arrived rather than merely that a 200 came back. A tool that dropped
		// the body would still return 200 from a handler that ignored it.
		//
		// The destination is a struct because that is what Bind requires — it
		// rejects a map with "dst must point to a struct". Binding into a struct
		// is also the stricter test: a field that did not arrive stays at its
		// zero value rather than merely being absent from a map.
		router.Handle(breeze.POST, "/api/echo", func(ctx *breeze.Context) error {
			var payload struct {
				Name  string `json:"name"`
				Count int    `json:"count"`
			}
			if err := ctx.Bind(&payload); err != nil {
				ctx.Status(400)
				return ctx.JSON(map[string]any{"error": "body did not bind: " + err.Error()})
			}
			return ctx.JSON(map[string]any{"received": map[string]any{
				"name":  payload.Name,
				"count": payload.Count,
			}})
		})

		// Requires credentials. This is how the tool's auth handling is checked
		// against a real middleware decision instead of against its own report.
		router.Handle(breeze.GET, "/api/private", func(ctx *breeze.Context) error {
			auth := ""
			if ctx.Req != nil {
				// The parser lowercases header keys.
				auth = ctx.Req.Header["authorization"]
			}
			switch {
			case strings.HasPrefix(auth, "Bearer "):
				_ = ctx.JSON(map[string]any{"scheme": "bearer", "credential": strings.TrimPrefix(auth, "Bearer ")})
			case strings.HasPrefix(auth, "Basic "):
				_ = ctx.JSON(map[string]any{"scheme": "basic"})
			default:
				ctx.Status(401)
				_ = ctx.JSON(map[string]any{"error": "credentials required"})
			}

			return nil
		})

		// Claims JSON and writes something that is not. A handler that does this
		// is a real bug, and the tool has to name it rather than pass it on.
		//
		// The order matters and is not interchangeable: WriteString replaces the
		// response headers wholesale with text/plain and takes a pre-rendered
		// header fast path, so setting Content-Type before it has no effect.
		// SetHeader afterwards discards that fast path and copies the shared map
		// before mutating it, which is what actually produces the mismatch this
		// route exists to create.
		router.Handle(breeze.GET, "/api/liar", func(ctx *breeze.Context) error {
			_ = ctx.WriteString("this is not JSON")
			ctx.SetHeader("Content-Type", "application/json")

			return nil
		})

		router.Handle(breeze.GET, "/api/boom", func(ctx *breeze.Context) error {
			ctx.Status(500)
			return ctx.JSON(map[string]any{"error": "deliberate failure"})
		})

		go func() {
			_ = app.Run(port, false)
		}()

		waitForLivePort(t, port, 10*time.Second)

		simService = simFixture{
			url:   "http://127.0.0.1:" + strconv.Itoa(port),
			ready: true,
		}
	})

	if !simService.ready {
		t.Fatal("the simulate fixture failed to start")
	}
	return simService
}

// simulate is a shorthand for the call under test.
func simulate(t *testing.T, args map[string]any) (map[string]any, string, bool) {
	t.Helper()
	return callLiveTool(t, "breeze_simulate_request", args)
}

// TestSimulateRequestReportsARealSuccessfulRoute is the control: without it, a
// tool that reported everything as unreachable would pass the negative tests.
func TestSimulateRequestReportsARealSuccessfulRoute(t *testing.T) {
	svc := startSimFixture(t)

	report, summary, isErr := simulate(t, map[string]any{
		"service_url": svc.url,
		"path":        "/api/ping",
	})
	if isErr {
		t.Fatalf("a working route was reported as an error: %s", summary)
	}

	if status, _ := report["status"].(float64); int(status) != 200 {
		t.Errorf("status = %v, want 200", report["status"])
	}
	if reading, _ := report["reading"].(string); reading != "routed" {
		t.Errorf("reading = %q, want routed", reading)
	}

	// The decoded body is the field an agent can branch on. Returning only the
	// raw text would make it parse a JSON string that is already inside a JSON
	// result.
	body, ok := report["json_body"].(map[string]any)
	if !ok {
		t.Fatalf("json_body is %T, not a decoded object: %v", report["json_body"], report["json_body"])
	}
	if pong, _ := body["pong"].(bool); !pong {
		t.Errorf("json_body = %v, want the handler's {\"pong\":true}", body)
	}

	if _, present := report["duration_ms"]; !present {
		t.Error("no duration_ms was reported")
	}
}

// TestSimulateRequestSendsTheBodyItWasGiven.
//
// The echo handler binds the body and returns it. If the tool dropped the body,
// binding would fail and this would be a 400 — so the assertion is on the
// round-trip, not on the status alone.
func TestSimulateRequestSendsTheBodyItWasGiven(t *testing.T) {
	svc := startSimFixture(t)

	report, summary, isErr := simulate(t, map[string]any{
		"service_url": svc.url,
		"method":      "POST",
		"path":        "/api/echo",
		"body":        `{"name":"widget","count":3}`,
	})
	if isErr {
		t.Fatalf("posting a body failed: %s", summary)
	}
	if status, _ := report["status"].(float64); int(status) != 200 {
		t.Fatalf("status = %v, want 200 — the body did not bind: %v",
			report["status"], report["json_body"])
	}

	body, _ := report["json_body"].(map[string]any)
	received, ok := body["received"].(map[string]any)
	if !ok {
		t.Fatalf("the handler did not echo a body: %v", body)
	}
	if name, _ := received["name"].(string); name != "widget" {
		t.Errorf("the handler received name = %q, want widget", name)
	}

	// A JSON body with no declared type is sent as JSON, because a framework
	// whose binding is JSON-first would otherwise reject a correct request for a
	// reason the caller never stated.
	sent := strings.Join(stringsOf(report["sent_headers"]), ",")
	if !strings.Contains(strings.ToLower(sent), "content-type") {
		t.Errorf("no Content-Type was sent with a JSON body; sent_headers = %q", sent)
	}
}

// TestSimulateRequestDistinguishesMissingFromRejected.
//
// "404" and "500" are both failures to a caller reading a status code, and they
// call for opposite work: one means the route was never registered, the other
// means it was and the handler broke. The reading field is what carries that
// difference.
func TestSimulateRequestDistinguishesMissingFromRejected(t *testing.T) {
	svc := startSimFixture(t)

	t.Run("a route that does not exist", func(t *testing.T) {
		report, _, _ := simulate(t, map[string]any{
			"service_url": svc.url,
			"path":        "/api/there-is-no-such-route",
		})
		if status, _ := report["status"].(float64); int(status) != 404 {
			t.Errorf("status = %v, want 404", report["status"])
		}
		if reading, _ := report["reading"].(string); reading != "not-found" {
			t.Errorf("reading = %q, want not-found", reading)
		}
		// The note has to point somewhere. A bare "not found" leaves an agent
		// guessing whether the route is absent or the path is misspelled.
		if notes := notesText(report); !strings.Contains(notes, "registered") {
			t.Errorf("a 404 came back with no explanation: %q", notes)
		}
	})

	t.Run("a handler that fails", func(t *testing.T) {
		report, _, _ := simulate(t, map[string]any{
			"service_url": svc.url,
			"path":        "/api/boom",
		})
		if status, _ := report["status"].(float64); int(status) != 500 {
			t.Errorf("status = %v, want 500", report["status"])
		}
		if reading, _ := report["reading"].(string); reading != "server-error" {
			t.Errorf("reading = %q, want server-error", reading)
		}
	})

	// A method the router has no entry for. Breeze's router matches on method
	// and path together and has no 405 branch, so this is a 404 — asserted here
	// so that if the router ever grows one, this test says so rather than
	// silently passing on the wrong reading.
	t.Run("a method the route does not serve", func(t *testing.T) {
		report, _, _ := simulate(t, map[string]any{
			"service_url": svc.url,
			"method":      "DELETE",
			"path":        "/api/ping",
		})
		status, _ := report["status"].(float64)
		reading, _ := report["reading"].(string)

		switch int(status) {
		case 404:
			if reading != "not-found" {
				t.Errorf("reading = %q for a 404", reading)
			}
		case 405:
			if reading != "wrong-method" {
				t.Errorf("reading = %q for a 405", reading)
			}
		default:
			t.Errorf("status = %v, want 404 or 405", status)
		}
	})
}

// TestSimulateRequestIsNotAToolErrorWhenTheServiceSaysNo.
//
// A 401 is a correct answer to the question that was asked. Flagging it as a
// tool failure would make an agent retry a request that will keep giving the
// same right answer, and the retry is the expensive kind of wrong.
func TestSimulateRequestIsNotAToolErrorWhenTheServiceSaysNo(t *testing.T) {
	svc := startSimFixture(t)

	report, _, isErr := simulate(t, map[string]any{
		"service_url": svc.url,
		"path":        "/api/private",
	})
	if isErr {
		t.Error("a 401 from the service was reported as a failure of the tool")
	}
	if status, _ := report["status"].(float64); int(status) != 401 {
		t.Fatalf("status = %v, want 401", report["status"])
	}
	if reading, _ := report["reading"].(string); reading != "unauthorized" {
		t.Errorf("reading = %q, want unauthorized", reading)
	}
}

// TestSimulateRequestSendsCredentialsInTheSchemeItWasAskedFor.
//
// The handler reports which scheme it saw, so this checks the bytes that arrived
// rather than the tool's own account of what it sent.
func TestSimulateRequestSendsCredentialsInTheSchemeItWasAskedFor(t *testing.T) {
	svc := startSimFixture(t)

	t.Run("bearer", func(t *testing.T) {
		report, summary, isErr := simulate(t, map[string]any{
			"service_url": svc.url,
			"path":        "/api/private",
			"token":       simToken,
		})
		if isErr {
			t.Fatalf("a token was rejected: %s", summary)
		}
		body, _ := report["json_body"].(map[string]any)
		if scheme, _ := body["scheme"].(string); scheme != "bearer" {
			t.Errorf("the service saw scheme %q, want bearer", scheme)
		}
		if got, _ := body["credential"].(string); got != simToken {
			t.Errorf("the service received token %q, want %q", got, simToken)
		}
	})

	t.Run("basic", func(t *testing.T) {
		report, summary, isErr := simulate(t, map[string]any{
			"service_url": svc.url,
			"path":        "/api/private",
			"username":    simUser,
			"password":    simPassword,
		})
		if isErr {
			t.Fatalf("basic credentials were rejected: %s", summary)
		}
		body, _ := report["json_body"].(map[string]any)
		if scheme, _ := body["scheme"].(string); scheme != "basic" {
			t.Errorf("the service saw scheme %q, want basic", scheme)
		}
	})

	// Both schemes write the same header, so accepting both would silently send
	// one and ignore the other — and the caller would be debugging the service.
	t.Run("both at once is refused before anything is sent", func(t *testing.T) {
		_, summary, isErr := simulate(t, map[string]any{
			"service_url": svc.url,
			"path":        "/api/private",
			"token":       simToken,
			"username":    simUser,
			"password":    simPassword,
		})
		if !isErr {
			t.Fatal("a token and a username/password together were accepted")
		}
		if !strings.Contains(summary, "Authorization") {
			t.Errorf("the refusal does not say why the two conflict: %q", summary)
		}
	})
}

// TestSimulateRequestNeverEchoesCredentialValues.
//
// The report names the headers it sent so a caller can confirm a credential was
// attached. It must not carry the value: these results are transcript material
// and often end up in a log or a model's context.
func TestSimulateRequestNeverEchoesCredentialValues(t *testing.T) {
	svc := startSimFixture(t)

	report, summary, _ := simulate(t, map[string]any{
		"service_url": svc.url,
		"path":        "/api/private",
		"token":       simToken,
	})

	sent := stringsOf(report["sent_headers"])
	joined := strings.ToLower(strings.Join(sent, ","))
	if !strings.Contains(joined, "authorization") {
		t.Errorf("sent_headers does not record that a credential was attached: %v", sent)
	}
	for _, name := range sent {
		if strings.Contains(name, simToken) {
			t.Errorf("sent_headers leaked the credential: %q", name)
		}
	}
	// Only the first line is this tool's own prose. The rest of the text half is
	// the rendered payload, which here contains the token because the fixture
	// handler deliberately echoes it back — reporting a response body faithfully
	// is the tool's job, so asserting over the whole blob would be asserting
	// that the fixture is quiet rather than that the tool is careful.
	composed := summary
	if newline := strings.IndexByte(summary, '\n'); newline >= 0 {
		composed = summary[:newline]
	}
	if strings.Contains(composed, simToken) {
		t.Errorf("the summary line leaked the credential: %q", composed)
	}

	// The request headers the tool recorded must never carry values, whatever
	// the response happened to contain.
	if headers, ok := report["headers"].(map[string]any); ok {
		if _, present := headers["authorization"]; present {
			t.Error("the report echoed an Authorization header back as response data")
		}
	}
}

// TestSimulateRequestFlagsABodyThatContradictsItsContentType.
//
// A handler that sets application/json and then writes something else is a real
// bug, most often a header set before a failure. Passing the text through with
// no remark would leave the caller thinking the parser was at fault.
func TestSimulateRequestFlagsABodyThatContradictsItsContentType(t *testing.T) {
	svc := startSimFixture(t)

	report, _, _ := simulate(t, map[string]any{
		"service_url": svc.url,
		"path":        "/api/liar",
	})
	if status, _ := report["status"].(float64); int(status) != 200 {
		t.Fatalf("status = %v, want 200 — this fixture answers, badly", report["status"])
	}
	if _, decoded := report["json_body"]; decoded {
		t.Error("a body that is not JSON was reported as decoded JSON")
	}
	if notes := notesText(report); !strings.Contains(notes, "does not parse") {
		t.Errorf("the mismatch was not reported: %q", notes)
	}
	// The undecodable text still has to come back, or there is nothing to look
	// at when deciding what the handler actually wrote.
	if body, _ := report["body"].(string); !strings.Contains(body, "not JSON") {
		t.Errorf("the raw body was dropped: %q", body)
	}
}

// TestSimulateRequestSaysNothingWasSentWhenItCouldNotConnect.
//
// This is the reading most likely to be misdiagnosed: an agent that reads a
// connection failure as a 404 will start editing routes to fix a service that
// is not running.
func TestSimulateRequestSaysNothingWasSentWhenItCouldNotConnect(t *testing.T) {
	// A port that was reserved and released, so nothing is listening on it.
	dead := "http://127.0.0.1:" + strconv.Itoa(freeLivePort(t))

	report, summary, isErr := simulate(t, map[string]any{
		"service_url": dead,
		"path":        "/api/ping",
	})
	if !isErr {
		t.Error("an unreachable service was reported as a successful call")
	}
	if kind, _ := report["kind"].(string); kind != "unreachable" {
		t.Errorf("kind = %q, want unreachable", kind)
	}
	if _, present := report["status"]; present {
		t.Error("a status was reported for a request that was never answered")
	}
	note, _ := report["note"].(string)
	if !strings.Contains(note, "nothing was sent") {
		t.Errorf("the result does not make clear that no request reached a handler: %q", note)
	}
	if !strings.Contains(summary, "could not reach") {
		t.Errorf("summary = %q", summary)
	}
}

// TestSimulateRequestRejectsArgumentsItCannotHonour.
//
// Each of these is refused before anything is sent, because a request built from
// a misunderstanding produces a status code the caller will then try to explain.
func TestSimulateRequestRejectsArgumentsItCannotHonour(t *testing.T) {
	svc := startSimFixture(t)

	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{
			name: "a method the tool cannot send",
			args: map[string]any{"service_url": svc.url, "path": "/api/ping", "method": "TRACE"},
			want: "not a method",
		},
		{
			name: "no path at all",
			args: map[string]any{"service_url": svc.url, "path": ""},
			want: "path is required",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, summary, isErr := simulate(t, tc.args)
			if !isErr {
				t.Fatalf("%s was accepted", tc.name)
			}
			if !strings.Contains(summary, tc.want) {
				t.Errorf("the refusal does not explain itself: %q", summary)
			}
		})
	}
}

// stringsOf reads a JSON list of strings.
func stringsOf(node any) []string {
	raw, ok := node.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, entry := range raw {
		if text, ok := entry.(string); ok {
			out = append(out, text)
		}
	}
	return out
}
