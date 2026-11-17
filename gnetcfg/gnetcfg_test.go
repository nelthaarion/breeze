package gnetcfg

import (
	"testing"
	"time"

	"github.com/nelthaarion/gnetplus/v2"
)

// breezeDefaults is what the HTTP server passes: one loop per pool worker, a five second keep-alive,
// SO_REUSEADDR where it is safe, and a ticker because the connection timeouts run from OnTick.
func breezeDefaults() Defaults {
	return Defaults{EventLoops: 4, TCPKeepAlive: 5 * time.Second, ReuseAddr: true, Ticker: true}
}

// rpcDefaults is what the JSON-RPC server passes: the loop count left to multiCore, no keep-alive,
// no ticker.
func rpcDefaults() Defaults { return Defaults{ReuseAddr: true} }

func TestZeroConfigIsTheHistoricalBreezeConfiguration(t *testing.T) {
	got := Config{}.resolve(breezeDefaults(), false, "linux")
	want := gnetplus.Options{
		NumEventLoop:   4,
		LB:             gnetplus.RoundRobin,
		ReadBufferCap:  64 << 10,
		WriteBufferCap: 64 << 10,
		TCPKeepAlive:   5 * time.Second,
		TCPNoDelay:     gnetplus.TCPNoDelay,
		ReuseAddr:      true,
		Ticker:         true,
	}
	if got != want {
		t.Fatalf("zero Config resolved to\n  %+v\nwant\n  %+v", got, want)
	}
}

func TestZeroConfigIsTheHistoricalRPCConfiguration(t *testing.T) {
	got := Config{}.resolve(rpcDefaults(), true, "linux")
	if got.NumEventLoop != 0 || !got.Multicore {
		t.Errorf("loops = %d, multicore = %v: with no configured count multiCore has to decide", got.NumEventLoop, got.Multicore)
	}
	if got.TCPKeepAlive != 0 {
		t.Errorf("keep-alive = %v, want off", got.TCPKeepAlive)
	}
	if got.Ticker {
		t.Error("the JSON-RPC server has no OnTick, so it must not ask for a ticker")
	}
	if got.ReadBufferCap != 64<<10 || got.WriteBufferCap != 64<<10 || got.TCPNoDelay != gnetplus.TCPNoDelay || got.LB != gnetplus.RoundRobin {
		t.Errorf("buffers/no-delay/load balancing are not the historical ones: %+v", got)
	}
}

// The ticker is a property of the server, not of the operator: a Config has no field for it, and
// whatever the Config says the server's choice comes through. This is the property whose absence
// switched off the idle and read-header timeouts.
func TestTheTickerIsTheServersChoiceWhateverTheConfigSays(t *testing.T) {
	cfgs := []Config{
		{},
		{EventLoops: 8, ReusePort: true, DisableReuseAddr: true, DisableTCPNoDelay: true},
		{TickInterval: time.Hour},
	}
	for _, c := range cfgs {
		for _, goos := range []string{"linux", "windows", "darwin"} {
			if !c.resolve(Defaults{Ticker: true}, false, goos).Ticker {
				t.Errorf("%+v on %s lost the ticker", c, goos)
			}
			if c.resolve(Defaults{Ticker: false}, false, goos).Ticker {
				t.Errorf("%+v on %s turned a ticker on that the server did not ask for", c, goos)
			}
		}
	}
}

func TestEveryFieldReachesGnet(t *testing.T) {
	c := Config{
		EventLoops:        2,
		LoadBalancing:     gnetplus.LeastConnections,
		ReadBufferCap:     128 << 10,
		WriteBufferCap:    32 << 10,
		TCPKeepAlive:      30 * time.Second,
		DisableTCPNoDelay: true,
		DisableReuseAddr:  true,
		ReusePort:         true,
		AlwaysPollYield:   true,
	}
	d := breezeDefaults()
	d.NoPollYield = true
	got := c.resolve(d, true, "linux")
	want := gnetplus.Options{
		NumEventLoop:    2,
		Multicore:       true,
		LB:              gnetplus.LeastConnections,
		ReadBufferCap:   128 << 10,
		WriteBufferCap:  32 << 10,
		TCPKeepAlive:    30 * time.Second,
		TCPNoDelay:      gnetplus.TCPDelay,
		ReuseAddr:       false,
		ReusePort:       true,
		Ticker:          true,
		NoPollYield:     true,
		AlwaysPollYield: true,
	}
	if got != want {
		t.Fatalf("resolved to\n  %+v\nwant\n  %+v", got, want)
	}
}

