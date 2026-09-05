package dashboard

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nelthaarion/breeze/v2"
	"github.com/nelthaarion/breeze/v2/observability"
)

// Collector is the central aggregation point for every dashboard signal.
//
// A single Collector instance is created by Install() and shared across:
//   - The dashboard middleware (writes requests, timelines, errors)
//   - The ORM query hook (writes query records)
//   - The log hook (writes log entries)
//   - The metrics sampler (writes runtime metrics)
//   - The WebSocket hub (reads snapshots, broadcasts deltas)
//
// The Collector owns a *breeze.Router reference so it can answer the
// Routes Explorer page by inspecting the live router state.
type Collector struct {
	cfg    Config
	router *breeze.Router

	// app is the application the dashboard is installed in, or nil when the
	// Collector was built without one (every test that calls newCollector, and
	// any caller that only wants the recording API).
	//
	// It is held for exactly one reason: the API Explorer has to know which port
	// this service is listening on so it can send its request there and nowhere
	// else. See explorerTarget.
	app *breeze.Breeze

	// hub is set by Install() once the Breeze engine is available.
	hub *wsHub

	// engine is the Breeze TemplateEngine used to render dashboard views.
	engine *breeze.TemplateEngine

	// sessions is the in-memory session store for cookie-based auth.
	sessions *sessionStore

	// Rolling buffers for time-series views.
	requests   *ringBuffer[RequestRecord]
	queries    *ringBuffer[QueryRecord]
	timelines  *ringBuffer[Timeline]
	logsApp    *ringBuffer[LogEntry]
	logsHTTP   *ringBuffer[LogEntry]
	logsErrors *ringBuffer[LogEntry]
	logsPanics *ringBuffer[LogEntry]
	logsWarn   *ringBuffer[LogEntry]
	metrics    *ringBuffer[MetricsSnapshot]

	// Per-route aggregations keyed by "METHOD /pattern".
	routeStatsMu sync.RWMutex
	routeStats   map[string]*routeStatAccumulator

	// Counters.
	//
	// requestsTotal is bumped on every request from every event loop at
	// once, so it sits on a cache line of its own. A cache line is the unit
	// of coherency traffic: sharing one with another counter means each
	// increment steals the line from every core holding it, including cores
	// about to touch a different counter entirely. The counters are
	// logically independent but would physically contend. dayCount below
	// gets the same treatment for the same reason — it is the other
	// every-request counter. See paddedCounter.
	//
	// The rest are cold (sampler-driven or per-session) and can share.
	requestsTotal  paddedCounter
	errorsTotal    atomic.Int64
	activeSessions atomic.Int64

	// Cache stats counters (incremented by middleware/cache.go via hooks).
	cacheHits   atomic.Int64
	cacheMisses atomic.Int64

	// Queue state (set by queue hooks).
	queueMu sync.Mutex
	queue   QueueStat
	jobs    map[string]*QueueJob

	// Scheduler state.
	schedMu sync.Mutex
	tasks   map[string]*SchedulerTask

	// Health checks registered by the application.
	healthMu   sync.RWMutex
	checks     map[string]HealthCheckFunc
	lastHealth map[string]HealthStatus

	// External connections (databases, caches, queues, etc.) for the
	// Architecture visualization page.
	connStore *connectionStore

	// Database inspector used by the database browser.
	dbInspector DBInspector

	// Event observability. eventCol is nil until the application calls
	// AttachEvents; every read path tolerates that, so a dashboard
	// without the event system wired up costs nothing. See events.go.
	eventsMu sync.RWMutex
	eventCol *observability.Collector

	// wfLive tracks workflow executions that are still running. The
	// event ring buffer only ever holds finished executions, so live
	// progress has to be accumulated separately as step events arrive.
	wfLive *workflowLive

	// vidLive tracks video streaming per file. Like wfLive it is nil
	// until explicitly attached, because a server that serves no video
	// should not pay for a tracker or a sweep goroutine.
	vidLive *videoLive

	// vidDetach releases the previous video subscription. It is held so
	// a second AttachVideo replaces the first rather than leaving two
	// trackers counting the same events.
	vidDetach func()

	// Database writer used by the database browser's CRUD UI. Optional —
	// nil means the Database Browser stays read-only regardless of
	// Config.AllowWrites (see writableGuard in api.go).
	dbWriter DBWriter

	// Persistence: storage backend + state tracking.
	storage       Storage
	uniqueIPsMu   sync.RWMutex
	uniqueIPs     map[string]bool
	dailyCountsMu sync.RWMutex
	dailyCounts   map[string]int64

	// dayEpoch and dayCount hold today's tally outside dailyCounts, so the
	// request path can bump it with a pair of atomics instead of formatting
	// a date and taking dailyCountsMu. dayEpoch is the UTC day number
	// (unix/86400); dayCount is the number of requests counted for it that
	// have not yet been folded into the map. See trackDailyCount.
	//
	// dayCount is padded: it is incremented on every request, so it must not
	// share a cache line with requestsTotal. dayEpoch is read-mostly — it
	// only changes at a rollover — so a reader pulling it into shared state
	// costs nothing.
	dayEpoch atomic.Int64
	dayCount paddedCounter
}

