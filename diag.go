package breeze

// diag.go — the framework core's diagnostic probes.
//
// Seven subsystems live in this package and none of them had a diagnostic surface
// before: the router (how many routes, how many blocking, how many tagged as MCP
// tools), the worker pool (rejected and panicked task counts it already keeps),
// the template engine (cache size, and whether DevMode is re-parsing on every
// render in production), the i18n bundle (which locales actually loaded), the
// WebSocket hub (live connection count), Auto-MCP (whether it is listening, and on
// what), and the static-file mounts (which directory each prefix maps to, and
// whether it exists).
//
// Every one of them is answered from state the subsystem already holds. The pool
// counters existed already; the router's counts are a walk over a slice that is
// fixed after startup; the engine's cache size is a map length under the lock it
// already uses. Nothing here is added to a request path — the closest thing to
// one is the template cache read, and that happens only when a probe runs.
//
// # Registration
//
// Router, pool, Auto-MCP and static are registered by New, because that is where a
// Breeze is assembled and the handles are in scope. The template engine and i18n
// bundle register themselves in their constructors. The hub registers when it is
// created, which is the first WebSocket call — before that there is genuinely
// nothing to report and the probe would be a lie either way.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/nelthaarion/breeze/diag"
)

// Diagnostic registry keys. These match the names an agent will have seen from
// the generator's feature list where one exists, and are otherwise the obvious
// name for the subsystem.
const (
	diagRouter    = "router"
	diagPool      = "workerpool"
	diagTemplates = "templates"
	diagI18n      = "i18n"
	diagWebSocket = "websocket"
	diagAutoMCP   = "auto-mcp"
	diagStatic    = "static"
)

// staticCounter counts what ServeStatic served.
//
// Gated like the middleware counters, and for the same reason — this is a
// response path — but with one difference worth noting: the counted work here has
// already opened a file, stat'd it and read it into memory, so even with counting
// on the atomic is invisible. The gate is kept for uniformity rather than
// necessity.
var staticCounter diag.Counter

// registerCoreDiagnostics publishes the probes owned by a Breeze instance.
//
// Called by New. A second application in the same process replaces the first,
// which is the registry's uniform rule and the right one here: two Breeze
// instances in one process is a test fixture, not a deployment.
func (s *Breeze) registerCoreDiagnostics() {
	if s == nil {
		return
	}
	diag.Register(diagRouter, s.routerProbe)
	diag.Register(diagPool, s.poolProbe)
	diag.Register(diagAutoMCP, s.autoMCPProbe)
	diag.Register(diagWebSocket, s.webSocketProbe)
	diag.Register(diagStatic, s.staticProbe)
}

