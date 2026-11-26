package dashboard

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nelthaarion/go-json-plus"
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
		if base := strings.TrimSuffix(c.cfg.BasePath, "/"); base != "" && (pattern == base || strings.HasPrefix(pattern, base+"/")) {
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
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = "GET"
	}
	switch method {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
	default:
		return jsonError(ctx, 400, "unsupported method: "+method)
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
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "host", "content-length", "connection", "transfer-encoding", "upgrade", "te", "expect", "":
			// Not the caller's to set: the transport owns framing and the target is pinned.
			continue
		}
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
	const maxExplorerResponseBody = 8 << 20
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxExplorerResponseBody+1))
	if readErr != nil {
		return jsonError(ctx, 502, "could not read upstream response")
	}
	if len(body) > maxExplorerResponseBody {
		return jsonError(ctx, 502, "upstream response body exceeds the 8 MiB dashboard limit")
	}
	duration := time.Since(start)

	// Build the response.
	headers := make(map[string]string, len(resp.Header))
	for k, vs := range resp.Header {
		lower := strings.ToLower(k)
		if lower == "set-cookie" || lower == "authorization" || lower == "proxy-authorization" || lower == "cookie" || lower == "x-api-key" {
			continue
		}
		if len(vs) > 0 {
			// Every value, not just the first: a repeated header (Link, Vary, Cache-Control) was cut to one.
			headers[lower] = strings.Join(vs, ", ")
		}
	}

	// Try to parse the body as JSON for pretty-printing.
	var bodyJSON any
	if strings.Contains(headers["content-type"], "json") {
		_ = json.Unmarshal(body, &bodyJSON)
	}

	// Generate code snippets.
	snippets := generateSnippetsFor(req, snippetBase(ctx))

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

// snippetBase is the origin a relative URL in a snippet is completed with: the one the dashboard was
// reached on. A snippet that says `curl -X GET '/users/1'` is not something anyone can run.
func snippetBase(ctx *breeze.Context) string {
	host := strings.TrimSpace(ctx.Req.Header["host"])
	if host == "" || len(host) > 255 {
		return ""
	}
	for i := 0; i < len(host); i++ {
		ch := host[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9':
		case ch == '.', ch == '-', ch == ':', ch == '[', ch == ']':
		default:
			return "" // text that is not a host name never reaches the generated code
		}
	}
	scheme := "http"
	if requestIsSecure(ctx) {
		scheme = "https"
	}
	return scheme + "://" + host
}

// generateSnippets produces curl, Go, JavaScript, Python, C#, and PHP code
// for the given request.
func generateSnippets(req APIExplorerExecRequest) map[string]string {
	return generateSnippetsFor(req, "")
}

// generateSnippetsFor is generateSnippets with a base origin for relative URLs.
//
// Each generator quotes for its own language. They used to splice the URL, every header and the
// body into the source with, at best, the body escaped — and for the wrong rules: a header value
// with a single quote ended the curl argument and let the rest run as shell commands when a
// developer pasted the snippet; a quote in a URL broke the Go and JavaScript literals; a PHP double-quoted
// string interpolates $variables and {$expressions} in a JSON body; a multi-line body was a syntax
// error in Python and C#. What the generated code contains is the caller's input, so it is quoted as data.
func generateSnippetsFor(req APIExplorerExecRequest, base string) map[string]string {
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = "GET"
	}
	if base != "" && strings.HasPrefix(req.URL, "/") {
		req.URL = strings.TrimSuffix(base, "/") + req.URL
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

// sortedHeaders returns a request's headers in a stable order. Ranging over the map gave every
// call its own, so the snippet panel rewrote itself with each send.
func sortedHeaders(m map[string]string) [][2]string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return strings.ToLower(keys[i]) < strings.ToLower(keys[j]) })
	out := make([][2]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, [2]string{k, m[k]})
	}
	return out
}

func snippetCurl(req APIExplorerExecRequest, method string) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "curl -X %s %s", method, shQuote(req.URL))
	for _, h := range sortedHeaders(req.Headers) {
		fmt.Fprintf(&b, " \\\n  -H %s", shQuote(h[0]+": "+h[1]))
	}
	if req.Body != "" {
		fmt.Fprintf(&b, " \\\n  -d %s", shQuote(req.Body))
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
	url := %s
	body := strings.NewReader(%s)
	req, _ := http.NewRequest(%s, url, body)
`, strconv.Quote(req.URL), strconv.Quote(req.Body), strconv.Quote(method))
	for _, h := range sortedHeaders(req.Headers) {
		fmt.Fprintf(&b, "\treq.Header.Set(%s, %s)\n", strconv.Quote(h[0]), strconv.Quote(h[1]))
	}
	b.WriteString(`	resp, err := http.DefaultClient.Do(req)
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
	fmt.Fprintf(&b, "const resp = await fetch(%s, {\n  method: %s,\n", jsString(req.URL), jsString(method))
	if hs := sortedHeaders(req.Headers); len(hs) > 0 {
		b.WriteString("  headers: {\n")
		for i, h := range hs {
			sep := ","
			if i == len(hs)-1 {
				sep = ""
			}
			fmt.Fprintf(&b, "    %s: %s%s\n", jsString(h[0]), jsString(h[1]), sep)
		}
		b.WriteString("  },\n")
	}
	if req.Body != "" {
		fmt.Fprintf(&b, "  body: %s,\n", jsString(req.Body))
	}
	b.WriteString("});\n")
	b.WriteString("const data = await resp.text();\n")
	b.WriteString("console.log(resp.status, data);\n")
	return b.String()
}

