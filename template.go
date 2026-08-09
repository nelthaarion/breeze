package breeze

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// bodyOpenTag matches the opening <body ...> tag (with or without
// attributes), case-insensitively, so the SPA runtime can be injected
// immediately after it regardless of how the layout wrote it.
var bodyOpenTag = regexp.MustCompile(`(?i)<body[^>]*>`)

// ─── Template Engine ──────────────────────────────────────────────────────────
//
// BreezeTemplate provides a server-side HTML template engine that renders views
// and components using Go's html/template package, with a built-in SPA runtime
// injected automatically into every page response.
//
// SPA behaviour:
//   - All <a href="..."> clicks are intercepted client-side.
//   - Navigation sends a fetch() to the same URL with header "X-Breeze-Partial: true".
//   - The server detects this header and returns only the inner content (no layout).
//   - The client swaps the content into the #breeze-app container without a full reload.
//   - The browser URL and history are updated with pushState.
//
// Views vs Components:
//   - Views are full pages (optionally wrapped in a layout).
//   - Components are partial HTML fragments that can be embedded via {{component "name" .}}
//
// Directory structure (configurable):
//
//	views/
//	  layout.html          ← optional shared layout (define "layout")
//	  home.html
//	  about.html
//	components/
//	  nav.html             ← define "nav"
//	  card.html            ← define "card"

// TemplateEngine holds all parsed templates and configuration.
type TemplateEngine struct {
	mu         sync.RWMutex
	templates  map[string]*template.Template // view name → full template set
	components map[string]*template.Template // component name → template
	viewsDir   string
	compDir    string
	layoutFile string
	funcMap    template.FuncMap
	devMode    bool  // if true, re-parse on every render (hot reload)
	i18n       *I18n // optional translation bundle backing the t helper
}

// TemplateConfig configures the template engine.
type TemplateConfig struct {
	// ViewsDir is the directory containing view templates. Default: "views"
	ViewsDir string
	// ComponentsDir is the directory containing component templates. Default: "components"
	ComponentsDir string
	// LayoutFile is the path to the layout template. Default: "views/layout.html"
	// Set to "" to disable layout wrapping.
	LayoutFile string
	// FuncMap adds custom template functions.
	FuncMap template.FuncMap
	// DevMode disables template caching so changes are reflected immediately.
	DevMode bool
	// I18n enables the {{t "some.key"}} translation helper, bound per
	// request to the locale resolved by middleware.LocaleMiddleware.
	// When nil, t still parses but echoes the key.
	I18n *I18n
}

// NewTemplateEngine creates a template engine from the given config.
func NewTemplateEngine(cfg TemplateConfig) *TemplateEngine {
	if cfg.ViewsDir == "" {
		cfg.ViewsDir = "views"
	}
	if cfg.ComponentsDir == "" {
		cfg.ComponentsDir = "components"
	}
	if cfg.LayoutFile == "" {
		cfg.LayoutFile = filepath.Join(cfg.ViewsDir, "layout.html")
	}

	te := &TemplateEngine{
		templates:  make(map[string]*template.Template),
		components: make(map[string]*template.Template),
		viewsDir:   cfg.ViewsDir,
		compDir:    cfg.ComponentsDir,
		layoutFile: cfg.LayoutFile,
		devMode:    cfg.DevMode,
		funcMap:    cfg.FuncMap,
		i18n:       cfg.I18n,
	}

	if te.funcMap == nil {
		te.funcMap = template.FuncMap{}
	}

	// Built-in "component" function — renders a named component with data.
	// Rebound per locale in funcsForLocale so nested components translate.
	te.funcMap["component"] = func(name string, data any) (template.HTML, error) {
		var buf bytes.Buffer
		if err := te.renderComponent(name, "", data, &buf); err != nil {
			return "", err
		}
		return template.HTML(buf.String()), nil
	}

	// Built-in "partial" alias for component.
	te.funcMap["partial"] = te.funcMap["component"]

	// Built-in "t" translation helper. This base version covers templates
	// rendered with no locale; funcsForLocale rebinds it per locale.
	te.funcMap["t"] = te.tFunc("")

	// Built-in "map" helper: create a map[string]any inline inside a template.
	// Usage: {{component "card" (map "title" "Hello" "body" "World")}}
	te.funcMap["map"] = func(pairs ...any) (map[string]any, error) {
		if len(pairs)%2 != 0 {
			return nil, fmt.Errorf("breeze/template: map requires an even number of arguments (key-value pairs)")
		}
		m := make(map[string]any, len(pairs)/2)
		for i := 0; i < len(pairs); i += 2 {
			key, ok := pairs[i].(string)
			if !ok {
				return nil, fmt.Errorf("breeze/template: map keys must be strings")
			}
			m[key] = pairs[i+1]
		}
		return m, nil
	}

	return te
}

// ─── i18n plumbing ────────────────────────────────────────────────────────────
//
// Go binds template funcs at parse time while the locale is per-request, so
// template sets are parsed and cached once per (name, locale) pair with t
// bound to that locale. Locales are a small finite set, so this costs a few
// extra cached sets and zero per-request cloning.

// tFunc returns a t helper bound to locale. Without a bundle it echoes the
// key so templates using t still parse and render.
func (te *TemplateEngine) tFunc(locale string) func(key string, args ...any) string {
	return func(key string, args ...any) string {
		if te.i18n == nil {
			return key
		}
		return te.i18n.T(locale, key, args...)
	}
}

// funcsForLocale returns the engine funcMap with t and component/partial
// rebound to the given locale, so translation reaches nested components.
func (te *TemplateEngine) funcsForLocale(locale string) template.FuncMap {
	if te.i18n == nil || locale == "" {
		return te.funcMap
	}
	fm := make(template.FuncMap, len(te.funcMap))
	for k, v := range te.funcMap {
		fm[k] = v
	}
	fm["t"] = te.tFunc(locale)
	fm["component"] = func(name string, data any) (template.HTML, error) {
		var buf bytes.Buffer
		if err := te.renderComponent(name, locale, data, &buf); err != nil {
			return "", err
		}
		return template.HTML(buf.String()), nil
	}
	fm["partial"] = fm["component"]
	return fm
}

// localeKey builds the cache key for a (template name, locale) pair.
// \x00 cannot appear in either part, so keys are unambiguous.
func localeKey(name, locale string) string {
	if locale == "" {
		return name
	}
	return name + "\x00" + locale
}

// requestLocale resolves the locale for a render: the one set by the locale
// middleware, else the bundle default, else "".
func (te *TemplateEngine) requestLocale(ctx *Context) string {
	if l := ctx.Locale(); l != "" {
		return l
	}
	if te.i18n != nil {
		return te.i18n.DefaultLocale()
	}
	return ""
}

// ─── Parsing ──────────────────────────────────────────────────────────────────

// parseView parses a view file together with all component files and the layout
// (if present) into a single *template.Template set, with funcs bound to locale.
func (te *TemplateEngine) parseView(viewName, locale string) (*template.Template, error) {
	viewPath := filepath.Join(te.viewsDir, viewName+".html")

	// Collect files: view first, then components, then layout.
	files := []string{viewPath}

	// Glob all component files.
	compFiles, _ := filepath.Glob(filepath.Join(te.compDir, "*.html"))
	files = append(files, compFiles...)

	// Include layout if it exists and is not the view itself.
	if te.layoutFile != "" {
		if _, err := os.Stat(te.layoutFile); err == nil {
			absLayout, _ := filepath.Abs(te.layoutFile)
			absView, _ := filepath.Abs(viewPath)
			if absLayout != absView {
				files = append(files, te.layoutFile)
			}
		}
	}

	t := template.New(filepath.Base(viewPath)).Funcs(te.funcsForLocale(locale))
	return t.ParseFiles(files...)
}

// parseComponent parses a single component file with funcs bound to locale.
func (te *TemplateEngine) parseComponent(name, locale string) (*template.Template, error) {
	path := filepath.Join(te.compDir, name+".html")
	t := template.New(filepath.Base(path)).Funcs(te.funcsForLocale(locale))
	return t.ParseFiles(path)
}

// ─── Rendering ────────────────────────────────────────────────────────────────

// renderComponent renders a named component to w.
//
// Component files define a named block: {{define "nav"}}...{{end}}
// After ParseFiles, that block lives as a named template inside the set.
// We must call t.Lookup(name) — t.Execute() runs the anonymous root template
// (the file wrapper), which is empty and produces no output.
func (te *TemplateEngine) renderComponent(name, locale string, data any, w *bytes.Buffer) error {
	exec := func(t *template.Template) error {
		named := t.Lookup(name)
		if named == nil {
			named = t // fallback: no {{define}}, execute root directly
		}
		return named.Execute(w, data)
	}

	if te.devMode {
		t, err := te.parseComponent(name, locale)
		if err != nil {
			return fmt.Errorf("breeze/template: component %q: %w", name, err)
		}
		return exec(t)
	}

	key := localeKey(name, locale)
	te.mu.RLock()
	t, ok := te.components[key]
	te.mu.RUnlock()

	if !ok {
		var err error
		t, err = te.parseComponent(name, locale)
		if err != nil {
			return fmt.Errorf("breeze/template: component %q: %w", name, err)
		}
		te.mu.Lock()
		te.components[key] = t
		te.mu.Unlock()
	}
	return exec(t)
}

