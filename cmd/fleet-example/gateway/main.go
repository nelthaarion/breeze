// Command gateway is the edge service of the Fleet Tracing example.
//
// It is the only service a client talks to directly, so it is where a trace is
// born: no inbound traceparent exists, so fleet.Middleware starts a root trace
// and every hop below inherits it. It also hosts the dashboard whose Fleet View
// renders the assembled result.
//
// Environment: AUTH_URL, FLEET_WRITE_URL, FLEET_READ_URL, FLEET_INGEST_TOKEN,
// FLEET_SERVICE_TOKEN, PORT.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/nelthaarion/breeze"
	"github.com/nelthaarion/breeze/dashboard"
	"github.com/nelthaarion/breeze/fleet"
	"github.com/nelthaarion/breeze/fleet/transport/httptransport"
	"github.com/nelthaarion/breeze/mcp"
)

func main() {
	port := envInt("PORT", 3000)
	router := breeze.NewRouter()
	app := breeze.New(router, breeze.NewEventLoopWorkerPool(runtime.NumCPU()))

	dcfg := dashboard.DefaultConfig()
	dcfg.Username, dcfg.Password = "admin", "admin"
	dcfg.FleetAggregatorURL = env("FLEET_READ_URL", "http://localhost:9000/fleet")
	dcfg.FleetAggregatorUsername, dcfg.FleetAggregatorPassword = "admin", "admin"
	// Lets the aggregator fetch this service's own logs for a given trace
	// (§9C.2). Every service in the fleet shares one token, the same trust
	// model as the ingest token.
	dcfg.ServiceToken = env("FLEET_SERVICE_TOKEN", "fleet-demo-service-token")
	coll := dashboard.Install(app, router, dcfg)

	tr := newTracer("gateway", coll.PushLog, router)
	defer tr.Close(context.Background())

	// Fleet first so trace context exists before anything else runs, then the
	// dashboard, whose TimelineRecorder the span picks up on the way out.
	router.Use(skipUntraced(fleet.Middleware(tr)))
	router.Use(coll.Middleware())

	// Liveness. Deliberately excluded from tracing by skipUntraced: a probe
	// every few seconds is not a request anyone wants to debug, and letting it
	// through would bury real traffic in the trace list and inflate this
	// service's call count in the topology graph.
	router.Handle(breeze.GET, "/healthz", func(ctx *breeze.Context) error {
		return ctx.JSON(map[string]string{"status": "ok", "service": "gateway"})
	})

	authURL := env("AUTH_URL", "http://localhost:3001")

	// HandleBlocking because this handler makes a downstream HTTP call:
	// running blocking I/O inline would stall every connection pinned to the
	// same event loop for its duration.
	router.HandleBlocking(breeze.GET, "/api/orders/:id", func(ctx *breeze.Context) error {
		id := ctx.Param("id")

		// Tagged here, at the edge, and propagated as baggage to auth and
		// orders — so one tag search finds every hop that touched this
		// order (§9C.1).
		fleet.Tag(ctx, "order_id", id)

		// PushLogCtx, not PushLog: passing ctx stamps the line with this
		// request's trace id, which is what makes it appear in the merged
		// log panel under this trace (§9C.2). The same call without ctx
		// still logs, but only to this service's own Logs page — invisible
		// from the trace, which is where anyone debugging is looking.
		coll.PushLogCtx(ctx, "info", "gateway received order "+id, "app")

		req, err := http.NewRequest(http.MethodGet, authURL+"/internal/auth/"+id, nil)
		if err != nil {
			coll.PushLogCtx(ctx, "error", "gateway could not build the auth request: "+err.Error(), "app")

			ctx.Status(500)
			return ctx.JSON(map[string]string{"error": "internal error"})
		}

		// WrapClient injects traceparent and baggage, then calls Do. This is
		// the one explicit step that links this hop to the next: without it
		// auth would start a brand-new trace instead of joining this one.
		resp, err := fleet.WrapClient(http.DefaultClient, ctx).Do(req)
		if err != nil {
			coll.PushLogCtx(ctx, "error", "gateway could not reach auth-service: "+err.Error(), "app")

			ctx.Status(502)
			return ctx.JSON(map[string]string{"error": err.Error()})
		}
		defer resp.Body.Close()

		var result any
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			coll.PushLogCtx(ctx, "error", "gateway got an undecodable auth response: "+err.Error(), "app")

			ctx.Status(502)
			return ctx.JSON(map[string]string{"error": "invalid downstream response"})
		}

		// Propagate the downstream failure rather than reporting success over
		// it: the chaos demo depends on this hop actually being red, and a
		// gateway that swallowed a 500 would hide the cascade it is meant to
		// illustrate.
		if resp.StatusCode >= 500 {
			coll.PushLogCtx(ctx, "error", "gateway saw auth-service fail with "+strconv.Itoa(resp.StatusCode), "app")
			ctx.Status(resp.StatusCode)
		} else {
			coll.PushLogCtx(ctx, "info", "gateway completed order "+id, "app")

		}
		return ctx.JSON(map[string]any{"order_id": id, "result": result})
	})

	fmt.Printf("gateway :%d — dashboard http://localhost:%d/dashboard (admin/admin)\n", port, port)
	startControlPlane(app, "gateway")
	app.Run(port, true)
}