func snippetPython(req APIExplorerExecRequest, method string) string {
	var b bytes.Buffer
	b.WriteString("import requests\n\n")
	fmt.Fprintf(&b, "url = %s\n", pyString(req.URL))
	fmt.Fprintf(&b, "headers = %s\n", pyDict(req.Headers))
	if req.Body != "" {
		fmt.Fprintf(&b, "data = %s\n", pyString(req.Body))
	}
	fmt.Fprintf(&b, "resp = requests.request(%s, url", pyString(method))
	if len(req.Headers) > 0 {
		b.WriteString(", headers=headers")
	}
	if req.Body != "" {
		b.WriteString(", data=data")
	}
	b.WriteString(")\n")
	b.WriteString("print(resp.status_code)\n")
	b.WriteString("print(resp.text)\n")
	return b.String()
}

func snippetCSharp(req APIExplorerExecRequest, method string) string {
	var b bytes.Buffer
	b.WriteString("using System.Net.Http;\n\n")
	b.WriteString("var client = new HttpClient();\n")
	fmt.Fprintf(&b, "var request = new HttpRequestMessage(HttpMethod.%s, %s);\n", csharpMethod(method), csharpString(req.URL))
	contentType := "application/json"
	for _, h := range sortedHeaders(req.Headers) {
		switch strings.ToLower(h[0]) {
		case "content-type":
			// A content header cannot go on request.Headers (Add throws); it belongs to the content.
			contentType = h[1]
			continue
		case "content-length":
			continue
		}
		fmt.Fprintf(&b, "request.Headers.TryAddWithoutValidation(%s, %s);\n", csharpString(h[0]), csharpString(h[1]))
	}
	if req.Body != "" {
		mediaType := strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0])
		fmt.Fprintf(&b, "request.Content = new StringContent(%s, System.Text.Encoding.UTF8, %s);\n", csharpString(req.Body), csharpString(mediaType))
	}
	b.WriteString("var response = await client.SendAsync(request);\n")
	b.WriteString("var body = await response.Content.ReadAsStringAsync();\n")
	b.WriteString("Console.WriteLine((int)response.StatusCode);\n")
	b.WriteString("Console.WriteLine(body);\n")
	return b.String()
}

func snippetPHP(req APIExplorerExecRequest, method string) string {
	var b bytes.Buffer
	b.WriteString("<?php\n")
	b.WriteString("$ch = curl_init();\n")
	fmt.Fprintf(&b, "curl_setopt($ch, CURLOPT_URL, %s);\n", phpString(req.URL))
	fmt.Fprintf(&b, "curl_setopt($ch, CURLOPT_CUSTOMREQUEST, %s);\n", phpString(method))
	b.WriteString("curl_setopt($ch, CURLOPT_RETURNTRANSFER, true);\n")
	if hs := sortedHeaders(req.Headers); len(hs) > 0 {
		b.WriteString("$headers = [\n")
		for _, h := range hs {
			fmt.Fprintf(&b, "    %s,\n", phpString(h[0]+": "+h[1]))
		}
		b.WriteString("];\n")
		b.WriteString("curl_setopt($ch, CURLOPT_HTTPHEADER, $headers);\n")
	}
	if req.Body != "" {
		fmt.Fprintf(&b, "curl_setopt($ch, CURLOPT_POSTFIELDS, %s);\n", phpString(req.Body))
	}
	b.WriteString("$response = curl_exec($ch);\n")
	b.WriteString("$httpCode = curl_getinfo($ch, CURLINFO_HTTP_CODE);\n")
	b.WriteString("curl_close($ch);\n")
	b.WriteString("echo $httpCode . \"\\n\";\n")
	b.WriteString("echo $response;\n")
	return b.String()
}

// ─── Quoting, one function per language ──────────────────────────────────

// shQuote quotes s as one POSIX shell word: single quotes, with each embedded single quote
// closing the quoting, adding an escaped quote, and reopening it.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// jsString is a JavaScript string literal. It is also valid Python and valid JSON.
func jsString(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // keep < > & readable in a snippet
	if err := enc.Encode(s); err != nil {
		return strconv.Quote(s)
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

// pyString is a Python string literal. A JSON string literal is one: the escapes JSON uses
// (\n, \", \\, \uXXXX) mean the same in Python.
func pyString(s string) string { return jsString(s) }

func pyDict(m map[string]string) string {
	if len(m) == 0 {
		return "{}"
	}
	var b bytes.Buffer
	b.WriteString("{")
	for i, h := range sortedHeaders(m) {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s: %s", pyString(h[0]), pyString(h[1]))
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
	case "HEAD":
		return "Head"
	default:
		return "Post"
	}
}

// csharpString is a C# regular string literal: backslash, quote and every control character escaped.
func csharpString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == '"':
			b.WriteString(`\"`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// phpString is a PHP single-quoted string: nothing in one is interpolated (a double-quoted one
// expands $name and {$expression}), and only \ and ' need escaping.
func phpString(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}