// staticProbe reports the static-file mounts.
//
// Separate from the router probe even though a static mount *is* a route, because
// the question is different: the router probe answers "is my routing table what I
// think it is", and this one answers "why is this file 404ing". The two facts that
// answer the second are the root directory each prefix maps to and whether the
// directory exists — and neither is recoverable from a wildcard route.
//
// The existence check is a stat per mount, at read time only. That is I/O in a
// probe, which the package rule warns against; it is admissible here because it is
// a single local stat of a directory, cannot block on anything remote, and is the
// entire content of the answer. A missing root is by far the most common cause of
// a static mount serving nothing.
func (s *Breeze) staticProbe() diag.Report {
	if s == nil || s.Router == nil {
		return diag.Off("no router is registered, so nothing is serving static files")
	}
	mounts := s.Router.staticMounts
	snap := staticCounter.Snapshot()

	if len(mounts) == 0 {
		report := diag.Off("no static mount is registered; call router.ServeStatic(prefix, root) "+
			"(or `breeze add static`)").
			WithDetail("auto_serve_root", s.Router.autoServeRoot).
			WithDetail("index_dir", s.Router.staticDir)
		if s.Router.autoServeRoot {
			return report.WithNotes(fmt.Sprintf("GET / still falls back to %s/index.html when no "+
				"route matches, which is a separate mechanism from ServeStatic and serves exactly "+
				"that one file.", s.Router.staticDir))
		}
		return report
	}

	entries := make([]map[string]any, 0, len(mounts))
	missing := make([]string, 0, len(mounts))
	for _, m := range mounts {
		abs, err := filepath.Abs(m.root)
		if err != nil {
			abs = m.root
		}
		info, statErr := os.Stat(m.root)
		exists := statErr == nil && info.IsDir()
		if !exists {
			missing = append(missing, m.root)
		}
		entries = append(entries, map[string]any{
			"prefix":     m.prefix,
			"root":       m.root,
			"root_abs":   abs,
			"root_found": exists,
		})
	}

	detail := map[string]any{
		"mounts":          entries,
		"files_served":    snap.Hits,
		"not_found":       snap.Misses,
		"read_errors":     snap.Errors,
		"bytes_sent":      snap.Bytes,
		"counting":        snap.Counting,
		"auto_serve_root": s.Router.autoServeRoot,
	}
	if snap.Last != "" {
		detail["last_request"] = snap.Last
	}

	summary := fmt.Sprintf("%d mount(s); %d file(s) served, %d not found",
		len(mounts), snap.Hits, snap.Misses)

	var notes []string
	if !snap.Counting {
		notes = append(notes, "Counted diagnostics are off, so the served and not-found counts "+
			"above were not measured — they are not a report of an unused mount. The mount list "+
			"and root_found are exact either way.")
	}
	notes = append(notes, "Paths are resolved relative to the process's working directory, which "+
		"is why root_abs is reported: a relative root that works when run from the project "+
		"directory fails under a service manager that starts elsewhere.")

	if len(missing) > 0 {
		return diag.Degraded(fmt.Sprintf("%s — %d root director(y/ies) do not exist: %v",
			summary, len(missing), missing), detail).
			WithNotes(append(notes, "A mount whose root does not exist answers 404 for every "+
				"request under its prefix, with no error at startup.")...)
	}
	return diag.OK(summary, detail).WithNotes(notes...)
}

// routerProbe reports the routing table.
//
// The counts are the ones that answer a real question. "How many routes" alone is
// trivia; how many are blocking tells an operator whether the event loops are
// protected from I/O, and how many are tagged as MCP tools tells an agent what
// part of this service it can drive.
func (s *Breeze) routerProbe() diag.Report {
	if s == nil || s.Router == nil {
		return diag.Off("no router is registered")
	}
	r := s.Router

	var blocking, withParams, wildcards int
	byMethod := map[string]int{}
	for _, rt := range r.routes {
		byMethod[string(rt.method)]++
		if rt.blocking {
			blocking++
		}
		if rt.paramCount > 0 {
			withParams++
		}
		if rt.hasWildcard {
			wildcards++
		}
	}

	detail := map[string]any{
		"routes":               len(r.routes),
		"by_method":            byMethod,
		"blocking":             blocking,
		"non_blocking":         len(r.routes) - blocking,
		"with_params":          withParams,
		"with_wildcard":        wildcards,
		"global_middleware":    len(r.middlewares),
		"mcp_tool_routes":      len(r.mcpTools),
		"static_dir":           r.staticDir,
		"auto_serve_root":      r.autoServeRoot,
		"inline_execution":     s.inlineExec,
		"zero_copy_headers":    s.zeroCopyHeaders,
		"custom_error_handler": s.ErrorHandler != nil,
	}

	summary := fmt.Sprintf("%d route(s), %d blocking, %d global middleware",
		len(r.routes), blocking, len(r.middlewares))

	var notes []string
	if len(r.routes) == 0 {
		notes = append(notes, "No routes are registered. A request will reach the static-file "+
			"fallback or a 404, never application code.")
	}
	if s.zeroCopyHeaders {
		notes = append(notes, "Zero-copy headers are on, so every header string on a request "+
			"references gnet's read buffer and must not outlive the handler. A handler that "+
			"stores one sees corrupted data later.")
	}
	if s.ErrorHandler == nil {
		notes = append(notes, "No ErrorHandler is set, so an error returned by a handler is "+
			"rendered by the framework default as RFC 9457 problem JSON.")
	}
	return diag.OK(summary, detail).WithNotes(notes...)
}

