// Command fleet-aggregator runs Breeze Fleet's bounded in-memory aggregator.
//
// Environment:
//
//	FLEET_PORT (9000), FLEET_BASE_PATH (/fleet), FLEET_USERNAME,
//	FLEET_PASSWORD, FLEET_INGEST_TOKEN.
package main

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"

	"github.com/nelthaarion/breeze/v2"
	"github.com/nelthaarion/breeze/v2/fleet/aggregator"
)

func main() {
	port := envInt("FLEET_PORT", 9000)
	router := breeze.NewRouter()
	app := breeze.New(router, breeze.NewEventLoopWorkerPool(runtime.NumCPU()))
	cfg := aggregator.DefaultConfig()

	cfg.BasePath = env("FLEET_BASE_PATH", "/fleet")
	cfg.Username = env("FLEET_USERNAME", "admin")
	cfg.Password = env("FLEET_PASSWORD", "admin")
	cfg.IngestToken = os.Getenv("FLEET_INGEST_TOKEN")
	cfg.TransportsEnabled = []string{"events", "http", "ws"}
	cfg.Logger = func(level, message, source string) { fmt.Printf("[%s] %s: %s\n", level, source, message) }
	agg := aggregator.InstallAggregator(app, router, cfg)

	// Unauthenticated liveness, outside cfg.BasePath so it is not covered by
	// the read-side Basic Auth: an orchestrator's probe has no credentials, and
	// requiring them would make a healthy aggregator report as unhealthy. It
	// discloses nothing beyond the fact that the process is up.
	router.Handle(breeze.GET, "/healthz", func(ctx *breeze.Context) error {
		return ctx.JSON(map[string]string{"status": "ok", "service": "fleet-aggregator"})
	})

	defer agg.Close(context.Background())

	fmt.Printf("Breeze Fleet Aggregator listening on :%d%s\n", port, cfg.BasePath)
	app.Run(port, true)
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if n, err := strconv.Atoi(os.Getenv(key)); err == nil && n > 0 {
		return n
	}
	return fallback
}
