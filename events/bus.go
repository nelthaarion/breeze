package events

import (
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
)

// channelHandle is the type-erased view of a [channel].
//
// The bus stores channels for many different event types in one map, so
// it cannot name them generically. Every operation that does not need the
// event type — counting, describing, clearing — goes through this
// interface; only dispatch and registration recover the concrete
// *channel[T].
type channelHandle interface {
	len() int
	describe() []ListenerInfo
	clearAll()
	pruneFired()
	remove(id uint64) bool
	has(id uint64) bool
}

// entry is everything the bus tracks for one event type.
//
// Grouping the channel, its statistics and its name into a single value
// means a dispatch performs exactly one map lookup, rather than one per
// concern.
type entry struct {
	typ    reflect.Type
	ch     any // *channel[T]; asserted by the typed API
	erased channelHandle
	stats  eventStats

	// name is the registered display name. It is an atomic pointer
	// because [Name] may be called while dispatches are running.
	name atomic.Pointer[string]
}

// displayName returns the registered name, falling back to the Go type.
func (e *entry) displayName() string {
	if p := e.name.Load(); p != nil {
		return *p
	}
	return e.typ.String()
}

// Bus is an isolated event bus.
//
// The zero value is not usable; construct one with [New]. Most
// applications use the package-level functions, which operate on
// [Default].
//
// All methods are safe for concurrent use.
type Bus struct {
	cfg Config

	// entries maps event type to its registry entry. sync.Map is the
	// right structure here because the access pattern is
	// write-once-read-many: an event type is registered a handful of
	// times at startup and then dispatched for the lifetime of the
	// process. Its read path avoids the lock contention an RWMutex would
	// suffer when many goroutines emit at once.
	entries sync.Map // reflect.Type -> *entry

	// mu guards byName and the middleware slice's writers.
	mu sync.Mutex

	// byName resolves a registered name back to its type, for the
	// dashboard and for name-based lookups.
	byName map[string]reflect.Type

	// middleware is published copy-on-write so dispatch reads it with a
	// single atomic load.
	middleware atomic.Pointer[[]Middleware]

	pool     *workerPool
	poolOnce sync.Once

	recorder *recorder

	// obs holds the optional observability hook. A nil pointer means no
	// observer is attached, which is the case the dispatch fast path is
	// tuned for: one atomic load and a nil check per dispatch.
	obs atomic.Pointer[Observer]

	// obsPayload records whether the observer asked to receive event
	// payloads, which costs one interface boxing per dispatch.
	obsPayload atomic.Bool

	eventSeq    atomic.Uint64
	listenerSeq atomic.Uint64

	closed atomic.Bool
}

// New creates a Bus with the given configuration. The zero Config is
// valid and selects the documented defaults.
func New(cfg ...Config) *Bus {
	var c Config
	if len(cfg) > 0 {
		c = cfg[0]
	}
	c = c.normalize()

	b := &Bus{
		cfg:      c,
		byName:   make(map[string]reflect.Type),
		recorder: newRecorder(c.RecorderSize),
	}
	empty := make([]Middleware, 0)
	b.middleware.Store(&empty)

	if c.Recorder {
		b.recorder.enable(false)
	}
	return b
}

// Default is the bus used by the package-level functions. Framework
// subsystems publish to it so that application code can subscribe without
// having to be handed a bus instance.
var Default = New()

// nextEventID returns a fresh dispatch identifier.
func (b *Bus) nextEventID() uint64 { return b.eventSeq.Add(1) }

// nextListenerID returns a fresh listener identifier.
func (b *Bus) nextListenerID() uint64 { return b.listenerSeq.Add(1) }

// Config returns the bus's effective configuration, with defaults
// resolved.
func (b *Bus) Config() Config { return b.cfg }

// typeKey returns the map key for T.
//
// Converting a nil *T rather than a zero T keeps this allocation-free:
// the argument to reflect.TypeOf is pointer-shaped, so it needs no boxing,
// whereas passing a large struct by value would heap-allocate on every
// call. Elem() then recovers T itself.
func typeKey[T any]() reflect.Type {
	return reflect.TypeOf((*T)(nil)).Elem()
}

// lookup returns the entry for key, or nil when the type has no
// registrations. Emit uses it to bail out before doing any work.
func (b *Bus) lookup(key reflect.Type) *entry {
	v, ok := b.entries.Load(key)
	if !ok {
		return nil
	}
	return v.(*entry)
}

// entryFor returns the entry for T, creating it if necessary.
func entryFor[T any](b *Bus) *entry {
	key := typeKey[T]()
	if v, ok := b.entries.Load(key); ok {
		return v.(*entry)
	}

	ch := newChannel[T]()
	e := &entry{typ: key, ch: ch, erased: ch}

	// LoadOrStore rather than Store: two goroutines registering the first
	// listener for the same type concurrently must agree on one entry, or
	// one of them would publish into a channel nobody dispatches from.
	actual, _ := b.entries.LoadOrStore(key, e)
	return actual.(*entry)
}

