package breeze

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/goccy/go-json"
)

// ─── I18n ─────────────────────────────────────────────────────────────────────
//
// Internationalization backed by per-language JSON files.
//
// Locale files live in a directory (default "locales"), one file per
// language, with the locale inferred from the filename:
//
//	locales/
//	  en.json
//	  da.json
//
// Files hold nested objects; keys are addressed with dotted paths:
//
//	{"home": {"title": "Welcome", "greeting": "Hello, %{name}!"}}
//	→ t("home.title"), t("home.greeting" with name arg)
//
// A top-level locale wrapper ({"en": {...}}) is tolerated, so files
// exported from other i18n systems load without editing.
//
// Each file is parsed once at load time and flattened into a
// map["dotted.key"]string, so a lookup at request time is a single map read.
//
// Interpolation uses %{name} placeholders. Pluralization treats a key whose
// children are "one"/"other" (optionally "zero") as plural, selecting the
// form with a "count" argument:
//
//	{"cart": {"items": {"one": "1 item", "other": "%{count} items"}}}
//	→ t("cart.items", "count", 3) → "3 items"

// I18nConfig configures the translation bundle.
type I18nConfig struct {
	// Dir is the directory containing <locale>.json files. Default: "locales"
	Dir string
	// DefaultLocale is used when a request has no resolvable locale and as
	// the fallback source when Fallback is true. Default: "en"
	DefaultLocale string
	// Fallback makes missing keys fall back to the DefaultLocale value
	// before giving up.
	Fallback bool
	// DevMode makes missing keys render loudly as
	// "translation missing: <locale>.<key>" and re-reads locale files on
	// each template render (matching the template engine's hot reload).
	DevMode bool
}

// I18n is a thread-safe translation bundle.
type I18n struct {
	mu       sync.RWMutex
	locales  map[string]map[string]string // locale → flattened key → value
	dir      string
	def      string
	fallback bool
	devMode  bool
}

// NewI18n loads all <locale>.json files from cfg.Dir and returns the bundle.
func NewI18n(cfg I18nConfig) (*I18n, error) {
	if cfg.Dir == "" {
		cfg.Dir = "locales"
	}
	if cfg.DefaultLocale == "" {
		cfg.DefaultLocale = "en"
	}
	i := &I18n{
		dir:      cfg.Dir,
		def:      cfg.DefaultLocale,
		fallback: cfg.Fallback,
		devMode:  cfg.DevMode,
	}
	if err := i.Reload(); err != nil {
		return nil, err
	}
	// One registry append, at construction; see diag.go.
	i.registerDiagnostics()
	return i, nil
}

// Reload re-reads every locale file from disk. Safe for concurrent use.
func (i *I18n) Reload() error {
	files, err := filepath.Glob(filepath.Join(i.dir, "*.json"))
	if err != nil {
		return fmt.Errorf("breeze/i18n: glob %q: %w", i.dir, err)
	}
	if len(files) == 0 {
		return fmt.Errorf("breeze/i18n: no locale files found in %q", i.dir)
	}

	locales := make(map[string]map[string]string, len(files))
	for _, f := range files {
		locale := strings.TrimSuffix(filepath.Base(f), ".json")

		raw, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("breeze/i18n: read %q: %w", f, err)
		}
		var tree map[string]any
		if err := json.Unmarshal(raw, &tree); err != nil {
			return fmt.Errorf("breeze/i18n: parse %q: %w", f, err)
		}

		// Tolerate a top-level locale wrapper: {"en": {...}}.
		if len(tree) == 1 {
			if inner, ok := tree[locale].(map[string]any); ok {
				tree = inner
			}
		}

		flat := make(map[string]string)
		flattenLocale("", tree, flat)
		locales[locale] = flat
	}

	i.mu.Lock()
	i.locales = locales
	i.mu.Unlock()
	return nil
}

// flattenLocale walks a nested locale tree and writes "dotted.key" → value
// entries into out. Non-string leaves are stringified with fmt.Sprint.
func flattenLocale(prefix string, tree map[string]any, out map[string]string) {
	for k, v := range tree {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		switch val := v.(type) {
		case map[string]any:
			flattenLocale(key, val, out)
		case string:
			out[key] = val
		default:
			out[key] = fmt.Sprint(val)
		}
	}
}

// Locales returns the sorted list of loaded locale codes.
func (i *I18n) Locales() []string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	out := make([]string, 0, len(i.locales))
	for l := range i.locales {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

// DefaultLocale returns the configured default locale.
func (i *I18n) DefaultLocale() string { return i.def }

// HasLocale reports whether the given locale is loaded.
func (i *I18n) HasLocale(locale string) bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	_, ok := i.locales[locale]
	return ok
}

// Dict returns the flattened key → value map for a locale (nil if absent).
// The returned map must be treated as read-only.
func (i *I18n) Dict(locale string) map[string]string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.locales[locale]
}

// reloadIfDev re-reads locale files when DevMode is on. Errors are swallowed
// so a transient bad save doesn't take renders down; the stale dictionary
// stays active until the file parses again.
func (i *I18n) reloadIfDev() {
	if i.devMode {
		_ = i.Reload()
	}
}

// T translates key for locale. args are alternating name/value pairs used
// for %{name} interpolation; a "count" argument additionally selects a
// plural form from "zero"/"one"/"other" child keys.
//
// Missing keys: DevMode renders "translation missing: <locale>.<key>";
// otherwise the value falls back to the default locale (when Fallback is
// on), then to a humanized last key segment so pages never render blank.
func (i *I18n) T(locale, key string, args ...any) string {
	if locale == "" {
		locale = i.def
	}

	count, hasCount := findCountArg(args)

	if val, ok := i.lookup(locale, key, count, hasCount); ok {
		return interpolate(val, args)
	}

	if i.devMode {
		return "translation missing: " + locale + "." + key
	}
	if i.fallback && locale != i.def {
		if val, ok := i.lookup(i.def, key, count, hasCount); ok {
			return interpolate(val, args)
		}
	}
	return humanizeKey(key)
}