// RenderView renders a view and writes the response to ctx.
//
//   - In a normal request it wraps the view in the layout (if defined) and
//     injects the Breeze SPA runtime script.
//   - When the request carries "X-Breeze-Partial: true" it returns only the
//     inner view fragment so the client-side router can swap it in without a
//     full page reload.
func (te *TemplateEngine) RenderView(ctx *Context, viewName string, data any) {
	isPartial := ctx.Req.Header["x-breeze-partial"] == "true"
	locale := te.requestLocale(ctx)

	if te.devMode && te.i18n != nil {
		te.i18n.reloadIfDev()
	}

	var buf bytes.Buffer

	t, err := te.getView(viewName, locale)
	if err != nil {
		ctx.Status(500)
		ctx.WriteString(fmt.Sprintf("template error: %v", err))
		return
	}
	if err := te.execView(t, viewName, locale, data, isPartial, &buf); err != nil {
		ctx.Status(500)
		ctx.WriteString(fmt.Sprintf("render error: %v", err))
		return
	}

	ctx.HTML(buf.Bytes())
}

// getView returns the parsed template set for (viewName, locale), parsing
// and caching it on first use. In devMode it re-parses every time.
func (te *TemplateEngine) getView(viewName, locale string) (*template.Template, error) {
	if te.devMode {
		return te.parseView(viewName, locale)
	}

	key := localeKey(viewName, locale)
	te.mu.RLock()
	t, ok := te.templates[key]
	te.mu.RUnlock()
	if ok {
		return t, nil
	}

	t, err := te.parseView(viewName, locale)
	if err != nil {
		return nil, err
	}
	te.mu.Lock()
	te.templates[key] = t
	te.mu.Unlock()
	return t, nil
}

// execView executes the right template definition inside t.
//
// Resolution order:
//  1. If partial → execute the template named after the view file (the content block).
//  2. If layout is defined → execute "layout" (which embeds content via {{template "content" .}}).
//  3. Otherwise → execute the view template directly.
func (te *TemplateEngine) execView(
	t *template.Template,
	viewName string,
	locale string,
	data any,
	isPartial bool,
	buf *bytes.Buffer,
) error {
	// Wrap data with template helpers.
	td := &TemplateData{
		Data:    data,
		Locale:  locale,
		engine:  te,
		partial: isPartial,
	}

	if isPartial {
		// Render only the content block; client swaps it.
		contentTmpl := t.Lookup("content")
		if contentTmpl == nil {
			// No "content" block defined — render the whole view file.
			contentTmpl = t.Lookup(viewName + ".html")
		}
		if contentTmpl == nil {
			contentTmpl = t
		}
		if err := contentTmpl.Execute(buf, td); err != nil {
			return err
		}

		// Embed the same data/template-sources/i18n blob that a full page
		// load would carry, *inside* the fragment itself. Partial responses
		// are swapped into #breeze-app via innerHTML, so anything appended
		// here becomes a child of the swapped container and the client
		// runtime can pick it up and refresh breeze.data()/the client-side
		// template evaluator/i18n dict for the new route. Without this,
		// those caches only ever reflect the very first full page load and
		// silently go stale after the first SPA navigation.
		dataJSON, jsonErr := marshalPageData(data)
		if jsonErr != nil {
			dataJSON = []byte("{}")
		}
		tmplSources := te.collectTemplateSources(viewName)
		buf.WriteString(breezeDataScript(dataJSON))
		buf.WriteString(breezeTemplateScript(tmplSources))
		buf.WriteString(te.breezeI18nScript(locale))
		return nil
	}

	// Full page: prefer a "layout" definition, else render view directly.
	layoutTmpl := t.Lookup("layout")
	if layoutTmpl != nil {
		if err := layoutTmpl.Execute(buf, td); err != nil {
			return err
		}
	} else {
		viewTmpl := t.Lookup(viewName + ".html")
		if viewTmpl == nil {
			viewTmpl = t
		}
		if err := viewTmpl.Execute(buf, td); err != nil {
			return err
		}
	}

	// Serialize page data as JSON so breeze.data() can read it client-side.
	dataJSON, jsonErr := marshalPageData(data)
	if jsonErr != nil {
		dataJSON = []byte("{}")
	}

	// Embed raw template sources so the client can re-render without a server round-trip.
	tmplSources := te.collectTemplateSources(viewName)

	// Inject data tag + template sources + SPA runtime right after the
	// opening <body> tag — NOT just before </body>.
	//
	// The runtime defines window.breeze / window.Breeze, and the data/tmpl/
	// i18n tags are what breeze.data() and the client-side template
	// evaluator read from. If injected at the end of <body> (the previous
	// behaviour), any inline <script> inside the page's own content —
	// which the parser reaches first, since it comes earlier in the
	// document — would execute before those globals exist. That's
	// invisible during ordinary SPA navigation, because by then the
	// runtime is already loaded from a prior page and simply persists
	// (only #breeze-app's children get replaced). But it bites on the very
	// first load of a route reached directly, and on every hard refresh:
	// any breeze.*/Breeze.* call made at the top level of a view's own
	// inline script throws a ReferenceError before the runtime has had a
	// chance to define those globals, silently breaking that view — the
	// "sometimes with refresh, things stop working" symptom. Injecting
	// right after <body> guarantees the runtime and current route's data
	// are always in place before any page content — and therefore any
	// script it contains — is parsed, on both full loads and swaps alike.
	html := buf.String()
	injection := breezeDataScript(dataJSON) + breezeTemplateScript(tmplSources) +
		te.breezeI18nScript(locale) + breezeRuntime()
	if loc := bodyOpenTag.FindStringIndex(html); loc != nil {
		buf.Reset()
		buf.WriteString(html[:loc[1]])
		buf.WriteString(injection)
		buf.WriteString(html[loc[1]:])
	} else {
		// No <body> tag found — fall back to prepending at the very start
		// so the runtime still loads before anything else has a chance to
		// reference it.
		buf.Reset()
		buf.WriteString(injection)
		buf.WriteString(html)
	}

	return nil
}

// marshalPageData safely serializes page data to JSON for client access.
func marshalPageData(data any) ([]byte, error) {
	if data == nil {
		return []byte("{}"), nil
	}
	b, err := json.Marshal(data)
	if err != nil {
		return []byte("{}"), err
	}
	return b, nil
}

// ─── Client-side template embedding ──────────────────────────────────────────

// collectTemplateSources reads the raw source of a view and all component files,
// strips the {{define "name"}}...{{end}} wrapper, and returns a map of
// template-name → inner source string. This map is embedded in the page so the
// client can re-render without an extra round-trip to the server.
func (te *TemplateEngine) collectTemplateSources(viewName string) map[string]string {
	sources := make(map[string]string)

	// ── View content block ────────────────────────────────────────────────
	viewPath := filepath.Join(te.viewsDir, viewName+".html")
	if raw, err := os.ReadFile(viewPath); err == nil {
		sources[viewName] = stripDefine(string(raw))
	}

	// ── All component files ───────────────────────────────────────────────
	compFiles, _ := filepath.Glob(filepath.Join(te.compDir, "*.html"))
	for _, cf := range compFiles {
		name := strings.TrimSuffix(filepath.Base(cf), ".html")
		if raw, err := os.ReadFile(cf); err == nil {
			sources[name] = stripDefine(string(raw))
		}
	}

	return sources
}

// stripDefine removes the outer {{define "name"}} ... {{end}} wrapper from a
// template file, leaving only the inner content string.
// If no define wrapper is present the whole source is returned unchanged.
func stripDefine(src string) string {
	s := strings.TrimSpace(src)

	// Match  {{define "..."}}  or  {{define `...`}}  at the start.
	for _, quote := range []string{`"`, "`"} {
		prefix := `{{define ` + quote
		if strings.HasPrefix(s, prefix) {
			closeQuote := strings.Index(s[len(prefix):], quote)
			if closeQuote < 0 {
				continue
			}
			afterName := s[len(prefix)+closeQuote+1:]
			// skip optional whitespace then "}}"
			afterName = strings.TrimLeft(afterName, " \t")
			if strings.HasPrefix(afterName, "}}") {
				inner := afterName[2:]
				// strip trailing {{end}}
				if idx := strings.LastIndex(inner, "{{end}}"); idx >= 0 {
					inner = inner[:idx]
				}
				return strings.TrimSpace(inner)
			}
		}
	}
	return s
}

