package dashboard

import (
	"fmt"
	"runtime/debug"
	"time"

	"github.com/nelthaarion/breeze/v2"
	"github.com/nelthaarion/breeze/v2/diag"
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
// second call is a no-op). The middleware returned by Middleware(c) should
// be installed via router.Use() before any application routes are added so
// every request is instrumented.
//
// Usage:
//
//	app := breeze.New(router, pool)
//	coll := dashboard.Install(app, router, dashboard.DefaultConfig())
//	router.Use(dashboard.Middleware(coll))
//	// ... register application routes ...
//	app.Run(3000, true)
func Install(app *breeze.Breeze, router *breeze.Router, cfg Config) *Collector {
	cfg = cfg.withDefaults()

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
	if cfg.GOGC != 0 {
		debug.SetGCPercent(cfg.GOGC)
	}
	if cfg.GOMEMLIMIT > 0 {
		debug.SetMemoryLimit(cfg.GOMEMLIMIT)
	}

	c := newCollector(cfg, router)
	c.app = app
	c.hub = newWSHub(c)

	// Counted diagnostics on. A process that installed the dashboard has already
	// accepted per-request instrumentation — that is what the dashboard is — so
	// the middleware counters that are gated off by default are exactly the
	// numbers its operator wants. See diag/counters.go for the gate's cost.
	diag.EnableCounters()
	c.registerDiagnostics()

	// ── Initialize storage and load persisted state ──────────────────────
	c.storage = newStorage(cfg)
	if c.storage != nil {
		c.loadState()
		saveInterval, err := time.ParseDuration(cfg.SaveInterval)
		if err != nil || saveInterval <= 0 {
			saveInterval = time.Minute
		}
		startSaveLoop(c, saveInterval)
	}

	c.registerRoutes(router, app)
	startMetricsSampler(c)

	// Register a real health check for the dashboard itself.
	// This verifies the collector and metrics sampler are actually running,
	// not just returning a hardcoded "green".
	c.RegisterHealthCheck("dashboard", func() (string, string) {
		if !c.cfg.Enabled {
			return "yellow", "dashboard is disabled in config"
		}
		m := c.Metrics()
		if m.Time.IsZero() {
			return "yellow", "metrics sampler not yet started"
		}
		// Check that the metrics sampler has run recently (within last 5s).
		if time.Since(m.Time) > 5*time.Second {
			return "red", "metrics sampler stalled (last sample " + time.Since(m.Time).String() + " ago)"
		}
		return "green", fmt.Sprintf("online — %d requests captured, %d goroutines",
			c.requestsTotal.Load(), m.Goroutines)
	})

	return c
}

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
	if c.hub != nil {
		pushEvent(c.hub, "query", q)
	}
}

// PushLog records a log entry on the appropriate tab.
func (c *Collector) PushLog(level, message, source string) {
	c.RecordLog(level, LogEntry{
		Time:    now(),
		Message: maskLine(c.cfg, message),
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
		Message: maskLine(c.cfg, message),
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
