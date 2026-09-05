package breeze

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newI18nEngine builds a template engine wired to an I18n bundle, with a
// view and a component that both use the t helper.
func newI18nEngine(t *testing.T) *TemplateEngine {
	t.Helper()
	root := t.TempDir()
	viewsDir := filepath.Join(root, "views")
	compDir := filepath.Join(root, "components")
	for _, d := range []string{viewsDir, compDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	view := `<html lang="{{.Locale}}"><body>` +
		`<h1>{{t "home.title"}}</h1>` +
		`<p>{{t "home.greeting" "name" .Data.Name}}</p>` +
		`{{component "badge" .Data}}` +
		`</body></html>`
	if err := os.WriteFile(filepath.Join(viewsDir, "home.html"), []byte(view), 0o644); err != nil {
		t.Fatal(err)
	}

	comp := `{{define "badge"}}<span>{{t "cart.items" "count" 3}}</span>{{end}}`
	if err := os.WriteFile(filepath.Join(compDir, "badge.html"), []byte(comp), 0o644); err != nil {
		t.Fatal(err)
	}

	i18n, err := NewI18n(I18nConfig{Dir: writeLocaleFiles(t), DefaultLocale: "en", Fallback: true})
	if err != nil {
		t.Fatal(err)
	}

	return NewTemplateEngine(TemplateConfig{
		ViewsDir:      viewsDir,
		ComponentsDir: compDir,
		LayoutFile:    filepath.Join(viewsDir, "layout.html"), // does not exist — no layout
		I18n:          i18n,
	})
}

// renderForLocale renders the "home" view with the given request locale and
// returns the response body.
func renderForLocale(t *testing.T, te *TemplateEngine, locale string) string {
	t.Helper()
	ctx := NewContext(GET, "/")
	if locale != "" {
		ctx.SetLocale(locale)
	}
	te.RenderView(ctx, "home", map[string]any{"Name": "Alice"})
	if ctx.Res == nil {
		t.Fatal("no response written")
	}
	if ctx.Res.Status != 200 {
		t.Fatalf("status = %d, body = %s", ctx.Res.Status, ctx.Res.Body)
	}
	return string(ctx.Res.Body)
}

func TestTemplateI18n_TranslatesPerRequestLocale(t *testing.T) {
	te := newI18nEngine(t)

	en := renderForLocale(t, te, "en")
	for _, want := range []string{"Welcome", "Hello, Alice!", `lang="en"`} {
		if !strings.Contains(en, want) {
			t.Errorf("en render missing %q in:\n%s", want, en)
		}
	}

	// Same engine, same view, different locale — must not serve the cached
	// English template set.
	da := renderForLocale(t, te, "da")
	for _, want := range []string{"Velkommen", "Hej, Alice!", `lang="da"`} {
		if !strings.Contains(da, want) {
			t.Errorf("da render missing %q in:\n%s", want, da)
		}
	}
	if strings.Contains(da, "Welcome") {
		t.Error("da render leaked English from the per-view cache")
	}

	// And back to English — the da render must not have poisoned the cache.
	en2 := renderForLocale(t, te, "en")
	if !strings.Contains(en2, "Welcome") {
		t.Errorf("second en render missing Welcome:\n%s", en2)
	}
}

func TestTemplateI18n_ComponentsTranslate(t *testing.T) {
	te := newI18nEngine(t)
	da := renderForLocale(t, te, "da")
	if !strings.Contains(da, "<span>3 varer</span>") {
		t.Errorf("component not translated, got:\n%s", da)
	}
}

func TestTemplateI18n_NoLocaleUsesDefault(t *testing.T) {
	te := newI18nEngine(t)
	body := renderForLocale(t, te, "")
	if !strings.Contains(body, "Welcome") {
		t.Errorf("default-locale render missing Welcome:\n%s", body)
	}
}

func TestTemplateI18n_DictionaryInjected(t *testing.T) {
	te := newI18nEngine(t)
	da := renderForLocale(t, te, "da")

	if !strings.Contains(da, `id="__breeze_i18n__"`) {
		t.Fatalf("missing __breeze_i18n__ tag in:\n%s", da)
	}
	if !strings.Contains(da, `data-locale="da"`) {
		t.Error("i18n tag missing data-locale attribute")
	}
	// The active locale's flattened dictionary ships to the client.
	if !strings.Contains(da, `"home.title":"Velkommen"`) {
		t.Error("i18n tag missing flattened da dictionary")
	}
	if strings.Contains(da, "Velkommen</h1>Welcome") ||
		strings.Contains(da, `"home.title":"Welcome"`) {
		t.Error("i18n tag leaked a non-active locale dictionary")
	}
}

func TestTemplateI18n_EngineWithoutI18n(t *testing.T) {
	// An engine with no bundle still parses templates that use t —
	// the helper degrades to returning the key.
	root := t.TempDir()
	viewsDir := filepath.Join(root, "views")
	if err := os.MkdirAll(viewsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	view := `<p>{{t "home.title"}}</p>`
	if err := os.WriteFile(filepath.Join(viewsDir, "plain.html"), []byte(view), 0o644); err != nil {
		t.Fatal(err)
	}

	te := NewTemplateEngine(TemplateConfig{
		ViewsDir:      viewsDir,
		ComponentsDir: filepath.Join(root, "components"),
		LayoutFile:    filepath.Join(viewsDir, "layout.html"),
	})

	ctx := NewContext(GET, "/")
	te.RenderView(ctx, "plain", nil)
	if ctx.Res == nil || ctx.Res.Status != 200 {
		t.Fatalf("render failed: %+v", ctx.Res)
	}
	if !strings.Contains(string(ctx.Res.Body), "home.title") {
		t.Errorf("t without bundle should echo the key, got:\n%s", ctx.Res.Body)
	}
	if strings.Contains(string(ctx.Res.Body), `id="__breeze_i18n__"`) {
		t.Error("engine without i18n must not inject a dictionary tag")
	}
}

func TestTemplateI18n_PreloadWithI18n(t *testing.T) {
	te := newI18nEngine(t)
	if err := te.Preload(); err != nil {
		t.Fatalf("Preload: %v", err)
	}
	// Preload must not break locale-specific rendering afterwards.
	if da := renderForLocale(t, te, "da"); !strings.Contains(da, "Velkommen") {
		t.Errorf("post-preload da render broken:\n%s", da)
	}
}
