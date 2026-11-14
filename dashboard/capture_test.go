package dashboard

// Tests for the capture policy: every request counted against its route and status class, a
// failure kept in full only within a bounded per-route sample, and nothing on that path
// allocating. The router is real and the middleware is the one Install is used with; the
// handlers only choose a status, which is all the policy reads.

import (
	"net/url"
	"testing"
	"time"

	"github.com/nelthaarion/breeze/v2"
)

// captureRig is a collector over a router with a few routes, and a way to send it requests.
type captureRig struct {
	coll *Collector
	mw   breeze.HandlerFunc
}

func newCaptureRig(t testing.TB, tune func(*Config)) *captureRig {
	t.Helper()

	router := breeze.NewRouter()
	h := func(ctx *breeze.Context) error { return nil }
	router.Handle(breeze.GET, "/api/orders/:id", h)
	router.Handle(breeze.POST, "/api/orders", h)
	router.Handle(breeze.GET, "/api/health", h)
	router.Handle(breeze.GET, "/api/never-called", h)

	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.DisableAuth = true
	if tune != nil {
		tune(&cfg)
	}
	coll := newCollector(cfg, router)
	return &captureRig{coll: coll, mw: Middleware(coll)}
}

// send drives one request through the middleware and returns once it has been counted.
func (r *captureRig) send(method breeze.Method, path string, status int) {
	ctx := breeze.NewContext(method, path)
	ctx.Req.Header["user-agent"] = "capture-test"
	ctx.SetMiddlewareChain([]breeze.HandlerFunc{r.mw}, func(c *breeze.Context) error {
		c.Status(status)
		return nil
	})
	_ = ctx.Next()
}

func (r *captureRig) sendN(n int, method breeze.Method, path string, status int) {
	for i := 0; i < n; i++ {
		r.send(method, path, status)
	}
}

func (r *captureRig) stat(t testing.TB, method, pattern string) RouteStat {
	t.Helper()
	for _, s := range r.coll.RouteStats() {
		if s.Method == method && s.Pattern == pattern {
			return s
		}
	}
	t.Fatalf("no statistics for %s %s among %v", method, pattern, r.coll.RouteStats())
	return RouteStat{}
}

func (r *captureRig) recorded(status int, route string) int {
	n := 0
	for _, rec := range r.coll.Requests(10000) {
		if rec.Status == status && rec.Route == route {
			n++
		}
	}
	return n
}

func TestEveryRequestIsCountedAgainstItsRoute(t *testing.T) {
	rig := newCaptureRig(t, nil)

	for i := 0; i < 500; i++ {
		rig.send(breeze.GET, "/api/orders/"+url.PathEscape("id-"+string(rune('a'+i%26))), 200)
	}
	rig.sendN(7, breeze.POST, "/api/orders", 201)

	got := rig.stat(t, "GET", "/api/orders/:id")
	if got.Requests != 500 || got.Errors != 0 || got.ClientErrors != 0 {
		t.Errorf("GET /api/orders/:id = %+v, want exactly 500 requests and no failures", got)
	}
	if got.LastRequest == "" {
		t.Error("the route's last request time is unset")
	}
	if post := rig.stat(t, "POST", "/api/orders"); post.Requests != 7 {
		t.Errorf("POST /api/orders requests = %d, want 7", post.Requests)
	}

	// The counts are exact and nothing was recorded: fast successes leave no full record,
	// which is what keeps the path cheap.
	if n := len(rig.coll.Requests(1000)); n != 0 {
		t.Errorf("%d full records were kept for requests that were fast and fine", n)
	}
	// A route nothing asked for is not in the statistics; the routes endpoint lists it from
	// the router's table.
	for _, s := range rig.coll.RouteStats() {
		if s.Pattern == "/api/never-called" {
			t.Errorf("a route nothing called is in the statistics: %+v", s)
		}
	}
	if total := rig.coll.requestsTotal.Load(); total != 507 {
		t.Errorf("requestsTotal = %d, want 507", total)
	}
}

