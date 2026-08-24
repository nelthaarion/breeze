package dashboard

import (
	"fmt"
	"strings"
	"time"

	"github.com/nelthaarion/breeze"
)

// Middleware returns a Breeze middleware that instruments every request for
// the dashboard.
//
// # Performance: sampling-based collection
//
// The middleware uses a two-tier strategy to minimize overhead:
//
//  1. **Always** (zero-cost fast path): increment the request counter and
//     measure duration with time.Now()/time.Since(). This is ~5ns per
//     request and has no allocations.
//
//  2. **Only when someone is watching**: if there are WebSocket clients
//     connected (someone has the dashboard open), OR the request is slow
//     (duration > SlowRequestMs), OR the request errored (status >= 500),
//     we capture the full request record (IP, headers, route pattern,
//     timeline, etc.).
//
// This means when nobody is viewing the dashboard, the overhead is just
// an atomic increment + 2 time.Now() calls. When someone IS viewing, we
// do the full capture so the Live Requests feed and Timeline page work.
//
// # Critical: capture-before-Next pattern
//
// All values derived from ctx (IP, method, path, user-agent) are captured
// BEFORE ctx.Next() is called. After ctx.Next() returns, the handler may
// have closed the connection. See the crash analysis in PHASE 1-9 of the
// runtime audit for details.
func Middleware(c *Collector) breeze.HandlerFunc {
	base := strings.TrimSuffix(c.cfg.BasePath, "/")
	if base == "" {
		base = "/dashboard"
	}

	// Pre-compute whether timeline is enabled to avoid checking c.cfg.Timeline
	// on every request.
	timelineEnabled := c.cfg.Timeline

	return func(ctx *breeze.Context) {
		if !c.cfg.Enabled {
			ctx.Next()
			return
		}

		// Skip instrumenting the dashboard's own routes.
		// This is a single string comparison — no allocation.
		p := ctx.Req.Path
		if p == base || strings.HasPrefix(p, base+"/") {
			ctx.Next()
			return
		}

		// ── FAST PATH: just count + time ──────────────────────────────────
		//
		// We always measure the duration and count the request, but we
		// defer all expensive work (IP extraction, route matching, header
		// masking, timeline recording, JSON marshaling) until we know
		// someone is watching OR the request is interesting (slow/error).
		start := time.Now()

		// Timeline recorder: we must attach it BEFORE ctx.Next() because
		// downstream middleware/handlers call rec.Step() to record phases.
		// But we only attach it when someone is watching (WS clients) AND
		// timeline is enabled — otherwise it's wasted allocation.
		var rec *TimelineRecorder
		var endFn func(map[string]any)
		if timelineEnabled && c.hub != nil && c.hub.clientCount() > 0 {
			rec = NewTimelineRecorder(c.cfg)
			ctx.Set("breeze.dashboard.timeline", rec)
			endFn = rec.Step("Request Received")
		}

		// Continue the chain first — we need the duration + status to
		// decide whether to capture the full record.
		ctx.Next()

		duration := time.Since(start)
		status := 0
		if ctx.Res != nil {
			status = ctx.Res.Status
		}

		// Always count the request (atomic — zero contention).
		//
		// requestsToday used to be bumped here too. It was never reset
		// at a day boundary, so it counted requests since process start
		// — the same thing requestsTotal counts — while costing a second
		// atomic RMW in the same cache line. TodayCount() answers the
		// question it was named for, accurately. See collector.go.
		c.requestsTotal.Add(1)
		if status >= 500 {
			c.errorsTotal.Add(1)
		}

		// Track unique IP (for persistence — total unique viewers).
		// The IP was captured before Next() in the slow path, but on
		// the fast path we need to capture it here from headers only
		// (no ctx.Conn touch after Next).
		if ip := ctx.Req.Header["x-forwarded-for"]; ip != "" {
			c.trackUniqueIP(strings.TrimSpace(ip))
		}

		// Track daily count (for persistence — daily viewers).
		c.trackDailyCount()

		// Update per-route stats (only if we can match the route cheaply).
		// We do this lazily — only if there are stats being tracked or
		// if someone is viewing the routes page.
		shouldCapture := c.shouldCaptureFullRecord(duration, status)
		if !shouldCapture {
			// Nobody is watching and the request isn't interesting.
			// We're done — zero allocations, zero locks (beyond the atomic).
			return
		}

		// ── SLOW PATH: capture full record ────────────────────────────────
		//
		// We're either being watched (WS clients connected) or the request
		// is slow/errored. Now we do the full capture.
		//
		// NOTE: ctx.Conn may be closed by now. We do NOT touch ctx.Conn.
		//
		// Strings taken off ctx.Req are cloned. They are views into the
		// bytes the request was parsed from, and under breeze's
		// SetZeroCopyHeaders those bytes are the connection's read buffer,
		// which is reused for the next read — so a view is valid only
		// until the handler returns. Everything captured here outlives it:
		// the record goes into a ring buffer and is marshalled later,
		// possibly on the hub's goroutine. Without the copies a stored
		// record would silently rewrite itself into fragments of whatever
		// request came next.
		//
		// This is the slow path by construction, so the copies land on
		// requests that are already being watched or already slow.
		reqID := newID()
		method := string(ctx.Req.Method)
		path := strings.Clone(ctx.Req.Path)
		ua := strings.Clone(ctx.Req.Header["user-agent"])
		ip := clientIP(ctx)
		user := dashboardUser(ctx)
		routePattern := matchRoute(c, ctx)

		r := RequestRecord{
			ID:         reqID,
			Time:       start,
			Method:     method,
			Path:       path,
			Route:      routePattern,
			Status:     status,
			Duration:   duration.Microseconds(),
			DurationMS: float64(duration.Microseconds()) / 1000.0,
			IP:         ip,
			User:       user,
			UserAgent:  ua,
		}
		if ctx.Res != nil && ctx.Res.Body != nil {
			r.RespSize = len(ctx.Res.Body)
		}
		if c.cfg.Requests {
			r.Headers = maskHeaders(c.cfg, selectHeaders(ctx))
			r.TimelineID = reqID
		}
		if status >= 500 {
			r.Error = fmt.Sprintf("HTTP %d", status)
		}

		// Push to the ring buffer (mutex-protected, bounded).
		c.RecordRequest(r)

		// Push to WebSocket clients (JSON marshal + broadcast).
		// pushEvent checks clientCount() internally and is a no-op
		// when nobody is connected.
		if c.hub != nil {
			c.hub.pushEvent("request", r)
		}

		// Update per-route aggregation.
		if routePattern != "" {
			c.updateRouteStats(method, routePattern, duration.Microseconds(), status, start)
		}

		// Close the timeline recorder if we attached one.
		if endFn != nil {
			endFn(map[string]any{
				"method": method,
				"path":   path,
				"ip":     ip,
			})
		}
		if rec != nil {
			tl := Timeline{
				ID:     reqID,
				Time:   start,
				Method: method,
				Path:   path,
				Status: status,
				Total:  duration.Microseconds(),
				Steps:  rec.Build(),
			}
			c.RecordTimeline(tl)
			if c.hub != nil {
				c.hub.pushEvent("timeline", tl)
			}
		}
	}
}