// breezeI18nScript serializes the active locale's flattened dictionary inside
// a non-executing script tag so the client-side template evaluator can resolve
// {{t "key"}} tags during breeze.render()/setData() re-renders. Only the
// active locale ships to the client. Returns "" when i18n is not enabled.
func (te *TemplateEngine) breezeI18nScript(locale string) string {
	if te.i18n == nil || locale == "" {
		return ""
	}
	dict := te.i18n.Dict(locale)
	b, err := json.Marshal(dict)
	if err != nil {
		b = []byte("{}")
	}
	return `<script id="__breeze_i18n__" type="application/json" data-locale="` +
		template.HTMLEscapeString(locale) + `">` +
		string(b) +
		`</script>` + "\n"
}

// breezeTemplateScript serializes the template-sources map as JSON inside a
// non-executing script tag so the client can read it with breeze._tmpl(name).
func breezeTemplateScript(sources map[string]string) string {
	b, err := json.Marshal(sources)
	if err != nil {
		b = []byte("{}")
	}
	return `<script id="__breeze_tmpl__" type="application/json">` +
		string(b) +
		`</script>` + "\n"
}

// ─── TemplateData ─────────────────────────────────────────────────────────────

// TemplateData wraps the user's data and exposes helpers inside templates.
type TemplateData struct {
	Data any
	// Locale is the request locale resolved by the locale middleware,
	// e.g. for <html lang="{{.Locale}}">. Empty when i18n is not enabled.
	Locale  string
	engine  *TemplateEngine
	partial bool
}

// IsPartial returns true when the current render is a SPA partial request.
func (td *TemplateData) IsPartial() bool { return td.partial }

// ─── Re-render endpoint ───────────────────────────────────────────────────────

// reRenderRequest is the JSON body sent by breeze.render() on the client.
type reRenderRequest struct {
	View      string `json:"view"`      // render a full view's content block
	Component string `json:"component"` // render a single component
	Data      any    `json:"data"`      // arbitrary data passed to the template
}

// RenderJSON renders a view or component from a JSON request body.
// It is used by the /breeze/render endpoint registered by EnableReRender.
// The caller decides which wins: Component takes precedence over View.
func (te *TemplateEngine) RenderJSON(ctx *Context) {
	var req reRenderRequest
	if err := json.Unmarshal(ctx.Req.Body, &req); err != nil {
		ctx.Status(400)
		ctx.WriteString("breeze/render: invalid JSON body: " + err.Error())
		return
	}

	var buf bytes.Buffer
	locale := te.requestLocale(ctx)

	if req.Component != "" {
		// Component render — bare fragment, no layout.
		if err := te.renderComponent(req.Component, locale, req.Data, &buf); err != nil {
			ctx.Status(500)
			ctx.WriteString(fmt.Sprintf("breeze/render: component %q: %v", req.Component, err))
			return
		}
		ctx.HTML(buf.Bytes())
		return
	}

	if req.View != "" {
		// View render — returns only the content block (always partial).
		t, err := te.getView(req.View, locale)
		if err != nil {
			ctx.Status(500)
			ctx.WriteString(fmt.Sprintf("breeze/render: view %q: %v", req.View, err))
			return
		}
		if err := te.execView(t, req.View, locale, req.Data, true /* always partial */, &buf); err != nil {
			ctx.Status(500)
			ctx.WriteString(fmt.Sprintf("breeze/render: view %q exec: %v", req.View, err))
			return
		}
		ctx.HTML(buf.Bytes())
		return
	}

	ctx.Status(400)
	ctx.WriteString(`breeze/render: request must include "view" or "component"`)
}

// ─── Router integration ───────────────────────────────────────────────────────

// View registers a GET route that renders the named view template.
//
// Usage:
//
//	engine := breeze.NewTemplateEngine(breeze.TemplateConfig{})
//	router.View("/", engine, "home", nil)
//	router.View("/about", engine, "about", func(ctx *breeze.Context) any {
//	    return map[string]any{"title": "About Us"}
//	})
func (r *Router) View(
	pattern string,
	engine *TemplateEngine,
	viewName string,
	dataFn func(*Context) any,
) {
	r.Handle(GET, pattern, func(ctx *Context) {
		var data any
		if dataFn != nil {
			data = dataFn(ctx)
		}
		engine.RenderView(ctx, viewName, data)
	})
}

// ─── Context helper ───────────────────────────────────────────────────────────

// Render renders a view template with the given data and writes it to the response.
//
// Usage inside a handler:
//
//	ctx.Render(engine, "home", map[string]any{"title": "Home"})
func (ctx *Context) Render(engine *TemplateEngine, viewName string, data any) {
	engine.RenderView(ctx, viewName, data)
}

// ─── Server-side fragment helpers ────────────────────────────────────────────

// RenderComponent renders a single component as a bare HTML fragment.
// No layout, no SPA script injection. Designed for endpoints that feed
// breeze.fetch() / breeze.poll() on the client.
func (te *TemplateEngine) RenderComponent(ctx *Context, componentName string, data any) {
	var buf bytes.Buffer
	if err := te.renderComponent(componentName, te.requestLocale(ctx), data, &buf); err != nil {
		ctx.Status(500)
		ctx.WriteString(fmt.Sprintf("component error: %v", err))
		return
	}
	ctx.HTML(buf.Bytes())
}

// Fragment registers a GET route that returns a bare HTML fragment — either a
// component or a view's content block — with no layout and no SPA script.
// These endpoints are meant to be consumed by breeze.fetch() / breeze.poll().
//
// Usage:
//
//	// serve a component fragment at /fragments/stats
//	router.Fragment("/fragments/stats", engine, "stats", func(ctx *breeze.Context) any {
//	    return map[string]any{"count": getCount()}
//	})
//
// Then in any template:
//
//	<div id="stats-box"></div>
//	<script>
//	  breeze.poll('/fragments/stats', '#stats-box', 3000)
//	</script>
func (r *Router) Fragment(
	pattern string,
	engine *TemplateEngine,
	componentName string,
	dataFn func(*Context) any,
) {
	r.Handle(GET, pattern, func(ctx *Context) {
		var data any
		if dataFn != nil {
			data = dataFn(ctx)
		}
		engine.RenderComponent(ctx, componentName, data)
	})
}

// EnableReRender registers the built-in POST /breeze/render endpoint.
//
// Call this once at startup (after creating the engine). The client-side
// breeze.render() and breeze.setData() functions use this endpoint to
// re-render any view or component with new data and swap it into the DOM.
//
// Usage:
//
//	engine := breeze.NewTemplateEngine(breeze.TemplateConfig{...})
//	router.EnableReRender(engine)
func (r *Router) EnableReRender(engine *TemplateEngine) {
	r.Handle(POST, "/breeze/render", func(ctx *Context) {
		engine.RenderJSON(ctx)
	})
}

// ─── SPA Runtime ──────────────────────────────────────────────────────────────

