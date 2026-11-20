package mcp

// network_hardening_test.go — the transport properties added by the security review.
//
// Each test here asserts something an attacker would try, at the place the attempt lands:
// the wire for guessing and probing, the session table for stale ids, and the *http.Server
// for connections that never finish. None of them can pass by the handler merely existing.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"

	"github.com/nelthaarion/breeze/v2/internal/authguard"
)

// remoteScope is a scope narrow enough to satisfy the non-loopback rule.
func remoteScope(t *testing.T) Scope {
	t.Helper()
	scope, err := NewScope(CapIntrospection)
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

// newRemoteNetwork builds a server configured as if bound beyond loopback and returns its
// handler. The bind itself is not opened: the limiter and the token rules key off the
// configured host, which is what is under test, and driving the handler directly lets a
// test choose the peer address a request appears to come from.
func newRemoteNetwork(t *testing.T, token string) (*NetworkServer, http.Handler, *recorder) {
	t.Helper()
	ns, _, err := NewNetworkServer(NewServer("test"), NetworkConfig{
		Mode:  ModeGenerator,
		Host:  "0.0.0.0",
		Token: token,
		Scope: remoteScope(t),
	})
	if err != nil {
		t.Fatalf("NewNetworkServer: %v", err)
	}
	rec := &recorder{}
	ns.SetLogger(rec)
	return ns, ns.Handler(), rec
}

// publicPeer is an address the throttle treats as an internet client.
const publicPeer = "203.0.113.7:4444"

// directCall sends one request to h as if it came from remote.
func directCall(t *testing.T, h http.Handler, method, path, remote, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	payload := ""
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		payload = string(b)
	}
	req := httptest.NewRequest(method, path, strings.NewReader(payload))
	req.RemoteAddr = remote
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set(protocolHeader, protocolVersion)
	if token != "" {
		req.Header.Set("Authorization", bearerPrefix+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestRemoteBindRefusesAWeakToken(t *testing.T) {
	for _, token := range []string{"secret", "devtoken", strings.Repeat("a", authguard.MinRemoteTokenLength-1), "has a space in it, long enough"} {
		_, _, err := NewNetworkServer(NewServer("test"), NetworkConfig{
			Mode: ModeGenerator, Host: "0.0.0.0", Token: token, Scope: remoteScope(t),
		})
		if err == nil {
			t.Errorf("token %q was accepted on a non-loopback bind; the network can reach this listener and the "+
				"token is the only credential", token)
			continue
		}
		if strings.Contains(err.Error(), token) {
			t.Errorf("the refusal echoes the token it refused: %v", err)
		}
	}

	strong := strings.Repeat("k", 32)
	if _, _, err := NewNetworkServer(NewServer("test"), NetworkConfig{
		Mode: ModeGenerator, Host: "0.0.0.0", Token: strong, Scope: remoteScope(t),
	}); err != nil {
		t.Errorf("a 32-character token was refused on a non-loopback bind: %v", err)
	}
}

func TestLoopbackStillAcceptsAShortDevelopmentToken(t *testing.T) {
	// Unchanged behaviour, asserted so that tightening the remote rule cannot quietly
	// break every local setup: on loopback the bind is the first lock.
	if _, _, err := NewNetworkServer(NewServer("test"), NetworkConfig{
		Mode: ModeGenerator, Token: "devtoken",
	}); err != nil {
		t.Fatalf("a short token on loopback was refused: %v", err)
	}
}

func TestRemoteBindLocksOutAGuessingClient(t *testing.T) {
	token := strings.Repeat("g", 32)
	ns, h, rec := newRemoteNetwork(t, token)
	limiter := ns.limiter
	if limiter == nil {
		t.Fatal("a non-loopback server has no authentication limiter")
	}

	for i := 1; i <= limiter.MaxFailures; i++ {
		w := directCall(t, h, http.MethodPost, DefaultEndpointPath, publicPeer, "wrong-token-"+strings.Repeat("x", i), initializeRequest(i))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("guess %d: status = %d, want 401", i, w.Code)
		}
	}

	// The next request carries the RIGHT token and is still refused: a lockout that
	// answered a correct guess would let the attacker read the result off the status.
	w := directCall(t, h, http.MethodPost, DefaultEndpointPath, publicPeer, token, initializeRequest(99))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("correct token during a lockout: status = %d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("the 429 carries no Retry-After, so a well-behaved client cannot back off")
	}
	if strings.Contains(w.Body.String(), token) {
		t.Error("the refusal contains the token")
	}

	// /mcp/features is another oracle for the same token and must share the lockout.
	fw := directCall(t, h, http.MethodGet, FeaturesPath, publicPeer, token, nil)
	if fw.Code != http.StatusTooManyRequests {
		t.Errorf("GET %s during a lockout: status = %d, want 429 — the second endpoint is a way around the throttle",
			FeaturesPath, fw.Code)
	}

	// A different public client is unaffected by this one's lockout.
	other := directCall(t, h, http.MethodPost, DefaultEndpointPath, "198.51.100.9:1", token, initializeRequest(7))
	if other.Code != http.StatusOK {
		t.Errorf("an unrelated client was refused during someone else's lockout: status = %d", other.Code)
	}

	var throttled int
	for _, ev := range rec.all() {
		if ev.Kind == EventTransportRefusal && ev.Reason == ReasonAuthThrottled && ev.Status == http.StatusTooManyRequests {
			throttled++
			if ev.Remote == "" {
				t.Error("a throttled request was logged without the peer address")
			}
		}
	}
	if throttled < 2 {
		t.Errorf("%d throttle events logged, want one per refused request (2)", throttled)
	}
}

func TestRemoteBindDoesNotPunishACorrectClient(t *testing.T) {
	token := strings.Repeat("c", 32)
	ns, h, _ := newRemoteNetwork(t, token)

	// A few typos followed by the right token: well inside the allowance.
	for i := 0; i < ns.limiter.MaxFailures-1; i++ {
		directCall(t, h, http.MethodPost, DefaultEndpointPath, publicPeer, "typo", initializeRequest(i))
	}
	if w := directCall(t, h, http.MethodPost, DefaultEndpointPath, publicPeer, token, initializeRequest(100)); w.Code != http.StatusOK {
		t.Fatalf("a client that stayed inside the allowance was refused: status = %d", w.Code)
	}
	// A success clears the count, so the typos do not accumulate across periods of use.
	for i := 0; i < ns.limiter.MaxFailures-1; i++ {
		directCall(t, h, http.MethodPost, DefaultEndpointPath, publicPeer, "typo", initializeRequest(i))
	}
	if w := directCall(t, h, http.MethodPost, DefaultEndpointPath, publicPeer, token, initializeRequest(101)); w.Code != http.StatusOK {
		t.Fatalf("failures were not cleared by an intervening success: status = %d", w.Code)
	}
}

func TestRemoteBindNeverLocksOutAProxyOrBridgePeer(t *testing.T) {
	// Behind a reverse proxy, a container runtime's bridge or an overlay network, every
	// client shares one address. Counting failures per address there would let anyone who
	// can reach the proxy lock everyone out with ten wrong tokens — turning the throttle
	// into the denial of service it exists to prevent.
	token := strings.Repeat("p", 32)
	_, h, _ := newRemoteNetwork(t, token)

	for _, peer := range []string{"127.0.0.1:5000", "[::1]:5000", "10.1.2.3:1", "172.17.0.1:1", "192.168.1.20:1", "100.64.0.5:1", "[fd12:3456::1]:1"} {
		for i := 0; i < 50; i++ {
			if w := directCall(t, h, http.MethodPost, DefaultEndpointPath, peer, "wrong", initializeRequest(i)); w.Code != http.StatusUnauthorized {
				t.Fatalf("%s attempt %d: status = %d, want 401 (never 429)", peer, i, w.Code)
			}
		}
		if w := directCall(t, h, http.MethodPost, DefaultEndpointPath, peer, token, initializeRequest(1000)); w.Code != http.StatusOK {
			t.Fatalf("%s: the right token was refused after wrong ones: status = %d — a shared address was locked out", peer, w.Code)
		}
	}
}

func TestLoopbackIsNotThrottled(t *testing.T) {
	// On loopback a lockout would only let any local process shut the legitimate client
	// out of its own control port, so none is applied.
	ns, srv, token := newTestNetwork(t)
	if ns.limiter != nil {
		t.Fatal("a loopback server has an authentication limiter")
	}
	for i := 0; i < 50; i++ {
		if resp := post(t, srv, "wrong", "", initializeRequest(i)); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401", i, resp.StatusCode)
		}
	}
	if resp := post(t, srv, token, "", initializeRequest(1000)); resp.StatusCode != http.StatusOK {
		t.Fatalf("the right token was refused after repeated wrong ones: status = %d", resp.StatusCode)
	}
}

func TestSessionsExpireWhenIdle(t *testing.T) {
	ns, srv, token := newTestNetwork(t)
	session := handshake(t, srv, token)
	toolsList := map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": map[string]any{}}

	if resp := post(t, srv, token, session, toolsList); resp.StatusCode != http.StatusOK {
		t.Fatalf("a fresh session was refused: %d", resp.StatusCode)
	}

	// Age the session past its lifetime.
	ns.mu.Lock()
	ns.sessions[session] = time.Now().Add(-maxSessionAge - time.Minute)
	ns.mu.Unlock()

	resp := post(t, srv, token, session, toolsList)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("an expired session was accepted: status = %d, want 404", resp.StatusCode)
	}
	ns.mu.Lock()
	_, stillThere := ns.sessions[session]
	ns.mu.Unlock()
	if stillThere {
		t.Error("an expired session was refused but left in the table")
	}
	// The documented recovery works.
	if again := handshake(t, srv, token); again == session {
		t.Error("a new handshake returned the expired id")
	}
}