// poolProbe reports the worker pool.
//
// Rejected and panicked are the two numbers worth escalating on, and both were
// already being counted — the pool has kept them since it was written. Nothing
// here measures anything new; the probe just makes them reachable without a
// typed handle.
func (s *Breeze) poolProbe() diag.Report {
	if s == nil || s.Pool == nil {
		return diag.Off("no worker pool is registered; blocking routes have nowhere to run")
	}

	m := s.Pool.Metrics()
	detail := map[string]any{
		"workers":   m.Workers,
		"submitted": m.Submitted,
		"queued":    m.Queued,
		"spawned":   m.Spawned,
		"rejected":  m.Rejected,
		"panicked":  m.Panicked,
		"in_flight": m.InFlight,
	}

	summary := fmt.Sprintf("%d worker(s), %d task(s) submitted, %d in flight",
		m.Workers, m.Submitted, m.InFlight)

	var notes []string
	degraded := false
	if m.Rejected > 0 {
		degraded = true
		notes = append(notes, fmt.Sprintf("%d task(s) were rejected because the queue was full "+
			"under OverflowReject. Each rejection is a blocking request that never ran.", m.Rejected))
	}
	if m.Panicked > 0 {
		degraded = true
		notes = append(notes, fmt.Sprintf("%d task(s) panicked and were recovered by their worker. "+
			"The connection they were serving got no response.", m.Panicked))
	}
	if m.Spawned > 0 {
		notes = append(notes, fmt.Sprintf("%d task(s) ran on an overflow goroutine rather than a "+
			"pool worker, which means the queue was full at that moment. Sustained spawning means "+
			"the pool is undersized for the blocking work being submitted.", m.Spawned))
	}

	if degraded {
		return diag.Degraded(summary, detail).WithNotes(notes...)
	}
	return diag.OK(summary, detail).WithNotes(notes...)
}

// autoMCPProbe reports whether the tagged-route endpoint is listening.
//
// This is the subsystem with the least visible failure: MCPTool tags a route, and
// if EnableMCP was never called the tags do nothing at all and no error is
// produced anywhere. The probe reports both halves, so "I tagged four routes and
// the agent sees none" has an answer.
func (s *Breeze) autoMCPProbe() diag.Report {
	if s == nil {
		return diag.Off("no application is registered")
	}

	addr := s.AutoMCPAddr()
	tagged := 0
	if s.Router != nil {
		tagged = len(s.Router.mcpTools)
	}

	if addr == "" {
		report := diag.Off("Auto-MCP is not enabled; call app.EnableMCP(addr) to serve tagged "+
			"routes as MCP tools").WithDetail("tagged_routes", tagged)
		if tagged > 0 {
			return report.WithNotes(fmt.Sprintf("%d route(s) carry an MCPTool tag but no endpoint "+
				"is serving them, so the tags currently have no effect.", tagged))
		}
		return report
	}

	detail := map[string]any{
		"address":       addr,
		"tagged_routes": tagged,
	}
	if s.Router != nil {
		detail["tools"] = s.mcpToolNamesForDiag()
	}

	if tagged == 0 {
		return diag.Degraded(fmt.Sprintf("Auto-MCP is listening on %s but no route is tagged", addr),
			detail).WithNotes("Tag a route with breeze.MCPTool(name, description) as its last " +
			"middleware argument; an untagged route is never exposed.")
	}
	return diag.OK(fmt.Sprintf("Auto-MCP on %s exposing %d tagged route(s)", addr, tagged), detail)
}

// mcpToolNamesForDiag lists the tagged tool names, sorted.
//
// Sorted rather than in registration order, so two reads of an unchanged
// application produce identical output — a diff of two diagnostic snapshots
// should show only what actually changed.
func (s *Breeze) mcpToolNamesForDiag() []string {
	out := make([]string, 0, len(s.Router.mcpTools))
	for _, t := range s.Router.mcpTools {
		if t.spec != nil {
			out = append(out, t.spec.Name)
		}
	}
	sort.Strings(out)
	return out
}