// cacheLine is the coherency granule on every architecture Breeze targets.
const cacheLine = 64

// paddedCounter is an atomic.Int64 that occupies a full cache line on its own,
// so incrementing it cannot invalidate a neighbouring counter's line.
//
// This mirrors breeze.paddedCounter in workerpool.go, which carries the full
// rationale. It is duplicated rather than exported because a padded counter is
// an implementation detail of whichever hot path owns it, not an API.
type paddedCounter struct {
	v atomic.Int64
	_ [cacheLine - 8]byte
}

func (c *paddedCounter) Add(n int64) { c.v.Add(n) }

func (c *paddedCounter) Load() int64 { return c.v.Load() }

func (c *paddedCounter) Store(n int64) { c.v.Store(n) }

func (c *paddedCounter) Swap(n int64) int64 { return c.v.Swap(n) }

// routeStatAccumulator aggregates per-route metrics.
type routeStatAccumulator struct {
	method      string
	pattern     string
	controller  string
	middleware  []string
	requests    atomic.Int64
	totalDurUS  atomic.Int64 // sum of durations in microseconds
	maxDurUS    atomic.Int64
	lastRequest atomic.Int64 // unix nanos
	errors      atomic.Int64
}

// HealthCheckFunc is a named probe returning a status string ("green",
// "yellow", "red") and a human-readable message.
type HealthCheckFunc func() (status, message string)

// NewCollector creates a Collector bound to the given router.
func newCollector(cfg Config, router *breeze.Router) *Collector {
	cfg = cfg.withDefaults()
	return &Collector{
		cfg:         cfg,
		router:      router,
		requests:    newRingBuffer[RequestRecord](cfg.MaxRequests),
		queries:     newRingBuffer[QueryRecord](cfg.MaxQueries),
		timelines:   newRingBuffer[Timeline](cfg.MaxRequests),
		logsApp:     newRingBuffer[LogEntry](cfg.MaxLogs),
		logsHTTP:    newRingBuffer[LogEntry](cfg.MaxLogs),
		logsErrors:  newRingBuffer[LogEntry](cfg.MaxLogs),
		logsPanics:  newRingBuffer[LogEntry](cfg.MaxLogs),
		logsWarn:    newRingBuffer[LogEntry](cfg.MaxLogs),
		metrics:     newRingBuffer[MetricsSnapshot](600), // 10min @ 1Hz
		routeStats:  make(map[string]*routeStatAccumulator),
		jobs:        make(map[string]*QueueJob),
		tasks:       make(map[string]*SchedulerTask),
		checks:      make(map[string]HealthCheckFunc),
		lastHealth:  make(map[string]HealthStatus),
		connStore:   newConnectionStore(),
		uniqueIPs:   make(map[string]bool),
		dailyCounts: make(map[string]int64),
	}
}

