package dashboard

import (
	"sync"
	"time"
)

// ConnectionType categorises an external service for icon/color selection.
//
// Applications should pick the closest match — the UI uses this to choose
// the right icon family. If none fits, use ConnCustom.
type ConnectionType string

const (
	// Database — relational, document, key-value, time-series
	ConnDatabase ConnectionType = "database" // PostgreSQL, MySQL, SQLite, MongoDB, CockroachDB

	// Cache — in-memory stores
	ConnCache ConnectionType = "cache" // Redis, Memcached, DragonflyDB

	// MessageQueue — brokers and event streams
	ConnMessageQueue ConnectionType = "message_queue" // RabbitMQ, Kafka, NATS, NSQ, Pulsar

	// Search — full-text search engines
	ConnSearch ConnectionType = "search" // Elasticsearch, OpenSearch, Solr, Meilisearch

	// ObjectStore — blob/file storage
	ConnObjectStore ConnectionType = "object_store" // S3, MinIO, GCS, Azure Blob, Ceph

	// SMTP — mail servers
	ConnSMTP ConnectionType = "smtp" // Postfix, SendGrid, Mailgun, SES

	// HTTP — external HTTP APIs (REST)
	ConnHTTP ConnectionType = "http" // any external REST API

	// GRPC — gRPC services
	ConnGRPC ConnectionType = "grpc" // gRPC microservice

	// WebSocket — external WS endpoints
	ConnWebSocket ConnectionType = "websocket" // external WS server

	// Cloud — cloud provider services (managed)
	ConnCloud ConnectionType = "cloud" // AWS, GCP, Azure managed services

	// Server — a generic server / VM / bare metal
	ConnServer ConnectionType = "server" // SSH host, EC2 instance, dedicated server

	// Graph — graph databases
	ConnGraph ConnectionType = "graph" // Neo4j, ArangoDB, Dgraph

	// TimeseriesDB — time-series databases (distinct from generic DB)
	ConnTimeseriesDB ConnectionType = "timeseries_db" // InfluxDB, TimescaleDB, Prometheus

	// VectorDB — vector/embedding databases
	ConnVectorDB ConnectionType = "vector_db" // Pinecone, Weaviate, Milvus, Qdrant

	// CDN — content delivery network
	ConnCDN ConnectionType = "cdn" // Cloudflare, CloudFront, Fastly

	// DNS — DNS service
	ConnDNS ConnectionType = "dns" // Route53, CoreDNS, BIND

	// LDAP — directory service
	ConnLDAP ConnectionType = "ldap" // Active Directory, OpenLDAP

	// Custom — anything else (uses server icon)
	ConnCustom ConnectionType = "custom"
)

// ConnectionTypeLabel returns a human-readable label for a ConnectionType.
// Used in the UI when the type needs to be spelled out.
func ConnectionTypeLabel(t ConnectionType) string {
	switch t {
	case ConnDatabase:
		return "Database"
	case ConnCache:
		return "Cache"
	case ConnMessageQueue:
		return "Message Broker"
	case ConnSearch:
		return "Search Engine"
	case ConnObjectStore:
		return "Object Storage"
	case ConnSMTP:
		return "Mail Server"
	case ConnHTTP:
		return "HTTP API"
	case ConnGRPC:
		return "gRPC Service"
	case ConnWebSocket:
		return "WebSocket"
	case ConnCloud:
		return "Cloud Service"
	case ConnServer:
		return "Server"
	case ConnGraph:
		return "Graph Database"
	case ConnTimeseriesDB:
		return "Time-Series DB"
	case ConnVectorDB:
		return "Vector DB"
	case ConnCDN:
		return "CDN"
	case ConnDNS:
		return "DNS"
	case ConnLDAP:
		return "Directory"
	default:
		return "Service"
	}
}

// ConnectionStatus is the health state of an external service.
type ConnectionStatus string

