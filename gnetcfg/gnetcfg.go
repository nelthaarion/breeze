// Package gnetcfg holds the settings Breeze's servers hand to the gnet engine.
//
// The HTTP server (breeze.Breeze) and the JSON-RPC server (rpc.Server) used to pass these to
// gnetplus.Run as literals, each in its own copy of the same option list. They live here
// instead, in one struct whose zero value means "the default this server has always used":
// a deployment can change one setting without editing the framework, and the two servers
// cannot drift apart.
//
// # How a Config reaches gnet
//
// [Config.Resolve] turns a Config into one gnetplus.Options value, and a server passes that
// to gnetplus.Run through a single gnetplus.WithOptions. That is deliberate. WithOptions
// replaces the whole option set instead of adding to it, so every option listed before it is
// thrown away. Breeze lost its ticker exactly that way: WithTicker(true) sat before a
// WithOptions, OnTick was never scheduled, and neither the idle timeout nor the read-header
// timeout ever closed a connection. A struct built in one place has no order to get wrong.
//
// # What is not configurable, and why
//
// The ticker. Breeze's connection timeouts run from OnTick, so a server with the ticker off
// quietly stops enforcing them; whether the ticker runs is a property of the server, set in
// [Defaults], not a knob. The interval between ticks is a knob ([Config.TickInterval]).
package gnetcfg

import (
	"runtime"
	"time"

	"github.com/nelthaarion/gnetplus/v2"
)

const (
	// DefaultBufferCap is the read and the write buffer capacity per event loop when Config
	// leaves it unset: 64 KiB, gnet's own default and large enough to take a pipelined batch
	// of small requests in one read.
	DefaultBufferCap = 64 << 10

	// MaxBufferCap bounds ReadBufferCap and WriteBufferCap. The memory is paid once per event
	// loop, so a figure in the hundreds of megabytes is a unit mix-up (bytes for kilobytes),
	// not a tuning, and it is lowered to this instead of being allocated.
	MaxBufferCap = 64 << 20

	// MaxEventLoops is the most event loops gnet will run; a larger request is lowered to it.
	MaxEventLoops = 256

	// DefaultTickInterval is how often a server's OnTick runs when Config leaves it unset.
	DefaultTickInterval = time.Second

	// MinTickInterval is the shortest tick accepted. Every tick walks the connection table,
	// so a shorter one costs more than the timeout resolution it buys.
	MinTickInterval = 10 * time.Millisecond
)

// Config is the gnet engine configuration of one server. The zero value is a complete,
// safe configuration: every field left at zero takes the default documented on it.
//
// Set it before the server starts. A server reads it once, when it begins to listen.
type Config struct {
	// EventLoops is how many gnet event loops serve connections. Zero keeps the server's own
	// default: Breeze runs one loop per worker in its pool, and the JSON-RPC server lets its
	// multiCore argument decide (one loop, or one per usable CPU). A negative value counts as
	// zero; a value above [MaxEventLoops] is lowered to it.
	EventLoops int

	// LoadBalancing is how a newly accepted connection is assigned to an event loop. The zero
	// value is gnetplus.RoundRobin. It does nothing on a ReusePort server, where the kernel
	// does the assigning. A value gnet does not know counts as RoundRobin.
	LoadBalancing gnetplus.LoadBalancing

	// ReadBufferCap is the most bytes an event loop reads from one connection per readable
	// event. Zero means [DefaultBufferCap]; a negative value counts as zero; a value above
	// [MaxBufferCap] is lowered to it. gnet rounds it up to a power of two.
	ReadBufferCap int

	// WriteBufferCap is the size of the static outbound buffer; bytes past it go to gnet's
	// elastic list. The same zero, negative, maximum and rounding rules as ReadBufferCap.
	WriteBufferCap int

	// TCPKeepAlive turns on SO_KEEPALIVE and is the idle time before the first probe (gnet
	// probes five times, at a fifth of this apart). Zero keeps the server's default: five
	// seconds for Breeze, off for the JSON-RPC server. A negative value turns keep-alive off.
	TCPKeepAlive time.Duration

	// TickInterval is the period of the server's OnTick, which Breeze uses to advance its
	// coarse clock and to enforce the idle and read-header timeouts, so it is also the
	// resolution of those two. Zero means [DefaultTickInterval]; a negative value counts as
	// zero; a positive one below [MinTickInterval] is raised to it. A server that has no
	// ticker (the JSON-RPC server) ignores it.
	TickInterval time.Duration

	// DisableTCPNoDelay leaves Nagle's algorithm on. The default is TCP_NODELAY on, because a
	// request-response protocol must not wait for a delayed ACK before it can answer.
	DisableTCPNoDelay bool

	// DisableReuseAddr leaves SO_REUSEADDR off. The default is on for a TCP listener, so a
	// server restarted straight after it served traffic can bind its port again instead of
	// failing with "address already in use" while the old connections sit in TIME_WAIT.
	//
	// It is never set on Windows whatever this says. There SO_REUSEADDR does not mean what it
	// means elsewhere: it lets a second socket bind a port that another process is already
	// listening on and take connections from it.
	DisableReuseAddr bool

	// ReusePort sets SO_REUSEPORT and gives every event loop its own listener, which spreads
	// accepts across loops in the kernel. Off by default, because it also lets another
	// process bind the same port, and because it changes how connections are distributed.
	// Honoured only where gnet supports it for a multi-loop server (Linux, FreeBSD and
	// DragonFly); everywhere else it is ignored, and on Windows in particular it would become
	// SO_REUSEADDR, with the effect described above.
	ReusePort bool

	// AlwaysPollYield makes every idle event loop yield to the Go scheduler before it blocks
	// in the kernel, as upstream gnet does and as this fork did before the yield became
	// adaptive. Off by default, which is gnetplus's adaptive behaviour. A poll-yield policy
	// that removes the yield (breeze.PollYieldNever or PollYieldAuto) wins over this.
	AlwaysPollYield bool
}

