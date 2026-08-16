package dashboard

import (
        "crypto/subtle"
        "errors"
        "fmt"
        "net/url"
        "runtime"
        "strconv"
        "strings"
        "time"

        "github.com/goccy/go-json"
        "github.com/nelthaarion/breeze"
)

// jsonUnmarshal is a small wrapper around go-json so we don't pull in
// encoding/json (which is slower) on the login hot path.
func jsonUnmarshal(body []byte, v any) error {
        return json.Unmarshal(body, v)
}

// API installs all dashboard HTTP routes on the given router.
//
// Route layout (under cfg.BasePath, default "/dashboard"):
//
//   GET  /dashboard                  → SPA shell
//   GET  /dashboard/assets/*         → static assets (CSS/JS embedded in SPA)
//   GET  /dashboard/api/overview     → Overview metrics
//   GET  /dashboard/api/routes       → Routes explorer
//   GET  /dashboard/api/api-explorer → API explorer route list
//   POST /dashboard/api/api-explorer → Execute an API request from the explorer
//   GET  /dashboard/api/requests     → Live requests (paginated)
//   GET  /dashboard/api/queries      → ORM query monitor
//   GET  /dashboard/api/cache        → Cache monitor
//   POST /dashboard/api/cache/clear  → Clear cache (optional prefix)
//   GET  /dashboard/api/queue        → Queue monitor
//   POST /dashboard/api/queue/retry  → Retry a failed job
//   GET  /dashboard/api/scheduler    → Scheduler
//   GET  /dashboard/api/logs         → Logs (level param)
//   GET  /dashboard/api/health       → Health checks
//   GET  /dashboard/api/performance  → Runtime performance
//   GET  /dashboard/api/timeline     → Recent timelines
//   GET  /dashboard/api/timeline/:id → Single timeline by ID
//
// All routes are wrapped by AuthMiddleware.
func (c *Collector) registerRoutes(router *breeze.Router, app *breeze.Breeze) {
        base := strings.TrimSuffix(c.cfg.BasePath, "/")
        if base == "" {
                base = "/dashboard"
        }

        c.sessions = newSessionStore()
        auth := AuthMiddleware(c.cfg, c.sessions)

        // ── Extract embedded templates to a temp directory ────────────────────
        dir, err := writeTemplates()
        if err != nil {
                router.Handle(breeze.GET, base, func(ctx *breeze.Context) {
                        ctx.Status(500)
                        ctx.WriteString("dashboard: failed to extract templates: " + err.Error())
                })
        } else {
                templatesDir = dir
                engine := templateEngine(dir)
                c.engine = engine

                // ── Static assets (CSS/JS) — no auth required ─────────────────────
                publicDir := dir + "/public"
                router.ServeStatic(base+"/assets", publicDir)

                // ── Login page (GET) — public, no auth ────────────────────────────
                loginLayout := dir + "/views/login_layout.html"
                loginEngine := breeze.NewTemplateEngine(breeze.TemplateConfig{
                        ViewsDir:      dir + "/views",
                        ComponentsDir: dir + "/components",
                        LayoutFile:    loginLayout,
                        DevMode:       false,
                })
                router.Handle(breeze.GET, base+"/login", func(ctx *breeze.Context) {
                        // If already logged in, redirect to dashboard.
                        cookie := ctx.Req.Header["cookie"]
                        token := extractCookieValue(cookie, sessionCookieName)
                        if _, ok := c.sessions.valid(token); ok {
                                ctx.Res = &breeze.HTTPResponse{
                                        Status: 302,
                                        Headers: map[string]string{
                                                "Location": base,
                                        },
                                        Body: []byte("redirecting..."),
                                }
                                return
                        }
                        data := map[string]any{
                                "BasePath":   base,
                                "LoginPath":  base + "/login",
                                "PageTitle":  "Login",
                        }
                        loginEngine.RenderView(ctx, "login", data)
                })

                // ── Login POST — validates credentials, sets session cookie ────────
                router.Handle(breeze.POST, base+"/login", func(ctx *breeze.Context) {
                        var req struct {
                                Username string `json:"username"`
                                Password string `json:"password"`
                        }
                        body := ctx.Req.Body
                        if err := jsonUnmarshal(body, &req); err != nil {
                                ctx.JSON(map[string]any{"ok": false, "error": "invalid request body"})
                                return
                        }
                        wantUser := []byte(c.cfg.Username)
                        wantPass := hashPass(c.cfg.Password)
                        if subtle.ConstantTimeCompare([]byte(req.Username), wantUser) != 1 ||
                                subtle.ConstantTimeCompare(hashPass(req.Password), wantPass) != 1 {
                                ctx.JSON(map[string]any{"ok": false, "error": "invalid username or password"})
                                return
                        }
                        token := c.sessions.create(req.Username)
                        // Build the response manually so Set-Cookie is included.
                        // (ctx.JSON then ctx.SetHeader doesn't work because JSON
                        // creates a response with shared headers that SetHeader
                        // would copy-on-write, but the cookie needs to be on the
                        // final response.)
                        respBody, _ := json.Marshal(map[string]any{"ok": true, "redirect": base})
                        ctx.Res = &breeze.HTTPResponse{
                                Status: 200,
                                Headers: map[string]string{
                                        "Content-Type": "application/json",
                                        "Set-Cookie":   buildSessionCookie(token, base, int(sessionDuration.Seconds())),
                                },
                                Body: respBody,
                        }
                })

                // ── Logout — destroys session, redirects to login ─────────────────
                router.Handle(breeze.GET, base+"/logout", func(ctx *breeze.Context) {
                        cookie := ctx.Req.Header["cookie"]
                        token := extractCookieValue(cookie, sessionCookieName)
                        c.sessions.destroy(token)
                        ctx.Res = &breeze.HTTPResponse{
                                Status: 302,
                                Headers: map[string]string{
                                        "Location":   base + "/login",
                                        "Set-Cookie": buildSessionCookie("", base, 0),
                                },
                                Body: []byte("redirecting..."),
                        }
                })

                // ── View routes — one per dashboard page ──────────────────────────
                pages := []string{
                        "overview", "routes", "api", "requests",
                        "cache", "logs",
                        "health", "performance", "timeline", "architecture",
                        "events", "video",

                }


                // Index route — auth + render overview
                router.Handle(breeze.GET, base, func(ctx *breeze.Context) {
                        auth(ctx)
                        if ctx.Res != nil && (ctx.Res.Status == 302 || ctx.Res.Status == 401) {
                                return
                        }
                        data := c.viewData(ctx, "overview")
                        engine.RenderView(ctx, "overview", data)
                })

                for _, page := range pages {
                        pageName := page
                        router.Handle(breeze.GET, base+"/"+pageName, func(ctx *breeze.Context) {
                                auth(ctx)
                                if ctx.Res != nil && (ctx.Res.Status == 302 || ctx.Res.Status == 401) {
                                        return
                                }
                                data := c.viewData(ctx, pageName)
                                engine.RenderView(ctx, pageName, data)
                        })
                }
        }

        // ── API endpoints ──────────────────────────────────────────────────────
        api := base + "/api"

        router.Handle(breeze.GET, api+"/overview", c.wrap(auth, c.handleOverview))
        router.Handle(breeze.GET, api+"/routes", c.wrap(auth, c.handleRoutes))
        router.Handle(breeze.GET, api+"/api-explorer", c.wrap(auth, c.handleAPIExplorerList))
        router.Handle(breeze.POST, api+"/api-explorer", c.wrap(auth, c.handleAPIExplorerExec))
        router.Handle(breeze.GET, api+"/requests", c.wrap(auth, c.handleRequests))
        router.Handle(breeze.GET, api+"/cache", c.wrap(auth, c.handleCache))
        router.Handle(breeze.POST, api+"/cache/clear", c.wrap(auth, c.handleCacheClear))
        router.Handle(breeze.GET, api+"/logs", c.wrap(auth, c.handleLogs))
        router.Handle(breeze.GET, api+"/health", c.wrap(auth, c.handleHealth))
        router.Handle(breeze.GET, api+"/performance", c.wrap(auth, c.handlePerformance))
        router.Handle(breeze.GET, api+"/timeline", c.wrap(auth, c.handleTimelineList))
        router.Handle(breeze.GET, api+"/timeline/:id", c.wrap(auth, c.handleTimelineGet))
        router.Handle(breeze.GET, api+"/architecture", c.wrap(auth, c.handleArchitecture))
        router.Handle(breeze.GET, api+"/events", c.wrap(auth, c.handleEvents))
        router.Handle(breeze.GET, api+"/video", c.wrap(auth, c.handleVideo))


                router.Handle(breeze.GET, api+"/db/tables", c.wrap(auth, c.handleDBTables))
                router.Handle(breeze.GET, api+"/db/tables/:name", c.wrap(auth, c.handleDBTableData))
                router.Handle(breeze.POST, api+"/db/tables/:name/rows", c.wrap(auth, c.handleDBTableInsert))
                router.Handle(breeze.PUT, api+"/db/tables/:name/rows/:pk", c.wrap(auth, c.handleDBTableUpdate))
                router.Handle(breeze.DELETE, api+"/db/tables/:name/rows/:pk", c.wrap(auth, c.handleDBTableDelete))

	// ── WebSocket endpoint for real-time updates ──────────────────────────
	if app != nil {
		app.WebSocket(base+"/ws", &wsHandler{hub: c.hub})
	}
}

