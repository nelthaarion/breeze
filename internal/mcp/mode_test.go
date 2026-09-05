package mcp

// mode_test.go — Part 9: mode is required, and app-runtime is structurally safe.

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestModeHasNoDefault is the central assertion of Part 9. Every construction path
// must refuse an unset mode, because a server running in a posture nobody selected
// is the failure this exists to prevent.
func TestModeHasNoDefault(t *testing.T) {
	t.Run("ParseMode empty", func(t *testing.T) {
		if _, err := ParseMode(""); err == nil {
			t.Fatal("ParseMode(\"\") was accepted; --mode must have no default")
		}
	})

	t.Run("ParseMode whitespace", func(t *testing.T) {
		// A shell that expands an unset variable produces this, and it must not be
		// treated as a value.
		if _, err := ParseMode("   "); err == nil {
			t.Fatal("ParseMode(\"   \") was accepted")
		}
	})

	t.Run("NewServerForMode unset", func(t *testing.T) {
		if _, err := NewServerForMode("test", ModeUnset); err == nil {
			t.Fatal("NewServerForMode accepted ModeUnset")
		}
	})

	t.Run("NewNetworkServer unset", func(t *testing.T) {
		_, _, err := NewNetworkServer(NewServer("test"), NetworkConfig{})
		if err == nil {
			t.Fatal("NewNetworkServer accepted a config with no Mode")
		}
		if !strings.Contains(err.Error(), "Mode is required") {
			t.Errorf("error does not say Mode is required: %v", err)
		}
	})

	t.Run("NewInProcess unset", func(t *testing.T) {
		_, _, err := NewInProcess("test", InProcessConfig{Port: 1})
		if err == nil {
			t.Fatal("NewInProcess accepted a config with no Mode")
		}
	})
}

// TestModeRejectsUnknownValue covers the typo, which must not fall back to either
// mode. "app_runtime" and "gen" are the two a person actually types.
func TestModeRejectsUnknownValue(t *testing.T) {
	for _, bad := range []string{"app_runtime", "gen", "runtime", "gENERATOR"} {
		if mode, err := ParseMode(bad); err == nil {
			t.Errorf("ParseMode(%q) was accepted as %q", bad, mode)
		}
	}
}

// TestModeAcceptsBothValidValues is the other direction: the two documented
// spellings must work, exactly as written in the flag help and the docs.
func TestModeAcceptsBothValidValues(t *testing.T) {
	for raw, want := range map[string]ServerMode{
		"generator":   ModeGenerator,
		"app-runtime": ModeAppRuntime,
	} {
		got, err := ParseMode(raw)
		if err != nil {
			t.Errorf("ParseMode(%q): %v", raw, err)
			continue
		}
		if got != want {
			t.Errorf("ParseMode(%q) = %q, want %q", raw, got, want)
		}
	}
}

// TestAppRuntimeRegistryExcludesMutatingTools is the structural guarantee, and the
// reason Part 9 is not merely a scope check: the tools are absent from the
// registry, so no token, no misconfiguration and no argument trick can reach one.
func TestAppRuntimeRegistryExcludesMutatingTools(t *testing.T) {
	server, err := NewServerForMode("test", ModeAppRuntime)
	if err != nil {
		t.Fatal(err)
	}

	// Named explicitly rather than derived, so that reclassifying a tool in scope.go
	// cannot silently make this test stop checking the thing it is named for.
	forbidden := []string{
		"breeze_new", "breeze_generate", "breeze_add",
		"provision_service", "provision_fleet", "deprovision_service",
		"list_provisioned_services",
		"breeze_plan_project", "breeze_diff_config",
		"breeze_begin_change_set", "breeze_stage_call", "breeze_commit_change_set",
		"breeze_generate_llms_txt",
		"breeze_verify_project", "breeze_run_benchmarks", "breeze_get_test_coverage",
		"breeze_check_idioms",
	}
	for _, name := range forbidden {
		if _, present := server.tools[name]; present {
			t.Errorf("app-runtime registry contains %q; it must not be registered at all", name)
		}
	}

	// And the whole registry, so a mutating tool added later is caught even if
	// nobody adds it to the list above.
	for name := range server.tools {
		if mutating(name) {
			t.Errorf("app-runtime registry contains mutating tool %q", name)
		}
	}
}