// Defaults are the choices that belong to a server rather than to its operator: what the
// zero Config means for this server, and what the server needs from the engine.
type Defaults struct {
	// EventLoops is the loop count used when Config.EventLoops is zero. Zero lets multiCore
	// decide.
	EventLoops int

	// TCPKeepAlive is the keep-alive idle time used when Config.TCPKeepAlive is zero. Zero is
	// off.
	TCPKeepAlive time.Duration

	// ReuseAddr is whether this listener wants SO_REUSEADDR unless Config.DisableReuseAddr
	// says otherwise. A listener that is not TCP (a Unix socket, say) has no use for it.
	ReuseAddr bool

	// Ticker is whether the engine must run its ticker, i.e. whether the server has an OnTick
	// that does work. It is not part of Config on purpose; see the package comment.
	Ticker bool

	// NoPollYield is the server's poll-yield decision (see breeze.PollYield), already made.
	NoPollYield bool
}

// Normalized returns c with every out-of-range value replaced by the nearest valid one, so
// that what a server stores, reports and uses is one thing. It is idempotent.
func (c Config) Normalized() Config {
	switch {
	case c.EventLoops < 0:
		c.EventLoops = 0
	case c.EventLoops > MaxEventLoops:
		c.EventLoops = MaxEventLoops
	}

	switch c.LoadBalancing {
	case gnetplus.RoundRobin, gnetplus.LeastConnections, gnetplus.SourceAddrHash:
	default:
		c.LoadBalancing = gnetplus.RoundRobin
	}

	c.ReadBufferCap = clampBuffer(c.ReadBufferCap)
	c.WriteBufferCap = clampBuffer(c.WriteBufferCap)

	// TCPKeepAlive is left as it is: a negative value is an explicit "off", which Resolve has
	// to tell apart from the unset zero.

	if c.TickInterval < 0 {
		c.TickInterval = 0
	} else if c.TickInterval > 0 && c.TickInterval < MinTickInterval {
		c.TickInterval = MinTickInterval
	}
	return c
}

// clampBuffer maps a buffer size to what is stored: zero for "the default", else the size
// bounded by MaxBufferCap.
func clampBuffer(n int) int {
	switch {
	case n <= 0:
		return 0
	case n > MaxBufferCap:
		return MaxBufferCap
	}
	return n
}

// Tick returns the OnTick period the Config asks for, with the default applied.
func (c Config) Tick() time.Duration {
	if t := c.Normalized().TickInterval; t > 0 {
		return t
	}
	return DefaultTickInterval
}

// Resolve returns the gnetplus.Options that d and c add up to. multiCore is the server's own
// Run argument; it matters only where the resulting loop count is zero.
//
// A server passes the result to gnetplus.Run as the one and only option:
//
//	gnetplus.Run(h, addr, gnetplus.WithOptions(cfg.Resolve(defaults, multiCore)))
func (c Config) Resolve(d Defaults, multiCore bool) gnetplus.Options {
	return c.resolve(d, multiCore, runtime.GOOS)
}

// resolve is Resolve with the operating system passed in, so a test can ask what Windows or
// Linux would get without being on it.
func (c Config) resolve(d Defaults, multiCore bool, goos string) gnetplus.Options {
	c = c.Normalized()

	loops := d.EventLoops
	if c.EventLoops > 0 {
		loops = c.EventLoops
	}
	if loops < 0 {
		loops = 0
	}
	if loops > MaxEventLoops {
		loops = MaxEventLoops
	}

	keepAlive := d.TCPKeepAlive
	switch {
	case c.TCPKeepAlive > 0:
		keepAlive = c.TCPKeepAlive
	case c.TCPKeepAlive < 0:
		keepAlive = 0
	}
	if keepAlive < 0 {
		keepAlive = 0
	}

	noDelay := gnetplus.TCPNoDelay
	if c.DisableTCPNoDelay {
		noDelay = gnetplus.TCPDelay
	}

	return gnetplus.Options{
		NumEventLoop:    loops,
		Multicore:       multiCore,
		LB:              c.LoadBalancing,
		ReadBufferCap:   bufferCap(c.ReadBufferCap),
		WriteBufferCap:  bufferCap(c.WriteBufferCap),
		TCPKeepAlive:    keepAlive,
		TCPNoDelay:      noDelay,
		ReuseAddr:       d.ReuseAddr && !c.DisableReuseAddr && goos != "windows",
		ReusePort:       c.ReusePort && reusePortWorks(goos),
		Ticker:          d.Ticker,
		NoPollYield:     d.NoPollYield,
		AlwaysPollYield: c.AlwaysPollYield,
	}
}

// bufferCap turns a normalized buffer size into the value gnet gets: the default for zero.
func bufferCap(n int) int {
	if n == 0 {
		return DefaultBufferCap
	}
	return n
}

// reusePortWorks reports whether SO_REUSEPORT with several event loops is something gnet
// supports on goos. It is the list gnet itself applies before it silently turns the option
// off, repeated here so that the answer is visible in the resolved options.
func reusePortWorks(goos string) bool {
	switch goos {
	case "linux", "freebsd", "dragonfly":
		return true
	}
	return false
}