// shouldCaptureFullRecord returns true if we should capture the full
// request record (IP, headers, route, etc.).
//
// We capture when ANY of these are true:
//   - There are WebSocket clients connected (someone is viewing the dashboard)
//   - The request is slow (duration > SlowRequestMs)
//   - The request errored (status >= 500)
//
// This keeps the fast path zero-allocation when nobody is watching.
func (c *Collector) shouldCaptureFullRecord(duration time.Duration, status int) bool {
	// Someone is viewing the dashboard — capture everything.
	if c.hub != nil && c.hub.clientCount() > 0 {
		return true
	}
	// Slow request — always capture for the timeline.
	if c.cfg.SlowRequestMs > 0 && duration.Microseconds() >= int64(c.cfg.SlowRequestMs)*1000 {
		return true
	}
	// Error — always capture for debugging.
	if status >= 500 {
		return true
	}
	return false
}

// updateRouteStats updates per-route aggregation for a request.
// This is called only when capturing a full record.
func (c *Collector) updateRouteStats(method, pattern string, durationUS int64, status int, when time.Time) {
	key := method + " " + pattern
	c.routeStatsMu.RLock()
	acc, ok := c.routeStats[key]
	c.routeStatsMu.RUnlock()
	if !ok {
		acc = &routeStatAccumulator{
			method:  method,
			pattern: pattern,
		}
		c.routeStatsMu.Lock()
		if existing, ok := c.routeStats[key]; ok {
			acc = existing
		} else {
			c.routeStats[key] = acc
		}
		c.routeStatsMu.Unlock()
	}
	acc.requests.Add(1)
	acc.totalDurUS.Add(durationUS)
	for {
		cur := acc.maxDurUS.Load()
		if durationUS <= cur || acc.maxDurUS.CompareAndSwap(cur, durationUS) {
			break
		}
	}
	acc.lastRequest.Store(when.UnixNano())
	if status >= 500 {
		acc.errors.Add(1)
	}
}

