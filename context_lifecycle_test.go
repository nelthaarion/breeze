package breeze

import (
	"context"
	"runtime"
	"testing"
	"time"
)

// TestWorkerPool_NoGoroutineLeak verifies that the worker pool does not
// leak goroutines. After Shutdown, all workers should exit.
//
// This is a regression test for the goroutine leak bug: the original
// WorkerPool had no panic recovery in the exec closure, so a panicking
// handler would crash the worker goroutine. The WaitGroup counter would
// never be decremented (the defer Done() never ran), causing Shutdown to
// block forever.
func TestWorkerPool_NoGoroutineLeak(t *testing.T) {
	before := runtime.NumGoroutine()

	pool := NewWorkerPool(4)
	for i := 0; i < 100; i++ {
		pool.Submit(func() {
			time.Sleep(time.Millisecond)
		})
	}

	time.Sleep(100 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pool.Shutdown(ctx)

	time.Sleep(200 * time.Millisecond)

	after := runtime.NumGoroutine()
	leaked := after - before
	if leaked > 1 {
		t.Errorf("goroutine leak: before=%d, after=%d, leaked=%d", before, after, leaked)
	}
}

// TestWorkerPool_PanicDoesNotLeak verifies that a panicking handler does
// not leak the worker goroutine or the WaitGroup counter.
//
// This is the direct regression test for the crash scenario. Before the
// fix, a panic in a handler would:
//  1. Crash the worker goroutine (defer Done() never runs)
//  2. Leak the WaitGroup counter
//  3. Cause Shutdown to block forever (deadlock)
func TestWorkerPool_PanicDoesNotLeak(t *testing.T) {
	before := runtime.NumGoroutine()

	pool := NewWorkerPool(4)

	// Submit tasks that panic.
	for i := 0; i < 10; i++ {
		pool.Submit(func() {
			panic("test panic")
		})
	}

	// Give workers time to process (and recover from) the panics.
	time.Sleep(200 * time.Millisecond)

	// Shutdown should complete without deadlocking.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		pool.Shutdown(ctx)
		close(done)
	}()

	select {
	case <-done:
		// good — Shutdown completed
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown deadlocked — panic leaked the WaitGroup counter")
	}

	time.Sleep(200 * time.Millisecond)

	after := runtime.NumGoroutine()
	leaked := after - before
	if leaked > 1 {
		t.Errorf("goroutine leak after panics: before=%d, after=%d, leaked=%d", before, after, leaked)
	}
}

// TestWorkerPool_SubmitAfterShutdown verifies that Submit after Shutdown
// does not panic (it should be a silent no-op).
func TestWorkerPool_SubmitAfterShutdown(t *testing.T) {
	pool := NewWorkerPool(2)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	pool.Shutdown(ctx)

	// This should NOT panic.
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("Submit after Shutdown panicked: %v", r)
		}
	}()

	pool.Submit(func() {
		t.Error("task executed after Shutdown — should be dropped")
	})
}

// TestWorkerPool_SubmitDoesNotBlock verifies that Submit never blocks
// indefinitely — it either queues the task or spawns a goroutine.
func TestWorkerPool_SubmitDoesNotBlock(t *testing.T) {
	pool := NewWorkerPool(2) // small pool, small buffer

	// Fill the queue with slow tasks.
	block := make(chan struct{})
	for i := 0; i < 50; i++ {
		pool.Submit(func() {
			<-block // block all workers
		})
	}

	// Now Submit should not block — it should fall back to spawning a goroutine.
	done := make(chan struct{})
	go func() {
		pool.Submit(func() {})
		close(done)
	}()

	select {
	case <-done:
		// good — Submit returned
	case <-time.After(time.Second):
		t.Fatal("Submit blocked for >1s — should fall back to goroutine")
	}

	close(block) // unblock workers
}

// TestContextNextNoDoubleExecution verifies that Next() cannot execute
// the same middleware twice.
func TestContextNextNoDoubleExecution(t *testing.T) {
	execCount := 0
	mw1 := func(ctx *Context) error { execCount++; return ctx.Next() }
	mw2 := func(ctx *Context) error { execCount++; return ctx.Next() }
	handler := func(ctx *Context) error {
		execCount++ /* do not call Next */
		return nil
	}

	ctx := &Context{
		middlewares: []HandlerFunc{mw1, mw2, handler},
		index:       -1,
	}
	ctx.Next()

	if execCount != 3 {
		t.Errorf("execCount = %d, want 3 (each middleware executed exactly once)", execCount)
	}
}

// TestContextNextDoesNotLoop verifies that calling Next() multiple times
// after the chain is exhausted is safe (no panic, no re-execution).
func TestContextNextDoesNotLoop(t *testing.T) {
	execCount := 0
	handler := func(ctx *Context) error {
		execCount++
		return nil
	}

	ctx := &Context{
		middlewares: []HandlerFunc{handler},
		index:       -1,
	}
	ctx.Next()
	ctx.Next() // should be a no-op
	ctx.Next() // should be a no-op

	if execCount != 1 {
		t.Errorf("execCount = %d, want 1 (handler should only run once)", execCount)
	}
}

// TestContextStoreNotRetainedAcrossRequests verifies that the store map
// is not shared between requests (would be a memory leak + data race).
func TestContextStoreNotRetainedAcrossRequests(t *testing.T) {
	ctx1 := &Context{}
	ctx1.Set("user", "alice")

	// Simulate a second request getting a "fresh" context.
	ctx2 := &Context{}
	if v, ok := ctx2.Get("user"); ok {
		t.Errorf("ctx2 leaked data from ctx1: got %v", v)
	}
}

// TestContextNewContext verifies the test helper creates a valid context.
func TestContextNewContext(t *testing.T) {
	ctx := NewContext(GET, "/test")
	if ctx.Req.Method != GET {
		t.Errorf("Method = %q, want GET", ctx.Req.Method)
	}
	if ctx.Req.Path != "/test" {
		t.Errorf("Path = %q, want /test", ctx.Req.Path)
	}
	if ctx.Req.Header == nil {
		t.Error("Header map is nil")
	}
}

// TestContextSetMiddlewareChain verifies the chain is set correctly.
func TestContextSetMiddlewareChain(t *testing.T) {
	execCount := 0
	mw := func(ctx *Context) error { execCount++; return ctx.Next() }
	handler := func(ctx *Context) error {
		execCount++
		return nil
	}

	ctx := NewContext(GET, "/test")
	ctx.SetMiddlewareChain([]HandlerFunc{mw}, handler)
	ctx.Next()

	if execCount != 2 {
		t.Errorf("execCount = %d, want 2 (mw + handler)", execCount)
	}
}
