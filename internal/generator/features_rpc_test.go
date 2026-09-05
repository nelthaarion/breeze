package generator

// Tests for the jsonrpc feature generator.
//
// The end-to-end test compiles a project containing this block, which is what
// catches a call to an rpc API that does not exist. What it cannot catch is
// generated code that compiles and is wrong: `RPCServer.Run(...)` called
// directly rather than in a goroutine compiles perfectly and produces a process
// that serves JSON-RPC and never opens its HTTP port. A blocking method
// registered with Register instead of RegisterBlocking compiles perfectly and
// stalls an event loop under load.
//
// Neither shows up in a build, and the second does not show up in a smoke test
// either. So the properties are asserted directly on the emitted source.

import (
	"go/format"
	"strings"
	"testing"
)

func buildRPC(t *testing.T, cfg JSONRPCConfig) featureOutput {
	t.Helper()
	cfg.Enabled = true
	if cfg.Port == 0 {
		cfg.Port = Defaults().JSONRPC.Port
	}
	out, err := buildJSONRPCOutput(cfg)
	if err != nil {
		t.Fatalf("buildJSONRPCOutput: %v", err)
	}
	return out
}

// TestGeneratedRunIsInAGoroutine is the one that matters most.
//
// Server.Run blocks. The dispatcher calls every setup function in sequence and
// then main calls app.Run, so a direct call here means the HTTP listener is
// never reached â€” and the project compiles, starts, and answers JSON-RPC, so
// nothing about it looks broken until someone tries the HTTP port.
func TestGeneratedRunIsInAGoroutine(t *testing.T) {
	body := buildRPC(t, JSONRPCConfig{}).Body

	if !strings.Contains(body, "go func() {") {
		t.Fatalf("Run is not started in a goroutine:\n%s", body)
	}

	// Assert the ordering, not just the presence of both: `go func()` appearing
	// somewhere else in the body would satisfy a contains-check while Run was
	// still called inline.
	goAt := strings.Index(body, "go func() {")
	runAt := strings.Index(body, "RPCServer.Run(")
	if runAt < goAt {
		t.Errorf("RPCServer.Run is called before the goroutine starts, so it would block the dispatcher:\n%s", body)
	}
}

// TestGeneratedRunErrorIsReported â€” a listener that failed to bind must say so.
func TestGeneratedRunErrorIsReported(t *testing.T) {
	body := buildRPC(t, JSONRPCConfig{}).Body
	if !strings.Contains(body, "if err := RPCServer.Run(") {
		t.Errorf("Run's error is discarded, so a failed bind would be silent:\n%s", body)
	}
}

// TestPoolOnlyWhenBlockingMethodsExist â€” the pool is what makes blocking
// methods safe, and carrying one for a project with none is dead weight.
func TestPoolOnlyWhenBlockingMethodsExist(t *testing.T) {
	plain := buildRPC(t, JSONRPCConfig{Methods: []string{"sum"}})
	if strings.Contains(plain.Body, "SetPool") {
		t.Error("a pool was created for a project with no blocking methods")
	}
	if containsString(plain.Imports, runtimeImport) {
		t.Error("runtime was imported but not used â€” that does not compile")
	}

	withBlocking := buildRPC(t, JSONRPCConfig{
		Methods:         []string{"sum", "db.query"},
		BlockingMethods: []string{"db.query"},
	})
	if !strings.Contains(withBlocking.Body, "SetPool") {
		t.Error("a blocking method was registered with no pool to run it on")
	}
	if !containsString(withBlocking.Imports, runtimeImport) {
		t.Error("runtime.NumCPU is called but runtime is not imported")
	}
}

