package events

import (
	"errors"
	"fmt"
)

// Stop is a sentinel error a listener may return to halt propagation of
// the current dispatch without signalling a failure.
//
//	events.On(UserCreated{}, func(ctx *events.Context, e UserCreated) error {
//		if e.UserID == 0 {
//			return events.Stop
//		}
//		return nil
//	})
//
// Emit does not return Stop to the caller: it is consumed by the
// dispatcher and reported as a nil error, because stopping is a normal
// control-flow outcome rather than an error. Metrics count a stopped
// dispatch under Stopped, not Failures.
//
// The name is not ErrStop, and that is deliberate rather than an oversight.
// This value exists to be *returned by a listener*, so the call site reads
// `return events.Stop` — a statement about control flow, in a position where
// `return events.ErrStop` would claim something failed. It is documented under this
// name in events/README.md and emitted under it by every generated listener stub,
// so it is also API that cannot be renamed without breaking callers.
//
//lint:ignore ST1012 named for its call site (`return events.Stop`), and exported API.
var Stop = errors.New("events: stop propagation")

// ErrBusClosed is returned by emit and registration APIs after [Bus.Close]
// has been called.
var ErrBusClosed = errors.New("events: bus is closed")

// PanicError wraps a value recovered from a panicking listener or
// middleware. It is returned by a synchronous Emit when the configured
// [Config.PanicMode] is [PanicRecoverAndFail].
type PanicError struct {
	// Event is the name of the event type being dispatched.
	Event string
	// Listener is the name of the listener that panicked, if known.
	Listener string
	// Value is the value passed to panic().
	Value any
	// Stack is the stack trace captured at recovery time.
	Stack []byte
}

// Error implements the error interface.
func (e *PanicError) Error() string {
	if e.Listener != "" {
		return fmt.Sprintf(
			"events: panic in listener %q for event %q: %v",
			e.Listener,
			e.Event,
			e.Value,
		)
	}
	return fmt.Sprintf("events: panic while dispatching %q: %v", e.Event, e.Value)
}

// Unwrap exposes the panic value when it is itself an error, so that
// errors.Is and errors.As can reach it.
func (e *PanicError) Unwrap() error {
	if err, ok := e.Value.(error); ok {
		return err
	}
	return nil
}

// ListenerError associates an error with the listener that produced it.
// It is used by [MultiError] so callers can attribute failures when a
// dispatch continues past the first error.
type ListenerError struct {
	// Listener is the name of the failing listener.
	Listener string
	// Err is the error the listener returned.
	Err error
}

// Error implements the error interface.
func (e *ListenerError) Error() string {
	return fmt.Sprintf("%s: %v", e.Listener, e.Err)
}

// Unwrap returns the underlying listener error.
func (e *ListenerError) Unwrap() error { return e.Err }

// MultiError aggregates every error produced by a dispatch that was
// configured to continue past failures ([Config.ContinueOnError]).
type MultiError struct {
	// Errors holds one entry per failing listener, in execution order.
	Errors []error
}

// Error implements the error interface.
func (m *MultiError) Error() string {
	switch len(m.Errors) {
	case 0:
		return "events: no errors"
	case 1:
		return m.Errors[0].Error()
	default:
		return fmt.Sprintf("events: %d listeners failed: %v (and %d more)",
			len(m.Errors), m.Errors[0], len(m.Errors)-1)
	}
}

// Unwrap returns the aggregated errors so errors.Is and errors.As can
// traverse all of them.
func (m *MultiError) Unwrap() []error { return m.Errors }

// asError collapses a slice of collected errors into a single error.
// It returns nil for an empty slice and the sole error for a slice of one,
// avoiding a MultiError allocation in the common cases.
func asError(errs []error) error {
	switch len(errs) {
	case 0:
		return nil
	case 1:
		return errs[0]
	default:
		return &MultiError{Errors: errs}
	}
}