func TestClientErrorsAreCountedExactlyAndKeptWithinTheBudget(t *testing.T) {
	rig := newCaptureRig(t, nil)
	rig.sendN(100, breeze.GET, "/api/orders/missing", 404)

	got := rig.stat(t, "GET", "/api/orders/:id")
	if got.ClientErrors != 100 || got.Errors != 0 || got.Requests != 100 {
		t.Fatalf("%+v, want 100 requests, 100 client errors, no server errors", got)
	}
	if n := rig.recorded(404, "/api/orders/:id"); n != clientErrorBudget {
		t.Errorf("%d full records of the 404 flood were kept, want the budget of %d", n, clientErrorBudget)
	}
	traffic := rig.coll.trafficPayload()
	if len(traffic.Routes) != 1 || traffic.Routes[0].NotRecorded != 100-clientErrorBudget {
		t.Errorf("traffic = %+v, want the %d dropped records reported", traffic.Routes, 100-clientErrorBudget)
	}
	if traffic.ClientErrors != 100 {
		t.Errorf("the traffic total of client errors = %d, want 100", traffic.ClientErrors)
	}
}

func TestRequestsNoRouteServedAreCountedAndBounded(t *testing.T) {
	rig := newCaptureRig(t, nil)
	rig.sendN(300, breeze.GET, "/wp-admin/setup.php", 404)

	traffic := rig.coll.trafficPayload()
	if u := traffic.Unmatched; u.Requests != 300 || u.ClientErrors != 300 || u.NotRecorded != 300-clientErrorBudget {
		t.Errorf("unmatched = %+v, want 300 counted and %d not kept", u, 300-clientErrorBudget)
	}
	if n := rig.recorded(404, ""); n != clientErrorBudget {
		t.Errorf("%d full records of unmatched requests were kept, want %d", n, clientErrorBudget)
	}
	// They are not a route: they must not appear among the routes' statistics.
	if n := len(rig.coll.RouteStats()); n != 0 {
		t.Errorf("RouteStats has %d entries after traffic that matched nothing", n)
	}
}

func TestServerErrorsKeepTheirOwnLargerBudget(t *testing.T) {
	rig := newCaptureRig(t, nil)
	rig.sendN(250, breeze.GET, "/api/health", 500)

	got := rig.stat(t, "GET", "/api/health")
	if got.Errors != 250 || got.ClientErrors != 0 {
		t.Fatalf("%+v, want 250 server errors", got)
	}
	if n := rig.recorded(500, "/api/health"); n != serverErrorBudget {
		t.Errorf("%d records kept, want the server error budget of %d", n, serverErrorBudget)
	}
	if rig.coll.errorsTotal.Load() != 250 {
		t.Errorf("errorsTotal = %d, want 250: the dashboard's own error count is not sampled", rig.coll.errorsTotal.Load())
	}
}

// A flood of one failure must not stop the dashboard keeping the next, different one.
func TestOneRoutesFloodDoesNotHideAnotherFailure(t *testing.T) {
	rig := newCaptureRig(t, nil)

	rig.sendN(200, breeze.GET, "/api/orders/x", 404) // exhausts that route's budget for 404s
	rig.send(breeze.GET, "/api/orders/x", 500)       // the same route, a different class
	rig.send(breeze.GET, "/api/health", 404)         // another route, the same class
	rig.send(breeze.GET, "/nothing-here", 404)       // no route, the same class

	if rig.recorded(500, "/api/orders/:id") != 1 {
		t.Error("a 500 on a route whose 404 budget was spent was not kept")
	}
	if rig.recorded(404, "/api/health") != 1 {
		t.Error("the first 404 on another route was not kept because a different route flooded")
	}
	if rig.recorded(404, "") != 1 {
		t.Error("the first unmatched 404 was not kept because a routed one flooded")
	}
}

func TestTheBudgetRefillsWhenTheWindowPasses(t *testing.T) {
	var s sampler
	base := time.Now().UnixNano()
	kept := 0
	for i := 0; i < 50; i++ {
		if s.admit(base, 5) {
			kept++
		}
	}
	if kept != 5 {
		t.Fatalf("kept %d in one window, want the budget of 5", kept)
	}
	later := base + int64(captureWindow) + 1
	if !s.admit(later, 5) {
		t.Error("the first failure of the next window was refused")
	}
	if s.dropped.Load() != 45 {
		t.Errorf("dropped = %d, want 45", s.dropped.Load())
	}
}

