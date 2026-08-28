package dashboard

import "time"

// RequestRecord captures one HTTP request as seen by the dashboard middleware.
type RequestRecord struct {
	ID         string            `json:"id"`
	Time       time.Time         `json:"time"`
	Method     string            `json:"method"`
	Path       string            `json:"path"`
	Route      string            `json:"route"` // matched route pattern, "" if no match
	Status     int               `json:"status"`
	Duration   int64             `json:"duration_us"` // microseconds
	DurationMS float64           `json:"duration_ms"`
	IP         string            `json:"ip"`
	User       string            `json:"user"` // extracted from ctx store ("user") if present
	UserAgent  string            `json:"user_agent"`
	RespSize   int               `json:"resp_size"`
	Headers    map[string]string `json:"headers,omitempty"` // masked
	Error      string            `json:"error,omitempty"`
	TimelineID string            `json:"timeline_id,omitempty"`
}

// QueryRecord captures one SQL statement executed by the ORM.
type QueryRecord struct {
	ID         string    `json:"id"`
	Time       time.Time `json:"time"`
	SQL        string    `json:"sql"`
	Args       []any     `json:"args,omitempty"`
	Duration   int64     `json:"duration_us"`
	DurationMS float64   `json:"duration_ms"`
	Rows       int64     `json:"rows"`
	File       string    `json:"file"`
	Line       int       `json:"line"`
	RowsRead   int64     `json:"rows_read,omitempty"`
	Slow       bool      `json:"slow"`
	Error      string    `json:"error,omitempty"`
}

// TableInfo describes one database table in the dashboard database browser.
type TableInfo struct {
	Name string `json:"name"`
	Rows int64  `json:"rows"`
}

// TableColumn describes one column in a database table.
type TableColumn struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	PrimaryKey bool   `json:"primary_key,omitempty"`
	Nullable   bool   `json:"nullable,omitempty"`
}

// TableData is one paginated table view used by the database browser.
type TableData struct {
	Table    string `json:"table"`
	Page     int    `json:"page"`
	PageSize int    `json:"page_size"`
	Total    int64  `json:"total"`
	// Writable is true when the Database Browser's Create/Update/Delete UI
	// is enabled for this response — i.e. Config.AllowWrites is true AND
	// a DBWriter is configured. The frontend uses this as the single
	// source of truth for whether to render edit controls; it never
	// infers writability on its own.
	Writable bool             `json:"writable,omitempty"`
	Columns  []TableColumn    `json:"columns,omitempty"`
	Rows     []map[string]any `json:"rows,omitempty"`
}

// RouteStat aggregates per-route metrics.
type RouteStat struct {
	Method       string   `json:"method"`
	Pattern      string   `json:"pattern"`
	Controller   string   `json:"controller"`
	Middleware   []string `json:"middleware"`
	Requests     int64    `json:"requests"`
	AvgLatencyMS float64  `json:"avg_latency_ms"`
	MaxLatencyMS float64  `json:"max_latency_ms"`
	LastRequest  string   `json:"last_request"` // RFC3339, "" if never
	Errors       int64    `json:"errors"`
}

// LogEntry is a single line in the dashboard log viewer.
type LogEntry struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"` // app / http / error / panic / warning
	Message string    `json:"message"`
	Source  string    `json:"source,omitempty"` // file:line when known
	TraceID string    `json:"trace_id,omitempty"`
}

// CacheStat is a snapshot of cache driver statistics.
type CacheStat struct {
	Driver  string  `json:"driver"`
	Keys    int64   `json:"keys"`
	Hits    int64   `json:"hits"`
	Misses  int64   `json:"misses"`
	Memory  int64   `json:"memory_bytes"`
	HitRate float64 `json:"hit_rate"`
}

// QueueStat is a snapshot of the queue subsystem.
type QueueStat struct {
	Pending   int `json:"pending"`
	Running   int `json:"running"`
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
}

