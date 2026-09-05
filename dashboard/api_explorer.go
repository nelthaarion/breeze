package dashboard

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/nelthaarion/breeze/v2"
	"github.com/nelthaarion/breeze/v2/scalar"
)

// APIExplorerRoute describes one registered route for the API Explorer UI.
type APIExplorerRoute struct {
	Method  string             `json:"method"`
	Path    string             `json:"path"`    // OpenAPI-style: /users/{id}
	Pattern string             `json:"pattern"` // Breeze-style: /users/:id
	Tags    []string           `json:"tags,omitempty"`
	Summary string             `json:"summary,omitempty"`
	Inputs  []APIExplorerInput `json:"inputs,omitempty"`

	// Description is the longer explanation from the route's RouteDoc.
	Description string `json:"description,omitempty"`

	// Documented reports whether a RouteDoc was found. Sent even when false,
	// because a route missing from the OpenAPI document is a defect the explorer
	// is in a position to show and nothing else is.
	Documented bool `json:"documented"`
}

// APIExplorerInput describes one input group for a route.
type APIExplorerInput struct {
	Type        string                 `json:"type"` // body / query / params / header
	Description string                 `json:"description,omitempty"`
	Fields      map[string]FieldSchema `json:"fields,omitempty"`
}

// FieldSchema is a simple type descriptor for an input field.
type FieldSchema struct {
	Type     string `json:"type"`
	Required bool   `json:"required,omitempty"`
	Default  any    `json:"default,omitempty"`

	// Description is the field's `description:"…"` struct tag, which is the only
	// place the explorer can learn what a field means rather than what type it is.
	Description string `json:"description,omitempty"`
}

// handleAPIExplorerList returns the list of routes formatted for the API
// explorer, with parameter schemas extracted from the Scalar registry
// (if available) or inferred from the route pattern.
//
// The registry is the better source and is tried first. What it knows and the
// pattern does not: what each field means, what type it is, whether the body is
// required, and what the endpoint is for. The pattern can only ever yield the path
// parameters, typed as string, with no description — which is why a documented
// route now produces a usable explorer entry and an undocumented one still
// produces the fallback rather than nothing.
func (c *Collector) handleAPIExplorerList(ctx *breeze.Context) error {
	docs := make(map[string]scalar.RouteDoc, 16)
	for _, r := range scalar.Routes() {
		docs[strings.ToUpper(r.Method)+" "+r.Path] = r.Doc
	}

	routes := make([]APIExplorerRoute, 0)
	for _, rt := range c.router.RoutesInfo() {
		pattern := rt.Pattern()
		// Skip dashboard's own routes to keep the explorer focused on the app.
		if strings.HasPrefix(pattern, c.cfg.BasePath) {
			continue
		}
		// Skip static file routes.
		if rt.HasWildcard() && strings.HasSuffix(pattern, "/*filepath") {
			continue
		}
		path := breezeToOpenAPIPath(pattern)
		entry := APIExplorerRoute{
			Method:  string(rt.Method()),
			Path:    path,
			Pattern: pattern,
		}

		doc, documented := docs[strings.ToUpper(string(rt.Method()))+" "+path]
		if documented {
			entry.Documented = true
			entry.Summary = doc.Title
			entry.Description = doc.Description
			entry.Tags = doc.Tags
			entry.Inputs = explorerInputs(doc)
		}

		// The pattern-derived fallback, for a route with no Doc wrapper or one
		// whose documentation omitted its path parameters. Without it an
		// undocumented route with an :id would render a URL the explorer cannot
		// fill in, which is worse than a bare string field.
		if !hasExplorerInput(entry.Inputs, "params") && rt.ParamCount() > 0 {
			fields := make(map[string]FieldSchema)
			for _, seg := range rt.Segments() {
				if len(seg) > 0 && seg[0] == ':' {
					fields[seg[1:]] = FieldSchema{Type: "string", Required: true}
				}
			}
			entry.Inputs = append(entry.Inputs, APIExplorerInput{
				Type:   "params",
				Fields: fields,
			})
		}
		routes = append(routes, entry)
	}
	return ctx.JSON(routes)
}

