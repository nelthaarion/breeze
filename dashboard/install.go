package dashboard

import (
	"fmt"
	"log"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/nelthaarion/breeze/v2"
	"github.com/nelthaarion/breeze/v2/diag"
	"github.com/nelthaarion/breeze/v2/events"
)

// Install wires the Developer Dashboard into a Breeze application.
//
// It registers the SPA, the API endpoints, the WebSocket hub, and the
// dashboard middleware on the given router/app. The returned Collector is
// the application's handle for pushing data into the dashboard:
//
//   - RecordQuery/RecordLog/UpdateQueue/RegisterTask/RegisterHealthCheck
//
// Install also applies Go runtime memory tuning (GOGC and GOMEMLIMIT) if
// they are set in the Config. This is the SINGLE MOST EFFECTIVE way to
// control Go's RSS — without a memory limit, Go's runtime holds onto
// memory it has allocated (HeapIdle stays high), causing the process RSS
// to grow to the peak heap size and never shrink.
//
// Install is idempotent: calling it twice on the same router is safe (the
// second call returns the collector the first one built and does nothing else).
// The middleware returned by Middleware(c) should be installed via router.Use()
// before any application routes are added so every request is instrumented.
//
// With Config.Enabled false, Install builds an inert collector: nothing is counted
// or sampled, no goroutine is started (no sampler, WebSocket loop or state saver) and
// no runtime tuning is applied. The routes are still registered, behind the login, so
// the read API keeps answering — with zero counts — and a tool that asks can tell a
// dashboard that is switched off from one that has seen no traffic (the MCP routes
// tool reports "not measured" from it). The live WebSocket is refused.
//
// Stop what Install started — the sampler, the WebSocket broadcast loop and the
// state saver, which writes the counters one last time — with Collector.Close.
//
// Usage:
//
//	app := breeze.New(router, pool)
//	coll := dashboard.Install(app, router, dashboard.DefaultConfig())
//	router.Use(dashboard.Middleware(coll))
//	// ... register application routes ...
//	app.Run(3000, true)
func Install(app *breeze.Breeze, router *breeze.Router, cfg Config) *Collector {
	// The documented contract is that a second Install on the same router is a no-op. It was
	// not: every call built a new collector, started another sampler and hub, tuned the GC
	// again and registered every route a second time.
	if router != nil {
		if prev, ok := installed.Load(router); ok {
			c := prev.(*Collector)
			if c.cfg.Enabled {
				c.MakeCurrent()
			}
			return c
		}
	}

	cfg = cfg.withDefaults()
	c := newCollector(cfg, router)
	c.app = app

	if !cfg.Enabled {
		// Off means nothing runs. The sampler used to tick (a stop-the-world ReadMemStats every
		// second), the hub's loop to spin and the GC to be retuned for a dashboard that was
		// switched off. The read API stays (see Install's documentation).
		diag.EnableCounters()
		c.registerDiagnostics()
		c.MakeCurrent()
		c.registerRoutes(router, app)
		if router != nil {
			installed.Store(router, c)
		}
		return c
	}

	// ── Apply Go runtime memory tuning ────────────────────────────────────
	//
	// This is the fix for the "3GB RSS" problem. Without these settings,
	// Go's runtime holds onto memory it has allocated from the OS even
	// after GC reclaims the objects. The result: HeapIdle grows to the
	// peak heap size and the process RSS never shrinks.
	//
	// GOMEMLIMIT (Go 1.19+) is a soft memory limit. When the process
	// approaches this limit, the GC runs more aggressively to keep
	// memory under the cap. This causes the scavenger to return idle
	// pages to the OS (HeapReleased increases), which reduces RSS.
	//
	// GOGC controls the GC trigger rate. Lower = more frequent GC =
	// smaller heap. We default to 50 (vs Go's 100) to keep the heap
	// from growing too large between GCs.
	//
	// An operator's own GOGC / GOMEMLIMIT environment variables win: these are the
	// library's defaults, and a default that silently overrides the setting someone chose for
	// their deployment (a 4 GiB limit replaced by 512 MiB) is the worst kind of surprise.
	if cfg.GOGC != 0 && os.Getenv("GOGC") == "" {
		debug.SetGCPercent(cfg.GOGC)
	}
	if cfg.GOMEMLIMIT > 0 && os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(cfg.GOMEMLIMIT)
	}

	c.hub = newWSHub(c)
	c.onClose(c.hub.close)

	// Counted diagnostics on. A process that installed the dashboard has already
	// accepted per-request instrumentation — that is what the dashboard is — so
	// the middleware counters that are gated off by default are exactly the
	// numbers its operator wants. See diag/counters.go for the gate's cost.
	diag.EnableCounters()
	c.registerDiagnostics()

	// Publish the collector for an MCP endpoint in this process, which reads it
	// directly instead of calling this dashboard's own HTTP API (see local.go).
	c.MakeCurrent()

	// ── Initialize storage and load persisted state ──────────────────────
	c.storage = newStorage(cfg)
	if c.storage != nil {
		c.loadState()
		saveInterval, err := time.ParseDuration(cfg.SaveInterval)
		if err != nil || saveInterval <= 0 {
			saveInterval = time.Minute
		}
		c.onClose(startSaveLoop(c, saveInterval))
	}

	c.registerRoutes(router, app)
	if cfg.Metrics {
		c.onClose(startMetricsSampler(c))
	}
	installedAt := time.Now()

	if !cfg.DisableAuth && !c.authConfigured() {
		authWarnOnce.Do(func() {
			log.Printf("breeze dashboard: %s is locked (HTTP 503) because no credentials are configured — "+
				"set Config.Username and Config.Password, or Config.DisableAuth for a trusted local setup", cfg.BasePath)
		})
	}

	// Register a real health check for the dashboard itself.
	// This verifies the collector and metrics sampler are actually running,
	// not just returning a hardcoded "green".
	c.RegisterHealthCheck("dashboard", func() (string, string) {
		if !cfg.Metrics {
			return "green", fmt.Sprintf("online — %d requests counted (runtime metrics are off)", c.requestsTotal.Load())
		}
		// Metrics() answers with the current time when nothing has been sampled, which
		// made "not yet started" unreachable; ask the history instead.
		snaps := c.MetricsHistory(1)
		if len(snaps) == 0 {
			// The sampler ticks once a second, so a check made just after Install has nothing to
			// look at yet. Past a few seconds with still no sample, it never started.
			if time.Since(installedAt) < 5*time.Second {
				return "green", "starting — the metrics sampler has not ticked yet"
			}
			return "red", "metrics sampler has not produced a sample"
		}
		m := snaps[len(snaps)-1]
		// Check that the metrics sampler has run recently (within last 5s).
		if since := time.Since(m.Time); since > 5*time.Second {
			return "red", "metrics sampler stalled (last sample " + since.Round(time.Millisecond).String() + " ago)"
		}
		return "green", fmt.Sprintf("online — %d requests captured, %d goroutines",
			c.requestsTotal.Load(), m.Goroutines)
	})

	if router != nil {
		installed.Store(router, c)
	}
	return c
}

