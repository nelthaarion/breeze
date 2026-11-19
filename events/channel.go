package events

import (
	"slices"
	"sync"
	"sync/atomic"
)

// channel holds every listener registered for one event type.
//
// # Copy-on-write
//
// Registrations are rare; dispatches are not. The channel therefore keeps
// a mutex-guarded master slice for writers and publishes an immutable,
// pre-sorted copy through an atomic pointer for readers. A dispatch does
// one atomic load and then walks a plain slice — no locks, no sorting, no
// bounds on how many dispatches run in parallel.
//
// A dispatch that is already running keeps using the snapshot it loaded,
// so listeners added or removed mid-dispatch take effect from the next
// dispatch onwards. That is the property that makes it safe for a
// listener to unsubscribe itself, or to register another listener, from
// inside a handler.
type channel[T any] struct {
	mu sync.Mutex

	// master is the writer-side source of truth, guarded by mu.
	master []*listener[T]

	// snapshot points at an immutable sorted copy of master. It is never
	// nil after construction.
	snapshot atomic.Pointer[[]*listener[T]]

	// size mirrors len(master) so readers can report the listener count
	// without taking the lock or dereferencing the snapshot.
	size atomic.Int64
}

// newChannel returns an empty channel with a published empty snapshot,
// so the dispatch path never has to nil-check the pointer.
func newChannel[T any]() *channel[T] {
	c := &channel[T]{}
	empty := make([]*listener[T], 0)
	c.snapshot.Store(&empty)
	return c
}

// load returns the current snapshot. Callers must treat it as read-only.
func (c *channel[T]) load() []*listener[T] {
	return *c.snapshot.Load()
}

// len returns the number of registered listeners.
func (c *channel[T]) len() int { return int(c.size.Load()) }

// publishLocked rebuilds and publishes the snapshot. The caller must hold
// c.mu.
//
// The copy is deliberate: publishing `master` directly would let a later
// append with spare capacity mutate a slice that running dispatches are
// still reading.
func (c *channel[T]) publishLocked() {
	next := make([]*listener[T], len(c.master))
	copy(next, c.master)
	sortListeners(next)
	c.snapshot.Store(&next)
	c.size.Store(int64(len(next)))
}

// add registers l and republishes the snapshot.
func (c *channel[T]) add(l *listener[T]) {
	c.mu.Lock()
	c.master = append(c.master, l)
	c.publishLocked()
	c.mu.Unlock()
}

// remove deletes the listener with the given id.
// It reports whether a listener was found.
func (c *channel[T]) remove(id uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, l := range c.master {
		if l.id != id {
			continue
		}
		// Order within master does not matter — publishLocked sorts the
		// snapshot — so delete by shifting the tail down. The slice is
		// small and this keeps the master free of nil holes.
		c.master = append(c.master[:i], c.master[i+1:]...)
		c.publishLocked()
		return true
	}
	return false
}

// update applies mutate to a clone of the listener with the given id and
// republishes. Cloning keeps the listener that running dispatches hold
// untouched; they finish against the values they started with.
func (c *channel[T]) update(id uint64, mutate func(*listener[T])) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, l := range c.master {
		if l.id != id {
			continue
		}
		cl := l.clone()
		mutate(cl)
		c.master[i] = cl
		c.publishLocked()
		return true
	}
	return false
}

// has reports whether a listener with the given id is registered.
func (c *channel[T]) has(id uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, l := range c.master {
		if l.id == id {
			return true
		}
	}
	return false
}

// clearAll removes every listener.
func (c *channel[T]) clearAll() {
	c.mu.Lock()
	c.master = nil
	c.publishLocked()
	c.mu.Unlock()
}

// removeIDs removes every listener whose id is in ids, with a single republish.
//
// This is how a dispatch retires the once-listeners it ran: each dispatch
// removes exactly the ones it claimed. Sweeping everything that merely looks
// fired is not safe, because an async emit that claimed a once-listener and
// then failed to schedule it un-fires it again; a sweep in that window would
// remove a listener that never ran.
func (c *channel[T]) removeIDs(ids []uint64) {
	if len(ids) == 0 {
		return
	}
	// A linear scan of ids per listener is right for the usual one or two;
	// a dispatch that retires many at once would make it quadratic.
	var set map[uint64]struct{}
	if len(ids) > 8 {
		set = make(map[uint64]struct{}, len(ids))
		for _, id := range ids {
			set[id] = struct{}{}
		}
	}
	drop := func(id uint64) bool {
		if set != nil {
			_, ok := set[id]
			return ok
		}
		return slices.Contains(ids, id)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	kept := c.master[:0]
	changed := false
	for _, l := range c.master {
		if drop(l.id) {
			changed = true
			continue
		}
		kept = append(kept, l)
	}
	if !changed {
		return
	}
	// Zero the vacated tail so the removed listeners (and anything their
	// closures captured) become collectable.
	for i := len(kept); i < len(c.master); i++ {
		c.master[i] = nil
	}
	c.master = kept
	c.publishLocked()
}

// pruneFired drops every once-listener that has already fired.
//
// Dispatch does not use it (see removeIDs): by the time a dispatch sees
// fired=true it cannot tell a listener that ran from one that another dispatch
// has claimed and may yet hand back. It is a sweep for a quiescent bus, and a
// cleanup for the rare leftover — a dispatch unwound by PanicPropagate before
// it could retire what it ran.
func (c *channel[T]) pruneFired() {
	c.mu.Lock()
	defer c.mu.Unlock()
	kept := c.master[:0]
	changed := false
	for _, l := range c.master {
		if l.once && l.fired.Load() {
			changed = true
			continue
		}
		kept = append(kept, l)
	}
	if !changed {
		return
	}
	// Zero the vacated tail so the removed listeners (and anything their
	// closures captured) become collectable.
	for i := len(kept); i < len(c.master); i++ {
		c.master[i] = nil
	}
	c.master = kept
	c.publishLocked()
}

// describe returns an inspector view of every listener in execution
// order. It reads the snapshot, so it never blocks a dispatch.
func (c *channel[T]) describe() []ListenerInfo {
	snap := c.load()
	out := make([]ListenerInfo, 0, len(snap))
	for i, l := range snap {
		info := ListenerInfo{
			ID:       l.id,
			Name:     l.name,
			Priority: l.priority,
			Phase:    l.phase.String(),
			Order:    i,
			Once:     l.once,
			Filtered: l.filter != nil,
			Calls:    l.calls.Load(),
		}
		if l.once {
			info.Fired = l.fired.Load()
		}
		out = append(out, info)
	}
	return out
}