// TestPoolIsTheEventLoopConstructor.
//
// SetPool's contract is that Submit runs on the event-loop goroutine, so the
// pool must not block when full. NewWorkerPool would compile and behave
// identically until the queue filled, at which point it would stall the reactor
// and every connection on it. The difference is one identifier, and it only
// shows up under load.
func TestPoolIsTheEventLoopConstructor(t *testing.T) {
	body := buildRPC(t, JSONRPCConfig{
		Methods:         []string{"db.query"},
		BlockingMethods: []string{"db.query"},
	}).Body

	if !strings.Contains(body, "breeze.NewEventLoopWorkerPool(") {
		t.Errorf("the pool must come from NewEventLoopWorkerPool (OverflowSpawn), or a full queue stalls the event loop:\n%s", body)
	}
}

// TestBlockingMethodsUseRegisterBlocking â€” the whole point of declaring them.
func TestBlockingMethodsUseRegisterBlocking(t *testing.T) {
	body := buildRPC(t, JSONRPCConfig{
		Methods:         []string{"sum", "db.query"},
		BlockingMethods: []string{"db.query"},
	}).Body

	if !strings.Contains(body, `RPCServer.RegisterBlocking("db.query"`) {
		t.Errorf("db.query was declared blocking but is not registered as such:\n%s", body)
	}
	// And the non-blocking one must not be promoted.
	if !strings.Contains(body, `RPCServer.Register("sum"`) {
		t.Errorf("sum should use plain Register:\n%s", body)
	}
	if strings.Contains(body, `RPCServer.RegisterBlocking("sum"`) {
		t.Error("sum was registered as blocking, which would send it to the pool for no reason")
	}
}

// TestDottedMethodNamesBecomeValidIdentifiers.
//
// Dotted method names are the JSON-RPC convention, and a dot is not legal in a
// Go identifier. The registration and the scaffold have to agree on the derived
// name or the project does not compile.
func TestDottedMethodNamesBecomeValidIdentifiers(t *testing.T) {
	cases := map[string]string{
		"sum":            "rpcSum",
		"user.create":    "rpcUserCreate",
		"a.b.c":          "rpcABC",
		"snake_case":     "rpcSnakeCase",
		"kebab-case":     "rpcKebabCase",
		"path/segmented": "rpcPathSegmented",
	}
	for method, want := range cases {
		if got := rpcHandlerName(method); got != want {
			t.Errorf("rpcHandlerName(%q) = %q, want %q", method, got, want)
		}
	}
}

// TestRegistrationAndScaffoldAgreeOnNames is the property the test above only
// implies: whatever name is derived, both sides must use the same one.
func TestRegistrationAndScaffoldAgreeOnNames(t *testing.T) {
	cfg := JSONRPCConfig{
		Methods:         []string{"sum", "user.create", "db.query"},
		BlockingMethods: []string{"db.query"},
	}
	out := buildRPC(t, cfg)
	methods := out.Files["rpc_methods.go"]

	for _, m := range cfg.Methods {
		name := rpcHandlerName(m)
		if !strings.Contains(out.Body, name) {
			t.Errorf("method %q: registration does not reference %s", m, name)
		}
		if !strings.Contains(methods, "func "+name+"(ctx *rpc.Context)") {
			t.Errorf("method %q: no scaffold declares %s", m, name)
		}
	}
}

// TestGeneratedSourceIsValidGo parses both outputs.
//
// format.Source is what every generator in this package routes through, so a
// syntax error would be caught at generation time â€” but only for the block. The
// scaffold file is written directly, and an unparseable one would surface as a
// compile error in the user's project.
func TestGeneratedSourceIsValidGo(t *testing.T) {
	out := buildRPC(t, JSONRPCConfig{
		Methods:         []string{"sum", "user.create"},
		BlockingMethods: []string{"user.create"},
		MaxMessageBytes: 1 << 20,
	})

	if _, err := format.Source([]byte(out.Files["rpc_methods.go"])); err != nil {
		t.Errorf("the generated scaffold file is not valid Go: %v\n%s", err, out.Files["rpc_methods.go"])
	}

	// The block is a fragment, so it needs a package clause to parse.
	whole := "package main\n\nimport (\n\t\"log\"\n\t\"runtime\"\n\n\t\"github.com/nelthaarion/breeze\"\n\t\"github.com/nelthaarion/breeze/v2/rpc\"\n)\n\n" + out.Body + "\n"
	if _, err := format.Source([]byte(whole)); err != nil {
		t.Errorf("the generated block is not valid Go: %v\n%s", err, whole)
	}
}

