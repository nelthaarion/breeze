package dashboard

// explorer_target_test.go — the API Explorer may only call this service.
//
// # What these assert
//
// explorerTarget is the whole boundary between "the dashboard can call your
// routes" and "anyone who reaches the dashboard can make your server fetch
// arbitrary URLs from inside your network". The tests below are grouped by the
// attack each input represents, because a table of URLs with a want-error column
// says nothing about why any given row matters.
//
// The strongest assertion in the file is the one every case shares: whatever comes
// out points at 127.0.0.1 on this service's own port. Checking that a bad input
// errors is necessary but not sufficient — a rewrite that accepted an input and
// then dialled the wrong host would pass an error-only test.

import (
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/nelthaarion/breeze/v2"
)

const explorerTestPort = 3000

// mustBeLocal fails unless target is loopback on the expected port.
func mustBeLocal(t *testing.T, target string, port int) {
	t.Helper()

	parsed, err := url.Parse(target)
	if err != nil {
		t.Fatalf("explorerTarget produced an unparseable URL %q: %v", target, err)
	}
	if parsed.Scheme != "http" {
		t.Errorf("scheme = %q, want http", parsed.Scheme)
	}
	if parsed.Hostname() != "127.0.0.1" {
		t.Errorf(
			"host = %q, want 127.0.0.1 — the explorer dialled something else",
			parsed.Hostname(),
		)
	}
	if parsed.Port() != strconv.Itoa(port) {
		t.Errorf("port = %q, want %d", parsed.Port(), port)
	}
}

// TestExplorerTargetRefusesAnExternalHost is the finding itself.
//
// Each host here is a different consequence, not a variation on one: metadata
// credentials, an internal service by cluster DNS, a private address, and a public
// host that proves the endpoint was a general-purpose fetcher.
func TestExplorerTargetRefusesAnExternalHost(t *testing.T) {
	hostile := map[string]string{
		"cloud metadata credentials":  "http://169.254.169.254/latest/meta-data/iam/security-credentials/",
		"internal service by DNS":     "http://postgres:5432/",
		"private address":             "http://10.0.0.5/admin",
		"arbitrary public host":       "https://example.com/",
		"loopback name that is not":   "http://localhost.evil.example/",
		"credentials hiding the host": "http://127.0.0.1@evil.example/",
		"scheme-relative":             "//evil.example/x",
		"not http at all":             "file:///etc/passwd",
	}

	for name, raw := range hostile {
		t.Run(name, func(t *testing.T) {
			got, err := explorerTarget(raw, "127.0.0.1:3000", explorerTestPort)
			if err == nil {
				t.Fatalf(
					"explorerTarget(%q) returned %q; the server would have fetched it",
					raw,
					got,
				)
			}
			if got != "" {
				t.Errorf("a rejected input still produced a target: %q", got)
			}
		})
	}
}

// TestExplorerTargetRefusesAnotherPortOnThisHost covers the case a host-only check
// would let through.
//
// Loopback is not a safety property. An admin interface, a debug endpoint, a
// database, or a second service on the same box is reachable at 127.0.0.1 and is
// exactly what the explorer must not be able to reach.
func TestExplorerTargetRefusesAnotherPortOnThisHost(t *testing.T) {
	for _, raw := range []string{
		"http://127.0.0.1:9090/metrics",
		"http://localhost:6379/",
		"http://[::1]:5432/",
	} {
		if got, err := explorerTarget(raw, "127.0.0.1:3000", explorerTestPort); err == nil {
			t.Errorf(
				"explorerTarget(%q) returned %q, want a refusal — that is a different service",
				raw,
				got,
			)
		}
	}
}

// TestExplorerTargetAcceptsAPath is the case the feature exists for.
func TestExplorerTargetAcceptsAPath(t *testing.T) {
	got, err := explorerTarget("/users/1?verbose=true", "127.0.0.1:3000", explorerTestPort)
	if err != nil {
		t.Fatalf("a relative path was refused: %v", err)
	}
	mustBeLocal(t, got, explorerTestPort)
	if !strings.HasSuffix(got, "/users/1?verbose=true") {
		t.Errorf("target = %q, want the path and query preserved", got)
	}
}

// TestExplorerTargetAcceptsItsOwnAbsoluteURL keeps the fix from breaking the UI:
// the snippet panel shows a full URL and a developer will paste it back.
func TestExplorerTargetAcceptsItsOwnAbsoluteURL(t *testing.T) {
	for _, raw := range []string{
		"http://127.0.0.1:3000/users/1",
		"http://localhost:3000/users/1",
		"http://localhost/users/1", // no port — inherits ours rather than defaulting to 80
	} {
		got, err := explorerTarget(raw, "127.0.0.1:3000", explorerTestPort)
		if err != nil {
			t.Fatalf("explorerTarget(%q) was refused: %v", raw, err)
		}
		mustBeLocal(t, got, explorerTestPort)
		if !strings.HasSuffix(got, "/users/1") {
			t.Errorf("target = %q, want the path preserved", got)
		}
	}
}

