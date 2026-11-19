package events

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Context carries per-dispatch state. One Context is created for each
// Emit and shared by every listener and middleware that runs during that
// dispatch, which makes it the channel through which listeners cooperate:
// a validator can stash a parsed value with Set and a later persister can
// read it with Get.
//
// # Lifetime
//
// A Context is valid only for the duration of the dispatch that created
// it. Contexts are pooled and reused, so retaining one after Emit returns
// (or after an async listener finishes) will observe another dispatch's
// state. Copy anything you need instead.
//
// Async dispatch is the exception: a Context handed to async listeners is
// not pooled, because the dispatcher cannot know when the last listener
// has finished without adding synchronisation to the hot path.
//
// # Concurrency
//
// Under [EmitAsync] the same Context is shared by listeners running on
// different goroutines. Cancel, Cancelled, Set, Get and the metadata
// accessors are therefore all safe for concurrent use. The immutable
// fields (Time, EventID, EventName, CorrelationID, RequestID) are written
// before any listener runs and only read afterwards.
type Context struct {
	// Time is the instant the dispatch started.
	Time time.Time

	// EventID uniquely identifies this dispatch within the process. It is
	// drawn from a monotonically increasing per-bus counter.
	EventID uint64

	// EventName is the registered name of the event type, or its Go type
	// name when no name has been assigned via [Name].
	EventName string

	// CorrelationID ties a dispatch to a wider logical operation (a saga,
	// a job run, a trace).
	//
	// The bus never fills it in and does not propagate it: it starts empty
	// on every dispatch. A [Middleware] may assign it (it runs before the
	// listeners, and the value is carried into [DispatchResult] and the
	// recorder), and a listener that emits a follow-up event is responsible
	// for carrying the identifier forward itself, for instance in the event
	// value. (An earlier version of this comment promised propagation "via
	// Context.Emitter"; no such method has ever existed.)
	CorrelationID string

	// RequestID ties a dispatch to the inbound HTTP request or WebSocket
	// frame that triggered it. Like CorrelationID it is empty unless a
	// [Middleware] sets it.
	RequestID string

	// Ctx is the standard context governing the dispatch. It is never nil
	// during a dispatch; it defaults to context.Background().
	Ctx context.Context

	// bus is the owning bus.
	bus *Bus

	// cancelled is atomic because async listeners may cancel concurrently.
	cancelled atomic.Bool

	// mu guards meta. Metadata is touched far less often than Cancelled,
	// so a mutex is preferable to the memory cost of a sync.Map here.
	mu sync.Mutex

	// meta is allocated lazily: dispatches that never call Set pay nothing.
	meta map[string]any

	// pooled records whether this Context should be returned to the pool.
	// Async dispatch clears it because the release point is unknowable.
	pooled bool

	// index is the position of the currently executing listener, exposed
	// through [Context.ListenerIndex] for diagnostics.
	index int
}

// contextPool recycles Context values across synchronous dispatches.
var contextPool = sync.Pool{
	New: func() any { return new(Context) },
}

// newContext leases a Context from the pool and initialises it.
func newContext(b *Bus, name string, ctx context.Context) *Context {
	c := contextPool.Get().(*Context)
	c.Time = time.Now()
	c.EventID = b.nextEventID()
	c.EventName = name
	c.CorrelationID = ""
	c.RequestID = ""
	if ctx == nil {
		ctx = context.Background()
	}
	c.Ctx = ctx
	c.bus = b
	c.cancelled.Store(false)
	c.meta = nil
	c.pooled = true
	c.index = 0
	return c
}

// release returns a Context to the pool if it is poolable.
//
// The metadata map is dropped rather than cleared so that values captured
// by listeners cannot leak into an unrelated dispatch, and so that a
// single large map does not pin memory in the pool indefinitely.
func (c *Context) release() {
	if c == nil || !c.pooled {
		return
	}
	c.meta = nil
	c.Ctx = nil
	c.bus = nil
	contextPool.Put(c)
}

// Cancel stops the dispatch. Listeners that have not started yet will be
// skipped; a listener already running is not interrupted.
//
// Cancel is the imperative counterpart to returning [Stop]; both suppress
// the remaining listeners, but Cancel can also be called from middleware
// or from a nested helper that has no way to return an error.
func (c *Context) Cancel() { c.cancelled.Store(true) }

// cancelOnce cancels the dispatch and reports whether this call was the one
// that did, as opposed to finding it already cancelled.
func (c *Context) cancelOnce() bool { return !c.cancelled.Swap(true) }

// Cancelled reports whether the dispatch has been cancelled, either by
// [Context.Cancel] or by a listener returning [Stop].
func (c *Context) Cancelled() bool { return c.cancelled.Load() }

// ListenerIndex returns the zero-based position of the currently executing
// listener within the dispatch order. It is intended for logging.
func (c *Context) ListenerIndex() int { return c.index }

// Set stores a value under key for the remainder of the dispatch.
func (c *Context) Set(key string, value any) {
	c.mu.Lock()
	if c.meta == nil {
		// 4 buckets covers the overwhelming majority of dispatches
		// without over-allocating for the ones that use metadata lightly.
		c.meta = make(map[string]any, 4)
	}
	c.meta[key] = value
	c.mu.Unlock()
}

// Get returns the value stored under key and whether it was present.
func (c *Context) Get(key string) (any, bool) {
	c.mu.Lock()
	v, ok := c.meta[key]
	c.mu.Unlock()
	return v, ok
}

// Metadata returns a copy of all metadata set on the Context. It returns
// nil when no metadata was set. The copy keeps callers (the dashboard, the
// recorder) from racing with listeners that are still writing.
func (c *Context) Metadata() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.meta) == 0 {
		return nil
	}
	out := make(map[string]any, len(c.meta))
	for k, v := range c.meta {
		out[k] = v
	}
	return out
}

// Delete removes key from the metadata.
func (c *Context) Delete(key string) {
	c.mu.Lock()
	delete(c.meta, key)
	c.mu.Unlock()
}

// MetaString returns the value under key as a string. It reports false if
// the key is absent or holds a different type. This saves listeners from
// writing the same two-step assertion for the common string case.
func (c *Context) MetaString(key string) (string, bool) {
	v, ok := c.Get(key)
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// GetMeta returns the value stored under key as T, reporting false if the
// key is absent or holds a different type.
//
// It is a free function rather than a method because Go does not permit
// type parameters on methods.
//
//	user, ok := events.GetMeta[*User](ctx, "user")
func GetMeta[T any](c *Context, key string) (T, bool) {
	var zero T
	v, ok := c.Get(key)
	if !ok {
		return zero, false
	}
	t, ok := v.(T)
	if !ok {
		return zero, false
	}
	return t, ok
}
