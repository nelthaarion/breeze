package dashboard

// api_extra.go — the dashboard endpoints and guards that sit beside api.go: the WebSocket
// guard, the queue / scheduler / log-count readers, the cache clear and job retry hooks,
// and the checks the Database Browser's write path runs before it hands anything to the
// application's DBWriter.

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/nelthaarion/breeze/v2"
)

// ─── Authentication state ────────────────────────────────────────────────

// authConfigured reports whether the dashboard has credentials to check a login against.
func (c *Collector) authConfigured() bool {
	return strings.TrimSpace(c.cfg.Username) != "" && c.cfg.Password != ""
}

// wsGuard decides whether a WebSocket handshake may upgrade.
//
// It follows the same rules as AuthMiddleware, which it used to diverge from in three
// ways. With no credentials configured it still honoured a session cookie, and the login
// endpoint would mint one for an empty username and password (both sides of the comparison
// were empty), so the default, "locked" dashboard streamed every request to anyone who
// asked. With DisableAuth set it demanded a cookie that nobody can obtain (there is no login
// to perform), so live updates never connected. And it never looked at where the handshake
// came from, so a page on another origin of the same site could open it with the person's
// cookie and read the stream.
func (c *Collector) wsGuard(ctx *breeze.Context) bool {
	if !c.cfg.Enabled || c.hub == nil {
		return false // switched off: there is no stream to join
	}
	if !requestOriginOK(ctx, true) {
		return false
	}
	if c.cfg.DisableAuth {
		return true
	}
	if !c.authConfigured() || c.sessions == nil {
		return false
	}
	token := extractCookieValue(ctx.Req.Header["cookie"], sessionCookieName)
	if token == "" {
		return false
	}
	username, ok := c.sessions.valid(token)
	if !ok {
		return false
	}
	ctx.Set("breeze.dashboard.user", username)
	return true
}

// renderPage renders one dashboard view and stamps it with the security headers.
func (c *Collector) renderPage(ctx *breeze.Context, page string) {
	c.engine.RenderView(ctx, page, c.viewData(ctx, page))
	if ctx.Res != nil {
		setSecurityHeaders(ctx)
	}
}

// auditUser names who is acting, for the lines the dashboard writes about its own writes.
func auditUser(ctx *breeze.Context) string {
	if v, ok := ctx.Get("breeze.dashboard.user"); ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return "unknown"
}

// ─── Hooks the application provides ──────────────────────────────────────

// CacheClearer removes cached entries whose key starts with prefix (all of them when prefix
// is empty) and reports how many it removed.
type CacheClearer func(prefix string) (int, error)

// JobRetrier asks the application's real queue to run a failed job again.
type JobRetrier func(id string) error

// SetCacheClearer connects the Cache page's Clear buttons to the application's cache.
//
// The dashboard cannot see inside a cache it does not own. Until one is attached the buttons
// answer 501 instead of pretending: they used to reset the hit/miss counters and report
// success, which looked like a cleared cache and was not.
func (c *Collector) SetCacheClearer(fn CacheClearer) {
	c.hooksMu.Lock()
	c.cacheClear = fn
	c.hooksMu.Unlock()
}

// SetJobRetrier connects the Queue page's Retry button to the application's queue.
func (c *Collector) SetJobRetrier(fn JobRetrier) {
	c.hooksMu.Lock()
	c.jobRetry = fn
	c.hooksMu.Unlock()
}

func (c *Collector) cacheClearer() CacheClearer {
	c.hooksMu.RLock()
	defer c.hooksMu.RUnlock()
	return c.cacheClear
}

func (c *Collector) jobRetrier() JobRetrier {
	c.hooksMu.RLock()
	defer c.hooksMu.RUnlock()
	return c.jobRetry
}

func (c *Collector) handleCacheClear(ctx *breeze.Context) error {
	var req struct {
		Prefix string `json:"prefix"`
	}
	if len(ctx.Req.Body) > 0 {
		_ = jsonUnmarshal(ctx.Req.Body, &req)
	}
	prefix := req.Prefix
	if prefix == "" {
		prefix = ctx.Query("prefix")
	}
	fn := c.cacheClearer()
	if fn == nil {
		return jsonError(ctx, http.StatusNotImplemented,
			"no cache is attached to the dashboard — call Collector.SetCacheClearer to enable clearing")
	}
	n, err := fn(prefix)
	if err != nil {
		return jsonError(ctx, http.StatusInternalServerError, err.Error())
	}
	if prefix == "" {
		c.cacheHits.Store(0)
		c.cacheMisses.Store(0)
	}
	c.RecordLog("app", LogEntry{Time: now(), Source: "dashboard",
		Message: fmt.Sprintf("cache cleared by %s (prefix=%q, %d entries)", auditUser(ctx), prefix, n)})
	return ctx.JSON(map[string]any{"ok": true, "cleared": n, "prefix": prefix})
}