// breezeRuntime returns the client-side JavaScript that enables:
//
//  1. Smart script execution — external scripts load once; inline scripts honour
//     data-spa-run="always"|"once"|default(never re-run); modules are preserved.
//  2. SPA navigation — intercepts <a> clicks, fetches partials, swaps #breeze-app.
//  3. SPA form handling — intercepts <form> submits (GET serialises query string;
//     POST uses fetch). Skips target="_blank", multipart, data-spa="false", external.
//  4. Lifecycle hooks — Breeze.onBeforeNavigate / onAfterNavigate /
//     onBeforeSubmit / onAfterSubmit.
//  5. Loading state — adds/removes body.breeze-loading during navigation & submit.
//  6. Error handling — fetch failures fall back to normal browser navigation/submit.
//  7. breeze.fetch / poll / stop / swap / data / setData / render / watch / ws.
func breezeRuntime() string {
	return `
<script id="__breeze_spa__">
(function () {
  'use strict';

  // ── Lifecycle hooks ────────────────────────────────────────────────────
  //
  // Breeze.onBeforeNavigate(fn) / onAfterNavigate(fn)
  // Breeze.onBeforeSubmit(fn)   / onAfterSubmit(fn)
  //
  // fn receives a detail object:
  //   onBeforeNavigate({ url })          — return false to cancel
  //   onAfterNavigate({ url })
  //   onBeforeSubmit({ form, url })      — return false to cancel
  //   onAfterSubmit({ form, url, html })
  //
  // Multiple handlers can be registered; they run in registration order.

  var _hooks = {
    beforeNavigate: [],
    afterNavigate:  [],
    beforeSubmit:   [],
    afterSubmit:    [],
  };

  function _runHooks(name, detail) {
    var list = _hooks[name];
    for (var i = 0; i < list.length; i++) {
      if (list[i](detail) === false) return false;
    }
    return true;
  }

  // ── Loading state ──────────────────────────────────────────────────────

  function _loadingStart() { document.body.classList.add('breeze-loading'); }
  function _loadingEnd()   { document.body.classList.remove('breeze-loading'); }

  // ── Smart script execution ─────────────────────────────────────────────
  //
  // Rules (applied inside swap() after every fragment insert):
  //
  //   External scripts (src="..."):
  //     Execute only once. A module-level Set tracks loaded URLs.
  //     If already loaded, the <script> element is removed so it doesn't
  //     appear in the DOM twice but its side-effects are not repeated.
  //
  //   Inline scripts (no src):
  //     data-spa-run="always" → execute on every swap (the explicit opt-in).
  //     data-spa-run="once"   → execute once per page lifecycle (tracked by Set).
  //     no attribute          → never re-execute after the initial page load.
  //     The attribute is checked case-insensitively.
  //
  //   Module scripts (type="module"):
  //     Always external-style de-duplication for src modules.
  //     Inline modules execute only on initial page load (browser de-dupes them
  //     anyway for the src case; we honour the same for inline).
  //
  //   Execution order:
  //     Scripts are processed in document order (forEach preserves DOM order).
  //     Each eligible script is cloned and appended; the old node is removed.

  // URLs of external scripts already executed (survives across navigations).
  var _loadedScripts = new Set();
  // IDs/src of inline once-scripts already run (keyed by data-spa-id or content hash).
  var _onceScripts   = new Set();

  // Simple djb2-style hash for content-keying inline once-scripts.
  function _hashStr(s) {
    var h = 5381;
    for (var i = 0; i < s.length; i++) h = (h * 33) ^ s.charCodeAt(i);
    return (h >>> 0).toString(36);
  }

  // Execute scripts inside el after a swap. el is the element whose innerHTML
  // was just replaced. Scripts already in the document head are not affected.
  function _runScripts(el) {
    var scripts = el.querySelectorAll('script');
    scripts.forEach(function (old) {
      var src     = old.getAttribute('src');
      var type    = (old.getAttribute('type') || '').toLowerCase().trim();
      var spaRun  = (old.getAttribute('data-spa-run') || '').toLowerCase().trim();
      var isModule = type === 'module';

      // ── External script ──────────────────────────────────────────────
      if (src) {
        var key = src;
        if (_loadedScripts.has(key)) {
          // Already loaded — remove the stale node, do not re-execute.
          old.parentNode && old.parentNode.removeChild(old);
          return;
        }
        _loadedScripts.add(key);
        var s = document.createElement('script');
        Array.from(old.attributes).forEach(function (a) { s.setAttribute(a.name, a.value); });
        old.parentNode.replaceChild(s, old);
        return;
      }

      // ── Inline script ────────────────────────────────────────────────
      var content = old.textContent || '';

      if (spaRun === 'always') {
        // Explicit always — execute unconditionally.
        var s = document.createElement('script');
        Array.from(old.attributes).forEach(function (a) { s.setAttribute(a.name, a.value); });
        s.textContent = content;
        old.parentNode.replaceChild(s, old);
        return;
      }

      if (spaRun === 'once') {
        var key = old.getAttribute('data-spa-id') || _hashStr(content);
        if (_onceScripts.has(key)) {
          old.parentNode && old.parentNode.removeChild(old);
          return;
        }
        _onceScripts.add(key);
        var s = document.createElement('script');
        Array.from(old.attributes).forEach(function (a) { s.setAttribute(a.name, a.value); });
        s.textContent = content;
        old.parentNode.replaceChild(s, old);
        return;
      }

      // No data-spa-run attribute (including inline modules):
      // Remove from DOM; do not execute. The initial page-load already ran it.
      old.parentNode && old.parentNode.removeChild(old);
    });
  }

  // ── Utilities ──────────────────────────────────────────────────────────

  function resolveEl(target) {
    if (!target) return document.getElementById('breeze-app') ||
                         document.querySelector('main') ||
                         document.body;
    if (typeof target === 'string') return document.querySelector(target);
    return target;
  }

  // Swap innerHTML of el, then run scripts with smart de-duplication.
  //
  // _refreshStateTags runs FIRST, before _runScripts, for two reasons:
  //   1. It relocates any __breeze_data__/__breeze_tmpl__/__breeze_i18n__
  //      tags found in the fragment out to <body>, removing them from el's
  //      subtree. _runScripts treats any <script> without a data-spa-run
  //      attribute as a stale inline script and deletes it — running it
  //      first would strip these JSON tags before they could ever be read.
  //   2. Any content script in the fragment that calls breeze.* at its own
  //      top level (e.g. breeze.bind(...)) must see this route's data, not
  //      whatever the previously displayed route left behind.
  function swap(el, html) {
    el.innerHTML = html;
    _refreshStateTags(el);
    _runScripts(el);
  }

  // ── Core fetch helper ──────────────────────────────────────────────────

  async function breezeGet(url, target, options) {
    options = options || {};
    var el = resolveEl(target);

    try {
      var res = await fetch(url, {
        method:      options.method  || 'GET',
        body:        options.body    || undefined,
        credentials: 'same-origin',
        headers: Object.assign(
          { 'X-Breeze-Fragment': 'true' },
          options.headers || {}
        ),
      });

      var html = await res.text();

      if (!res.ok) {
        if (options.onError) { options.onError(res, el); }
        return html;
      }

      if (el) { swap(el, html); }
      if (options.onSuccess) { options.onSuccess(html, el); }

      window.dispatchEvent(new CustomEvent('breeze:update', {
        detail: { url: url, target: target, html: html }
      }));

      return html;
    } catch (e) {
      if (options.onError) { options.onError(e, el); }
      throw e;
    }
  }

  // ── Polling ────────────────────────────────────────────────────────────

  var _polls = new WeakMap();

  function breezePoll(url, target, intervalMs, options) {
    var el = resolveEl(target);
    if (!el) { console.warn('breeze.poll: target not found', target); return; }
    breezeStop(el);
    breezeGet(url, el, options);
    var id = setInterval(function () { breezeGet(url, el, options); }, intervalMs || 5000);
    _polls.set(el, id);
  }

  function breezeStop(target) {
    var el = resolveEl(target);
    if (!el) return;
    var id = _polls.get(el);
    if (id !== undefined) { clearInterval(id); _polls.delete(el); }
  }

  // ── SPA navigation ─────────────────────────────────────────────────────

  // Take manual control of scroll restoration. The browser's native
  // restoration runs synchronously on popstate — before our async fragment
  // fetch resolves — which would restore scroll against stale content. We
  // restore scroll ourselves once the new content is in place instead.
  if ('scrollRestoration' in history) {
    try { history.scrollRestoration = 'manual'; } catch (e) {}
  }

  function getAppTarget() {
    return document.getElementById('breeze-app') ||
           document.querySelector('main') ||
           document.body;
  }

  // Locale this page was rendered with ('' when i18n is not enabled).
  // _pageLocale is hoisted; the i18n tag is injected before this script.
  var _pageLang = _pageLocale();

  // Persists the current scroll offset onto the *current* history entry so
  // it can be restored later if the user navigates back/forward to it.
  // Called continuously (debounced) while scrolling, and once more right
  // before any programmatic navigation, so the saved value is always fresh
  // regardless of which path the user takes away from the page.
  function _saveScroll() {
    try {
      var s = (history.state && typeof history.state === 'object') ? history.state : {};
      history.replaceState(
        Object.assign({}, s, { scrollY: window.scrollY }),
        '',
        window.location.pathname + window.location.search
      );
    } catch (e) {}
  }

  var _scrollSaveTimer = null;
  window.addEventListener('scroll', function () {
    if (_scrollSaveTimer) return;
    _scrollSaveTimer = setTimeout(function () {
      _scrollSaveTimer = null;
      _saveScroll();
    }, 150);
  }, { passive: true });

  // Caches successful partial-fragment responses by normalized URL so that
  // revisiting an already-fetched route (e.g. A -> B -> A, or Back/Forward)
  // reuses the cached HTML instead of re-fetching. Failed responses are
  // never cached. In-memory only, capped to avoid unbounded growth on
  // long-lived sessions.
  var _routeCache    = new Map();
  var _routeCacheMax = 50;

  function _normalizeUrl(url) {
    try {
      var u = new URL(url, window.location.origin);
      return u.pathname + u.search;
    } catch (e) {
      return url;
    }
  }

  function _cacheRoute(key, html) {
    if (_routeCache.has(key)) _routeCache.delete(key); // refresh recency
    _routeCache.set(key, html);
    if (_routeCache.size > _routeCacheMax) {
      _routeCache.delete(_routeCache.keys().next().value); // evict oldest
    }
  }

  // Invalidation helpers. Wired into breeze.setData()/the reactive store and
  // into non-GET SPA form submissions (see below), so that any client- or
  // server-side state mutation forces the *next* visit to a route — via
  // link, back/forward, or history restore — to re-fetch instead of
  // replaying a pre-mutation snapshot from _routeCache. This is the fix for
  // "go back and the page shows stale/broken content": the cache had no
  // eviction path tied to writes, so it kept serving fragments that no
  // longer matched server (or client store) state.
  function _invalidateRoute(url) {
    _routeCache.delete(_normalizeUrl(url));
  }

  function _invalidateCache() {
    _routeCache.clear();
  }

  // Cache policy. A fragment is stored only when the server has not marked
  // it as private or uncacheable. This follows HTTP semantics rather than
  // guessing: Cache-Control: no-store | no-cache | private means the body
  // is tied to one user or one moment, so replaying it later — after a
  // logout, or under a different session — would show one user content
  // rendered for another. Responses that say nothing about caching keep
  // the previous behaviour and are cached, so public routes are unaffected.
  function _isCacheable(res) {
    var cc = (res.headers.get('Cache-Control') || '').toLowerCase();
    if (cc === '') return true;
    return cc.indexOf('no-store') === -1 &&
           cc.indexOf('no-cache') === -1 &&
           cc.indexOf('private')  === -1;
  }

  // Authentication epoch. A server may stamp responses with
  //   X-Breeze-Auth: <opaque identity>
  // carrying a user id, session id, or hash of either. Any change in that
  // value is an authentication state transition — login, logout, or a
  // switch of user — and every fragment cached for the previous identity
  // is dropped at once, because those fragments were rendered for someone
  // who is no longer the one looking at the screen. Responses that omit
  // the header leave the epoch untouched.
  var _authEpoch = null;

  function _syncAuthEpoch(res) {
    var id = res.headers.get('X-Breeze-Auth');
    if (id === null) return;
    if (_authEpoch !== null && _authEpoch !== id) _invalidateCache();
    _authEpoch = id;
  }


  // Monotonic sequence guard: if a newer navigation starts while an older
  // one is still in flight, the older response is discarded on arrival so
  // out-of-order fetches can never clobber newer content (duplicate/stale
  // rendering) or push a stale history entry. Paired with an AbortController
  // so the superseded request is actually cancelled, not just ignored.
  var _navSeq       = 0;
  var _navController = null;

  async function navigate(url, push) {
    // Before hook — returning false cancels navigation.
    if (_runHooks('beforeNavigate', { url: url }) === false) return;

    // Cancel any navigation request still in flight — its response would
    // be discarded anyway, so stop wasting bandwidth/CPU on it.
    if (_navController) _navController.abort();
    var controller = new AbortController();
    _navController = controller;

    // "once" inline scripts (data-spa-run="once") are scoped to a single
    // navigation/mount, not to the whole page lifetime: every real
    // navigation tears down and rebuilds the DOM via innerHTML, so any
    // listeners a once-script attached directly to elements (rather than
    // via delegation) are gone and must be reattached when the view is
    // (re)entered — including via the back/forward button. Clearing here
    // (and not inside breezeGet/poll/_rerender) means frequent polling or
    // partial re-renders still only run a once-script a single time.
    _onceScripts.clear();

    var seq = ++_navSeq;
    var key = _normalizeUrl(url);
    _loadingStart();
    try {
      var html = _routeCache.get(key);

      if (html === undefined) {
        var res = await fetch(url, {
          headers: { 'X-Breeze-Partial': 'true' },
          credentials: 'same-origin',
          signal: controller.signal,
        });

        if (!res.ok) { window.location.href = url; return; }

        // An identity change invalidates everything cached for whoever
        // was signed in before, and must be applied before this response
        // is itself considered for caching.
        _syncAuthEpoch(res);

        // A response in a different language than the page was rendered
        // with means the locale changed (e.g. a ?lang= switch). Fall back
        // to a full page load so the embedded i18n dictionary, template
        // sources, and route cache are all rebuilt for the new locale.
        var lang = res.headers.get('Content-Language') || '';
        if (lang && _pageLang && lang !== _pageLang) {
          window.location.href = url;
          return;
        }

        html = await res.text();
        if (_isCacheable(res)) _cacheRoute(key, html);
      }


      // A newer navigation started while this fetch was in flight — drop
      // this stale response rather than render/push it out of order.
      if (seq !== _navSeq) return;

      var target = getAppTarget();
      swap(target, html);

      if (push) { history.pushState({ breezeUrl: url, scrollY: 0 }, '', url); }

      window.dispatchEvent(new CustomEvent('breeze:navigate', { detail: { url: url } }));

      // Restore/reset scroll only after the browser has finished laying out
      // the newly swapped DOM (next frame), so we don't scroll against
      // stale geometry from before the swap.
      requestAnimationFrame(function () {
        if (push) {
          window.scrollTo(0, 0);
        } else {
          var restoreY = (history.state && typeof history.state.scrollY === 'number')
            ? history.state.scrollY : 0;
          window.scrollTo(0, restoreY);
        }
      });

      _runHooks('afterNavigate', { url: url });
    } catch (e) {
      // A superseded navigation's request was aborted on purpose — that is
      // not a network failure, so it must never trigger the error fallback.
      if (e && e.name === 'AbortError') return;
      window.location.href = url;
    } finally {
      if (_navController === controller) _navController = null;
      if (seq === _navSeq) _loadingEnd();
    }
  }

  document.addEventListener('click', function (e) {
    if (!e.target || e.target.nodeType !== 1) return;
    var a = e.target.closest('a[href]');
    if (!a) return;

    var href = a.getAttribute('href');
    if (!href) return;
    if (
      a.hasAttribute('data-no-spa') || a.hasAttribute('target') ||
      href.startsWith('http') || href.startsWith('//') ||
      href.startsWith('#')    || href.startsWith('mailto:') ||
      href.startsWith('tel:')
    ) return;

    e.preventDefault();
    var targetUrl   = new URL(href, window.location.origin);
    var targetFull  = targetUrl.pathname + targetUrl.search;
    var currentFull = window.location.pathname + window.location.search;
    // Skip only when the destination is truly identical (path AND query) —
    // comparing pathname alone let same-path-same-query clicks through and
    // push a duplicate history entry for the URL already on screen.
    if (targetFull === currentFull) return;
    _saveScroll();
    navigate(href, true);
  });

  window.addEventListener('popstate', function () {
    navigate(window.location.pathname + window.location.search, false);
  });

  if (!history.state || !history.state.breezeUrl) {
    history.replaceState(
      { breezeUrl: window.location.pathname + window.location.search, scrollY: window.scrollY },
      '',
      window.location.pathname + window.location.search
    );
  }

  // ── SPA form handling ──────────────────────────────────────────────────
  //
  // Intercepts form submits unless:
  //   - target="_blank"
  //   - enctype="multipart/form-data" (file uploads — let browser handle)
  //   - data-spa="false" (explicit opt-out)
  //   - action is an external URL (different origin)
  //   - the form has a [download] attribute (unusual but guard it)
  //
  // GET forms:
  //   Serialise with URLSearchParams, navigate via SPA router.
  //
  // POST forms:
  //   Use fetch(). Content-Type negotiation:
  //     application/x-www-form-urlencoded (default)
  //     application/json (if form has data-content-type="application/json")
  //     text/plain       (if form has data-content-type="text/plain")
  //   Response HTML is swapped into #breeze-app exactly like a navigation.
  //   History is pushed with the form's action URL.
  //
  // Progressive enhancement: if JS is disabled the form submits normally.

  document.addEventListener('submit', function (e) {
    var form = e.target;
    if (!form || form.tagName !== 'FORM') return;

    // ── Opt-out conditions ───────────────────────────────────────────────
    if (form.getAttribute('data-spa') === 'false') return;
    if (form.getAttribute('target') === '_blank')  return;
    if ((form.getAttribute('enctype') || '').toLowerCase() === 'multipart/form-data') return;

    var rawAction = form.getAttribute('action') || window.location.pathname;
    // External action → let browser handle.
    try {
      var actionUrl = new URL(rawAction, window.location.origin);
      if (actionUrl.origin !== window.location.origin) return;
    } catch (_) { return; }

    e.preventDefault();

    var method = (form.getAttribute('method') || 'GET').toUpperCase();
    var data   = new FormData(form);

    // Before hook — returning false cancels submission.
    if (_runHooks('beforeSubmit', { form: form, url: rawAction }) === false) return;

    _loadingStart();

    if (method === 'GET') {
      // GET: serialise to query string and navigate.
      var params = new URLSearchParams();
      data.forEach(function (val, key) { params.append(key, val); });
      var qs  = params.toString();
      var url = actionUrl.pathname + (qs ? '?' + qs : '') + actionUrl.hash;
      _saveScroll();
      navigate(url, true).finally(_loadingEnd);
      return;
    }

    // POST (and PUT/PATCH/DELETE via method override if present).
    var contentType = (form.getAttribute('data-content-type') || '').toLowerCase().trim();
    var body, ct;

    if (contentType === 'application/json') {
      // Convert FormData to a plain object for JSON serialisation.
      var obj = {};
      data.forEach(function (val, key) {
        if (Object.prototype.hasOwnProperty.call(obj, key)) {
          if (!Array.isArray(obj[key])) obj[key] = [obj[key]];
          obj[key].push(val);
        } else {
          obj[key] = val;
        }
      });
      body = JSON.stringify(obj);
      ct   = 'application/json';
    } else if (contentType === 'text/plain') {
      var lines = [];
      data.forEach(function (val, key) { lines.push(key + '=' + val); });
      body = lines.join('\n');
      ct   = 'text/plain';
    } else {
      // Default: application/x-www-form-urlencoded.
      body = new URLSearchParams(data).toString();
      ct   = 'application/x-www-form-urlencoded';
    }

    // The action query string is part of the endpoint, not decoration:
    // POST /items?list=2 addresses a different resource than POST /items,
    // and dropping it sent every such form to the wrong place. Only the
    // fragment is left off the request, since it is never sent to a server;
    // it is kept for the URL the user ends up looking at.
    var actionPath = actionUrl.pathname + actionUrl.search;
    var actionHref = actionPath + actionUrl.hash;

    _saveScroll();

    fetch(actionPath, {
      method:      method,
      credentials: 'same-origin',
      headers: {
        'Content-Type':     ct,
        'X-Breeze-Partial': 'true',
      },
      body: body,
    }).then(function (res) {
      return res.text().then(function (html) {
        if (!res.ok) {
          // Server error — fall back to a real navigation so the user sees it.
          window.location.href = actionHref;
          return;
        }
        var target = getAppTarget();
        // A non-GET submit is inherently a mutation — any other cached
        // route may now be stale (e.g. a list page this POST just added
        // a row to), so drop the whole cache rather than guess which
        // entries are affected. The response we just got is re-cached
        // fresh immediately after.
        _invalidateCache();
        _syncAuthEpoch(res);
        swap(target, html);
        if (_isCacheable(res)) _cacheRoute(_normalizeUrl(actionPath), html);
        history.pushState({ breezeUrl: actionHref, scrollY: 0 }, '', actionHref);
        window.scrollTo(0, 0);
        window.dispatchEvent(new CustomEvent('breeze:navigate', { detail: { url: actionPath } }));
        _runHooks('afterSubmit', { form: form, url: actionPath, html: html });
      });
    }).catch(function () {
      // Network failure — fall back to normal browser submit.
      // Taken from the prototype: a control named or id'd "submit" shadows
      // form.submit with the element itself, so calling it directly throws
      // instead of submitting, losing the user input at the exact moment
      // this fallback exists to save it.
      HTMLFormElement.prototype.submit.call(form);
    }).finally(function () {
      _loadingEnd();
    });
  });

  // ── Reactive data store ────────────────────────────────────────────────
  //
  // breeze.data() used to be a one-shot read of the page's embedded JSON:
  // fine for rendering the initial view, but mutating an array or object
  // returned from it (push/splice/property assignment/delete) was
  // invisible to the framework — nothing re-rendered and nothing
  // invalidated the cached route, so the UI silently drifted out of sync
  // with the data. _store is now wrapped in a Proxy so that any mutation,
  // however it's made, is observable: it notifies breeze.watch()
  // callbacks, re-renders any region registered with breeze.bind(), and
  // invalidates the current route's cache entry (see _onStoreChange).

  var _store       = null;
  var _watchers    = [];
  var _bindings    = []; // { target, name, key } registered via breeze.bind()
  var _renderTimer = null;

  var _mutatingArrayMethods = ['push', 'pop', 'shift', 'unshift', 'splice', 'sort', 'reverse', 'copyWithin', 'fill'];

  // Wraps an object/array in a Proxy that calls onChange after any
  // mutation: direct property/index writes, deletes, or array mutator
  // methods. Nested objects/arrays are wrapped lazily on read, so the
  // whole tree stays reactive without an eager deep walk.
  //
  // One Proxy per underlying object, remembered in a WeakMap. Nested values
  // are wrapped on read, and a template that walks the same branch a
  // thousand times used to mint a thousand throwaway Proxies for the same
  // object. Reusing the wrapper makes reads allocation-free after the first
  // and — just as importantly — keeps identity stable, so data.user ===
  // data.user holds and code that compares or keys off a nested object
  // behaves. The map is weak, so a value dropped from the store is still
  // collectable along with its wrapper.
  var _proxyCache = new WeakMap();

  function _makeReactive(value, onChange) {
    if (value === null || typeof value !== 'object' || value.__breezeReactive__) return value;

    // Cached per (object, onChange): a wrapper carries its callback, so a
    // second store with a different callback must not reuse the first's.
    var hit = _proxyCache.get(value);
    if (hit !== undefined && hit.fn === onChange) return hit.proxy;

    var proxy = new Proxy(value, {
      get: function (target, prop, receiver) {
        if (prop === '__breezeReactive__') return true;
        var val = Reflect.get(target, prop, receiver);
        if (Array.isArray(target) && typeof val === 'function' && _mutatingArrayMethods.indexOf(prop) !== -1) {
          return function () {
            var result = val.apply(target, arguments);
            onChange();
            return result;
          };
        }
        return (val !== null && typeof val === 'object') ? _makeReactive(val, onChange) : val;
      },
      set: function (target, prop, val, receiver) {
        // Assigning the value a property already holds is not a change.
        // Firing onChange for it would schedule a render whose output is
        // byte-for-byte what is already on screen — and for a server-side
        // template, a network round-trip to produce it. Object.is is the
        // right comparison: NaN equals itself, and 0 differs from -0.
        var had  = Object.prototype.hasOwnProperty.call(target, prop);
        var prev = had ? target[prop] : undefined;
        var ok   = Reflect.set(target, prop, val, receiver);
        if (ok && (!had || !Object.is(prev, val))) onChange();
        return ok;
      },
      deleteProperty: function (target, prop) {
        // Deleting an absent property changes nothing either.
        var had = Object.prototype.hasOwnProperty.call(target, prop);
        var ok  = Reflect.deleteProperty(target, prop);
        if (ok && had) onChange();
        return ok;
      },
    });

    _proxyCache.set(value, { fn: onChange, proxy: proxy });
    return proxy;
  }


  // Debounced (via setTimeout 0) so several synchronous mutations in the
  // same tick — e.g. a loop of list.add() calls — collapse into a single
  // re-render/watcher pass instead of one per mutation.
  function _onStoreChange() {
    if (_renderTimer) return;
    _renderTimer = setTimeout(function () {
      _renderTimer = null;
      _invalidateRoute(window.location.pathname + window.location.search);
      _notifyWatchers(_store);
      _renderBindings();
    }, 0);
  }

  // Set for the duration of one binding pass. Bindings asking for the same
  // template with the same data share a single server render through it;
  // see _rerender. Null outside a pass, so a direct breeze.render() call is
  // never folded into anything else.
  var _renderBatch = null;

  function _renderBindings() {
    var outer = _renderBatch;
    _renderBatch = new Map();
    try {
      for (var i = 0; i < _bindings.length; i++) {
        var b = _bindings[i];
        var data = b.key ? (_store ? _store[b.key] : undefined) : _store;
        _rerender(b.name, data, b.target);
      }
    } finally {
      _renderBatch = outer;
    }
  }

  function _readDataTag() {
    var el = document.getElementById('__breeze_data__');
    if (!el) return {};
    try { return JSON.parse(el.textContent); } catch(e) { return {}; }
  }

  function _notifyWatchers(data) {
    for (var i = 0; i < _watchers.length; i++) {
      try { _watchers[i](data); } catch(e) { console.error('breeze.watch callback error', e); }
    }
  }

  // Called by swap() after every fragment insert (navigation, breeze.fetch,
  // poll, render, form submit). If the fragment carries fresh
  // __breeze_data__ / __breeze_i18n__ / __breeze_tmpl__ tags — every view
  // now embeds these, see execView on the server — the corresponding
  // client-side caches are dropped and the store is rehydrated from the
  // new tag. Previously these tags were only ever injected into full page
  // loads and lived outside #breeze-app, so breeze.data() silently froze
  // at whatever the very first page load contained and never reflected
  // the route actually on screen after an SPA navigation.
  function _refreshStateTags(el) {
    var sawData = false;
    ['__breeze_data__', '__breeze_i18n__', '__breeze_tmpl__'].forEach(function (id) {
      var fresh = el.querySelector('#' + id);
      if (!fresh) return;
      var existing = document.getElementById(id);
      if (existing && existing !== fresh) existing.parentNode.removeChild(existing);
      // Relocate to <body> so exactly one canonical instance exists and it
      // survives later swaps that don't happen to touch this tag.
      document.body.appendChild(fresh);
      if (id === '__breeze_data__') sawData = true;
    });

    _i18nCache = null;
    _tmplCache = null;

    if (sawData) {
      _store = _makeReactive(_readDataTag(), _onStoreChange);
      _notifyWatchers(_store);
      _renderBindings();
    }
  }

  // ── Client-side i18n ───────────────────────────────────────────────────
  //
  // The server injects the active locale's flattened dictionary as a
  // non-executing JSON tag (id __breeze_i18n__, data-locale attribute) so
  // the client-side evaluator can resolve {{t "key"}} during re-renders.

  var _i18nCache = null;

  function _i18nDict() {
    if (_i18nCache) return _i18nCache;
    var el = document.getElementById('__breeze_i18n__');
    if (!el) { _i18nCache = {}; return _i18nCache; }
    try { _i18nCache = JSON.parse(el.textContent); } catch(e) { _i18nCache = {}; }
    return _i18nCache;
  }

  function _pageLocale() {
    var el = document.getElementById('__breeze_i18n__');
    return el ? (el.getAttribute('data-locale') || '') : '';
  }

  // Tokenize the argument list of a t tag: quoted strings become literals,
  // everything else is an expression (number or dot-path).
  function _tTokens(s) {
    var tokens = [];
    var i = 0;
    while (i < s.length) {
      var c = s[i];
      if (c === ' ' || c === '\t') { i++; continue; }
      if (c === '"' || c === "'") {
        var end = s.indexOf(c, i + 1);
        if (end === -1) { tokens.push({ lit: s.slice(i + 1) }); break; }
        tokens.push({ lit: s.slice(i + 1, end) });
        i = end + 1;
      } else {
        var j = i;
        while (j < s.length && s[j] !== ' ' && s[j] !== '\t') j++;
        tokens.push({ expr: s.slice(i, j) });
        i = j;
      }
    }
    return tokens;
  }

  // Evaluate a {{t "key" ...args}} tag against the embedded dictionary,
  // mirroring the server-side semantics: "count" selects a zero/one/other
  // plural form, %{name} placeholders interpolate from the args, and a
  // missing key echoes the key itself.
  function _evalT(rest, ctx) {
    var tokens = _tTokens(rest);
    if (!tokens.length || tokens[0].lit === undefined) return '';
    var key = tokens[0].lit;
    var dict = _i18nDict();

    var args = {};
    for (var i = 1; i + 1 < tokens.length; i += 2) {
      var name = tokens[i].lit !== undefined ? tokens[i].lit : tokens[i].expr;
      var vt = tokens[i + 1];
      var argVal; // NOT named val — var hoists, and it must not shadow the lookup below
      if (vt.lit !== undefined) {
        argVal = vt.lit;
      } else if (vt.expr !== '' && !isNaN(Number(vt.expr))) {
        argVal = Number(vt.expr);
      } else {
        argVal = _resolvePath(vt.expr, ctx);
      }
      args[name] = argVal;
    }

    var val;
    if (Object.prototype.hasOwnProperty.call(args, 'count')) {
      var n = Number(args['count']);
      if (n === 0 && dict[key + '.zero'] !== undefined) val = dict[key + '.zero'];
      else if (n === 1 && dict[key + '.one'] !== undefined) val = dict[key + '.one'];
      else if (dict[key + '.other'] !== undefined) val = dict[key + '.other'];
    }
    if (val === undefined) val = dict[key];
    if (val === undefined) return key;

    return val.replace(/%\{([^}]+)\}/g, function (m, name) {
      return Object.prototype.hasOwnProperty.call(args, name) ? String(args[name]) : m;
    });
  }

  // ── Client-side Go-template evaluator ──────────────────────────────────

  var _tmplCache = null;

  function _tmplSources() {
    if (_tmplCache) return _tmplCache;
    var el = document.getElementById('__breeze_tmpl__');
    if (!el) { _tmplCache = {}; return _tmplCache; }
    try { _tmplCache = JSON.parse(el.textContent); } catch(e) { _tmplCache = {}; }
    return _tmplCache;
  }

  function _resolvePath(path, ctx) {
    var p = path.trim();
    if (p === '.' || p === '') return ctx;
    if (p[0] === '.') p = p.slice(1);
    var parts = p.split('.');
    var val = ctx;
    for (var i = 0; i < parts.length; i++) {
      if (val == null) return undefined;
      val = val[parts[i]];
    }
    return val;
  }

  function _evalTmpl(src, ctx, sources, depth) {
    depth = depth || 0;
    if (depth > 16) return '';

    var out = '';
    var pos = 0;

    while (pos < src.length) {
      var open = src.indexOf('{{', pos);
      if (open === -1) { out += src.slice(pos); break; }
      out += src.slice(pos, open);

      var close = src.indexOf('}}', open + 2);
      if (close === -1) { out += src.slice(open); break; }

      var tag = src.slice(open + 2, close).trim();
      pos = close + 2;

      if (tag.slice(0, 2) === '/*') continue;

      if (tag.slice(0, 5) === 'range') {
        var rangePath = tag.slice(5).trim();
        var block = _extractBlock(src, pos, 'range');
        pos = block.end;
        var items = _resolvePath(rangePath, ctx);
        if (Array.isArray(items)) {
          for (var ri = 0; ri < items.length; ri++) {
            out += _evalTmpl(block.body, items[ri], sources, depth + 1);
          }
        } else if (items && typeof items === 'object') {
          var keys = Object.keys(items);
          for (var ki = 0; ki < keys.length; ki++) {
            out += _evalTmpl(block.body, items[keys[ki]], sources, depth + 1);
          }
        }
        continue;
      }

      if (tag.slice(0, 2) === 'if') {
        var ifExpr = tag.slice(2).trim();
        var negate = false;
        if (ifExpr.slice(0, 3) === 'not') { negate = true; ifExpr = ifExpr.slice(3).trim(); }
        var block = _extractBlock(src, pos, 'if');
        pos = block.end;
        var val = _resolvePath(ifExpr, ctx);
        var truthy = !!(Array.isArray(val) ? val.length : val);
        if (negate) truthy = !truthy;
        if (truthy) out += _evalTmpl(block.body, ctx, sources, depth + 1);
        continue;
      }

      if (tag === 'end') continue;

      if (tag.slice(0, 2) === 't ') {
        out += _evalT(tag.slice(2), ctx);
        continue;
      }

      if (tag.slice(0, 9) === 'component' || tag.slice(0, 7) === 'partial') {
        var rest = tag.slice(tag[0] === 'c' ? 9 : 7).trim();
        var q = rest[0];
        if (q === '"' || q === "'") {
          var nameEnd  = rest.indexOf(q, 1);
          var compName = rest.slice(1, nameEnd);
          var dataExpr = rest.slice(nameEnd + 1).trim();
          var compData = dataExpr ? _resolvePath(dataExpr, ctx) : ctx;
          var compSrc  = sources[compName];
          if (compSrc !== undefined) {
            out += _evalTmpl(compSrc, compData, sources, depth + 1);
          }
        }
        continue;
      }

      if (tag[0] === '.') {
        var resolved = _resolvePath(tag, ctx);
        if (resolved != null) out += String(resolved);
        continue;
      }
    }

    return out;
  }

  function _extractBlock(src, pos, tag) {
    var depth = 1;
    var body  = '';
    var i     = pos;

    while (i < src.length) {
      var open  = src.indexOf('{{', i);
      if (open === -1) break;
      var close = src.indexOf('}}', open + 2);
      if (close === -1) break;

      var inner = src.slice(open + 2, close).trim();

      if (inner.slice(0, tag.length) === tag) {
        depth++;
        body += src.slice(i, close + 2);
        i = close + 2;
      } else if (inner === 'end') {
        depth--;
        if (depth === 0) { body += src.slice(i, open); return { body: body, end: close + 2 }; }
        body += src.slice(i, close + 2);
        i = close + 2;
      } else {
        body += src.slice(i, close + 2);
        i = close + 2;
      }
    }

    return { body: body, end: i };
  }

  function _rerender(name, data, target) {
    var el      = resolveEl(target);
    var sources = _tmplSources();

    if (sources[name] !== undefined) {
      var html = _evalTmpl(sources[name], data, sources, 0);
      if (el) { swap(el, html); }
      window.dispatchEvent(new CustomEvent('breeze:render', {
        detail: { name: name, target: target, html: html, local: true }
      }));
      return Promise.resolve(html);
    }

    function postRender(body) {
      return fetch('/breeze/render', {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
    }

    // Ten regions bound to one template and one object used to fire ten
    // identical POSTs for a single state change, each returning the same
    // HTML. Within a binding pass they share one request; the reply is
    // swapped into every target separately, so each still renders itself.
    var pending = null;
    if (_renderBatch !== null) {
      var hit = _renderBatch.get(name);
      if (hit !== undefined && hit.data === data) pending = hit.promise;
    }

    if (pending === null) {
      pending = postRender({ component: name, data: data })
        .then(function(res) {
          if (res.status === 400) return postRender({ view: name, data: data });
          return res;
        })
        .then(function(res) {
          return res.text().then(function(html) {
            return { ok: res.ok, html: html };
          });
        });
      if (_renderBatch !== null) _renderBatch.set(name, { data: data, promise: pending });
    }

    return pending.then(function(out) {
      if (!out.ok) { console.error('breeze.render error:', out.html); return out.html; }
      if (el) { swap(el, out.html); }
      window.dispatchEvent(new CustomEvent('breeze:render', {
        detail: { name: name, target: target, html: out.html, local: false }
      }));
      return out.html;
    });
  }

  // ── Public API ─────────────────────────────────────────────────────────

  window.Breeze = {
    // Lifecycle hooks — register before navigation/submit events fire.
    //
    // Breeze.onBeforeNavigate(fn({ url }))     — return false to cancel
    // Breeze.onAfterNavigate(fn({ url }))
    // Breeze.onBeforeSubmit(fn({ form, url })) — return false to cancel
    // Breeze.onAfterSubmit(fn({ form, url, html }))
    //
    // Example:
    //   Breeze.onBeforeNavigate(function(e) {
    //     console.log('navigating to', e.url);
    //   });
    onBeforeNavigate: function(fn) { _hooks.beforeNavigate.push(fn); },
    onAfterNavigate:  function(fn) { _hooks.afterNavigate.push(fn);  },
    onBeforeSubmit:   function(fn) { _hooks.beforeSubmit.push(fn);   },
    onAfterSubmit:    function(fn) { _hooks.afterSubmit.push(fn);    },
  };

  window.breeze = {
    fetch:    breezeGet,
    poll:     breezePoll,
    stop:     breezeStop,
    swap:     function(target, html) { swap(resolveEl(target), html); },
    navigate: function(url) { navigate(url, true); },

    data: function(key) {
      if (_store === null) _store = _makeReactive(_readDataTag(), _onStoreChange);
      return key ? (_store && typeof _store === 'object' ? _store[key] : undefined) : _store;
    },

    setData: function(newData, target, name) {
      _store = _makeReactive(newData, _onStoreChange);
      _invalidateRoute(window.location.pathname + window.location.search);
      _notifyWatchers(_store);
      _renderBindings();
      if (name) { return _rerender(name, _store, target); }
      return Promise.resolve(_store);
    },

    render: function(name, data, target) {
      return _rerender(name, data !== undefined ? data : _store, target);
    },

    watch: function(fn) {
      _watchers.push(fn);
      return function() { _watchers = _watchers.filter(function(w) { return w !== fn; }); };
    },

    // ── Reactive collections ──────────────────────────────────────────────
    //
    // breeze.list('items') returns helpers over breeze.data().items that
    // mutate the array in place (push/splice/index assignment). Because the
    // store is Proxy-backed, every one of these calls automatically:
    //   - notifies breeze.watch() callbacks
    //   - re-renders any region registered with breeze.bind()
    //   - invalidates the current route's cache entry
    // — no manual breeze.render()/breeze.setData() call needed afterwards.
    list: function(key) {
      var d = window.breeze.data();
      if (!Array.isArray(d[key])) d[key] = [];
      return {
        all:    function()      { return d[key]; },
        add:    function(item)  { d[key].push(item); return d[key]; },
        update: function(index, patch) {
          var i = (typeof index === 'function') ? d[key].findIndex(index) : index;
          if (i !== -1 && d[key][i] !== undefined) {
            d[key][i] = Object.assign({}, d[key][i], patch);
          }
          return d[key];
        },
        remove: function(index) {
          var i = (typeof index === 'function') ? d[key].findIndex(index) : index;
          if (i !== -1) d[key].splice(i, 1);
          return d[key];
        },
        set: function(arr) { d[key] = arr; return d[key]; },
      };
    },

    // Registers a persistent binding: whenever the store (or store[key], if
    // given) changes — via setData, breeze.list(), or direct mutation of
    // breeze.data() — target is automatically re-rendered with the
    // client-side template 'name'. Renders once immediately with current
    // data. Returns an unbind function.
    bind: function(target, name, key) {
      var b = { target: target, name: name, key: key };
      _bindings.push(b);
      var data = key ? window.breeze.data(key) : window.breeze.data();
      _rerender(name, data, target);
      return function unbind() { _bindings = _bindings.filter(function(x) { return x !== b; }); };
    },

    // Force the next visit to 'url' (default: the current route) to
    // re-fetch instead of reusing a cached fragment. invalidateAll() clears
    // every cached route. Called automatically on any store mutation and
    // on non-GET SPA form submissions; exposed here for manual use when a
    // mutation happens through some path the framework can't see (e.g. a
    // fetch() call you make yourself).
    invalidate:    function(url) { _invalidateRoute(url || (window.location.pathname + window.location.search)); },
    invalidateAll: function()    { _invalidateCache(); },

    // Delegated event binding: attaches once to document and matches
    // selector via closest() on every event, so handlers keep working
    // for elements that get replaced by a later swap — unlike
    // el.addEventListener() calls made inside a view's own inline script,
    // which lose their target the moment that view's content is swapped
    // out and back in. Returns a function that removes the listener.
    on: function(selector, event, handler) {
      var listener = function(e) {
        // A target is not always an Element: clicks can land on a text
        // node, and events retargeted from the document or window have no
        // closest() at all. Walk up to the nearest element first so those
        // cases match normally instead of throwing inside the listener.
        var node = e.target;
        if (node && node.nodeType !== 1) node = node.parentElement;
        if (!node || typeof node.closest !== 'function') return;
        var el = node.closest(selector);
        if (el) handler.call(el, e, el);
      };
      document.addEventListener(event, listener);

      return function off() { document.removeEventListener(event, listener); };
    },

    ws: function(path, handlers) {
      handlers = handlers || {};
      var delay = 1000, maxDelay = 30000, stopped = false, socket = null;
      var proto = location.protocol === 'https:' ? 'wss' : 'ws';
      var url   = proto + '://' + location.host + path;

      function connect() {
        socket = new WebSocket(url);

        socket.addEventListener('open', function(e) {
          delay = 1000;
          if (handlers.onOpen) handlers.onOpen(e);
          window.dispatchEvent(new CustomEvent('breeze:ws:open', { detail: { path: path } }));
        });

        socket.addEventListener('message', function(e) {
          if (handlers.onMessage) handlers.onMessage(e);
          window.dispatchEvent(new CustomEvent('breeze:ws:message', {
            detail: { data: e.data, path: path }
          }));
        });

        socket.addEventListener('close', function(e) {
          if (handlers.onClose) handlers.onClose(e);
          window.dispatchEvent(new CustomEvent('breeze:ws:close', {
            detail: { path: path, code: e.code }
          }));
          if (!stopped) {
            setTimeout(connect, delay);
            delay = Math.min(delay * 2, maxDelay);
          }
        });

        socket.addEventListener('error', function(e) {
          if (handlers.onError) handlers.onError(e);
        });
      }

      connect();

      return {
        send:  function(msg) {
          if (socket && socket.readyState === WebSocket.OPEN) socket.send(msg);
        },
        close: function() { stopped = true; if (socket) socket.close(); },
        get socket() { return socket; },
      };
    },
  };

})();
</script>
`
}

