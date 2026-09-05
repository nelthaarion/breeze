// Command orders is the deepest hop of the Fleet Tracing example, and the one
// that demonstrates the two differentiator features.
//
//   - Contract validation (§9A): it serves an OpenAPI document declaring
//     additionalProperties:false, then deliberately returns an undeclared
//     debug_note field. The aggregator fetches that document by hash from the
//     heartbeat and reports the mismatch from live traffic — no fixture, no
//     hand-written contract test.
//   - Root cause and blast radius (§9B): POST /chaos/fail makes it return 500s,
//     so the cascade it causes upstream can be seen being attributed back to it.
//
// Environment: FLEET_WRITE_URL, FLEET_INGEST_TOKEN, FLEET_SERVICE_TOKEN,
// ORDERS_OPENAPI_URL, PORT.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nelthaarion/breeze/v2"
	"github.com/nelthaarion/breeze/v2/dashboard"
	"github.com/nelthaarion/breeze/v2/fleet"
	"github.com/nelthaarion/breeze/v2/fleet/transport/httptransport"
)

// openAPIDocument is what this service claims to return. The response handler
// deliberately violates it — see the debug_note field below.
//
// additionalProperties:false is what makes the extra field an error rather than
// a warning: the schema explicitly states the response is closed, so an
// undeclared field is a contract breach and not merely an unrecognised addition.
var openAPIDocument = []byte(`{"openapi":"3.1.0","info":{"title":"Orders","version":"1.0.0"},"paths":{"/internal/orders/{id}":{"post":{"responses":{"200":{"description":"OK","content":{"application/json":{"schema":{"type":"object","properties":{"id":{"type":"string"},"state":{"type":"string"}},"required":["id","state"],"additionalProperties":false}}}},"500":{"description":"Failure"}}}}}}`)

// fail is the chaos switch. Atomic because the toggle route and the order route
// run on different goroutines.
var fail atomic.Bool

func main() {
	port := envInt("PORT", 3002)
	router := breeze.NewRouter()
	app := breeze.New(router, breeze.NewEventLoopWorkerPool(runtime.NumCPU()))

	dcfg := dashboard.DefaultConfig()
	dcfg.Username, dcfg.Password = "admin", "admin"
	dcfg.ServiceToken = env("FLEET_SERVICE_TOKEN", "fleet-demo-service-token")
	coll := dashboard.Install(app, router, dcfg)

	tr := newTracer("orders-service", coll.PushLog, router)
	defer tr.Close(context.Background())
	router.Use(skipUntraced(fleet.Middleware(tr)))
	router.Use(coll.Middleware())

	router.Handle(breeze.GET, "/healthz", func(ctx *breeze.Context) error {
		return ctx.JSON(map[string]string{"status": "ok", "service": "orders-service"})
	})

	// The document the aggregator's SchemaRegistry fetches whenever the hash in
	// this service's heartbeat changes.
	router.Handle(breeze.GET, "/openapi.json", func(ctx *breeze.Context) error {
		ctx.SetHeader("Content-Type", "application/json")
		return ctx.WriteString(string(openAPIDocument))
	})

	router.Handle(breeze.POST, "/internal/orders/:id", func(ctx *breeze.Context) error {
		id := ctx.Param("id")
		fleet.Tag(ctx, "payment_provider", "stripe")

		if fail.Load() {
			// The error text is what the trace summary quotes verbatim, so
			// it is written to read as a cause rather than as a status code.
			coll.PushLogCtx(ctx, "error", "orders-service payment provider timed out for order "+id, "app")

			ctx.Status(500)
			return ctx.JSON(map[string]string{"error": "payment provider timeout"})
		}

		// PushLogCtx, so this line carries the trace id and lands in the
		// merged log panel next to the gateway's and auth's lines for the
		// same request (§9C.2). The chaos toggles below stay on PushLog:
		// they describe a service-wide state change that outlives the
		// request that flipped it, not a step in anyone's trace.
		coll.PushLogCtx(ctx, "info", "orders-service charged order "+id, "app")

		// debug_note is the deliberate schema mismatch: undeclared, on a
		// closed schema, so Contract Violations reports it as an error within
		// seconds of this response being sampled.
		return ctx.JSON(map[string]any{"id": id, "state": "paid", "debug_note": "not declared in the response schema"})
	})

	router.Handle(breeze.POST, "/chaos/fail", func(ctx *breeze.Context) error {
		fail.Store(true)
		coll.PushLog("warning", "orders-service chaos enabled: now failing every order", "app")
		return ctx.JSON(map[string]bool{"failing": true})
	})
	router.Handle(breeze.POST, "/chaos/recover", func(ctx *breeze.Context) error {
		fail.Store(false)
		coll.PushLog("info", "orders-service chaos disabled: serving normally", "app")
		return ctx.JSON(map[string]bool{"failing": false})
	})

	fmt.Printf("orders-service :%d — POST /chaos/fail toggles 500s\n", port)
	app.Run(port, true)
}

// skipUntraced keeps this service's out-of-band routes out of the trace data.
//
// Three kinds of traffic are excluded, all for the same reason: none of them is
// a request the fleet is meant to explain, and all of them would corrupt the
// picture if traced.
//
//   - /healthz — a probe every few seconds, which would swamp the trace list.
//   - /chaos/* — operator controls for the demo, not application traffic.
//   - /openapi.json — fetched by the aggregator itself. Tracing it is actively
//     misleading: the fetch arrives with no traceparent, so it becomes a *root*
//     span, and this service — the deepest hop in the fleet — starts rendering
//     as an entry point in the topology graph alongside the gateway.
func skipUntraced(traced breeze.HandlerFunc) breeze.HandlerFunc {
	return func(ctx *breeze.Context) error {
		if ctx.Req != nil {
			p := ctx.Req.Path
			if p == "/healthz" || p == "/openapi.json" || strings.HasPrefix(p, "/chaos/") {
				return ctx.Next()
			}
		}
		traced(ctx)

		return nil
	}
}

func newTracer(service string, log func(string, string, string), router *breeze.Router) *fleet.Tracer {
	hash := sha256.Sum256(openAPIDocument)
	return fleet.New(fleet.TracerConfig{
		Enabled:       true,
		ServiceName:   service,
		AggregatorURL: env("FLEET_WRITE_URL", "http://localhost:9000/fleet"),
		SampleRate:    1,
		RouteResolver: fleet.RouterResolver(router),
		Logger:        log,
		// The hash is what tells the aggregator the document changed; the URL
		// is where to fetch it. It must be reachable *from the aggregator*,
		// which is why Compose overrides it with this service's DNS name.
		OpenAPIHash: hex.EncodeToString(hash[:]),
		OpenAPIURL:  env("ORDERS_OPENAPI_URL", "http://localhost:3002/openapi.json"),
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