// ─── Queue and scheduler ─────────────────────────────────────────────────

const maxQueueRowsServed = 500

// queuePayload is what the queue endpoint serves: the summary, the newest jobs, and whether
// the Retry button can do anything.
func (c *Collector) queuePayload() map[string]any {
	jobs := c.Jobs()
	sort.Slice(jobs, func(i, j int) bool {
		if !jobs[i].QueuedAt.Equal(jobs[j].QueuedAt) {
			return jobs[i].QueuedAt.After(jobs[j].QueuedAt)
		}
		return jobs[i].ID < jobs[j].ID
	})
	summary := c.QueueStats()
	if summary == (QueueStat{}) {
		// The application registered jobs but never reported a summary: derive it, so the
		// cards agree with the table under them.
		for _, j := range jobs {
			switch j.State {
			case "pending":
				summary.Pending++
			case "running":
				summary.Running++
			case "completed":
				summary.Completed++
			case "failed":
				summary.Failed++
			}
		}
	}
	if len(jobs) > maxQueueRowsServed {
		jobs = jobs[:maxQueueRowsServed]
	}
	return map[string]any{"summary": summary, "jobs": jobs, "can_retry": c.jobRetrier() != nil}
}

func (c *Collector) handleQueue(ctx *breeze.Context) error {
	return ctx.JSON(c.queuePayload())
}

func (c *Collector) handleQueueRetry(ctx *breeze.Context) error {
	var req struct {
		ID string `json:"id"`
	}
	if err := jsonUnmarshal(ctx.Req.Body, &req); err != nil || req.ID == "" {
		return jsonError(ctx, http.StatusBadRequest, "job id required")
	}
	fn := c.jobRetrier()
	if fn == nil {
		return jsonError(ctx, http.StatusNotImplemented,
			"no queue is attached to the dashboard — call Collector.SetJobRetrier to enable retrying")
	}
	var job *QueueJob
	for _, j := range c.Jobs() {
		if j.ID == req.ID {
			jc := j
			job = &jc
			break
		}
	}
	if job == nil {
		return jsonError(ctx, http.StatusNotFound, "job not found")
	}
	if job.State != "failed" {
		return jsonError(ctx, http.StatusConflict, "only failed jobs can be retried")
	}
	if err := fn(req.ID); err != nil {
		return jsonError(ctx, http.StatusBadGateway, err.Error())
	}
	c.RetryJob(req.ID)
	c.RecordLog("app", LogEntry{Time: now(), Source: "dashboard",
		Message: fmt.Sprintf("job %s retried by %s", req.ID, auditUser(ctx))})
	return ctx.JSON(map[string]any{"ok": true})
}

func (c *Collector) handleScheduler(ctx *breeze.Context) error {
	tasks := c.Tasks()
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].Name < tasks[j].Name })
	return ctx.JSON(tasks)
}

// logCounts is how many lines each log tab holds. The tabs load one at a time, so without
// this the badges on the tabs that were not opened read 0 while holding thousands of lines.
func (c *Collector) logCounts() map[string]int {
	return map[string]int{
		"app":     c.logsApp.Len(),
		"http":    c.logsHTTP.Len(),
		"error":   c.logsErrors.Len(),
		"panic":   c.logsPanics.Len(),
		"warning": c.logsWarn.Len(),
	}
}

func (c *Collector) handleLogCounts(ctx *breeze.Context) error {
	return ctx.JSON(c.logCounts())
}

// capabilitiesPayload tells the page what the application has wired up, so it can offer only
// controls that do something and say so when it cannot.
func (c *Collector) capabilitiesPayload() map[string]any {
	return map[string]any{
		"fleet_enabled":   c.cfg.FleetAggregatorURL != "",
		"slow_request_ms": c.cfg.SlowRequestMs,
		"slow_query_ms":   c.cfg.SlowQueryMs,
		"has_db":          c.DBInspector() != nil,
		"allow_writes":    c.cfg.AllowWrites && c.DBWriter() != nil && c.DBInspector() != nil,
		"can_clear_cache": c.cacheClearer() != nil,
		"can_retry_jobs":  c.jobRetrier() != nil,
		"video":           c.VideoAttached(),
	}
}