// viewData builds the template data passed to every dashboard view.
// It includes the current page name, the base path for URL construction,
// the assets path, and the page title.
func (c *Collector) viewData(ctx *breeze.Context, page string) map[string]any {
        return map[string]any{
                "Page":       page,
                "BasePath":   strings.TrimSuffix(c.cfg.BasePath, "/"),
                "AssetsPath": strings.TrimSuffix(c.cfg.BasePath, "/") + "/assets",
                "PageTitle":  pageLabelFor(page),
        }
}

// pageLabelFor returns the human-readable title for a dashboard page.
func pageLabelFor(page string) string {
        titles := map[string]string{
                "overview":    "Overview",
                "routes":      "Routes",
                "api":         "API Explorer",
                "requests":    "Live Requests",
                "cache":       "Cache",
                "logs":        "Logs",
                "health":      "Health",
                "performance": "Performance",
                "timeline":    "Timeline",
                "architecture": "Architecture",
                "events":      "Events",
                "video":       "Video Streaming",
        }


        if t, ok := titles[page]; ok {
                return t
        }
        return page
}

// wrap runs the auth middleware then the given handler. The auth middleware
// is responsible for short-circuiting unauthenticated requests; if it does
// not abort, we call h.
func (c *Collector) wrap(auth breeze.HandlerFunc, h breeze.HandlerFunc) breeze.HandlerFunc {
        return func(ctx *breeze.Context) {
                auth(ctx)
                if ctx.Res != nil && (ctx.Res.Status == 401 || ctx.Res.Status == 403 || ctx.Res.Status == 302) {
                        return
                }
                h(ctx)
        }
}