const (
	StatusConnected    ConnectionStatus = "connected"    // green — healthy
	StatusDisconnected ConnectionStatus = "disconnected" // red — unreachable
	StatusDegraded     ConnectionStatus = "degraded"     // yellow — slow or partial
	StatusUnknown      ConnectionStatus = "unknown"      // gray — not checked yet
)

// Connection describes one external service the application talks to.
//
// Applications register connections via coll.RegisterConnection(...) and
// update their status periodically (e.g. from a health-check goroutine).
// The Architecture page visualises all registered connections in a radial
// diagram with the Breeze app at the center.
type Connection struct {
	// ID is a unique identifier for this connection (e.g. "primary-db").
	ID string `json:"id"`

	// Name is the human-readable label shown in the UI (e.g. "PostgreSQL Primary").
	Name string `json:"name"`

	// Type determines the icon and accent color in the visualization.
	Type ConnectionType `json:"type"`

	// Driver is the specific technology (e.g. "postgres", "redis", "rabbitmq").
	Driver string `json:"driver"`

	// Host is the network address (e.g. "10.0.0.5:5432"). Masked if sensitive.
	Host string `json:"host"`

	// Database is the DB name (for database connections) or equivalent namespace.
	Database string `json:"database,omitempty"`

	// Status is the current health state.
	Status ConnectionStatus `json:"status"`

	// LatencyMS is the most recent round-trip time in milliseconds (e.g. ping latency).
	LatencyMS float64 `json:"latency_ms,omitempty"`

	// Message is a human-readable status detail (e.g. "connection pool: 8/10 in use").
	Message string `json:"message,omitempty"`

	// Details is arbitrary key-value metadata shown in the expanded view.
	Details map[string]string `json:"details,omitempty"`

	// LastChecked is when the status was last verified.
	LastChecked time.Time `json:"last_checked"`

	// PoolInUse is the number of currently acquired connections (for pooled resources).
	PoolInUse int `json:"pool_in_use,omitempty"`

	// PoolMax is the maximum pool size.
	PoolMax int `json:"pool_max,omitempty"`
}

// connectionStore holds all registered external connections.
type connectionStore struct {
	mu          sync.RWMutex
	connections map[string]*Connection
}

func newConnectionStore() *connectionStore {
	return &connectionStore{
		connections: make(map[string]*Connection),
	}
}

// RegisterConnection adds or replaces a connection by ID.
//
// Call this at startup (or whenever a new external service is connected)
// to make it appear on the Architecture page. The status should be updated
// periodically via UpdateConnectionStatus.
func (c *Collector) RegisterConnection(conn Connection) {
	c.connStore.mu.Lock()
	defer c.connStore.mu.Unlock()
	cp := conn // copy to avoid aliasing
	c.connStore.connections[conn.ID] = &cp
}

// UpdateConnectionStatus updates the status, latency, and message of a
// connection identified by ID. If the connection is not registered, this
// is a no-op.
//
// Call this from a health-check goroutine (e.g. every 10 seconds) to keep
// the Architecture page live.
func (c *Collector) UpdateConnectionStatus(id string, status ConnectionStatus, latencyMS float64, message string) {
	c.connStore.mu.Lock()
	defer c.connStore.mu.Unlock()
	conn, ok := c.connStore.connections[id]
	if !ok {
		return
	}
	conn.Status = status
	conn.LatencyMS = latencyMS
	conn.Message = message
	conn.LastChecked = time.Now()
}

// UpdateConnectionPool updates the pool usage counters for a connection.
func (c *Collector) UpdateConnectionPool(id string, inUse, max int) {
	c.connStore.mu.Lock()
	defer c.connStore.mu.Unlock()
	conn, ok := c.connStore.connections[id]
	if !ok {
		return
	}
	conn.PoolInUse = inUse
	conn.PoolMax = max
}

// Connections returns a snapshot of all registered connections.
func (c *Collector) Connections() []Connection {
	c.connStore.mu.RLock()
	defer c.connStore.mu.RUnlock()
	out := make([]Connection, 0, len(c.connStore.connections))
	for _, conn := range c.connStore.connections {
		out = append(out, *conn)
	}
	return out
}
