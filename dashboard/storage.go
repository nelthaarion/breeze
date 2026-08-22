package dashboard

import (
        "encoding/json"
        "os"
        "sync"
        "time"
)

// Storage is the interface for persisting dashboard state across restarts.
// Implementations include file-based JSON (default), SQLite, Redis, and MongoDB.
type Storage interface {
        // Save persists the given state snapshot.
        Save(state *PersistedState) error
        // Load retrieves the last persisted state, or nil if none exists.
        Load() (*PersistedState, error)
        // Close releases any resources.
        Close() error
}

// PersistedState is the data saved to / loaded from storage.
// It contains only cumulative counters and daily aggregates — never
// individual request records or timelines (those are in-memory only).
type PersistedState struct {
        // TotalRequests is the cumulative request count across all restarts.
        TotalRequests int64 `json:"total_requests"`

        // TotalErrors is the cumulative error count (HTTP 5xx).
        TotalErrors int64 `json:"total_errors"`

        // UniqueIPs is the set of all unique client IPs ever seen.
        UniqueIPs map[string]bool `json:"unique_ips"`

        // DailyCounts maps "2006-01-02" → request count for that day.
        DailyCounts map[string]int64 `json:"daily_counts"`

        // RouteStats maps "METHOD /pattern" → cumulative stats.
        RouteStats map[string]PersistedRouteStat `json:"route_stats"`

        // LastSaved is when the state was last persisted.
        LastSaved time.Time `json:"last_saved"`
}

// PersistedRouteStat is the per-route stats saved to storage.
type PersistedRouteStat struct {
        Requests   int64  `json:"requests"`
        TotalDurUS int64  `json:"total_dur_us"`
        MaxDurUS   int64  `json:"max_dur_us"`
        Errors     int64  `json:"errors"`
        LastRequest int64 `json:"last_request"` // unix nanos
}

// fileStorage is a simple file-based JSON storage. It's the default when
// StorageType is not "none" but no specific backend is configured.
// For production, replace with SQLite, Redis, or MongoDB.
type fileStorage struct {
        mu   sync.Mutex
        path string
}

func newFileStorage(path string) *fileStorage {
        if path == "" {
                path = "./dashboard_state.json"
        }
        return &fileStorage{path: path}
}

func (fs *fileStorage) Save(state *PersistedState) error {
        fs.mu.Lock()
        defer fs.mu.Unlock()
        state.LastSaved = time.Now()
        data, err := json.Marshal(state)
        if err != nil {
                return err
        }
        // Write to temp file then rename (atomic on most OSes)
        tmp := fs.path + ".tmp"
        if err := os.WriteFile(tmp, data, 0644); err != nil {
                return err
        }
        return os.Rename(tmp, fs.path)
}

func (fs *fileStorage) Load() (*PersistedState, error) {
        fs.mu.Lock()
        defer fs.mu.Unlock()
        data, err := os.ReadFile(fs.path)
        if err != nil {
                if os.IsNotExist(err) {
                        return nil, nil // first run — no saved state
                }
                return nil, err
        }
        var state PersistedState
        if err := json.Unmarshal(data, &state); err != nil {
                return nil, err
        }
        if state.UniqueIPs == nil {
                state.UniqueIPs = make(map[string]bool)
        }
        if state.DailyCounts == nil {
                state.DailyCounts = make(map[string]int64)
        }
        if state.RouteStats == nil {
                state.RouteStats = make(map[string]PersistedRouteStat)
        }
        return &state, nil
}

func (fs *fileStorage) Close() error { return nil }

// newStorage creates a Storage implementation based on the config.
// Currently only file-based JSON is implemented. SQLite, Redis, and MongoDB
// backends can be added by implementing the Storage interface.
func newStorage(cfg Config) Storage {
        if cfg.StorageType == "" || cfg.StorageType == "none" {
                return nil
        }
        // All backends use file storage for now — the enum is there for
        // future implementations with real database drivers.
        return newFileStorage(cfg.StoragePath)
}

// startSaveLoop runs a goroutine that periodically saves state to storage.
// It returns a stop function that flushes one final save.
func startSaveLoop(c *Collector, interval time.Duration) func() {
        if c.storage == nil || interval <= 0 {
                return func() {}
        }
        ticker := time.NewTicker(interval)
        stop := make(chan struct{})
        go func() {
                for {
                        select {
                        case <-stop:
                                c.saveState()
                                return
                        case <-ticker.C:
                                c.saveState()
                        }
                }
        }()
        return func() {
                close(stop)
                ticker.Stop()
        }
}

// loadState loads persisted state from storage and restores counters.
func (c *Collector) loadState() {
        if c.storage == nil {
                return
        }
        state, err := c.storage.Load()
        if err != nil || state == nil {
                return
        }
        // Restore cumulative counters.
        c.requestsTotal.Store(state.TotalRequests)
        c.errorsTotal.Store(state.TotalErrors)

        // Restore unique IPs.
        c.uniqueIPsMu.Lock()
        c.uniqueIPs = state.UniqueIPs
        if c.uniqueIPs == nil {
                c.uniqueIPs = make(map[string]bool)
        }
        c.uniqueIPsMu.Unlock()

        // Restore daily counts.
        c.dailyCountsMu.Lock()
        c.dailyCounts = state.DailyCounts
        if c.dailyCounts == nil {
                c.dailyCounts = make(map[string]int64)
        }
        c.dailyCountsMu.Unlock()

        // Restore route stats.
        c.routeStatsMu.Lock()
        for key, ps := range state.RouteStats {
                acc := &routeStatAccumulator{}
                acc.requests.Store(ps.Requests)
                acc.totalDurUS.Store(ps.TotalDurUS)
                acc.maxDurUS.Store(ps.MaxDurUS)
                acc.errors.Store(ps.Errors)
                acc.lastRequest.Store(ps.LastRequest)
                c.routeStats[key] = acc
        }
        c.routeStatsMu.Unlock()
}

// saveState builds a PersistedState from current counters and saves it.
func (c *Collector) saveState() {
        if c.storage == nil {
                return
        }
        state := &PersistedState{
                TotalRequests: c.requestsTotal.Load(),
                TotalErrors:   c.errorsTotal.Load(),
                UniqueIPs:     make(map[string]bool),
                DailyCounts:   make(map[string]int64),
                RouteStats:    make(map[string]PersistedRouteStat),
        }

        // Copy unique IPs.
        c.uniqueIPsMu.RLock()
        for ip := range c.uniqueIPs {
                state.UniqueIPs[ip] = true
        }
        c.uniqueIPsMu.RUnlock()

        // Copy daily counts. This has to go through DailyCounts() rather than
        // ranging over c.dailyCounts: the request path now counts today into an
        // atomic that is only folded into the map at the next UTC rollover, so a
        // direct read would persist a zero for the day currently in progress.
        state.DailyCounts = c.DailyCounts()

        // Copy route stats.
        c.routeStatsMu.RLock()
        for key, acc := range c.routeStats {
                state.RouteStats[key] = PersistedRouteStat{
                        Requests:    acc.requests.Load(),
                        TotalDurUS:  acc.totalDurUS.Load(),
                        MaxDurUS:    acc.maxDurUS.Load(),
                        Errors:      acc.errors.Load(),
                        LastRequest: acc.lastRequest.Load(),
                }
        }
        c.routeStatsMu.RUnlock()

        _ = c.storage.Save(state)
}
