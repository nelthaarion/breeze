// Command automcp-example is one service that publishes some of its own routes as
// MCP tools, and does not publish the rest.
//
// # What Auto-MCP is
//
// A route this application already serves is a capability an agent could use. Auto-MCP
// makes it one without a second description of it: tag the route, and its OpenAPI
// declaration becomes the tool's schema and its middleware chain becomes the tool's
// access control.
//
//	router.Handle(breeze.POST, "/orders", createOrder,
//	    breeze.MCPTool("create_order", "Creates a new order for a customer"))
//
// The tag is the entire opt-in surface. It is stripped at registration, so it costs
// nothing per request and changes nothing about how the route is served over HTTP.
//
// # The three routes here exist to be compared
//
//	POST /orders            tagged, open          — the basic case
//	GET  /orders/:id        tagged, behind JWT    — the chain still runs
//	GET  /internal/metrics  untagged              — never becomes a tool
//
// The untagged route is documented, so it appears in /openapi.json and in the Scalar
// UI. It still never appears in tools/list and cannot be called by guessing its name.
// Being discoverable is not being callable.
//
// # Two listeners, on purpose
//
// EnableMCP takes its own address. MCP speaks JSON-RPC and HTTP speaks HTTP;
// multiplexing them would mean sniffing the first bytes of every connection forever.
// A second listener also lets an operator keep MCP on loopback while the API faces the
// world, which is the deployment most people want — and is what this example does.
//
// The MCP listener is raw JSON-RPC 2.0 on TCP, not MCP Streamable HTTP: a client writes a
// JSON value and reads one back, with no HTTP framing and no session. `breeze-mcp --port`
// is the HTTP-framed one; these are different endpoints for different purposes.
//
// # SECURITY: the MCP port has no authentication of its own
//
// Auto-MCP is a plain JSON-RPC listener. There is no bearer token on it, so anything
// that can reach the port can call every tagged tool. What stands between a caller and
// a route is the route's own middleware — which is why get_order is behind JWT and why
// this binds to 127.0.0.1 rather than every interface. Exposing this port off-host
// means exposing every tagged route's chain to whatever is out there; put it behind
// something that authenticates first.
//
// breeze/mcp.StartInProcess is the other endpoint, and it does require a token. It
// serves framework introspection, not this application's routes — see
// docs/mcp-walkthrough.md.
//
//	go run ./cmd/automcp-example
package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/nelthaarion/breeze/v2"
	middleware "github.com/nelthaarion/breeze/v2/middlewares"
	"github.com/nelthaarion/breeze/v2/scalar"
)

// ─── the model ───────────────────────────────────────────────────────────────

// CreateOrderRequest is the POST /orders body.
//
// One flat struct rather than a nested one, because every tag on it is doing visible
// work and nesting would add a level without adding a lesson:
//
//   - `json` names the field, and the absence of omitempty is what makes it required in
//     the generated OpenAPI schema — and therefore in the MCP tool schema, since both
//     are derived from this struct.
//   - `validate` is what ctx.Bind enforces at request time.
//   - `description` is the sentence a model reads to decide what to put here. It is the
//     highest-leverage tag in the struct: a tool with good descriptions gets called
//     correctly, and a tool without them gets guessed at.
//
// The two rule sets are written to agree. They are enforced by different code — schema
// generation and binding — so a field optional in one and required in the other would
// produce a tool that advertises a call it then refuses.
type CreateOrderRequest struct {
	SKU      string `json:"sku"      validate:"required,min=3,max=32" description:"Stock keeping unit to order, for example BRZ-100."`
	Quantity int    `json:"quantity" validate:"required,min=1,max=100" description:"How many units to order, from 1 to 100."`
	Customer string `json:"customer" validate:"required,email"         description:"Email address the confirmation is sent to."`
}

// Order is what the service stores and returns.
type Order struct {
	ID         string `json:"id"`
	SKU        string `json:"sku"`
	Quantity   int    `json:"quantity"`
	Customer   string `json:"customer"`
	UnitCents  int    `json:"unit_cents"`
	TotalCents int    `json:"total_cents"`
	Status     string `json:"status"`
	PlacedAt   string `json:"placed_at"`
}

// orderPathParams declares the :id segment of GET /orders/:id.
type orderPathParams struct {
	ID string `json:"id" description:"Identifier returned by create_order, for example ord-1."`
}

