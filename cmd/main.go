package main

import (
	"fmt"
	"runtime"

	"github.com/nelthaarion/breeze"
	middleware "github.com/nelthaarion/breeze/middlewares"
	"github.com/nelthaarion/breeze/scalar"
)

// ─── HTTP request / response types ───────────────────────────────────────────

type CreateUserRequest struct {
	Name  string `json:"name"  description:"Full name of the user"`
	Email string `json:"email" description:"Email address"`
	Age   int    `json:"age"   description:"Age in years"`
}

type UserResponse struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// wsStats is the /ws/stats payload. It exists so the handler can marshal a
// struct instead of a map — see the comment at its use site.
type wsStats struct {
	Connections int64 `json:"connections"`
}

type UserListResponse struct {
	Users []UserResponse `json:"users"`
	Total int            `json:"total"`
}
type UserPathParams struct {
	ID string `json:"id" description:"User UUID"`
}

type UserQueryParams struct {
	Page  int    `json:"page"  description:"Page number (default 1)"  omitempty:"true"`
	Limit int    `json:"limit" description:"Items per page (default 20)" omitempty:"true"`
	Sort  string `json:"sort"  description:"Sort field"                omitempty:"true"`
}

// ─── WebSocket chat handler ───────────────────────────────────────────────────

type ChatHandler struct {
	hub *breeze.WSHub
}

func (h *ChatHandler) OnConnect(conn *breeze.WSConn) {
	fmt.Printf("[ws] client connected: %s (total: %d)\n", conn.RemoteAddr(), h.hub.Count())
	h.hub.BroadcastExcept(breeze.WsOpText, []byte("a user joined"), conn)
}

func (h *ChatHandler) OnMessage(conn *breeze.WSConn, opcode byte, payload []byte) {
	if opcode == breeze.WsOpText {
		msg := fmt.Sprintf("[%s]: %s", conn.RemoteAddr(), string(payload))
		h.hub.BroadcastText(msg)
	} else {
		_ = conn.SendBinary(payload)
	}
}

func (h *ChatHandler) OnClose(conn *breeze.WSConn, code uint16, reason string) {
	fmt.Printf("[ws] client disconnected: %s code=%d reason=%q (remaining: %d)\n",
		conn.RemoteAddr(), code, reason, h.hub.Count())
	h.hub.BroadcastText("a user left")
}

// ─── main ─────────────────────────────────────────────────────────────────────

func main() {
	router := breeze.NewRouter()
	pool := breeze.NewEventLoopWorkerPool(runtime.NumCPU())
	app := breeze.New(router, pool)

	// Nothing in this app keeps a string off ctx.Req past the handler that read
	// it — the handlers marshal a response and return, ServeStatic and the
	// Scalar routes are registered blocking (so their requests are promoted to
	// owned memory before they reach a worker), and no middleware caches by
	// path. That is the whole precondition, so take the last allocation out of
	// the request path.
	app.SetZeroCopyHeaders(true)

	// WebSocket() returns the hub immediately — inject it into the handler.
	chat := &ChatHandler{}
	chat.hub = app.WebSocket("/ws", chat)
	// router.Use(middleware.LoggingMiddleware())
	// Inline echo endpoint using WSHandlerFunc.
	app.WebSocket("/ws/echo", &breeze.WSHandlerFunc{
		Connect: func(conn *breeze.WSConn) {

			_ = conn.SendText("echo server ready")
		},
		Message: func(conn *breeze.WSConn, opcode byte, payload []byte) {
			_ = conn.Send(opcode, payload)
		},
	})

	// ── HTTP routes ───────────────────────────────────────────────────────
	router.Use(middleware.ScalarMiddleware(router, middleware.ScalarOptions{
		Title:       "Breeze Example API",
		Version:     "1.0.0",
		Description: "A demonstration of the Breeze Scalar middleware.",
		JSONPath:    "/openapi.json",
		UIPath:      "/scalar",
	}))
	router.ServeStatic("/files", "./files/")
	router.Handle(breeze.GET, "/users", listUsers,
		middleware.DocGET("/users", scalar.RouteDoc{
			Title:       "List users",
			Tags:        []string{"Users"},
			Description: "Returns a paginated list of users.",
			Input: []scalar.InputGroup{
				{
					Type:        scalar.InputQuery,
					Fields:      UserQueryParams{},
					Description: "Pagination and sorting options",
				},
			},
			Output:            UserListResponse{},
			OutputStatus:      200,
			OutputDescription: "Paginated user list",
		}),
	)

	// POST /users — create user
	router.Handle(breeze.POST, "/users", createUser,
		middleware.DocPOST("/users", scalar.RouteDoc{
			Title: "Create user",
			Tags:  []string{"Users"},
			Input: []scalar.InputGroup{
				{
					Type:     scalar.InputBody,
					Fields:   CreateUserRequest{},
					Required: true,
				},
			},
			Output:       UserResponse{},
			OutputStatus: 201,
		}),
	)

	// GET /users/:id — get single user
	router.Handle(breeze.GET, "/users/:id", getUser,
		middleware.DocGET("/users/:id", scalar.RouteDoc{
			Title: "Get user by ID",
			Tags:  []string{"Users"},
			Input: []scalar.InputGroup{
				{
					Type:   scalar.InputParams,
					Fields: UserPathParams{},
				},
			},
			Output: UserResponse{},
		}),
	)

	// DELETE /users/:id — delete user
	router.Handle(breeze.DELETE, "/users/:id", deleteUser,
		middleware.DocDELETE("/users/:id", scalar.RouteDoc{
			Title: "Delete user",
			Tags:  []string{"Users"},
			Input: []scalar.InputGroup{
				{Type: scalar.InputParams, Fields: UserPathParams{}},
			},
			Output:            struct{}{},
			OutputStatus:      204,
			OutputDescription: "User deleted",
		}),
	)

	router.Handle(breeze.GET, "/ws/stats", func(ctx *breeze.Context) {
		count := int64(0)
		if h := app.Hub(); h != nil {
			count = h.Count()
		}
		// A struct, not map[string]int64. encoding/json caches an encoder per
		// type, so a struct writes its fields directly; a map is allocated,
		// hashed, reflected over, and has its keys sorted before anything is
		// written. That is ~780ns against ~170ns for identical output — more
		// than the framework's entire request path either way. See
		// BenchmarkZZJSONMapHandler.
		ctx.JSON(wsStats{Connections: count})
	})

	router.Handle(breeze.GET, "/", func(ctx *breeze.Context) {
		ctx.WriteString("Breeze — HTTP + WebSocket server")
	})

	fmt.Println("Breeze listening on :3000")
	fmt.Println("  WebSocket chat : ws://localhost:3000/ws")
	fmt.Println("  WebSocket echo : ws://localhost:3000/ws/echo")
	fmt.Println("  WS stats       : GET /ws/stats")
	app.Run(3000, true)
}

// ─── HTTP handlers ────────────────────────────────────────────────────────────

func listUsers(ctx *breeze.Context) {
	ctx.JSON(UserListResponse{
		Users: []UserResponse{{ID: "1", Name: "Alice", Email: "alice@example.com"}},
		Total: 1,
	})
}

func createUser(ctx *breeze.Context) {
	ctx.Status(201)
	ctx.JSON(UserResponse{ID: "2", Name: "Bob", Email: "bob@example.com"})
}

func getUser(ctx *breeze.Context) {

	ctx.JSON(UserResponse{
		ID:    ctx.GetParam("id"),
		Name:  "Alice",
		Email: "alice@example.com",
	})
}

func deleteUser(ctx *breeze.Context) {
	ctx.Status(204)
}