func TestKeepAliveHasThreeStates(t *testing.T) {
	d := breezeDefaults()
	for _, tc := range []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"unset keeps the server's default", 0, 5 * time.Second},
		{"positive replaces it", 90 * time.Second, 90 * time.Second},
		{"negative turns it off", -1, 0},
	} {
		if got := (Config{TCPKeepAlive: tc.in}).resolve(d, false, "linux").TCPKeepAlive; got != tc.want {
			t.Errorf("%s: TCPKeepAlive = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestOutOfRangeValuesAreBroughtIntoRange(t *testing.T) {
	got := Config{
		EventLoops:     -3,
		LoadBalancing:  gnetplus.LoadBalancing(99),
		ReadBufferCap:  -1,
		WriteBufferCap: 1 << 40,
		TickInterval:   time.Nanosecond,
	}.Normalized()
	if got.EventLoops != 0 {
		t.Errorf("EventLoops = %d, want 0", got.EventLoops)
	}
	if got.LoadBalancing != gnetplus.RoundRobin {
		t.Errorf("LoadBalancing = %v, want RoundRobin for a value gnet does not know", got.LoadBalancing)
	}
	if got.ReadBufferCap != 0 {
		t.Errorf("ReadBufferCap = %d, want 0 (the default)", got.ReadBufferCap)
	}
	if got.WriteBufferCap != MaxBufferCap {
		t.Errorf("WriteBufferCap = %d, want it lowered to %d", got.WriteBufferCap, MaxBufferCap)
	}
	if got.TickInterval != MinTickInterval {
		t.Errorf("TickInterval = %v, want %v", got.TickInterval, MinTickInterval)
	}

	if (Config{EventLoops: 100000}).Normalized().EventLoops != MaxEventLoops {
		t.Error("an absurd loop count was not lowered to what gnet runs")
	}
	if (Config{TickInterval: -time.Second}).Normalized().TickInterval != 0 {
		t.Error("a negative tick interval was not read as unset")
	}
}

func TestNormalizedIsIdempotent(t *testing.T) {
	c := Config{EventLoops: 1 << 20, ReadBufferCap: -5, WriteBufferCap: 1 << 50, TCPKeepAlive: -time.Hour, TickInterval: time.Microsecond}
	once := c.Normalized()
	if twice := once.Normalized(); twice != once {
		t.Fatalf("Normalized changed its own output:\n  %+v\n  %+v", once, twice)
	}
	// Resolving an unnormalized Config must give what resolving the normalized one gives: the
	// server stores the normalized form and a caller of Resolve may not have one.
	d := breezeDefaults()
	if c.resolve(d, false, "linux") != once.resolve(d, false, "linux") {
		t.Error("Resolve does not normalize")
	}
}

func TestTickFallsBackToTheDefault(t *testing.T) {
	if got := (Config{}).Tick(); got != DefaultTickInterval {
		t.Errorf("Tick() = %v, want %v", got, DefaultTickInterval)
	}
	if got := (Config{TickInterval: 250 * time.Millisecond}).Tick(); got != 250*time.Millisecond {
		t.Errorf("Tick() = %v, want 250ms", got)
	}
	if got := (Config{TickInterval: time.Nanosecond}).Tick(); got != MinTickInterval {
		t.Errorf("Tick() = %v, want %v", got, MinTickInterval)
	}
}

// SO_REUSEADDR on Windows lets a second socket take a port that another process is listening on,
// so nothing may turn it on there, and SO_REUSEPORT would be translated into it.
func TestWindowsNeverGetsReuseAddrOrReusePort(t *testing.T) {
	for _, c := range []Config{{}, {ReusePort: true}, {ReusePort: true, DisableReuseAddr: false}} {
		got := c.resolve(breezeDefaults(), false, "windows")
		if got.ReuseAddr || got.ReusePort {
			t.Errorf("%+v on windows resolved to ReuseAddr=%v ReusePort=%v", c, got.ReuseAddr, got.ReusePort)
		}
	}
}

func TestReusePortIsHonouredOnlyWhereGnetSupportsIt(t *testing.T) {
	for goos, want := range map[string]bool{
		"linux": true, "freebsd": true, "dragonfly": true,
		"darwin": false, "windows": false, "netbsd": false, "openbsd": false,
	} {
		if got := (Config{ReusePort: true}).resolve(breezeDefaults(), false, goos).ReusePort; got != want {
			t.Errorf("ReusePort on %s = %v, want %v", goos, got, want)
		}
		if (Config{}).resolve(breezeDefaults(), false, goos).ReusePort {
			t.Errorf("ReusePort is on by default on %s", goos)
		}
	}
}

func TestReuseAddrFollowsTheServerAndTheOperator(t *testing.T) {
	on, off := Defaults{ReuseAddr: true}, Defaults{ReuseAddr: false}
	if !(Config{}).resolve(on, false, "linux").ReuseAddr {
		t.Error("a TCP listener on Linux must get SO_REUSEADDR by default")
	}
	if (Config{}).resolve(off, false, "linux").ReuseAddr {
		t.Error("a listener that does not want SO_REUSEADDR (a Unix socket) was given it")
	}
	if (Config{DisableReuseAddr: true}).resolve(on, false, "linux").ReuseAddr {
		t.Error("DisableReuseAddr did not turn it off")
	}
}

func TestOverrideBeatsTheServersLoopCount(t *testing.T) {
	if got := (Config{EventLoops: 3}).resolve(breezeDefaults(), false, "linux").NumEventLoop; got != 3 {
		t.Errorf("NumEventLoop = %d, want the configured 3", got)
	}
	if got := (Config{}).resolve(Defaults{EventLoops: -2}, false, "linux").NumEventLoop; got != 0 {
		t.Errorf("NumEventLoop = %d, want a negative default read as 0", got)
	}
}