// startControlPlane serves the in-process MCP endpoint beside the application.
//
// Off unless BREEZE_MCP_PORT is set, so the example's default behaviour is unchanged
// and nothing new is exposed by simply running it.
//
// ModeAppRuntime, always: this process is a deployed service, and the generating and
// provisioning tools are not merely unhelpful here — they would chdir and rewrite a
// source tree the container does not have, while this same process is serving requests.
// In that mode they are not registered at all.
//
// BREEZE_MCP_SCOPE narrows the token further. Mode decides what this server offers;
// scope decides what the credential reaches. An empty value means unscoped, which is
// the documented default.
func startControlPlane(app *breeze.Breeze, service string) {
	port := envInt("BREEZE_MCP_PORT", 0)
	if port == 0 {
		return
	}

	scope, err := mcp.ParseScope(os.Getenv("BREEZE_MCP_SCOPE"))
	if err != nil {
		// Refused rather than downgraded to unscoped: a typo in a scope must not
		// silently widen the token it was meant to narrow.
		fmt.Printf("%s: mcp scope rejected: %v\n", service, err)
		os.Exit(1)
	}

	// Host 0.0.0.0 because the caller is on the Docker host, not inside the container,
	// so loopback would be unreachable. The bearer token is then the only guard, which
	// is why it is required rather than generated-and-forgotten here.
	server, token, err := mcp.StartInProcess(app, mcp.InProcessConfig{
		Mode:  mcp.ModeAppRuntime,
		Port:  port,
		Host:  "0.0.0.0",
		Token: env("BREEZE_MCP_TOKEN", ""),
		Scope: scope,
	})
	if err != nil {
		fmt.Printf("%s: mcp endpoint failed to start: %v\n", service, err)
		os.Exit(1)
	}

	granted := "all capabilities (unscoped)"
	if scope.IsScoped() {
		names := make([]string, 0, len(scope.Granted()))
		for _, c := range scope.Granted() {
			names = append(names, string(c))
		}
		granted = strings.Join(names, ",")
	}
	fmt.Printf("%s: mcp app-runtime endpoint %s (scope %s, token %s)\n",
		service, server.URL(), granted, token)

	go func() { _ = server.Serve() }()
}

// skipUntraced wraps the fleet middleware so probe traffic never becomes a span.
//
// A predicate here rather than in the framework because "which of my routes are
// worth tracing" is an application decision: this example wants /healthz out,
// another app might want it in. The dashboard's own routes need no such
// treatment — they are not registered through the router's middleware chain, so
// polling the dashboard already produces no spans.
func skipUntraced(traced breeze.HandlerFunc) breeze.HandlerFunc {
	return func(ctx *breeze.Context) error {
		if ctx.Req != nil && ctx.Req.Path == "/healthz" {
			return ctx.Next()
		}
		traced(ctx)

		return nil
	}
}

func newTracer(service string, log func(string, string, string), router *breeze.Router) *fleet.Tracer {
	return fleet.New(fleet.TracerConfig{
		Enabled:       true,
		ServiceName:   service,
		AggregatorURL: env("FLEET_WRITE_URL", "http://localhost:9000/fleet"),
		SampleRate:    1,
		// Resolves /api/orders/A-1 to the pattern /api/orders/:id, so the
		// topology graph has one edge per route instead of one per order id.
		RouteResolver: fleet.RouterResolver(router),
		Logger:        log,
		Transport: httptransport.NewWithGzip(httptransport.Config{
			IngestToken: env("FLEET_INGEST_TOKEN", "fleet-demo-token"),
			ServiceName: service,
			Timeout:     2 * time.Second,
		}),
	})
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil && v > 0 {
		return v
	}
	return fallback
}
