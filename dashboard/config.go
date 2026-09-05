package dashboard

// Config controls every aspect of the Developer Dashboard.
//
// When Enabled is false the dashboard middleware installed via Install()
// short-circuits before doing any work, so production deployments pay zero
// overhead. Individual collectors can be toggled independently so teams can
// turn on only the signals they care about (e.g. timeline + queries, but not
// full per-request logging).
//
// Example YAML configuration:
//
//	dashboard:
//	  enabled: true
//	  timeline: true
//	  queries: true
//	  metrics: true
//	  requests: true
//	  base_path: "/dashboard"
//	  username: "admin"
//	  password: "s3cret"
//	  max_requests: 1000
//	  max_queries: 500
//	  max_logs: 1000
//	  slow_query_ms: 100
type Config struct {
	// Enabled gates the entire dashboard. When false, the middleware and
	// WebSocket hub are no-ops and the SPA is not served.
	Enabled bool `yaml:"enabled" json:"enabled"`

	// Timeline toggles the per-request timeline profiler.
	Timeline bool `yaml:"timeline" json:"timeline"`

	// Queries toggles the ORM/SQL query monitor.
	Queries bool `yaml:"queries" json:"queries"`

	// Metrics toggles Go runtime metrics collection (GC, heap, goroutines).
	Metrics bool `yaml:"metrics" json:"metrics"`

	// Requests toggles live request tracking.
	Requests bool `yaml:"requests" json:"requests"`

	// BasePath is the URL prefix where the dashboard SPA is served.
	// Default: "/dashboard".
	BasePath string `yaml:"base_path" json:"base_path"`

	// Username and Password are the HTTP Basic Auth credentials required
	// to access the dashboard. Both must be non-empty for authentication
	// to be enforced; if either is empty, the dashboard is open. This is
	// a footgun intended only for local development.
	Username string `yaml:"username" json:"username"`
	Password string `yaml:"password" json:"password"`

	// FleetAggregatorURL activates Fleet View and is the read-side base URL
	// of a fleet.Aggregator (for example http://fleet:9000/fleet). Empty keeps
	// the page and all proxy routes disabled, so existing dashboards allocate
	// and dial nothing.
	FleetAggregatorURL      string `yaml:"fleet_aggregator_url" json:"fleet_aggregator_url"`
	FleetAggregatorUsername string `yaml:"fleet_aggregator_username" json:"-"`
	FleetAggregatorPassword string `yaml:"fleet_aggregator_password" json:"-"`

	// ServiceToken guards trace-correlated log fan-out. The token is compared
	// against X-Fleet-Token by the logs endpoint and never sent to browsers.
	ServiceToken string `yaml:"service_token" json:"-"`

	// DisableAuth turns off authentication entirely, even if Username and
	// Password are set. Useful for local development or when auth is
	// handled by a reverse proxy. Defaults to false.
	DisableAuth bool `yaml:"disable_auth" json:"disable_auth"`

	// DevMode links the readable client bundles instead of the minified ones.
	//
	// Defaults to false, so a dashboard serves dashboard.min.css and
	// dashboard.min.js — ~55KB less per page load than the commented sources.
	// Both files are always present under /assets; this only decides what the
	// layout references, so a developer can still open the readable copy by URL.
	//
	// Named to match breeze.TemplateConfig.DevMode, which makes the same choice
	// for the SPA runtime, because they are the same decision.
	DevMode bool `yaml:"dev_mode" json:"dev_mode"`

	// AllowWrites enables Create/Update/Delete in the Database Browser.
	// Defaults to false. Even when a DBWriter is configured via
	// Collector.SetDBWriter, writes stay disabled until this is
	// explicitly set — a deliberate double opt-in (operator config +
	// application code) so upgrading breeze or wiring a DBWriter for
	// read-side reasons never silently makes production data editable.
	AllowWrites bool `yaml:"allow_writes" json:"allow_writes"`

	// MaxRequests is the rolling window size for the live requests buffer.
	// Older entries are evicted when the buffer is full.
	MaxRequests int `yaml:"max_requests" json:"max_requests"`

	// MaxQueries is the rolling window size for the query monitor.
	MaxQueries int `yaml:"max_queries" json:"max_queries"`

	// MaxLogs is the rolling window size for each log tab.
	MaxLogs int `yaml:"max_logs" json:"max_logs"`

	// EventCapacity is how many event dispatches the Events page
	// retains. It only takes effect if AttachEvents is called; a
	// dashboard with no event bus attached allocates nothing for it.
	// Defaults to 500.
	EventCapacity int `yaml:"event_capacity" json:"event_capacity"`

	// MaxTimelineEntries caps the number of timeline entries retained
	// per request to bound memory usage for long-lived requests.
	MaxTimelineEntries int `yaml:"max_timeline_entries" json:"max_timeline_entries"`

	// SlowQueryMs is the threshold in milliseconds above which a query is
	// highlighted as "slow" in the UI.
	SlowQueryMs int `yaml:"slow_query_ms" json:"slow_query_ms"`

	// SlowRequestMs is the threshold in milliseconds above which a request
	// is highlighted in the live request list.
	SlowRequestMs int `yaml:"slow_request_ms" json:"slow_request_ms"`

	// MaskedHeaders is the set of header names (case-insensitive) whose
	// values are masked in the request inspector and timeline metadata.
	// Defaults to a sensible set of sensitive headers.
	MaskedHeaders []string `yaml:"masked_headers" json:"masked_headers"`

	// GOGC controls the GC trigger rate. The default is 100 (GC runs when
	// the heap doubles). Lowering this value makes the GC run more often,
	// keeping the heap smaller at the cost of CPU.
	//
	// Set to -1 to disable the GC entirely (NOT recommended for production).
	// Set to 0 to leave the runtime default (100) unchanged.
	//
	// Common values:
	//   100 — Go default (heap doubles before GC)
	//    50 — GC when heap grows by 50% (lower memory, more CPU)
	//    25 — aggressive GC (lowest memory, highest CPU overhead)
	GOGC int `yaml:"gogc" json:"gogc"`

	// GOMEMLIMIT sets a soft memory limit for the Go runtime (Go 1.19+).
	// When the process approaches this limit, the GC runs more aggressively
	// to keep memory under the cap.
	//
	// This is the SINGLE MOST EFFECTIVE way to control Go's RSS. Without
	// it, Go's runtime holds onto memory it has allocated (HeapIdle stays
	// high, HeapReleased stays low), causing the process RSS to grow to
	// the peak heap size and never shrink — even after GC reclaims the
	// objects.
	//
	// Set to 0 to leave the runtime default (no limit) unchanged.
	// Format: bytes. Examples:
	//   536870912   — 512 MB
	//  1073741824   — 1 GB
	//  2147483648   — 2 GB
	GOMEMLIMIT int64 `yaml:"gomemlimit" json:"gomemlimit"`

	// StorageType controls how dashboard state is persisted across restarts.
	//   "none"     — no persistence (default)
	//   "sqlite"   — SQLite database file (recommended)
	//   "sql"      — generic SQL (PostgreSQL, MySQL)
	//   "redis"    — Redis / Memcached / Dragonfly
	//   "mongodb"  — MongoDB
	StorageType string `yaml:"storage_type" json:"storage_type"`

	// StoragePath is the connection string or file path for the storage backend.
	StoragePath string `yaml:"storage_path" json:"storage_path"`

	// SaveInterval is how often state is persisted (Go duration string).
	// Default: "1m" (every minute).
	SaveInterval string `yaml:"save_interval" json:"save_interval"`
}

