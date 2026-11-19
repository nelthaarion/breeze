package events

import (
	"context"
	"errors"
	"runtime/debug"
	"sync"
	"time"
	"unsafe"
)

// EmitBus dispatches event synchronously on the given bus and returns
// once every eligible listener has run.
//
// Listeners execute in phase order (before, normal, after), then by
// descending priority, then by registration order. The order is computed
// at registration time; a dispatch only walks the pre-sorted snapshot.
//
// The error contract:
//
//   - nil — every listener ran, or one of them stopped propagation.
//   - the listener's error — a listener failed and
//     [Config.ContinueOnError] is false (the default).
//   - a [*MultiError] — one or more listeners failed and
//     ContinueOnError is true.
//   - a [*PanicError] — a listener panicked under
//     [PanicRecoverAndFail].
//   - [ErrBusClosed] — the bus is closed.
//
// [Stop] is never returned: stopping is normal control flow, not failure.
//
// Emitting an event with no listeners is close to free — one map lookup
// and an atomic load, with no Context allocated.
func EmitBus[T any](b *Bus, event T) error {
	return emitCtx(b, context.Background(), event)
}

// EmitCtxBus is [EmitBus] with a caller-supplied context.Context, made
// available to listeners as [Context.Ctx]. The dispatcher does not itself
// abort on ctx expiry — a listener that cares should check ctx.Err() —
// because aborting midway would leave the event half-handled.
func EmitCtxBus[T any](b *Bus, ctx context.Context, event T) error {
	return emitCtx(b, ctx, event)
}

// emitCtx is the synchronous dispatch implementation.
func emitCtx[T any](b *Bus, stdctx context.Context, event T) error {
	if b.closed.Load() {
		return ErrBusClosed
	}

	mw := b.middlewares()

	// One atomic load per dispatch. When nothing is attached this is the
	// only cost the observability layer imposes, and obs is threaded
	// through the rest of the dispatch so no further loads are needed.
	obs := b.observer()

	e := b.lookup(typeKey[T]())
	if e == nil {
		// Nothing to do and nobody watching: this is the common case for
		// an event nobody listens to, and it costs one map lookup.
		if len(mw) == 0 && obs == nil {
			return nil
		}
		// Middleware still runs for an event with no listeners, because a
		// tracer legitimately wants to see that it was emitted. That
		// needs an entry to hold the name and metrics, so create one.
		e = entryFor[T](b)
	}
	snap := e.ch.(*channel[T]).load()
	if len(snap) == 0 && len(mw) == 0 && obs == nil {
		return nil
	}

	ctx := newContext(b, e.displayName(), stdctx)
	defer ctx.release()

	start := ctx.Time
	var ran int
	var stopped bool

	if obs != nil {
		b.notifyStart(obs, DispatchInfo{
			EventID:       ctx.EventID,
			EventName:     ctx.EventName,
			Time:          start,
			CorrelationID: ctx.CorrelationID,
			RequestID:     ctx.RequestID,
			ListenerCount: len(snap),
			PayloadSize:   int(unsafe.Sizeof(event)),
		})
	}

	// Split rather than always going through runChain. The middleware form
	// needs a closure over ran/stopped, and a closure that captures their
	// addresses forces both onto the heap for every dispatch — escape analysis
	// is per-function, so merely *having* the closure in this function costs
	// two allocations even on the path that never builds it. Keeping it in
	// runMiddlewareChain leaves these two as ordinary stack locals here.
	var err error
	if len(mw) == 0 {
		ran, stopped, err = runListeners(b, e, ctx, snap, event, obs)
	} else {
		ran, stopped, err = runMiddlewareChain(mw, b, e, ctx, snap, event, obs)
	}

	elapsed := time.Since(start)
	// The payload is boxed into an interface only when something will read it.
	// Passing `event` unconditionally allocated on every dispatch to hand a
	// value to two branches that were almost always disabled.
	b.finish(e, ctx, elapsed, start, ran, stopped, err, payloadFor(b, event), false, obs)
	return err
}

