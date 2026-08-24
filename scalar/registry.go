package scalar

import (
	"strings"
	"sync"
)

type routeEntry struct {
	method string
	path   string
	doc    RouteDoc
}

var (
	mu         sync.RWMutex
	routes     []routeEntry
	enabled    bool
	apiTitle   string
	apiVersion string
	apiDesc    string
)

// Enable activates Scalar doc collection.
func Enable() { enabled = true }

// Enabled returns whether Scalar is currently active.
func Enabled() bool { return enabled }

// SetInfo sets the API title, version, and optional description shown in Scalar.
func SetInfo(title, version, description string) {
	mu.Lock()
	defer mu.Unlock()
	apiTitle = title
	apiVersion = version
	apiDesc = description
}

// RegisterRoute records a route's documentation at startup time.
func RegisterRoute(method, path string, doc RouteDoc) {
	if !enabled {
		return
	}
	entry := routeEntry{
		method: strings.ToLower(method),
		path:   breezePath(path),
		doc:    doc,
	}
	mu.Lock()
	routes = append(routes, entry)
	mu.Unlock()
}

func allRoutes() []routeEntry {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]routeEntry, len(routes))
	copy(out, routes)
	return out
}

func breezePath(pattern string) string {
	parts := strings.Split(pattern, "/")
	for i, p := range parts {
		if strings.HasPrefix(p, ":") {
			parts[i] = "{" + p[1:] + "}"
		}
	}
	return strings.Join(parts, "/")
}
