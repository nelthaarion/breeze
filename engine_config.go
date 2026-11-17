package breeze

import (
	"sync/atomic"
	"time"

	"github.com/nelthaarion/breeze/v2/gnetcfg"
	"github.com/nelthaarion/gnetplus/v2"
)

// engine_config.go — the gnet engine settings of a Breeze server.
//
// Run used to hand gnet a list of literals: the event-loop count, the buffer sizes, the
// keep-alive, which socket options to set. They are now one gnetcfg.Config, set with
// SetEngineConfig, and every field left at zero keeps the value Run has always used.

// defaultTCPKeepAlive is the keep-alive idle time Run asks gnet for when the EngineConfig
// leaves it unset. It is short on purpose: a client that vanished without a FIN (a pulled
// cable, a NAT that dropped the mapping) is found in seconds instead of holding a connection
// slot for the kernel's two-hour default.
const defaultTCPKeepAlive = 5 * time.Second

// gnetRun is gnetplus.Run. It is a variable so that a test can see what Run hands to gnet
// without opening a socket.
var gnetRun = gnetplus.Run

// engineFields is the engine-configuration part of Breeze, embedded in it the way
// shutdownFields and mcpFields are.
type engineFields struct {
	// engineCfg is what SetEngineConfig stored, normalized. Nil means the zero Config.
	engineCfg atomic.Pointer[gnetcfg.Config]

	// tickEvery is the period OnTick asks gnet for, in nanoseconds, as Run resolved it from
	// engineCfg. Zero before Run, which tickInterval reads as the default.
	tickEvery atomic.Int64
}

// SetEngineConfig sets the gnet engine configuration. Call it before Run: the configuration
// is read once, when the server starts listening, and a change made afterwards waits for the
// next start. The zero gnetcfg.Config, which is also what a server that never calls this has,
// is the configuration Breeze always ran with:
//
//   - one event loop per worker in the pool, round-robin assignment of connections
//   - 64 KiB read and write buffers, TCP_NODELAY on
//   - a five second TCP keep-alive
//   - SO_REUSEADDR on, except on Windows, and SO_REUSEPORT off
//   - a one second tick for the coarse clock and the idle and read-header timeouts
//
// A value that is out of range is replaced by the nearest valid one (see gnetcfg.Config.Normalized),
// so a bad setting cannot stop the server from starting. What the server will use is what
// EngineConfig reports back.
//
//	app.SetEngineConfig(gnetcfg.Config{
//		EventLoops:   4,
//		TCPKeepAlive: 30 * time.Second,
//	})
//
// The event-loop yield policy is chosen separately, with SetPollYield, and the connection
// timeouts with SetIdleTimeout and SetReadHeaderTimeout.
func (s *Breeze) SetEngineConfig(c gnetcfg.Config) {
	n := c.Normalized()
	s.engineCfg.Store(&n)
}

// EngineConfig reports the engine configuration SetEngineConfig stored, normalized. A server
// that was never given one reports the zero value, which means the defaults.
func (s *Breeze) EngineConfig() gnetcfg.Config {
	if p := s.engineCfg.Load(); p != nil {
		return *p
	}
	return gnetcfg.Config{}
}

// gnetOptions resolves the engine configuration into the one gnetplus.Options that Run
// passes to gnet, and records the tick period OnTick will return. noYield is the poll-yield
// decision Run has already taken.
func (s *Breeze) gnetOptions(multiCore, noYield bool) gnetplus.Options {
	cfg := s.EngineConfig()
	s.tickEvery.Store(int64(cfg.Tick()))

	return cfg.Resolve(gnetcfg.Defaults{
		// The loop count Breeze has always used: as many loops as workers. The pool is sized by
		// the caller, who knows how many cores the process may use.
		EventLoops:   s.Pool.Metrics().Workers,
		TCPKeepAlive: defaultTCPKeepAlive,
		ReuseAddr:    reuseAddr,
		// OnTick advances the coarse clock and enforces the idle and read-header timeouts. A
		// server without the ticker enforces neither, so it is not a setting.
		Ticker:      true,
		NoPollYield: noYield,
	}, multiCore)
}

// tickInterval is the period OnTick asks gnet for.
func (s *Breeze) tickInterval() time.Duration {
	if d := s.tickEvery.Load(); d > 0 {
		return time.Duration(d)
	}
	return gnetcfg.DefaultTickInterval
}
