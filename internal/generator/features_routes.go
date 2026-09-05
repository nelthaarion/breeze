package generator

// Features that register routes rather than wrap them: docs, static files,
// video streaming, WebSocket endpoints, and the template engine.
//
// Their priorities sit above the middleware band (120-160) because route
// registration is order-independent â€” Router.Use rebuilds every route's chain
// on each call, so a route added here still picks up middleware registered at
// priority 10. What the ordering does buy is a stable, readable call list.

import (
	"flag"
	"fmt"
	"strings"
)

func registerRouteFeatures() {
	registerDocs()
	registerStatic()
	registerVideo()
	registerWebSocket()
	registerJSONRPC()
	registerTemplates()
}

func registerDocs() {
	register(&feature{
		Name:     "docs",
		Summary:  "OpenAPI spec endpoint and Scalar API reference UI",
		Priority: 120,
		Imports:  []string{middlewareImport},
		Build: func(fs *flag.FlagSet) func(featureCtx) (featureOutput, error) {
			title := fs.String("title", "Breeze API", "API title shown in the docs")
			version := fs.String("api-version", "1.0.0", "API version shown in the docs")
			description := fs.String("description", "", "API description shown in the docs")
			jsonPath := fs.String("json-path", "/openapi.json", "path serving the OpenAPI JSON")
			uiPath := fs.String("ui-path", "/scalar", "path serving the reference UI")

			return func(ctx featureCtx) (featureOutput, error) {
				descLine := ""
				if *description != "" {
					descLine = fmt.Sprintf("\t\tDescription: %q,\n", *description)
				}

				body := fmt.Sprintf(`func setupDocs(app *breeze.Breeze, router *breeze.Router) {
	// ScalarMiddleware does its work as a side effect of being called: it
	// enables spec collection and registers the two docs routes on the router
	// it is handed. The middleware it returns is a pass-through, so it is
	// discarded rather than added to every request's chain for nothing.
	_ = middleware.ScalarMiddleware(router, middleware.ScalarOptions{
		Title:       %q,
		Version:     %q,
%s		JSONPath:    %q,
		UIPath:      %q,
	})
}`, *title, *version, descLine, *jsonPath, *uiPath)

				return featureOutput{Body: body, Notes: []string{
					fmt.Sprintf("Reference UI at %s, spec at %s.", *uiPath, *jsonPath),
					"Routes appear in the spec only when documented. `breeze generate resource` emits the middleware.Doc calls that do it; add them by hand for routes you wrote yourself.",
				}}, nil
			}
		},
	})
}

func registerStatic() {
	register(&feature{
		Name:     "static",
		Summary:  "serve a directory of static files",
		Priority: 130,
		Build: func(fs *flag.FlagSet) func(featureCtx) (featureOutput, error) {
			prefix := fs.String("prefix", "/public", "URL prefix to serve under")
			root := fs.String("root", "./public", "directory on disk to serve from")

			return func(ctx featureCtx) (featureOutput, error) {
				if !strings.HasPrefix(*prefix, "/") {
					return featureOutput{}, fmt.Errorf("--prefix must start with a slash, got %q", *prefix)
				}
				// --root is both embedded in the generated ServeStatic call and
				// used below to create the directory, so an unchecked value
				// serves a directory outside the project *and* creates one.
				if err := validatePathFlag("root", *root); err != nil {
					return featureOutput{}, err
				}

				body := fmt.Sprintf(`func setupStatic(app *breeze.Breeze, router *breeze.Router) {
	router.ServeStatic(%q, %q)
}`, *prefix, *root)

				dir := strings.TrimPrefix(strings.TrimPrefix(*root, "./"), "/")
				files := map[string]string{}
				if dir != "" {
					// The directory has to exist for the route to serve
					// anything, and an empty directory is not something git
					// will carry.
					files[dir+"/.gitkeep"] = ""
				}

				return featureOutput{Body: body, Files: files, Dirs: []string{dir}, Notes: []string{
					fmt.Sprintf("Serving %s from %s.", *prefix, *root),
				}}, nil
			}
		},
	})
}

