package rpc

// diag.go — the JSON-RPC server's diagnostic probe.
//
// # Why this subsystem needed one most
//
// A JSON-RPC listener is the framework's most opaque subsystem from the outside.
// It runs on its own port with its own protocol, so none of the HTTP-shaped tools
// see it at all: it has no routes, its calls do not appear in the dashboard's
// request list, and a method name that nothing registered answers -32601 to the
// client and reports nothing anywhere else. A project that generated
// `breeze add jsonrpc` and wired the methods into the wrong registry gets a
// listener that works perfectly and answers "method not found" to everything.
//
// This probe reports the method table, so that failure is one read away: the
// names actually registered, how many run on the event loop versus a worker, and
// whether a pool exists for the blocking ones to run on.
//
// # What is counted, and what is not
//
// Counts here are gated through [diag.Counter], because the dispatch path is the
// framework's cheapest — a registry lookup and a JSON decode — and an
// unconditional atomic per call would be a measurable fraction of it.
//
// The exception is unknownMethods, which is ungated for the same reason the
// recovery middleware's panic count is: it is the number that explains a broken
// deployment, and it must not be zero because nobody enabled counting. It is also
// incremented only on a path that is already writing an error response, so it
// costs nothing measurable there.

import (
	"fmt"
	"sort"
	"sync/atomic"

	"github.com/nelthaarion/breeze/diag"
)

// diagName is the registry key, matching the `breeze add jsonrpc` feature name.
const diagName = "jsonrpc"

// callCounter counts dispatched calls. Hits are calls that reached a handler,
// misses are calls for a method that is not registered.
var callCounter diag.Counter

// Ungated facts. See the file comment for why these two are not behind the gate.
var (
	unknownMethods  atomic.Uint64
	lastUnknownName atomic.Pointer[string]
)

// registerDiagnostics publishes s as the process's JSON-RPC diagnostic.
//
// Called by NewServer. A process with two servers reports the one constructed
// last, which is the registry's uniform rule; two JSON-RPC listeners in one
// process is rare enough that a per-port key would be more confusing than the
// limitation.
func (s *Server) registerDiagnostics() {
	if s == nil {
		return
	}
	diag.Register(diagName, s.probe)
}

// noteUnknownMethod records a call for a method that is not registered.
//
// Called from the dispatch path's -32601 branch, which is already formatting an
// error response.
func noteUnknownMethod(name string) {
	unknownMethods.Add(1)
	if len(name) > 120 {
		name = name[:120] + "…"
	}
	lastUnknownName.Store(&name)
	callCounter.Miss()
}

// probe reports the server's state.
func (s *Server) probe() diag.Report {
	if s == nil || s.reg == nil {
		return diag.Off("no JSON-RPC server is registered; construct one with " +
			"rpc.NewServer(rpc.NewRegistry())")
	}

	methods, blocking := s.methodSummary()
	snap := callCounter.Snapshot()
	unknown := unknownMethods.Load()

	detail := map[string]any{
		"methods":           methods,
		"method_count":      len(methods),
		"blocking_methods":  blocking,
		"inline_methods":    len(methods) - blocking,
		"global_middleware": len(s.reg.Middlewares()),
		"worker_pool":       s.pool != nil,
		"max_message_bytes": s.maxMessageBytes,
		"calls_dispatched":  snap.Hits,
		"unknown_methods":   unknown,
		"counting":          snap.Counting,
	}
	if last := lastUnknownName.Load(); last != nil {
		detail["last_unknown_method"] = *last
	}
	if snap.Last != "" {
		detail["last_call"] = snap.Last
	}

	summary := fmt.Sprintf("%d method(s), %d blocking; %d call(s) dispatched",
		len(methods), blocking, snap.Hits)

	var notes []string
	if !snap.Counting {
		notes = append(notes, "Counted diagnostics are off, so calls_dispatched was not measured. "+
			"unknown_methods and the method list are exact either way.")
	}
	if blocking > 0 && s.pool == nil {
		notes = append(notes, fmt.Sprintf("%d method(s) are registered as blocking but no worker "+
			"pool is set, so each call runs on a fresh goroutine with no bound on concurrency. "+
			"Call SetPool(breeze.NewEventLoopWorkerPool(n)).", blocking))
	}

	// No methods at all is the generated-but-unwired case, and the one this probe
	// exists for: the listener accepts connections and answers -32601 to
	// everything, which looks like a client bug from the client's side.
	if len(methods) == 0 {
		return diag.Degraded("a JSON-RPC server exists but no method is registered — every call "+
			"will be answered with -32601 Method not found", detail).
			WithNotes(append(notes, "Register methods on the registry this server was constructed "+
				"with, or through server.Register/RegisterBlocking. Methods registered on a "+
				"different Registry instance are invisible to this server.")...)
	}

	// A large share of unknown-method calls means the client and server disagree
	// about the method names, which no amount of correct wiring on one side fixes.
	if total := unknown + snap.Hits; total > 0 && unknown*4 > total {
		return diag.Degraded(fmt.Sprintf("%s — %d call(s) named a method that is not registered",
			summary, unknown), detail).
			WithNotes(append(notes, "Over a quarter of calls were for unregistered methods. Compare "+
				"the names in the method list above against what the client is sending; the "+
				"client sees only -32601 and cannot tell a typo from a server that is not "+
				"finished.")...)
	}
	return diag.OK(summary, detail).WithNotes(notes...)
}

// methodSummary lists the registered method names, sorted, and counts the
// blocking ones.
//
// Sorted so two reads of an unchanged server produce identical output. Reads under
// the registry's own lock, held for the length of a map walk and nothing else.
func (s *Server) methodSummary() (names []string, blocking int) {
	s.reg.mu.RLock()
	defer s.reg.mu.RUnlock()

	names = make([]string, 0, len(s.reg.methods))
	for name, m := range s.reg.methods {
		names = append(names, name)
		if m.blocking {
			blocking++
		}
	}
	sort.Strings(names)
	return names, blocking
}