// payloadFor boxes event into an `any` only if some consumer will read it.
//
// Boxing a generic T into an interface allocates whenever T does not fit in a
// pointer word, and the recorder and observer are the only readers. Both are
// off by default and both are gated by an atomic flag, so the check is two
// atomic loads against an allocation the dispatch would otherwise pay on every
// emit and immediately discard.
//
// It is a free function rather than a method because Go does not permit type
// parameters on methods.
func payloadFor[T any](b *Bus, event T) any {
	if b.recorder.isEnabled() && b.recorder.wantsPayload() {
		return event
	}
	if b.obsPayload.Load() && b.obs.Load() != nil {
		return event
	}
	return nil
}

// runMiddlewareChain runs the listeners wrapped in the middleware chain.
//
// It exists to own the closure. See the comment at its only call site: the
// closure's captures escape, and isolating it here keeps that cost on the
// dispatches that actually have middleware registered.
func runMiddlewareChain[T any](mw []Middleware, b *Bus, e *entry, ctx *Context, snap []*listener[T], event T, obs Observer) (ran int, stopped bool, err error) {
	// A middleware is plugin code like a listener, and PanicError is
	// documented as covering both. Recovery is scoped to this function, so
	// the no-middleware path pays nothing for it. A panic before next() means
	// the listeners never ran, so the caller gets the *PanicError instead of
	// a silent nil.
	//
	// PanicPropagate is a pass-through and must stay one: recovering here
	// would also catch a listener panic that invokeListener has already
	// reported and is re-panicking, and report it a second time. So in that
	// mode the deferred function does not call recover at all.
	defer func() {
		if b.cfg.PanicMode == PanicPropagate {
			return
		}
		r := recover()
		if r == nil {
			return
		}
		err = b.reportPanic(e, ctx.EventName, "", r)
	}()
	err = runChain(mw, ctx, func() error {
		var derr error
		ran, stopped, derr = runListeners(b, e, ctx, snap, event, obs)
		return derr
	})
	return ran, stopped, err
}

// runListeners walks the snapshot in order.
//
// It returns the number of listeners that actually ran, whether
// propagation was stopped, and the resulting error.
func runListeners[T any](b *Bus, e *entry, ctx *Context, snap []*listener[T], event T, obs Observer) (ran int, stopped bool, err error) {
	// errs stays nil unless ContinueOnError collects more than one
	// failure, so the common paths allocate nothing.
	var errs []error

	// spent collects the once-listeners this walk ran. They are removed once
	// the walk is over: "run at most once, then remove automatically" is the
	// documented contract, and a spent listener left in the snapshot is
	// skipped by every later dispatch for ever. Only listeners this dispatch
	// claimed are collected; see channel.removeIDs. It stays nil, and the
	// walk allocates nothing, unless a once-listener runs.
	var spent []uint64

	for i, l := range snap {
		// Checked before each listener rather than once up front: any
		// listener may cancel, and the ones after it must not run.
		if ctx.Cancelled() {
			stopped = true
			break
		}
		ctx.index = i

		lerr, skipped := invokeListener(b, e, ctx, l, event, obs, i)
		if skipped {
			continue
		}
		if l.once && l.fired.Load() {
			spent = append(spent, l.id)
		}
		ran++

		if lerr == nil {
			continue
		}

		if errors.Is(lerr, Stop) {
			ctx.Cancel()
			stopped = true
			break
		}

		if b.cfg.Metrics {
			e.stats.failures.Add(1)
		}
		if b.cfg.OnError != nil {
			b.cfg.OnError(ctx, l.name, lerr)
		}

		if !b.cfg.ContinueOnError {
			err, stopped = lerr, true
			break
		}
		errs = append(errs, &ListenerError{Listener: l.name, Err: lerr})
	}

	if len(spent) > 0 {
		e.erased.removeIDs(spent)
	}
	if err == nil {
		err = asError(errs)
	}
	return ran, stopped, err
}