// ─── Handlers ────────────────────────────────────────────────────────────

func (c *Collector) handleOverview(ctx *breeze.Context) {
        m := c.Metrics()
        recent := c.Requests(0)
        // Compute today's date boundary.
        history := c.MetricsHistory(60)
        ctx.JSON(map[string]any{
                "metrics":        m,
                "history":        history,
                "routes":         len(c.RouteStats()),
                "requests":       len(recent),
                "cache":          c.CacheStats(),
                "collector":      c.cfg.Enabled,
                "total_views":    c.requestsTotal.Load(),
                "unique_viewers": c.UniqueIPCount(),
                "today_viewers":  c.TodayCount(),
                "daily_counts":   c.DailyCounts(),
                "storage_type":   c.cfg.StorageType,
        })
}

func (c *Collector) handleRoutes(ctx *breeze.Context) {
        base := strings.TrimSuffix(c.cfg.BasePath, "/")
        if base == "" {
                base = "/dashboard"
        }
        stats := c.RouteStats()
        // Merge with the static route table so routes that haven't been hit
        // yet still appear in the explorer. Skip dashboard's own routes —
        // they're not application routes and would just add noise.
        seen := make(map[string]bool, len(stats))
        for _, s := range stats {
                seen[s.Method+" "+s.Pattern] = true
        }
        for _, rt := range c.router.RoutesInfo() {
                pattern := rt.Pattern()
                // Skip dashboard's own routes.
                if pattern == base || strings.HasPrefix(pattern, base+"/") {
                        continue
                }
                // Skip static file serving routes.
                if strings.HasSuffix(pattern, "/*filepath") {
                        continue
                }
                key := string(rt.Method()) + " " + pattern
                if !seen[key] {
                        stats = append(stats, RouteStat{
                                Method:     string(rt.Method()),
                                Pattern:    pattern,
                                Controller: "",
                                Requests:   0,
                        })
                }
        }
        ctx.JSON(stats)
}

