// Package workflow is the durable orchestration layer of the Breeze
// Framework.
//
// A workflow is an ordered set of steps that must all happen, in order,
// with retries, timeouts and rollback when something in the middle
// fails. The engine runs in-process by default and needs no broker, no
// database and no external service to be useful.
//
// # Defining a workflow
//
//	def := workflow.New("order-processing").
//		Step("validate-payment", ValidatePayment).
//		Step("charge-payment", ChargePayment).
//		Step("create-shipment", CreateShipment)
//
//	engine := workflow.NewEngine()
//	engine.Register(def)
//	res, err := engine.Run(ctx, "order-processing", order)
//
// That is the whole API for the simple case. Everything below is
// opt-in: a workflow that sets nothing gets sequential execution, no
// retries, no timeout and no persistence, and costs one goroutine.
//
// # Steps
//
// A step is a function of the workflow [Context]:
//
//	func ChargePayment(ctx *workflow.Context) error {
//		order, _ := workflow.Payload[Order](ctx)
//		return billing.Charge(ctx.Ctx, order.Total)
//	}
//
// Steps communicate through the context's metadata, which is shared by
// every step of one execution and visible to the step after them.
//
// # Ordering
//
// A step with no declared dependency runs after the step declared
// before it, which is why the example above is sequential without
// saying so. Declaring dependencies opts into a DAG:
//
//	Step("charge", Charge, workflow.WithDependsOn("validate")),
//	Step("reserve", Reserve, workflow.WithDependsOn("validate")),
//	Step("ship", Ship, workflow.WithDependsOn("charge", "reserve")),
//
// Here charge and reserve run concurrently and ship waits for both.
// The graph is validated at registration time — cycles, duplicate
// names, unknown dependencies and unreachable steps are rejected
// before anything runs.
//
// # Retries and timeouts
//
//	Step("charge", Charge,
//		workflow.WithTimeout(10*time.Second),
//		workflow.WithRetry(workflow.RetryPolicy{
//			MaxAttempts:  5,
//			Backoff:      workflow.BackoffExponential,
//			InitialDelay: 500 * time.Millisecond,
//			MaxDelay:     30 * time.Second,
//			Jitter:       0.2,
//		}))
//
// A timeout cancels the step's context and counts as a failure, so a
// retry policy applies to it like any other. Wrap an error in
// [NonRetryable] to stop retrying immediately.
//
// # Compensation
//
// When a step fails, the steps that already succeeded are rolled back
// in reverse order:
//
//	Step("charge", Charge, workflow.WithCompensation(Refund)),
//	Step("reserve", Reserve, workflow.WithCompensation(Release)),
//
// If reserve fails, Refund runs. Steps without a compensation handler
// are skipped during rollback rather than blocking it.
//
// # Durability
//
// With a [Store] configured, workflow and step state is persisted as it
// changes. After a crash, [Engine.Resume] replays the executions that
// were still running: completed steps are not re-executed, and the
// workflow continues from the first step that had not finished.
//
// # Events
//
// The engine publishes through the Breeze event bus — it does not own
// one. Every lifecycle moment is an event in
// [github.com/nelthaarion/breeze/v2/events], from WorkflowStarted to
// WorkflowCompensationFailed, so the dashboard and any listener see
// workflow activity with no extra wiring.
//
// # Concurrency model
//
// Steps within one level run on a bounded worker pool; the engine
// never spawns an unbounded number of goroutines. Definitions are
// immutable once registered, so dispatch reads them without a lock,
// and no lock is ever held while user code runs.
package workflow
