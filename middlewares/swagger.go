package middleware

import (
	"github.com/nelthaarion/breeze"
	"github.com/nelthaarion/breeze/scalar"
)

// SwaggerOptions configures the OpenAPI/Scalar middleware.
type SwaggerOptions struct {
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

// ScalarOptions is the Scalar-native alias for SwaggerOptions.
type ScalarOptions = SwaggerOptions

// SwaggerMiddleware enables the OpenAPI documentation system and registers the
// spec/UI endpoints.  Call it once at startup, before adding routes:
//
//	router.Use(middleware.SwaggerMiddleware(router, middleware.SwaggerOptions{
//	    Title:   "My API",
//	    Version: "2.0.0",
//	}))
//
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
func SwaggerMiddleware(router *breeze.Router, opts SwaggerOptions) breeze.HandlerFunc {
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
	router.HandleBlocking(breeze.GET, opts.JSONPath, func(ctx *breeze.Context) {
		data := scalar.Generate()
		ctx.SetHeader("Content-Type", "application/json")
		ctx.SetHeader("Access-Control-Allow-Origin", "*")
		ctx.Status(200)
		ctx.Res.Body = data
	})

	// Register the Scalar UI endpoint (if a path is configured).
	if opts.UIPath != "" {
		jsonPath := opts.JSONPath // capture for closure
		router.HandleBlocking(breeze.GET, opts.UIPath, func(ctx *breeze.Context) {
			data := scalar.GenerateUI(jsonPath)
			ctx.HTML(data)
		})
	}

	// Return a pass-through middleware (OpenAPI doc collection happens at
	// route registration via Doc(), not at request time).
	return func(ctx *breeze.Context) {
		ctx.Next()
	}
}

// ScalarMiddleware is the Scalar-native entrypoint for the docs middleware.
func ScalarMiddleware(router *breeze.Router, opts ScalarOptions) breeze.HandlerFunc {
	return SwaggerMiddleware(router, opts)
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
	return func(ctx *breeze.Context) {
		ctx.Next()
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
