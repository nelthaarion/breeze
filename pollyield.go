package breeze

import "runtime"

// PollYield selects what the gnet event loops do right before they block in the
// kernel waiting for the next event.
//
// By default gnet calls runtime.Gosched there, so that goroutines the callbacks
// have just made runnable (a worker-pool task, a WebSocket dispatch) get the CPU
// before the loop sleeps. A server that handles every request inline in OnTraffic
// never creates such goroutines, and for it the yield is cost: an extra
// non-blocking poll, a scheduler round trip and the futex wake-ups that go with
// it, on every idle transition.
//
// Skipping it is an opt-in because the effect is workload- and machine-dependent.
// Measured on a 1-vCPU box with wrk sharing the core and GOMAXPROCS=2: 5-13% more
// requests per second for light handlers (plaintext, small JSON, header
// middleware), no change for a JWT-verifying handler, about 9% fewer for an
// MD5-hashing ETag handler, and about 20% fewer with GOMAXPROCS=1. Measure it on
// the hardware you deploy to before enabling it.
type PollYield int32

const (
	// PollYieldDefault keeps gnet's behaviour: yield before blocking. This is
	// what Breeze has always done and it is the default.
	PollYieldDefault PollYield = iota
	// PollYieldAuto skips the yield when this server serves its routes inline
	// (inline execution is on and no route was registered with HandleBlocking,
	// which also covers static files and WebSocket upgrades) and GOMAXPROCS is
	// above 1. It keeps the yield otherwise. The decision is taken once, in Run.
	// When the yield is skipped, a loop that does hand a request to a worker (the
	// auto-served index page, say) yields right after the hand-off instead.
	PollYieldAuto
	// PollYieldNever removes the yield whatever the routes and GOMAXPROCS are.
	PollYieldNever
)

// SetPollYield chooses the event-loop yield policy. Call it before Run.
func (s *Breeze) SetPollYield(mode PollYield) { s.pollYield.Store(int32(mode)) }

func (s *Breeze) noPollYield() bool {
	return decideNoPollYield(PollYield(s.pollYield.Load()), s.inlineExec, s.Router, runtime.GOMAXPROCS(0))
}

func decideNoPollYield(mode PollYield, inline bool, r *Router, procs int) bool {
	switch mode {
	case PollYieldNever:
		return true
	case PollYieldAuto:
		// With a single P nothing else can run while the loop sleeps in the
		// kernel, and the scheduler has to hand the P over and take it back on
		// every sleep; there the yield is the cheaper arrangement.
		return procs > 1 && inline && r != nil && !r.hasBlockingRoutes()
	}
	return false
}

// hasBlockingRoutes reports whether a route registered with HandleBlocking (which
// also covers WebSocket upgrades and static files) exists, i.e. whether requests
// are routinely dispatched off the event loop. The auto-served index page is not
// counted: it only answers "GET /" when no route does, and dispatch yields for it.
func (r *Router) hasBlockingRoutes() bool {
	for _, rt := range r.routes {
		if rt.blocking {
			return true
		}
	}
	return false
}