// orderAuthHeader declares the credential GET /orders/:id requires.
//
// # Why the header is declared as an input
//
// A tool call may only send arguments the tool advertises, and the tool advertises
// exactly what the route declared. An undeclared header is refused by name rather than
// dropped — so a caller cannot smuggle an Authorization, a Cookie or an
// X-Forwarded-For into a route that never asked for one.
//
// The consequence is that a route behind authentication has to say so here to be
// callable with credentials at all. That is the intended shape: which headers a tool
// can influence is a decision the route author writes down, not a default.
type orderAuthHeader struct {
	Authorization string `json:"authorization" description:"Bearer token, as in: Bearer <jwt>."`
}

// MetricsResponse is the untagged route's output.
type MetricsResponse struct {
	Orders   int    `json:"orders"`
	Revenue  int    `json:"revenue_cents"`
	Internal bool   `json:"internal"`
	Note     string `json:"note"`
}

// ─── the store ───────────────────────────────────────────────────────────────

// catalogue is the price list, in cents. An unknown SKU is a real refusal rather than a
// default price: a tool that silently invents a price is worse than one that fails.
var catalogue = map[string]int{
	"BRZ-100": 1250,
	"BRZ-200": 4999,
	"BRZ-300": 899,
}

// store holds the orders this process has accepted.
//
// A mutex rather than a bare map because the HTTP routes are served from event-loop
// goroutines while the MCP listener has its own: concurrent access is the normal case
// here, not the exceptional one.
type store struct {
	mu     sync.Mutex
	seq    int
	orders map[string]Order
}

func newStore() *store { return &store{orders: make(map[string]Order)} }