// selectHeaders returns a small subset of request headers for the inspector.
//
// Values are cloned. The keys are the constants below, but the values are views
// into the request buffer, and the returned map is stored on a RequestRecord
// that outlives the request. See the capture block in CollectorMiddleware.
func selectHeaders(ctx *breeze.Context) map[string]string {
	if ctx.Req == nil || ctx.Req.Header == nil {
		return nil
	}
	want := []string{
		"content-type", "content-length", "accept", "origin",
		"referer", "user-agent", "x-request-id", "authorization", "cookie",
	}
	out := make(map[string]string, len(want))
	for _, k := range want {
		if v, ok := ctx.Req.Header[k]; ok && v != "" {
			out[k] = strings.Clone(v)
		}
	}
	return out
}

// matchRoute finds the registered route pattern matching the request path.
func matchRoute(c *Collector, ctx *breeze.Context) string {
	if c.router == nil {
		return ""
	}
	reqPath := ctx.Req.Path
	reqSegments := splitPath(reqPath)
	for _, rt := range c.router.RoutesInfo() {
		if rt.Method() != ctx.Req.Method {
			continue
		}
		if routeMatches(rt, reqSegments) {
			return rt.Pattern()
		}
	}
	return ""
}

func splitPath(p string) []string {
	if len(p) > 0 && p[0] == '/' {
		p = p[1:]
	}
	if len(p) > 0 && p[len(p)-1] == '/' {
		p = p[:len(p)-1]
	}
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

func routeMatches(rt breeze.RouteInfo, req []string) bool {
	segs := rt.Segments()
	if rt.HasWildcard() {
		if len(req) < len(segs) {
			return false
		}
	} else if len(segs) != len(req) {
		return false
	}
	for i, s := range segs {
		if len(s) > 0 && s[0] == ':' {
			continue
		}
		if i >= len(req) {
			return false
		}
		if s != req[i] {
			return false
		}
	}
	return true
}

// clientIP extracts the client IP. Safe with nil ctx.Conn.
//
// The X-Forwarded-For branch returns a clone: the result is a slice of a header
// value, so it views the request buffer, and callers keep it (a RequestRecord in
// the ring buffer, a key in the unique-IP set). See the capture block in
// CollectorMiddleware.
func clientIP(ctx *breeze.Context) string {
	if ctx.Req != nil {
		if xff := ctx.Req.Header["x-forwarded-for"]; xff != "" {
			if i := strings.IndexByte(xff, ','); i > 0 {
				return strings.Clone(strings.TrimSpace(xff[:i]))
			}
			return strings.Clone(strings.TrimSpace(xff))
		}
	}
	if ctx.Conn != nil {
		if addr := ctx.Conn.RemoteAddr(); addr != nil {
			return addr.String()
		}
	}
	return ""
}

// dashboardUser extracts a username from the context store.
func dashboardUser(ctx *breeze.Context) string {
	if v, ok := ctx.Get("user"); ok {
		if m, ok := v.(map[string]any); ok {
			if u, ok := m["sub"].(string); ok && u != "" {
				return u
			}
			if u, ok := m["user_id"].(string); ok && u != "" {
				return u
			}
			if u, ok := m["username"].(string); ok && u != "" {
				return u
			}
		}
		return fmt.Sprint(v)
	}
	if v, ok := ctx.Get("breeze.dashboard.user"); ok {
		return fmt.Sprint(v)
	}
	return ""
}
