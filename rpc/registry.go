package rpc

import "sync"

// HandlerFunc handles one JSON-RPC call.
//
// It mirrors breeze.HandlerFunc — one argument, no return value, the reply set
// on the context — so a developer moving between an HTTP route and an RPC
// method writes the same shape of function. Returning an error instead would
// have been a smaller signature but a different idiom from the rest of the
// framework, and it would not have covered middleware, which needs to run code
// after the handler as well as before it.
type HandlerFunc func(*Context)

// method is one registered method with its chain flattened at registration.
//
// chain is [global middleware..., method middleware..., handler] built once by
// buildChain, exactly as Router precomputes a route's chain. At call time it is
// read-only — Context.Next mutates only the index — so dispatch assigns it
// directly with no per-call allocation.
type method struct {
	name     string
	chain    []HandlerFunc
	blocking bool
}

// Registry maps method names to handlers.
//
// # Concurrency
//
// Registration is expected to happen during setup, before Run, which is how the
// HTTP router is used. Unlike the router, though, lookups here take a read lock
// rather than assuming the map is frozen: an RPC server commonly gains methods
// as plugins or modules initialise, and a torn map read is a crash rather than a
// wrong answer. An RWMutex read lock on an uncontended path is a single atomic
// add, which does not show up against the JSON decode that follows it.
type Registry struct {
	mu sync.RWMutex

	// methods is the lookup table. A plain map is used rather than the
	// router's trie because JSON-RPC method names are opaque strings with no
	// path structure to exploit — there are no parameters to capture and no
	// prefixes to share, so hashing the name once beats walking it.
	methods map[string]*method

	// middlewares are the global chain, prepended to every method's chain.
	middlewares []HandlerFunc
}

// NewRegistry returns an empty Registry, mirroring NewRouter.
func NewRegistry() *Registry {
	return &Registry{methods: make(map[string]*method, 16)}
}

// Use appends global middleware, mirroring Router.Use.
//
// Like the router, Use rebuilds the chains of already-registered methods, so
// registration order between Use and Register does not change behaviour. That
// property is worth the rebuild: the alternative silently drops middleware from
// any method registered before the Use call, which is a bug that only shows up
// in production traffic.
func (r *Registry) Use(mw ...HandlerFunc) {
	if len(mw) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.middlewares = append(r.middlewares, mw...)
	for name, m := range r.methods {
		// The method's own middleware is everything between the old global
		// prefix and the trailing handler. Recovering it from the flattened
		// chain avoids storing a second copy per method.
		own := m.chain[len(r.middlewares)-len(mw) : len(m.chain)-1]
		handler := m.chain[len(m.chain)-1]
		r.methods[name] = &method{
			name:     name,
			chain:    r.buildChain(handler, own),
			blocking: m.blocking,
		}
	}
}

// Register registers handler under name, mirroring Router.Handle.
//
// The handler runs inline on the gnet event-loop goroutine that read the
// request. Use RegisterBlocking for anything that performs I/O or takes a lock.
//
// Registering the same name twice replaces the first registration. The router
// keeps the first match instead, but that behaviour is a consequence of walking
// a route list, and silently ignoring a re-registration is the less useful of
// the two for a method table — a plugin that overrides a built-in method should
// win, and a duplicate registration is otherwise impossible to notice.
func (r *Registry) Register(name string, handler HandlerFunc, middlewares ...HandlerFunc) {
	r.register(name, handler, middlewares, false)
}

// RegisterBlocking registers a handler that must not run on an event loop,
// mirroring Router.HandleBlocking.
//
// A call to a blocking method is handed to the server's worker pool and
// answered with AsyncWrite. Without a pool configured the server runs it on a
// fresh goroutine, which is still off the event loop.
func (r *Registry) RegisterBlocking(name string, handler HandlerFunc, middlewares ...HandlerFunc) {
	r.register(name, handler, middlewares, true)
}

func (r *Registry) register(name string, handler HandlerFunc, mw []HandlerFunc, blocking bool) {
	if name == "" || handler == nil {
		// A nil handler would panic on the first call to the method, which is
		// a worse failure than never registering it: the panic happens under
		// traffic, far from the registration bug that caused it.
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.methods[name] = &method{
		name:     name,
		chain:    r.buildChain(handler, mw),
		blocking: blocking,
	}
}

// buildChain flattens [global..., own..., handler] into one slice.
//
// Called under r.mu. The result is allocated exactly once, at its final length,
// and never mutated afterwards, which is what makes it safe to share across
// concurrent calls without copying per request.
func (r *Registry) buildChain(handler HandlerFunc, own []HandlerFunc) []HandlerFunc {
	chain := make([]HandlerFunc, 0, len(r.middlewares)+len(own)+1)
	chain = append(chain, r.middlewares...)
	chain = append(chain, own...)
	return append(chain, handler)
}

// lookup returns the method registered under name.
func (r *Registry) lookup(name string) (*method, bool) {
	r.mu.RLock()
	m, ok := r.methods[name]
	r.mu.RUnlock()
	return m, ok
}

// Methods returns the registered method names, unsorted.
//
// It exists for introspection — a "rpc.discover"-style endpoint, a startup log
// line, or a test asserting what got wired up. The slice is freshly allocated,
// so the caller may keep it.
func (r *Registry) Methods() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.methods))
	for name := range r.methods {
		names = append(names, name)
	}
	return names
}

// Middlewares returns the global middleware chain, mirroring Router.Middlewares.
func (r *Registry) Middlewares() []HandlerFunc {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.middlewares
}