// place computes and records an order. Validation already happened in ctx.Bind.
func (s *store) place(req CreateOrderRequest, unitCents int) Order {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.seq++
	order := Order{
		ID:         "ord-" + strconv.Itoa(s.seq),
		SKU:        req.SKU,
		Quantity:   req.Quantity,
		Customer:   req.Customer,
		UnitCents:  unitCents,
		TotalCents: unitCents * req.Quantity,
		Status:     "confirmed",
		PlacedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	s.orders[order.ID] = order
	return order
}

func (s *store) find(id string) (Order, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	order, ok := s.orders[id]
	return order, ok
}

// totals reports the order count and revenue, for the untagged metrics route.
func (s *store) totals() (count, revenueCents int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range s.orders {
		revenueCents += o.TotalCents
	}
	return len(s.orders), revenueCents
}

// ─── handlers ────────────────────────────────────────────────────────────────

// createOrder is the handler behind create_order.
//
// It does real work, and that is the point of the example rather than an incidental
// detail: the tool result carries whatever this returns, so a schema that matches the
// route means nothing unless the handler behind it actually behaves. An echo would make
// every assertion in the driver test pass vacuously.
//
// Three things are observable in the response that could not be echoed from the input:
// the generated id, the unit price looked up from the catalogue, and total_cents =
// unit_cents × quantity.
func (a *app) createOrder(ctx *breeze.Context) error {
	var req CreateOrderRequest
	if err := ctx.Bind(&req); err != nil {
		// Bind has already written the 422 (validation) or 400 (malformed body) with an
		// RFC 9457 problem+json body. Returning nil sends that, rather than replacing a
		// specific failure with a generic one.
		return nil
	}

	unitCents, stocked := catalogue[req.SKU]
	if !stocked {
		// A 404 for an unknown SKU. The MCP result carries this status verbatim, so an
		// agent can tell "you asked for something that does not exist" apart from "the
		// service is broken" — which a 500 would not.
		ctx.Status(404)
		return ctx.JSON(map[string]any{
			"error": "unknown sku",
			"sku":   req.SKU,
			"known": sortedSKUs(),
		})
	}

	order := a.store.place(req, unitCents)
	ctx.Status(201)
	return ctx.JSON(order)
}

// getOrder is the handler behind get_order, which sits behind JWT.
//
// By the time this runs the middleware has already accepted the token, so there is no
// authentication logic here at all — that is the guarantee being demonstrated. The
// claims are read to prove the middleware ran and put them somewhere reachable.
func (a *app) getOrder(ctx *breeze.Context) error {
	order, ok := a.store.find(ctx.Param("id"))
	if !ok {
		ctx.Status(404)
		return ctx.JSON(map[string]any{"error": "no such order", "id": ctx.Param("id")})
	}
	return ctx.JSON(map[string]any{
		"order": order,
		// Populated by JWTAuthMiddleware under its UserContextKey. Present in the
		// response so the driver test can assert the chain ran rather than inferring it
		// from a status code.
		"read_by": claimString(ctx, "user_id"),
	})
}

// internalMetrics is the route that is deliberately NOT tagged.
//
// It is documented, so it is in /openapi.json and in the Scalar UI. It is still absent
// from tools/list, and calling "internal_metrics" over MCP fails with a list of the
// tools that do exist. Discoverable is not callable.
func (a *app) internalMetrics(ctx *breeze.Context) error {
	count, revenue := a.store.totals()
	return ctx.JSON(MetricsResponse{
		Orders:   count,
		Revenue:  revenue,
		Internal: true,
		Note:     "this route carries no MCPTool tag, so no agent can reach it",
	})
}

// claimString reads one string claim out of the JWT middleware's context entry.
func claimString(ctx *breeze.Context, key string) string {
	raw, ok := ctx.Get(jwtContextKey)
	if !ok {
		return ""
	}
	claims, ok := raw.(jwt.MapClaims)
	if !ok {
		return ""
	}
	s, _ := claims[key].(string)
	return s
}

// sortedSKUs lists the catalogue deterministically, so an error body does not churn
// between calls on Go's map iteration order.
func sortedSKUs() []string {
	out := make([]string, 0, len(catalogue))
	for sku := range catalogue {
		out = append(out, sku)
	}
	sort.Strings(out)
	return out
}

// ─── wiring ──────────────────────────────────────────────────────────────────

// jwtContextKey is where JWTAuthMiddleware puts the verified claims.
const jwtContextKey = "user"

// app bundles the state the handlers share, so they are methods rather than closures
// over package-level variables. One store, injected once, with no service locator and
// no nil window.
type app struct {
	store *store
}

// register wires every route, its documentation, and — for two of the three — its MCP
// tag.
//
// Documentation and tag arrive as trailing arguments to the same Handle call as the
// route itself. That is the only moment the three are guaranteed to agree: a separate
// registration table keyed by method and path is a table that can drift.
//
// Exported to the test in this package, which registers against a router of its own
// rather than reaching into main.
func (a *app) register(router *breeze.Router, jwtSecret string) {
	// ── POST /orders — tagged, open ───────────────────────────────────────
	router.Handle(breeze.POST, "/orders", a.createOrder,
		middleware.DocPOST("/orders", scalar.RouteDoc{
			Title:       "Create an order",
			Tags:        []string{"Orders"},
			Description: "Places an order for a catalogue SKU and returns the priced order.",
			Input: []scalar.InputGroup{
				{Type: scalar.InputBody, Fields: CreateOrderRequest{}, Required: true},
			},
			Output:            Order{},
			OutputStatus:      201,
			OutputDescription: "The order that was placed.",
		}),
		// The tag. Its name is what a model reads first, which is why it is
		// "create_order" and not "post_orders": the former earns a correct call more
		// often. The description is the one sentence telling a model when to use it.
		breeze.MCPTool("create_order", "Creates a new order for a customer"),
	)

	// ── GET /orders/:id — tagged, behind JWT ──────────────────────────────
	//
	// The tag and the authentication middleware are siblings in the same call, and
	// their order does not matter: Handle strips the tag before the chain is built, so
	// the chain is [global..., JWT, handler] either way. An MCP call runs that exact
	// slice — not a copy, not an equivalent — which is what makes the refusal below
	// identical to the HTTP one.
	router.Handle(breeze.GET, "/orders/:id", a.getOrder,
		middleware.DocGET("/orders/{id}", scalar.RouteDoc{
			Title:       "Fetch an order",
			Tags:        []string{"Orders"},
			Description: "Returns one order. Requires a bearer token.",
			Input: []scalar.InputGroup{
				{Type: scalar.InputParams, Fields: orderPathParams{}},
				// Declared so the tool advertises it and a call may carry credentials.
				// See orderAuthHeader for why an undeclared header cannot be sent.
				{Type: scalar.InputHeader, Fields: orderAuthHeader{}},
			},
			Output: map[string]any{},
		}),
		middleware.JWTAuthMiddleware(middleware.JWTOptions{
			AccessSecret:   jwtSecret,
			UserContextKey: jwtContextKey,
		}),
		breeze.MCPTool("get_order", "Fetches one order by its identifier"),
	)

	// ── GET /internal/metrics — documented, deliberately untagged ─────────
	//
	// No MCPTool argument. That is the whole mechanism: r.mcpTools is the only place
	// Auto-MCP learns what exists, and nothing writes to it except a tag on a route.
	router.Handle(breeze.GET, "/internal/metrics", a.internalMetrics,
		middleware.DocGET("/internal/metrics", scalar.RouteDoc{
			Title:       "Internal metrics",
			Tags:        []string{"Internal"},
			Description: "Operational counters. Documented, but never exposed as an MCP tool.",
			Output:      MetricsResponse{},
		}),
	)
}

// ─── main ────────────────────────────────────────────────────────────────────

const (
	// httpPort is the application's own port: the API a browser or curl talks to.
	httpPort = 3000

	// mcpAddr is the Auto-MCP listener. Loopback only, deliberately — see the security
	// note in this file's package comment. A different number from httpPort because
	// these are two protocols on two listeners, and StartInProcess would want a third.
	mcpAddr = "127.0.0.1:2001"
)

func main() {
	secret := jwtSecret()

	router := breeze.NewRouter()
	application := breeze.New(router, breeze.NewEventLoopWorkerPool(runtime.NumCPU()))

	// Scalar first: it calls scalar.Enable(), and Doc* helpers registered before that
	// are dropped. A tagged route with no documentation is a startup error from
	// EnableMCP — which is the good failure, but this is the ordering that avoids it.
	router.Use(middleware.ScalarMiddleware(router, middleware.ScalarOptions{
		Title:       "Auto-MCP Example API",
		Version:     "1.0.0",
		Description: "An order service that publishes two of its three routes as MCP tools.",
		JSONPath:    "/openapi.json",
		UIPath:      "/scalar",
	}))

	service := &app{store: newStore()}
	service.register(router, secret)

	// EnableMCP validates every tag synchronously and returns before listening, so a
	// misdescribed tool stops the process here rather than surfacing as a bad call on
	// an agent's first attempt.
	if err := application.EnableMCP(mcpAddr); err != nil {
		log.Fatalf("automcp-example: Auto-MCP refused to start: %v", err)
	}

	demoToken, err := middleware.GenerateJWT(secret, jwt.MapClaims{
		"user_id": "demo-operator",
		"role":    "operator",
	}, time.Hour, nil)
	if err != nil {
		log.Fatalf("automcp-example: minting the demo token: %v", err)
	}

	fmt.Printf("HTTP      http://127.0.0.1:%d  (docs at /scalar)\n", httpPort)
	fmt.Printf("Auto-MCP  %s  — raw JSON-RPC 2.0 over TCP; tools: create_order, get_order\n", mcpAddr)
	fmt.Printf("Untagged  GET /internal/metrics — in the OpenAPI document, never a tool\n\n")
	fmt.Printf("A one-hour token for get_order:\n  %s\n\n", demoToken)
	fmt.Println("Try:")
	fmt.Printf("  curl -X POST localhost:%d/orders -d '{\"sku\":\"BRZ-100\",\"quantity\":2,\"customer\":\"ada@example.com\"}'\n", httpPort)
	fmt.Printf("  curl localhost:%d/orders/ord-1 -H \"Authorization: Bearer $TOKEN\"\n", httpPort)
	fmt.Printf("  printf '{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/list\"}\\n' | nc %s\n", mcpAddr)
	fmt.Printf("  go test ./cmd/automcp-example   # the same claims, asserted\n\n")

	if err := application.Run(httpPort, true); err != nil {
		log.Fatalf("automcp-example: %v", err)
	}
}

// jwtSecret reads the signing secret, generating an ephemeral one when unset.
//
// # Why generated rather than hardcoded
//
// A literal secret in an example is a secret in every project that copies the example,
// and JWTAuthMiddleware panics on an empty one — correctly, since an empty HMAC key
// verifies any token an attacker signs with "". So the fallback is a random 32 bytes,
// which makes tokens from a previous run stop working. That is the right trade for a
// demo and the wrong one for a deployment, which is what the printed line says.
func jwtSecret() string {
	if s := os.Getenv("BREEZE_EXAMPLE_JWT_SECRET"); s != "" {
		return s
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		log.Fatalf("automcp-example: generating a JWT secret: %v", err)
	}
	secret := hex.EncodeToString(buf)
	fmt.Println("automcp-example: generated an ephemeral JWT secret for this run;")
	fmt.Println("                 set BREEZE_EXAMPLE_JWT_SECRET to keep tokens valid across restarts.")
	return secret
}
