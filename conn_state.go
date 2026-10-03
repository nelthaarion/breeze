package breeze

import (
	"bytes"
	"sync/atomic"
	"time"

	"github.com/nelthaarion/gnet/v2"
)

// Per-connection state.
//
// gnet pins a connection to one event-loop goroutine, so almost everything here
// is touched single-threaded. The exceptions are the fields the timeout sweeper
// reads from the ticker goroutine and the busy flag a worker may clear, which is
// why those are atomics.
type connState struct {
	// buf holds bytes received but not yet parsed: a partial next request, or
	// requests held back while an earlier one is still executing.
	buf []byte

	// busy is true while a request dispatched to the worker pool has not yet
	// been answered. While it is set no later request on this connection is
	// parsed, which is what keeps pipelined responses in request order: an inline
	// request answered immediately must not overtake a blocking one still running.
	busy atomic.Bool

	lastRead     atomic.Int64 // coarse unix nanos of the last bytes received
	partialSince atomic.Int64 // when the current incomplete request began, 0 if none
	partialHdr   atomic.Bool  // that incomplete request has not finished its headers
}

func newConnState() *connState {
	st := &connState{}
	st.lastRead.Store(coarseNow())
	return st
}

// connStateOf returns the state stored on c, creating it when OnOpen did not
// (hand-built connections in tests, or a connection opened before this server
// took over its callbacks).
func connStateOf(c gnet.Conn) *connState {
	if st, ok := c.Context().(*connState); ok && st != nil {
		return st
	}
	st := newConnState()
	c.SetContext(st)
	return st
}

// coarse clock ---------------------------------------------------------------
//
// Reading the wall clock on every request costs more than the connection state
// update it feeds. The sweeper only needs second-level resolution, so the clock
// is advanced once a second by OnTick and read with a single atomic load.
var coarseClock atomic.Int64

func init() { coarseClock.Store(time.Now().UnixNano()) }

func coarseNow() int64 { return coarseClock.Load() }

// connEntry is what the sweeper's registry keeps per open connection.
type connEntry struct {
	c  gnet.Conn
	st *connState
}

// Timeouts -------------------------------------------------------------------

const (
	// DefaultIdleTimeout closes a connection that has sent nothing for this long
	// and has no request in flight. Without it, every idle or half-open socket
	// holds a file descriptor forever.
	DefaultIdleTimeout = 120 * time.Second
	// DefaultReadHeaderTimeout bounds how long a request may take to deliver its
	// headers. This is the slowloris defence: a client trickling one byte every
	// few seconds keeps the idle timer fresh, so it needs its own limit.
	DefaultReadHeaderTimeout = 30 * time.Second
)

// SetIdleTimeout sets how long a keep-alive connection may sit silent before the
// server closes it. Zero disables the check.
func (s *Breeze) SetIdleTimeout(d time.Duration) { s.idleTimeout.Store(int64(d)) }

// SetReadHeaderTimeout sets how long a request may take to deliver its header
// block once its first byte has arrived. Zero disables the check.
func (s *Breeze) SetReadHeaderTimeout(d time.Duration) { s.headerTimeout.Store(int64(d)) }

// OnTick advances the coarse clock and sweeps for connections past a timeout.
func (s *Breeze) OnTick() (time.Duration, gnet.Action) {
	now := time.Now().UnixNano()
	coarseClock.Store(now)
	s.sweepConns(now)
	return time.Second, gnet.None
}

func (s *Breeze) sweepConns(now int64) {
	idle, hdr := s.idleTimeout.Load(), s.headerTimeout.Load()
	if idle <= 0 && hdr <= 0 {
		return
	}
	s.conns.Range(func(k, v any) bool {
		// The entry carries the state itself: reading it back through
		// c.Context() from this goroutine would race with gnet releasing the
		// connection on its own event loop.
		e := v.(connEntry)
		c, st := e.c, e.st
		if st.busy.Load() {
			return true // a running handler is not idle
		}
		if s.wsCount.Load() != 0 {
			if _, isWS := s.isWSConn(k.(int)); isWS {
				return true // WebSockets keep their own liveness
			}
		}
		if hdr > 0 {
			if ps := st.partialSince.Load(); ps != 0 && st.partialHdr.Load() && now-ps > hdr {
				_ = c.Close()
				return true
			}
		}
		if idle > 0 {
			if lr := st.lastRead.Load(); lr != 0 && now-lr > idle {
				_ = c.Close()
			}
		}
		return true
	})
}

// Connection header injection --------------------------------------------------

var crlf = []byte("\r\n")

// withConnectionHeader splices "Connection: <value>" in right after the status
// line of an already-serialised response. The serializer strips an application's
// own Connection header (framing belongs to the server), so the server adds its
// own here, only when the client's request calls for one.
func withConnectionHeader(wire []byte, mode uint8) []byte {
	if mode == connModeKeepAlive {
		return wire
	}
	hdr := "Connection: close\r\n"
	if mode == connModeKeepAlive10 {
		hdr = "Connection: keep-alive\r\n"
	}
	i := bytes.Index(wire, crlf)
	if i < 0 {
		return wire
	}
	out := make([]byte, 0, len(wire)+len(hdr))
	out = append(out, wire[:i+2]...)
	out = append(out, hdr...)
	return append(out, wire[i+2:]...)
}