func registerVideo() {
	register(&feature{
		Name:      "video",
		Summary:   "range-request video streaming with optional signed URLs",
		Priority:  140,
		Imports:   []string{videoImport, logImport},
		DependsOn: []string{"events", "observability"},
		Build: func(fs *flag.FlagSet) func(featureCtx) (featureOutput, error) {
			root := fs.String("root", "./media", "directory holding the video files")
			prefix := fs.String("prefix", "/videos", "URL prefix to serve under")
			signed := fs.Bool("signed", false, "require a signed token on every request")
			extensions := fs.String("extensions", "", "comma-separated allowed extensions (default: the package default)")
			opaque := fs.Bool("opaque", false, "hide filesystem detail from error responses")

			return func(ctx featureCtx) (featureOutput, error) {
				if err := validatePathFlag("root", *root); err != nil {
					return featureOutput{}, err
				}

				var extra strings.Builder
				var imports []string

				if list := splitList(*extensions); len(list) > 0 {
					quoted := make([]string, len(list))
					for i, e := range list {
						if !strings.HasPrefix(e, ".") {
							e = "." + e
						}
						quoted[i] = fmt.Sprintf("%q", e)
					}
					fmt.Fprintf(&extra, "\t\tExtensions: []string{%s},\n", strings.Join(quoted, ", "))
				}
				if *opaque {
					extra.WriteString("\t\tOpaque:     true,\n")
				}
				if *signed {
					extra.WriteString("\t\tSecret:     []byte(os.Getenv(\"VIDEO_SIGNING_SECRET\")),\n")
					imports = append(imports, osImport)
				}
				// The streamer publishes per-request signals, so hand it the
				// bus and collector when the project has them.
				if ctx.HasEvents {
					extra.WriteString("\t\tBus:        EventBus,\n")
				}
				if ctx.HasObservability {
					extra.WriteString("\t\tCollector:  ObsCollector,\n")
				}

				signHelper := ""
				if *signed {
					signHelper = `

// SignVideo mints a time-limited URL for one file. With Secret set, an
// unsigned or expired request is rejected, so this is the only way to hand a
// player a working URL.
func SignVideo(name string, ttl time.Duration) string {
	return ` + `"` + *prefix + `/" + name + "?" + video.Sign([]byte(os.Getenv("VIDEO_SIGNING_SECRET")), name, ttl)
}`
					imports = append(imports, timeImport)
				}

				body := fmt.Sprintf(`func setupVideo(app *breeze.Breeze, router *breeze.Router) {
	// Mount returns an error for an unreadable or missing root. Serving
	// nothing but 404s from a path that looks configured is worse than
	// refusing to start.
	if err := video.Mount(router, video.Config{
		Root:       %q,
		Prefix:     %q,
%s	}); err != nil {
		log.Fatalf("video: %%v", err)
	}
}%s`, *root, *prefix, extra.String(), signHelper)

				dir := strings.TrimPrefix(strings.TrimPrefix(*root, "./"), "/")
				notes := []string{
					fmt.Sprintf("Serving %s/* from %s, honouring Range requests with 206 + Content-Range.", *prefix, *root),
					fmt.Sprintf("Mount fails at boot if %s does not exist.", *root),
				}
				if *signed {
					notes = append(notes,
						"Set VIDEO_SIGNING_SECRET â€” with signing on, an unsigned request is rejected.",
						"Build URLs with SignVideo(name, ttl).")
				}

				return featureOutput{
					Body:    body,
					Imports: imports,
					Dirs:    []string{dir},
					Files:   map[string]string{dir + "/.gitkeep": ""},
					Notes:   notes,
				}, nil
			}
		},
	})
}

func registerWebSocket() {
	register(&feature{
		Name:     "websocket",
		Summary:  "WebSocket endpoint with connect/message/close hooks",
		Priority: 150,
		Imports:  []string{logImport},
		Build: func(fs *flag.FlagSet) func(featureCtx) (featureOutput, error) {
			path := fs.String("path", "/ws", "path the endpoint is served on")
			broadcast := fs.Bool("broadcast", false, "relay each message to every connected client instead of echoing")

			return func(ctx featureCtx) (featureOutput, error) {
				if !strings.HasPrefix(*path, "/") {
					return featureOutput{}, fmt.Errorf("--path must start with a slash, got %q", *path)
				}

				handleBody := `			// Echo. Replace with your own dispatch.
			if err := conn.Send(opcode, payload); err != nil {
				log.Printf("ws: send: %v", err)
			}`
				if *broadcast {
					handleBody = `			// Relay to everyone, including the sender. Hub().Broadcast
			// walks every live connection, so the cost grows with the
			// number of clients â€” fan out from a queue instead once that
			// number gets large.
			WSHub.Broadcast(opcode, payload)`
				}

				body := fmt.Sprintf(`// WSHub is the hub for this endpoint: it tracks live connections and can
// broadcast to all of them.
var WSHub *breeze.WSHub

func setupWebsocket(app *breeze.Breeze, router *breeze.Router) {
	// WebSocket is on the app rather than the router: the upgrade takes the
	// connection out of the HTTP request cycle, so it is the server that owns
	// it from then on.
	WSHub = app.WebSocket(%q, &breeze.WSHandlerFunc{
		Connect: func(conn *breeze.WSConn) {
			log.Printf("ws: %%s connected", conn.RemoteAddr())
		},
		Message: func(conn *breeze.WSConn, opcode byte, payload []byte) {
%s
		},
		Close: func(conn *breeze.WSConn, code uint16, reason string) {
			log.Printf("ws: %%s closed (%%d %%s)", conn.RemoteAddr(), code, reason)
		},
	})
}`, *path, handleBody)

				notes := []string{
					fmt.Sprintf("Endpoint at %s.", *path),
					"payload aliases the read buffer, which is reused once the callback returns â€” copy anything you keep.",
					"Scaffold a richer handler with `breeze generate ws <Name>`.",
				}
				return featureOutput{Body: body, Notes: notes}, nil
			}
		},
	})
}

