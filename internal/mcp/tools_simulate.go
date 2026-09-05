package mcp

// tools_simulate.go — sending one real request to a running service.
//
// This is the only tool in the package that is not a read of state or a
// description of code. Everything else answers "what is true?"; this one answers
// "what happens if?", and the difference matters enough to keep it in its own
// file.
//
// The reason it exists is that the other tools cannot close the loop. An agent
// can generate a handler, verify that it compiles, and read the route table back
// out of a live service — and still not know whether the handler returns what it
// was supposed to. The dashboard's own statistics do not help, because a fast
// successful request is never attributed to a route; the only way to find out is
// to make the request and look at the answer.
//
// It deliberately does not go through the dashboard or any other feature. It
// speaks plain HTTP to the service's real port with the framework's own client,
// which means it works on a service with no dashboard installed, and means the
// status and body reported here are the ones a real caller would get rather than
// a proxy's account of them.
//
// The single most useful thing this can report is not the success case. It is
// the difference between "the route is not registered" (404 from the router),
// "the route exists and rejected the input" (400 from binding), "the route
// exists and the credentials were wrong" (401 from middleware) and "the route
// exists and the handler broke" (500). Those four are one status code apart and
// have completely different remedies, so the result says which it thinks it is
// rather than leaving an agent to infer it.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/nelthaarion/breeze/client"
)

// simulateTimeout bounds one simulated call.
//
// This is longer than liveTimeout because the target is arbitrary application
// code rather than a dashboard endpoint that reads a ring buffer: a handler that
// queries a database legitimately takes longer than five seconds on a cold
// connection, and reporting that as unreachable would be wrong.
const simulateTimeout = 30 * time.Second

// simulateMethods are the methods this will send.
//
// The list is closed on purpose. An arbitrary method string would be passed
// through to a router that may or may not handle it, and the failure would look
// like a missing route; naming the supported set makes a typo an argument error
// instead. TRACE and CONNECT are absent because neither is something a Breeze
// route is registered for.
var simulateMethods = map[string]bool{
	http.MethodGet:     true,
	http.MethodPost:    true,
	http.MethodPut:     true,
	http.MethodPatch:   true,
	http.MethodDelete:  true,
	http.MethodHead:    true,
	http.MethodOptions: true,
}

func registerSimulationTools(s *Server) {
	s.addTool(simulateRequestTool())
}

// ─── simulate_request ────────────────────────────────────────────────────────

type simulateArgs struct {
	ServiceURL string `json:"service_url"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	// Body is sent as-is. It is a string rather than an object so that a caller
	// can deliberately send malformed JSON to check that binding rejects it —
	// which is a test worth running, and one an object-typed field would make
	// impossible to express.
	Body string `json:"body"`
	// Headers are additional request headers.
	Headers map[string]string `json:"headers"`
	// Token and the Basic credentials are separated out because they are the two
	// schemes the framework's own middleware understands, and spelling them as
	// headers is where callers get it wrong.
	Token    string `json:"token"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// simulateReport is what one simulated call produced.
type simulateReport struct {
	URL    string `json:"url"`
	Method string `json:"method"`
	Status int    `json:"status"`
	// Reading is this tool's interpretation of the status: routed, rejected,
	// unauthorized, not-found, server-error or redirect. It is the field that
	// makes the result actionable, and it is explicitly an interpretation rather
	// than something the service said.
	Reading    string `json:"reading"`
	DurationMS int64  `json:"duration_ms"`
	// ContentType is pulled out of the headers because it is the one header that
	// changes how the body should be read.
	ContentType string `json:"content_type,omitempty"`
	BodyBytes   int    `json:"body_bytes"`
	// Body is the response body as text, truncated. A handler that returns a
	// megabyte of JSON would otherwise fill the caller's context with data it
	// did not ask for.
	Body string `json:"body,omitempty"`
	// BodyTruncated says the text above is not all of it.
	BodyTruncated bool `json:"body_truncated,omitempty"`
	// JSONBody is the decoded body when it is JSON, so a caller can read a field
	// without decoding a string that is already inside a JSON result.
	JSONBody any `json:"json_body,omitempty"`
	// Headers is the response's headers, flattened and sorted.
	Headers map[string]string `json:"headers,omitempty"`
	// SentHeaders names the headers that were sent, without their values. This
	// is what makes an unauthorized result diagnosable: it says whether the
	// credential actually left, which is the thing a caller most often has
	// wrong, without copying a secret into a transcript.
	SentHeaders []string `json:"sent_headers,omitempty"`
	Notes       []string `json:"notes,omitempty"`
}

func simulateRequestTool() *tool {
	return &tool{
		name: "breeze_simulate_request",
		description: "Send one real HTTP request to a running Breeze service and report the status, " +
			"headers and body, with an interpretation of what the status means: whether the route " +
			"is missing, the input was rejected, the credentials were refused, or the handler " +
			"failed. Use this to confirm a generated or edited handler actually behaves, which " +
			"neither compiling it nor reading the route table can tell you. This performs a real " +
			"request, so a non-GET call will have whatever effect the handler has.",
		schema: objectSchema(map[string]any{
			"service_url": stringProp("Base URL of the running service, for example " +
				"http://127.0.0.1:8080. This is the service's own port, not the dashboard's."),
			"method": stringProp("HTTP method: GET, POST, PUT, PATCH, DELETE, HEAD or OPTIONS. " +
				"Defaults to GET."),
			"path": stringProp("Request path, including any query string, for example " +
				"/api/users/1?expand=true."),
			"body": stringProp("Request body, sent verbatim. Content-Type defaults to " +
				"application/json when this looks like JSON and no Content-Type header is given. " +
				"Send deliberately malformed JSON here to check that binding rejects it."),
			"headers": schema{
				"type": "object",
				"description": "Additional request headers. Use this for anything beyond the " +
					"token and Basic credential arguments.",
				"additionalProperties": schema{"type": "string"},
			},
			"token": stringProp("Bearer token, sent as 'Authorization: Bearer <token>'. This is " +
				"the JWT middleware's scheme."),
			"username": stringProp("Username for HTTP Basic authentication."),
			"password": stringProp("Password for HTTP Basic authentication."),
		}, "service_url", "path"),
		run: func(raw json.RawMessage) toolCallResult {
			var a simulateArgs
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}
			return simulateRequest(a)
		},
	}
}