// QueueJob is one queued job with its current state.
type QueueJob struct {
	ID         string    `json:"id"`
	Queue      string    `json:"queue"`
	Payload    string    `json:"payload"`
	State      string    `json:"state"` // pending / running / completed / failed
	Attempts   int       `json:"attempts"`
	Error      string    `json:"error,omitempty"`
	QueuedAt   time.Time `json:"queued_at"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	DurationMS float64   `json:"duration_ms,omitempty"`
}

// SchedulerTask describes one registered scheduled job.
type SchedulerTask struct {
	Name      string    `json:"name"`
	Cron      string    `json:"cron"`
	NextRun   time.Time `json:"next_run"`
	LastRun   time.Time `json:"last_run,omitempty"`
	LastRunMS float64   `json:"last_run_ms,omitempty"`
	Status    string    `json:"status"` // idle / running / failed
	LastError string    `json:"last_error,omitempty"`
	RunCount  int64     `json:"run_count"`
	FailCount int64     `json:"fail_count"`
}

// HealthStatus is the result of one health check.
type HealthStatus struct {
	Name    string    `json:"name"`
	Status  string    `json:"status"` // green / yellow / red
	Latency int64     `json:"latency_us"`
	Message string    `json:"message,omitempty"`
	Checked time.Time `json:"checked"`
}

// TimelineStep is one entry in a per-request timeline.
type TimelineStep struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Start    time.Time      `json:"start"`
	End      time.Time      `json:"end"`
	Duration int64          `json:"duration_us"` // End - Start
	Metadata map[string]any `json:"metadata,omitempty"`
	Children []TimelineStep `json:"children,omitempty"`
}

// Timeline is the full per-request timeline.
type Timeline struct {
	ID     string         `json:"id"`
	Time   time.Time      `json:"time"`
	Method string         `json:"method"`
	Path   string         `json:"path"`
	Status int            `json:"status"`
	Total  int64          `json:"total_us"`
	Steps  []TimelineStep `json:"steps"`
}

// MetricsSnapshot is one point of Go runtime + HTTP metrics time-series.
//
// Every field is a SNAPSHOT of the current value at sample time — never
// cumulative (except TotalAlloc, Mallocs, Frees, NumGC, PauseTotalNs which
// are inherently cumulative counters from the Go runtime).
//
// HeapAlloc vs TotalAlloc vs HeapSys vs Sys:
//   - HeapAlloc: bytes of live heap objects (DROPS after GC)
//   - TotalAlloc: cumulative bytes allocated over all time (only grows)
//   - HeapSys: bytes obtained from OS for the heap (slowly released)
//   - Sys: total bytes obtained from OS (HeapSys + StackSys + MSpan + ...)
//
// These must NEVER be confused. Charts must use the correct field:
//   - "Heap Allocation" chart → HeapAlloc (drops after GC)
//   - "Memory from OS" chart → Sys (slowly released)
//   - NEVER plot TotalAlloc as "current memory"
type MetricsSnapshot struct {
	Time time.Time `json:"time"`
	// HTTP
	RequestsTotal  int64   `json:"requests_total"`
	RequestsToday  int64   `json:"requests_today"`
	RequestsPerSec float64 `json:"requests_per_sec"`
	AvgRespTimeMS  float64 `json:"avg_resp_time_ms"`
	ErrorRate      float64 `json:"error_rate"`
	ActiveSessions int64   `json:"active_sessions"`
	// Runtime — heap (all from runtime.MemStats)
	Goroutines   int    `json:"goroutines"`
	HeapAlloc    uint64 `json:"heap_alloc"`    // live heap bytes (drops after GC)
	HeapSys      uint64 `json:"heap_sys"`      // heap bytes from OS
	HeapIdle     uint64 `json:"heap_idle"`     // idle heap bytes
	HeapInuse    uint64 `json:"heap_inuse"`    // in-use heap bytes
	HeapReleased uint64 `json:"heap_released"` // bytes returned to OS
	HeapObjects  uint64 `json:"heap_objects"`  // count of live heap objects
	TotalAlloc   uint64 `json:"total_alloc"`   // cumulative bytes allocated (only grows)
	Mallocs      uint64 `json:"mallocs"`       // cumulative malloc count (only grows)
	Frees        uint64 `json:"frees"`         // cumulative free count (only grows)
	// Runtime — stack
	StackInUse uint64 `json:"stack_in_use"`
	StackSys   uint64 `json:"stack_sys"`
	// Runtime — other system memory
	MSpanInuse  uint64 `json:"mspan_inuse"`
	MSpanSys    uint64 `json:"mspan_sys"`
	MCacheInuse uint64 `json:"mcache_inuse"`
	MCacheSys   uint64 `json:"mcache_sys"`
	BuckHashSys uint64 `json:"buck_hash_sys"`
	OtherSys    uint64 `json:"other_sys"`
	// Runtime — total from OS
	Sys uint64 `json:"sys"` // total bytes from OS (MemStats.Sys)
	// GC
	NumGC         uint32    `json:"num_gc"`
	LastGC        time.Time `json:"last_gc"`
	NextGC        uint64    `json:"next_gc"`         // target heap size for next GC
	PauseTotalNs  uint64    `json:"pause_total_ns"`  // cumulative GC pause time
	PauseNs       uint64    `json:"pause_ns"`        // most recent GC pause duration
	GCCPUFraction float64   `json:"gc_cpu_fraction"` // fraction of CPU used by GC
	GCEnabled     bool      `json:"gc_enabled"`
	// CPU
	CPUUsage   float64 `json:"cpu_usage"`
	NumCPU     int     `json:"num_cpu"`
	GOMAXPROCS int     `json:"gomaxprocs"`
	CGOCalls   int64   `json:"cgo_calls"`
	// Database
	DBConnections int `json:"db_connections"`
	// Cache
	CacheHitRatio float64 `json:"cache_hit_ratio"`
	// Queue
	QueueJobs int `json:"queue_jobs"`
}

// PerfMetrics is the detailed runtime metrics snapshot used by the Performance page.
// It is built from a FRESH runtime.ReadMemStats() call — not the cached
// MetricsSnapshot — so the Performance page always shows current data.
type PerfMetrics struct {
	Time       time.Time    `json:"time"`
	Goroutines int          `json:"goroutines"`
	Heap       HeapStats    `json:"heap"`
	Stack      StackStats   `json:"stack"`
	OffHeap    OffHeapStats `json:"off_heap"`
	GC         GCStats      `json:"gc"`
	Allocs     AllocStats   `json:"allocs"`
	Memory     MemoryStats  `json:"memory"`
	CPU        CPUStats     `json:"cpu"`
	Network    NetworkStats `json:"network"`
	// RuntimeTuning shows the current GOGC and GOMEMLIMIT values applied
	// to the Go runtime. This helps users verify that their memory
	// settings are taking effect.
	RuntimeTuning RuntimeTuning `json:"runtime_tuning"`
}

// RuntimeTuning exposes the current Go runtime memory settings.
type RuntimeTuning struct {
	GOGC       int   `json:"gogc"`       // GC trigger percentage (-1 = disabled)
	GOMEMLIMIT int64 `json:"gomemlimit"` // soft memory limit in bytes (0 = no limit)
}

type HeapStats struct {
	Alloc      uint64 `json:"alloc"`       // HeapAlloc — live heap bytes (drops after GC)
	TotalAlloc uint64 `json:"total_alloc"` // cumulative bytes allocated (only grows)
	Sys        uint64 `json:"sys"`         // HeapSys — heap bytes from OS
	Idle       uint64 `json:"idle"`        // HeapIdle — idle heap bytes
	Inuse      uint64 `json:"inuse"`       // HeapInuse — in-use heap bytes
	Released   uint64 `json:"released"`    // HeapReleased — bytes returned to OS
	Objects    uint64 `json:"objects"`     // HeapObjects — count of live heap objects
	Lookups    uint64 `json:"lookups"`     // Lookups
	Mallocs    uint64 `json:"mallocs"`     // Mallocs — cumulative count (only grows)
	Frees      uint64 `json:"frees"`       // Frees — cumulative count (only grows)
}

type StackStats struct {
	InUse uint64 `json:"in_use"` // StackInuse
	Sys   uint64 `json:"sys"`    // StackSys
}

type OffHeapStats struct {
	MSpanInuse  uint64 `json:"mspan_inuse"`
	MSpanSys    uint64 `json:"mspan_sys"`
	MCacheInuse uint64 `json:"mcache_inuse"`
	MCacheSys   uint64 `json:"mcache_sys"`
	BuckHashSys uint64 `json:"buck_hash_sys"`
	OtherSys    uint64 `json:"other_sys"`
}

type GCStats struct {
	NumGC        uint32    `json:"num_gc"`
	LastGC       time.Time `json:"last_gc"`
	NextGC       uint64    `json:"next_gc"`        // target heap size for next GC
	PauseTotalNS uint64    `json:"pause_total_ns"` // cumulative pause (only grows)
	PauseNS      uint64    `json:"pause_ns"`       // most recent GC pause duration
	CPUFraction  float64   `json:"cpu_fraction"`   // GCCPUFraction
	Enabled      bool      `json:"enabled"`
}

type AllocStats struct {
	TotalAlloc uint64 `json:"total_alloc"`  // cumulative bytes allocated
	Mallocs    uint64 `json:"mallocs"`      // cumulative malloc count
	Frees      uint64 `json:"frees"`        // cumulative free count
	BytesPerOp uint64 `json:"bytes_per_op"` // TotalAlloc / (Mallocs+1) — derived
}

type MemoryStats struct {
	Sys          uint64  `json:"sys"`           // MemStats.Sys — total from OS
	HeapInUse    uint64  `json:"heap_in_use"`   // HeapAlloc (live heap)
	HeapIdle     uint64  `json:"heap_idle"`     // HeapIdle
	HeapReleased uint64  `json:"heap_released"` // HeapReleased
	StackInUse   uint64  `json:"stack_in_use"`
	Other        uint64  `json:"other"`     // Sys - HeapInuse - StackInUse
	UsagePct     float64 `json:"usage_pct"` // HeapInUse / Sys * 100
}

type CPUStats struct {
	NumCPU       int     `json:"num_cpu"`
	GOMAXPROCS   int     `json:"gomaxprocs"`
	CGOCalls     int64   `json:"cgo_calls"`
	UsagePct     float64 `json:"usage_pct"`
	UserTimeNS   int64   `json:"user_time_ns"`
	SystemTimeNS int64   `json:"system_time_ns"`
}

type NetworkStats struct {
	Connections   int   `json:"connections"`
	WebSocketOpen int64 `json:"websocket_open"`
	BytesIn       int64 `json:"bytes_in"`
	BytesOut      int64 `json:"bytes_out"`
}