// TestAppRuntimeStillServesLiveTools guards the opposite failure: a filter so
// aggressive that the mode is useless. An app-runtime server with no tools would
// pass the test above and be worthless.
func TestAppRuntimeStillServesLiveTools(t *testing.T) {
	server, err := NewServerForMode("test", ModeAppRuntime)
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"breeze_get_routes", "breeze_get_performance", "breeze_get_recent_errors",
		"breeze_get_logs", "breeze_query_openapi",
	} {
		if _, present := server.tools[want]; !present {
			t.Errorf("app-runtime registry is missing %q, which is what the mode is for", want)
		}
	}
	if len(server.tools) == 0 {
		t.Fatal("app-runtime registry is empty")
	}
}

// TestGeneratorModeKeepsEverything asserts the generator mode is not filtered: it
// is the full toolchain, and a mode that quietly dropped a tool would look like a
// missing feature.
func TestGeneratorModeKeepsEverything(t *testing.T) {
	full := NewServer("test")
	gen, err := NewServerForMode("test", ModeGenerator)
	if err != nil {
		t.Fatal(err)
	}

	if len(gen.tools) != len(full.tools) {
		t.Errorf("generator mode has %d tools, the unfiltered registry has %d",
			len(gen.tools), len(full.tools))
	}
	for name := range full.tools {
		if _, present := gen.tools[name]; !present {
			t.Errorf("generator mode is missing %q", name)
		}
	}
}

// TestInitializeReportsServerKind checks the handshake field an agent reads to
// learn which kind of server it reached. Asserted through the real dispatcher, on
// the wire, because that is what a client sees.
func TestInitializeReportsServerKind(t *testing.T) {
	for _, mode := range []ServerMode{ModeGenerator, ModeAppRuntime} {
		server, err := NewServerForMode("test", mode)
		if err != nil {
			t.Fatal(err)
		}

		raw := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":` +
			`{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
		out := server.RPCServer().Handle(raw)

		var reply struct {
			Result initializeResult `json:"result"`
		}
		if err := json.Unmarshal(out, &reply); err != nil {
			t.Fatalf("initialize reply is not JSON: %v\n%s", err, out)
		}
		if reply.Result.BreezeServerKind != mode {
			t.Errorf("breezeServerKind = %q, want %q", reply.Result.BreezeServerKind, mode)
		}

		// The field must be spelled exactly this way on the wire: a client keys on
		// the string, so a renamed JSON tag is a breaking change a Go-level
		// comparison would not notice.
		if !strings.Contains(string(out), `"breezeServerKind":"`+string(mode)+`"`) {
			t.Errorf("wire form does not carry breezeServerKind=%q:\n%s", mode, out)
		}
	}
}

// TestModeMismatchIsRefused covers the one way the handshake could lie: a server
// built for one mode handed to a transport configured for the other.
func TestModeMismatchIsRefused(t *testing.T) {
	appServer, err := NewServerForMode("test", ModeAppRuntime)
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = NewNetworkServer(appServer, NetworkConfig{Mode: ModeGenerator})
	if err == nil {
		t.Fatal("an app-runtime server was served by a generator-mode transport")
	}
	if !strings.Contains(err.Error(), "must match") {
		t.Errorf("error does not explain the mismatch: %v", err)
	}
}

// TestAppRuntimeRefusesWorkspaceOptIn — AllowWorkspaceTools in app-runtime mode is
// a contradiction, and silently ignoring it would leave the caller believing they
// had enabled something.
func TestAppRuntimeRefusesWorkspaceOptIn(t *testing.T) {
	_, _, err := NewInProcess("test", InProcessConfig{
		Mode:                ModeAppRuntime,
		Port:                1,
		AllowWorkspaceTools: true,
	})
	if err == nil {
		t.Fatal("AllowWorkspaceTools was accepted on an app-runtime server")
	}
	if !strings.Contains(err.Error(), "AllowWorkspaceTools") {
		t.Errorf("error does not name the offending field: %v", err)
	}
}