func simulateRequest(a simulateArgs) toolCallResult {
	base, err := normaliseBaseURL(a.ServiceURL)
	if err != nil {
		return errorResult("simulating a request: " + err.Error())
	}

	method := strings.ToUpper(strings.TrimSpace(a.Method))
	if method == "" {
		method = http.MethodGet
	}
	if !simulateMethods[method] {
		return errorResult(fmt.Sprintf("%q is not a method this can send. Use one of "+
			"GET, POST, PUT, PATCH, DELETE, HEAD or OPTIONS.", a.Method))
	}

	path := strings.TrimSpace(a.Path)
	if path == "" {
		return errorResult("a path is required, for example /api/users. Use / for the root route.")
	}
	if !strings.HasPrefix(path, "/") {
		// Silently prepending is right here rather than an error: the intent is
		// unambiguous, and a router that received "api/users" would report a
		// missing route, which is a misleading answer to a typo.
		path = "/" + path
	}

	// Both the token and the Basic credentials write Authorization, and a caller
	// that passed both means one of them and would otherwise silently get
	// whichever this happened to set last.
	if a.Token != "" && (a.Username != "" || a.Password != "") {
		return errorResult("both a token and a username/password were given, and both are sent " +
			"as the Authorization header, so only one can apply. Pass whichever scheme the " +
			"service's middleware expects: Bearer for JWT, Basic for the dashboard.")
	}

	c := client.New(client.Config{Timeout: simulateTimeout})
	defer c.Close()

	target := base + path
	var body []byte
	if a.Body != "" {
		body = []byte(a.Body)
	}

	req := client.NewRequest(method, target, body)

	// Caller-supplied headers go on first so the credential arguments below win
	// a conflict: the dedicated argument is the more specific instruction.
	for key, value := range a.Headers {
		req.SetHeader(key, value)
	}
	switch {
	case a.Token != "":
		req.SetHeader("Authorization", "Bearer "+a.Token)
	case a.Username != "" || a.Password != "":
		req.SetHeader("Authorization", "Basic "+
			base64.StdEncoding.EncodeToString([]byte(a.Username+":"+a.Password)))
	}

	if len(body) > 0 {
		if _, set := req.GetHeader("Content-Type"); !set {
			// Guessing is confined to the one case that is nearly always right
			// and always visible in the result: a body starting with { or [ in a
			// framework whose binding is JSON-first. A caller who wanted
			// something else can say so, and the note records the guess.
			if looksLikeJSON(a.Body) {
				req.SetHeader("Content-Type", "application/json")
			}
		}
	}

	started := time.Now()
	resp, callErr := c.Do(req)
	elapsed := time.Since(started)

	if callErr != nil {
		return structuredErrorResult(fmt.Sprintf("could not reach %s", target), map[string]any{
			"url":    target,
			"method": method,
			"kind":   "unreachable",
			"error":  callErr.Error(),
			"note": "nothing was sent, so this says nothing about the route or the handler. " +
				"Start the service, or check the host and port. Note that a Breeze service's own " +
				"port is separate from the dashboard's and from any MCP listener.",
		})
	}

	report := simulateReport{
		URL:         target,
		Method:      method,
		Status:      resp.Status,
		Reading:     readStatus(resp.Status),
		DurationMS:  elapsed.Milliseconds(),
		BodyBytes:   len(resp.Body),
		Headers:     flattenHeaders(resp.Header),
		SentHeaders: sentHeaderNames(req.Header()),
	}
	report.ContentType = report.Headers["content-type"]

	report.Body, report.BodyTruncated = truncateBody(resp.Body)
	if isJSONContent(report.ContentType) || looksLikeJSON(string(resp.Body)) {
		var decoded any
		if json.Unmarshal(resp.Body, &decoded) == nil {
			report.JSONBody = decoded
		} else if isJSONContent(report.ContentType) {
			report.Notes = append(report.Notes, "the response claims to be JSON but does not "+
				"parse as JSON, which usually means a handler wrote a partial body or set the "+
				"header before failing")
		}
	}

	report.Notes = append(report.Notes, statusNote(resp.Status, method, path)...)
	if len(body) > 0 && report.ContentType == "" {
		report.Notes = append(report.Notes, "the response carried no Content-Type")
	}
	if method == http.MethodHead && len(resp.Body) > 0 {
		report.Notes = append(report.Notes, "a HEAD response should have no body, but this one did")
	}

	summary := fmt.Sprintf("%s %s → %d (%s) in %dms",
		method, path, resp.Status, report.Reading, report.DurationMS)

	// A 4xx or 5xx is not a failed call. The tool was asked what happens, and it
	// found out; marking it an error would make an agent retry a request that
	// will keep giving the same correct answer.
	return structuredResult(summary, report)
}