// DefaultConfig returns a Config with sensible defaults for local development.
// Enabled is true, all collectors are on, and credentials default to
// "admin"/"admin" so the dashboard is immediately usable.
//
// GOMEMLIMIT defaults to 512 MB — a reasonable cap for a developer dashboard
// that prevents Go's runtime from holding onto gigabytes of idle memory.
// Production deployments should increase this based on available RAM.
func DefaultConfig() Config {
	return Config{
		Enabled:            true,
		Timeline:           true,
		Queries:            true,
		Metrics:            true,
		Requests:           true,
		BasePath:           "/dashboard",
		Username:           "admin",
		Password:           "admin",
		MaxRequests:        1000,
		MaxQueries:         500,
		MaxLogs:            1000,
		MaxTimelineEntries: 256,
		SlowQueryMs:        100,
		SlowRequestMs:      500,
		GOGC:               50,
		GOMEMLIMIT:         512 * 1024 * 1024, // 512 MB soft limit
		StorageType:        "none",
		SaveInterval:       "1m",
		MaskedHeaders: []string{
			"authorization",
			"cookie",
			"set-cookie",
			"x-api-key",
			"x-auth-token",
			"api-key",
			"password",
			"token",
			"secret",
		},
	}
}

// withDefaults returns a copy of c with zero values replaced by defaults.
func (c Config) withDefaults() Config {
	out := c
	if out.BasePath == "" {
		out.BasePath = "/dashboard"
	}
	if out.MaxRequests <= 0 {
		out.MaxRequests = 1000
	}
	if out.MaxQueries <= 0 {
		out.MaxQueries = 500
	}
	if out.MaxLogs <= 0 {
		out.MaxLogs = 1000
	}
	if out.MaxTimelineEntries <= 0 {
		out.MaxTimelineEntries = 256
	}
	if out.SlowQueryMs <= 0 {
		out.SlowQueryMs = 100
	}
	if out.SlowRequestMs <= 0 {
		out.SlowRequestMs = 500
	}
	if len(out.MaskedHeaders) == 0 {
		out.MaskedHeaders = DefaultConfig().MaskedHeaders
	}
	// GOGC and GOMEMLIMIT default to 0 (runtime default) if not set —
	// the application owner chooses whether to apply memory limits.
	return out
}