// TestExplorerTargetIgnoresTheHostHeaderWhenTheListenerIsKnown is the reason
// ListenPort was added.
//
// The Host header is caller-supplied. If it decided the destination, the fix would
// be no fix — the attacker would move from the url field to a header.
func TestExplorerTargetIgnoresTheHostHeaderWhenTheListenerIsKnown(t *testing.T) {
	got, err := explorerTarget("/health", "evil.example:8080", explorerTestPort)
	if err != nil {
		t.Fatalf("explorerTarget refused a path: %v", err)
	}
	mustBeLocal(t, got, explorerTestPort)
}

// TestExplorerTargetFallsBackToTheHostPort covers a Collector with no application,
// which is every unit test in this package and a Collector used only for recording.
//
// Only the port is taken from the header; the host is still loopback.
func TestExplorerTargetFallsBackToTheHostPort(t *testing.T) {
	got, err := explorerTarget("/health", "evil.example:8080", 0)
	if err != nil {
		t.Fatalf("explorerTarget refused a path with no known listener: %v", err)
	}
	mustBeLocal(t, got, 8080)
}

// TestExplorerTargetRejectsAnEmptyURL keeps the handler's own url-required check
// from being the only one — explorerTarget is called from one place today.
func TestExplorerTargetRejectsAnEmptyURL(t *testing.T) {
	if _, err := explorerTarget("   ", "127.0.0.1:3000", explorerTestPort); err == nil {
		t.Error("a blank url was accepted")
	}
}

// TestPortFromHostHeader pins the parsing that the fallback above depends on.
func TestPortFromHostHeader(t *testing.T) {
	cases := map[string]int{
		"127.0.0.1:3000": 3000,
		"localhost:8080": 8080,
		"[::1]:3000":     3000,
		"example.com":    80,
		"":               0,
		"[::1]":          0,
		"host:notaport":  0,
		"host:0":         0,
		"host:70000":     0,
	}
	for in, want := range cases {
		if got := portFromHostHeader(in); got != want {
			t.Errorf("portFromHostHeader(%q) = %d, want %d", in, got, want)
		}
	}
}

// TestExplorerExecRefusesAForeignHostThroughTheRouter is the end-to-end case.
//
// The helper tests above prove explorerTarget refuses a hostile URL. This one
// proves the handler calls it — through the real router, with the real auth
// middleware — because a correct helper that nothing consults is not a fix.
//
// DisableAuth is set for the same reason the DBWriter router test sets it: this
// asserts the SSRF guard, not the auth layer, and needing credentials here would
// obscure which of the two produced the refusal.
func TestExplorerExecRefusesAForeignHostThroughTheRouter(t *testing.T) {
	router := breeze.NewRouter()
	cfg := DefaultConfig()
	cfg.DisableAuth = true
	Install(nil, router, cfg)

	body, err := json.Marshal(APIExplorerExecRequest{
		Method: "GET",
		URL:    "http://169.254.169.254/latest/meta-data/",
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	req := &breeze.HTTPRequest{
		Method: breeze.POST,
		Path:   "/dashboard/api/api-explorer",
		Header: map[string]string{"host": "127.0.0.1:3000"},
		Body:   body,
	}
	handler, middlewares, params := router.Find(req)
	if handler == nil {
		t.Fatal("router.Find(POST /dashboard/api/api-explorer) did not resolve to a handler")
	}

	ctx := breeze.NewContext(req.Method, req.Path)
	ctx.Req = req
	ctx.SetParams(params)
	ctx.SetMiddlewareChain(middlewares, handler)
	_ = ctx.Next()

	if ctx.Res == nil {
		t.Fatal("no response was written")
	}
	if ctx.Res.Status != 400 {
		t.Fatalf("status = %d, want 400 — the metadata service was not refused; body=%s",
			ctx.Res.Status, ctx.Res.Body)
	}

	// And the refusal has to be the target check rather than some earlier
	// rejection that happens to also produce a 400.
	var out map[string]string
	if err := json.Unmarshal(ctx.Res.Body, &out); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, ctx.Res.Body)
	}
	if !strings.Contains(out["error"], "only calls this service") {
		t.Errorf("error = %q, want the target refusal", out["error"])
	}
}