// explorerInputs converts a RouteDoc's input groups into explorer fields.
//
// The schemas are inferred by the same scalar.InferSchema the OpenAPI generator
// uses, so the explorer cannot disagree with the document about what a route
// accepts.
func explorerInputs(doc scalar.RouteDoc) []APIExplorerInput {
	out := make([]APIExplorerInput, 0, len(doc.Input))
	for _, group := range doc.Input {
		schema := scalar.InferSchema(group.Fields)
		if schema == nil || len(schema.Properties) == 0 {
			continue
		}

		required := make(map[string]bool, len(schema.Required))
		for _, name := range schema.Required {
			required[name] = true
		}

		fields := make(map[string]FieldSchema, len(schema.Properties))
		for name, field := range schema.Properties {
			fields[name] = FieldSchema{
				Type: field.Type,
				// A path parameter is required whatever the struct said: without
				// it there is no URL to request.
				Required:    required[name] || group.Type == scalar.InputParams,
				Description: field.Description,
				Default:     field.Example,
			}
		}
		out = append(out, APIExplorerInput{
			Type:        string(group.Type),
			Description: group.Description,
			Fields:      fields,
		})
	}
	return out
}

// hasExplorerInput reports whether a group of the given type is already present.
func hasExplorerInput(inputs []APIExplorerInput, kind string) bool {
	for _, in := range inputs {
		if in.Type == kind && len(in.Fields) > 0 {
			return true
		}
	}
	return false
}

// breezeToOpenAPIPath converts "/users/:id" to "/users/{id}".
func breezeToOpenAPIPath(pattern string) string {
	parts := strings.Split(pattern, "/")
	for i, p := range parts {
		if strings.HasPrefix(p, ":") {
			parts[i] = "{" + p[1:] + "}"
		}
	}
	return strings.Join(parts, "/")
}

// APIExplorerExecRequest is the body of POST /api/api-explorer.
type APIExplorerExecRequest struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"` // full URL or path
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

// APIExplorerExecResponse is the result of executing an explorer request.
type APIExplorerExecResponse struct {
	Status     int               `json:"status"`
	Headers    map[string]string `json:"headers"`
	Body       string            `json:"body"`
	BodyJSON   any               `json:"body_json,omitempty"`
	Size       int               `json:"size"`
	DurationMS float64           `json:"duration_ms"`
	// Pre-generated code snippets for the same request.
	Snippets map[string]string `json:"snippets"`
}

