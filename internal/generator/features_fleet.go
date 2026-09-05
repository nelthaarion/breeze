package generator

// The `breeze add fleet` feature: distributed tracing wiring.
//
// This is the one feature whose generated code differs structurally depending on
// configuration, because Fleet's transports are not interchangeable at the call
// site. http and ws take a URL and a token; events takes a bus and two topic
// names. So the transport construction is emitted per transport rather than
// parameterised over one shape.
//
// The wiring itself is modelled on cmd/fleet-example/gateway, which is a working
// service rather than a sketch â€” the ordering constraint below (Fleet middleware
// before anything that reads trace context) is copied from there because it was
// derived by running it, not by reasoning about it.
//
// Only the transports with real implementations can be selected. gnet and grpc
// are named by the spec, and gnettransport even exists as a package, but every
// method on it delegates to httptransport: generating it would produce a service
// that appears to use gnet and does not. validateFleet refuses those, and this
// generator would too if it were reached.

import (
	"flag"
	"fmt"
	"strings"
)

const (
	fleetImport           = `"github.com/nelthaarion/breeze/fleet"`
	fleetHTTPImport       = `"github.com/nelthaarion/breeze/fleet/transport/httptransport"`
	fleetWSImport         = `"github.com/nelthaarion/breeze/fleet/transport/wstransport"`
	fleetEventsImport     = `"github.com/nelthaarion/breeze/fleet/transport/eventtransport"`
	fleetTransportTimeout = "2 * time.Second"
)

func registerFleetFeature() {
	register(&feature{
		Name:    "fleet",
		Summary: "distributed tracing: trace context propagation and span export",
		// Ahead of observability (8) and the middleware band. Fleet's
		// middleware has to run before anything that reads trace context, and
		// the dispatcher's order is what enforces that.
		Priority: 6,
		// timeImport is not here: it is needed by the http and ws transports'
		// Timeout field and unused by events. Nothing prunes unused imports
		// from a generated block, so an unconditional "time" would be a compile
		// error in an events-transport project. It is added per transport in
		// fleetTransportExpr instead.
		Imports: []string{fleetImport, osImport},
		// The tracer's Logger is wired to the dashboard collector when one is
		// present, which is what puts a service's own log lines under the right
		// trace in the merged log panel.
		//
		// events is deliberately absent: the events transport publishes to
		// events.Default rather than to a bus this feature looks up, so an
		// events block being present does not change what is generated.
		DependsOn: []string{"dashboard"},

		Build: func(fs *flag.FlagSet) func(featureCtx) (featureOutput, error) {
			service := fs.String("service", "", "service name reported on every span (default: the module's last path element)")
			transport := fs.String("transport", "http", "span transport: "+strings.Join(fleetImplementedTransports, ", "))
			url := fs.String("aggregator-url", "http://localhost:9000/fleet", "aggregator HTTP write endpoint")
			wsURL := fs.String("aggregator-ws-url", "ws://localhost:9000/fleet/ws", "aggregator WebSocket ingest endpoint (ws transport only)")
			sample := fs.Float64("sample-rate", 1, "fraction of traces to sample, 0..1")
			gzip := fs.Bool("gzip", false, "compress span batches (http transport only)")

			return func(ctx featureCtx) (featureOutput, error) {
				cfg := FleetConfig{
					Enabled:         true,
					ServiceName:     *service,
					Transport:       *transport,
					Backend:         "memory",
					AggregatorURL:   *url,
					AggregatorWSURL: *wsURL,
					SampleRate:      *sample,
				}
				if cfg.ServiceName == "" {
					cfg.ServiceName = defaultServiceName(ctx.ModulePath)
				}

				// Validated through the same path a config file takes, so
				// `add fleet --transport=grpc` and a YAML file naming grpc fail
				// with the same message rather than one failing at the compiler.
				probe := Defaults()
				probe.Fleet = cfg
				if errs := probe.validateFleet(); len(errs) > 0 {
					return featureOutput{}, fmt.Errorf("%s", strings.Join(errs, "; "))
				}

				return buildFleetOutput(cfg, ctx, *gzip)
			}
		},
	})
}

// defaultServiceName derives a service name from the module path.
//
// A span with no service name is not attributable to anything, so this falls
// back to the module's last element rather than leaving it empty.
func defaultServiceName(modulePath string) string {
	if modulePath == "" {
		return "service"
	}
	parts := strings.Split(strings.Trim(modulePath, "/"), "/")
	last := parts[len(parts)-1]
	if last == "" {
		return "service"
	}
	return last
}