// invoke runs one listener with panic recovery.
//
// Recovery lives in its own function so the deferred recover is scoped to
// a single listener: a panic unwinds only that call, and the loop in
// runListeners survives to run the rest.
func invokeListener[T any](b *Bus, e *entry, ctx *Context, l *listener[T], event T, obs Observer, idx int) (err error, skipped bool) {
	// panicked is set by the recovery defer below and read by the
	// observer defer. Both are declared here so the observer sees the
	// outcome after recovery has settled it.
	var panicked bool

	// Registered before the recovery defer so that it runs *after* it:
	// deferred calls unwind last-in-first-out, and the observer must
	// observe the final err/skipped values, not the mid-panic ones.
	if obs != nil {
		lstart := time.Now()
		b.notifyListenerStart(obs, ListenerCall{
			EventID:      ctx.EventID,
			EventName:    ctx.EventName,
			ListenerName: l.name,
			ListenerID:   l.id,
			Priority:     l.priority,
			Phase:        l.phase.String(),
			Index:        idx,
			StartTime:    lstart,
		})
		defer func() {
			b.notifyListenerEnd(obs, ListenerOutcome{
				EventID:      ctx.EventID,
				EventName:    ctx.EventName,
				ListenerName: l.name,
				ListenerID:   l.id,
				Priority:     l.priority,
				Phase:        l.phase.String(),
				Index:        idx,
				Duration:     time.Since(lstart),
				Err:          err,
				Panicked:     panicked,
				Skipped:      skipped,
			})
		}()
	}

	defer func() {
		r := recover()
		if r == nil {
			return
		}

		panicked = true
		pe := b.reportPanic(e, ctx.EventName, l.name, r)

		switch b.cfg.PanicMode {
		case PanicPropagate:
			panic(r)
		case PanicRecoverAndFail:
			err = pe
		default: // PanicRecoverAndContinue
			err = nil
		}
		skipped = false
	}()

	err, skipped = l.invoke(ctx, event)
	if skipped && b.cfg.Metrics && l.filter != nil {
		e.stats.filtered.Add(1)
	}
	return err, skipped
}

// finish records metrics and history for a completed dispatch, and hands
// the result to the observer if one is attached.
//
// obs is the observer the dispatch announced itself to, not whatever is attached
// now: an observer swapped or detached mid-dispatch must still receive the end
// of the dispatch it saw begin, and one attached mid-dispatch must not receive
// the end of a dispatch it never saw start.
//
// For an async dispatch only the scheduling is finished here; its listeners
// count themselves as they run (see runAsyncListener), so adding ran and
// stopped here as well would count every invocation twice.
func (b *Bus) finish(e *entry, ctx *Context, d time.Duration, start time.Time, ran int, stopped bool, err error, payload any, async bool, obs Observer) {
	if b.cfg.Metrics {
		e.stats.observe(d, start)
		if !async {
			e.stats.listeners.Add(uint64(ran))
			if stopped {
				e.stats.stopped.Add(1)
			}
		}
	}

	if b.recorder.isEnabled() {
		rec := Record{
			EventID:       ctx.EventID,
			Name:          ctx.EventName,
			Time:          start,
			Duration:      d,
			Listeners:     ran,
			Async:         async,
			Stopped:       stopped,
			Err:           err,
			CorrelationID: ctx.CorrelationID,
			RequestID:     ctx.RequestID,
		}
		if b.recorder.wantsPayload() {
			rec.Payload = payload
		}
		b.recorder.push(rec)
	}

	if obs != nil {
		res := DispatchResult{
			EventID:           ctx.EventID,
			EventName:         ctx.EventName,
			Time:              start,
			Duration:          d,
			ListenersExecuted: ran,
			Cancelled:         stopped,
			Err:               err,
			CorrelationID:     ctx.CorrelationID,
			RequestID:         ctx.RequestID,
			Async:             async,
		}
		if b.obsPayload.Load() {
			res.Payload = payload
		}
		b.notifyEnd(obs, res)
	}
}

