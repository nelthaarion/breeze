package breeze

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/nelthaarion/breeze/v2/rpc"
)

const (
	autoMCPProtocolLegacy  = "2024-11-05"
	autoMCPProtocol2025Mar = "2025-03-26"
	autoMCPProtocol2025Jun = "2025-06-18"
	autoMCPProtocol2025Nov = "2025-11-25"
	autoMCPProtocolModern  = "2026-07-28"
	autoMCPMaxBodyBytes    = 8 << 20
	autoMCPHeaderMethod    = "Mcp-Method"
	autoMCPHeaderName      = "Mcp-Name"
	autoMCPHeaderVersion   = "MCP-Protocol-Version"
	autoMCPErrMismatch     = -32020
)

type autoMCPHTTPHandler struct {
	rpc   *rpc.Server
	token string
}

func newAutoMCPHTTPHandler(rpcSrv *rpc.Server, token string) http.Handler {
	h := &autoMCPHTTPHandler{rpc: rpcSrv, token: token}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", h.serve)
	return mux
}

func (h *autoMCPHTTPHandler) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		autoMCPError(w, http.StatusMethodNotAllowed, -32000, "MCP endpoint accepts POST only")
		return
	}
	if !h.originAllowed(r) {
		autoMCPError(w, http.StatusForbidden, -32000, "request Origin is not allowed")
		return
	}
	if !h.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="breeze-automcp"`)
		autoMCPError(w, http.StatusUnauthorized, -32000, "a bearer token is required")
		return
	}
	if !acceptsJSON(r.Header.Get("Accept")) {
		autoMCPError(w, http.StatusNotAcceptable, -32000, "Accept must allow application/json")
		return
	}
	ct := strings.ToLower(strings.TrimSpace(strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0]))
	if ct != "application/json" {
		autoMCPError(w, http.StatusUnsupportedMediaType, -32000, "Content-Type application/json is required")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, autoMCPMaxBodyBytes+1))
	if err != nil {
		autoMCPError(w, http.StatusBadRequest, -32000, "request body could not be read")
		return
	}
	if len(body) > autoMCPMaxBodyBytes {
		autoMCPError(w, http.StatusRequestEntityTooLarge, -32000, fmt.Sprintf("request exceeds %d bytes", autoMCPMaxBodyBytes))
		return
	}

	version := strings.TrimSpace(r.Header.Get(autoMCPHeaderVersion))
	if version == "" {
		version = autoMCPProtocolLegacy
	}
	if version != autoMCPProtocolLegacy && version != autoMCPProtocol2025Mar && version != autoMCPProtocol2025Jun && version != autoMCPProtocol2025Nov && version != autoMCPProtocolModern {
		autoMCPError(w, http.StatusBadRequest, -32000, "unsupported MCP protocol version")
		return
	}
	modern := version == autoMCPProtocolModern
	if modern {
		if err := validateAutoMCPModernHeaders(r.Header, body); err != nil {
			autoMCPError(w, http.StatusBadRequest, autoMCPErrMismatch, err.Error())
			return
		}
	}

	out := h.rpc.Handle(body)
	if len(out) > 0 {
		out = decorateAutoMCPResponse(out, body, version)
	}
	if len(out) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
}

func (h *autoMCPHTTPHandler) originAllowed(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if !isLoopbackMCPHost(host) {
		for _, raw := range strings.Split(os.Getenv("BREEZE_MCP_ALLOWED_ORIGIN"), ",") {
			if strings.EqualFold(strings.TrimRight(strings.TrimSpace(raw), "/"), origin) {
				return true
			}
		}
		return false
	}
	return strings.EqualFold(u.Scheme, "http") || strings.EqualFold(u.Scheme, "https")
}

func isLoopbackMCPHost(host string) bool {
	h := strings.Trim(strings.TrimSpace(host), "[]")
	if h == "localhost" {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func (h *autoMCPHTTPHandler) authorized(r *http.Request) bool {
	raw := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(raw) < len("Bearer ") || !strings.EqualFold(raw[:len("Bearer ")], "Bearer ") {
		return false
	}
	presented := strings.TrimSpace(raw[len("Bearer "):])
	return subtle.ConstantTimeCompare([]byte(presented), []byte(h.token)) == 1
}

func acceptsJSON(v string) bool {
	if strings.TrimSpace(v) == "" {
		return true
	}
	for _, part := range strings.Split(v, ",") {
		items := strings.Split(part, ";")
		media := strings.TrimSpace(strings.ToLower(items[0]))
		q := 1.0
		for _, param := range items[1:] {
			kv := strings.SplitN(strings.TrimSpace(param), "=", 2)
			if len(kv) == 2 && strings.EqualFold(strings.TrimSpace(kv[0]), "q") {
				var err error
				if _, err = fmt.Sscanf(strings.TrimSpace(kv[1]), "%f", &q); err != nil {
					q = 0
				}
			}
		}
		if q <= 0 {
			continue
		}
		if media == "application/json" || media == "application/*" || media == "*/*" {
			return true
		}
	}
	return false
}

func decorateAutoMCPResponse(out, request []byte, version string) []byte {
	var probe struct {
		Method string `json:"method"`
	}
	if json.Unmarshal(request, &probe) != nil {
		return out
	}
	var envelope map[string]any
	if json.Unmarshal(out, &envelope) != nil {
		return out
	}
	if result, ok := envelope["result"].(map[string]any); ok {
		if probe.Method == "initialize" && version != autoMCPProtocolModern {
			result["protocolVersion"] = version
		}
		if version == autoMCPProtocolModern && (probe.Method == "tools/list" || probe.Method == "server/discover") {
			result["ttlMs"] = float64(0)
			result["cacheScope"] = "private"
		}
	}
	b, err := json.Marshal(envelope)
	if err != nil {
		return out
	}
	return b
}

func validateAutoMCPModernHeaders(h http.Header, body []byte) error {
	var p struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &p); err != nil || p.Method == "" {
		return fmt.Errorf("invalid JSON-RPC request")
	}
	if method := strings.TrimSpace(h.Get(autoMCPHeaderMethod)); method == "" || method != p.Method {
		return fmt.Errorf("%s does not match JSON-RPC method", autoMCPHeaderMethod)
	}
	if p.Method == "initialize" || p.Method == "notifications/initialized" {
		return fmt.Errorf("%s is not supported by MCP %s", p.Method, autoMCPProtocolModern)
	}
	name := strings.TrimSpace(h.Get(autoMCPHeaderName))
	if p.Method == "tools/call" {
		if name == "" || name != p.Params.Name {
			return fmt.Errorf("%s does not match tools/call params.name", autoMCPHeaderName)
		}
	} else if name != "" {
		return fmt.Errorf("%s must be absent for %s", autoMCPHeaderName, p.Method)
	}
	return nil
}

func autoMCPError(w http.ResponseWriter, status, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	payload, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      nil,
		"error":   map[string]any{"code": code, "message": message},
	})
	_, _ = w.Write(payload)
}
