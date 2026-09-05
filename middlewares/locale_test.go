package middleware

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nelthaarion/breeze/v2"
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
//
// setup takes the handler signature rather than a bare callback so a test can write
// its arrangement in the same shape as a handler — which is what every call site here
// already does, and what the migration to an error-returning HandlerFunc made
// mandatory. Its error is asserted rather than ignored: an arrangement step that fails
// would otherwise leave the test running against a context it did not set up.
func runLocale(
	t *testing.T,
	i18n *breeze.I18n,
	setup func(ctx *breeze.Context) error,
) *breeze.Context {
	t.Helper()
	ctx := breeze.NewContext(breeze.GET, "/")
	if setup != nil {
		if err := setup(ctx); err != nil {
			t.Fatalf("arranging the request: %v", err)
		}
	}
	ctx.SetMiddlewareChain(
		[]breeze.HandlerFunc{LocaleMiddleware(i18n)},
		func(ctx *breeze.Context) error { return ctx.WriteString("ok") },
	)
	ctx.Next()
	return ctx
}

func TestLocaleMiddleware_QueryParamWins(t *testing.T) {
	ctx := runLocale(t, testI18n(t), func(ctx *breeze.Context) error {
		ctx.Req.Query = url.Values{"lang": {"da"}}
		ctx.Req.Header["cookie"] = "breeze_locale=en"
		ctx.Req.Header["accept-language"] = "en"

		return nil
	})
	if got := ctx.Locale(); got != "da" {
		t.Errorf("locale = %q, want da (query param wins)", got)
	}
}

func TestLocaleMiddleware_QueryParamSetsCookie(t *testing.T) {
	ctx := runLocale(t, testI18n(t), func(ctx *breeze.Context) error {
		ctx.Req.Query = url.Values{"lang": {"da"}}

		return nil
	})
	cookie := ctx.GetHeader("Set-Cookie")
	if !strings.Contains(cookie, "breeze_locale=da") {
		t.Errorf("Set-Cookie = %q, want breeze_locale=da", cookie)
	}
}

func TestLocaleMiddleware_UnknownQueryLocaleIgnored(t *testing.T) {
	ctx := runLocale(t, testI18n(t), func(ctx *breeze.Context) error {
		ctx.Req.Query = url.Values{"lang": {"xx"}}

		return nil
	})
	if got := ctx.Locale(); got != "en" {
		t.Errorf("locale = %q, want en (unknown lang falls to default)", got)
	}
	if got := ctx.GetHeader("Set-Cookie"); got != "" {
		t.Error("unknown lang must not persist a cookie")
	}
}

func TestLocaleMiddleware_Cookie(t *testing.T) {
	ctx := runLocale(t, testI18n(t), func(ctx *breeze.Context) error {
		ctx.Req.Header["cookie"] = "other=1; breeze_locale=da; more=2"
		ctx.Req.Header["accept-language"] = "en"

		return nil
	})
	if got := ctx.Locale(); got != "da" {
		t.Errorf("locale = %q, want da (cookie beats Accept-Language)", got)
	}
}

func TestLocaleMiddleware_AcceptLanguage(t *testing.T) {
	ctx := runLocale(t, testI18n(t), func(ctx *breeze.Context) error {
		ctx.Req.Header["accept-language"] = "fr, da;q=0.9, en;q=0.5"

		return nil
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
	ctx := runLocale(t, testI18n(t), func(ctx *breeze.Context) error {
		ctx.Req.Query = url.Values{"lang": {"da"}}

		return nil
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
		func(ctx *breeze.Context) error { got = ctx.T("hello"); return ctx.WriteString(got) },
	)
	ctx.Next()
	if got != "Hej" {
		t.Errorf(`ctx.T("hello") = %q, want "Hej"`, got)
	}
}
