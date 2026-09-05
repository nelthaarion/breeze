package middleware

import (
	"github.com/nelthaarion/breeze/v2"
	"github.com/nelthaarion/breeze/v2/scalar"
)

// ScalarOptions configures the OpenAPI documentation middleware.
//
// Scalar is the only viewer this framework ships. The Swagger-prefixed spellings
// below are retained as aliases because they are exported API, but the
// Scalar-prefixed ones are canonical: what actually gets served is a Scalar
// reference UI over a spec produced by the scalar package.
//
// Swagger UI generation was removed rather than kept as an opt-in viewer. That
// was a change of direction from the earlier plan, not the plan all along, and
// it is recorded in CHANGELOG.md under "Documentation viewer: Scalar only" with
// the reasoning and the migration note. Restoring a second viewer is a real
// option; it is a deliberate decision that it is not currently taken, and the
// changelog entry is the place that decision lives.
type ScalarOptions struct {
	// Title is the API name shown in Scalar (default: "Breeze API").
	Title string

	// Version is the API version string (default: "1.0.0").
	Version string

	// Description is an optional long description of the API.
	Description string

	// JSONPath is the URL that serves the raw OpenAPI JSON (default: "/openapi.json").
	JSONPath string

	// UIPath is the URL that serves the Scalar UI HTML (default: "/scalar").
	// Set to "" to disable the UI and serve only the JSON spec.
	UIPath string
}

// SwaggerOptions is a deprecated alias for ScalarOptions.
//
// Deprecated: use ScalarOptions. The name predates the move to Scalar and
// describes a viewer this framework no longer ships (see CHANGELOG.md,
// "Documentation viewer: Scalar only"); it is kept only so existing code
// continues to compile.
type SwaggerOptions = ScalarOptions

// ScalarMiddleware enables the OpenAPI documentation system and registers the
// spec/UI endpoints.  Call it once at startup, before adding routes:
//
//	router.Use(middleware.ScalarMiddleware(router, middleware.ScalarOptions{
//	    Title:   "My API",
//	    Version: "2.0.0",
//	}))

// Then annotate individual routes by passing a *scalar.RouteDoc as the last
// argument to router.Handle via the Doc() helper:
//
//	router.Handle(breeze.POST, "/users", createUser,
//	    middleware.Doc(scalar.RouteDoc{
//	        Title: "Create user",
//	        Input: []scalar.InputGroup{
//	            {Type: scalar.InputBody, Fields: CreateUserRequest{}, Required: true},
//	        },
//	        Output: UserResponse{},
//	    }),
//	)
func ScalarMiddleware(router *breeze.Router, opts ScalarOptions) breeze.HandlerFunc {
	// Apply defaults

	if opts.Title == "" {
		opts.Title = "Breeze API"
	}
	if opts.Version == "" {
		opts.Version = "1.0.0"
	}
	if opts.JSONPath == "" {
		opts.JSONPath = "/openapi.json"
	}
	if opts.UIPath == "" {
		opts.UIPath = "/scalar"
	}

	// Activate OpenAPI doc collection and store global API info.
	scalar.Enable()
	scalar.SetInfo(opts.Title, opts.Version, opts.Description)

	// Register the JSON spec endpoint
	// Both docs endpoints are registered as blocking. They regenerate the whole
	// OpenAPI document (and, for the UI, the page around it) on every request —
	// milliseconds of work for a route that is hit by hand a few times a day.
	// That is the opposite of what belongs on an event loop.
	router.HandleBlocking(breeze.GET, opts.JSONPath, func(ctx *breeze.Context) error {
		data := scalar.Generate()
		ctx.SetHeader("Content-Type", "application/json")
		ctx.SetHeader("Access-Control-Allow-Origin", "*")
		ctx.Status(200)
		ctx.Res.Body = data

		return nil
	})

	// Register the Scalar UI endpoint (if a path is configured).
	if opts.UIPath != "" {
		jsonPath := opts.JSONPath // capture for closure
		router.HandleBlocking(breeze.GET, opts.UIPath, func(ctx *breeze.Context) error {
			data := scalar.GenerateUI(jsonPath)
			return ctx.HTML(data)
		})
	}

	// Return a pass-through middleware (OpenAPI doc collection happens at
	// route registration via Doc(), not at request time).
	return func(ctx *breeze.Context) error {
		return ctx.Next()
	}
}

// SwaggerMiddleware is a deprecated alias for ScalarMiddleware.
//
// Deprecated: use ScalarMiddleware. This name is retained because it is exported
// API and removing it would break callers. Note what it does *not* do: it serves
// Scalar, not Swagger UI. If a project needs Swagger UI specifically, it is not
// available here — see CHANGELOG.md, "Documentation viewer: Scalar only".
func SwaggerMiddleware(router *breeze.Router, opts ScalarOptions) breeze.HandlerFunc {
	return ScalarMiddleware(router, opts)
}

// ─── Route documentation helper ─────────────────────────────────────────────

// Doc returns a Breeze HandlerFunc that registers the given RouteDoc for the
// route it is placed on and then immediately yields to the next handler.
//
// Use it as the last middleware in a Handle() call:
//
//	router.Handle(breeze.GET, "/items/:id", getItem,
//	    middleware.Doc(scalar.RouteDoc{
//	        Title: "Get item by ID",
//	        Input: []scalar.InputGroup{
//	            {Type: scalar.InputParams, Fields: struct{ ID string `json:"id" }{}},
//	        },
//	        Output: Item{},
//	    }),
//	)
//
// Doc is a no-op when OpenAPI doc collection is not enabled, so it is safe to leave in
// production code.

func Doc(method, path string, doc scalar.RouteDoc) breeze.HandlerFunc {
	// Register at the moment Doc() is called (i.e., at startup).
	scalar.RegisterRoute(method, path, doc)

	// The returned HandlerFunc is a transparent pass-through at runtime.
	return func(ctx *breeze.Context) error {
		return ctx.Next()
	}
}

// ─── Convenience wrappers per HTTP method ────────────────────────────────────
// These allow a slightly shorter call site when the method is known statically.

func DocGET(path string, doc scalar.RouteDoc) breeze.HandlerFunc {
	return Doc("GET", path, doc)
}

func DocPOST(path string, doc scalar.RouteDoc) breeze.HandlerFunc {
	return Doc("POST", path, doc)
}

func DocPUT(path string, doc scalar.RouteDoc) breeze.HandlerFunc {
	return Doc("PUT", path, doc)
}

func DocPATCH(path string, doc scalar.RouteDoc) breeze.HandlerFunc {
	return Doc("PATCH", path, doc)
}

func DocDELETE(path string, doc scalar.RouteDoc) breeze.HandlerFunc {
	return Doc("DELETE", path, doc)
}

// ─── Tag helper ──────────────────────────────────────────────────────────────

// Tag is a convenience that sets Tags on a RouteDoc and returns it, useful for
// inline construction:
//
//	middleware.Doc("POST", "/users", middleware.Tag("Users", scalar.RouteDoc{...}))
func Tag(tag string, doc scalar.RouteDoc) scalar.RouteDoc {
	doc.Tags = append([]string{tag}, doc.Tags...)
	return doc
}