func TestSlowRequestsAreStillKeptInFull(t *testing.T) {
	rig := newCaptureRig(t, func(c *Config) { c.SlowRequestMs = 1 })

	ctx := breeze.NewContext(breeze.GET, "/api/health")
	ctx.SetMiddlewareChain([]breeze.HandlerFunc{rig.mw}, func(c *breeze.Context) error {
		time.Sleep(5 * time.Millisecond)
		c.Status(200)
		return nil
	})
	_ = ctx.Next()

	if n := rig.recorded(200, "/api/health"); n != 1 {
		t.Errorf("a slow 200 was kept %d times, want once", n)
	}
	if got := rig.stat(t, "GET", "/api/health"); got.MaxLatencyMS < 4 {
		t.Errorf("the route's maximum latency = %vms, want the slow request's 5ms counted", got.MaxLatencyMS)
	}
}

func TestEveryRequestIsKeptWhileSomeoneIsWatching(t *testing.T) {
	rig := newCaptureRig(t, nil)
	entry := rig.coll.resolveRoute("GET", "/api/health")

	if rig.coll.shouldCaptureFullRecord(time.Microsecond, 200, entry, time.Now().UnixNano()) {
		t.Fatal("a fast 200 is kept with nobody watching")
	}
	rig.coll.hub = &wsHub{clients: map[*breeze.WSConn]struct{}{{}: {}}}
	if !rig.coll.shouldCaptureFullRecord(time.Microsecond, 200, entry, time.Now().UnixNano()) {
		t.Error("a fast 200 is not kept while the dashboard is open")
	}
}

// The claim the whole design rests on: counting and sampling add no allocation per request, in
// the cases a flood is made of. The fixture allocates (a Context, a closure), so the measure is
// the difference from a chain with no middleware in it.
func TestCountingAndSamplingDoNotAllocatePerRequest(t *testing.T) {
	rig := newCaptureRig(t, nil)

	chain := func(mw breeze.HandlerFunc, method breeze.Method, path string, status int) func() {
		return func() {
			ctx := breeze.NewContext(method, path)
			ctx.SetMiddlewareChain([]breeze.HandlerFunc{mw}, func(c *breeze.Context) error {
				c.Status(status)
				return nil
			})
			_ = ctx.Next()
		}
	}
	passthrough := func(c *breeze.Context) error { return c.Next() }

	// Warm everything the first request builds (the index), and spend each budget.
	rig.sendN(200, breeze.GET, "/api/health", 404)
	rig.sendN(300, breeze.GET, "/api/health", 500)
	rig.sendN(200, breeze.GET, "/no/route/here", 404)

	for _, tc := range []struct {
		name   string
		method breeze.Method
		path   string
		status int
	}{
		{"a 200 on a static route", breeze.GET, "/api/health", 200},
		{"a 200 on a parameter route", breeze.GET, "/api/orders/12345", 200},
		{"a 404 over budget on a route", breeze.GET, "/api/health", 404},
		{"a 500 over budget on a route", breeze.GET, "/api/health", 500},
		{"a 404 over budget with no route", breeze.GET, "/no/route/here", 404},
	} {
		with := testing.AllocsPerRun(200, chain(rig.mw, tc.method, tc.path, tc.status))
		without := testing.AllocsPerRun(200, chain(passthrough, tc.method, tc.path, tc.status))
		if with > without {
			t.Errorf("%s: %.0f allocations with the middleware, %.0f without", tc.name, with, without)
		}
	}
}