// handleAPIExplorerExec executes an HTTP request from the dashboard UI and
// returns the response along with multi-language code snippets.
//
// # The target is this service, and only this service
//
// The explorer exists to call the application's own routes. Before this, it would
// fetch any absolute URL the request body named — from inside the process, and
// return the body verbatim. That is a server-side request forgery primitive with
// the service's own network position: a caller who reached this endpoint could read
// the cloud metadata service at 169.254.169.254, any internal address the container
// can route to, and any port on the host — none of which the caller could reach
// directly, which is the whole point of the attack.
//
// It is not gated behind much, either. The dashboard's auth is a no-op when
// Username or Password is empty, and `breeze add dashboard --no-auth` generates
// exactly that.
//
// So the target is now pinned to loopback and the port the request arrived on.
// A relative path is resolved against it, and an absolute URL is accepted only if
// it names that same origin. This is a narrowing of what the code did and not of
// what it was for: the doc comment already said "against the running Breeze server
// itself, on localhost".
//
// # Redirects are not followed
//
// Following them would reopen the hole through the front door: a route on this
// service that 302s to an attacker-chosen Location — a URL shortener, an OAuth
// callback, anything reflecting a parameter — would have the explorer fetch it. The
// redirect response is returned as-is instead, which is also more useful to someone
// debugging a redirect.
//
// The stdlib net/http client is used rather than the Breeze client because the
// dashboard needs fine-grained control over headers, body, timeout and — now —
// redirect behaviour.
func (c *Collector) handleAPIExplorerExec(ctx *breeze.Context) error {
	var req APIExplorerExecRequest
	if err := json.Unmarshal(ctx.Req.Body, &req); err != nil {
		return jsonError(ctx, 400, "invalid body: "+err.Error())
	}
	if req.URL == "" {
		return jsonError(ctx, 400, "url required")
	}
	method := strings.ToUpper(req.Method)
	if method == "" {
		method = "GET"
	}

	target, err := explorerTarget(req.URL, ctx.Req.Header["host"], c.listenPort())
	if err != nil {
		return jsonError(ctx, 400, err.Error())
	}

	httpReq, err := http.NewRequest(method, target, strings.NewReader(req.Body))
	if err != nil {
		return jsonError(ctx, 400, err.Error())
	}
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}

	client := &http.Client{
		Timeout: 30 * time.Second,
		// Returning the redirect rather than following it. See the comment above.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	start := time.Now()
	resp, err := client.Do(httpReq)
	if err != nil {
		return jsonError(ctx, 502, err.Error())
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	duration := time.Since(start)

	// Build the response.
	headers := make(map[string]string, len(resp.Header))
	for k, vs := range resp.Header {
		if len(vs) > 0 {
			headers[strings.ToLower(k)] = vs[0]
		}
	}

	// Try to parse the body as JSON for pretty-printing.
	var bodyJSON any
	if strings.Contains(headers["content-type"], "json") {
		_ = json.Unmarshal(body, &bodyJSON)
	}

	// Generate code snippets.
	snippets := generateSnippets(req)

	out := APIExplorerExecResponse{
		Status:     resp.StatusCode,
		Headers:    headers,
		Body:       string(body),
		BodyJSON:   bodyJSON,
		Size:       len(body),
		DurationMS: float64(duration.Microseconds()) / 1000.0,
		Snippets:   snippets,
	}
	return ctx.JSON(out)
}

// generateSnippets produces curl, Go, JavaScript, Python, C#, and PHP code
// for the given request.
func generateSnippets(req APIExplorerExecRequest) map[string]string {
	method := strings.ToUpper(req.Method)
	if method == "" {
		method = "GET"
	}
	return map[string]string{
		"curl":       snippetCurl(req, method),
		"go":         snippetGo(req, method),
		"javascript": snippetJS(req, method),
		"python":     snippetPython(req, method),
		"csharp":     snippetCSharp(req, method),
		"php":        snippetPHP(req, method),
	}
}

func snippetCurl(req APIExplorerExecRequest, method string) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "curl -X %s '%s'", method, req.URL)
	for k, v := range req.Headers {
		fmt.Fprintf(&b, " \\\n  -H '%s: %s'", k, v)
	}
	if req.Body != "" {
		fmt.Fprintf(&b, " \\\n  -d '%s'", strings.ReplaceAll(req.Body, "'", "'\\''"))
	}
	return b.String()
}

func snippetGo(req APIExplorerExecRequest, method string) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, `package main

import (
	"fmt"
	"io"
	"net/http"
	"strings"
)

func main() {
	url := "%s"
	body := strings.NewReader(%#v)
	req, _ := http.NewRequest("%s", url, body)
`, req.URL, req.Body, method)
	for k, v := range req.Headers {
		fmt.Fprintf(&b, `	req.Header.Set("%s", "%s")
`, k, v)
	}
	fmt.Fprintf(&b, `	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	fmt.Println(resp.Status)
	fmt.Println(string(data))
}
`)
	return b.String()
}

func snippetJS(req APIExplorerExecRequest, method string) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, `const resp = await fetch("%s", {
  method: "%s",`, req.URL, method)
	if len(req.Headers) > 0 || req.Body != "" {
		fmt.Fprintln(&b, "")
	}
	if len(req.Headers) > 0 {
		fmt.Fprintln(&b, "  headers: {")
		first := true
		for k, v := range req.Headers {
			if !first {
				fmt.Fprintln(&b, ",")
			}
			fmt.Fprintf(&b, "    \"%s\": \"%s\"", k, v)
			first = false
		}
		fmt.Fprintln(&b, "\n  },")
	}
	if req.Body != "" {
		fmt.Fprintf(&b, "  body: %s,\n", jsString(req.Body))
	}
	fmt.Fprintln(&b, "});")
	fmt.Fprintln(&b, "const data = await resp.text();")
	fmt.Fprintln(&b, "console.log(resp.status, data);")
	return b.String()
}

func snippetPython(req APIExplorerExecRequest, method string) string {
	var b bytes.Buffer
	fmt.Fprintln(&b, "import requests")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "url = \"%s\"\n", req.URL)
	fmt.Fprintf(&b, "headers = %s\n", pyDict(req.Headers))
	if req.Body != "" {
		fmt.Fprintf(&b, "data = %s\n", pyString(req.Body))
	}
	fmt.Fprintf(&b, "resp = requests.request(\"%s\", url", method)
	if len(req.Headers) > 0 {
		fmt.Fprint(&b, ", headers=headers")
	}
	if req.Body != "" {
		fmt.Fprint(&b, ", data=data")
	}
	fmt.Fprintln(&b, ")")
	fmt.Fprintln(&b, "print(resp.status_code)")
	fmt.Fprintln(&b, "print(resp.text)")
	return b.String()
}

