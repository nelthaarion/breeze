package breeze

// mcp_auth_hardening_test.go — what stands between a network peer and the application's
// tagged routes once Auto-MCP is reachable beyond loopback.
//
// The bearer token is the only credential there. These tests pin the three properties the
// security review added around it: a typed-in token is refused on an exposed listener, a
// client that keeps guessing is locked out (even when its next guess is right), and
// nothing about the comparison depends on how long the token is.

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nelthaarion/breeze/v2/internal/authguard"
	"github.com/nelthaarion/breeze/v2/rpc"
)

func autoMCPTestHandler(throttle bool, token string) http.Handler {
	srv := rpc.NewServer(rpc.NewRegistry())
	srv.Register("tools/list", func(ctx *rpc.Context) {
		ctx.Result(map[string]any{"tools": []any{}})
	})
	return newAutoMCPHTTPHandlerFor(srv, token, throttle)
}

func autoMCPCall(h http.Handler, presented string) *httptest.ResponseRecorder {
	return autoMCPCallFrom(h, "203.0.113.7:4444", presented) // a publicly routable peer
}

func autoMCPCallFrom(h http.Handler, remote, presented string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "http://example.test/mcp",
		bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)))
	req.RemoteAddr = remote
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if presented != "" {
		req.Header.Set("Authorization", "Bearer "+presented)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestAutoMCPRefusesATypedTokenOnANonLoopbackBind(t *testing.T) {
	mcpFixture(t)
	app := &Breeze{Router: mcpFixtureRouter}

	const typed = "hunter2hunter2" // 14 characters
	for _, bind := range []string{"0.0.0.0:0", ":0", "[::]:0"} {
		err := app.EnableMCPWithToken(bind, typed)
		if err == nil {
			t.Fatalf("a 14-character token was accepted for %s; the network can reach it and the token is the only credential", bind)
		}
		if strings.Contains(err.Error(), typed) {
			t.Errorf("the refusal for %s echoes the token it refused: %v", bind, err)
		}
	}
	if app.mcpHTTP.Load() != nil {
		t.Error("a listener was started although every bind was refused")
	}
}

func TestAutoMCPAcceptsAStrongTokenOnANonLoopbackBindAndAShortOneOnLoopback(t *testing.T) {
	mcpFixture(t)
	t.Setenv("BREEZE_MCP_TOKEN", "")

	shutdown := func(app *Breeze) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if h := app.mcpHTTP.Load(); h != nil {
			_ = h.Shutdown(ctx)
		}
	}

	strong := &Breeze{Router: mcpFixtureRouter}
	if err := strong.EnableMCPWithToken("0.0.0.0:0", strings.Repeat("s", authguard.MinRemoteTokenLength*2)); err != nil {
		t.Errorf("a strong token was refused on a non-loopback bind: %v", err)
	}
	shutdown(strong)

	local := &Breeze{Router: mcpFixtureRouter}
	if err := local.EnableMCPWithToken("127.0.0.1:0", "devtoken"); err != nil {
		t.Errorf("a short development token was refused on loopback, where the bind is the first lock: %v", err)
	}
	shutdown(local)
}

func TestAutoMCPLocksOutAGuessingClientWhenExposed(t *testing.T) {
	const token = "a-token-that-is-long-enough-0123456789"
	h := autoMCPTestHandler(true, token)

	limit := authguard.NewLimiter().MaxFailures
	for i := 1; i <= limit; i++ {
		if w := autoMCPCall(h, "guess-"+strings.Repeat("x", i)); w.Code != http.StatusUnauthorized {
			t.Fatalf("guess %d: status = %d, want 401", i, w.Code)
		}
	}
	// The right token, during the lockout: still refused, or the attacker reads the
	// answer off the status code.
	w := autoMCPCall(h, token)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("correct token during a lockout: status = %d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("the 429 carries no Retry-After")
	}
	if strings.Contains(w.Body.String(), token) {
		t.Error("the refusal contains the token")
	}
}

func TestAutoMCPDoesNotPunishAClientThatStaysInsideTheAllowance(t *testing.T) {
	const token = "a-token-that-is-long-enough-0123456789"
	h := autoMCPTestHandler(true, token)
	limit := authguard.NewLimiter().MaxFailures

	for round := 0; round < 3; round++ {
		for i := 0; i < limit-1; i++ {
			autoMCPCall(h, "typo")
		}
		if w := autoMCPCall(h, token); w.Code != http.StatusOK {
			t.Fatalf("round %d: a client inside its allowance was refused: status = %d", round, w.Code)
		}
	}
}

func TestAutoMCPNeverLocksOutAProxyOrBridgePeer(t *testing.T) {
	// Behind a reverse proxy or a container bridge every client shares one address, so a
	// per-address lockout there would let anyone lock everyone out with ten wrong tokens.
	const token = "a-token-that-is-long-enough-0123456789"
	h := autoMCPTestHandler(true, token)
	for _, peer := range []string{"127.0.0.1:5000", "10.1.2.3:1", "172.17.0.1:1", "192.168.1.20:1", "100.64.0.5:1"} {
		for i := 0; i < 50; i++ {
			if w := autoMCPCallFrom(h, peer, "wrong"); w.Code != http.StatusUnauthorized {
				t.Fatalf("%s attempt %d: status = %d, want 401 (never 429)", peer, i, w.Code)
			}
		}
		if w := autoMCPCallFrom(h, peer, token); w.Code != http.StatusOK {
			t.Fatalf("%s: the right token was refused after wrong ones: status = %d", peer, w.Code)
		}
	}
}

func TestAutoMCPLoopbackIsNotThrottled(t *testing.T) {
	const token = "devtoken"
	h := autoMCPTestHandler(false, token)
	for i := 0; i < 50; i++ {
		if w := autoMCPCall(h, "wrong"); w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401", i, w.Code)
		}
	}
	if w := autoMCPCall(h, token); w.Code != http.StatusOK {
		t.Fatalf("the right token was refused after repeated wrong ones on loopback: status = %d", w.Code)
	}
}

func TestAutoMCPTokenComparisonIgnoresLength(t *testing.T) {
	const token = "a-token-that-is-long-enough-0123456789"
	h := autoMCPTestHandler(false, token)
	for _, presented := range []string{token[:len(token)-1], token + "x", "x"} {
		if w := autoMCPCall(h, presented); w.Code != http.StatusUnauthorized {
			t.Errorf("presented %q: status = %d, want 401", presented, w.Code)
		}
	}
}
