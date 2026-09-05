// Command templates-example is the server-rendered half of Breeze: HTML
// templates, layouts and partials, i18n with locale resolution, HTMX partial
// swaps, and static asset serving.
//
// It is the companion to cmd/api-example, which covers the JSON side. Everything
// here is rendered on the server; the only client-side dependency is HTMX,
// loaded from a CDN in the layout.
//
// Run it from this directory so the relative ./views, ./locales and ./public
// paths resolve:
//
//	cd cmd/templates-example && go run .
package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"runtime"
	"sync"
	"time"

	"github.com/nelthaarion/breeze"
	middleware "github.com/nelthaarion/breeze/middlewares"
)

type User struct {
	ID    string
	Name  string
	Email string
}

type StatsData struct {
	Count int
	Time  string
}

// In-memory user store shared across requests.
var (
	usersMu sync.RWMutex
	users   = []User{
		{ID: "1", Name: "Alice", Email: "alice@example.com"},
		{ID: "2", Name: "Bob", Email: "bob@example.com"},
		{ID: "3", Name: "Carol", Email: "carol@example.com"},
	}
)

func getUsers() []User {
	usersMu.RLock()
	defer usersMu.RUnlock()
	cp := make([]User, len(users))
	copy(cp, users)
	return cp
}

func addUser(u User) {
	usersMu.Lock()
	defer usersMu.Unlock()
	users = append(users, u)
}

func removeUser(id string) {
	usersMu.Lock()
	defer usersMu.Unlock()
	for i, u := range users {
		if u.ID == id {
			users = append(users[:i], users[i+1:]...)
			return
		}
	}
}

func main() {
	router := breeze.NewRouter()
	pool := breeze.NewEventLoopWorkerPool(runtime.NumCPU())
	app := breeze.New(router, pool)

	// ── i18n ──────────────────────────────────────────────────────────────
	//
	// Translations: locales/<lang>.json + {{t "some.key"}} in templates.
	// The middleware resolves the request locale from ?lang=, the
	// breeze_locale cookie, or Accept-Language.
	i18n, err := breeze.NewI18n(breeze.I18nConfig{
		Dir:           "./locales",
		DefaultLocale: "en",
		Fallback:      true,
		DevMode:       true,
	})
	if err != nil {
		panic(err)
	}
	router.Use(middleware.LocaleMiddleware(i18n))

	engine := breeze.NewTemplateEngine(breeze.TemplateConfig{
		ViewsDir:      "./views",
		ComponentsDir: "./components",
		LayoutFile:    "./views/layout.html",
		DevMode:       true,
		I18n:          i18n,
	})

	if err := engine.Preload(); err != nil {
		panic(err)
	}

	// ── Re-render support ─────────────────────────────────────────────────
	router.EnableReRender(engine)

	// ── View routes ───────────────────────────────────────────────────────
	router.View("/", engine, "home", nil)
	router.View("/about", engine, "about", nil)
	router.View("/users", engine, "users", func(ctx *breeze.Context) any {
		return map[string]any{
			"users": getUsers(),
			"logs":  []string{},
		}
	})

	// ── SPA form demo routes ──────────────────────────────────────────────
	//
	// GET /search — SPA GET form target. Returns a partial when X-Breeze-Partial
	// is present (SPA navigation / form), or a full page otherwise.
	router.Handle(breeze.GET, "/search", func(ctx *breeze.Context) error {
		q := ctx.Query("q")
		engine.RenderView(ctx, "home", map[string]any{
			"SearchQuery": q,
			"SearchNote":  fmt.Sprintf("Search results for: %q (demo — no real results)", q),
		})

		return nil
	})

	// POST /contact — SPA POST form target. Renders a confirmation fragment.
	// When called with X-Breeze-Partial: true (the SPA form handler always sets it)
	// the template engine returns only the content block — no layout, no scripts.
	router.Handle(breeze.POST, "/contact", func(ctx *breeze.Context) error {
		// Read form fields from URL-encoded body.
		// For a real app you'd use a form parser; for the demo we read raw body.
		name := "someone"
		_ = ctx.Req.Body // body is application/x-www-form-urlencoded
		engine.RenderView(ctx, "about", map[string]any{
			"ContactNote": fmt.Sprintf("Thanks, %s! Message received.", name),
		})

		return nil
	})

	// ── API routes ────────────────────────────────────────────────────────
	router.Handle(breeze.POST, "/api/users", func(ctx *breeze.Context) error {
		var u User
		if err := json.Unmarshal(ctx.Req.Body, &u); err != nil {
			ctx.Status(400)
			return ctx.JSON(map[string]string{"error": "invalid body"})
		}
		usersMu.Lock()
		u.ID = fmt.Sprintf("%d", len(users)+1)
		usersMu.Unlock()
		addUser(u)
		return ctx.JSON(u)
	})

	router.Handle(breeze.GET, "/api/users", func(ctx *breeze.Context) error {
		return ctx.JSON(getUsers())
	})

	router.Handle(breeze.DELETE, "/api/users/:id", func(ctx *breeze.Context) error {
		removeUser(ctx.Param("id"))
		ctx.Status(204)

		return nil
	})

	// ── Fragment routes ───────────────────────────────────────────────────
	router.Fragment("/fragments/stats", engine, "stats", func(ctx *breeze.Context) any {
		return StatsData{
			Count: rand.Intn(500) + 100,
			Time:  time.Now().Format("15:04:05"),
		}
	})

	// ── Static assets ─────────────────────────────────────────────────────
	router.ServeStatic("/public", "./public")

	// ── WebSocket ─────────────────────────────────────────────────────────
	app.WebSocket("/ws", &breeze.WSHandlerFunc{
		Message: func(conn *breeze.WSConn, op byte, payload []byte) {
			app.Hub().BroadcastText(string(payload))
		},
	})

	fmt.Println("Breeze listening on :3000")
	app.Run(3000, true)
}
