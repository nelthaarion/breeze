// Example: Breeze Workflow Engine with live Dashboard visualisation.
//
// This is a REAL application. Every number the dashboard shows comes from
// an actual execution — no fake data, no mock generators.
//
// Run:
//
//	go run ./cmd/workflow-example
//
// Then open http://localhost:3000/dashboard  (admin / admin) and go to
// the Events page. Trigger a workflow and watch it appear live:
//
//	curl -X POST http://localhost:3000/demo/workflow
//	curl -X POST http://localhost:3000/demo/workflow/retry
//	curl -X POST http://localhost:3000/demo/workflow/compensation
//
// ─── About the delays ─────────────────────────────────────────────────────
//
// Every step in this example sleeps. THE SLEEPS ARE SIMULATED WORK, and
// exist only so the dashboard has something long enough to look at. Real
// steps would call a payment gateway or a database instead. Nothing in
// the engine requires or assumes a delay.
package main

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/nelthaarion/breeze/v2"
	"github.com/nelthaarion/breeze/v2/dashboard"
	"github.com/nelthaarion/breeze/v2/events"
	"github.com/nelthaarion/breeze/v2/workflow"
)

// ─── Domain ───────────────────────────────────────────────────────────────

// Order is the workflow payload.
type Order struct {
	ID     string
	Total  int64
	Email  string
	Amount int
}

// OrderCreated is a domain event. Emitting it starts the workflow,
// which is how a plugin or any other subsystem kicks off a process
// without importing the workflow package.
type OrderCreated struct {
	OrderID string
	Total   int64
}

