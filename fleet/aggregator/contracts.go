package aggregator

import (
	"context"
	"time"

	"github.com/nelthaarion/breeze/v2/fleet"
	"github.com/nelthaarion/breeze/v2/fleet/contracts"
)

const contractQueueSize = 2048

type contractJob struct {
	span   fleet.Span
	caller string
}
type schemaJob struct{ heartbeat fleet.Heartbeat }

// contractEngine keeps fetch and validation entirely off ingestion. Producers
// use bounded, non-blocking queues; overload drops validation work, never spans.
type contractEngine struct {
	registry   *contracts.SchemaRegistry
	violations *contracts.ViolationStore
	validate   chan contractJob
	schemas    chan schemaJob
	stop       chan struct{}
	done       chan struct{}
	hub        func(contracts.Group)
}

func newContractEngine(cfg Config) *contractEngine {
	e := &contractEngine{
		registry:   contracts.NewSchemaRegistry(nil),
		violations: contracts.NewViolationStore(cfg.MaxViolations, cfg.ViolationDedupeWindow),
		validate:   make(chan contractJob, contractQueueSize),
		schemas:    make(chan schemaJob, 128),
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}
	go e.run(cfg)
	return e
}

func (e *contractEngine) enqueueSpan(span fleet.Span, caller string) {
	if len(span.RequestPayload) == 0 && len(span.ResponsePayload) == 0 {
		return
	}
	select {
	case e.validate <- contractJob{span, caller}:
	default:
	}
}

func (e *contractEngine) enqueueSchema(hb fleet.Heartbeat) {
	if hb.OpenAPIHash == "" || hb.OpenAPIURL == "" {
		return
	}
	select {
	case e.schemas <- schemaJob{hb}:
	default:
	}
}

func (e *contractEngine) run(cfg Config) {
	defer close(e.done)
	for {
		select {
		case <-e.stop:
			return
		case job := <-e.schemas:
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_, err := e.registry.Refresh(
				ctx,
				job.heartbeat.Service,
				job.heartbeat.OpenAPIHash,
				job.heartbeat.OpenAPIURL,
			)
			cancel()
			if err != nil && cfg.Logger != nil {
				cfg.Logger(
					"warning",
					"fleet OpenAPI refresh failed for "+job.heartbeat.Service+": "+err.Error(),
					"fleet",
				)
			}
		case job := <-e.validate:
			op, ok := e.registry.Operation(job.span.Service, job.span.Route, job.span.Method)
			if !ok {
				continue
			}
			for _, v := range contracts.Validate(job.span, job.caller, op, time.Now().UnixNano()) {
				g := e.violations.Add(v)
				if e.hub != nil {
					e.hub(g)
				}
			}
		}
	}
}
func (e *contractEngine) close() { close(e.stop); <-e.done }
