package mcp

// embedded_confine_test.go — an embedded generator-mode endpoint must come up confined.
//
// Every standalone entrypoint installs a workspace policy before serving. An embedded one
// has no entrypoint of its own, and the installer is unreachable from outside the module,
// so before this was fixed an embed in ModeGenerator served the filesystem tools against
// any path on the host — the only deployment of them with no jail, and the one that
// listens on a socket.

import (
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nelthaarion/go-json-plus"
)

// resetConfinement puts the process-wide policy back to "nobody has decided" for one test
// and restores whatever was there afterwards, so these tests neither depend on nor leak
// into the rest of the suite.
func resetConfinement(t *testing.T) {
	t.Helper()
	prevPolicy := confinement.Load()
	prevDecided := confinementDecided.Load()
	confinement.Store(nil)
	confinementDecided.Store(false)
	t.Cleanup(func() {
		confinement.Store(prevPolicy)
		confinementDecided.Store(prevDecided)
	})
}

// evalDir resolves symlinks so two spellings of one directory compare equal.
func evalDir(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(resolved)
}

func jsonString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// callToolRaw sends one tools/call through the endpoint's own JSON-RPC server, the way a
// network caller's request would reach it, and returns the raw reply.
func callToolRaw(ns *NetworkServer, name, argsJSON string) string {
	req := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + name + `","arguments":` + argsJSON + `}}`
	return string(ns.rpc.Handle([]byte(req)))
}

func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()
	return port
}

func startGeneratorEmbed(t *testing.T, cfg InProcessConfig) *NetworkServer {
	t.Helper()
	cfg.Mode = ModeGenerator
	cfg.AllowWorkspaceTools = true
	cfg.Port = freeLoopbackPort(t)
	ns, _, err := NewInProcess("test", cfg)
	if err != nil {
		t.Fatalf("NewInProcess: %v", err)
	}
	t.Cleanup(func() { _ = ns.Close() })
	return ns
}

func TestEmbeddedGeneratorEndpointIsConfinedToTheWorkingDirectoryByDefault(t *testing.T) {
	resetConfinement(t)
	home := t.TempDir()
	elsewhere := t.TempDir()
	t.Chdir(home)

	ns := startGeneratorEmbed(t, InProcessConfig{})

	roots := WorkspaceRoots()
	if len(roots) != 1 {
		t.Fatalf("roots = %v; an embedded generator endpoint with no Workspace must be confined to the working directory", roots)
	}
	if got, want := evalDir(t, roots[0]), evalDir(t, home); got != want {
		t.Fatalf("confined to %s, want the working directory %s", got, want)
	}

	if _, err := resolvePath(elsewhere); err == nil {
		t.Fatalf("a path outside the workspace was accepted: %s", elsewhere)
	}
	if _, err := resolvePath(filepath.Join(home, "project")); err != nil {
		t.Fatalf("a path inside the workspace was refused: %v", err)
	}

	// And through a tool, which is what a network caller would actually use.
	out := callToolRaw(ns, "breeze_verify_project", `{"path":`+jsonString(t, elsewhere)+`}`)
	if !strings.Contains(out, "outside") {
		t.Errorf("breeze_verify_project on a path outside the workspace was not refused as such: %s", out)
	}
}

func TestEmbeddedGeneratorEndpointHonoursAnExplicitWorkspace(t *testing.T) {
	resetConfinement(t)
	work := t.TempDir()
	other := t.TempDir()

	startGeneratorEmbed(t, InProcessConfig{Workspace: []string{work}})

	if _, err := resolvePath(work); err != nil {
		t.Errorf("the configured workspace was refused: %v", err)
	}
	if _, err := resolvePath(other); err == nil {
		t.Error("a directory outside the configured workspace was accepted")
	}
}

func TestEmbeddingDoesNotOverrideADecisionAlreadyMade(t *testing.T) {
	t.Run("explicit-unconfine-stays", func(t *testing.T) {
		resetConfinement(t)
		t.Chdir(t.TempDir())
		Unconfine() // a sandbox that is already the boundary, chosen on purpose
		startGeneratorEmbed(t, InProcessConfig{})
		if roots := WorkspaceRoots(); roots != nil {
			t.Errorf("an explicit Unconfine was overridden by embedding: roots = %v", roots)
		}
	})
	t.Run("installed-policy-stays", func(t *testing.T) {
		resetConfinement(t)
		mine := t.TempDir()
		t.Chdir(t.TempDir())
		if err := ConfineToWorkspace(mine); err != nil {
			t.Fatal(err)
		}
		startGeneratorEmbed(t, InProcessConfig{})
		roots := WorkspaceRoots()
		if len(roots) != 1 || evalDir(t, roots[0]) != evalDir(t, mine) {
			t.Errorf("roots = %v, want the policy the process installed (%s)", roots, mine)
		}
	})
}

func TestWorkspaceOnAnAppRuntimeEmbedIsAnError(t *testing.T) {
	resetConfinement(t)
	_, _, err := NewInProcess("test", InProcessConfig{
		Mode: ModeAppRuntime, Port: freeLoopbackPort(t), Workspace: []string{t.TempDir()},
	})
	if err == nil {
		t.Fatal("Workspace on an app-runtime server was silently accepted; it confines nothing there")
	}
	if !strings.Contains(err.Error(), "Workspace") {
		t.Errorf("the error does not name the offending setting: %v", err)
	}
}

func TestAppRuntimeEmbedDoesNotTouchConfinement(t *testing.T) {
	// App-runtime registers no filesystem tools, so it must not install a policy on the
	// host application's behalf: that is process-wide state it has no reason to change.
	resetConfinement(t)
	ns, _, err := NewInProcess("test", InProcessConfig{Mode: ModeAppRuntime, Port: freeLoopbackPort(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ns.Close() })
	if confinementDecided.Load() || confinement.Load() != nil {
		t.Error("an app-runtime embed changed the process's confinement")
	}
}
