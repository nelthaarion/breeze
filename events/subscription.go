package events

import "sync/atomic"

// Subscription is the handle returned by every registration function. It
// identifies one listener and allows it to be configured or removed.
//
// The configuration methods return the Subscription so they can be
// chained:
//
//	events.On(UserCreated{}, save).
//		Priority(events.PriorityLow).
//		Where(func(e UserCreated) bool { return e.UserID > 100 })
//
// Each configuration call republishes the event's listener snapshot, which
// is why they belong at registration time rather than inside a handler.
// The listener is live from the moment [On] returns, so a chained
// Priority may land after a concurrent dispatch has already read the
// order. Register from init or startup code, where nothing is emitting
// yet, and this is a non-issue.
//
// A Subscription is safe for concurrent use. It is generic over the event
// type so that [Subscription.Where] can accept a strongly typed filter.
type Subscription[T any] struct {
	bus *Bus
	ch  *channel[T]
	id  uint64

	// active guards Unsubscribe so repeated calls are harmless and so a
	// listener can safely unsubscribe itself from inside its handler.
	active atomic.Bool
}

// ID returns the listener's unique identifier. Identifiers are unique per
// bus and are never reused.
func (s *Subscription[T]) ID() uint64 { return s.id }

// Priority sets the listener's execution priority. Higher priorities run
// first; listeners with equal priority run in registration order. The
// default is [PriorityNormal].
//
// Priority orders listeners within their lifecycle phase — a before-hook
// always precedes a normal listener regardless of the numbers involved.
func (s *Subscription[T]) Priority(p int) *Subscription[T] {
	s.ch.update(s.id, func(l *listener[T]) { l.priority = p })
	return s
}

// Where installs a filter. The listener runs only for events the filter
// accepts, and a rejected event never enters the handler.
//
// The filter receives the event but not the [Context], because it must be
// a pure predicate over the payload: it is evaluated before the listener
// is counted as invoked, and giving it the shared Context would invite
// side effects at a point in the dispatch where they are hard to reason
// about.
//
// Calling Where twice replaces the previous filter. Compose predicates in
// one function if you need several.
func (s *Subscription[T]) Where(filter Filter[T]) *Subscription[T] {
	s.ch.update(s.id, func(l *listener[T]) { l.filter = filter })
	return s
}

// Named overrides the listener's display name in the inspector. By default
// the name is derived from the handler function's symbol, which for a
// closure is rarely informative.
func (s *Subscription[T]) Named(name string) *Subscription[T] {
	s.ch.update(s.id, func(l *listener[T]) { l.name = name })
	return s
}

// Unsubscribe removes the listener. It is idempotent, and safe to call
// from inside the listener's own handler: the running dispatch continues
// against the snapshot it already loaded, and the listener is gone from
// the next one.
func (s *Subscription[T]) Unsubscribe() {
	if !s.active.CompareAndSwap(true, false) {
		return
	}
	s.ch.remove(s.id)
}

// Active reports whether the listener is still registered.
//
// It is false after Unsubscribe, but also after the listener was removed some
// other way: [Clear], [Bus.Reset], [OffBus], or — for a once-listener — having
// fired.
func (s *Subscription[T]) Active() bool {
	return s.active.Load() && s.ch.has(s.id)
}

// register is the single implementation behind On, Once, Before and After.
func register[T any](b *Bus, ph phase, once bool, fn Handler[T]) *Subscription[T] {
	return registerWith(b, ph, once, fn, nil, "")
}

