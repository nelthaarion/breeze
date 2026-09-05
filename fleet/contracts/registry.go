package contracts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/nelthaarion/breeze/v2/client"
	"github.com/nelthaarion/breeze/v2/scalar"
)

const maxOpenAPIDocument = 8 << 20

type schemaEntry struct {
	hash string
	doc  scalar.OpenAPI
}

// SchemaRegistry caches each service's generated OpenAPI document by heartbeat
// hash. Fetch is explicit and context-bounded; callers run it off ingestion.
type SchemaRegistry struct {
	mu     sync.RWMutex
	docs   map[string]schemaEntry
	client *client.Client
}

func NewSchemaRegistry(c *client.Client) *SchemaRegistry {
	if c == nil {
		c = client.New(client.Config{})
	}
	return &SchemaRegistry{docs: make(map[string]schemaEntry), client: c}
}

func (r *SchemaRegistry) Refresh(ctx context.Context, service, hash, url string) (bool, error) {
	if service == "" || hash == "" || url == "" {
		return false, nil
	}
	r.mu.RLock()
	old, ok := r.docs[service]
	r.mu.RUnlock()
	if ok && old.hash == hash {
		return false, nil
	}
	req := client.NewRequest("GET", url, nil).WithContext(ctx)
	resp, err := r.client.Do(req)
	if err != nil {
		return false, err
	}
	if resp == nil {
		return false, errors.New("openapi fetch returned no response")
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return false, errors.New("openapi fetch returned status " + fmt.Sprint(resp.Status))
	}
	body := resp.Body
	if len(body) > maxOpenAPIDocument {
		return false, errors.New("openapi document exceeds 8 MiB")
	}
	var doc scalar.OpenAPI
	if err := json.Unmarshal(body, &doc); err != nil {
		return false, err
	}
	r.mu.Lock()
	r.docs[service] = schemaEntry{hash: hash, doc: doc}
	r.mu.Unlock()
	return true, nil
}

func (r *SchemaRegistry) Operation(service, route, method string) (scalar.Operation, bool) {
	r.mu.RLock()
	entry, ok := r.docs[service]
	r.mu.RUnlock()
	if !ok {
		return scalar.Operation{}, false
	}
	route = normalizeRoute(route)
	item, ok := entry.doc.Paths[route]
	if !ok {
		return scalar.Operation{}, false
	}
	op, ok := item[strings.ToLower(method)]
	return op, ok
}

func normalizeRoute(route string) string {
	parts := strings.Split(route, "/")
	for i, p := range parts {
		if strings.HasPrefix(p, ":") && len(p) > 1 {
			parts[i] = "{" + p[1:] + "}"
		}
	}
	return strings.Join(parts, "/")
}
