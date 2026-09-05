package dashboard

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/nelthaarion/breeze"
	"github.com/nelthaarion/breeze/scalar"
)

// jsonUnmarshal is a small wrapper around go-json so we don't pull in
// encoding/json (which is slower) on the login hot path.
func jsonUnmarshal(body []byte, v any) error {
	return json.Unmarshal(body, v)
}

// jsonError writes {"error": message} with a status and returns it as the
// handler's result.
//
// Modelled on aggregator.writeJSON: one function so that every refusal from this
// package has the same shape, because the SPA reads `.error` off every failed
// response and a handler that answered with a bare string or a differently-named
// field would show up as an empty error box rather than a missing one.
//
// Only for single-key errors. Several sites here add a second field — the
// aggregator URL that could not be reached, the list of registered subsystems —
// and those are more useful than a uniform shape, so they build their own map.
// Widening this to accept extra keys would make the common call read worse to
// serve the exception.
func jsonError(ctx *breeze.Context, status int, message string) error {
	ctx.Status(status)
	return ctx.JSON(map[string]string{"error": message})
}

// API installs all dashboard HTTP routes on the given router.
//
// Route layout (under cfg.BasePath, default "/dashboard"):
//
//	GET  /dashboard                  → SPA shell
//	GET  /dashboard/assets/*         → static assets (CSS/JS embedded in SPA)
//	GET  /dashboard/api/overview     → Overview metrics
//	GET  /dashboard/api/routes       → Routes explorer
//	GET  /dashboard/api/api-explorer → API explorer route list
//	POST /dashboard/api/api-explorer → Execute an API request from the explorer
//	GET  /dashboard/api/requests     → Live requests (paginated)
//	GET  /dashboard/api/queries      → ORM query monitor
//	GET  /dashboard/api/cache        → Cache monitor
//	POST /dashboard/api/cache/clear  → Clear cache (optional prefix)
//	GET  /dashboard/api/queue        → Queue monitor
//	POST /dashboard/api/queue/retry  → Retry a failed job
//	GET  /dashboard/api/scheduler    → Scheduler
//	GET  /dashboard/api/logs         → Logs (level param)
//	GET  /dashboard/api/health       → Health checks
//	GET  /dashboard/api/performance  → Runtime performance
//	GET  /dashboard/api/timeline     → Recent timelines
//	GET  /dashboard/api/timeline/:id → Single timeline by ID
//	GET  /dashboard/api/diagnostics  → Every subsystem's diagnostic report
//
// All routes are wrapped by AuthMiddleware.
//
// Split into four helpers below rather than one long body. The split is by
// dependency, not by length: the view routes need the extracted template
// directory and cannot be registered if extraction failed, the API routes need
// neither, and the WebSocket route needs the *Breeze rather than the *Router. Each
// helper takes exactly what it uses, which is what makes the failure branch below
// obviously correct — the API surface stays up when templates cannot be written.
func (c *Collector) registerRoutes(router *breeze.Router, app *breeze.Breeze) {
	// Every dashboard route is registered with HandleBlocking, never
	// Handle. The view routes render templates (a cache miss parses from
	// disk under a write lock), the DB routes issue SQL round trips, and
	// the API-explorer route makes an outbound HTTP request. None of that
	// may run on a gnet event-loop goroutine, where it would stall every
	// connection pinned to that reactor.
	//
	// Nothing is lost by it: the dashboard is an operator tool measured in
	// requests per minute, and the worker-pool hop it costs is invisible
	// next to the work these handlers actually do.
	base := strings.TrimSuffix(c.cfg.BasePath, "/")
	if base == "" {
		base = "/dashboard"
	}

	c.sessions = newSessionStore()
	auth := AuthMiddleware(c.cfg, c.sessions)

	// Templates are extracted to a temp directory because the Breeze
	// TemplateEngine reads from the filesystem. A failure here disables the
	// HTML half only: the API routes below still serve, which is what an
	// operator debugging the failure needs.
	dir, err := writeTemplates()
	if err != nil {
		router.HandleBlocking(breeze.GET, base, func(ctx *breeze.Context) error {
			ctx.Status(500)
			return ctx.WriteString("dashboard: failed to extract templates: " + err.Error())
		})
	} else {
		c.engine = templateEngine(dir)
		router.ServeStatic(base+"/assets", dir+"/public")
		c.registerAuthRoutes(router, base, dir)
		c.registerPageRoutes(router, base, auth)
	}

	c.registerAPIRoutes(router, base, auth)

	// ── WebSocket endpoint for real-time updates ──────────────────────────
	if app != nil {
		app.WebSocket(base+"/ws", &wsHandler{hub: c.hub})
	}
}

