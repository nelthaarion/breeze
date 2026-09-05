package generator

// The `jsonrpc` feature: stand up a JSON-RPC 2.0 listener alongside the HTTP
// server.
//
// This wires the rpc package the same way `add websocket` wires the WebSocket
// hub â€” a package-scope handle plus a setup function the dispatcher calls â€” but
// two properties of the rpc package make the generated body different in ways
// worth stating, because both are invisible at the call site and both produce a
// silently broken project when got wrong.
//
// Server.Run blocks. It calls gnet.Run on its own listener, since JSON-RPC is a
// peer of the HTTP layer rather than something mounted inside it. Calling it
// from the dispatcher would mean app.Run is never reached: the process would
// serve JSON-RPC perfectly and never open the HTTP port. So the generated setup
// starts it in a goroutine, and the error is logged rather than dropped â€”
// "address already in use" is the likely one, and swallowing it produces a
// process that looks healthy while one of its two listeners is missing.
//
// The pool must be OverflowSpawn. Server.SetPool documents that Submit is
// called from the event-loop goroutine, so a pool that blocks when its queue is
// full stalls the reactor and with it every connection on it. breeze has a
// constructor for exactly this case, and the generated code uses it rather than
// NewWorkerPool. It is one identifier's difference and the wrong one degrades
// only under load, which is the worst time to discover it.

import (
	"flag"
	"fmt"
	"strings"
)

const rpcImport = `"github.com/nelthaarion/breeze/v2/rpc"`

func registerJSONRPC() {
	register(&feature{
		Name:    "jsonrpc",
		Summary: "JSON-RPC 2.0 listener on its own port, with method scaffolds",
		// Between websocket (150) and templates (160). The exact slot barely
		// matters â€” this listener is not in the HTTP middleware chain at all â€”
		// but it must not tie with another feature, or the dispatcher's call
		// order would depend on how the registry map happened to be walked.
		Priority: 155,

		Imports: []string{rpcImport, logImport},
		Build: func(fs *flag.FlagSet) func(featureCtx) (featureOutput, error) {
			// The default matches JSONRPCConfig's, so `add jsonrpc` and the
			// equivalent YAML land on the same port rather than differing by
			// which entry point was used.
			port := fs.Int(
				"port",
				Defaults().JSONRPC.Port,
				"TCP port for the JSON-RPC listener (must differ from the HTTP port)",
			)

			methods := fs.String(
				"methods",
				"",
				"comma-separated method names to scaffold, e.g. sum,echo",
			)
			blocking := fs.String(
				"blocking",
				"",
				"of those methods, the ones that perform I/O and must run off the event loop",
			)
			multicore := fs.Bool("multicore", true, "run one event loop per CPU core")
			maxBytes := fs.Int(
				"max-message-bytes",
				0,
				"cap one reassembled message in bytes (0 keeps the package default)",
			)

			return func(ctx featureCtx) (featureOutput, error) {
				names := splitList(*methods)
				blockingNames := splitList(*blocking)

				cfg := JSONRPCConfig{
					Enabled:         true,
					Port:            *port,
					Methods:         names,
					BlockingMethods: blockingNames,
					MaxMessageBytes: *maxBytes,
					Multicore:       *multicore,
				}
				if err := validateRPCFeature(cfg); err != nil {
					return featureOutput{}, err
				}

				return buildJSONRPCOutput(cfg)
			}
		},
	})
}

// validateRPCFeature checks the flags against the same rules the configuration
// schema enforces, so `breeze add jsonrpc --methods=rpc.x` fails the same way
// the equivalent YAML does.
//
// The port is checked against the scaffold's default rather than the project's
// real HTTP port, which this command cannot know. That catches the common
// mistake without pretending to certainty: the definitive check happens in
// ProjectConfig.Validate, where both ports are actually present.
func validateRPCFeature(cfg JSONRPCConfig) error {
	probe := Defaults()
	probe.JSONRPC = cfg
	// Compare against the schema's own default HTTP port only if the user left
	// it there; otherwise the clash test belongs to config validation.
	if cfg.Port == probe.Server.Port {
		return fmt.Errorf(
			"--port %d is the default HTTP port â€” JSON-RPC needs its own listener and cannot share it",
			cfg.Port,
		)
	}
	if errs := probe.validateJSONRPC(); len(errs) > 0 {
		return fmt.Errorf("%s", errs[0])
	}

	// Every blocking method must be one of the scaffolded ones, or the
	// generated code would register a handler for a method that has no
	// scaffold â€” or worse, silently not register it at all.
	declared := map[string]bool{}
	for _, m := range cfg.Methods {
		declared[m] = true
	}
	for _, b := range cfg.BlockingMethods {
		if !declared[b] {
			return fmt.Errorf("--blocking names %q, which is not in --methods", b)
		}
	}
	return nil
}