// registerWith is register with a filter and a display name installed
// *before* the listener is published.
//
// Chaining Where or Named onto a finished registration leaves a window in
// which the listener is live without them. For a type-wide listener that is a
// documented, startup-only hazard; for a topic subscription it meant receiving
// messages for every topic. Callers that need the listener to be correct from
// the first dispatch use this. An empty name keeps the name derived from fn.
func registerWith[T any](b *Bus, ph phase, once bool, fn Handler[T], filter Filter[T], name string) *Subscription[T] {
	ch := channelFor[T](b)
	sub := &Subscription[T]{bus: b, ch: ch, id: b.nextListenerID()}
	sub.active.Store(true)

	// The name comes from the handler as given: a nil handler is named
	// "<nil>" in the inspector, which is what the comment below promises.
	if name == "" {
		name = handlerName(fn)
	}

	// A nil handler is accepted and registered as a no-op rather than
	// panicking: a plugin passing nil by mistake should not take the
	// process down at startup, and the inspector will show the listener
	// as "<nil>".
	if fn == nil {
		fn = func(*Context, T) error { return nil }
	}

	l := &listener[T]{
		fn:       fn,
		filter:   filter,
		name:     name,
		id:       sub.id,
		priority: PriorityNormal,
		phase:    ph,
		once:     once,
		calls:    new(atomic.Uint64),
	}
	if once {
		l.fired = new(atomic.Bool)
	}

	// A closed bus accepts the Subscription but never registers the
	// listener, so callers can shut down without racing their own
	// registration goroutines.
	if b.closed.Load() {
		sub.active.Store(false)
		return sub
	}

	ch.add(l)
	return sub
}

// OnBus registers a listener for T on the given bus.
//
// The sample argument is used only for type inference and is never read;
// pass a zero value. Use [OnTypeBus] to specify T explicitly.
func OnBus[T any](b *Bus, _ T, fn Handler[T]) *Subscription[T] {
	return register(b, phaseNormal, false, fn)
}

// OnTypeBus registers a listener for T on the given bus, with T given
// explicitly.
func OnTypeBus[T any](b *Bus, fn Handler[T]) *Subscription[T] {
	return register(b, phaseNormal, false, fn)
}

// OnceBus registers a listener that runs at most once and is then
// removed.
//
// Exactly one invocation happens even if several goroutines emit
// concurrently; the losing dispatches skip the listener.
func OnceBus[T any](b *Bus, _ T, fn Handler[T]) *Subscription[T] {
	return register(b, phaseNormal, true, fn)
}

// OnceTypeBus registers a once-listener with T given explicitly.
func OnceTypeBus[T any](b *Bus, fn Handler[T]) *Subscription[T] {
	return register(b, phaseNormal, true, fn)
}

// BeforeBus registers a listener in the before phase. Before-hooks run
// ahead of every normal listener for the event, whatever their
// priorities, which lets a subsystem reliably prepare state that ordinary
// listeners depend on.
func BeforeBus[T any](b *Bus, _ T, fn Handler[T]) *Subscription[T] {
	return register(b, phaseBefore, false, fn)
}

// BeforeTypeBus registers a before-hook with T given explicitly.
func BeforeTypeBus[T any](b *Bus, fn Handler[T]) *Subscription[T] {
	return register(b, phaseBefore, false, fn)
}

// AfterBus registers a listener in the after phase. After-hooks run once
// every normal listener has finished, which suits metrics and auditing.
//
// An after-hook is still part of the dispatch: it is skipped if an earlier
// listener stopped propagation or cancelled the context. Use middleware
// when you need work that runs unconditionally.
func AfterBus[T any](b *Bus, _ T, fn Handler[T]) *Subscription[T] {
	return register(b, phaseAfter, false, fn)
}

// AfterTypeBus registers an after-hook with T given explicitly.
func AfterTypeBus[T any](b *Bus, fn Handler[T]) *Subscription[T] {
	return register(b, phaseAfter, false, fn)
}

// OffBus removes the listener with the given id from T's registrations.
// It reports whether a listener was removed.
//
// Prefer [Subscription.Unsubscribe]; OffBus exists for callers that
// persist a bare id rather than the handle.
func OffBus[T any](b *Bus, id uint64) bool {
	if e := b.lookup(typeKey[T]()); e != nil {
		return e.erased.remove(id)
	}
	return false
}
