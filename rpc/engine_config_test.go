package rpc

import (
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/nelthaarion/breeze/v2/gnetcfg"
	"github.com/nelthaarion/gnetplus/v2"
)

// gnetCall is one call to gnetRun: the address, the options as gnet would apply them, and how
// many options there were.
type gnetCall struct {
	addr string
	opts gnetplus.Options
	n    int
}

// captureGnetCalls replaces gnetRun with a stub that records every call and returns at once (the
// first with failFirst, if it is not nil). gnetRun is restored when the test ends.
func captureGnetCalls(t *testing.T, failFirst error) func() []gnetCall {
	t.Helper()
	var (
		mu    sync.Mutex
		calls []gnetCall
	)
	orig := gnetRun
	t.Cleanup(func() { gnetRun = orig })
	gnetRun = func(_ gnetplus.EventHandler, addr string, opts ...gnetplus.Option) error {
		var o gnetplus.Options
		for _, f := range opts {
			f(&o)
		}
		mu.Lock()
		calls = append(calls, gnetCall{addr: addr, opts: o, n: len(opts)})
		first := len(calls) == 1
		mu.Unlock()
		if first {
			return failFirst
		}
		return nil
	}
	return func() []gnetCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]gnetCall(nil), calls...)
	}
}

func wantReuseAddr() bool { return runtime.GOOS != "windows" }

func TestRunOnHandsGnetTheHistoricalEngineConfigByDefault(t *testing.T) {
	calls := captureGnetCalls(t, nil)
	s := newListenServer()
	if err := s.RunOn("", 9123, true); err != nil {
		t.Fatal(err)
	}
	got := calls()
	if len(got) != 1 {
		t.Fatalf("gnet was asked to bind %d times, want 1", len(got))
	}
	if got[0].n != 1 {
		t.Errorf("RunOn handed gnet %d options, want exactly one (WithOptions replaces the whole set)", got[0].n)
	}
	want := gnetplus.Options{
		Multicore:      true,
		LB:             gnetplus.RoundRobin,
		ReadBufferCap:  64 << 10,
		WriteBufferCap: 64 << 10,
		TCPNoDelay:     gnetplus.TCPNoDelay,
		ReuseAddr:      wantReuseAddr(),
	}
	if got[0].opts != want {
		t.Fatalf("RunOn handed gnet\n  %+v\nwant\n  %+v", got[0].opts, want)
	}
}

func TestRunOnHandsGnetTheConfiguredEngine(t *testing.T) {
	calls := captureGnetCalls(t, nil)
	s := newListenServer()
	s.SetEngineConfig(gnetcfg.Config{
		EventLoops:        2,
		LoadBalancing:     gnetplus.LeastConnections,
		ReadBufferCap:     8 << 10,
		WriteBufferCap:    8 << 10,
		TCPKeepAlive:      time.Minute,
		DisableTCPNoDelay: true,
		DisableReuseAddr:  true,
	})
	if err := s.RunOn("127.0.0.1", 9124, false); err != nil {
		t.Fatal(err)
	}
	want := gnetplus.Options{
		NumEventLoop:   2,
		LB:             gnetplus.LeastConnections,
		ReadBufferCap:  8 << 10,
		WriteBufferCap: 8 << 10,
		TCPKeepAlive:   time.Minute,
		TCPNoDelay:     gnetplus.TCPDelay,
	}
	if got := calls()[0].opts; got != want {
		t.Fatalf("RunOn handed gnet\n  %+v\nwant\n  %+v", got, want)
	}
}

// The wildcard that falls back to IPv4 on a host without IPv6 is the same listener asked a second
// time, so it must get the same engine settings.
func TestTheIPv4FallbackGetsTheSameEngineConfig(t *testing.T) {
	calls := captureGnetCalls(t, &errUnsupported{})
	s := newListenServer()
	s.SetEngineConfig(gnetcfg.Config{EventLoops: 2, ReadBufferCap: 8 << 10})
	if err := s.RunOn("", 9125, false); err != nil {
		t.Fatal(err)
	}
	got := calls()
	if len(got) != 2 {
		t.Fatalf("gnet was asked to bind %d times, want 2 (IPv6 wildcard, then IPv4)", len(got))
	}
	if got[0].opts != got[1].opts || got[0].n != 1 || got[1].n != 1 {
		t.Errorf("the fallback was given different settings:\n  %+v\n  %+v", got[0].opts, got[1].opts)
	}
}

type errUnsupported struct{}

// Error is the text gnet gives back on a host without IPv6; addressFamilyUnsupported reads it.
func (*errUnsupported) Error() string { return "socket: address family not supported by protocol" }

// A Unix socket has no TIME_WAIT, and SO_REUSEADDR is a TCP idea.
func TestRunAddrSetsReuseAddrOnlyForTCP(t *testing.T) {
	for _, tc := range []struct {
		addr string
		want bool
	}{
		{"tcp://127.0.0.1:9126", wantReuseAddr()},
		{"tcp4://0.0.0.0:9127", wantReuseAddr()},
		{"unix:///tmp/breeze-rpc.sock", false},
	} {
		calls := captureGnetCalls(t, nil)
		s := newListenServer()
		if err := s.RunAddr(tc.addr, false); err != nil {
			t.Fatal(err)
		}
		got := calls()
		if len(got) != 1 || got[0].n != 1 {
			t.Fatalf("%s: calls = %+v", tc.addr, got)
		}
		if got[0].opts.ReuseAddr != tc.want {
			t.Errorf("%s: ReuseAddr = %v, want %v", tc.addr, got[0].opts.ReuseAddr, tc.want)
		}
		if got[0].opts.Ticker {
			t.Errorf("%s: asked gnet for a ticker, but this server has no OnTick", tc.addr)
		}
	}
}

func TestServerEngineConfigIsNormalizedAndReported(t *testing.T) {
	s := newListenServer()
	if got := s.EngineConfig(); got != (gnetcfg.Config{}) {
		t.Fatalf("a server that was never configured reports %+v, want the zero Config", got)
	}
	s.SetEngineConfig(gnetcfg.Config{EventLoops: 1 << 20, WriteBufferCap: -1})
	got := s.EngineConfig()
	if got.EventLoops != gnetcfg.MaxEventLoops || got.WriteBufferCap != 0 {
		t.Errorf("EngineConfig() = %+v, want the values brought into range", got)
	}
}