// registerAuthRoutes installs the login page, the login POST and logout.
//
// These are the only dashboard routes that are not behind auth — they cannot be,
// since they are how a session is obtained — and they use their own template
// engine because the login page has a different layout from every other view.
func (c *Collector) registerAuthRoutes(router *breeze.Router, base, dir string) {
	loginEngine := breeze.NewTemplateEngine(breeze.TemplateConfig{
		ViewsDir:      dir + "/views",
		ComponentsDir: dir + "/components",
		LayoutFile:    dir + "/views/login_layout.html",
		DevMode:       false,
	})

	router.HandleBlocking(breeze.GET, base+"/login", func(ctx *breeze.Context) error {
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
			return nil
		}
		data := map[string]any{
			"BasePath":  base,
			"LoginPath": base + "/login",
			"PageTitle": "Login",
		}
		loginEngine.RenderView(ctx, "login", data)

		return nil
	})

	// ── Login POST — validates credentials, sets session cookie ────────
	router.HandleBlocking(breeze.POST, base+"/login", func(ctx *breeze.Context) error {
		var req struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		body := ctx.Req.Body
		if err := jsonUnmarshal(body, &req); err != nil {
			return ctx.JSON(map[string]any{"ok": false, "error": "invalid request body"})
		}
		wantUser := []byte(c.cfg.Username)
		wantPass := hashPass(c.cfg.Password)
		if subtle.ConstantTimeCompare([]byte(req.Username), wantUser) != 1 ||
			subtle.ConstantTimeCompare(hashPass(req.Password), wantPass) != 1 {
			return ctx.JSON(map[string]any{"ok": false, "error": "invalid username or password"})
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

		return nil
	})

	// ── Logout — destroys session, redirects to login ─────────────────
	router.HandleBlocking(breeze.GET, base+"/logout", func(ctx *breeze.Context) error {
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

		return nil
	})
}

// registerPageRoutes installs one HTML route per dashboard page.
//
// Every page renders the same way, so the list is data and the handler is written
// once. auth is called inline rather than through c.wrap because a view route
// answers an unauthenticated request with a redirect to the login page, while
// wrap's callers are JSON endpoints that answer with a 401.
func (c *Collector) registerPageRoutes(router *breeze.Router, base string, auth breeze.HandlerFunc) {
	pages := []string{
		"overview", "routes", "api", "requests",
		"cache", "logs",
		"health", "performance", "timeline", "architecture",
		"events", "video",
	}
	if c.cfg.FleetAggregatorURL != "" {
		pages = append(pages, "fleet")
	}

	// Index route — auth + render overview
	router.HandleBlocking(breeze.GET, base, func(ctx *breeze.Context) error {
		auth(ctx)
		if ctx.Res != nil && (ctx.Res.Status == 302 || ctx.Res.Status == 401) {
			return nil
		}
		data := c.viewData(ctx, "overview")
		c.engine.RenderView(ctx, "overview", data)

		return nil
	})

	for _, page := range pages {
		pageName := page
		router.HandleBlocking(breeze.GET, base+"/"+pageName, func(ctx *breeze.Context) error {
			auth(ctx)
			if ctx.Res != nil && (ctx.Res.Status == 302 || ctx.Res.Status == 401) {
				return nil
			}
			data := c.viewData(ctx, pageName)
			c.engine.RenderView(ctx, pageName, data)

			return nil
		})
	}
}