// installed remembers the collector Install built for each router.
var installed sync.Map // *breeze.Router → *Collector

// authWarnOnce keeps the "locked" notice to one line per process however many dashboards are built.
var authWarnOnce sync.Once

// Middleware is re-exported here so consumers can install the instrumentation
// middleware via router.Use(dashboard.Middleware(coll)) in a single import.
//
// It is identical to the package-level Middleware function but exposed on
// the Collector for ergonomics.
func (c *Collector) Middleware() breeze.HandlerFunc {
	return Middleware(c)
}

// ─── Public push API (used by application code to feed the dashboard) ─────

// PushQuery records a single ORM query. Application ORM adapters should
// call this from their SQL execution path.
func (c *Collector) PushQuery(sql string, args []any, durationUS int64, rows int64, file string, line int, err error) {
	q := QueryRecord{
		ID:         newID(),
		Time:       now(),
		SQL:        sql,
		Args:       args,
		Duration:   durationUS,
		DurationMS: float64(durationUS) / 1000.0,
		Rows:       rows,
		File:       file,
		Line:       line,
		Slow:       c.cfg.SlowQueryMs > 0 && durationUS/1000 >= int64(c.cfg.SlowQueryMs),
	}
	if err != nil {
		q.Error = err.Error()
	}
	c.RecordQuery(q)

	// Query telemetry travels through Breeze's typed event system.
	// The dashboard listens to the same event and forwards it to its live
	// query panel, while the observability bridge records it like any other
	// framework event. When no bus is attached this path is allocation-free.
	if !c.cfg.Enabled || !c.cfg.Queries {
		return
	}
	c.eventsMu.RLock()
	bus := c.eventBus
	c.eventsMu.RUnlock()
	if bus != nil {
		_ = events.EmitBus(bus, events.DatabaseQuery{
			ID:         q.ID,
			Time:       q.Time.UnixNano(),
			SQL:        q.SQL,
			DurationUS: q.Duration,
			Rows:       q.Rows,
			File:       q.File,
			Line:       q.Line,
			Slow:       q.Slow,
			Error:      q.Error,
		})
	}
}

// PushLog records a log entry on the appropriate tab.
func (c *Collector) PushLog(level, message, source string) {
	c.RecordLog(level, LogEntry{
		Time:    now(),
		Message: maskLineWith(c.keysForMasking(), message),
		Source:  source,
	})
}

// traceIDResolver reads the active trace id off a request context.
//
// This is a hook rather than a direct call because fleet imports dashboard (it
// reuses TimelineStep and this Collector), so dashboard cannot import fleet
// back. The fleet package installs this on init; in a build that never imports
// fleet it stays nil and PushLogCtx degrades to PushLog, which is the correct
// behaviour for a single-service app that has no traces to correlate against.
var traceIDResolver func(*breeze.Context) string

// SetTraceIDResolver is called by the fleet package. Not intended for
// application use.
func SetTraceIDResolver(fn func(*breeze.Context) string) { traceIDResolver = fn }

// PushLogCtx is PushLog for a log line emitted while handling a request.
//
// It stamps the entry with the request's trace id, which is what lets §9C.2
// stitch this line into a cross-service trace view later. Without the trace id
// a line is still visible on the Logs page, but it is invisible to the merged
// trace panel — the one place someone debugging a distributed failure is
// actually looking.
//
// Kept separate from PushLog rather than changing that signature: PushLog is
// existing public API, and a log line emitted from a background job or at
// startup has no request to belong to.
func (c *Collector) PushLogCtx(ctx *breeze.Context, level, message, source string) {
	var traceID string
	if ctx != nil && traceIDResolver != nil {
		traceID = traceIDResolver(ctx)
	}
	c.RecordLog(level, LogEntry{
		Time:    now(),
		Message: maskLineWith(c.keysForMasking(), message),
		Source:  source,
		TraceID: traceID,
	})
}

// PushQueueJob records a queued job.
func (c *Collector) PushQueueJob(j QueueJob) { c.RegisterJob(j) }

// PushTask records a scheduler task.
func (c *Collector) PushTask(t SchedulerTask) { c.RegisterTask(t) }

// now returns the current time. It is a function variable so tests can
// override it.
var now = time.Now