func (c *Collector) handleRequests(ctx *breeze.Context) {
        n := atoiDefault(ctx.Query("limit"), 200)
        method := ctx.Query("method")
        status := ctx.Query("status")
        route := ctx.Query("route")
        user := ctx.Query("user")
        all := c.Requests(n)
        out := make([]RequestRecord, 0, len(all))
        for _, r := range all {
                if method != "" && r.Method != method {
                        continue
                }
                if status != "" && !statusMatch(status, r.Status) {
                        continue
                }
                if route != "" && !strings.Contains(r.Route, route) && !strings.Contains(r.Path, route) {
                        continue
                }
                if user != "" && !strings.EqualFold(r.User, user) {
                        continue
                }
                out = append(out, r)
        }
        ctx.JSON(out)
}

func (c *Collector) handleCache(ctx *breeze.Context) {
        ctx.JSON(c.CacheStats())
}

func (c *Collector) handleCacheClear(ctx *breeze.Context) {
        prefix := ctx.Query("prefix")
        _ = prefix
        c.cacheHits.Store(0)
        c.cacheMisses.Store(0)
        ctx.JSON(map[string]any{"ok": true})
}

func (c *Collector) handleLogs(ctx *breeze.Context) {
        level := ctx.Query("level")
        if level == "" {
                level = "app"
        }
        n := atoiDefault(ctx.Query("limit"), 500)
        q := ctx.Query("q")
        all := c.Logs(level, n)
        if q == "" {
                ctx.JSON(all)
                return
        }
        out := make([]LogEntry, 0, len(all))
        for _, e := range all {
                if strings.Contains(strings.ToLower(e.Message), strings.ToLower(q)) {
                        out = append(out, e)
                }
        }
        ctx.JSON(out)
}

func (c *Collector) handleHealth(ctx *breeze.Context) {
        ctx.JSON(c.RunHealthChecks())
}

func (c *Collector) handlePerformance(ctx *breeze.Context) {
        hist := c.MetricsHistory(120)
        pm := buildPerfMetrics(c)
        ctx.JSON(map[string]any{
                "current":  pm,
                "history":  hist,
        })
}

func (c *Collector) handleTimelineList(ctx *breeze.Context) {
        n := atoiDefault(ctx.Query("limit"), 50)
        ctx.JSON(c.Timelines(n))
}

func (c *Collector) handleTimelineGet(ctx *breeze.Context) {
        id := ctx.Param("id")
        for _, t := range c.Timelines(0) {
                if t.ID == id {
                        ctx.JSON(t)
                        return
                }
        }
        ctx.Status(404)
        ctx.JSON(map[string]string{"error": "timeline not found"})
}

func (c *Collector) handleArchitecture(ctx *breeze.Context) {
        conns := c.Connections()

        // Aggregate stats
        total := len(conns)
        connected := 0
        degraded := 0
        disconnected := 0
        unknown := 0
        for _, conn := range conns {
                switch conn.Status {
                case StatusConnected:
                        connected++
                case StatusDegraded:
                        degraded++
                case StatusDisconnected:
                        disconnected++
                default:
                        unknown++
                }
        }

        ctx.JSON(map[string]any{
                "connections":  conns,
                "total":        total,
                "connected":    connected,
                "degraded":     degraded,
                "disconnected": disconnected,
                "unknown":      unknown,
        })
}

func (c *Collector) handleDBTables(ctx *breeze.Context) {
        inspector := c.DBInspector()
        if inspector == nil {
                ctx.JSON(map[string]any{"tables": []TableInfo{}})
                return
        }
        tables, err := inspector.Tables()
        if err != nil {
                ctx.Status(500)
                ctx.JSON(map[string]any{"error": err.Error()})
                return
        }
        ctx.JSON(map[string]any{"tables": tables})
}