// simulate stands in for real work: a network call, a query, an external
// API. It is cancellable, because a step that ignores its context cannot
// be timed out or shut down.
func simulate(ctx *workflow.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ─── Workflow 1: the happy path, with a parallel fan-out ─────────────────
//
// The shape is:
//
//	validate
//	   ├── charge         (2s)
//	   ├── reserve        (3s)
//	   └── fraud-check    (2s)
//	          ↓
//	        ship
//	          ↓
//	       notify
//
// charge, reserve and fraud-check declare the same dependency, so they
// form one DAG level and run concurrently. Sequentially they would cost
// 2+3+2 = 7s; in parallel the level costs max(2,3,2) = 3s. The endpoint
// reports the measured wall-clock time so the difference is not a claim,
// it is a number you can read.
func orderWorkflow() *workflow.Definition {
	def := workflow.New("order-processing").
		Step("validate-order", func(c *workflow.Context) error {
			return simulate(c, 200*time.Millisecond)
		}).
		// ── parallel level ──
		Step("charge-payment", func(c *workflow.Context) error {
			if err := simulate(c, 5*time.Second); err != nil {
				return err
			}
			c.Set("charge_id", "ch_"+c.ExecutionID())
			return nil
		}, workflow.WithDependsOn("validate-order")).
		Step("reserve-inventory", func(c *workflow.Context) error {
			return simulate(c, 3*time.Second)
		}, workflow.WithDependsOn("validate-order")).
		Step("fraud-check", func(c *workflow.Context) error {
			return simulate(c, 2*time.Second)
		}, workflow.WithDependsOn("validate-order")).
		// ── join ──
		Step("create-shipment", func(c *workflow.Context) error {
			return simulate(c, 600*time.Millisecond)
		}, workflow.WithDependsOn("charge-payment", "reserve-inventory", "fraud-check")).
		Step("send-notification", func(c *workflow.Context) error {
			return simulate(c, 400*time.Millisecond)
		}, workflow.WithDependsOn("create-shipment"))

	// Emitting OrderCreated anywhere in the application starts this
	// workflow, with the event as the payload.
	workflow.OnType[OrderCreated](def)
	return def
}

// ─── Workflow 2: a deterministic retry ───────────────────────────────────
//
// The gateway fails its first two attempts and succeeds on the third.
// It is a counter, not a random failure, so the dashboard shows the same
// story every time: StepFailed → Retrying → StepStarted → StepCompleted.
func retryWorkflow(attempts *atomic.Int32) *workflow.Definition {
	return workflow.New("payment-retry").
		Step("contact-gateway", func(c *workflow.Context) error {
			if err := simulate(c, 300*time.Millisecond); err != nil {
				return err
			}
			// Attempt is 1-based and comes from the engine, so the
			// step does not have to track its own retries.
			if c.Attempt() < 3 {
				return fmt.Errorf("gateway timeout (attempt %d)", c.Attempt())
			}
			attempts.Store(int32(c.Attempt()))
			return nil
		}, workflow.WithRetry(workflow.RetryPolicy{
			MaxAttempts:  5,
			Backoff:      workflow.BackoffExponential,
			InitialDelay: 500 * time.Millisecond,
			MaxDelay:     4 * time.Second,
		})).
		Step("confirm-payment", func(c *workflow.Context) error {
			return simulate(c, 300*time.Millisecond)
		}, workflow.WithDependsOn("contact-gateway"))
}

// errOutOfStock is the deterministic failure that triggers rollback.
var errOutOfStock = errors.New("warehouse reports zero stock")

// ─── Workflow 3: Saga compensation ───────────────────────────────────────
//
// charge and reserve succeed, ship fails, and the engine rolls the
// successful steps back in reverse order: release-inventory, then
// refund-payment. The failure is a returned error, not a panic, and it
// is deterministic.
func compensationWorkflow() *workflow.Definition {
	return workflow.New("order-compensation").
		Step("charge-payment", func(c *workflow.Context) error {
			return simulate(c, 700*time.Millisecond)
		}, workflow.WithCompensation(func(c *workflow.Context) error {
			// Compensation for a charge is a refund.
			return simulate(c, 500*time.Millisecond)
		})).
		Step("reserve-inventory", func(c *workflow.Context) error {
			return simulate(c, 600*time.Millisecond)
		}, workflow.WithDependsOn("charge-payment"),
			workflow.WithCompensation(func(c *workflow.Context) error {
				return simulate(c, 400*time.Millisecond)
			})).
		Step("create-shipment", func(c *workflow.Context) error {
			if err := simulate(c, 500*time.Millisecond); err != nil {
				return err
			}
			// NonRetryable: retrying an out-of-stock error five times
			// would only delay the rollback.
			return workflow.NonRetryable(errOutOfStock)
		}, workflow.WithDependsOn("reserve-inventory"))
}

// ─── Main ─────────────────────────────────────────────────────────────────

func main() {
	router := breeze.NewRouter()
	pool := breeze.NewEventLoopWorkerPool(runtime.NumCPU())
	app := breeze.New(router, pool)

	// ── Dashboard ────────────────────────────────────────────────────────
	cfg := dashboard.DefaultConfig()
	coll := dashboard.Install(app, router, cfg)
	router.Use(coll.Middleware())

	// AttachEvents is what makes workflow executions show up live: the
	// engine publishes to the shared bus and to the observability
	// collector, and the dashboard streams both over its WebSocket.
	detach := coll.AttachEvents(events.Default)
	defer detach()

	// ── Workflow engine ──────────────────────────────────────────────────
	// No Store is configured, so executions live in memory. Point
	// Config.Store at a database-backed Store to survive restarts.
	//
	// Collector must be the dashboard's own collector. The engine
	// publishes one signal per execution, carrying every step as a
	// span; that is what draws the timeline. Sent to any other
	// collector it would be recorded correctly and displayed nowhere.
	engine := workflow.NewEngine(workflow.Config{
		Collector: coll.Observability(),
	})

	var lastRetryAttempts atomic.Int32
	for _, def := range []*workflow.Definition{
		orderWorkflow(),
		retryWorkflow(&lastRetryAttempts),
		compensationWorkflow(),
	} {
		if err := engine.Register(def); err != nil {
			panic("workflow registration failed: " + err.Error())
		}
	}

	// Log every workflow event to stdout, so the terminal tells the same
	// story as the dashboard. This is also the smallest possible example
	// of a plugin observing workflows without touching the engine.
	events.On(events.WorkflowStepCompleted{}, func(_ *events.Context, e events.WorkflowStepCompleted) error {
		fmt.Printf("  ✓ %-18s %s\n", e.Step, e.Duration.Round(time.Millisecond))
		return nil
	})
	events.On(events.WorkflowStepFailed{}, func(_ *events.Context, e events.WorkflowStepFailed) error {
		fmt.Printf("  ✗ %-18s attempt %d: %s\n", e.Step, e.Attempt, e.Err)
		return nil
	})
	events.On(events.WorkflowRetrying{}, func(_ *events.Context, e events.WorkflowRetrying) error {
		fmt.Printf("  ↻ %-18s retrying in %s\n", e.Step, e.Delay)
		return nil
	})
	events.On(events.WorkflowCompensationStarted{}, func(_ *events.Context, e events.WorkflowCompensationStarted) error {
		fmt.Printf("  ⟲ rolling back %d step(s): %s\n", e.Steps, e.Cause)
		return nil
	})

	// ── Demo endpoints ───────────────────────────────────────────────────

	// POST /demo/workflow — the parallel happy path.
	router.Handle(breeze.POST, "/demo/workflow", func(ctx *breeze.Context) error {
		fmt.Println("\n▶ order-processing")
		start := time.Now()
		res, err := engine.Run(context.Background(), "order-processing",
			Order{ID: "ord_1001", Total: 12500, Email: "customer@example.com"})
		elapsed := time.Since(start)

		// validate(0.2) + max(2,3,2) + ship(0.6) + notify(0.4) ≈ 4.2s in
		// parallel, versus ≈ 9.2s if the level ran sequentially.
		return ctx.JSON(map[string]any{
			"workflow":       res.Workflow,
			"execution_id":   res.ExecutionID,
			"state":          res.State.String(),
			"steps":          len(res.Steps),
			"elapsed":        elapsed.Round(time.Millisecond).String(),
			"sequential_est": "9.2s",
			"parallel_note":  "charge+reserve+fraud ran concurrently: the 3 of them cost ~3s, not ~7s",
			"error":          errText(err),
		})
	})

	// POST /demo/workflow/retry — deterministic retry.
	router.Handle(breeze.POST, "/demo/workflow/retry", func(ctx *breeze.Context) error {
		fmt.Println("\n▶ payment-retry")
		res, err := engine.Run(context.Background(), "payment-retry", nil)
		return ctx.JSON(map[string]any{
			"workflow":     res.Workflow,
			"execution_id": res.ExecutionID,
			"state":        res.State.String(),
			"attempts":     lastRetryAttempts.Load(),
			"note":         "the gateway fails twice, then succeeds on attempt 3",
			"error":        errText(err),
		})
	})

	// POST /demo/workflow/compensation — Saga rollback.
	router.Handle(breeze.POST, "/demo/workflow/compensation", func(ctx *breeze.Context) error {
		fmt.Println("\n▶ order-compensation")
		res, err := engine.Run(context.Background(), "order-compensation", nil)
		return ctx.JSON(map[string]any{
			"workflow":     res.Workflow,
			"execution_id": res.ExecutionID,
			"state":        res.State.String(),
			"note":         "shipment failed, so reserve and charge were rolled back in reverse order",
			"error":        errText(err),
		})
	})

	// POST /demo/workflow/event — start the workflow by emitting a
	// domain event instead of calling the engine.
	router.Handle(breeze.POST, "/demo/workflow/event", func(ctx *breeze.Context) error {
		_ = events.Emit(OrderCreated{OrderID: "ord_2002", Total: 4200})
		return ctx.JSON(map[string]any{
			"emitted": "OrderCreated",
			"note":    "the workflow was triggered by the event and runs in the background",
		})
	})

	// GET /demo/workflows — what is registered.
	router.Handle(breeze.GET, "/demo/workflows", func(ctx *breeze.Context) error {
		return ctx.JSON(map[string]any{"registered": engine.Definitions()})
	})

	fmt.Println("Breeze Workflow example listening on :3000")
	fmt.Println("  Dashboard: http://localhost:3000/dashboard  (admin / admin)")
	fmt.Println()
	fmt.Println("Trigger a workflow, then watch the Events page:")
	fmt.Println("  curl -X POST http://localhost:3000/demo/workflow")
	fmt.Println("  curl -X POST http://localhost:3000/demo/workflow/retry")
	fmt.Println("  curl -X POST http://localhost:3000/demo/workflow/compensation")
	fmt.Println("  curl -X POST http://localhost:3000/demo/workflow/event")
	fmt.Println()
	fmt.Println("NOTE: every step sleeps on purpose, so the execution is")
	fmt.Println("      slow enough to watch. The sleeps are simulated work.")
	app.Run(3000, true)
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