// DBInspector exposes the current cached database inspector, if one was set.
func (c *Collector) DBInspector() DBInspector {
	return c.dbInspector
}

// SetDBInspector installs a database inspector behind a cache layer.
// Passing nil clears the inspector.
func (c *Collector) SetDBInspector(inspector DBInspector) {
	if inspector == nil {
		c.dbInspector = nil
		return
	}
	c.dbInspector = newCachedDBInspector(inspector, 30*time.Second)
}

// DBWriter exposes the current database writer, if one was set.
func (c *Collector) DBWriter() DBWriter {
	return c.dbWriter
}

// SetDBWriter installs a database writer, enabling Create/Update/Delete in
// the Database Browser (still gated by Config.AllowWrites). Passing nil
// clears the writer and reverts the Database Browser to read-only.
func (c *Collector) SetDBWriter(w DBWriter) {
	c.dbWriter = w
}

// invalidateTableCache clears the cached Database Browser rows for table
// after a successful write. It is a no-op if no inspector is configured.
func (c *Collector) invalidateTableCache(table string) {
	if ci, ok := c.dbInspector.(*cachedDBInspector); ok {
		ci.Invalidate(table)
	}
}

// Config returns the active configuration.
func (c *Collector) Config() Config { return c.cfg }

// trackUniqueIP records a client IP for unique-viewer counting.
// The set is persisted to storage on save.
//
// The common case — an IP already in the set — is served under a read lock, so
// concurrent event loops do not serialize on it. Only a genuinely new IP takes
// the write lock. The earlier version took the write lock unconditionally, which
// put a global exclusive mutex on the request path of every deployment sitting
// behind a proxy (i.e. every deployment that sends X-Forwarded-For).
func (c *Collector) trackUniqueIP(ip string) {
	if ip == "" {
		return
	}
	c.uniqueIPsMu.RLock()
	seen := c.uniqueIPs[ip]
	c.uniqueIPsMu.RUnlock()
	if seen {
		return
	}

	c.uniqueIPsMu.Lock()
	if !c.uniqueIPs[ip] {
		// Re-checked under the write lock: two goroutines can both miss
		// the read above for the same new IP.
		//
		// Clone on insert. ip is sliced out of the X-Forwarded-For
		// header, so it is a view into the bytes the request was parsed
		// from — and with breeze's SetZeroCopyHeaders those bytes are
		// the connection's read buffer, reused for the next read.
		//
		// A map key that mutates after insertion is worse than a stale
		// string: it stays in the bucket its original hash chose, so
		// every later lookup of that IP misses, the set grows without
		// bound, and UniqueIPCount drifts upward forever.
		//
		// Assigning over an existing key would not help — Go keeps the
		// key already in the map — which is why this clones on insert
		// rather than on every call.
		c.uniqueIPs[strings.Clone(ip)] = true
	}
	c.uniqueIPsMu.Unlock()
}

// UniqueIPCount returns the number of unique client IPs seen.
func (c *Collector) UniqueIPCount() int {
	c.uniqueIPsMu.RLock()
	defer c.uniqueIPsMu.RUnlock()
	return len(c.uniqueIPs)
}

// trackDailyCount increments today's request count.
//
// This sits on the collector's fast path — ahead of the "is anyone watching"
// check — so it runs for every request the server handles, dashboard open or
// not. It therefore has to cost close to nothing.
//
// What it replaced cost a great deal:
//
//	today := time.Now().UTC().Format("2006-01-02")
//	c.dailyCountsMu.Lock()
//	c.dailyCounts[today]++
//	c.dailyCountsMu.Unlock()
//
// Format allocates a string and walks the layout on every call, and the lock
// funnelled every event loop in the process through one mutex. A critical
// section holding a single increment is the worst shape a lock can have: the
// cores spend their time moving that one cache line between each other instead
// of serving connections, and throughput stops scaling with cores no matter how
// fast the rest of the request path gets.
//
// The fast path here is a clock read, an integer divide, and two atomics on a
// counter nothing else touches. The formatting and the map write happen in
// rollDay, once per UTC day.
//
// Requests racing a midnight rollover can land on either side of it: a counter
// swapped out by rollDay may still take a few increments meant for the new day.
// This is a tally behind a dashboard chart, so a handful of requests on the
// wrong side of midnight does not justify a mutex on every request.
func (c *Collector) trackDailyCount() {
	day := time.Now().Unix() / 86400
	if c.dayEpoch.Load() == day {
		c.dayCount.Add(1)
		return
	}
	c.rollDay(day)
}