// breezeDataScript wraps the page JSON in a non-executing script tag so the
// client can read it with breeze.data() without it polluting the global scope.
func breezeDataScript(dataJSON []byte) string {
	return `<script id="__breeze_data__" type="application/json">` +
		string(dataJSON) +
		`</script>` + "\n"
}

// ─── Preload ──────────────────────────────────────────────────────────────────

// Preload parses all view and component templates eagerly.
// Call this at startup (after all routes are registered) to surface template
// errors early and warm the cache before the first request.
//
// With i18n enabled, templates are parsed for the default locale; other
// locales parse lazily on their first request (parse errors are locale-
// independent, so Preload still surfaces every template error).
func (te *TemplateEngine) Preload() error {
	locale := ""
	if te.i18n != nil {
		locale = te.i18n.DefaultLocale()
	}

	// Load components.
	compFiles, _ := filepath.Glob(filepath.Join(te.compDir, "*.html"))
	for _, cf := range compFiles {
		name := strings.TrimSuffix(filepath.Base(cf), ".html")
		t, err := te.parseComponent(name, locale)
		if err != nil {
			return fmt.Errorf("breeze/template: preload component %q: %w", name, err)
		}
		te.mu.Lock()
		te.components[localeKey(name, locale)] = t
		te.mu.Unlock()
	}

	// Load views.
	viewFiles, _ := filepath.Glob(filepath.Join(te.viewsDir, "*.html"))
	for _, vf := range viewFiles {
		base := filepath.Base(vf)
		// Skip the layout file itself.
		if te.layoutFile != "" {
			if abs, _ := filepath.Abs(vf); abs == func() string { a, _ := filepath.Abs(te.layoutFile); return a }() {
				continue
			}
		}
		name := strings.TrimSuffix(base, ".html")
		t, err := te.parseView(name, locale)
		if err != nil {
			return fmt.Errorf("breeze/template: preload view %q: %w", name, err)
		}
		te.mu.Lock()
		te.templates[localeKey(name, locale)] = t
		te.mu.Unlock()
	}

	return nil
}