// TestSetupFunctionNameMatchesTheDispatcher.
//
// The dispatcher's call list is rebuilt from the feature name alone, so a setup
// function named anything else is generated, compiled, and never called.
func TestSetupFunctionNameMatchesTheDispatcher(t *testing.T) {
	body := buildRPC(t, JSONRPCConfig{}).Body
	want := "func " + featureSetupFunc("jsonrpc") + "(app *breeze.Breeze, router *breeze.Router)"
	if !strings.Contains(body, want) {
		t.Errorf("the block does not declare %q, so the dispatcher would not call it:\n%s", want, body)
	}
}

// TestMaxMessageBytesOnlyWhenSet â€” zero means "keep the package default", and
// emitting SetMaxMessageBytes(0) would say something the user did not ask for.
func TestMaxMessageBytesOnlyWhenSet(t *testing.T) {
	if body := buildRPC(t, JSONRPCConfig{}).Body; strings.Contains(body, "SetMaxMessageBytes") {
		t.Error("SetMaxMessageBytes was emitted for an unset limit")
	}
	body := buildRPC(t, JSONRPCConfig{MaxMessageBytes: 65536}).Body
	if !strings.Contains(body, "SetMaxMessageBytes(65536)") {
		t.Errorf("the configured limit is missing:\n%s", body)
	}
}

// TestNoMethodsStillProducesAWorkingBlock â€” `add jsonrpc` with no methods is a
// reasonable thing to do, and must not emit an empty file or a dangling import.
func TestNoMethodsStillProducesAWorkingBlock(t *testing.T) {
	out := buildRPC(t, JSONRPCConfig{})

	if _, ok := out.Files["rpc_methods.go"]; ok {
		t.Error("a scaffold file was written for a project with no methods")
	}
	if !strings.Contains(out.Body, "rpc.NewServer(nil)") {
		t.Errorf("no server is constructed:\n%s", out.Body)
	}
	// The empty case has to teach the next step, since there is no scaffold to
	// read.
	if !strings.Contains(out.Body, "RPCServer.Register(") {
		t.Error("the empty block does not show how to register a method")
	}
}

// â”€â”€â”€ Validation â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

func TestRPCFeatureValidationRejects(t *testing.T) {
	cases := map[string]JSONRPCConfig{
		"reserved rpc. prefix":  {Port: 9090, Methods: []string{"rpc.internal"}},
		"clashes with HTTP":     {Port: Defaults().Server.Port},
		"blocking not declared": {Port: 9090, Methods: []string{"sum"}, BlockingMethods: []string{"db.query"}},
		"duplicate method":      {Port: 9090, Methods: []string{"sum", "sum"}},
		"port out of range":     {Port: 70000},
		"negative max bytes":    {Port: 9090, MaxMessageBytes: -1},
	}

	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			cfg.Enabled = true
			if err := validateRPCFeature(cfg); err == nil {
				t.Error("accepted an invalid configuration")
			}
		})
	}
}

// TestBlockingSubsetRuleIsEnforcedOnBothPaths.
//
// The flag path and the YAML path are separate entry points into the same
// generator, and a rule enforced in only one of them is a rule that can be
// bypassed by choosing the other spelling.
func TestBlockingSubsetRuleIsEnforcedOnBothPaths(t *testing.T) {
	bad := JSONRPCConfig{Enabled: true, Port: 9090, Methods: []string{"sum"}, BlockingMethods: []string{"ghost"}}

	if err := validateRPCFeature(bad); err == nil {
		t.Error("the flag path accepted a blocking method that is not declared")
	}

	cfg := Defaults()
	cfg.JSONRPC = bad
	if err := cfg.Validate(); err == nil {
		t.Error("the YAML path accepted a blocking method that is not declared")
	}
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