// rollDay folds the finished day's tally into dailyCounts and starts counting
// the new one. Reached once per UTC day, plus once on the first request.
func (c *Collector) rollDay(day int64) {
	c.dailyCountsMu.Lock()
	if prev := c.dayEpoch.Load(); prev != day {
		// Re-checked under the lock: several goroutines can see the
		// stale epoch at once, and only the first should roll it.
		if prev != 0 {
			c.dailyCounts[dayKey(prev)] += c.dayCount.Swap(0)
		} else {
			c.dayCount.Store(0)
		}
		c.dayEpoch.Store(day)
	}
	c.dailyCountsMu.Unlock()
	c.dayCount.Add(1)
}

// dayKey renders a UTC day number (unix/86400) as the "2006-01-02" key used by
// dailyCounts and by the persisted state.
func dayKey(day int64) string {
	return time.Unix(day*86400, 0).UTC().Format("2006-01-02")
}

// TodayCount returns today's request count, including the increments still
// sitting in the unflushed counter.
func (c *Collector) TodayCount() int64 {
	day := time.Now().Unix() / 86400
	c.dailyCountsMu.RLock()
	stored := c.dailyCounts[dayKey(day)]
	c.dailyCountsMu.RUnlock()
	if c.dayEpoch.Load() == day {
		stored += c.dayCount.Load()
	}
	return stored
}

// DailyCounts returns a copy of all daily counts with the unflushed counter
// folded into its day.
//
// Readers must go through this rather than ranging over c.dailyCounts, or they
// will miss everything counted since the last rollover — which, on any given
// day, is every request that day.
func (c *Collector) DailyCounts() map[string]int64 {
	day := c.dayEpoch.Load()
	live := c.dayCount.Load()
	c.dailyCountsMu.RLock()
	out := make(map[string]int64, len(c.dailyCounts)+1)
	for k, v := range c.dailyCounts {
		out[k] = v
	}
	c.dailyCountsMu.RUnlock()
	if day != 0 {
		out[dayKey(day)] += live
	}
	return out
}

// ─── Request collection ───────────────────────────────────────────────────

// RecordRequest appends a request record to the ring buffer.
//
// NOTE: This does NOT update the request counter or per-route stats —
// those are done by the middleware's fast path (counter) and slow path
// (route stats) respectively. RecordRequest only pushes the full record
// to the ring buffer for the Live Requests page.
func (c *Collector) RecordRequest(r RequestRecord) {
	if !c.cfg.Enabled || !c.cfg.Requests {
		return
	}
	c.requests.Push(r)
}

// RecordQuery appends an ORM query record.
func (c *Collector) RecordQuery(q QueryRecord) {
	if !c.cfg.Enabled || !c.cfg.Queries {
		return
	}
	if c.cfg.SlowQueryMs > 0 && q.DurationMS >= float64(c.cfg.SlowQueryMs) {
		q.Slow = true
	}
	c.queries.Push(q)
}

// RecordTimeline appends a per-request timeline.
func (c *Collector) RecordTimeline(t Timeline) {
	if !c.cfg.Enabled || !c.cfg.Timeline {
		return
	}
	c.timelines.Push(t)
}