// lookup resolves key in locale, applying plural-form selection when a
// count argument is present.
func (i *I18n) lookup(locale, key string, count float64, hasCount bool) (string, bool) {
	i.mu.RLock()
	dict := i.locales[locale]
	i.mu.RUnlock()
	if dict == nil {
		return "", false
	}

	if hasCount {
		if count == 0 {
			if v, ok := dict[key+".zero"]; ok {
				return v, true
			}
		}
		if count == 1 {
			if v, ok := dict[key+".one"]; ok {
				return v, true
			}
		}
		if v, ok := dict[key+".other"]; ok {
			return v, true
		}
	}

	v, ok := dict[key]
	return v, ok
}

// findCountArg scans interpolation args for a "count" pair and returns its
// numeric value.
func findCountArg(args []any) (float64, bool) {
	for n := 0; n+1 < len(args); n += 2 {
		name, ok := args[n].(string)
		if !ok || name != "count" {
			continue
		}
		switch v := args[n+1].(type) {
		case int:
			return float64(v), true
		case int64:
			return float64(v), true
		case float64:
			return v, true
		case string:
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				return f, true
			}
		}
	}
	return 0, false
}

// interpolate replaces %{name} placeholders with values from alternating
// name/value pairs. Placeholders with no matching argument are left as-is
// so mistakes stay visible instead of silently vanishing.
func interpolate(s string, args []any) string {
	if len(args) < 2 || !strings.Contains(s, "%{") {
		return s
	}
	for n := 0; n+1 < len(args); n += 2 {
		name, ok := args[n].(string)
		if !ok {
			continue
		}
		s = strings.ReplaceAll(s, "%{"+name+"}", fmt.Sprint(args[n+1]))
	}
	return s
}

// humanizeKey converts the last segment of a dotted key into a readable
// string: "checkout.promo_code" → "Promo code".
func humanizeKey(key string) string {
	seg := key
	if idx := strings.LastIndexByte(key, '.'); idx >= 0 {
		seg = key[idx+1:]
	}
	seg = strings.NewReplacer("_", " ", "-", " ").Replace(seg)
	if seg == "" {
		return key
	}
	return strings.ToUpper(seg[:1]) + seg[1:]
}

// NegotiateLocale picks the best loaded locale for an Accept-Language
// header value (RFC 9110 §12.5.4). Exact matches win, then the primary
// subtag ("en-GB" → "en"); "*" maps to the default locale. Returns "" when
// nothing matches.
func (i *I18n) NegotiateLocale(acceptLanguage string) string {
	if acceptLanguage == "" {
		return ""
	}

	type langQ struct {
		tag string
		q   float64
	}
	var prefs []langQ

	for _, part := range strings.Split(acceptLanguage, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		tag := part
		q := 1.0
		if idx := strings.IndexByte(part, ';'); idx >= 0 {
			tag = strings.TrimSpace(part[:idx])
			for _, param := range strings.Split(part[idx+1:], ";") {
				param = strings.TrimSpace(param)
				if strings.HasPrefix(param, "q=") {
					if f, err := strconv.ParseFloat(param[2:], 64); err == nil {
						q = f
					}
				}
			}
		}
		if tag == "" || q <= 0 {
			continue
		}
		prefs = append(prefs, langQ{tag: strings.ToLower(tag), q: q})
	}

	// Stable sort keeps header order for equal q-values.
	sort.SliceStable(prefs, func(a, b int) bool { return prefs[a].q > prefs[b].q })

	for _, p := range prefs {
		if p.tag == "*" {
			return i.def
		}
		if i.HasLocale(p.tag) {
			return p.tag
		}
		// Primary subtag: "en-GB" → "en".
		if idx := strings.IndexByte(p.tag, '-'); idx > 0 {
			if primary := p.tag[:idx]; i.HasLocale(primary) {
				return primary
			}
		}
	}
	return ""
}

// ─── Context integration ──────────────────────────────────────────────────────

// Context store keys used by the locale middleware.
const (
	ctxLocaleKey = "breeze:locale"
	ctxI18nKey   = "breeze:i18n"
)

// SetLocale stores the resolved request locale on the context.
// Called by the locale middleware; also useful in tests.
func (ctx *Context) SetLocale(locale string) {
	ctx.Set(ctxLocaleKey, locale)
}

// Locale returns the request locale resolved by the locale middleware,
// or "" when none was set.
func (ctx *Context) Locale() string {
	if v, ok := ctx.Get(ctxLocaleKey); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// SetI18n attaches the translation bundle to the context so handlers can
// call ctx.T. Called by the locale middleware.
func (ctx *Context) SetI18n(i *I18n) {
	ctx.Set(ctxI18nKey, i)
}

// I18n returns the translation bundle attached by the locale middleware,
// or nil when none was set.
func (ctx *Context) I18n() *I18n {
	if v, ok := ctx.Get(ctxI18nKey); ok {
		if i, ok := v.(*I18n); ok {
			return i
		}
	}
	return nil
}

// T translates key in the request's locale — the handler-side counterpart
// of the t template helper, e.g. for flash messages or JSON error bodies.
// Degrades to returning the key when no bundle is attached.
func (ctx *Context) T(key string, args ...any) string {
	i := ctx.I18n()
	if i == nil {
		return key
	}
	return i.T(ctx.Locale(), key, args...)
}
