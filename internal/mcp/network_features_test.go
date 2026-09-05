package mcp

// network_features_test.go — the convenience endpoint, including the part that matters
// most: that it cannot disagree with the handshake.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fetchFeatures calls GET /mcp/features and returns the status and decoded body.
func fetchFeatures(
	t *testing.T,
	srv *httptest.Server,
	token, method string,
) (int, featuresResponse, string) {
	t.Helper()

	req, err := http.NewRequest(method, srv.URL+FeaturesPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	var report featuresResponse
	_ = json.Unmarshal(raw, &report)
	return resp.StatusCode, report, string(raw)
}

// TestFeaturesEndpointRequiresTheToken — the report describes a credential's
// privileges, so an unauthenticated caller must not be able to enumerate them. This is
// the same reason the protocol endpoint is authenticated, applied to the same data.
func TestFeaturesEndpointRequiresTheToken(t *testing.T) {
	_, srv, _ := scopedServer(t, ModeGenerator, UnscopedScope())

	status, _, body := fetchFeatures(t, srv, "", http.MethodGet)
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401; body: %s", status, body)
	}

	status, _, body = fetchFeatures(t, srv, "not-the-token", http.MethodGet)
	if status != http.StatusUnauthorized {
		t.Errorf("a wrong token got %d, want 401; body: %s", status, body)
	}
}

// TestFeaturesEndpointRejectsOtherMethods — GET answers a question; anything else
// implies this endpoint changes something, and it does not.
func TestFeaturesEndpointRejectsOtherMethods(t *testing.T) {
	_, srv, token := scopedServer(t, ModeGenerator, UnscopedScope())

	for _, method := range []string{http.MethodPost, http.MethodDelete, http.MethodPut} {
		status, _, _ := fetchFeatures(t, srv, token, method)
		if status != http.StatusMethodNotAllowed {
			t.Errorf("%s got %d, want 405", method, status)
		}
	}
}

// TestFeaturesEndpointReportsTheScope is the happy path.
func TestFeaturesEndpointReportsTheScope(t *testing.T) {
	scope, err := NewScope(CapFleet, CapRuntime)
	if err != nil {
		t.Fatal(err)
	}
	_, srv, token := scopedServer(t, ModeGenerator, scope)

	status, report, body := fetchFeatures(t, srv, token, http.MethodGet)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", status, body)
	}
	if report.ServerKind != string(ModeGenerator) {
		t.Errorf("server_kind = %q, want %q", report.ServerKind, ModeGenerator)
	}
	if strings.Join(report.Granted, ",") != "fleet,runtime" {
		t.Errorf("granted = %v, want [fleet runtime]", report.Granted)
	}
	if !report.Scoped {
		t.Error("scoped = false for a two-capability token")
	}
	if len(report.Known) != len(KnownCapabilities()) {
		t.Errorf("known = %v, want all %d", report.Known, len(KnownCapabilities()))
	}
	if len(report.Tools) == 0 {
		t.Error("tools is empty; an operator checking a credential learns nothing")
	}
	// Every advertised tool is inside the scope — the tool list is the answer to the
	// question an operator is actually asking.
	for _, name := range report.Tools {
		c, ok := capabilityOf(name)
		if !ok {
			t.Errorf("tools lists unclassified %q", name)
			continue
		}
		if c != CapFleet && c != CapRuntime {
			t.Errorf("tools lists %q (%s), outside the scope", name, c)
		}
	}
	if report.Note == "" {
		t.Error("note is empty; nothing points a reader at the authoritative mechanism")
	}
}

// TestFeaturesEndpointAgreesWithTheHandshake is the reason this endpoint is allowed to
// exist. Two ways to ask one question is a drift hazard; this pins them together, so a
// future change to either has to change both or fail here.
func TestFeaturesEndpointAgreesWithTheHandshake(t *testing.T) {
	scope, err := NewScope(CapPlanning, CapKnowledge)
	if err != nil {
		t.Fatal(err)
	}
	_, srv, token := scopedServer(t, ModeGenerator, scope)

	_, report, _ := fetchFeatures(t, srv, token, http.MethodGet)
	handshake := initializeFor(t, srv, token)

	if handshake.BreezeCapabilities == nil {
		t.Fatal("the handshake carried no capability report to compare against")
	}
	if got, want := strings.Join(report.Granted, ","),
		strings.Join(handshake.BreezeCapabilities.Granted, ","); got != want {
		t.Errorf("granted: endpoint %q, handshake %q", got, want)
	}
	if got, want := strings.Join(report.Known, ","),
		strings.Join(handshake.BreezeCapabilities.Known, ","); got != want {
		t.Errorf("known: endpoint %q, handshake %q", got, want)
	}
	if report.Scoped != handshake.BreezeCapabilities.Scoped {
		t.Errorf(
			"scoped: endpoint %v, handshake %v",
			report.Scoped,
			handshake.BreezeCapabilities.Scoped,
		)
	}
	if report.ServerKind != string(handshake.BreezeServerKind) {
		t.Errorf(
			"server_kind: endpoint %q, handshake %q",
			report.ServerKind,
			handshake.BreezeServerKind,
		)
	}

	// And against tools/list, which is the third place the same decision surfaces.
	_, listed := scopedListTools(t, srv, token)
	if strings.Join(report.Tools, ",") != strings.Join(listed, ",") {
		t.Errorf("tools: endpoint %v, tools/list %v", report.Tools, listed)
	}
}

// TestFeaturesEndpointReportsAppRuntimeKind — mode and scope are separate layers, and
// the report has to show both or it cannot explain why a tool is missing.
func TestFeaturesEndpointReportsAppRuntimeKind(t *testing.T) {
	_, srv, token := scopedServer(t, ModeAppRuntime, UnscopedScope())

	_, report, _ := fetchFeatures(t, srv, token, http.MethodGet)
	if report.ServerKind != string(ModeAppRuntime) {
		t.Errorf("server_kind = %q, want %q", report.ServerKind, ModeAppRuntime)
	}
	// Unscoped, yet generation tools are absent — because mode removed them from the
	// registry entirely. Granted still lists generation: the token would allow it, and
	// this server does not offer it. Conflating the two would hide which layer refused.
	for _, name := range report.Tools {
		if c, _ := capabilityOf(name); c == CapGeneration || c == CapProvisioning {
			t.Errorf("an app-runtime server advertises %q (%s)", name, c)
		}
	}
	if report.Scoped {
		t.Error("scoped = true for a token that was never narrowed")
	}
	if len(report.Granted) != len(KnownCapabilities()) {
		t.Errorf("granted = %v; an unscoped token grants everything even where mode "+
			"registered nothing", report.Granted)
	}
}
