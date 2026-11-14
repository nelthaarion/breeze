package generator

// features_mcp.go — `breeze add mcp`: the application's own in-process MCP endpoint.
//
// Until this existed the only way to turn the endpoint on was to read
// cmd/fleet-example/gateway and copy its startControlPlane into main. That is a
// reasonable thing to ask of someone who wants to embed it, and an unreasonable one
// to ask of an agent that has just built a service through this same generator and
// has no other way to make it observable.
//
// What the block wires is deliberately narrow, and each narrowing is a decision the
// framework already made, not one made here:
//
//   - ModeAppRuntime, always. A deployed service has no source tree for the
//     generating and provisioning tools to work on, and in that mode they are not
//     registered at all, so there is nothing for a leaked token to reach. The block
//     never sets AllowWorkspaceTools, and a flag for it would be a way to generate a
//     service that rewrites itself while serving requests.
//   - Everything operational comes from the environment, not from flags, so one
//     build is correct in every deployment and no generated file has to be edited to
//     move from a laptop to a container.
//   - A scope that does not parse stops the process. Falling back to unscoped would
//     turn a typo into a wider credential than the one the operator asked for.
//
// The block starts after every other feature (see Priority), because what the
// endpoint reports on is whatever has registered a diagnostic by then.

import (
	"flag"
	"fmt"
	"strconv"
	"strings"
)

const (
	mcpImport     = `"github.com/nelthaarion/breeze/v2/mcp"`
	strconvImport = `"strconv"`

	// mcpDefaultPort is the control port when neither --port nor BREEZE_MCP_PORT says
	// otherwise. It is never the application's own port: conflating the two is the
	// mistake the in-process documentation is written against.
	mcpDefaultPort = 2000
)

func registerMCPFeature() {
	register(&feature{
		Name:    "mcp",
		Summary: "in-process MCP endpoint: a read-only window onto this running service, for an AI agent",
		// Last of the features that describe the service, ahead only of the
		// standalone migrator. Starting the endpoint is what turns counted
		// diagnostics on and what registers the endpoint's own probe; doing it after
		// the other blocks means every subsystem they created is already reporting.
		Priority: 220,

		// No DependsOn. The block is the same whether or not a dashboard is present:
		// the endpoint works either way, and what differs is how much of it has
		// anything to read, which is advice (a note), not code.
		Imports: []string{mcpImport, osImport, logImport, strconvImport},

		Build: func(fs *flag.FlagSet) func(featureCtx) (featureOutput, error) {
			port := fs.Int("port", mcpDefaultPort, "default control port; BREEZE_MCP_PORT overrides it at run time (never the application's own port)")

			return func(ctx featureCtx) (featureOutput, error) {
				if *port < 1 || *port > 65535 {
					return featureOutput{}, fmt.Errorf("--port %d is not a port; use 1-65535. "+
						"BREEZE_MCP_PORT=0 switches the endpoint off at run time", *port)
				}

				notes := []string{
					fmt.Sprintf("The endpoint serves 127.0.0.1:%d/mcp when the service starts, in app-runtime mode: "+
						"read-only, and the tools that generate code, run migrations or drive Docker are not registered at all.", *port),
					"Set BREEZE_MCP_TOKEN to a fixed token. Without one a token is generated and printed once at startup.",
					"Run time environment: BREEZE_MCP_PORT (0 switches the endpoint off), BREEZE_MCP_HOST (0.0.0.0 inside a container, " +
						"where the token is then the only guard), BREEZE_MCP_TOKEN, BREEZE_MCP_SCOPE (for example runtime,fleet).",
					"To let an agent act on this service, expose the action as a route and mark it for Auto-MCP (app.EnableMCP). This endpoint stays read-only.",
					"The binary grows by several megabytes: the mcp package links the generator and the verification tools, whether or not an agent ever calls them.",
				}
				if !ctx.HasDashboard {
					notes = append(notes, "No dashboard block yet: the tools that read routes, errors, logs, signals, health and performance "+
						"read this service's dashboard, and without one in the process they answer that there is nothing to read. "+
						"Only breeze_diagnose_service and the Fleet tools work. Run `breeze add dashboard` so the rest have something to read.")
				} else {
					notes = append(notes, "With the dashboard in the same process those tools read its collector in memory when called without a "+
						"service_url: no credentials are needed, and the application's own rate limiter cannot refuse the call, which matters "+
						"most while the service is rejecting traffic.")
				}

				// Placeholder substitution, not Sprintf: the block carries its own %q verbs.
				body := strings.ReplaceAll(mcpBlock, "__PORT__", strconv.Itoa(*port))
				return featureOutput{Body: body, Notes: notes}, nil
			}
		},
	})
}

const mcpBlock = `// setupMcp serves this service's in-process MCP endpoint: a window onto the running
// service for an AI agent. It can read the routes, recent errors, logs, performance and
// the health of every subsystem, and with Fleet installed the traces. Most of those
// tools read the dashboard, so add that too (breeze add dashboard); called without a
// service_url they read it in memory, so they need no credentials and the application's
// own rate limiter cannot refuse them.
//
// Read-only by construction. The mode below is app-runtime, in which the tools that
// generate code, run migrations or drive Docker are not registered at all, so a leaked
// token cannot reach them. To let an agent act on this service, expose the action as a
// route and mark it for Auto-MCP (app.EnableMCP): that is a decision about one endpoint,
// made by the person who wrote it, and this endpoint stays a window.
//
// Configured from the environment, so one build serves every deployment:
//
//	BREEZE_MCP_PORT   control port, never the application's own (default __PORT__; 0 switches the endpoint off)
//	BREEZE_MCP_HOST   bind address (default loopback; 0.0.0.0 inside a container, where the token is the only guard)
//	BREEZE_MCP_TOKEN  bearer token (default: one is generated and printed once)
//	BREEZE_MCP_SCOPE  capabilities the token may use, for example runtime,fleet (default: every one the mode offers)
func setupMcp(app *breeze.Breeze, router *breeze.Router) {
	port := __PORT__
	if v := os.Getenv("BREEZE_MCP_PORT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 65535 {
			log.Fatalf("mcp: BREEZE_MCP_PORT=%q is not a port", v)
		}
		port = n
	}
	if port == 0 {
		return
	}

	// A scope that does not parse stops the process rather than being treated as
	// unscoped: a typo in a scope must not silently widen the token it was meant to
	// narrow.
	scope, err := mcp.ParseScope(os.Getenv("BREEZE_MCP_SCOPE"))
	if err != nil {
		log.Fatalf("mcp: BREEZE_MCP_SCOPE: %v", err)
	}

	server, token, err := mcp.StartInProcess(app, mcp.InProcessConfig{
		Mode:  mcp.ModeAppRuntime,
		Port:  port,
		Host:  os.Getenv("BREEZE_MCP_HOST"),
		Token: os.Getenv("BREEZE_MCP_TOKEN"),
		Scope: scope,
	})
	if err != nil {
		log.Fatalf("mcp: endpoint failed to start: %v", err)
	}

	// The token is printed only when it was generated. One that came from the
	// environment is already known to whoever set it, and a log is the wrong place to
	// repeat a secret.
	if os.Getenv("BREEZE_MCP_TOKEN") == "" {
		log.Printf("mcp: read-only endpoint %s, token %s", server.URL(), token)
	} else {
		log.Printf("mcp: read-only endpoint %s", server.URL())
	}

	go func() {
		if err := server.Serve(); err != nil {
			log.Printf("mcp: endpoint stopped: %v", err)
		}
	}()
}`