// RecordLog appends a log entry to the appropriate tab.
func (c *Collector) RecordLog(level string, e LogEntry) {
	if !c.cfg.Enabled {
		return
	}
	e.Level = level
	switch level {
	case "app":
		c.logsApp.Push(e)
	case "http":
		c.logsHTTP.Push(e)
	case "error":
		c.logsErrors.Push(e)
	case "panic":
		c.logsPanics.Push(e)
	case "warning":
		c.logsWarn.Push(e)
	}
}

// RecordCacheHit increments cache hit/miss counters.
func (c *Collector) RecordCacheHit(hit bool) {
	if !c.cfg.Enabled {
		return
	}
	if hit {
		c.cacheHits.Add(1)
	} else {
		c.cacheMisses.Add(1)
	}
}

// ─── Snapshots ────────────────────────────────────────────────────────────

// Requests returns a snapshot of the latest N request records.
func (c *Collector) Requests(n int) []RequestRecord {
	all := c.requests.Snapshot()
	if n <= 0 || n >= len(all) {
		return all
	}
	return all[len(all)-n:]
}

// Queries returns a snapshot of the latest N query records.
func (c *Collector) Queries(n int) []QueryRecord {
	all := c.queries.Snapshot()
	if n <= 0 || n >= len(all) {
		return all
	}
	return all[len(all)-n:]
}

// Timelines returns a snapshot of recent timelines.
func (c *Collector) Timelines(n int) []Timeline {
	all := c.timelines.Snapshot()
	if n <= 0 || n >= len(all) {
		return all
	}
	return all[len(all)-n:]
}

// Logs returns the requested log tab snapshot.
func (c *Collector) Logs(level string, n int) []LogEntry {
	var buf *ringBuffer[LogEntry]
	switch level {
	case "app":
		buf = c.logsApp
	case "http":
		buf = c.logsHTTP
	case "error":
		buf = c.logsErrors
	case "panic":
		buf = c.logsPanics
	case "warning":
		buf = c.logsWarn
	default:
		return nil
	}
	all := buf.Snapshot()
	if n <= 0 || n >= len(all) {
		return all
	}
	return all[len(all)-n:]
}

// RouteStats returns aggregated per-route statistics.
func (c *Collector) RouteStats() []RouteStat {
	c.routeStatsMu.RLock()
	defer c.routeStatsMu.RUnlock()
	out := make([]RouteStat, 0, len(c.routeStats))
	for _, acc := range c.routeStats {
		reqs := acc.requests.Load()
		dur := acc.totalDurUS.Load()
		avg := 0.0
		if reqs > 0 {
			avg = float64(dur) / float64(reqs) / 1000.0
		}
		last := ""
		if t := acc.lastRequest.Load(); t > 0 {
			last = time.Unix(0, t).UTC().Format(time.RFC3339)
		}
		out = append(out, RouteStat{
			Method:       acc.method,
			Pattern:      acc.pattern,
			Controller:   acc.controller,
			Middleware:   acc.middleware,
			Requests:     reqs,
			AvgLatencyMS: avg,
			MaxLatencyMS: float64(acc.maxDurUS.Load()) / 1000.0,
			LastRequest:  last,
			Errors:       acc.errors.Load(),
		})
	}
	return out
}

// Metrics returns the latest metrics snapshot, or a zero snapshot if none yet.
func (c *Collector) Metrics() MetricsSnapshot {
	snaps := c.metrics.Snapshot()
	if len(snaps) == 0 {
		return MetricsSnapshot{Time: time.Now()}
	}
	return snaps[len(snaps)-1]
}

// MetricsHistory returns up to n recent metrics snapshots.
func (c *Collector) MetricsHistory(n int) []MetricsSnapshot {
	all := c.metrics.Snapshot()
	if n <= 0 || n >= len(all) {
		return all
	}
	return all[len(all)-n:]
}

