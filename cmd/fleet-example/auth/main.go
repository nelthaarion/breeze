// Command auth is the middle hop of the Fleet Tracing example.
//
// Its only job in the demo is to be a hop: it inherits the trace the gateway
// started, adds a tag of its own, and calls orders. That makes it the service
// that proves propagation survives more than one boundary — and, when orders
// fails, the one that shows up as a derived error rather than a root cause.
//
// It installs a dashboard for the same reason the gateway does: the aggregator
// fetches each service's logs from its own dashboard when stitching a trace's
// merged log panel (§9C.2), so a service without one contributes no log lines.
//
// Environment: ORDERS_URL, FLEET_WRITE_URL, FLEET_INGEST_TOKEN,
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
	"time"

	"github.com/nelthaarion/breeze"
	"github.com/nelthaarion/breeze/dashboard"
	"github.com/nelthaarion/breeze/fleet"
	"github.com/nelthaarion/breeze/fleet/transport/httptransport"
)

func main() {
	port := envInt("PORT", 3001)
	router := breeze.NewRouter()
	app := breeze.New(router, breeze.NewEventLoopWorkerPool(runtime.NumCPU()))

	dcfg := dashboard.DefaultConfig()
	dcfg.Username, dcfg.Password = "admin", "admin"
	dcfg.ServiceToken = env("FLEET_SERVICE_TOKEN", "fleet-demo-service-token")
	coll := dashboard.Install(app, router, dcfg)

	tr := newTracer("auth-service", coll.PushLog, router)
	defer tr.Close(context.Background())
	router.Use(skipUntraced(fleet.Middleware(tr)))
	router.Use(coll.Middleware())

	router.Handle(breeze.GET, "/healthz", func(ctx *breeze.Context) {
		ctx.JSON(map[string]string{"status": "ok", "service": "auth-service"})
	})

	ordersURL := env("ORDERS_URL", "http://localhost:3002")

	// HandleBlocking: this handler calls orders over HTTP.
	router.HandleBlocking(breeze.GET, "/internal/auth/:id", func(ctx *breeze.Context) {
		id := ctx.Param("id")

		// The gateway's order_id arrives as baggage and is already on this
		// span; auth_subject is added here, so the merged trace shows tags
		// accumulating hop by hop rather than only at the edge.
		fleet.Tag(ctx, "auth_subject", "demo-user")

		// PushLogCtx stamps the line with this request's trace id, which is
		// what puts it in the merged log panel alongside the gateway's and
		// orders' lines for the same trace (§9C.2).
		coll.PushLogCtx(ctx, "info", "auth-service authorized order "+id, "app")

		req, err := http.NewRequest(http.MethodPost, ordersURL+"/internal/orders/"+id, nil)
		if err != nil {
			coll.PushLogCtx(ctx, "error", "auth-service could not build the orders request: "+err.Error(), "app")

			ctx.Status(500)
			ctx.JSON(map[string]string{"error": "internal error"})
			return
		}

		resp, err := fleet.WrapClient(http.DefaultClient, ctx).Do(req)
		if err != nil {
			coll.PushLogCtx(ctx, "error", "auth-service could not reach orders-service: "+err.Error(), "app")

			ctx.Status(502)
			ctx.JSON(map[string]string{"error": err.Error()})
			return
		}
		defer resp.Body.Close()

		var result any
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			// Decoded on a best-effort basis, but no longer silently: the
			// original example discarded this error, so a malformed
			// downstream body looked like a successful authorization.
			coll.PushLogCtx(ctx, "warning", "auth-service got an undecodable orders response: "+err.Error(), "app")
		}

		if resp.StatusCode >= 500 {
			coll.PushLogCtx(ctx, "error", "auth-service saw orders-service fail with "+strconv.Itoa(resp.StatusCode), "app")

			ctx.Status(resp.StatusCode)
		}
		ctx.JSON(map[string]any{"authorized": true, "orders": result})
	})

	fmt.Printf("auth-service :%d\n", port)
	app.Run(port, true)
}

// skipUntraced keeps liveness probes out of the trace list. See the gateway's
// copy for why this is an application-level decision rather than a framework one.
func skipUntraced(traced breeze.HandlerFunc) breeze.HandlerFunc {
	return func(ctx *breeze.Context) {
		if ctx.Req != nil && ctx.Req.Path == "/healthz" {
			ctx.Next()
			return
		}
		traced(ctx)
	}
}

func newTracer(service string, log func(string, string, string), router *breeze.Router) *fleet.Tracer {
	return fleet.New(fleet.TracerConfig{
		Enabled:       true,
		ServiceName:   service,
		AggregatorURL: env("FLEET_WRITE_URL", "http://localhost:9000/fleet"),
		SampleRate:    1,
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