// buildFleetOutput emits the feature block for a resolved config.
//
// It is separate from Build so tests can drive it from a FleetConfig directly,
// without going through flag parsing.
func buildFleetOutput(cfg FleetConfig, ctx featureCtx, gzip bool) (featureOutput, error) {
	imports := []string{fleetImport, osImport}

	transportExpr, transportImport, err := fleetTransportExpr(cfg, gzip)
	if err != nil {
		return featureOutput{}, err
	}
	imports = append(imports, transportImport)

	// "time" only where a Timeout is actually emitted. An unused import is a
	// compile error in Go, and nothing downstream prunes one from a generated
	// block, so this has to be decided here rather than added unconditionally.
	if strings.Contains(transportExpr, "time.") {
		imports = append(imports, timeImport)
	}

	// The tracer's Logger is what puts this service's log lines under the right
	// trace in the aggregator's merged log panel.
	//
	// dashboard.Collector.PushLog has exactly the signature TracerConfig.Logger
	// wants â€” func(level, message, source string) â€” so with a dashboard block
	// present it is wired straight through, as cmd/fleet-example/gateway does.
	// Without one there is nowhere for those lines to go, so the field is left
	// nil: the tracer then skips logging rather than writing somewhere the
	// developer will never look.
	logger := "nil"
	loggerNote := ""
	if ctx.HasDashboard {
		logger = "DashboardCollector.PushLog"
	} else {
		loggerNote = "No dashboard block, so FleetTracer.Logger is nil and this service's logs will not appear under its traces. Run `breeze add dashboard`, then re-run `breeze add fleet` to wire them up."
	}

	var body strings.Builder

	fmt.Fprintf(&body, `// FleetTracer is this service's tracer. It propagates trace context on
// outbound calls and exports finished spans to the aggregator.
//
// Close it on shutdown to flush spans still in the buffer:
//
//	defer FleetTracer.Close(context.Background())
var FleetTracer *fleet.Tracer

func setupFleet(app *breeze.Breeze, router *breeze.Router) {
	FleetTracer = fleet.New(fleet.TracerConfig{
		Enabled:     true,
		ServiceName: fleetEnv("FLEET_SERVICE_NAME", %q),
		SampleRate:  %v,
		// Resolves /users/42 to the registered pattern /users/:id, so the
		// topology graph has one edge per route rather than one per id.
		RouteResolver: fleet.RouterResolver(router),
		Logger:        %s,
		AggregatorURL: fleetEnv("FLEET_WRITE_URL", %q),
		Transport: %s,
	})

	// Registered before any middleware that reads trace context, so a span
	// exists by the time they run. Router.Use rebuilds every route's chain on
	// each call, so this holds regardless of when routes were declared.
	router.Use(fleet.Middleware(FleetTracer))
}

// fleetEnv prefers an environment variable, falling back to the value chosen at
// generation time. Endpoints and tokens differ per deployment, and a generated
// default that cannot be overridden without editing code is a default that gets
// edited into a secret committed by accident.
func fleetEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}`,
		cfg.ServiceName,
		cfg.SampleRate,
		logger,
		cfg.AggregatorURL,
		transportExpr,
	)

	notes := []string{
		"Set FLEET_INGEST_TOKEN, FLEET_WRITE_URL and FLEET_SERVICE_NAME in the environment rather than editing the generated defaults.",
		"Call FleetTracer.Close(context.Background()) on shutdown, or spans buffered at exit are lost.",
	}
	if loggerNote != "" {
		notes = append(notes, loggerNote)
	}
	if cfg.Transport == "ws" {
		notes = append(notes,
			"The ws transport falls back to HTTP when the WebSocket cannot be established, so fleet.aggregator_url must stay reachable.")
	}
	if cfg.Transport == "events" {
		notes = append(notes,
			"The events transport publishes spans onto the bus; the aggregator must subscribe to the same backend to receive them.")
	}

	return featureOutput{Body: body.String(), Imports: imports, Notes: notes}, nil
}

// fleetTransportExpr returns the transport constructor call and the import it
// needs.
//
// Each arm mirrors the constructor's actual Config fields. That is why this is a
// switch over emitted source rather than a table: eventtransport takes a bus and
// topics where the others take a URL, so there is no shared shape to fill in.
func fleetTransportExpr(cfg FleetConfig, gzip bool) (expr, imp string, err error) {
	switch cfg.Transport {
	case "http":
		ctor := "httptransport.New"
		if gzip {
			// Gzip is worth it for span batches, which are highly compressible
			// JSON, but it costs CPU on the exporting service â€” so it stays
			// opt-in rather than becoming the default.
			ctor = "httptransport.NewWithGzip"
		}
		return fmt.Sprintf(`%s(httptransport.Config{
			IngestToken: fleetEnv("FLEET_INGEST_TOKEN", "change-me"),
			ServiceName: fleetEnv("FLEET_SERVICE_NAME", %q),
			Timeout:     %s,
		})`, ctor, cfg.ServiceName, fleetTransportTimeout), fleetHTTPImport, nil

	case "ws":
		// Only the WebSocket URL is set here. wstransport.Config has no
		// AggregatorURL field: it builds its HTTP fallback from these same
		// credentials and takes the write endpoint from the addr the tracer
		// passes on export, which is TracerConfig.AggregatorURL. Setting a
		// field of that name here would simply not compile.
		return fmt.Sprintf(`wstransport.New(wstransport.Config{
			AggregatorWSURL: fleetEnv("FLEET_WS_URL", %q),
			IngestToken:     fleetEnv("FLEET_INGEST_TOKEN", "change-me"),
			ServiceName:     fleetEnv("FLEET_SERVICE_NAME", %q),
			Timeout:         %s,
		})`, cfg.AggregatorWSURL, cfg.ServiceName, fleetTransportTimeout), fleetWSImport, nil

	case "events":
		// Bus and Backend are left to the transport's own defaults, which
		// resolve to events.Default â€” the bus the framework already publishes
		// to. Standing up a second bus here would split the stream in two.
		//
		// No Timeout field, so no "time" import for this arm; see the guard in
		// buildFleetOutput.
		return fmt.Sprintf(`eventtransport.New(eventtransport.Config{
			IngestToken: fleetEnv("FLEET_INGEST_TOKEN", "change-me"),
			ServiceName: fleetEnv("FLEET_SERVICE_NAME", %q),
		})`, cfg.ServiceName), fleetEventsImport, nil

	default:
		// Unreachable through the CLI: validateFleet rejects these first. Kept
		// because a wrong transport must never silently emit a working-looking
		// project, and a future transport added to the allowlist without a case
		// here should fail loudly at generation.
		return "", "", fmt.Errorf(
			"fleet transport %q has no generator â€” implemented: %s",
			cfg.Transport, strings.Join(fleetImplementedTransports, ", "))
	}
}