// ─── Database Browser: what a write may name ─────────────────────────────

const (
	maxDBPageSize = 500
	maxDBSearch   = 200
)

// identRe is what a column name has to look like when the inspector did not report the
// table's columns, so there is nothing to check it against. It is the conservative subset of
// SQL identifiers: whatever the application's DBWriter does with a name, it is not quoting,
// whitespace or punctuation.
var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]{0,127}$`)

// tableColumns returns the columns the inspector reports for a table, or nil.
//
// A cached inspector that already holds a page of this table answers from it, so a write
// does not cost an extra round trip to the database.
func tableColumns(inspector DBInspector, table string) []TableColumn {
	if ci, ok := inspector.(*cachedDBInspector); ok {
		if cols, found := ci.cachedColumns(table); found {
			return cols
		}
	}
	d, err := inspector.TableData(table, 1, 1, "")
	if err != nil {
		return nil
	}
	return d.Columns
}

// validateWriteKeys checks the column names a write request supplies before the DBWriter sees
// them. The table name was always checked against the inspector's list; the column names (the
// keys of "values" and of the primary key) were passed straight through, and a DBWriter that
// builds its SQL from them would have been injectable by anyone allowed to use the editor.
//
// With the table's columns known, every name has to be one of them and a primary key has to
// be exactly the table's primary-key columns. Without them, a name has to be a plain
// identifier.
func validateWriteKeys(cols []TableColumn, values, pk map[string]any, needPK bool) error {
	known := make(map[string]TableColumn, len(cols))
	for _, col := range cols {
		known[col.Name] = col
	}
	check := func(kind, name string) error {
		if len(cols) > 0 {
			if _, ok := known[name]; !ok {
				return fmt.Errorf("unknown column: %s", name)
			}
			return nil
		}
		if !identRe.MatchString(name) {
			return fmt.Errorf("invalid %s name", kind)
		}
		return nil
	}
	for name := range values {
		if err := check("column", name); err != nil {
			return err
		}
	}
	if !needPK {
		return nil
	}
	if len(pk) == 0 {
		return errors.New("primary key required")
	}
	for name := range pk {
		if err := check("key", name); err != nil {
			return err
		}
	}
	if len(cols) > 0 {
		pkCols := 0
		for _, col := range cols {
			if col.PrimaryKey {
				pkCols++
				if _, ok := pk[col.Name]; !ok {
					return fmt.Errorf("primary key column %s is missing", col.Name)
				}
			}
		}
		if pkCols == 0 {
			return errors.New("table has no primary key; rows cannot be addressed")
		}
		for name := range pk {
			if !known[name].PrimaryKey {
				return fmt.Errorf("%s is not a primary-key column", name)
			}
		}
	}
	return nil
}

// parsePKStrict is parsePK for the write path: a malformed or empty key is an error rather
// than a smaller key. parsePK drops a pair it cannot read, so "id=1,%zz" became {id: 1} and
// "garbage" became {} — and a DBWriter handed an empty key has only its own checks between it
// and a statement with no WHERE clause.
func parsePKStrict(raw string) (map[string]any, error) {
	if raw == "" {
		return nil, errors.New("primary key required")
	}
	pk := make(map[string]any)
	for _, pair := range strings.Split(raw, ",") {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) != 2 {
			return nil, errors.New("malformed primary key")
		}
		key, err1 := url.PathUnescape(kv[0])
		val, err2 := url.PathUnescape(kv[1])
		if err1 != nil || err2 != nil || key == "" {
			return nil, errors.New("malformed primary key")
		}
		pk[key] = val
	}
	return pk, nil
}

// clampPage keeps the Database Browser's paging within sane bounds.
func clampPage(page, pageSize int) (int, int) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 50
	}
	if pageSize > maxDBPageSize {
		pageSize = maxDBPageSize
	}
	return page, pageSize
}

// validFleetPath reports whether p is a path the fleet proxy will forward: plain URL path
// characters, no empty or dot segments. Anything else could walk out of /api/ on the
// aggregator, to endpoints the dashboard's credentials were never meant to reach.
func validFleetPath(p string) bool {
	if p == "" || len(p) > 256 {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	for i := 0; i < len(p); i++ {
		ch := p[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9':
		case ch == '-', ch == '_', ch == '.', ch == '/', ch == '~', ch == ':', ch == '@':
		default:
			return false
		}
	}
	return true
}