func TestADisabledDashboardCountsNothing(t *testing.T) {
	rig := newCaptureRig(t, func(c *Config) { c.Enabled = false })
	rig.sendN(10, breeze.GET, "/api/health", 500)

	if n := rig.coll.requestsTotal.Load(); n != 0 {
		t.Errorf("a disabled dashboard counted %d requests", n)
	}
	if traffic := rig.coll.trafficPayload(); traffic.Enabled || !traffic.Instrumented {
		t.Errorf("traffic = enabled %v, instrumented %v; want disabled but with its middleware installed", traffic.Enabled, traffic.Instrumented)
	}
}

func TestTrafficSaysWhenNothingIsCounting(t *testing.T) {
	router := breeze.NewRouter()
	cfg := DefaultConfig()
	cfg.DisableAuth = true
	coll := Install(nil, router, cfg) // Install, and no router.Use(Middleware(coll))

	if traffic := coll.trafficPayload(); traffic.Instrumented {
		t.Error("traffic says the middleware is installed when nobody created it")
	}
	Middleware(coll)
	if traffic := coll.trafficPayload(); !traffic.Instrumented {
		t.Error("traffic does not say the middleware is installed once it is")
	}
}

func TestTrafficStatesThePolicyItAppliesAndRoutesAreListedWorstFirst(t *testing.T) {
	rig := newCaptureRig(t, func(c *Config) { c.SlowRequestMs = 750; c.MaxRequests = 321 })
	rig.sendN(3, breeze.GET, "/api/health", 404)
	rig.sendN(9, breeze.GET, "/api/orders/x", 500)
	rig.sendN(5, breeze.POST, "/api/orders", 200)

	traffic := rig.coll.trafficPayload()
	want := CapturePolicy{WindowSeconds: 10, ClientErrorsPerWindow: clientErrorBudget, ServerErrorsPerWindow: serverErrorBudget, SlowRequestMS: 750, RingCapacity: 321}
	if traffic.Capture != want {
		t.Errorf("capture = %+v, want %+v", traffic.Capture, want)
	}
	if len(traffic.Routes) != 2 || traffic.Routes[0].Pattern != "/api/orders/:id" || traffic.Routes[1].Pattern != "/api/health" {
		t.Errorf("routes = %+v, want the two failing routes, the 500s first, and not the route that only succeeded", traffic.Routes)
	}
	if traffic.Requests != 17 || traffic.ServerErrors != 9 || traffic.ClientErrors != 3 {
		t.Errorf("totals = %d requests, %d server errors, %d client errors; want 17, 9, 3", traffic.Requests, traffic.ServerErrors, traffic.ClientErrors)
	}
}

// ── persistence ──────────────────────────────────────────────────────────────

type memStorage struct{ state *PersistedState }

func (m *memStorage) Save(s *PersistedState) error { m.state = s; return nil }
func (m *memStorage) Load() (*PersistedState, error) {
	return m.state, nil
}
func (m *memStorage) Close() error { return nil }

func TestRouteCountsSurviveARestartWithTheirNamesAndKeepCounting(t *testing.T) {
	store := &memStorage{}

	before := newCaptureRig(t, nil)
	before.coll.storage = store
	before.sendN(40, breeze.GET, "/api/orders/a", 200)
	before.sendN(6, breeze.GET, "/api/orders/a", 404)
	before.sendN(2, breeze.GET, "/api/orders/a", 500)
	before.coll.saveState()

	after := newCaptureRig(t, nil)
	after.coll.storage = store
	after.coll.loadState()

	// Before anything has run, the restored counts carry their names.
	restored := after.stat(t, "GET", "/api/orders/:id")
	if restored.Requests != 48 || restored.ClientErrors != 6 || restored.Errors != 2 {
		t.Fatalf("restored = %+v, want 48 requests, 6 client errors, 2 errors", restored)
	}

	// And new traffic is added to them, not to a second accumulator that the index built.
	after.sendN(10, breeze.GET, "/api/orders/b", 200)
	if got := after.stat(t, "GET", "/api/orders/:id"); got.Requests != 58 {
		t.Errorf("requests = %d after a restart and 10 more, want 58", got.Requests)
	}
	n := 0
	for _, s := range after.coll.RouteStats() {
		if s.Pattern == "/api/orders/:id" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the route is listed %d times after a restart", n)
	}
}
