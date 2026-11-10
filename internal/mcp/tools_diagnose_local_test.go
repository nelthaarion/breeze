package mcp_test

// Tests for an embedded server answering breeze_diagnose_service about its own process.
//
// The fixture service and the embedded server live in this one test process, so they read
// one diagnostic registry. That is what makes the drift test possible: the same registry,
// asked once through the dashboard's HTTP endpoint and once directly, has to give the same
// answer, or the second copy of the rules has quietly become a different tool.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/nelthaarion/breeze/v2/internal/mcp"
)

// embeddedServer is built the way the real endpoint builds its server. It must not be
// NewInProcessServer: that is a different constructor the endpoint never calls, and a test
// that used it proved a behaviour no running application had.
func embeddedServer() *mcp.Server {
	srv, err := mcp.NewEmbeddedServer("test", mcp.ModeAppRuntime)
	if err != nil {
		panic(err)
	}
	return srv
}

// subsystemStatuses reads a diagnose result as name -> status.
func subsystemStatuses(t *testing.T, got map[string]any) map[string]string {
	t.Helper()

	out := map[string]string{}
	for _, row := range asRows(t, got["subsystems"]) {
		name, _ := row["subsystem"].(string)
		status, _ := row["status"].(string)
		out[name] = status
	}
	return out
}

func TestEmbeddedServerDiagnosesItsOwnProcessWithoutAServiceURL(t *testing.T) {
	got, summary, isErr := callLiveToolOn(t, embeddedServer(), "breeze_diagnose_service", map[string]any{})
	if isErr {
		t.Fatalf("breeze_diagnose_service with no service_url failed on an embedded server: %s", summary)
	}

	if got["service_url"] != "(this process)" {
		t.Errorf("service_url = %v, want the result labelled as this process", got["service_url"])
	}
	if total, _ := got["total"].(float64); total < 5 {
		t.Errorf("total = %v, want the process's registered subsystems", got["total"])
	}
	// The HTTP client registered this probe at init in every process that links it.
	if _, ok := subsystemStatuses(t, got)["client"]; !ok {
		t.Errorf("the client subsystem is not in the answer: %v", subsystemStatuses(t, got))
	}
}

// TestEmbeddedDiagnosticsMatchWhatTheDashboardServes: the dashboard builds its diagnostics
// endpoint from this registry and the embedded server builds the same envelope itself, so
// the two are held together by a test. Counts, the names reported, each one's status and
// the notes a reader is given must agree.
func TestEmbeddedDiagnosticsMatchWhatTheDashboardServes(t *testing.T) {
	f := startFixture(t)

	viaDashboard, summary, isErr := callLiveTool(t, "breeze_diagnose_service", creds(f.url))
	if isErr {
		t.Fatalf("diagnose over HTTP failed: %s", summary)
	}
	direct, summary, isErr := callLiveToolOn(t, embeddedServer(), "breeze_diagnose_service", map[string]any{})
	if isErr {
		t.Fatalf("diagnose in-process failed: %s", summary)
	}

	for _, key := range []string{"total", "ok", "degraded", "off", "unknown", "counting"} {
		if viaDashboard[key] != direct[key] {
			t.Errorf("%s: the dashboard says %v, the embedded server says %v", key, viaDashboard[key], direct[key])
		}
	}

	httpStatuses, localStatuses := subsystemStatuses(t, viaDashboard), subsystemStatuses(t, direct)
	if fmt.Sprint(httpStatuses) != fmt.Sprint(localStatuses) {
		var onlyHTTP, onlyLocal, differ []string
		for n, s := range httpStatuses {
			if ls, ok := localStatuses[n]; !ok {
				onlyHTTP = append(onlyHTTP, n)
			} else if ls != s {
				differ = append(differ, n+": "+s+" vs "+ls)
			}
		}
		for n := range localStatuses {
			if _, ok := httpStatuses[n]; !ok {
				onlyLocal = append(onlyLocal, n)
			}
		}
		sort.Strings(onlyHTTP)
		sort.Strings(onlyLocal)
		sort.Strings(differ)
		t.Errorf("the two disagree about the subsystems: only over HTTP %v, only in-process %v, differing %v",
			onlyHTTP, onlyLocal, differ)
	}

	if fmt.Sprint(viaDashboard["notes"]) != fmt.Sprint(direct["notes"]) {
		t.Errorf("the notes differ:\n  dashboard: %v\n  in-process: %v", viaDashboard["notes"], direct["notes"])
	}
}

func TestEmbeddedServerStillReachesAnotherServiceOverHTTP(t *testing.T) {
	f := startFixture(t)

	got, summary, isErr := callLiveToolOn(t, embeddedServer(), "breeze_diagnose_service", creds(f.url))
	if isErr {
		t.Fatalf("an embedded server could not diagnose another service: %s", summary)
	}
	if got["service_url"] != f.url {
		t.Errorf("service_url = %v, want %s: given a URL the tool reads that service, not this process", got["service_url"], f.url)
	}
}

func TestEmbeddedDiagnoseOneSubsystemAndAnUnknownOne(t *testing.T) {
	_, summary, isErr := callLiveToolOn(t, embeddedServer(), "breeze_diagnose_service", map[string]any{"subsystem": "client"})
	if isErr || !strings.HasPrefix(summary, "client:") {
		t.Errorf("one subsystem: summary = %q, isErr = %v", summary, isErr)
	}

	_, summary, isErr = callLiveToolOn(t, embeddedServer(), "breeze_diagnose_service", map[string]any{"subsystem": "nonesuch"})
	if !isErr {
		t.Fatal("an unknown subsystem was answered as if it existed")
	}
	for _, want := range []string{"no subsystem named", "nonesuch", "client"} {
		if !strings.Contains(summary, want) {
			t.Errorf("the refusal %q does not mention %q", summary, want)
		}
	}
}

// TestOnlyAnEmbeddedServerReadsItsOwnProcess: a standalone breeze-mcp has a registry of its
// own that describes nothing the caller cares about. Handing it the same shortcut would
// answer "what is wrong with my service" with the state of the tool asked.
func TestOnlyAnEmbeddedServerReadsItsOwnProcess(t *testing.T) {
	_, summary, isErr := callLiveTool(t, "breeze_diagnose_service", map[string]any{})
	if !isErr {
		t.Fatalf("a standalone server answered with no service_url: %s", summary)
	}

	required := func(srv *mcp.Server) []string {
		out := srv.RPCServer().Handle([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		var resp struct {
			Result struct {
				Tools []struct {
					Name        string `json:"name"`
					InputSchema struct {
						Required []string `json:"required"`
					} `json:"inputSchema"`
				} `json:"tools"`
			} `json:"result"`
		}
		if err := json.Unmarshal(out, &resp); err != nil {
			t.Fatalf("tools/list: %v", err)
		}
		for _, tl := range resp.Result.Tools {
			if tl.Name == "breeze_diagnose_service" {
				return tl.InputSchema.Required
			}
		}
		t.Fatal("breeze_diagnose_service is not listed")
		return nil
	}

	if got := required(mcp.NewServer("test")); len(got) != 1 || got[0] != "service_url" {
		t.Errorf("a standalone server's required arguments = %v, want service_url", got)
	}
	if got := required(embeddedServer()); len(got) != 0 {
		t.Errorf("an embedded server still requires %v, so a caller is told it cannot omit the URL", got)
	}
}