func (c *Collector) handleDBTableData(ctx *breeze.Context) {
        inspector := c.DBInspector()
        if inspector == nil {
                ctx.JSON(TableData{Table: ctx.Param("name"), Page: 1, PageSize: 50, Total: 0, Rows: []map[string]any{}, Columns: []TableColumn{}})
                return
        }
        page := atoiDefault(ctx.Query("page"), 1)
        pageSize := atoiDefault(ctx.Query("page_size"), 50)
        data, err := inspector.TableData(ctx.Param("name"), page, pageSize, ctx.Query("search"))
        if err != nil {
                ctx.Status(500)
                ctx.JSON(map[string]any{"error": err.Error()})
                return
        }
        data.Writable = c.cfg.AllowWrites && c.DBWriter() != nil
        ctx.JSON(data)
}

// writableGuard checks that the Database Browser's write path is enabled
// (Config.AllowWrites + a configured DBWriter) and that table is a table
// the inspector actually reports, so writes can't target an unlisted or
// hand-crafted table name. On failure it writes the appropriate error
// response to ctx and returns ok=false; callers must return immediately.
func (c *Collector) writableGuard(ctx *breeze.Context, table string) (DBWriter, bool) {
        if !c.cfg.AllowWrites {
                ctx.Status(403)
                ctx.JSON(map[string]any{"error": "writes are not enabled"})
                return nil, false
        }
        writer := c.DBWriter()
        inspector := c.DBInspector()
        if writer == nil || inspector == nil {
                ctx.Status(403)
                ctx.JSON(map[string]any{"error": "writes are not enabled"})
                return nil, false
        }
        tables, err := inspector.Tables()
        if err != nil {
                ctx.Status(500)
                ctx.JSON(map[string]any{"error": err.Error()})
                return nil, false
        }
        for _, t := range tables {
                if t.Name == table {
                        return writer, true
                }
        }
        ctx.Status(400)
        ctx.JSON(map[string]any{"error": "unknown table: " + table})
        return nil, false
}

func (c *Collector) handleDBTableInsert(ctx *breeze.Context) {
        table := ctx.Param("name")
        writer, ok := c.writableGuard(ctx, table)
        if !ok {
                return
        }
        var req struct {
                Values map[string]any `json:"values"`
        }
        if err := jsonUnmarshal(ctx.Req.Body, &req); err != nil {
                ctx.Status(400)
                ctx.JSON(map[string]any{"error": "invalid request body"})
                return
        }
        row, err := writer.InsertRow(table, req.Values)
        if err != nil {
                ctx.Status(400)
                ctx.JSON(map[string]any{"error": err.Error()})
                return
        }
        c.invalidateTableCache(table)
        c.RecordLog("app", LogEntry{Time: now(), Message: fmt.Sprintf("db write: insert into %s", table)})
        ctx.Status(201)
        ctx.JSON(row)
}

func (c *Collector) handleDBTableUpdate(ctx *breeze.Context) {
        table := ctx.Param("name")
        writer, ok := c.writableGuard(ctx, table)
        if !ok {
                return
        }
        pk := parsePK(ctx.Param("pk"))
        var req struct {
                Values map[string]any `json:"values"`
        }
        if err := jsonUnmarshal(ctx.Req.Body, &req); err != nil {
                ctx.Status(400)
                ctx.JSON(map[string]any{"error": "invalid request body"})
                return
        }
        err := writer.UpdateRow(table, pk, req.Values)
        if errors.Is(err, ErrRowNotFound) {
                ctx.Status(404)
                ctx.JSON(map[string]any{"error": "row not found"})
                return
        }
        if err != nil {
                ctx.Status(400)
                ctx.JSON(map[string]any{"error": err.Error()})
                return
        }
        c.invalidateTableCache(table)
        c.RecordLog("app", LogEntry{Time: now(), Message: fmt.Sprintf("db write: update %s pk=%s", table, ctx.Param("pk"))})
        ctx.JSON(map[string]any{"ok": true})
}

