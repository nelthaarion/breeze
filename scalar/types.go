package scalar

// OpenAPI is the root OpenAPI 3.1 document.
type OpenAPI struct {
	OpenAPI    string              `json:"openapi"`
	Info       Info                `json:"info"`
	Paths      map[string]PathItem `json:"paths,omitempty"`
	Components *Components         `json:"components,omitempty"`
}

// Info holds the API title and version shown in Scalar.
type Info struct {
	Title       string `json:"title"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
}

// Components holds reusable schemas (currently unused but available for extension).
type Components struct {
	Schemas map[string]*Schema `json:"schemas,omitempty"`
}

// PathItem maps HTTP method names (lowercase) → Operation.
type PathItem map[string]Operation

// Operation describes a single API endpoint.
type Operation struct {
	Summary     string              `json:"summary,omitempty"`
	Description string              `json:"description,omitempty"`
	Tags        []string            `json:"tags,omitempty"`
	Parameters  []Parameter         `json:"parameters,omitempty"`
	RequestBody *RequestBody        `json:"requestBody,omitempty"`
	Responses   map[string]Response `json:"responses"`
}

// Parameter describes a path, query, or header parameter.
type Parameter struct {
	Name        string  `json:"name"`
	In          string  `json:"in"` // "path", "query", "header"
	Required    bool    `json:"required,omitempty"`
	Description string  `json:"description,omitempty"`
	Schema      *Schema `json:"schema"`
}

// RequestBody describes the request body with its content type and schema.
type RequestBody struct {
	Description string               `json:"description,omitempty"`
	Required    bool                 `json:"required,omitempty"`
	Content     map[string]MediaType `json:"content"`
}

// MediaType pairs a schema with a content type key (e.g. "application/json").
type MediaType struct {
	Schema *Schema `json:"schema,omitempty"`
}

// Response describes a single HTTP response.
type Response struct {
	Description string               `json:"description"`
	Content     map[string]MediaType `json:"content,omitempty"`
}

// Schema is an OpenAPI/JSON Schema node.
type Schema struct {
	Type        string             `json:"type,omitempty"`
	Format      string             `json:"format,omitempty"`
	Description string             `json:"description,omitempty"`
	Properties  map[string]*Schema `json:"properties,omitempty"`
	Items       *Schema            `json:"items,omitempty"`
	Required    []string           `json:"required,omitempty"`
	Enum        []any              `json:"enum,omitempty"`
	Example     any                `json:"example,omitempty"`
	// AdditionalProperties preserves the OpenAPI 3.1 / JSON Schema keyword
	// when a fetched document declares it. A pointer distinguishes an omitted
	// keyword (Breeze reports unknown fields as a warning) from explicit false
	// (unknown fields are an error).
	AdditionalProperties *bool `json:"additionalProperties,omitempty"`
}

// ─── Route-level doc descriptor ────────────────────────────────────────────

// InputType declares where a set of fields comes from.
type InputType string

const (
	InputBody   InputType = "body"   // JSON request body
	InputQuery  InputType = "query"  // URL query parameters
	InputParams InputType = "params" // Path parameters (e.g. :id)
	InputHeader InputType = "header" // Request headers
)

// RouteDoc is attached to a route at registration time and describes its
// contract so Scalar can show accurate documentation without relying
// on live request sniffing.
type RouteDoc struct {
	// Title / summary shown in Scalar for this endpoint.
	Title string

	// Tags groups the endpoint in the UI (optional).
	Tags []string

	// Description is a longer human-readable explanation (optional).
	Description string

	// Input declares the input contract. Use one or more InputField slices,
	// each tagged with the appropriate InputType.
	Input []InputGroup

	// Output is the Go value whose shape describes the success response.
	// Pass a zero-value struct or a typed nil pointer, e.g. (*UserResponse)(nil).
	Output any

	// OutputStatus is the HTTP status code for the success response (default 200).
	OutputStatus int

	// OutputDescription is the human-readable description for the response (default "OK").
	OutputDescription string
}

// InputGroup bundles a set of fields with their source location.
type InputGroup struct {
	// Type says where these fields come from (body, query, params, header).
	Type InputType

	// Fields is a Go value whose shape is reflected to build the schema.
	// Pass a zero-value struct or a typed nil pointer.
	Fields any

	// Description is an optional note about this input group.
	Description string

	// Required marks the whole group as required (meaningful for body).
	Required bool
}
