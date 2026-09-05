# `cmd/templates-example`

## What this demonstrates

The server-rendered half of Breeze: HTML views with a shared layout, reusable
components, i18n with locale resolution, SPA navigation without a JavaScript
framework, and static assets.

Companion to [`../api-example`](../api-example), which covers the JSON side.
Everything here is rendered on the server; the only client-side dependency is the
Breeze runtime injected into the layout.

Specifically:

- `breeze.NewTemplateEngine` with `ViewsDir`, `ComponentsDir`, `LayoutFile`
- `engine.Preload()` — parse everything at startup rather than on first request
- `router.View(path, engine, name, dataFn)` — a view route in one line
- `router.EnableReRender(engine)` — SPA navigation: partials for
  `X-Breeze-Partial`, full pages otherwise
- `breeze.NewI18n` + `middleware.LocaleMiddleware` — `?lang=`, cookie,
  `Accept-Language`
- Five components, including one that is swapped on its own after a POST
- `ServeStatic` and a WebSocket log feed on the same app

## How to run it

Run from **this directory** — `./views`, `./components`, `./locales` and `./public`
are relative paths:

```bash
cd cmd/templates-example
go run .
```

Then:

- <http://localhost:3000/> — home
- <http://localhost:3000/users> — the users table, re-rendered on add and delete
- <http://localhost:3000/about>
- <http://localhost:3000/?lang=da> — the same pages in Danish

Try the users page with the network tab open: adding a user swaps one component,
not the document.

## What to look for

**`router.View` is a whole route.** `router.View("/about", engine, "about", nil)`
registers a GET, renders the named view through the layout, and handles the
partial/full decision. The `dataFn` form is there for a view that needs data; `nil`
is for one that does not.

**`EnableReRender` is the entire SPA story.** There is no client framework and no
build step. The engine renders a partial when the request carries
`X-Breeze-Partial` and a full page when it does not, so the *same route* serves a
first visit and a navigation. Disable JavaScript and every link still works.

**`Preload()` and `DevMode: true` together.** Preload parses every template at
startup so the first request is not slower than the rest. `DevMode` re-parses on
each render so an edit shows up without a restart — the two are not contradictory,
and the `templates` diagnostic probe reports if `DevMode` is still on in a build
that should not have it.

**`LocaleMiddleware` before the engine sees anything.** Locale resolution is a
middleware because it belongs to the request, not the template — which is what lets
`{{t "nav.home"}}` in a component work without threading a locale argument through
every render call.

**`locales/en.json` and `locales/da.json` are flat key-value files.** Adding a
language is adding a file; nothing registers it.

Next: [`../../docs/middlewares.md`](../../docs/middlewares.md) for
`LocaleMiddleware` in the chain, and [`../dashboard-example`](../dashboard-example)
to watch these renders on the dashboard's timeline.