func (c *Collector) handleDBTableDelete(ctx *breeze.Context) {
        table := ctx.Param("name")
        writer, ok := c.writableGuard(ctx, table)
        if !ok {
                return
        }
        pk := parsePK(ctx.Param("pk"))
        err := writer.DeleteRow(table, pk)
        if errors.Is(err, ErrRowNotFound) {
                ctx.Status(404)
                ctx.JSON(map[string]any{"error": "row not found"})
                return
        }
        if err != nil {
                ctx.Status(400)
                ctx.JSON(map[string]any{"error": err.Error()})
                return
        }
        c.invalidateTableCache(table)
        c.RecordLog("app", LogEntry{Time: now(), Message: fmt.Sprintf("db write: delete from %s pk=%s", table, ctx.Param("pk"))})
        ctx.Status(204)
}

// ─── Helpers ──────────────────────────────────────────────────────────────

func atoiDefault(s string, def int) int {
        if s == "" {
                return def
        }
        v, err := strconv.Atoi(s)
        if err != nil {
                return def
        }
        return v
}

// statusMatch supports both exact ("200") and range ("5xx", "4xx") filters.
func statusMatch(filter string, status int) bool {
        if filter == "" {
                return true
        }
        if strings.HasSuffix(filter, "xx") {
                prefix := filter[:len(filter)-2]
                class, err := strconv.Atoi(prefix)
                if err != nil {
                        return false
                }
                return status/100 == class
        }
        v, err := strconv.Atoi(filter)
        if err != nil {
                return false
        }
        return v == status
}

// jsonStringField extracts a string field from a small JSON object without
// pulling in encoding/json on the hot path. Returns "" if not found.
func jsonStringField(body []byte, field string) string {
        key := `"` + field + `":`
        i := strings.Index(string(body), key)
        if i < 0 {
                return ""
        }
        rest := string(body)[i+len(key):]
        rest = strings.TrimLeft(rest, " \t")
        if len(rest) == 0 || rest[0] != '"' {
                return ""
        }
        rest = rest[1:]
        end := strings.IndexByte(rest, '"')
        if end < 0 {
                return ""
        }
        v, err := url.QueryUnescape(rest[:end])
        if err != nil {
                return rest[:end]
        }
        return v
}