func TestSessionUseRefreshesItsLifetime(t *testing.T) {
	ns, srv, token := newTestNetwork(t)
	session := handshake(t, srv, token)
	toolsList := map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": map[string]any{}}

	// Almost expired, then used: it must live on, because the lifetime is idle time.
	ns.mu.Lock()
	ns.sessions[session] = time.Now().Add(-maxSessionAge + time.Minute)
	ns.mu.Unlock()
	if resp := post(t, srv, token, session, toolsList); resp.StatusCode != http.StatusOK {
		t.Fatalf("a session inside its lifetime was refused: %d", resp.StatusCode)
	}
	ns.mu.Lock()
	age := time.Since(ns.sessions[session])
	ns.mu.Unlock()
	if age > time.Minute {
		t.Errorf("using a session did not refresh it: last used %v ago", age)
	}
}

func TestExpiredSessionsAreDroppedBeforeLiveOnesAreEvicted(t *testing.T) {
	ns, _, _ := newTestNetwork(t)

	old := time.Now().Add(-maxSessionAge - time.Hour)
	ns.mu.Lock()
	for i := 0; i < maxSessions; i++ {
		ns.sessions[strings.Repeat("e", 8)+time.Duration(i).String()] = old
	}
	ns.mu.Unlock()

	live := "live-session-id-0123456789abcdef"
	ns.rememberSession(live)

	ns.mu.Lock()
	n := len(ns.sessions)
	_, kept := ns.sessions[live]
	ns.mu.Unlock()
	if !kept {
		t.Fatal("the new session was not recorded")
	}
	if n > 2 {
		t.Errorf("%d sessions remain; the expired ones should have been dropped when the table filled", n)
	}
}

