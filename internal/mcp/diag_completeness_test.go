package mcp_test

// diag_completeness_test.go — the Part 3 contract: every provisionable feature is
// also diagnosable.
//
// # What this actually asserts
//
// `breeze add` offers 23 features. Before Part 3 the diagnostic surface covered
// five of them, and the gap was invisible: nothing failed, nothing warned, a
// feature simply had no way to report on itself and the only symptom was an agent
// unable to answer "is compression working". A count in a comment would not have
// held, so the check is mechanical — the generator's own feature registry against
// the diag registry, both read at run time.
//
// # Why it lives here
//
// It needs both halves, and this is the only package that can import both. The
// generator is internal, so an external module cannot reach it; the root breeze
// package cannot import dashboard, which imports it. Package mcp_test is outside
// the import graph of everything it imports, so it can hold both ends.
//
// # The two exemptions
//
// One alias and one genuine exemption, both named explicitly rather than handled
// by a loose match, so a future feature cannot slip through by resembling them:
//
//   - migrator registers as "migrate". The feature installs a binary called
//     migrate and the CLI verb is `breeze migrate`, so the probe uses the word a
//     reader will have seen.
//   - tuning has no probe of its own. It is two setters on the application —
//     inline execution and zero-copy headers — and both are reported in the
//     router probe's detail, where a reader is already looking. A separate
//     "tuning" subsystem reporting two booleans would be noise.

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/nelthaarion/breeze/v2"
	"github.com/nelthaarion/breeze/v2/dashboard"
	"github.com/nelthaarion/breeze/v2/diag"
	"github.com/nelthaarion/breeze/v2/events"
	"github.com/nelthaarion/breeze/v2/fleet"
	"github.com/nelthaarion/breeze/v2/internal/generator"
	// Blank imports for the packages whose probes register from init. They are
	// the ones with no handle to construct — a middleware is a closure and the
	// OpenAPI registry is a package global — so importing them is the whole
	// wiring step.
	_ "github.com/nelthaarion/breeze/v2/middlewares"
	_ "github.com/nelthaarion/breeze/v2/middlewares/oauth2"
	"github.com/nelthaarion/breeze/v2/migrate"
	"github.com/nelthaarion/breeze/v2/observability"
	"github.com/nelthaarion/breeze/v2/rpc"
	_ "github.com/nelthaarion/breeze/v2/scalar"
	"github.com/nelthaarion/breeze/v2/video"
	"github.com/nelthaarion/breeze/v2/workflow"
)

// diagAliases maps a feature name to the registry key its probe uses.
var diagAliases = map[string]string{
	"migrator": "migrate",
}

// diagExempt is the set of features with no probe, and why.
var diagExempt = map[string]string{
	"tuning": "reported inside the router probe's detail as inline_execution and " +
		"zero_copy_headers; it is two application setters, not a subsystem",
}