// buildPerfMetrics assembles a detailed runtime performance snapshot from a
// FRESH runtime.ReadMemStats() call.
//
// We do NOT reuse the cached MetricsSnapshot from the sampler because:
//   1. The sampler runs at 1Hz, so the cached value could be up to 1s stale.
//   2. The Performance page should show current data when the user opens it.
//   3. The sampler's MetricsSnapshot doesn't store all MemStats fields
//      (e.g. MSpanInuse, MCacheInuse, BuckHashSys) — we need the full
//      runtime.MemStats struct.
//
// Every field is mapped directly from runtime.MemStats with the correct
// semantics:
//
//      HeapAlloc    → HeapStats.Alloc      (live heap bytes, DROPS after GC)
//      TotalAlloc   → HeapStats.TotalAlloc (cumulative, only grows)
//      HeapSys      → HeapStats.Sys        (heap bytes from OS)
//      HeapIdle     → HeapStats.Idle       (idle heap bytes)
//      HeapInuse    → HeapStats.Inuse      (in-use heap bytes)
//      HeapReleased → HeapStats.Released   (bytes returned to OS)
//      HeapObjects  → HeapStats.Objects    (live heap object count)
//      Mallocs      → HeapStats.Mallocs    (cumulative count, only grows)
//      Frees        → HeapStats.Frees      (cumulative count, only grows)
//
//      StackInuse → StackStats.InUse
//      StackSys   → StackStats.Sys
//
//      NumGC        → GCStats.NumGC
//      LastGC       → GCStats.LastGC
//      NextGC       → GCStats.NextGC
//      PauseTotalNs → GCStats.PauseTotalNS (cumulative, only grows)
//      PauseNs[(NumGC-1)%256] → GCStats.PauseNS (most recent pause)
//      GCCPUFraction → GCStats.CPUFraction
//
//      Sys (MemStats.Sys) → MemoryStats.Sys (total from OS)
//      HeapAlloc           → MemoryStats.HeapInUse (live heap)
//
// NEVER confuse:
//   - TotalAlloc with HeapAlloc (TotalAlloc is cumulative, HeapAlloc is current)
//   - HeapSys with HeapAlloc (HeapSys is from OS, HeapAlloc is live objects)
//   - PauseTotalNs with PauseNs (PauseTotalNs is cumulative, PauseNs is latest)
func buildPerfMetrics(c *Collector) PerfMetrics {
        // Fresh read — never reuse.
        var ms runtime.MemStats
        runtime.ReadMemStats(&ms)

        // Most recent GC pause from the circular buffer.
        var lastPauseNs uint64
        if ms.NumGC > 0 {
                lastPauseNs = ms.PauseNs[(ms.NumGC-1+256)%256]
        }

        // CPU times for the CPU stats section.
        userTime, sysTime := cpuTimes()

        return PerfMetrics{
                Time:       time.Now(),
                Goroutines: runtime.NumGoroutine(),

                Heap: HeapStats{
                        Alloc:      ms.HeapAlloc,    // live heap — DROPS after GC
                        TotalAlloc: ms.TotalAlloc,   // cumulative — only grows
                        Sys:        ms.HeapSys,      // heap bytes from OS
                        Idle:       ms.HeapIdle,
                        Inuse:      ms.HeapInuse,
                        Released:   ms.HeapReleased,
                        Objects:    ms.HeapObjects,
                        Lookups:    ms.Lookups,
                        Mallocs:    ms.Mallocs, // cumulative
                        Frees:      ms.Frees,   // cumulative
                },

                Stack: StackStats{
                        InUse: ms.StackInuse,
                        Sys:   ms.StackSys,
                },

                OffHeap: OffHeapStats{
                        MSpanInuse:  ms.MSpanInuse,
                        MSpanSys:    ms.MSpanSys,
                        MCacheInuse: ms.MCacheInuse,
                        MCacheSys:   ms.MCacheSys,
                        BuckHashSys: ms.BuckHashSys,
                        OtherSys:    ms.OtherSys,
                },

                GC: GCStats{
                        NumGC:        ms.NumGC,
                        LastGC:       time.Unix(0, int64(ms.LastGC)),
                        NextGC:       ms.NextGC,
                        PauseTotalNS: ms.PauseTotalNs, // cumulative
                        PauseNS:      lastPauseNs,     // most recent pause
                        CPUFraction:  ms.GCCPUFraction,
                        Enabled:      debugGCPercent() >= 0,
                },

                Allocs: AllocStats{
                        TotalAlloc: ms.TotalAlloc, // cumulative bytes allocated
                        Mallocs:    ms.Mallocs,    // cumulative count
                        Frees:      ms.Frees,      // cumulative count
                        BytesPerOp: ms.TotalAlloc / (ms.Mallocs + 1),
                },

                Memory: MemoryStats{
                        Sys:          ms.Sys,          // total from OS (MemStats.Sys)
                        HeapInUse:    ms.HeapAlloc,    // live heap bytes
                        HeapIdle:     ms.HeapIdle,     // idle heap bytes
                        HeapReleased: ms.HeapReleased, // bytes returned to OS
                        StackInUse:   ms.StackInuse,
                        Other:        ms.Sys - ms.HeapAlloc - ms.StackInuse,
                        UsagePct:     float64(ms.HeapAlloc) / float64(ms.Sys+1) * 100.0,
                },

                CPU: CPUStats{
                        NumCPU:       runtime.NumCPU(),
                        GOMAXPROCS:   runtime.GOMAXPROCS(0),
                        CGOCalls:     runtime.NumCgoCall(),
                        UsagePct:     c.Metrics().CPUUsage, // from sampler (computed delta)
                        UserTimeNS:   userTime.Nanoseconds(),
                        SystemTimeNS: sysTime.Nanoseconds(),
                },

                Network: NetworkStats{
                        Connections:   0,
                        WebSocketOpen: 0,
                },

                RuntimeTuning: RuntimeTuning{
                        GOGC:       debugGCPercent(),
                        GOMEMLIMIT: debugMemoryLimit(),
                },
        }
}

