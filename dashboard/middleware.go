package dashboard

import (
	"fmt"
	"strings"
	"time"

	"github.com/nelthaarion/breeze/v2"
)

// Middleware returns a Breeze middleware that instruments every request for
// the dashboard.
//
// # Performance: count everything, record what is worth recording
//
// The middleware uses a two-tier strategy to minimize overhead:
//
//  1. **Every request** (no allocation): resolve its route, add it to that
//     route's counters by status class, and fold its duration into the route's
//     average and maximum. A route lookup, a few atomic adds and two
//     time.Now() calls. The counts are exact, so a route that served a
//     thousand fast requests says so, and a 404 or a validation failure is
//     counted against the route it hit (or against "no route matched").
//
//  2. **Only when the request is worth a full record**: there are WebSocket
//     clients connected (someone has the dashboard open), OR the request is
//     slow (duration > SlowRequestMs), OR it failed (status >= 400) and its
//     route is still inside its sampling budget for the window. We then capture
//     the full request record (IP, headers, route pattern, timeline, etc.).
//
// A failure is sampled rather than always kept so that a flood of one (a
// client enumerating missing ids, a retry loop, a bad deploy) cannot churn the ring buffer or make the
// middleware allocate per request; routeindex.go states the budgets, and what
// they drop is counted. 2xx and 3xx stay unrecorded unless watched or slow.
//
// # Critical: capture-before-Next pattern
//
// All values derived from ctx (IP, method, path, user-agent) are captured
// BEFORE ctx.Next() is called, because after ctx.Next() returns the handler may
// have released the request back to the pool — every string on ctx.Req points
// into a pooled read buffer, and reading one afterwards yields whatever the next
// request wrote there. The failure mode is not a crash but a captured record
// attributed to the wrong request, which is worse.
func Middleware(c *Collector) breeze.HandlerFunc {
	base := strings.TrimSuffix(c.cfg.BasePath, "/")
	if base == "" {
		base = "/dashboard"
	}

	// Pre-compute whether timeline is enabled to avoid checking c.cfg.Timeline
	// on every request.
	timelineEnabled := c.cfg.Timeline

	// Say that something is counting. A dashboard that was installed but whose middleware was
	// never added would otherwise report zero for every route, which reads as "no traffic".
	c.instrumented.Store(true)

	return func(ctx *breeze.Context) error {
		if !c.cfg.Enabled {
			return ctx.Next()
		}

		// Skip instrumenting the dashboard's own routes.
		// This is a single string comparison — no allocation.
		p := ctx.Req.Path
		if p == base || strings.HasPrefix(p, base+"/") {
			return ctx.Next()
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
		//
		// The error is held rather than returned: this middleware's entire purpose is
		// the timing, counters and captured record below, and a failing request is
		// precisely the one an operator opens the dashboard to look at. Returning
		// early would make errors the only requests the dashboard never sees.
		chainErr := ctx.Next()

		duration := time.Since(start)
		status := 0
		if ctx.Res != nil {
			status = ctx.Res.Status
		}
		// The chain's error has not been turned into a response yet (that
		// happens after the whole chain unwinds), so Res.Status is still 0 for
		// a handler that returned an error. Resolve it as the default error
		// handler will, or errorsTotal and the captured record miss every
		// failing request.
		if chainErr != nil {
			status = breeze.ErrorStatus(chainErr)
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

		// Count the request against its route and status class. This is the part that used
		// to happen only for a captured request, which is why a route could report zero
		// requests after serving thousands. It allocates nothing.
		entry := c.resolveRoute(string(ctx.Req.Method), ctx.Req.Path)
		when := start.UnixNano()
		entry.record(status, duration.Microseconds(), when)

		shouldCapture := c.shouldCaptureFullRecord(duration, status, entry, when)
		if !shouldCapture {
			// Nobody is watching and the request isn't interesting, or it is a failure over
			// its route's sampling budget (and counted as such). We're done — zero
			// allocations, zero locks (beyond the atomics).
			return chainErr
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
		routePattern := entry.pattern

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
			pushEvent(c.hub, "request", r)
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
				pushEvent(c.hub, "timeline", tl)
			}
		}

		return chainErr
	}
}

// shouldCaptureFullRecord returns true if we should capture the full
// request record (IP, headers, route, etc.).
//
// We capture when ANY of these are true:
//   - There are WebSocket clients connected (someone is viewing the dashboard)
//   - The request is slow (duration > SlowRequestMs)
//   - The request failed (status >= 400) and its route is within its sampling
//     budget for the current window (see captureWindow in routeindex.go)
//
// This keeps the fast path zero-allocation when nobody is watching, and bounds
// what a flood of one failure can cost.
func (c *Collector) shouldCaptureFullRecord(duration time.Duration, status int, entry *routeEntry, now int64) bool {
	// Someone is viewing the dashboard — capture everything.
	if c.hub != nil && c.hub.clientCount() > 0 {
		return true
	}
	// Slow request — always capture for the timeline.
	if c.cfg.SlowRequestMs > 0 && duration.Microseconds() >= int64(c.cfg.SlowRequestMs)*1000 {
		return true
	}
	// Failure — captured for debugging, within the route's budget.
	return entry.admitFailure(status, now)
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