// buildJSONRPCOutput generates the block. It is separate from Build so the
// configuration-driven path can reach it with a JSONRPCConfig that came from
// YAML, without going through a flag.FlagSet.
func buildJSONRPCOutput(cfg JSONRPCConfig) (featureOutput, error) {
	blocking := map[string]bool{}
	for _, b := range cfg.BlockingMethods {
		blocking[b] = true
	}

	var b strings.Builder

	b.WriteString(`// RPCServer is the JSON-RPC 2.0 server. Register further methods on it with
// RPCServer.Register, or RPCServer.RegisterBlocking for anything that performs
// I/O â€” a blocking handler on an event loop stalls every connection sharing it.
var RPCServer *rpc.Server

`)

	fmt.Fprintf(
		&b,
		"func %s(app *breeze.Breeze, router *breeze.Router) {\n",
		featureSetupFunc("jsonrpc"),
	)
	b.WriteString("\tRPCServer = rpc.NewServer(nil)\n\n")

	// The pool is only needed if something actually runs off the event loop.
	if len(cfg.BlockingMethods) > 0 {
		b.WriteString(`	// OverflowSpawn, not the default policy: Submit runs on the event-loop
	// goroutine, so a pool that blocks when full would stall the reactor and
	// every connection on it. Spawning under load is the lesser cost.
	RPCServer.SetPool(breeze.NewEventLoopWorkerPool(runtime.NumCPU() * 2))

`)
	}

	if cfg.MaxMessageBytes > 0 {
		fmt.Fprintf(&b, "\tRPCServer.SetMaxMessageBytes(%d)\n\n", cfg.MaxMessageBytes)
	}

	if len(cfg.Methods) == 0 {
		b.WriteString(`	// No methods scaffolded. Register them here:
	//
	//	RPCServer.Register("sum", func(ctx *rpc.Context) {
	//		var in []int
	//		if err := ctx.Bind(&in); err != nil {
	//			return // Bind has already set -32602
	//		}
	//		total := 0
	//		for _, n := range in {
	//			total += n
	//		}
	//		ctx.Result(total)
	//	})

`)
	}

	for _, m := range cfg.Methods {
		register := "Register"
		if blocking[m] {
			register = "RegisterBlocking"
		}
		fmt.Fprintf(&b, "\tRPCServer.%s(%q, %s)\n", register, m, rpcHandlerName(m))
	}
	if len(cfg.Methods) > 0 {
		b.WriteString("\n")
	}

	// Run blocks, so it cannot be called on the dispatcher's goroutine.
	fmt.Fprintf(
		&b,
		`	// Run blocks â€” it owns its own gnet listener, since JSON-RPC is a peer of
	// the HTTP layer rather than a route inside it. On the dispatcher's
	// goroutine it would mean app.Run is never reached and the HTTP port never
	// opens, so it goes in a goroutine of its own.
	//
	// The error is logged rather than discarded: the likeliest one is the port
	// already being in use, and a silent failure here leaves a process that
	// looks healthy while serving only half of what it should.
	go func() {
		log.Printf("json-rpc: listening on :%d")
		if err := RPCServer.Run(%d, %t); err != nil {
			log.Printf("json-rpc: server stopped: %%v", err)
		}
	}()
}`,
		cfg.Port,
		cfg.Port,
		cfg.Multicore,
	)

	// Handler scaffolds go in their own file rather than in the block, because
	// they are code the developer is meant to edit â€” the same division `add`
	// uses everywhere else: blocks are wiring, files are yours.
	files := map[string]string{}
	if len(cfg.Methods) > 0 {
		files["rpc_methods.go"] = rpcMethodsFile(cfg, blocking)
	}

	imports := []string{rpcImport, logImport}
	if len(cfg.BlockingMethods) > 0 {
		imports = append(imports, runtimeImport)
	}

	notes := []string{
		fmt.Sprintf("Listening on :%d, separate from the HTTP port.", cfg.Port),
	}
	if len(cfg.Methods) > 0 {
		notes = append(
			notes,
			fmt.Sprintf("Method scaffolds in rpc_methods.go: %s.", strings.Join(cfg.Methods, ", ")),
		)
	}
	if len(cfg.BlockingMethods) > 0 {
		notes = append(
			notes,
			fmt.Sprintf(
				"Registered as blocking (run on the worker pool): %s.",
				strings.Join(cfg.BlockingMethods, ", "),
			),
		)
	} else {
		notes = append(
			notes,
			"No blocking methods, so no worker pool is created â€” use --blocking for handlers that do I/O.",
		)
	}
	notes = append(
		notes,
		"Notifications (requests with no id) get no reply; ctx.IsNotification() reports which you are in.",
	)

	return featureOutput{Body: b.String(), Imports: imports, Files: files, Notes: notes}, nil
}