// CacheStats returns the current cache hit/miss counters and computed ratio.
func (c *Collector) CacheStats() CacheStat {
	hits := c.cacheHits.Load()
	misses := c.cacheMisses.Load()
	total := hits + misses
	ratio := 0.0
	if total > 0 {
		ratio = float64(hits) / float64(total)
	}
	return CacheStat{
		Driver:  "memory",
		Keys:    total,
		Hits:    hits,
		Misses:  misses,
		HitRate: ratio,
	}
}

// ─── Queue (set by application) ───────────────────────────────────────────

// UpdateQueue replaces the queue summary.
func (c *Collector) UpdateQueue(s QueueStat) {
	c.queueMu.Lock()
	defer c.queueMu.Unlock()
	c.queue = s
}

// QueueStats returns the current queue summary.
func (c *Collector) QueueStats() QueueStat {
	c.queueMu.Lock()
	defer c.queueMu.Unlock()
	return c.queue
}

// RegisterJob records a queued job.
func (c *Collector) RegisterJob(j QueueJob) {
	c.queueMu.Lock()
	defer c.queueMu.Unlock()
	c.jobs[j.ID] = &j
}

// UpdateJob updates a job's state.
func (c *Collector) UpdateJob(id, state string) {
	c.queueMu.Lock()
	defer c.queueMu.Unlock()
	if j, ok := c.jobs[id]; ok {
		j.State = state
		if state == "running" {
			j.StartedAt = time.Now()
		}
		if state == "completed" || state == "failed" {
			j.FinishedAt = time.Now()
			if !j.StartedAt.IsZero() {
				j.DurationMS = float64(j.FinishedAt.Sub(j.StartedAt).Microseconds()) / 1000.0
			}
		}
	}
}

// RetryJob resets a failed job back to pending.
func (c *Collector) RetryJob(id string) bool {
	c.queueMu.Lock()
	defer c.queueMu.Unlock()
	if j, ok := c.jobs[id]; ok {
		j.State = "pending"
		j.Attempts++
		j.Error = ""
		j.StartedAt = time.Time{}
		j.FinishedAt = time.Time{}
		j.DurationMS = 0
		return true
	}
	return false
}

// Jobs returns all known jobs.
func (c *Collector) Jobs() []QueueJob {
	c.queueMu.Lock()
	defer c.queueMu.Unlock()
	out := make([]QueueJob, 0, len(c.jobs))
	for _, j := range c.jobs {
		out = append(out, *j)
	}
	return out
}

// ─── Scheduler (set by application) ───────────────────────────────────────

// RegisterTask registers or updates a scheduled task entry.
func (c *Collector) RegisterTask(t SchedulerTask) {
	c.schedMu.Lock()
	defer c.schedMu.Unlock()
	c.tasks[t.Name] = &t
}

// Tasks returns all known scheduler tasks.
func (c *Collector) Tasks() []SchedulerTask {
	c.schedMu.Lock()
	defer c.schedMu.Unlock()
	out := make([]SchedulerTask, 0, len(c.tasks))
	for _, t := range c.tasks {
		out = append(out, *t)
	}
	return out
}

// ─── Health checks ────────────────────────────────────────────────────────

// RegisterHealthCheck registers a named health probe.
func (c *Collector) RegisterHealthCheck(name string, fn HealthCheckFunc) {
	c.healthMu.Lock()
	defer c.healthMu.Unlock()
	c.checks[name] = fn
}

// RunHealthChecks runs every registered probe and caches the result.
func (c *Collector) RunHealthChecks() []HealthStatus {
	c.healthMu.RLock()
	defer c.healthMu.RUnlock()
	out := make([]HealthStatus, 0, len(c.checks))
	now := time.Now()
	for name, fn := range c.checks {
		status, msg := "red", "no probe"
		lat := time.Now()
		if fn != nil {
			status, msg = fn()
		}
		latency := time.Since(lat)
		h := HealthStatus{
			Name:    name,
			Status:  status,
			Message: msg,
			Latency: latency.Microseconds(),
			Checked: now,
		}
		c.lastHealth[name] = h
		out = append(out, h)
	}
	return out
}
