// Command fleet-aggregator runs Breeze Fleet's bounded in-memory aggregator.
//
// Environment:
//
//	FLEET_PORT (9000), FLEET_BASE_PATH (/fleet), FLEET_USERNAME,
//	FLEET_PASSWORD, FLEET_INGEST_TOKEN, FLEET_SERVICE_TOKEN,
//	FLEET_FETCH_ALLOWED_HOSTS, FLEET_SERVICE_TTL.
//
// FLEET_SERVICE_TTL is how long a service may go without a heartbeat before the registry
// reports it down, as a Go duration ("30s", "2m"); the default is 15s. The registry forgets
// a service that has been silent for twenty times that.
package main

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

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
	if os.Getenv("FLEET_PASSWORD") == "" {
		// The default stays "admin": the container provisioning in internal/mcp and
		// breeze_get_topology read this aggregator with it, so changing it silently
		// breaks them (the live provisioning test answers 401). But a well-known
		// credential on a service that reads every trace must not be quiet about it.
		fmt.Println("[warning] FLEET_PASSWORD is not set: the read API and dashboard accept the default " +
			"credentials admin/admin. Set FLEET_PASSWORD before exposing this port beyond localhost.")
	}
	cfg.IngestToken = os.Getenv("FLEET_INGEST_TOKEN")
	// Enables trace-correlated log stitching. It was impossible to turn on with
	// this binary because nothing ever set it.
	cfg.ServiceToken = os.Getenv("FLEET_SERVICE_TOKEN")
	if hosts := os.Getenv("FLEET_FETCH_ALLOWED_HOSTS"); hosts != "" {
		cfg.FetchAllowedHosts = strings.Split(hosts, ",")
	}
	if err := applyServiceTTL(&cfg, os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, "fleet-aggregator:", err)
		os.Exit(2)
	}
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

// applyServiceTTL sets cfg.ServiceTTL from FLEET_SERVICE_TTL.
//
// It was not settable at all: a deployment whose services heartbeat every minute was marked down
// four times in five at the 15 second default, and one that wanted a fast failure signal could not
// have it. An unset variable leaves the default. A set one that is not a positive duration stops
// the process, because a liveness window that silently reverts to the default is one nobody chose.
func applyServiceTTL(cfg *aggregator.Config, getenv func(string) string) error {
	v := strings.TrimSpace(getenv("FLEET_SERVICE_TTL"))
	if v == "" {
		return nil
	}
	ttl, err := time.ParseDuration(v)
	if err != nil {
		return fmt.Errorf("FLEET_SERVICE_TTL=%q is not a duration such as 30s or 2m: %v", v, err)
	}
	if ttl <= 0 {
		return fmt.Errorf("FLEET_SERVICE_TTL=%q must be positive", v)
	}
	cfg.ServiceTTL = ttl
	return nil
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