// registerAPIRoutes installs the JSON API under base+"/api".
//
// Registered whether or not template extraction succeeded, which is the reason
// this is a separate function: the API is what a script, the fleet aggregator and
// the MCP live tools read, and none of them need the HTML.
func (c *Collector) registerAPIRoutes(router *breeze.Router, base string, auth breeze.HandlerFunc) {
	api := base + "/api"

	router.HandleBlocking(breeze.GET, api+"/overview", c.wrap(auth, c.handleOverview))
	router.HandleBlocking(breeze.GET, api+"/routes", c.wrap(auth, c.handleRoutes))
	router.HandleBlocking(breeze.GET, api+"/api-explorer", c.wrap(auth, c.handleAPIExplorerList))
	router.HandleBlocking(breeze.POST, api+"/api-explorer", c.wrap(auth, c.handleAPIExplorerExec))
	router.HandleBlocking(breeze.GET, api+"/requests", c.wrap(auth, c.handleRequests))
	router.HandleBlocking(breeze.GET, api+"/cache", c.wrap(auth, c.handleCache))
	router.HandleBlocking(breeze.POST, api+"/cache/clear", c.wrap(auth, c.handleCacheClear))
	router.HandleBlocking(breeze.GET, api+"/logs", c.wrapService(auth, c.handleLogs))

	router.HandleBlocking(breeze.GET, api+"/health", c.wrap(auth, c.handleHealth))
	router.HandleBlocking(breeze.GET, api+"/performance", c.wrap(auth, c.handlePerformance))
	router.HandleBlocking(breeze.GET, api+"/timeline", c.wrap(auth, c.handleTimelineList))
	router.HandleBlocking(breeze.GET, api+"/timeline/:id", c.wrap(auth, c.handleTimelineGet))
	router.HandleBlocking(breeze.GET, api+"/architecture", c.wrap(auth, c.handleArchitecture))
	router.HandleBlocking(breeze.GET, api+"/events", c.wrap(auth, c.handleEvents))
	router.HandleBlocking(breeze.GET, api+"/video", c.wrap(auth, c.handleVideo))
	router.HandleBlocking(breeze.GET, api+"/capabilities", c.wrap(auth, c.handleCapabilities))
	// Diagnostics is wrapService, not wrap: the fleet aggregator fans out to it on
	// a human's behalf when assembling a cross-service picture, exactly as it does
	// for logs, and it has no session cookie to present.
	router.HandleBlocking(breeze.GET, api+"/diagnostics", c.wrapService(auth, c.handleDiagnostics))
	if c.cfg.FleetAggregatorURL != "" {
		router.HandleBlocking(breeze.GET, api+"/fleet/*path", c.wrap(auth, c.handleFleetProxy))
	}

	router.HandleBlocking(breeze.GET, api+"/db/tables", c.wrap(auth, c.handleDBTables))
	router.HandleBlocking(breeze.GET, api+"/db/tables/:name", c.wrap(auth, c.handleDBTableData))
	router.HandleBlocking(breeze.POST, api+"/db/tables/:name/rows", c.wrap(auth, c.handleDBTableInsert))
	router.HandleBlocking(breeze.PUT, api+"/db/tables/:name/rows/:pk", c.wrap(auth, c.handleDBTableUpdate))
	router.HandleBlocking(breeze.DELETE, api+"/db/tables/:name/rows/:pk", c.wrap(auth, c.handleDBTableDelete))
}

// viewData builds the template data passed to every dashboard view.
// It includes the current page name, the base path for URL construction,
// the assets path, and the page title.
func (c *Collector) viewData(ctx *breeze.Context, page string) map[string]any {
	style, script := assetNames(c.cfg.DevMode)
	return map[string]any{
		"Page":         page,
		"BasePath":     strings.TrimSuffix(c.cfg.BasePath, "/"),
		"AssetsPath":   strings.TrimSuffix(c.cfg.BasePath, "/") + "/assets",
		"StyleFile":    style,
		"ScriptFile":   script,
		"PageTitle":    pageLabelFor(page),
		"FleetEnabled": c.cfg.FleetAggregatorURL != "",
	}
}

