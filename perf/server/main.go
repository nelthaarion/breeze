// rpsbench server: a Breeze app with selectable scenarios, used for A/B load tests.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"syscall"
	"time"

	breeze "github.com/nelthaarion/breeze/v2"
	middleware "github.com/nelthaarion/breeze/v2/middlewares"
)

type User struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Email  string `json:"email"`
	Active bool   `json:"active"`
	Role   string `json:"role"`
}

type Msg struct {
	Message string `json:"message"`
}

var (
	port      = flag.Int("port", 8080, "port")
	scenario  = flag.String("scenario", "plain", "plain|json|rest|api")
	mwList    = flag.String("mw", "", "comma list: log,recover,cors,secure,ratelimit,compress,jwt,etag")
	zerocopy  = flag.Bool("zerocopy", false, "SetZeroCopyHeaders")
	inline    = flag.Bool("inline", true, "SetInlineExecution")
	profdir   = flag.String("profdir", "", "directory for pprof output (SIGUSR1 start, SIGUSR2 stop)")
	tokenOut  = flag.String("tokenout", "", "write a valid JWT to this file")
	multicore = flag.Bool("multicore", true, "multicore")
	nroutes   = flag.Int("nroutes", 1000, "approx number of routes for the rest scenario")
)

const jwtSecret = "bench-secret-bench-secret-bench-secret"

func main() {
	flag.Parse()
	router := breeze.NewRouter()
	pool := breeze.NewEventLoopWorkerPool(runtime.NumCPU())
	app := breeze.New(router, pool)
	app.SetInlineExecution(*inline)
	if *zerocopy {
		app.SetZeroCopyHeaders(true)
	}
	applyNewAPI(app) // no-op on the original tree

	for _, m := range strings.Split(*mwList, ",") {
		switch strings.TrimSpace(m) {
		case "":
		case "log":
			router.Use(middleware.LoggingMiddleware())
		case "recover":
			router.Use(middleware.RecoveryMiddleware())
		case "cors":
			router.Use(middleware.CORSMiddleware(middleware.CORSOptions{
				AllowOrigins: "*", AllowMethods: "GET,POST,PUT,DELETE", AllowHeaders: "Content-Type,Authorization", MaxAge: "600"}))
		case "secure":
			router.Use(middleware.DefaultSecurityMiddleware())
		case "ratelimit":
			router.Use(middleware.NewRateLimiter(middleware.RateLimiterOptions{Requests: 1 << 30, Per: time.Minute}))
		case "compress":
			router.Use(middleware.CompressionMiddleware())
		case "jwt":
			router.Use(middleware.JWTAuthMiddleware(middleware.JWTOptions{AccessSecret: jwtSecret}))
		case "etag":
			router.Use(middleware.NewETagCache().ETagMiddleware())
		default:
			if h := newMW(strings.TrimSpace(m)); h != nil {
				router.Use(h)
				continue
			}
			fmt.Fprintln(os.Stderr, "unknown mw", m)
			os.Exit(2)
		}
	}

	if *tokenOut != "" {
		tok, err := middleware.GenerateJWT(jwtSecret, map[string]any{"user_id": "42", "role": "admin"}, 24*time.Hour, nil)
		if err != nil {
			panic(err)
		}
		_ = os.WriteFile(*tokenOut, []byte(tok), 0o644)
	}

	switch *scenario {
	case "plain":
		router.Handle(breeze.GET, "/plaintext", func(c *breeze.Context) error { return c.WriteString("Hello, World!") })
	case "json":
		router.Handle(breeze.GET, "/json", func(c *breeze.Context) error {
			return c.JSON(map[string]string{"message": "Hello, World!"})
		})
		router.Handle(breeze.GET, "/jsonstruct", func(c *breeze.Context) error { return c.JSON(Msg{Message: "Hello, World!"}) })
	case "api":
		users := make([]User, 8)
		for i := range users {
			users[i] = User{ID: i + 1, Name: "User Number " + strconv.Itoa(i+1), Email: "user" + strconv.Itoa(i+1) + "@example.com", Active: i%2 == 0, Role: "member"}
		}
		router.Handle(breeze.GET, "/users", func(c *breeze.Context) error { return c.JSON(users) })
		router.Handle(breeze.GET, "/users/:id", func(c *breeze.Context) error {
			id, _ := strconv.Atoi(c.Param("id"))
			return c.JSON(User{ID: id, Name: "User " + c.Param("id"), Email: "u@example.com", Active: true, Role: "admin"})
		})
	case "rest":
		registerREST(router, *nroutes)
	default:
		fmt.Fprintln(os.Stderr, "unknown scenario")
		os.Exit(2)
	}

	if *profdir != "" {
		sigc := make(chan os.Signal, 4)
		signal.Notify(sigc, syscall.SIGUSR1, syscall.SIGUSR2)
		go func() {
			var f *os.File
			for s := range sigc {
				if s == syscall.SIGUSR1 {
					f, _ = os.Create(*profdir + "/cpu.pprof")
					_ = pprof.StartCPUProfile(f)
				} else if f != nil {
					pprof.StopCPUProfile()
					f.Close()
					f = nil
					if h, err := os.Create(*profdir + "/allocs.pprof"); err == nil {
						_ = pprof.Lookup("allocs").WriteTo(h, 0)
						h.Close()
					}
					fmt.Fprintln(os.Stderr, "profiles written")
				}
			}
		}()
	}
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-term
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = app.Stop(ctx)
	}()
	if err := app.Run(*port, *multicore); err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
	}
}

// registerREST builds a GitHub-API-shaped table: many resources, each with list/create/get/update/delete
// plus nested sub-resources, in the order an application would grow it.
func registerREST(r *breeze.Router, target int) {
	h := func(c *breeze.Context) error {
		return c.JSON(map[string]string{"ok": "1", "a": c.Param("a"), "b": c.Param("b")})
	}
	resources := []string{"users", "orgs", "teams", "repos", "issues", "pulls", "gists", "hooks", "keys", "labels", "milestones",
		"releases", "events", "notifications", "projects", "boards", "cards", "columns", "assets", "comments", "reviews",
		"commits", "branches", "tags", "forks", "stars", "watchers", "invites", "billing", "audit"}
	n := 0
	add := func(m breeze.Method, p string) {
		if n < target {
			r.Handle(m, p, h)
			n++
		}
	}
	for _, res := range resources {
		add(breeze.GET, "/api/v1/"+res)
		add(breeze.POST, "/api/v1/"+res)
		add(breeze.GET, "/api/v1/"+res+"/:a")
		add(breeze.PUT, "/api/v1/"+res+"/:a")
		add(breeze.DELETE, "/api/v1/"+res+"/:a")
		add(breeze.GET, "/api/v1/"+res+"/:a/sub")
		add(breeze.GET, "/api/v1/"+res+"/:a/sub/:b")
		add(breeze.POST, "/api/v1/"+res+"/:a/sub")
	}
	// A few static routes registered last, as apps do (health, version, metrics).
	for _, p := range []string{"/health", "/version", "/metrics", "/api/v1/ping"} {
		add(breeze.GET, p)
	}
}
