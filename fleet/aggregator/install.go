package aggregator

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/nelthaarion/breeze/v2"
	"github.com/nelthaarion/breeze/v2/events"
	"github.com/nelthaarion/breeze/v2/fleet/contracts"
)

// Aggregator owns the bounded store, service registry, topology projection, and
// their background expiry sweep. It is safe for concurrent ingestion and reads.
type Aggregator struct {
	cfg      Config
	store    SpanStore
	registry *ServiceRegistry
	topology *TopologyGraph

	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once

	eventCancels  []func()
	eventsBackend events.Backend
	hub           *wsHub
	contracts     *contractEngine

	// logs is the §9C.2 fan-out client, nil when no ServiceToken is set.
	// Nil-as-disabled rather than a bool plus a client, so there is no state
	// where the feature is enabled but has nothing to authenticate with.
	logs *logFanout
}

// InstallAggregator mirrors dashboard.Install: construct the subsystem, register
// its routes, and return the handle the process shuts down with.
func InstallAggregator(app *breeze.Breeze, router *breeze.Router, cfg Config) *Aggregator {
	a := installWithStore(router, cfg, nil)
	if app != nil && (a.cfg.transportEnabled("ws") || a.cfg.transportEnabled("events")) {
		a.hub = newWSHub(a)
		if a.contracts != nil {
			a.contracts.hub = func(group contracts.Group) { a.hub.broadcast("contract_violation", group) }
		}
		app.WebSocket(strings.TrimSuffix(a.cfg.BasePath, "/")+"/ws", a.hub)
	}
	return a
}

func installWithStore(router *breeze.Router, cfg Config, store SpanStore) *Aggregator {
	cfg = cfg.withDefaults()
	if store == nil {
		store = NewMemStore(cfg)
	}
	a := &Aggregator{
		cfg:      cfg,
		store:    store,
		registry: NewServiceRegistry(cfg),
		topology: NewTopologyGraph(cfg),
		done:     make(chan struct{}),
	}
	if cfg.ContractValidation {
		a.contracts = newContractEngine(cfg)
	}
	if cfg.ServiceToken != "" {
		a.logs = newLogFanout(cfg.ServiceToken)
	}

	if cfg.Logger != nil {
		if !cfg.AuthEnabled() {
			cfg.Logger(
				"warning",
				"fleet aggregator read API has no Basic Auth; both username and password must be non-empty for auth to be enforced",
				"fleet",
			)
		}
		if cfg.IngestToken == "" {
			cfg.Logger(
				"warning",
				"fleet aggregator ingestion has no X-Fleet-Token; do not expose it on a public network",
				"fleet",
			)
		}
	}
	a.registerRoutes(router)
	if err := a.attachEvents(); err != nil && cfg.Logger != nil {
		cfg.Logger("error", "fleet events ingestion disabled: "+err.Error(), "fleet")
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	go a.sweepLoop(ctx)
	return a
}

func (a *Aggregator) sweepLoop(ctx context.Context) {
	defer close(a.done)
	interval := a.cfg.TraceTTL / 4
	if interval > a.cfg.ServiceTTL/2 {
		interval = a.cfg.ServiceTTL / 2
	}
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			a.store.Sweep(now)
			a.registry.Sweep(now)
			a.topology.Sweep(now)
		}
	}
}

// Close stops the sweep goroutine. It is idempotent.
func (a *Aggregator) Close(ctx context.Context) error {
	a.once.Do(func() {
		a.cancel()
		if a.hub != nil {
			a.hub.close()
		}
		if a.contracts != nil {
			a.contracts.close()
		}
		for _, cancel := range a.eventCancels {
			cancel()
		}
		if a.eventsBackend != nil {
			_ = a.eventsBackend.Close()
		}
	})
	select {
	case <-a.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *Aggregator) Store() SpanStore           { return a.store }
func (a *Aggregator) Registry() *ServiceRegistry { return a.registry }
func (a *Aggregator) Topology() *TopologyGraph   { return a.topology }
func (a *Aggregator) Violations() []contracts.Group {
	if a == nil || a.contracts == nil {
		return nil
	}
	return a.contracts.violations.Snapshot()
}
