package middleware

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nelthaarion/breeze"
)

func testI18n(t *testing.T) *breeze.I18n {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"en.json": `{"hello": "Hello"}`,
		"da.json": `{"hello": "Hej"}`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	i18n, err := breeze.NewI18n(breeze.I18nConfig{Dir: dir, DefaultLocale: "en", Fallback: true})
	if err != nil {
		t.Fatal(err)
	}
	return i18n
}

// runLocale runs the locale middleware followed by a handler that writes a
// body (replacing ctx.Res, as real handlers do) and returns the context.
func runLocale(t *testing.T, i18n *breeze.I18n, setup func(ctx *breeze.Context)) *breeze.Context {
	t.Helper()
	ctx := breeze.NewContext(breeze.GET, "/")
	if setup != nil {
		setup(ctx)
	}
	ctx.SetMiddlewareChain(
		[]breeze.HandlerFunc{LocaleMiddleware(i18n)},
		func(ctx *breeze.Context) { ctx.WriteString("ok") },
	)
	ctx.Next()
	return ctx
}

func TestLocaleMiddleware_QueryParamWins(t *testing.T) {
	ctx := runLocale(t, testI18n(t), func(ctx *breeze.Context) {
		ctx.Req.Query = url.Values{"lang": {"da"}}
		ctx.Req.Header["cookie"] = "breeze_locale=en"
		ctx.Req.Header["accept-language"] = "en"
	})
	if got := ctx.Locale(); got != "da" {
		t.Errorf("locale = %q, want da (query param wins)", got)
	}
}

func TestLocaleMiddleware_QueryParamSetsCookie(t *testing.T) {
	ctx := runLocale(t, testI18n(t), func(ctx *breeze.Context) {
		ctx.Req.Query = url.Values{"lang": {"da"}}
	})
	cookie := ctx.GetHeader("Set-Cookie")
	if !strings.Contains(cookie, "breeze_locale=da") {
		t.Errorf("Set-Cookie = %q, want breeze_locale=da", cookie)
	}
}

func TestLocaleMiddleware_UnknownQueryLocaleIgnored(t *testing.T) {
	ctx := runLocale(t, testI18n(t), func(ctx *breeze.Context) {
		ctx.Req.Query = url.Values{"lang": {"xx"}}
	})
	if got := ctx.Locale(); got != "en" {
		t.Errorf("locale = %q, want en (unknown lang falls to default)", got)
	}
	if got := ctx.GetHeader("Set-Cookie"); got != "" {
		t.Error("unknown lang must not persist a cookie")
	}
}

func TestLocaleMiddleware_Cookie(t *testing.T) {
	ctx := runLocale(t, testI18n(t), func(ctx *breeze.Context) {
		ctx.Req.Header["cookie"] = "other=1; breeze_locale=da; more=2"
		ctx.Req.Header["accept-language"] = "en"
	})
	if got := ctx.Locale(); got != "da" {
		t.Errorf("locale = %q, want da (cookie beats Accept-Language)", got)
	}
}

func TestLocaleMiddleware_AcceptLanguage(t *testing.T) {
	ctx := runLocale(t, testI18n(t), func(ctx *breeze.Context) {
		ctx.Req.Header["accept-language"] = "fr, da;q=0.9, en;q=0.5"
	})
	if got := ctx.Locale(); got != "da" {
		t.Errorf("locale = %q, want da (Accept-Language)", got)
	}
}

func TestLocaleMiddleware_DefaultFallback(t *testing.T) {
	ctx := runLocale(t, testI18n(t), nil)
	if got := ctx.Locale(); got != "en" {
		t.Errorf("locale = %q, want en (default)", got)
	}
}

func TestLocaleMiddleware_ContentLanguageHeader(t *testing.T) {
	// The handler replaces ctx.Res via WriteString, so the middleware must
	// set Content-Language after ctx.Next() for it to survive.
	ctx := runLocale(t, testI18n(t), func(ctx *breeze.Context) {
		ctx.Req.Query = url.Values{"lang": {"da"}}
	})
	if got := ctx.GetHeader("Content-Language"); got != "da" {
		t.Errorf("Content-Language = %q, want da", got)
	}
}

func TestLocaleMiddleware_CtxT(t *testing.T) {
	i18n := testI18n(t)
	var got string
	ctx := breeze.NewContext(breeze.GET, "/")
	ctx.Req.Query = url.Values{"lang": {"da"}}
	ctx.SetMiddlewareChain(
		[]breeze.HandlerFunc{LocaleMiddleware(i18n)},
		func(ctx *breeze.Context) { got = ctx.T("hello"); ctx.WriteString(got) },
	)
	ctx.Next()
	if got != "Hej" {
		t.Errorf(`ctx.T("hello") = %q, want "Hej"`, got)
	}
}
