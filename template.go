package breeze

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
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
	// tmplScripts caches the rendered <script id="__breeze_tmpl__"> tag per
	// view, so a full page load does not re-read every template file from
	// disk and re-encode them to JSON on every request. Guarded by mu, like
	// the two caches above; unused in devMode.
	tmplScripts map[string]string
	viewsDir    string
	compDir     string
	layoutFile  string
	funcMap     template.FuncMap
	devMode     bool  // if true, re-parse on every render (hot reload)
	i18n        *I18n // optional translation bundle backing the t helper
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
		templates:   make(map[string]*template.Template),
		components:  make(map[string]*template.Template),
		tmplScripts: make(map[string]string),
		viewsDir:    cfg.ViewsDir,
		compDir:     cfg.ComponentsDir,
		layoutFile:  cfg.LayoutFile,
		devMode:     cfg.DevMode,
		funcMap:     cfg.FuncMap,
		i18n:        cfg.I18n,
	}

	// Dev mode already means "favour visibility over speed" (templates
	// re-parse per request), so it also serves the readable runtime: stepping
	// through minified output in devtools is not debugging. Latched rather
	// than per-engine because the runtime string is global; a process running
	// one dev engine is a dev process.
	if cfg.DevMode {
		useReadableRuntime.Store(true)
	}

	if te.funcMap == nil {
		te.funcMap = template.FuncMap{}
	}

	// Built-in "component" function — renders a named component with data.
	// Rebound per locale in funcsForLocale so nested components translate.
	//
	// The scratch buffer comes from the pool because this runs once per
	// {{component}} tag per render — a page with a nav and three cards calls it
	// four times, and each call previously allocated a buffer and grew it from
	// nothing. The String() copy stays: html/template.HTML is a string type, so
	// the bytes have to be copied out before the buffer is reused.
	te.funcMap["component"] = func(name string, data any) (template.HTML, error) {
		buf := acquireRenderBuf()
		defer releaseRenderBuf(buf)
		if err := te.renderComponent(name, "", data, buf); err != nil {
			return "", err
		}
		return template.HTML(buf.String()), nil
	}

	// Built-in "partial" alias for component.
	te.funcMap["partial"] = te.funcMap["component"]

	// One registry append, at construction; see diag.go.
	te.registerDiagnostics()

	// Built-in "t" translation helper. This base version covers templates
	// rendered with no locale; funcsForLocale rebinds it per locale.
	te.funcMap["t"] = te.tFunc("")

	// Built-in "map" helper: create a map[string]any inline inside a template.
	// Usage: {{component "card" (map "title" "Hello" "body" "World")}}
	te.funcMap["map"] = func(pairs ...any) (map[string]any, error) {
		if len(pairs)%2 != 0 {
			return nil, fmt.Errorf(
				"breeze/template: map requires an even number of arguments (key-value pairs)",
			)
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
		buf := acquireRenderBuf()
		defer releaseRenderBuf(buf)
		if err := te.renderComponent(name, locale, data, buf); err != nil {
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

// renderBufMaxKeep caps the capacity a scratch buffer may have and still be
// worth pooling. One enormous page would otherwise pin its buffer in the pool
// for the life of the process.
const renderBufMaxKeep = 256 << 10

// renderBufPool holds scratch buffers for template execution.
//
// Every render executes its template into a buffer, then assembles the final
// response — content plus the injected script tags — into a second, exactly
// sized one. Only the second buffer's bytes leave the function, so the first is
// pure scratch: a fresh one per request paid the doubling growth of the whole
// page (several allocations and copies for anything non-trivial) and then threw
// the capacity away. Pooling keeps that capacity, so a warm server executes
// templates into memory it already has.
//
// The response body is deliberately NOT pooled. It is handed to the connection,
// and on the async write path gnet returns before the bytes are flushed — see
// pool.go. That is why execView returns a freshly allocated slice rather than
// the scratch buffer's own bytes.
var renderBufPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

func acquireRenderBuf() *bytes.Buffer {
	buf := renderBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	return buf
}

func releaseRenderBuf(buf *bytes.Buffer) {
	if buf.Cap() > renderBufMaxKeep {
		return
	}
	renderBufPool.Put(buf)
}

// RenderView renders a view and writes the response to ctx.
//
//   - In a normal request it wraps the view in the layout (if defined) and
//     injects the Breeze SPA runtime script.
//   - When the request carries "X-Breeze-Partial: true" it returns only the
//     inner view fragment so the client-side router can swap it in without a
//     full page reload.
//
// RenderView writes a rendered view as the response.
//
// It returns an error rather than only writing a 500 body. Both still happen: the 500
// is written so a caller that ignores the return sends something intelligible, and the
// error is returned so one that propagates gets the cause logged by the framework's
// error path. A template that fails to parse is a deployment fault, and the operator
// needs it in a log rather than only in one user's browser.
func (te *TemplateEngine) RenderView(ctx *Context, viewName string, data any) error {
	isPartial := ctx.Req.Header["x-breeze-partial"] == "true"
	locale := te.requestLocale(ctx)

	if te.devMode && te.i18n != nil {
		te.i18n.reloadIfDev()
	}

	scratch := acquireRenderBuf()
	defer releaseRenderBuf(scratch)

	t, err := te.getView(viewName, locale)
	if err != nil {
		ctx.Status(500)
		_ = ctx.WriteString(fmt.Sprintf("template error: %v", err))
		return fmt.Errorf("rendering view %q: %w", viewName, err)
	}
	body, err := te.execView(t, viewName, locale, data, isPartial, scratch)
	if err != nil {
		ctx.Status(500)
		_ = ctx.WriteString(fmt.Sprintf("render error: %v", err))
		return fmt.Errorf("rendering view %q: %w", viewName, err)
	}

	return ctx.HTML(body)
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

// execView executes the right template definition inside t and returns the
// finished response body.
//
// scratch is working memory for the template execution and is not retained: the
// returned slice is freshly allocated, sized exactly, and owned by the caller.
// That split is what lets scratch be pooled — see renderBufPool for why the
// response bytes themselves must not be.
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
	scratch *bytes.Buffer,
) ([]byte, error) {
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
		if err := contentTmpl.Execute(scratch, td); err != nil {
			return nil, err
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
		tmplScript := te.templateScriptFor(viewName)
		i18nScript := te.breezeI18nScript(locale)

		body := scratch.Bytes()
		out := make([]byte, 0, len(body)+dataScriptLen(dataJSON)+len(tmplScript)+len(i18nScript))
		out = append(out, body...)
		out = appendDataScript(out, dataJSON)
		out = append(out, tmplScript...)
		return append(out, i18nScript...), nil
	}

	// Full page: prefer a "layout" definition, else render view directly.
	layoutTmpl := t.Lookup("layout")
	if layoutTmpl != nil {
		if err := layoutTmpl.Execute(scratch, td); err != nil {
			return nil, err
		}
	} else {
		viewTmpl := t.Lookup(viewName + ".html")
		if viewTmpl == nil {
			viewTmpl = t
		}
		if err := viewTmpl.Execute(scratch, td); err != nil {
			return nil, err
		}
	}

	// Serialize page data as JSON so breeze.data() can read it client-side.
	dataJSON, jsonErr := marshalPageData(data)
	if jsonErr != nil {
		dataJSON = []byte("{}")
	}

	// Embed raw template sources so the client can re-render without a server round-trip.
	tmplScript := te.templateScriptFor(viewName)
	i18nScript := te.breezeI18nScript(locale)
	runtime := breezeRuntime()

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
	//
	// Assembled into one exactly-sized slice rather than by concatenating
	// the injected parts and splicing them into a string. The runtime is
	// ~20KB, so the previous version copied it once to build `injection`,
	// again to write it into the buffer, and repeatedly as that buffer
	// doubled to fit — for one page, on every request. Each byte is now
	// copied once into memory sized for the final result.
	body := scratch.Bytes()
	// No <body> tag: inject at the very start so the runtime still loads
	// before anything else has a chance to reference it.
	cut := 0
	if loc := bodyOpenTag.FindIndex(body); loc != nil {
		cut = loc[1]
	}

	out := make([]byte, 0,
		len(body)+dataScriptLen(dataJSON)+len(tmplScript)+len(i18nScript)+len(runtime))
	out = append(out, body[:cut]...)
	out = appendDataScript(out, dataJSON)
	out = append(out, tmplScript...)
	out = append(out, i18nScript...)
	out = append(out, runtime...)
	return append(out, body[cut:]...), nil
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

// templateScriptFor returns the ready-to-inject <script id="__breeze_tmpl__">
// tag for viewName, building it on first use.
//
// Both callers do exactly one thing with the source map — hand it to
// breezeTemplateScript — so the cache holds the finished tag rather than the
// map. That removes the JSON encode and the tag concatenation from the render
// path along with the file reads.
//
// devMode is not cached, for the same reason the template set is not: editing a
// view must show up on the next request. Production caches for the lifetime of
// the process, which matches getView — a view file that changes under a running
// server is already outside what the engine promises to notice.
func (te *TemplateEngine) templateScriptFor(viewName string) string {
	if te.devMode {
		return breezeTemplateScript(te.collectTemplateSources(viewName))
	}

	te.mu.RLock()
	tag, ok := te.tmplScripts[viewName]
	te.mu.RUnlock()
	if ok {
		return tag
	}

	// Built outside the write lock: collectTemplateSources reads files and
	// globs a directory, and holding the lock that guards the template cache
	// across filesystem I/O would stall every concurrent render of every
	// other view. Two goroutines racing here both build it and the second
	// store wins, which is correct because the value is a pure function of
	// what is on disk.
	tag = breezeTemplateScript(te.collectTemplateSources(viewName))
	te.mu.Lock()
	te.tmplScripts[viewName] = tag
	te.mu.Unlock()
	return tag
}

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
func (te *TemplateEngine) RenderJSON(ctx *Context) error {
	var req reRenderRequest
	if err := json.Unmarshal(ctx.Req.Body, &req); err != nil {
		ctx.Status(400)
		return ctx.WriteString("breeze/render: invalid JSON body: " + err.Error())
	}

	buf := acquireRenderBuf()
	defer releaseRenderBuf(buf)
	locale := te.requestLocale(ctx)

	if req.Component != "" {
		// Component render — bare fragment, no layout.
		if err := te.renderComponent(req.Component, locale, req.Data, buf); err != nil {
			ctx.Status(500)
			return ctx.WriteString(
				fmt.Sprintf("breeze/render: component %q: %v", req.Component, err),
			)
		}
		// Copied: see RenderComponent for why a pooled buffer's bytes must
		// not become a response body.
		return ctx.HTML(append([]byte(nil), buf.Bytes()...))
	}

	if req.View != "" {
		// View render — returns only the content block (always partial).
		t, err := te.getView(req.View, locale)
		if err != nil {
			ctx.Status(500)
			return ctx.WriteString(fmt.Sprintf("breeze/render: view %q: %v", req.View, err))
		}
		body, err := te.execView(t, req.View, locale, req.Data, true /* always partial */, buf)
		if err != nil {
			ctx.Status(500)
			return ctx.WriteString(fmt.Sprintf("breeze/render: view %q exec: %v", req.View, err))
		}
		return ctx.HTML(body)
	}

	ctx.Status(400)
	return ctx.WriteString(`breeze/render: request must include "view" or "component"`)
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
	// Blocking: rendering parses the template from disk on a cache miss, and
	// on every request in dev mode. It also takes the engine's write lock to
	// populate the cache. None of that may run on a gnet event loop.
	r.HandleBlocking(GET, pattern, func(ctx *Context) error {
		var data any
		if dataFn != nil {
			data = dataFn(ctx)
		}
		// Propagated, not discarded: a view that fails to parse is a deployment
		// fault, and returning it is what gets it into the operator's log. The
		// browser still receives RenderView's own 500 body either way.
		return engine.RenderView(ctx, viewName, data)
	})
}

// ─── Context helper ───────────────────────────────────────────────────────────

// Render renders a view template with the given data and writes it to the response.
//
// Usage inside a handler:
//
//	ctx.Render(engine, "home", map[string]any{"title": "Home"})
func (ctx *Context) Render(engine *TemplateEngine, viewName string, data any) error {
	return engine.RenderView(ctx, viewName, data)
}

// ─── Server-side fragment helpers ────────────────────────────────────────────

// RenderComponent renders a single component as a bare HTML fragment.
// No layout, no SPA script injection. Designed for endpoints that feed
// breeze.fetch() / breeze.poll() on the client.
//
// Returns the render error for the same reason RenderView does: the 500 goes to the
// browser, the cause goes to the operator.
func (te *TemplateEngine) RenderComponent(ctx *Context, componentName string, data any) error {
	buf := acquireRenderBuf()
	defer releaseRenderBuf(buf)
	if err := te.renderComponent(componentName, te.requestLocale(ctx), data, buf); err != nil {
		ctx.Status(500)
		_ = ctx.WriteString(fmt.Sprintf("component error: %v", err))
		return fmt.Errorf("rendering component %q: %w", componentName, err)
	}
	// Copied out of the pooled buffer, not handed over. The response body
	// outlives this call — on the async write path gnet returns before the
	// bytes reach the socket — so returning buf.Bytes() would let the next
	// render overwrite a response already in flight.
	return ctx.HTML(append([]byte(nil), buf.Bytes()...))
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
	// Blocking for the same reason as View: a cache miss parses from disk.
	r.HandleBlocking(GET, pattern, func(ctx *Context) error {
		var data any
		if dataFn != nil {
			data = dataFn(ctx)
		}
		// Propagated for the same reason as View's.
		return engine.RenderComponent(ctx, componentName, data)
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
	// Blocking: RenderJSON resolves an arbitrary view or component by name,
	// which may parse it from disk.
	r.HandleBlocking(POST, "/breeze/render", func(ctx *Context) error {
		// Propagated for the same reason as View's.
		return engine.RenderJSON(ctx)
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

// Two copies of the runtime are embedded: the readable source, and the
// esbuild-minified bundle regenerated by `go generate ./...` (see gen_assets.go).
//
// Both are compiled into the binary, which costs ~19KB of RSS and buys the
// property that matters: what ships to a browser is byte-identical to what the
// test suite exercised, with no build step required of anyone who merely runs
// `go build`. The minified copy is the default because this string is inlined
// into every full page response — 74KB of comments per page load is a real
// cost paid by users, not developers.
//
// spa.js remains the single source of truth. spa.min.js is generated from it
// and is verified by TestSPAMinifiedMatchesSource, so the two cannot drift.
//
//go:embed spa.js
var spaJS string

//go:embed spa.min.js
var spaJSMin string

// spaRuntimeSource reports whether the readable runtime should be served
// instead of the minified one. Set by TemplateConfig.DevMode via
// useReadableRuntime, so a developer stepping through the runtime in devtools
// sees the commented source, and production does not.
var useReadableRuntime atomic.Bool

// runtimeTagMin and runtimeTagDev hold the wrapped <script> for each bundle,
// built at most once each.
//
// The wrapping used to happen per call, which meant every full page response
// allocated and copied the whole runtime — 20KB minified, 74KB readable — to
// produce a string that is a compile-time constant in all but which of two
// variants is selected. sync.OnceValue rather than an init(): a process serves
// one variant, so building both would spend the RSS this indirection exists to
// avoid.
var (
	runtimeTagMin = sync.OnceValue(func() string { return wrapRuntime(spaJSMin) })
	runtimeTagDev = sync.OnceValue(func() string { return wrapRuntime(spaJS) })
)

func wrapRuntime(js string) string {
	return `<script id="__breeze_spa__">` + js + `</script>`
}

func breezeRuntime() string {
	// A build that somehow lost its generated bundle must still serve a
	// working runtime rather than an empty <script>.
	if useReadableRuntime.Load() || spaJSMin == "" {
		return runtimeTagDev()
	}
	return runtimeTagMin()
}

// appendDataScript wraps the page JSON in a non-executing script tag so the
// client can read it with breeze.data() without it polluting the global scope.
//
// It appends onto dst rather than returning a string, because both callers are
// assembling a response and would otherwise allocate a string to hold a
// concatenation they immediately copy again. Page data is per-request and cannot
// be cached the way the runtime is, so this is the only way that copy goes away.
func appendDataScript(dst []byte, dataJSON []byte) []byte {
	dst = append(dst, dataScriptOpen...)
	dst = append(dst, dataJSON...)
	return append(dst, dataScriptClose...)
}

// dataScriptLen is the exact length appendDataScript will add, so a caller can
// size its buffer once and never grow it.
func dataScriptLen(dataJSON []byte) int {
	return len(dataScriptOpen) + len(dataJSON) + len(dataScriptClose)
}

const (
	dataScriptOpen  = `<script id="__breeze_data__" type="application/json">`
	dataScriptClose = `</script>` + "\n"
)

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
			if abs, _ := filepath.Abs(
				vf,
			); abs == func() string { a, _ := filepath.Abs(te.layoutFile); return a }() {
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
