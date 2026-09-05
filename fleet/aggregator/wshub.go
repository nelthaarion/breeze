package aggregator

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/nelthaarion/breeze/v2"
	"github.com/nelthaarion/breeze/v2/fleet"
	"github.com/nelthaarion/breeze/v2/fleet/transport/eventtransport"
)

const wsSnapshotInterval = 2 * time.Second

type fleetWSRole uint8

const (
	wsUnauthenticated fleetWSRole = iota
	wsViewer
	wsIngest
)

type fleetWSClient struct {
	role   fleetWSRole
	topics map[string]bool
}

// wsHub multiplexes read-side live events and eventtransport memory-backend
// publish envelopes over one /fleet/ws route. Connections receive no data and
// may publish nothing until an auth frame assigns a role.
type wsHub struct {
	a       *Aggregator
	mu      sync.RWMutex
	clients map[*breeze.WSConn]*fleetWSClient
	stop    chan struct{}
	once    sync.Once
}

func newWSHub(a *Aggregator) *wsHub {
	h := &wsHub{a: a, clients: make(map[*breeze.WSConn]*fleetWSClient), stop: make(chan struct{})}
	go h.loop()
	return h
}

func (h *wsHub) close() { h.once.Do(func() { close(h.stop) }) }

func (h *wsHub) OnConnect(conn *breeze.WSConn) {
	h.mu.Lock()
	h.clients[conn] = &fleetWSClient{}
	h.mu.Unlock()
}

func (h *wsHub) OnClose(conn *breeze.WSConn, _ uint16, _ string) {
	h.mu.Lock()
	delete(h.clients, conn)
	h.mu.Unlock()
}

type wsEnvelope struct {
	Type     string          `json:"type"`
	Role     string          `json:"role,omitempty"`
	Token    string          `json:"token,omitempty"`
	Username string          `json:"username,omitempty"`
	Password string          `json:"password,omitempty"`
	Topic    string          `json:"topic,omitempty"`
	Topics   []string        `json:"topics,omitempty"`
	Payload  json.RawMessage `json:"payload,omitempty"`
}

func (h *wsHub) OnMessage(conn *breeze.WSConn, opcode byte, payload []byte) {
	if opcode != 1 {
		conn.Close(breeze.WsCloseUnsupportedData, "text frames required")
		return
	}
	var env wsEnvelope
	if json.Unmarshal(payload, &env) != nil {
		h.sendError(conn, "malformed envelope")
		return
	}
	client := h.client(conn)
	if client == nil {
		return
	}
	if client.role == wsUnauthenticated {
		if env.Type != "auth" || !h.authenticate(client, env) {
			conn.Close(breeze.WsClosePolicyViolation, "authentication required")
			return
		}
		_ = conn.SendText(`{"type":"auth_ok"}`)
		if client.role == wsViewer {
			h.sendSnapshot(conn)
		}
		return
	}

	switch env.Type {
	case "subscribe":
		if client.role != wsViewer {
			h.sendError(conn, "viewer role required")
			return
		}
		h.mu.Lock()
		client.topics = make(map[string]bool, len(env.Topics))
		for _, topic := range env.Topics {
			client.topics[topic] = true
		}
		h.mu.Unlock()
	case "publish":
		if client.role != wsIngest {
			h.sendError(conn, "ingest role required")
			return
		}
		if err := h.acceptPublish(env.Topic, env.Payload); err != nil {
			h.sendError(conn, err.Error())
		}
	case "ping":
		_ = conn.SendText(`{"type":"pong"}`)
	default:
		h.sendError(conn, "unknown message type")
	}
}

func (h *wsHub) authenticate(client *fleetWSClient, env wsEnvelope) bool {
	switch env.Role {
	case "ingest":
		if h.a.cfg.IngestToken != "" && !secureEqual(env.Token, h.a.cfg.IngestToken) {
			return false
		}
		client.role = wsIngest
	case "viewer", "":
		if h.a.cfg.AuthEnabled() && (!secureEqual(env.Username, h.a.cfg.Username) || !secureEqual(env.Password, h.a.cfg.Password)) {
			return false
		}
		client.role = wsViewer
	default:
		return false
	}
	return true
}

func secureEqual(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func (h *wsHub) acceptPublish(topic string, payload json.RawMessage) error {
	switch topic {
	case eventtransport.DefaultSpansTopic:
		var spans []fleet.Span
		if err := json.Unmarshal(payload, &spans); err != nil {
			return err
		}
		return h.a.AcceptSpans(spans)
	case eventtransport.DefaultHeartbeatTopic:
		var hb fleet.Heartbeat
		if err := json.Unmarshal(payload, &hb); err != nil {
			return err
		}
		return h.a.AcceptHeartbeat(hb)
	default:
		return errors.New("fleet websocket: topic is not allowed")
	}
}

func (h *wsHub) client(conn *breeze.WSConn) *fleetWSClient {
	h.mu.RLock()
	c := h.clients[conn]
	h.mu.RUnlock()
	return c
}

func (h *wsHub) loop() {
	ticker := time.NewTicker(wsSnapshotInterval)
	defer ticker.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-ticker.C:
			h.broadcast("topology_snapshot", h.a.topology.Snapshot())
		}
	}
}

func (h *wsHub) traceEvent(spans []fleet.Span) {
	for _, span := range spans {
		if span.Failed() {
			h.broadcast("trace_event", map[string]any{"trace_id": span.TraceID, "service": span.Service, "error": true})
			return
		}
	}
	if len(spans) > 0 {
		h.broadcast("trace_event", map[string]any{"trace_id": spans[0].TraceID, "service": spans[0].Service})
	}
}

func (h *wsHub) serviceEvent(hb fleet.Heartbeat) { h.broadcast("service_event", hb) }

func (h *wsHub) broadcast(kind string, data any) {
	body, err := json.Marshal(map[string]any{"type": kind, "time": time.Now().UTC().Format(time.RFC3339Nano), "data": data})
	if err != nil {
		return
	}
	h.mu.RLock()
	targets := make([]*breeze.WSConn, 0, len(h.clients))
	for conn, client := range h.clients {
		if client.role == wsViewer && (len(client.topics) == 0 || client.topics[kind]) {
			targets = append(targets, conn)
		}
	}
	h.mu.RUnlock()
	for _, conn := range targets {
		if err := conn.SendText(string(body)); err != nil {
			h.OnClose(conn, 0, "write failed")
		}
	}
}

func (h *wsHub) sendSnapshot(conn *breeze.WSConn) {
	body, _ := json.Marshal(map[string]any{"type": "topology_snapshot", "data": h.a.topology.Snapshot()})
	_ = conn.SendText(string(body))
}

func (*wsHub) sendError(conn *breeze.WSConn, message string) {
	body, _ := json.Marshal(map[string]string{"type": "error", "error": message})
	_ = conn.SendText(string(body))
}