// EmitAsyncBus dispatches event without waiting for its listeners.
//
// Each listener runs independently, so one that fails or panics does not
// affect the others. Because listeners may run in parallel, priority
// determines the order in which they are *scheduled*, not the order in
// which they complete — use [EmitBus] when ordering matters.
//
// Errors are reported through [Config.OnError] rather than returned:
// there is no caller left to return them to. The returned error covers
// only the emit itself, and is non-nil only for a closed bus.
//
// Scheduling follows [Config.Async]: one goroutine per listener, or a
// bounded worker pool.
func EmitAsyncBus[T any](b *Bus, event T) error {
	return emitAsync(b, context.Background(), event, nil)
}

// EmitAsyncCtxBus is [EmitAsyncBus] with a caller-supplied context.
func EmitAsyncCtxBus[T any](b *Bus, ctx context.Context, event T) error {
	return emitAsync(b, ctx, event, nil)
}

// EmitAsyncWaitBus dispatches asynchronously and waits for every listener
// to finish. It combines async execution — listeners run in parallel and
// are isolated from each other — with a synchronous completion point, for
// callers that need the work done but not serialised.
//
// Listener errors are reported through [Config.OnError]; the returned
// error covers only the emit.
func EmitAsyncWaitBus[T any](b *Bus, event T) error {
	var wg sync.WaitGroup
	err := emitAsync(b, context.Background(), event, &wg)
	wg.Wait()
	return err
}

// emitAsync is the asynchronous dispatch implementation.
//
// The Context is marked non-poolable: listeners outlive this function, so
// there is no point at which it could be safely recycled.
func emitAsync[T any](b *Bus, stdctx context.Context, event T, wg *sync.WaitGroup) error {
	if b.closed.Load() {
		return ErrBusClosed
	}

	e := b.lookup(typeKey[T]())
	if e == nil {
		return nil
	}
	ch := e.ch.(*channel[T])
	snap := ch.load()
	if len(snap) == 0 {
		return nil
	}

	ctx := newContext(b, e.displayName(), stdctx)
	ctx.pooled = false
	start := ctx.Time

	obs := b.observer()
	if obs != nil {
		b.notifyStart(obs, DispatchInfo{
			EventID:       ctx.EventID,
			EventName:     ctx.EventName,
			Time:          start,
			CorrelationID: ctx.CorrelationID,
			RequestID:     ctx.RequestID,
			ListenerCount: len(snap),
			PayloadSize:   int(unsafe.Sizeof(event)),
			Async:         true,
		})
	}

	scheduled := 0
	var spent []uint64 // once-listeners this emit claimed and scheduled: removed below
	for _, l := range snap {
		if ctx.Cancelled() {
			break
		}
		// Filters are evaluated on the emitting goroutine so that a
		// rejected event never costs a goroutine or a queue slot.
		if !asyncFilterAccepts(b, e, ctx, l, event) {
			if b.cfg.Metrics {
				e.stats.filtered.Add(1)
			}
			continue
		}
		// Claim the once-listener here, before scheduling, so two
		// concurrent emits cannot both queue the same listener.
		if l.once && !l.fired.CompareAndSwap(false, true) {
			continue
		}

		lst := l
		task := func() {
			if wg != nil {
				defer wg.Done()
			}
			runAsyncListener(b, e, ctx, lst, event, obs)
		}
		if wg != nil {
			wg.Add(1)
		}

		if !b.schedule(task) {
			// Rejected by the pool: undo the bookkeeping so the
			// WaitGroup balances and a spent once-listener can fire
			// again on a later emit.
			if wg != nil {
				wg.Done()
			}
			if lst.once {
				lst.fired.Store(false)
			}
			continue
		}
		if lst.once {
			spent = append(spent, lst.id)
		}
		scheduled++
	}

	if len(spent) > 0 {
		ch.removeIDs(spent)
	}

	// Duration here measures scheduling, not execution; the Record's
	// Async flag tells consumers how to read it.
	b.finish(e, ctx, time.Since(start), start, scheduled, ctx.Cancelled(), nil, payloadFor(b, event), true, obs)
	return nil
}

