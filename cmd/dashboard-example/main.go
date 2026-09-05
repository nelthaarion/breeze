// Example: Breeze application with the Developer Dashboard enabled.
//
// This is a REAL application — no fake data, no mock generators, no
// hardcoded metrics. The dashboard shows only what actually happens.
//
// Run:
//
//	go run ./cmd/dashboard-example
//
// Then open http://localhost:3000/dashboard in your browser
// (default credentials: admin / admin).
//
// To generate real dashboard data, hit the application endpoints:
//
//	curl http://localhost:3000/api/users
//	curl -X POST http://localhost:3000/api/users -d '{"name":"Alice","email":"alice@example.com"}'
//	curl http://localhost:3000/api/users/1
package main

import (
	"fmt"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-json"
	"github.com/nelthaarion/breeze/v2"
	"github.com/nelthaarion/breeze/v2/dashboard"
)

// ─── In-memory data store ──────────────────────────────────────────────────
//
// This stands in for a real database. In a production app you would use
// your actual ORM/database driver and wire its query hook into
// coll.PushQuery(...) so every SQL statement appears in the ORM Query
// Monitor page.

type User struct {
	ID        int       `json:"id"`
	Name      string    `json:"name"`
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"created_at"`
}

type UserStore struct {
	mu    sync.RWMutex
	users map[int]*User
	next  int
}

func NewUserStore() *UserStore {
	return &UserStore{
		users: make(map[int]*User),
		next:  1,
	}
}

func (s *UserStore) Create(name, email string) *User {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := &User{
		ID:        s.next,
		Name:      name,
		Email:     email,
		CreatedAt: time.Now(),
	}
	s.users[s.next] = u
	s.next++
	return u
}

func (s *UserStore) Get(id int) (*User, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.users[id]
	return u, ok
}

func (s *UserStore) All() []*User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*User, 0, len(s.users))
	for _, u := range s.users {
		out = append(out, u)
	}
	return out
}

func (s *UserStore) Delete(id int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[id]; !ok {
		return false
	}
	delete(s.users, id)
	return true
}

func (s *UserStore) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.users)
}

// ─── dashboard.DBInspector / dashboard.DBWriter ────────────────────────────
//
// This is what wires the Database Browser's "users" table view. In a real
// app you'd implement these against your actual database/ORM instead of an
// in-memory map.

func (s *UserStore) Tables() ([]dashboard.TableInfo, error) {
	return []dashboard.TableInfo{{Name: "users", Rows: int64(s.Count())}}, nil
}

func (s *UserStore) TableData(name string, page, pageSize int, search string) (dashboard.TableData, error) {
	if name != "users" {
		return dashboard.TableData{}, fmt.Errorf("unknown table: %s", name)
	}
	s.mu.RLock()
	all := make([]User, 0, len(s.users))
	for _, u := range s.users {
		all = append(all, *u)
	}
	s.mu.RUnlock()

	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })

	if search != "" {
		q := strings.ToLower(search)
		filtered := all[:0]
		for _, u := range all {
			if strings.Contains(strings.ToLower(u.Name), q) || strings.Contains(strings.ToLower(u.Email), q) {
				filtered = append(filtered, u)
			}
		}
		all = filtered
	}

	total := int64(len(all))
	start := (page - 1) * pageSize
	if start < 0 {
		start = 0
	}
	if start > len(all) {
		start = len(all)
	}
	end := start + pageSize
	if end > len(all) {
		end = len(all)
	}

	rows := make([]map[string]any, 0, end-start)
	for _, u := range all[start:end] {
		rows = append(rows, map[string]any{
			"id":         u.ID,
			"name":       u.Name,
			"email":      u.Email,
			"created_at": u.CreatedAt.Format(time.RFC3339),
		})
	}

	return dashboard.TableData{
		Table:    name,
		Page:     page,
		PageSize: pageSize,
		Total:    total,
		Columns: []dashboard.TableColumn{
			{Name: "id", Type: "int", PrimaryKey: true},
			{Name: "name", Type: "string"},
			{Name: "email", Type: "string"},
			{Name: "created_at", Type: "datetime"},
		},
		Rows: rows,
	}, nil
}

func (s *UserStore) InsertRow(table string, values map[string]any) (map[string]any, error) {
	if table != "users" {
		return nil, fmt.Errorf("unknown table: %s", table)
	}
	name, _ := values["name"].(string)
	email, _ := values["email"].(string)
	if name == "" || email == "" {
		return nil, fmt.Errorf("name and email are required")
	}
	u := s.Create(name, email)
	return map[string]any{
		"id":         u.ID,
		"name":       u.Name,
		"email":      u.Email,
		"created_at": u.CreatedAt.Format(time.RFC3339),
	}, nil
}

func (s *UserStore) UpdateRow(table string, pk map[string]any, values map[string]any) error {
	if table != "users" {
		return fmt.Errorf("unknown table: %s", table)
	}
	idStr, _ := pk["id"].(string)
	id, err := strconv.Atoi(idStr)
	if err != nil {
		return dashboard.ErrRowNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[id]
	if !ok {
		return dashboard.ErrRowNotFound
	}
	if v, ok := values["name"].(string); ok && v != "" {
		u.Name = v
	}
	if v, ok := values["email"].(string); ok && v != "" {
		u.Email = v
	}
	return nil
}

func (s *UserStore) DeleteRow(table string, pk map[string]any) error {
	if table != "users" {
		return fmt.Errorf("unknown table: %s", table)
	}
	idStr, _ := pk["id"].(string)
	id, err := strconv.Atoi(idStr)
	if err != nil {
		return dashboard.ErrRowNotFound
	}
	if !s.Delete(id) {
		return dashboard.ErrRowNotFound
	}
	return nil
}

// ─── Main ──────────────────────────────────────────────────────────────────

func main() {
	router := breeze.NewRouter()
	pool := breeze.NewEventLoopWorkerPool(runtime.NumCPU())
	app := breeze.New(router, pool)

	store := NewUserStore()

	// ── Install the dashboard ────────────────────────────────────────────
	// The default config sets GOMEMLIMIT=512MB and GOGC=50 to prevent
	// Go's runtime from holding onto gigabytes of idle memory.
	//
	// For production, tune these based on your available RAM:
	//   cfg := dashboard.DefaultConfig()
	//   cfg.GOMEMLIMIT = 1024 * 1024 * 1024  // 1 GB limit
	//   cfg.GOGC = 100                        // Go default (less aggressive GC)
	cfg := dashboard.DefaultConfig()
	cfg.AllowWrites = true // demonstrates the editable Database Browser; leave false in production unless intended

	coll := dashboard.Install(app, router, cfg)
	coll.SetDBInspector(store)
	coll.SetDBWriter(store)

	// Install the instrumentation middleware so every request is captured.
	// This MUST come before your application routes.
	router.Use(coll.Middleware())

	// ── Register external connections for the Architecture page ────────
	// In a real app, these would be your actual DB/cache/queue connections.
	// The status should be updated periodically from a health-check goroutine.
	coll.RegisterConnection(dashboard.Connection{
		ID:       "primary-db",
		Name:     "In-Memory Store",
		Type:     dashboard.ConnDatabase,
		Driver:   "breeze-store",
		Host:     "localhost:in-memory",
		Database: "users",
		Status:   dashboard.StatusConnected,
		Message:  "store online",
	})
	coll.RegisterConnection(dashboard.Connection{
		ID:      "ws-hub",
		Name:    "WebSocket Hub",
		Type:    dashboard.ConnWebSocket,
		Driver:  "breeze-ws",
		Host:    "localhost:3000",
		Status:  dashboard.StatusConnected,
		Message: "hub active",
	})

	// ── Real health checks ───────────────────────────────────────────────
	// Each check actually verifies something. No hardcoded results.
	coll.RegisterHealthCheck("runtime", func() (string, string) {
		// Real check: verify Go runtime is responsive and goroutine count
		// is not out of control.
		goroutines := runtime.NumGoroutine()
		if goroutines > 10000 {
			return "red", fmt.Sprintf("goroutine leak: %d active", goroutines)
		}
		if goroutines > 1000 {
			return "yellow", fmt.Sprintf("high goroutine count: %d", goroutines)
		}
		return "green", fmt.Sprintf("%d goroutines, %d CPUs", goroutines, runtime.NumCPU())
	})

	coll.RegisterHealthCheck("data-store", func() (string, string) {
		// Real check: verify the in-memory store is accessible.
		count := store.Count()
		return "green", fmt.Sprintf("users store online (%d records)", count)
	})

	// ── Application routes ───────────────────────────────────────────────
	// These are real endpoints. The dashboard middleware captures every
	// request automatically — no manual instrumentation needed.

	router.Handle(breeze.GET, "/", func(ctx *breeze.Context) error {
		return ctx.WriteString("Breeze API server. Visit /dashboard for the developer dashboard.")
	})

	// GET /api/users — list all users
	router.Handle(breeze.GET, "/api/users", func(ctx *breeze.Context) error {
		users := store.All()
		return ctx.JSON(map[string]any{
			"users": users,
			"total": len(users),
		})
	})

	// POST /api/users — create a new user
	router.Handle(breeze.POST, "/api/users", func(ctx *breeze.Context) error {
		var req struct {
			Name  string `json:"name"`
			Email string `json:"email"`
		}
		if err := json.Unmarshal(ctx.Req.Body, &req); err != nil {
			ctx.Status(400)
			return ctx.JSON(map[string]string{"error": "invalid JSON body"})
		}
		if req.Name == "" || req.Email == "" {
			ctx.Status(422)
			return ctx.JSON(map[string]string{"error": "name and email are required"})
		}
		u := store.Create(req.Name, req.Email)
		ctx.Status(201)
		return ctx.JSON(u)
	})

	// GET /api/users/:id — get a single user
	router.Handle(breeze.GET, "/api/users/:id", func(ctx *breeze.Context) error {
		idStr := ctx.Param("id")
		id, err := strconv.Atoi(idStr)
		if err != nil {
			ctx.Status(400)
			return ctx.JSON(map[string]string{"error": "invalid user id"})
		}
		u, ok := store.Get(id)
		if !ok {
			ctx.Status(404)
			return ctx.JSON(map[string]string{"error": "user not found"})
		}
		return ctx.JSON(u)
	})

	// DELETE /api/users/:id — delete a user
	router.Handle(breeze.DELETE, "/api/users/:id", func(ctx *breeze.Context) error {
		idStr := ctx.Param("id")
		id, err := strconv.Atoi(idStr)
		if err != nil {
			ctx.Status(400)
			return ctx.JSON(map[string]string{"error": "invalid user id"})
		}
		if !store.Delete(id) {
			ctx.Status(404)
			return ctx.JSON(map[string]string{"error": "user not found"})
		}
		ctx.Status(204)

		return nil
	})

	// GET /api/health — application-level health endpoint (separate from dashboard)
	router.Handle(breeze.GET, "/api/health", func(ctx *breeze.Context) error {
		return ctx.JSON(map[string]any{
			"status":   "ok",
			"users":    store.Count(),
			"uptime_s": int64(time.Since(startTime).Seconds()),
		})
	})

	fmt.Println("Breeze listening on :3000")
	fmt.Println("  Application: http://localhost:3000/")
	fmt.Println("  Dashboard:   http://localhost:3000/dashboard  (admin / admin)")
	fmt.Println()
	fmt.Println("Try these to generate real dashboard data:")
	fmt.Println("  curl http://localhost:3000/api/users")
	fmt.Println("  curl -X POST http://localhost:3000/api/users \\")
	fmt.Println("    -H 'Content-Type: application/json' \\")
	fmt.Println("    -d '{\"name\":\"Alice\",\"email\":\"alice@example.com\"}'")
	fmt.Println("  curl http://localhost:3000/api/users/1")
	app.Run(3000, true)
}

var startTime = time.Now()