func snippetCSharp(req APIExplorerExecRequest, method string) string {
	var b bytes.Buffer
	fmt.Fprintln(&b, "using System.Net.Http;")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "var client = new HttpClient();")
	fmt.Fprintf(
		&b,
		"var request = new HttpRequestMessage(HttpMethod.%s, \"%s\");\n",
		csharpMethod(method),
		req.URL,
	)
	for k, v := range req.Headers {
		fmt.Fprintf(&b, "request.Headers.Add(\"%s\", \"%s\");\n", k, v)
	}
	if req.Body != "" {
		fmt.Fprintf(
			&b,
			"request.Content = new StringContent(%s, System.Text.Encoding.UTF8, \"application/json\");\n",
			csharpString(req.Body),
		)
	}
	fmt.Fprintln(&b, "var response = await client.SendAsync(request);")
	fmt.Fprintln(&b, "var body = await response.Content.ReadAsStringAsync();")
	fmt.Fprintln(&b, "Console.WriteLine((int)response.StatusCode);")
	fmt.Fprintln(&b, "Console.WriteLine(body);")
	return b.String()
}

func snippetPHP(req APIExplorerExecRequest, method string) string {
	var b bytes.Buffer
	fmt.Fprintln(&b, "<?php")
	fmt.Fprintln(&b, "$ch = curl_init();")
	fmt.Fprintf(&b, "curl_setopt($ch, CURLOPT_URL, \"%s\");\n", req.URL)
	fmt.Fprintf(&b, "curl_setopt($ch, CURLOPT_CUSTOMREQUEST, \"%s\");\n", method)
	fmt.Fprintln(&b, "curl_setopt($ch, CURLOPT_RETURNTRANSFER, true);")
	if len(req.Headers) > 0 {
		fmt.Fprintln(&b, "$headers = [")
		for k, v := range req.Headers {
			fmt.Fprintf(&b, "    \"%s: %s\",\n", k, v)
		}
		fmt.Fprintln(&b, "];")
		fmt.Fprintln(&b, "curl_setopt($ch, CURLOPT_HTTPHEADER, $headers);")
	}
	if req.Body != "" {
		fmt.Fprintf(&b, "curl_setopt($ch, CURLOPT_POSTFIELDS, %s);\n", phpString(req.Body))
	}
	fmt.Fprintln(&b, "$response = curl_exec($ch);")
	fmt.Fprintln(&b, "$httpCode = curl_getinfo($ch, CURLINFO_HTTP_CODE);")
	fmt.Fprintln(&b, "curl_close($ch);")
	fmt.Fprintln(&b, "echo $httpCode . \"\\n\";")
	fmt.Fprintln(&b, "echo $response;")
	return b.String()
}

// ─── Small string formatting helpers ──────────────────────────────────────

func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func pyString(s string) string {
	if strings.Contains(s, "\"") && !strings.Contains(s, "'") {
		return "'" + s + "'"
	}
	return "\"" + strings.ReplaceAll(s, "\"", "\\\"") + "\""
}

func pyDict(m map[string]string) string {
	if len(m) == 0 {
		return "{}"
	}
	var b bytes.Buffer
	b.WriteString("{")
	first := true
	for k, v := range m {
		if !first {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s: %s", pyString(k), pyString(v))
		first = false
	}
	b.WriteString("}")
	return b.String()
}

func csharpMethod(method string) string {
	switch method {
	case "GET":
		return "Get"
	case "POST":
		return "Post"
	case "PUT":
		return "Put"
	case "PATCH":
		return "Patch"
	case "DELETE":
		return "Delete"
	case "OPTIONS":
		return "Options"
	default:
		return "Post"
	}
}

func csharpString(s string) string {
	return "\"" + strings.ReplaceAll(strings.ReplaceAll(s, "\\", "\\\\"), "\"", "\\\"") + "\""
}

func phpString(s string) string {
	return "\"" + strings.ReplaceAll(s, "\"", "\\\"") + "\""
}