// webSocketProbe reports the WebSocket hub.
func (s *Breeze) webSocketProbe() diag.Report {
	if s == nil || s.wsHub == nil {
		return diag.Off("no WebSocket endpoint is registered; call app.WebSocket(path, handler)")
	}

	paths := make([]string, 0, len(s.wsHandlers))
	for p := range s.wsHandlers {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	live := s.wsHub.Count()
	return diag.OK(fmt.Sprintf("%d live connection(s) across %d endpoint(s)", live, len(paths)),
		map[string]any{
			"connections": live,
			"endpoints":   paths,
		})
}

// ─── template engine ─────────────────────────────────────────────────────────

// registerDiagnostics publishes te as the process's template diagnostic.
//
// Called by NewTemplateEngine. A process with several engines — the dashboard
// builds two of its own — reports the last one constructed under "templates", and
// that is a genuine limitation rather than a design: unlike video mounts, template
// engines have no stable name to key on. The probe therefore reports the engine's
// directories, which is what tells a reader which engine they are looking at.
func (te *TemplateEngine) registerDiagnostics() {
	diag.Register(diagTemplates, te.probe)
}

// probe reports the template engine's state.
//
// The one number here that matters operationally is devMode. A production process
// with DevMode on re-parses every template from disk on every render, under a
// write lock — which turns a page render into a filesystem walk and serialises
// every concurrent render behind it. It is a silent 10-100x regression, and
// nothing else in the framework reports it.
func (te *TemplateEngine) probe() diag.Report {
	if te == nil {
		return diag.Off("no template engine is registered; call breeze.NewTemplateEngine(cfg)")
	}

	te.mu.RLock()
	views := len(te.templates)
	components := len(te.components)
	cached := make([]string, 0, views)
	for name := range te.templates {
		cached = append(cached, name)
	}
	te.mu.RUnlock()
	sort.Strings(cached)

	detail := map[string]any{
		"views_dir":         te.viewsDir,
		"components_dir":    te.compDir,
		"layout_file":       te.layoutFile,
		"cached_views":      views,
		"cached_components": components,
		"view_names":        cached,
		"dev_mode":          te.devMode,
		"i18n_bound":        te.i18n != nil,
		"custom_funcs":      len(te.funcMap),
	}

	summary := fmt.Sprintf("%d view(s) and %d component(s) cached from %s",
		views, components, te.viewsDir)

	if te.devMode {
		return diag.Degraded(summary+" — DevMode is on", detail).
			WithNotes("DevMode re-parses every template from disk on every render, under a write " +
				"lock, so concurrent renders serialise behind the parse. It is correct for " +
				"development and a severe regression in production. Set TemplateConfig.DevMode " +
				"to false there.")
	}
	if views == 0 && components == 0 {
		return diag.OK(summary, detail).
			WithNotes("Nothing is cached yet. Views are parsed on first render, so an empty cache " +
				"on a freshly started process is expected. Call Preload to parse them at startup " +
				"and surface a broken template before a request finds it.")
	}
	return diag.OK(summary, detail)
}

// ─── i18n ────────────────────────────────────────────────────────────────────

// registerDiagnostics publishes i as the process's i18n diagnostic.
func (i *I18n) registerDiagnostics() {
	diag.Register(diagI18n, i.probe)
}

// probe reports the translation bundle's state.
//
// The failure this exists for: NewI18n succeeds against a directory containing no
// locale files, because a glob matching nothing is not an error. Every T call then
// returns its key, and the pages render with "user.greeting" where the greeting
// should be. That is reported here as degraded, which is what it is.
func (i *I18n) probe() diag.Report {
	if i == nil {
		return diag.Off("no i18n bundle is registered; call breeze.NewI18n(cfg)")
	}

	locales := i.Locales()
	keys := map[string]int{}
	for _, loc := range locales {
		keys[loc] = len(i.Dict(loc))
	}

	detail := map[string]any{
		"dir":             i.dir,
		"default_locale":  i.def,
		"locales":         locales,
		"keys_per_locale": keys,
		"fallback":        i.fallback,
		"dev_mode":        i.devMode,
	}

	if len(locales) == 0 {
		return diag.Degraded(fmt.Sprintf("no locale files were loaded from %s", i.dir), detail).
			WithNotes("A directory with no <locale>.json files is not an error at load time, so " +
				"this bundle was constructed successfully and every T call now returns its key " +
				"verbatim. That is what renders as \"user.greeting\" on a page.")
	}

	summary := fmt.Sprintf("%d locale(s) loaded from %s, default %s", len(locales), i.dir, i.def)

	var notes []string
	if _, ok := keys[i.def]; !ok {
		notes = append(notes, fmt.Sprintf("The default locale %q is not among the loaded locales, "+
			"so a request that negotiates to it falls through to key echoing.", i.def))
	}
	if i.devMode {
		notes = append(notes, "DevMode re-reads every locale file from disk on each lookup miss. "+
			"Correct for development, unnecessary I/O in production.")
	}
	return diag.OK(summary, detail).WithNotes(notes...)
}
