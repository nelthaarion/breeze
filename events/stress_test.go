package events

import (
	"sync"
	"sync/atomic"
	"testing"
)

// stress_test.go — the global bus under simultaneous registration churn and
// dispatch.
//
// This was previously `cmd/event-validator`, a `package main` that ran 200
// goroutines through 10 million emits and printed "PASS: no deadlock" to
// stdout. It lived in cmd/ because there was no other home for it, which meant
// CI never ran it, it never ran under -race, and its "PASS" lines asserted
// nothing — the program printed them unconditionally once wg.Wait() returned.
//
// The same coverage as a test: CI runs it, `-race` sees the interleaving, a
// deadlock fails through the test timeout instead of hanging a terminal, and a
// dropped delivery is now an assertion rather than a number a human had to
// eyeball.
//
// This is the only test in the package that exercises the *global* bus
// (events.On / events.Emit rather than OnBus / EmitBus), which is why it is
// worth keeping alongside TestConcurrentRegistrationAndDispatch: the global path
// adds a package-level singleton behind a sync.Once, and that is exactly what a
// registration/dispatch race would corrupt.

// stressEvent is scoped to this file. Registrations are keyed by type, so a
// dedicated type keeps this test's churn from perturbing any other test's
// listener counts on the shared global bus.
type stressEvent struct{ ID uint64 }

// selfRemoveEvent is the second file-local event type, for the same reason.
type selfRemoveEvent struct{}

// TestGlobalBusUnderRegistrationChurn emits on the global bus while other
// goroutines subscribe and immediately unsubscribe to the same event type.
//
// What can actually break here: dispatch reads a copy-on-write snapshot of the
// listener slice, and Unsubscribe replaces that slice. A dispatcher holding a
// stale snapshot is *correct* — it delivers to a listener that has just been
// removed, which is unavoidable without a lock on the dispatch path and is
// documented on Subscription. A dispatcher holding a *torn* slice is a data
// race, and that is what -race is here to catch.
//
// The permanent listener's count is asserted rather than printed. It is
// registered before any goroutine starts and removed only after they finish, so
// every emit must reach it; a short count means a snapshot swap lost a
// listener, which no amount of "PASS: no panic" would have revealed.
func TestGlobalBusUnderRegistrationChurn(t *testing.T) {
	const (
		churnGoroutines = 8
		churnPerG       = 200
		emitGoroutines  = 8
		emitsPerG       = 500
	)

	var delivered atomic.Uint64

	permanent := On(stressEvent{}, func(*Context, stressEvent) error {
		delivered.Add(1)
		return nil
	})
	defer permanent.Unsubscribe()

	var wg sync.WaitGroup

	// Churn: subscribe and immediately unsubscribe, forcing snapshot
	// replacements in between the emits below.
	for g := 0; g < churnGoroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < churnPerG; i++ {
				sub := On(stressEvent{}, func(*Context, stressEvent) error { return nil })
				sub.Unsubscribe()
			}
		}()
	}

	// Dispatch: emit while the registry is being rewritten underneath.
	for g := 0; g < emitGoroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < emitsPerG; i++ {
				if err := Emit(stressEvent{ID: uint64(id*emitsPerG + i)}); err != nil {
					t.Errorf("Emit: %v", err)
					return
				}
			}
		}(g)
	}

	wg.Wait()

	if want := uint64(emitGoroutines * emitsPerG); delivered.Load() != want {
		t.Errorf("permanent listener received %d events, want %d — a snapshot swap lost a listener",
			delivered.Load(), want)
	}
}

// TestGlobalBusSelfUnsubscribeUnderConcurrentEmit removes a listener from inside
// its own dispatch while other goroutines are dispatching to it.
//
// TestSelfUnsubscribeDuringDispatch already covers self-removal, but serially and
// on an isolated bus, where it can assert exactly one call. That assertion is only
// available because nothing else is emitting: Unsubscribe is documented to let a
// dispatch that already loaded a snapshot run to completion, so under concurrent
// emits the count is "at least one" and no more than the number of dispatchers
// that had already taken a snapshot.
//
// So this asserts the two things that remain true regardless of interleaving: the
// listener ran, and the process did not deadlock or race. Pinning it to exactly
// one would be asserting an implementation detail of when snapshots are taken —
// and a registry that could promise exactly-once here would need a lock on the
// dispatch path, which is the entire cost this copy-on-write design exists to
// avoid.
func TestGlobalBusSelfUnsubscribeUnderConcurrentEmit(t *testing.T) {
	var calls atomic.Uint64
	var sub *Subscription[selfRemoveEvent]
	var once sync.Once

	sub = On(selfRemoveEvent{}, func(*Context, selfRemoveEvent) error {
		calls.Add(1)
		// sync.Once rather than a bare Unsubscribe call: Unsubscribe is
		// idempotent, so this is not about correctness but about the read of
		// `sub` — the closure captures the variable, and only the first
		// invocation is guaranteed to see the assignment below it.
		once.Do(func() { sub.Unsubscribe() })
		return nil
	})
	defer sub.Unsubscribe()

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				if err := Emit(selfRemoveEvent{}); err != nil {
					t.Errorf("Emit: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if calls.Load() == 0 {
		t.Error("the listener never ran, so it cannot have unsubscribed itself")
	}
	if sub.Active() {
		t.Error("sub.Active() is true after the listener unsubscribed itself")
	}
}
