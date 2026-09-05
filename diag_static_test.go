package breeze

// diag_static_test.go — the static-mount probe.
//
// The probe exists to answer one question that nothing else in the framework could:
// "why is this file 404ing". Two facts answer it — which directory the prefix maps
// to, and whether that directory exists — and neither is recoverable from the
// wildcard route ServeStatic registers. These tests assert both, plus the counting
// caveat, because a report that said "0 files served" without saying whether it was
// measuring would be worse than no report.

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/nelthaarion/breeze/diag"
)

func TestStaticProbeReportsOffWithNoMount(t *testing.T) {
	app := New(NewRouter(), NewEventLoopWorkerPool(runtime.NumCPU()))

	report := app.staticProbe()
	if report.Status != diag.StatusOff {
		t.Fatalf("status = %q, want %q for an application with no static mount",
			report.Status, diag.StatusOff)
	}
	// autoServeRoot defaults to true, so the report must mention the GET / index
	// fallback — otherwise a reader concludes nothing serves files at all, which
	// is wrong in the one case they are most likely to hit.
	if len(report.Notes) == 0 {
		t.Error("no note about the GET / index fallback, which is on by default")
	}
	if report.Detail["auto_serve_root"] != true {
		t.Errorf("auto_serve_root = %v, want true", report.Detail["auto_serve_root"])
	}
}

func TestStaticProbeReportsTheMountAndItsResolvedRoot(t *testing.T) {
	dir := t.TempDir()
	router := NewRouter()
	app := New(router, NewEventLoopWorkerPool(runtime.NumCPU()))
	router.ServeStatic("/public/", dir)

	report := app.staticProbe()
	if report.Status != diag.StatusOK {
		t.Fatalf("status = %q, want %q for an existing root: %s",
			report.Status, diag.StatusOK, report.Summary)
	}

	mounts, ok := report.Detail["mounts"].([]map[string]any)
	if !ok {
		t.Fatalf("mounts is %T, want a list of entries", report.Detail["mounts"])
	}
	if len(mounts) != 1 {
		t.Fatalf("got %d mount(s), want 1", len(mounts))
	}

	// The trailing slash is trimmed at registration, so the reported prefix is the
	// one the route actually matches rather than the one that was typed.
	if mounts[0]["prefix"] != "/public" {
		t.Errorf("prefix = %v, want /public with the trailing slash trimmed", mounts[0]["prefix"])
	}
	if mounts[0]["root_found"] != true {
		t.Errorf("root_found = %v for a directory that exists", mounts[0]["root_found"])
	}
	// root_abs is the point of the whole entry: a relative root that works from
	// the project directory fails under a service manager that starts elsewhere.
	abs, _ := filepath.Abs(dir)
	if mounts[0]["root_abs"] != abs {
		t.Errorf("root_abs = %v, want %v", mounts[0]["root_abs"], abs)
	}
}

// TestStaticProbeIsDegradedWhenARootDoesNotExist is the failure the probe was
// written for: nothing errors at startup, and every request under the prefix 404s.
func TestStaticProbeIsDegradedWhenARootDoesNotExist(t *testing.T) {
	router := NewRouter()
	app := New(router, NewEventLoopWorkerPool(runtime.NumCPU()))
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	router.ServeStatic("/assets", missing)

	report := app.staticProbe()
	if report.Status != diag.StatusDegraded {
		t.Fatalf("status = %q, want %q for a missing root", report.Status, diag.StatusDegraded)
	}
	if len(report.Notes) == 0 {
		t.Error("a missing root produced no note explaining the consequence")
	}
}

// TestStaticProbeReportsAFileAsAMissingRoot covers the mistake of pointing a mount
// at a file: os.Stat succeeds, so only the IsDir check catches it.
func TestStaticProbeReportsAFileAsAMissingRoot(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	router := NewRouter()
	app := New(router, NewEventLoopWorkerPool(runtime.NumCPU()))
	router.ServeStatic("/assets", file)

	if report := app.staticProbe(); report.Status != diag.StatusDegraded {
		t.Errorf("status = %q for a root that is a file, want %q", report.Status, diag.StatusDegraded)
	}
}

// TestStaticProbeSaysWhetherItWasCounting is the ambiguity this framework refuses
// to ship: a zero must never be readable as both "nothing happened" and "nothing
// was measured".
func TestStaticProbeSaysWhetherItWasCounting(t *testing.T) {
	diag.DisableCounters()
	t.Cleanup(diag.DisableCounters)

	router := NewRouter()
	app := New(router, NewEventLoopWorkerPool(runtime.NumCPU()))
	router.ServeStatic("/public", t.TempDir())

	report := app.staticProbe()
	if report.Detail["counting"] != false {
		t.Errorf("counting = %v with the gate closed", report.Detail["counting"])
	}
	found := false
	for _, note := range report.Notes {
		if len(note) > 0 && note[0] == 'C' {
			found = true
		}
	}
	if !found {
		t.Errorf("no note explains that the counts were not measured: %q", report.Notes)
	}

	diag.EnableCounters()
	if report := app.staticProbe(); report.Detail["counting"] != true {
		t.Errorf("counting = %v after EnableCounters", report.Detail["counting"])
	}
}

// TestStaticProbeIsRegisteredByNew keeps the wiring honest: a probe nobody
// registered is a probe nobody can read.
func TestStaticProbeIsRegisteredByNew(t *testing.T) {
	New(NewRouter(), NewEventLoopWorkerPool(runtime.NumCPU()))

	if _, found := diag.Get("static"); !found {
		t.Errorf("no \"static\" probe after breeze.New; registered: %v", diag.Registered())
	}
}