func registerTemplates() {
	register(&feature{
		Name:     "templates",
		Summary:  "server-rendered views with layouts, components and SPA re-render",
		Priority: 160,
		Imports:  []string{logImport},
		Build: func(fs *flag.FlagSet) func(featureCtx) (featureOutput, error) {
			viewsDir := fs.String("views", "./views", "directory holding the view templates")
			componentsDir := fs.String("components", "./components", "directory holding the component templates")
			layout := fs.String("layout", "./views/layout.html", "layout template wrapping every view")
			devMode := fs.Bool("dev", true, "reparse templates on change")
			spa := fs.Bool("spa", true, "enable client-side navigation via EnableReRender")

			return func(ctx featureCtx) (featureOutput, error) {
				// All three name directories or files the generator creates, and
				// all three are embedded in the generated TemplateConfig, so an
				// escape here both writes outside the project and produces an app
				// that reads templates from there at runtime.
				for _, f := range []struct{ name, value string }{
					{"views", *viewsDir},
					{"components", *componentsDir},
					{"layout", *layout},
				} {
					if err := validatePathFlag(f.name, f.value); err != nil {
						return featureOutput{}, err
					}
				}

				reRender := ""
				if *spa {
					reRender = `

	// Serves the content block alone for XHR navigations, so a link click
	// swaps the page body instead of reloading the whole document.
	router.EnableReRender(Templates)`
				}

				body := fmt.Sprintf(`// Templates is the view engine. Register a page with router.View, which
// renders viewName inside the layout:
//
//	router.View("/about", Templates, "about", nil)
//
// The last argument can build per-request data:
//
//	router.View("/user", Templates, "user", func(ctx *breeze.Context) any {
//		return map[string]any{"Name": ctx.Query("name")}
//	})
var Templates *breeze.TemplateEngine

func setupTemplates(app *breeze.Breeze, router *breeze.Router) {
	Templates = breeze.NewTemplateEngine(breeze.TemplateConfig{
		ViewsDir:      %q,
		ComponentsDir: %q,
		LayoutFile:    %q,
		DevMode:       %t,
	})

	// Preload parses everything up front, so a broken template is a boot
	// failure with a line number rather than a 500 the first time someone
	// visits that page.
	if err := Templates.Preload(); err != nil {
		log.Fatalf("templates: %%v", err)
	}%s

	router.View("/", Templates, "home", nil)
}`, *viewsDir, *componentsDir, *layout, *devMode, reRender)

				views := strings.TrimPrefix(strings.TrimPrefix(*viewsDir, "./"), "/")
				components := strings.TrimPrefix(strings.TrimPrefix(*componentsDir, "./"), "/")
				layoutPath := strings.TrimPrefix(strings.TrimPrefix(*layout, "./"), "/")

				// Preload errors on a directory with no templates, and the
				// generated setup treats that as fatal â€” so a layout and one
				// view have to ship with the block.
				files := map[string]string{
					layoutPath:               templatesLayoutHTML,
					views + "/home.html":     templatesHomeHTML,
					components + "/.gitkeep": "",
				}

				return featureOutput{
					Body:  body,
					Files: files,
					Dirs:  []string{views, components},
					Notes: []string{
						fmt.Sprintf("Scaffolded %s and %s/home.html; the root route renders it.", layoutPath, views),
						"Add pages with router.View(pattern, Templates, viewName, dataFn).",
						"DevMode reparses on every request â€” turn it off for production builds.",
					},
				}, nil
			}
		},
	})
}

// templatesLayoutHTML is the wrapper every view renders into. It deliberately
// calls no component: {{component "nav" .}} would make the layout depend on a
// components file existing, and Preload would fail before the user has written
// one.
const templatesLayoutHTML = `{{define "layout"}}<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>Breeze App</title>
  <style>
    * { box-sizing: border-box; margin: 0; padding: 0; }
    body { font-family: system-ui, sans-serif; background: #f5f5f5; color: #333; }
    #breeze-app { max-width: 960px; margin: 2rem auto; padding: 0 1rem; }
    .loading { opacity: 0.5; transition: opacity .15s; }
  </style>
</head>
<body>
  <div id="breeze-app">
    {{template "content" .}}
  </div>
</body>
</html>
{{end}}
`

// templatesHomeHTML defines the "content" block the layout renders.
const templatesHomeHTML = `{{define "content"}}
<div class="page">
  <h1>Breeze templates</h1>
  <p>This view is views/home.html, rendered inside views/layout.html.</p>
  <p>Add another with <code>router.View("/about", Templates, "about", nil)</code>.</p>
</div>

<style>
  .page h1 { font-size: 2.5rem; margin-bottom: .5rem; }
  .page > p { color: #666; margin-bottom: 1rem; }
  code { background: #f0f0f0; border-radius: 4px; padding: 1px 5px; font-family: monospace; }
</style>
{{end}}
`