// pageLabelFor returns the human-readable title for a dashboard page.
func pageLabelFor(page string) string {
	titles := map[string]string{
		"overview":     "Overview",
		"routes":       "Routes",
		"api":          "API Explorer",
		"requests":     "Live Requests",
		"cache":        "Cache",
		"logs":         "Logs",
		"health":       "Health",
		"performance":  "Performance",
		"timeline":     "Timeline",
		"architecture": "Architecture",
		"events":       "Events",
		"video":        "Video Streaming",
		"fleet":        "Fleet View",
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
	return func(ctx *breeze.Context) error {
		auth(ctx)
		if ctx.Res != nil && (ctx.Res.Status == 401 || ctx.Res.Status == 403 || ctx.Res.Status == 302) {
			return nil
		}
		h(ctx)

		return nil
	}
}

// wrapService is wrap plus a service-to-service path, for endpoints the fleet
// aggregator fans out to on a human's behalf (§9C.2).
//
// The aggregator has no session cookie and no dashboard password — giving it one
// would mean putting the credential a person logs in with into another process's
// config. It instead presents ServiceToken, and this wrapper accepts that in
// place of a session. Order matters: the token is checked first, so a valid
// service call never depends on the browser auth path it cannot satisfy.
//
// When ServiceToken is unset there is no service path at all and this behaves
// exactly like wrap. That is what keeps the feature opt-in: an operator who
// never configures a token has not silently opened a second way in.
func (c *Collector) wrapService(auth breeze.HandlerFunc, h breeze.HandlerFunc) breeze.HandlerFunc {
	return func(ctx *breeze.Context) error {
		if token := ctx.Req.Header["x-fleet-token"]; token != "" {
			if c.cfg.ServiceToken == "" ||
				subtle.ConstantTimeCompare([]byte(token), []byte(c.cfg.ServiceToken)) != 1 {
				// A presented-but-wrong token is rejected outright
				// rather than falling through to session auth. Falling
				// through would turn a token typo into a confusing
				// login redirect instead of the 401 that names the
				// actual problem.
				ctx.Status(401)
				return ctx.JSON(map[string]any{"error": "unauthorized"})
			}
			h(ctx)
			return nil
		}
		c.wrap(auth, h)(ctx)

		return nil
	}
}

// ─── Handlers ────────────────────────────────────────────────────────────

func (c *Collector) handleOverview(ctx *breeze.Context) error {
	m := c.Metrics()
	recent := c.Requests(0)
	// Compute today's date boundary.
	history := c.MetricsHistory(60)
	return ctx.JSON(map[string]any{
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

// handleRoutes serves the routing table with live statistics and, for each
// route, the description its author wrote.
//
// # Why the descriptions are joined here
//
// The two facts live in different places and neither side can produce both. The
// collector knows what traffic a route has seen and nothing about what it is for;
// the Scalar registry knows the sentence the developer wrote and nothing about
// traffic. Joining them at read time is the only place both are in scope, and it
// costs one map build per request to this endpoint — an operator-facing page
// measured in requests per minute.
//
// The alternative would have been to copy the summary into the route accumulator
// at registration, which sounds cheaper and is worse: it would put a second copy
// of the documentation in a hot-path struct, and a route that gained a Doc
// wrapper after its first request would keep reporting the old one forever.
//
// # Why documented is reported explicitly
//
// A route with no Doc wrapper is absent from the OpenAPI document, which to
// anything consuming that document means "this endpoint does not exist". That is
// a real defect and it is invisible from the outside, so the join reports which
// routes it could not find rather than silently returning blank summaries.
func (c *Collector) handleRoutes(ctx *breeze.Context) error {
	base := strings.TrimSuffix(c.cfg.BasePath, "/")
	if base == "" {
		base = "/dashboard"
	}

	// Keyed by "METHOD /openapi/{path}", which is the form the registry stores.
	// Route patterns are converted to match rather than the other way round: the
	// registry is the side that also feeds the OpenAPI document, and rewriting it
	// here would be inventing a second normalisation.
	docs := make(map[string]scalar.RouteDoc, 16)
	for _, r := range scalar.Routes() {
		docs[strings.ToUpper(r.Method)+" "+r.Path] = r.Doc
	}

	stats := c.RouteStats()
	for i := range stats {
		describeRoute(&stats[i], docs)
	}

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
			stat := RouteStat{
				Method:     string(rt.Method()),
				Pattern:    pattern,
				Controller: "",
				Requests:   0,
			}
			describeRoute(&stat, docs)
			stats = append(stats, stat)
		}
	}
	return ctx.JSON(stats)
}

// describeRoute fills a RouteStat's documentation fields from the registry.
//
// Takes a pointer rather than returning a copy because it is called in a loop
// over a slice the caller owns, and copying a struct with three slices in it per
// route to achieve the same effect would be noise.
func describeRoute(stat *RouteStat, docs map[string]scalar.RouteDoc) {
	doc, found := docs[strings.ToUpper(stat.Method)+" "+breezeToOpenAPIPath(stat.Pattern)]
	if !found {
		return
	}
	stat.Documented = true
	stat.Summary = doc.Title
	stat.Description = doc.Description
	stat.Tags = doc.Tags
}

func (c *Collector) handleRequests(ctx *breeze.Context) error {
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
	return ctx.JSON(out)
}

func (c *Collector) handleCache(ctx *breeze.Context) error {
	return ctx.JSON(c.CacheStats())
}

func (c *Collector) handleCacheClear(ctx *breeze.Context) error {
	prefix := ctx.Query("prefix")
	_ = prefix
	c.cacheHits.Store(0)
	c.cacheMisses.Store(0)
	return ctx.JSON(map[string]any{"ok": true})
}

func (c *Collector) handleLogs(ctx *breeze.Context) error {
	level := ctx.Query("level")

	if level == "" {
		level = "app"
	}
	n := atoiDefault(ctx.Query("limit"), 500)
	q := ctx.Query("q")
	traceID := ctx.Query("trace_id")
	all := c.Logs(level, n)
	if q == "" && traceID == "" {
		return ctx.JSON(all)
	}
	out := make([]LogEntry, 0, len(all))
	for _, e := range all {
		if traceID != "" && e.TraceID != traceID {
			continue
		}
		if q == "" || strings.Contains(strings.ToLower(e.Message), strings.ToLower(q)) {
			out = append(out, e)
		}
	}
	return ctx.JSON(out)
}

func (c *Collector) handleCapabilities(ctx *breeze.Context) error {
	return ctx.JSON(map[string]any{"fleet_enabled": c.cfg.FleetAggregatorURL != ""})
}

// handleFleetProxy keeps aggregator credentials and topology off the browser.
// Only GET is registered: Fleet View is observability and must not mutate the
// aggregator through the dashboard seam.
func (c *Collector) handleFleetProxy(ctx *breeze.Context) error {
	path := strings.TrimPrefix(ctx.GetParam("path"), "/")
	base := strings.TrimSuffix(c.cfg.FleetAggregatorURL, "/")
	u := base + "/api/" + path
	if raw := ctx.Req.Query.Encode(); raw != "" {
		u += "?" + raw
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, u, nil)
	if err != nil {
		ctx.Status(502)
		return ctx.JSON(map[string]any{"error": "fleet aggregator unreachable", "url": base})
	}
	if c.cfg.FleetAggregatorUsername != "" && c.cfg.FleetAggregatorPassword != "" {
		req.SetBasicAuth(c.cfg.FleetAggregatorUsername, c.cfg.FleetAggregatorPassword)
	}
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		ctx.Status(502)
		return ctx.JSON(map[string]any{"error": "fleet aggregator unreachable", "url": base})
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		ctx.Status(502)
		return ctx.JSON(map[string]any{"error": "fleet aggregator response unreadable"})
	}
	ctx.Res = &breeze.HTTPResponse{Status: resp.StatusCode, Headers: map[string]string{"Content-Type": resp.Header.Get("Content-Type")}, Body: body}

	return nil
}

func (c *Collector) handleHealth(ctx *breeze.Context) error {
	return ctx.JSON(c.RunHealthChecks())
}

func (c *Collector) handlePerformance(ctx *breeze.Context) error {
	hist := c.MetricsHistory(120)
	pm := buildPerfMetrics(c)
	return ctx.JSON(map[string]any{
		"current": pm,
		"history": hist,
	})
}

func (c *Collector) handleTimelineList(ctx *breeze.Context) error {
	n := atoiDefault(ctx.Query("limit"), 50)
	return ctx.JSON(c.Timelines(n))
}

func (c *Collector) handleTimelineGet(ctx *breeze.Context) error {
	id := ctx.Param("id")
	for _, t := range c.Timelines(0) {
		if t.ID == id {
			return ctx.JSON(t)
		}
	}
	return jsonError(ctx, 404, "timeline not found")
}

func (c *Collector) handleArchitecture(ctx *breeze.Context) error {
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

	return ctx.JSON(map[string]any{
		"connections":  conns,
		"total":        total,
		"connected":    connected,
		"degraded":     degraded,
		"disconnected": disconnected,
		"unknown":      unknown,
	})
}

func (c *Collector) handleDBTables(ctx *breeze.Context) error {
	inspector := c.DBInspector()
	if inspector == nil {
		return ctx.JSON(map[string]any{"tables": []TableInfo{}})
	}
	tables, err := inspector.Tables()
	if err != nil {
		ctx.Status(500)
		return ctx.JSON(map[string]any{"error": err.Error()})
	}
	return ctx.JSON(map[string]any{"tables": tables})
}

func (c *Collector) handleDBTableData(ctx *breeze.Context) error {
	inspector := c.DBInspector()
	if inspector == nil {
		return ctx.JSON(TableData{Table: ctx.Param("name"), Page: 1, PageSize: 50, Total: 0, Rows: []map[string]any{}, Columns: []TableColumn{}})
	}
	page := atoiDefault(ctx.Query("page"), 1)
	pageSize := atoiDefault(ctx.Query("page_size"), 50)
	data, err := inspector.TableData(ctx.Param("name"), page, pageSize, ctx.Query("search"))
	if err != nil {
		ctx.Status(500)
		return ctx.JSON(map[string]any{"error": err.Error()})
	}
	data.Writable = c.cfg.AllowWrites && c.DBWriter() != nil
	return ctx.JSON(data)
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

func (c *Collector) handleDBTableInsert(ctx *breeze.Context) error {
	table := ctx.Param("name")
	writer, ok := c.writableGuard(ctx, table)
	if !ok {
		return nil
	}
	var req struct {
		Values map[string]any `json:"values"`
	}
	if err := jsonUnmarshal(ctx.Req.Body, &req); err != nil {
		ctx.Status(400)
		return ctx.JSON(map[string]any{"error": "invalid request body"})
	}
	row, err := writer.InsertRow(table, req.Values)
	if err != nil {
		ctx.Status(400)
		return ctx.JSON(map[string]any{"error": err.Error()})
	}
	c.invalidateTableCache(table)
	c.RecordLog("app", LogEntry{Time: now(), Message: fmt.Sprintf("db write: insert into %s", table)})
	ctx.Status(201)
	return ctx.JSON(row)
}

func (c *Collector) handleDBTableUpdate(ctx *breeze.Context) error {
	table := ctx.Param("name")
	writer, ok := c.writableGuard(ctx, table)
	if !ok {
		return nil
	}
	pk := parsePK(ctx.Param("pk"))
	var req struct {
		Values map[string]any `json:"values"`
	}
	if err := jsonUnmarshal(ctx.Req.Body, &req); err != nil {
		ctx.Status(400)
		return ctx.JSON(map[string]any{"error": "invalid request body"})
	}
	err := writer.UpdateRow(table, pk, req.Values)
	if errors.Is(err, ErrRowNotFound) {
		ctx.Status(404)
		return ctx.JSON(map[string]any{"error": "row not found"})
	}
	if err != nil {
		ctx.Status(400)
		return ctx.JSON(map[string]any{"error": err.Error()})
	}
	c.invalidateTableCache(table)
	c.RecordLog("app", LogEntry{Time: now(), Message: fmt.Sprintf("db write: update %s pk=%s", table, ctx.Param("pk"))})
	return ctx.JSON(map[string]any{"ok": true})
}

func (c *Collector) handleDBTableDelete(ctx *breeze.Context) error {
	table := ctx.Param("name")
	writer, ok := c.writableGuard(ctx, table)
	if !ok {
		return nil
	}
	pk := parsePK(ctx.Param("pk"))
	err := writer.DeleteRow(table, pk)
	if errors.Is(err, ErrRowNotFound) {
		ctx.Status(404)
		return ctx.JSON(map[string]any{"error": "row not found"})
	}
	if err != nil {
		ctx.Status(400)
		return ctx.JSON(map[string]any{"error": err.Error()})
	}
	c.invalidateTableCache(table)
	c.RecordLog("app", LogEntry{Time: now(), Message: fmt.Sprintf("db write: delete from %s pk=%s", table, ctx.Param("pk"))})
	ctx.Status(204)

	return nil
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

// buildPerfMetrics assembles a detailed runtime performance snapshot from a
// FRESH runtime.ReadMemStats() call.
//
// We do NOT reuse the cached MetricsSnapshot from the sampler because:
//  1. The sampler runs at 1Hz, so the cached value could be up to 1s stale.
//  2. The Performance page should show current data when the user opens it.
//  3. The sampler's MetricsSnapshot doesn't store all MemStats fields
//     (e.g. MSpanInuse, MCacheInuse, BuckHashSys) — we need the full
//     runtime.MemStats struct.
//
// Every field is mapped directly from runtime.MemStats with the correct
// semantics:
//
//	HeapAlloc    → HeapStats.Alloc      (live heap bytes, DROPS after GC)
//	TotalAlloc   → HeapStats.TotalAlloc (cumulative, only grows)
//	HeapSys      → HeapStats.Sys        (heap bytes from OS)
//	HeapIdle     → HeapStats.Idle       (idle heap bytes)
//	HeapInuse    → HeapStats.Inuse      (in-use heap bytes)
//	HeapReleased → HeapStats.Released   (bytes returned to OS)
//	HeapObjects  → HeapStats.Objects    (live heap object count)
//	Mallocs      → HeapStats.Mallocs    (cumulative count, only grows)
//	Frees        → HeapStats.Frees      (cumulative count, only grows)
//
//	StackInuse → StackStats.InUse
//	StackSys   → StackStats.Sys
//
//	NumGC        → GCStats.NumGC
//	LastGC       → GCStats.LastGC
//	NextGC       → GCStats.NextGC
//	PauseTotalNs → GCStats.PauseTotalNS (cumulative, only grows)
//	PauseNs[(NumGC-1)%256] → GCStats.PauseNS (most recent pause)
//	GCCPUFraction → GCStats.CPUFraction
//
//	Sys (MemStats.Sys) → MemoryStats.Sys (total from OS)
//	HeapAlloc           → MemoryStats.HeapInUse (live heap)
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
			Alloc:      ms.HeapAlloc,  // live heap — DROPS after GC
			TotalAlloc: ms.TotalAlloc, // cumulative — only grows
			Sys:        ms.HeapSys,    // heap bytes from OS
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