// channelFor returns the typed channel for T.
func channelFor[T any](b *Bus) *channel[T] {
	return entryFor[T](b).ch.(*channel[T])
}

// Use appends middleware to the dispatch chain. Middleware registered
// first wraps middleware registered later.
//
// Middleware applies to every event type on the bus and takes effect for
// dispatches that begin after the call returns.
func (b *Bus) Use(mw ...Middleware) {
	if len(mw) == 0 {
		return
	}
	b.mu.Lock()
	cur := *b.middleware.Load()
	next := make([]Middleware, 0, len(cur)+len(mw))
	next = append(next, cur...)
	for _, m := range mw {
		if m != nil {
			next = append(next, m)
		}
	}
	b.middleware.Store(&next)
	b.mu.Unlock()
}

// middlewares returns the current chain. The result is read-only.
func (b *Bus) middlewares() []Middleware { return *b.middleware.Load() }

// ClearMiddleware removes every registered middleware.
func (b *Bus) ClearMiddleware() {
	b.mu.Lock()
	empty := make([]Middleware, 0)
	b.middleware.Store(&empty)
	b.mu.Unlock()
}

// workers returns the async worker pool, starting it on first use.
//
// The pool is lazy so that a bus configured for [AsyncWorkerPool] but
// never used asynchronously does not hold idle goroutines — which matters
// for the default bus, present in every process whether or not it emits.
func (b *Bus) workers() *workerPool {
	b.poolOnce.Do(func() {
		b.pool = newWorkerPool(b.cfg.Workers, b.cfg.QueueSize, b.cfg.AsyncOverflow)
	})
	return b.pool
}

// Name assigns a stable display name to an event type. Names surface in
// the inspector, the recorder and the dashboard, and let operators refer
// to an event without knowing its Go type.
//
// Naming a type twice replaces the previous name.
func Name[T any](b *Bus, name string) {
	e := entryFor[T](b)
	key := e.typ

	b.mu.Lock()
	if prev := e.name.Load(); prev != nil {
		delete(b.byName, *prev)
	}
	e.name.Store(&name)
	b.byName[name] = key
	b.mu.Unlock()
}

// NameOf returns the display name registered for T, or its Go type name.
func NameOf[T any](b *Bus) string {
	if e := b.lookup(typeKey[T]()); e != nil {
		return e.displayName()
	}
	return typeKey[T]().String()
}

// Count returns the number of listeners registered for T.
func Count[T any](b *Bus) int {
	if e := b.lookup(typeKey[T]()); e != nil {
		return e.erased.len()
	}
	return 0
}

// Has reports whether T has at least one listener.
func Has[T any](b *Bus) bool { return Count[T](b) > 0 }

// Clear removes every listener registered for T.
func Clear[T any](b *Bus) {
	if e := b.lookup(typeKey[T]()); e != nil {
		e.erased.clearAll()
	}
}

// EventNames returns the display names of every registered event type,
// sorted alphabetically.
func (b *Bus) EventNames() []string {
	var out []string
	b.entries.Range(func(_, v any) bool {
		out = append(out, v.(*entry).displayName())
		return true
	})
	sort.Strings(out)
	return out
}

// EventCount returns the number of event types that have a registry
// entry. A type keeps its entry after its last listener is removed, so
// this counts types the bus has seen, not types currently listened to.
func (b *Bus) EventCount() int {
	n := 0
	b.entries.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// ListenerCount returns the total number of listeners across all events.
func (b *Bus) ListenerCount() int {
	n := 0
	b.entries.Range(func(_, v any) bool {
		n += v.(*entry).erased.len()
		return true
	})
	return n
}

// HasEvent reports whether an event type with the given display name has
// at least one listener.
func (b *Bus) HasEvent(name string) bool {
	b.mu.Lock()
	key, ok := b.byName[name]
	b.mu.Unlock()
	if !ok {
		return false
	}
	e := b.lookup(key)
	return e != nil && e.erased.len() > 0
}

// Reset removes every listener from every event type, along with all
// middleware, and clears the recorder. Metrics and names are retained.
//
// It exists mainly so tests can return the default bus to a known state.
func (b *Bus) Reset() {
	b.entries.Range(func(_, v any) bool {
		v.(*entry).erased.clearAll()
		return true
	})
	b.ClearMiddleware()
	b.recorder.clear()
}

// Close shuts the bus down. Subsequent emits return [ErrBusClosed] and
// registrations are ignored.
//
// Close waits for the async worker pool to drain, so listeners already
// scheduled still run to completion. Listeners started with
// [AsyncGoroutine] are not tracked and may outlive the call — the mode
// offers no completion signal by construction.
//
// Close is idempotent.
func (b *Bus) Close() {
	if !b.closed.CompareAndSwap(false, true) {
		return
	}
	if b.pool != nil {
		b.pool.close()
	}
}

// Closed reports whether [Bus.Close] has been called.
func (b *Bus) Closed() bool { return b.closed.Load() }
