package workflow

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// WorkflowRecord is the durable state of one execution.
type WorkflowRecord struct {
	ExecutionID    string            `json:"execution_id"`
	Workflow       string            `json:"workflow"`
	Version        int               `json:"version"`
	State          State             `json:"state"`
	IdempotencyKey string            `json:"idempotency_key,omitempty"`
	CorrelationID  string            `json:"correlation_id,omitempty"`
	Payload        []byte            `json:"payload,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
	Err            string            `json:"error,omitempty"`
	StartedAt      time.Time         `json:"started_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
	FinishedAt     time.Time         `json:"finished_at,omitempty"`
}

// StepRecord is the durable state of one step within an execution.
type StepRecord struct {
	ExecutionID string    `json:"execution_id"`
	Step        string    `json:"step"`
	State       State     `json:"state"`
	Attempt     int       `json:"attempt"`
	Err         string    `json:"error,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	FinishedAt  time.Time `json:"finished_at,omitempty"`
	Compensated bool      `json:"compensated,omitempty"`
}

// Store persists workflow state so an execution can outlive the process
// that started it.
//
// It is an interface, and deliberately a small one, because Breeze does
// not choose a database for you: BreezeORM, database/sql, GORM, Ent,
// Bun or none at all are all valid, and none of them is imported here.
//
// Implementations must be safe for concurrent use. The engine never
// holds a lock across a call into a Store, so an implementation is free
// to block on I/O.
type Store interface {
	// CreateWorkflow persists a new execution. It must return an error
	// wrapping [ErrWorkflowAlreadyExist] when the ExecutionID is taken.
	CreateWorkflow(ctx context.Context, rec WorkflowRecord) error

	// GetWorkflow returns an execution, or an error wrapping
	// [ErrWorkflowNotFound].
	GetWorkflow(ctx context.Context, executionID string) (WorkflowRecord, error)

	// UpdateWorkflow overwrites an existing execution.
	UpdateWorkflow(ctx context.Context, rec WorkflowRecord) error

	// DeleteWorkflow removes an execution and its steps.
	DeleteWorkflow(ctx context.Context, executionID string) error

	// SaveStep inserts or overwrites one step's state.
	SaveStep(ctx context.Context, rec StepRecord) error

	// GetStep returns one step, or an error wrapping
	// [ErrWorkflowNotFound].
	GetStep(ctx context.Context, executionID, step string) (StepRecord, error)

	// ListSteps returns every persisted step of an execution.
	ListSteps(ctx context.Context, executionID string) ([]StepRecord, error)

	// PendingWorkflows returns the executions that were not in a
	// terminal state when the process stopped, so [Engine.Resume] can
	// continue them.
	PendingWorkflows(ctx context.Context) ([]WorkflowRecord, error)

	// FindByIdempotencyKey returns the existing execution for a key,
	// which is what makes a repeated trigger a no-op.
	FindByIdempotencyKey(ctx context.Context, workflow, key string) (WorkflowRecord, bool, error)
}

// MemoryStore keeps workflow state in memory.
//
// It is the default, and it is enough for development, tests and
// workflows that do not need to survive a restart. Its durability is
// process-scoped: [Engine.Resume] will recover executions interrupted
// by an engine restart, but nothing survives the process exiting.
type MemoryStore struct {
	mu    sync.RWMutex
	flows map[string]WorkflowRecord
	steps map[string]map[string]StepRecord
}

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		flows: make(map[string]WorkflowRecord),
		steps: make(map[string]map[string]StepRecord),
	}
}

// cloneWorkflow copies a record and its maps and slices, so neither
// side can mutate the other's state through a retained reference.
func cloneWorkflow(r WorkflowRecord) WorkflowRecord {
	if r.Metadata != nil {
		m := make(map[string]string, len(r.Metadata))
		for k, v := range r.Metadata {
			m[k] = v
		}
		r.Metadata = m
	}
	if r.Payload != nil {
		r.Payload = append([]byte(nil), r.Payload...)
	}
	return r
}

func (m *MemoryStore) CreateWorkflow(_ context.Context, rec WorkflowRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.flows[rec.ExecutionID]; exists {
		return fmt.Errorf("%w: %s", ErrWorkflowAlreadyExist, rec.ExecutionID)
	}
	m.flows[rec.ExecutionID] = cloneWorkflow(rec)
	return nil
}

func (m *MemoryStore) GetWorkflow(_ context.Context, executionID string) (WorkflowRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rec, ok := m.flows[executionID]
	if !ok {
		return WorkflowRecord{}, fmt.Errorf("%w: %s", ErrWorkflowNotFound, executionID)
	}
	return cloneWorkflow(rec), nil
}

func (m *MemoryStore) UpdateWorkflow(_ context.Context, rec WorkflowRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.flows[rec.ExecutionID]; !ok {
		return fmt.Errorf("%w: %s", ErrWorkflowNotFound, rec.ExecutionID)
	}
	m.flows[rec.ExecutionID] = cloneWorkflow(rec)
	return nil
}

func (m *MemoryStore) DeleteWorkflow(_ context.Context, executionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.flows, executionID)
	delete(m.steps, executionID)
	return nil
}

func (m *MemoryStore) SaveStep(_ context.Context, rec StepRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	byStep, ok := m.steps[rec.ExecutionID]
	if !ok {
		byStep = make(map[string]StepRecord, 4)
		m.steps[rec.ExecutionID] = byStep
	}
	byStep[rec.Step] = rec
	return nil
}

func (m *MemoryStore) GetStep(_ context.Context, executionID, step string) (StepRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rec, ok := m.steps[executionID][step]
	if !ok {
		return StepRecord{}, fmt.Errorf("%w: %s/%s", ErrWorkflowNotFound, executionID, step)
	}
	return rec, nil
}

func (m *MemoryStore) ListSteps(_ context.Context, executionID string) ([]StepRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	byStep := m.steps[executionID]
	out := make([]StepRecord, 0, len(byStep))
	for _, rec := range byStep {
		out = append(out, rec)
	}
	return out, nil
}

func (m *MemoryStore) PendingWorkflows(_ context.Context) ([]WorkflowRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []WorkflowRecord
	for _, rec := range m.flows {
		if !rec.State.Terminal() {
			out = append(out, cloneWorkflow(rec))
		}
	}
	return out, nil
}

func (m *MemoryStore) FindByIdempotencyKey(
	_ context.Context,
	workflow, key string,
) (WorkflowRecord, bool, error) {
	if key == "" {
		return WorkflowRecord{}, false, nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, rec := range m.flows {
		if rec.Workflow == workflow && rec.IdempotencyKey == key {
			return cloneWorkflow(rec), true, nil
		}
	}
	return WorkflowRecord{}, false, nil
}

// Len returns the number of stored executions.
func (m *MemoryStore) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.flows)
}

// Reset discards everything. It exists for tests.
func (m *MemoryStore) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.flows = make(map[string]WorkflowRecord)
	m.steps = make(map[string]map[string]StepRecord)
}