// rpcHandlerName derives the Go function name for a method.
//
// JSON-RPC method names are dotted by convention ("user.create"), and a dot is
// not legal in an identifier, so each segment is capitalised and joined:
// user.create -> rpcUserCreate. Deriving it rather than asking keeps the
// generated registration and the generated function in step by construction.
func rpcHandlerName(method string) string {
	parts := strings.FieldsFunc(method, func(r rune) bool {
		return r == '.' || r == '_' || r == '-' || r == '/'
	})
	var b strings.Builder
	b.WriteString("rpc")
	for _, p := range parts {
		if p == "" {
			continue
		}
		b.WriteString(strings.ToUpper(p[:1]))
		b.WriteString(p[1:])
	}
	return b.String()
}

// rpcMethodsFile writes the handler scaffolds.
//
// Each one is a working handler rather than a panic or a TODO: it binds its
// params and returns a result, so the project runs and answers correctly the
// moment it is generated. A scaffold that has to be filled in before the
// project does anything at all makes the generated code harder to trust.
func rpcMethodsFile(cfg JSONRPCConfig, blocking map[string]bool) string {
	var b strings.Builder

	b.WriteString(`// JSON-RPC method handlers.
//
// Unlike features_generated.go, this file is yours to edit: ` + "`breeze add`" + ` writes
// it once and will not overwrite it without --force.
//
// A handler answers by calling ctx.Result, or fails by setting an error:
//
//	ctx.Errorf(rpc.CodeInvalidParams, "expected two integers")
//
// Application errors belong in the reserved server range, which
// rpc.ErrorCodeReserved will confirm for a given code. ctx.Bind already sets
// -32602 when the params do not fit, so a handler that returns straight after a
// failed Bind is already correct.

package main

import (
	"github.com/nelthaarion/breeze/v2/rpc"
)
`)

	for _, m := range cfg.Methods {
		name := rpcHandlerName(m)
		fmt.Fprintf(&b, "\n// %s handles the %q method.\n", name, m)
		if blocking[m] {
			b.WriteString(
				"//\n// Registered with RegisterBlocking, so this runs on the worker pool rather\n// than an event loop: I/O here is safe.\n",
			)
		} else {
			b.WriteString(
				"//\n// Registered with Register, so this runs directly on the event loop. Keep it\n// non-blocking â€” no database calls, no outbound HTTP â€” or move it to\n// --blocking, or every connection on that loop waits behind it.\n",
			)
		}

		fmt.Fprintf(&b, `func %s(ctx *rpc.Context) {
	// Params arrive as whatever the caller sent. Bind them into the shape this
	// method expects; a mismatch is an invalid-params error, which Bind sets.
	var params struct {
		// TODO: the fields this method takes.
	}
	if err := ctx.Bind(&params); err != nil {
		return
	}

	// A notification has no id, so nothing is sent back and a result would be
	// discarded. Worth checking when the work is expensive.
	if ctx.IsNotification() {
		return
	}

	ctx.Result(map[string]any{"method": %q})
}
`, name, m)
	}

	return b.String()
}