// asyncFilterAccepts evaluates l's filter on the emitting goroutine.
//
// Under sync dispatch a panicking filter is recovered inside invokeListener.
// Here there is no such frame, so without this a filter bug would unwind
// straight through EmitAsync into the caller — the one place the package
// promises a broken plugin cannot reach. A panicking filter is reported like
// any recovered panic and its listener is skipped.
func asyncFilterAccepts[T any](b *Bus, e *entry, ctx *Context, l *listener[T], event T) (accept bool) {
	if l.filter == nil {
		return true
	}
	defer func() {
		if r := recover(); r != nil {
			accept = false
			b.reportPanic(e, ctx.EventName, l.name, r)
		}
	}()
	return l.filter(event)
}

// reportPanic counts a recovered panic and hands it to the panic handler. It
// returns the *PanicError so a caller can also surface it as an error.
func (b *Bus) reportPanic(e *entry, event, listener string, r any) *PanicError {
	if b.cfg.Metrics {
		e.stats.panics.Add(1)
	}
	pe := &PanicError{
		Event:    event,
		Listener: listener,
		Value:    r,
		Stack:    debug.Stack(),
	}
	if b.cfg.OnPanic != nil {
		b.cfg.OnPanic(pe)
	}
	return pe
}

// schedule runs task according to the configured async mode. It reports
// whether the task was accepted.
func (b *Bus) schedule(task func()) bool {
	if b.cfg.Async == AsyncWorkerPool {
		p := b.workers()
		if p == nil { // bus closed: refuse rather than start a pool nobody will stop
			return false
		}
		return p.submit(task)
	}
	go task()
	return true
}

// runAsyncListener invokes one listener on its own goroutine, with panic
// recovery and metrics. The filter and once-claim have already been
// resolved by the caller.
//
// It is a free function rather than a method because Go does not permit
// type parameters on methods.
func runAsyncListener[T any](b *Bus, e *entry, ctx *Context, l *listener[T], event T, obs Observer) {
	var panicked bool
	var lstart time.Time

	// Registered first so it unwinds last and sees the settled outcome.
	if obs != nil {
		lstart = time.Now()
		b.notifyListenerStart(obs, ListenerCall{
			EventID:      ctx.EventID,
			EventName:    ctx.EventName,
			ListenerName: l.name,
			ListenerID:   l.id,
			Priority:     l.priority,
			Phase:        l.phase.String(),
			Index:        -1, // async listeners have no ordered position
			StartTime:    lstart,
		})
	}

	var lerr error
	if obs != nil {
		defer func() {
			b.notifyListenerEnd(obs, ListenerOutcome{
				EventID:      ctx.EventID,
				EventName:    ctx.EventName,
				ListenerName: l.name,
				ListenerID:   l.id,
				Priority:     l.priority,
				Phase:        l.phase.String(),
				Index:        -1, // async: no deterministic position
				Duration:     time.Since(lstart),
				Err:          lerr,
				Panicked:     panicked,
			})
		}()
	}

	defer func() {
		r := recover()
		if r == nil {
			return
		}
		panicked = true
		b.reportPanic(e, ctx.EventName, l.name, r)
		// PanicPropagate is deliberately not honoured here. Re-panicking
		// on a listener goroutine would crash the process from a stack
		// that has no relation to the emitter, so async always recovers.
	}()

	if b.cfg.Metrics {
		e.stats.listeners.Add(1)
	}
	l.calls.Add(1)

	// ctx.index is not set: goroutines share the Context and would race
	// on it. Async listeners have no meaningful position anyway.
	err := l.fn(ctx, event)
	lerr = err
	if err == nil {
		return
	}

	if errors.Is(err, Stop) {
		// Cancelling here suppresses listeners not yet scheduled. Those
		// already running are unaffected. Metrics.Stopped counts dispatches,
		// and several listeners may return Stop for the same one, so only the
		// call that actually flips the flag is counted.
		if ctx.cancelOnce() && b.cfg.Metrics {
			e.stats.stopped.Add(1)
		}
		return
	}

	if b.cfg.Metrics {
		e.stats.failures.Add(1)
	}
	if b.cfg.OnError != nil {
		b.cfg.OnError(ctx, l.name, err)
	}
}