// TestEveryAddableFeatureHasADiagnosticProbe is Part 3's completeness check.
func TestEveryAddableFeatureHasADiagnosticProbe(t *testing.T) {
	wireEverySubsystem(t)

	registered := map[string]bool{}
	for _, name := range diag.Registered() {
		registered[name] = true
	}

	features := generator.FeatureNames()
	if len(features) == 0 {
		t.Fatal("the generator reported no features; this test is checking nothing")
	}

	var missing []string
	for _, feature := range features {
		if reason, exempt := diagExempt[feature]; exempt {
			// Assert the exemption is still true rather than trusting the table:
			// a probe added later under the exempt name means the table is stale.
			if registered[feature] {
				t.Errorf("%q is listed as exempt (%s) but now has a probe; remove the exemption",
					feature, reason)
			}
			continue
		}

		key := feature
		if alias, ok := diagAliases[feature]; ok {
			key = alias
			if registered[feature] {
				t.Errorf("%q is aliased to %q but a probe is also registered under the feature "+
					"name; drop the alias", feature, alias)
			}
		}
		if !registered[key] {
			missing = append(missing, feature)
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("%d `breeze add` feature(s) can be provisioned but not diagnosed: %v\n"+
			"Every feature needs a diag probe so a running service can report on it. Registered: %v",
			len(missing), missing, diag.Registered())
	}
}

// TestNoProbeIsRegisteredUnderAnUnknownName is the reverse direction.
//
// A probe whose key matches no feature name is not necessarily wrong — the router,
// worker pool and Auto-MCP have no `breeze add` equivalent — but each one is a name
// an agent may be handed, so each is listed here deliberately.
func TestNoProbeIsRegisteredUnderAnUnknownName(t *testing.T) {
	wireEverySubsystem(t)

	features := map[string]bool{}
	for _, name := range generator.FeatureNames() {
		features[name] = true
		if alias, ok := diagAliases[name]; ok {
			features[alias] = true
		}
	}

	// Subsystems that exist without being features. Each is part of the framework
	// core rather than something added.
	nonFeature := map[string]bool{
		"router":     true, // always present
		"workerpool": true, // always present
		"auto-mcp":   true, // app.EnableMCP, not `breeze add`
		"locale":     true, // the middleware; "i18n" is the bundle
	}

	for _, name := range diag.Registered() {
		if features[name] || nonFeature[name] {
			continue
		}
		// video registers per-mount keys as "video:<prefix>" so a multi-mount
		// process can be asked about one of them.
		if strings.HasPrefix(name, "video:") {
			continue
		}
		t.Errorf("a probe is registered under %q, which is neither a `breeze add` feature nor a "+
			"listed framework subsystem. Add it to nonFeature with a reason, or rename it to "+
			"match its feature.", name)
	}
}

// wireEverySubsystem constructs one of everything, so every probe is registered.
//
// Constructors rather than a generated project: the point is to exercise the
// registration calls this repository ships, and a generated project would test the
// templates instead. Nothing here starts a listener.
//
// Called by both tests. The registry is process-wide and last-registration-wins, so
// running it twice is idempotent by design rather than by luck.
func wireEverySubsystem(t *testing.T) {
	t.Helper()

	router := breeze.NewRouter()
	app := breeze.New(router, breeze.NewEventLoopWorkerPool(runtime.NumCPU()))

	// router, workerpool, auto-mcp and static come from New; the static probe
	// needs a mount to report on.
	router.ServeStatic("/public", t.TempDir())

	// The subsystems with their own constructors.
	events.New()
	observability.NewCollector(observability.Config{})
	workflow.NewEngine()
	fleet.New(fleet.TracerConfig{})
	rpc.NewServer(rpc.NewRegistry())
	migrate.New(nil, nil)

	breeze.NewTemplateEngine(breeze.TemplateConfig{ViewsDir: t.TempDir()})

	if err := video.Mount(router, video.Config{Root: t.TempDir(), Prefix: "/videos"}); err != nil {
		t.Fatalf("mount video: %v", err)
	}

	// i18n only registers on a successful load, and a load needs at least one
	// locale file — so the bundle gets a real one rather than being skipped.
	localeDir := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(localeDir, "en.json"),
		[]byte(`{"hello":"hello"}`),
		0o600,
	); err != nil {
		t.Fatalf("write locale file: %v", err)
	}
	if _, err := breeze.NewI18n(breeze.I18nConfig{Dir: localeDir}); err != nil {
		t.Fatalf("load i18n: %v", err)
	}

	// The websocket probe only registers once an endpoint exists, which is the
	// first WebSocket call — before that there is nothing to report.
	app.WebSocket("/ws", nil)

	// The dashboard last, because Install is also what enables counted
	// diagnostics and the ordering makes that visible.
	cfg := dashboard.DefaultConfig()
	cfg.Username = "wire"
	cfg.Password = "wire"
	dashboard.Install(app, router, cfg)
}