// readStatus interprets a status code.
//
// These are the readings that matter for the loop this tool closes, which is why
// 404 and 401 are separated from the general 4xx case: the first says the route
// is not there and the second says it is.
func readStatus(status int) string {
	switch {
	case status == 404:
		return "not-found"
	case status == 401 || status == 403:
		return "unauthorized"
	case status == 405:
		return "wrong-method"
	case status >= 500:
		return "server-error"
	case status >= 400:
		return "rejected"
	case status >= 300:
		return "redirect"
	case status >= 200:
		return "routed"
	default:
		return "informational"
	}
}

// statusNote explains the readings whose cause is commonly misdiagnosed.
func statusNote(status int, method, path string) []string {
	switch {
	case status == 404:
		return []string{fmt.Sprintf("the router has no %s %s. Either the route was never "+
			"registered, or it is registered on a different method or with a different parameter "+
			"shape. breeze_get_routes lists what the service actually has, if the dashboard is "+
			"installed; breeze_query_openapi lists what its documentation claims.", method, path)}

	case status == 405:
		return []string{"the path exists but not for this method, so the route is registered " +
			"and the method is wrong. This is a routing answer, not a handler one."}

	case status == 401 || status == 403:
		return []string{"the route exists and middleware refused the request before the handler " +
			"ran. The handler is untested by this call. Check which scheme the middleware " +
			"expects: JWT reads 'Authorization: Bearer', the dashboard reads Basic."}

	case status >= 500:
		return []string{"the route exists and the handler failed while running, so this is a bug " +
			"in the handler rather than a routing or input problem. If panic recovery middleware " +
			"is installed the process is still up and the panic was logged; breeze_get_logs and " +
			"breeze_get_recent_errors will have the stack."}

	case status == 400 || status == 422:
		return []string{"the route exists and the handler rejected the input, which means " +
			"binding and validation ran. This is the expected answer for a malformed body, and " +
			"a sign that the route is wired correctly."}

	case status >= 300 && status < 400:
		return []string{"this is a redirect, so the body is unlikely to be the answer; the " +
			"Location header is."}
	}
	return nil
}

// looksLikeJSON reports whether text begins like a JSON object or array.
func looksLikeJSON(text string) bool {
	trimmed := strings.TrimSpace(text)
	return strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[")
}

// isJSONContent reports whether a Content-Type names JSON, tolerating the
// parameters and vendor suffixes real services send.
func isJSONContent(contentType string) bool {
	lower := strings.ToLower(contentType)
	return strings.Contains(lower, "application/json") || strings.Contains(lower, "+json")
}

// maxSimulateBody bounds the body text in a result.
const maxSimulateBody = 8 << 10

// truncateBody returns body as text, shortened if it is large.
func truncateBody(body []byte) (string, bool) {
	if len(body) <= maxSimulateBody {
		return string(body), false
	}
	return string(body[:maxSimulateBody]), true
}

// flattenHeaders turns a header map into single values, lowercased.
//
// Multi-valued headers are joined rather than dropped, because Set-Cookie is
// exactly the header a caller is checking for and exactly the one that repeats.
func flattenHeaders(header http.Header) map[string]string {
	if len(header) == 0 {
		return nil
	}
	out := make(map[string]string, len(header))
	for key, values := range header {
		out[strings.ToLower(key)] = strings.Join(values, ", ")
	}
	return out
}

// sentHeaderNames lists the request header names, without values.
func sentHeaderNames(header http.Header) []string {
	if len(header) == 0 {
		return nil
	}
	out := make([]string, 0, len(header))
	for key := range header {
		out = append(out, strings.ToLower(key))
	}
	sort.Strings(out)
	return out
}