func TestFeaturesEndpointLogsItsRefusals(t *testing.T) {
	// /mcp/features refused silently, so probing it left no trace. A guessing run against
	// it must be as visible as one against the protocol endpoint.
	ns, srv, _ := newTestNetwork(t)
	rec := &recorder{}
	ns.SetLogger(rec)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+FeaturesPath, nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	ev := rec.firstOfKind(t, EventTransportRefusal)
	if ev.Status != http.StatusUnauthorized || ev.Reason != ReasonNoToken {
		t.Errorf("logged %+v, want a 401 %q refusal", ev, ReasonNoToken)
	}

	// And a foreign Origin, which must be logged without quoting the caller's string.
	req, _ = http.NewRequest(http.MethodGet, srv.URL+FeaturesPath, nil)
	req.Header.Set("Origin", "https://evil.example")
	resp, err = srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	for _, e := range rec.all() {
		if strings.Contains(e.Reason, "evil.example") || strings.Contains(e.Tool, "evil.example") {
			t.Errorf("a log event carries the caller's Origin: %+v", e)
		}
	}
}

func TestListenBoundsHeadersAndIdleConnections(t *testing.T) {
	ns, _, _ := newTestNetwork(t)
	if err := ns.Listen(NetworkConfig{Mode: ModeGenerator}); err != nil {
		t.Fatal(err)
	}
	defer ns.Close()

	if ns.http.ReadHeaderTimeout <= 0 {
		t.Error("no ReadHeaderTimeout: a peer can open a connection and never send a request")
	}
	if ns.http.IdleTimeout <= 0 {
		t.Error("no IdleTimeout: a peer can finish one request and hold the connection open forever")
	}
	if ns.http.MaxHeaderBytes <= 0 || ns.http.MaxHeaderBytes > 256<<10 {
		t.Errorf("MaxHeaderBytes = %d; want a small positive bound", ns.http.MaxHeaderBytes)
	}
	// A server-wide ReadTimeout would be wrong, not merely absent: see bodyReadTimeout.
	if ns.http.ReadTimeout != 0 {
		t.Errorf("ReadTimeout = %v; a server-wide read deadline cancels long tool calls", ns.http.ReadTimeout)
	}
	if ns.http.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v; it would cut a long verify_project response off", ns.http.WriteTimeout)
	}
}

func TestTokenComparisonIgnoresLength(t *testing.T) {
	// Behavioural, not timing: a token that is a strict prefix of, or an extension of, the
	// real one is refused like any other wrong token.
	ns, srv, token := newTestNetwork(t)
	_ = ns
	for _, presented := range []string{token[:len(token)-1], token + "x", "", " "} {
		resp := post(t, srv, presented, "", initializeRequest(1))
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("presented %q: status = %d, want 401", presented, resp.StatusCode)
		}
	}
}
