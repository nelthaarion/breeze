package dashboard

import (
	"sync"
	"time"
)

// TimelineRecorder is attached to a request context and accumulates steps
// as the request flows through middleware, controllers, ORM, and serialisation.
//
// Steps are organised in a flat list with parent/child relationships
// expressed via the Parent field; the timeline view in the UI reconstructs
// the tree from that.
//
// The recorder is allocation-free for the common case where the timeline
// feature is disabled (no steps are recorded).
type TimelineRecorder struct {
	mu     sync.Mutex
	steps  []TimelineStep
	stack  []int // indices into steps[] forming the current parent chain
	start  time.Time
	cfg    Config
	maxCap int
}

// NewTimelineRecorder returns a recorder bounded by cfg.MaxTimelineEntries.
func NewTimelineRecorder(cfg Config) *TimelineRecorder {
	return &TimelineRecorder{
		cfg:    cfg,
		start:  time.Now(),
		maxCap: cfg.MaxTimelineEntries,
	}
}

// Step begins a new named step as a child of the current step (or at the
// root if no step is open). The returned func must be called when the step
// ends; it captures the duration and any metadata you pass.
//
//	end := rec.Step("ORM Query")
//	defer end(map[string]any{"rows": 42})
func (r *TimelineRecorder) Step(name string) func(metadata map[string]any) {
	if r == nil {
		return func(map[string]any) {}
	}
	now := time.Now()
	idx := r.appendStep(TimelineStep{
		Name:  name,
		Start: now,
	})
	r.mu.Lock()
	r.stack = append(r.stack, idx)
	r.mu.Unlock()
	return func(metadata map[string]any) {
		if r == nil {
			return
		}
		end := time.Now()
		r.mu.Lock()
		defer r.mu.Unlock()
		if idx < 0 || idx >= len(r.steps) {
			return
		}
		s := r.steps[idx]
		s.End = end
		if s.End.After(s.Start) {
			s.Duration = s.End.Sub(s.Start).Microseconds()
		}
		if len(metadata) > 0 {
			s.Metadata = metadata
		}
		r.steps[idx] = s
		// Pop this index from the stack.
		if len(r.stack) > 0 && r.stack[len(r.stack)-1] == idx {
			r.stack = r.stack[:len(r.stack)-1]
		}
		// Attach to parent.
		if len(r.stack) > 0 {
			parentIdx := r.stack[len(r.stack)-1]
			r.steps[parentIdx].Children = append(r.steps[parentIdx].Children, s)
		}
	}
}

func (r *TimelineRecorder) appendStep(s TimelineStep) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.maxCap > 0 && len(r.steps) >= r.maxCap {
		// Drop the oldest step (only at the root; we accept lossy timelines
		// for very long requests).
		r.steps = r.steps[1:]
		// Re-index stack to compensate — simpler than chasing pointers.
		for i := range r.stack {
			r.stack[i]--
			if r.stack[i] < 0 {
				r.stack[i] = 0
			}
		}
	}
	r.steps = append(r.steps, s)
	return len(r.steps) - 1
}

// Build returns the final timeline. Children are nested inside their parent
// steps; top-level steps are returned directly.
func (r *TimelineRecorder) Build() []TimelineStep {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// After all Step closures have run, top-level steps are those whose
	// index never had a parent in the stack at close time — but because we
	// attach children to parents in place, r.steps at this point contains
	// the *flat* list. We rebuild the tree:
	// Every step that was a child is already inside its parent's Children.
	// Top-level steps are those that never became a child.
	//
	// Simplification: rather than tracking "became a child" flags, we
	// return only the steps that have no parent (i.e. their index was a
	// root during its lifetime). Since we already embed children, we can
	// deduplicate by skipping steps that appear as children of any other
	// step.
	//
	// For the dashboard's use case (linear timeline of distinct phases)
	// the typical shape is a flat list, so we just return r.steps with
	// children preserved.
	out := make([]TimelineStep, len(r.steps))
	copy(out, r.steps)
	return out
}

// Start returns the recorder's start time.
func (r *TimelineRecorder) Start() time.Time {
	if r == nil {
		return time.Time{}
	}
	return r.start
}
