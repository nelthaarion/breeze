package breeze

import (
	"context"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/nelthaarion/breeze/v2/gnetcfg"
	"github.com/nelthaarion/gnetplus/v2"
)

// captureGnet replaces gnetRun with a stub that records what Run hands to gnet and returns at once,
// so a test can read the engine settings without opening a socket. It applies the options the way
// gnet does, in order, and also reports how many there were. gnetRun is restored when the test
// ends, so tests that use this do not run in parallel.
func captureGnet(t *testing.T) func() (addr string, opts gnetplus.Options, n int) {
	t.Helper()
	var (
		mu   sync.Mutex
		addr string
		got  gnetplus.Options
		n    int
	)
	orig := gnetRun
	t.Cleanup(func() { gnetRun = orig })
	gnetRun = func(_ gnetplus.EventHandler, a string, opts ...gnetplus.Option) error {
		var o gnetplus.Options
		for _, f := range opts {
			f(&o)
		}
		mu.Lock()
		addr, got, n = a, o, len(opts)
		mu.Unlock()
		return nil
	}
	return func() (string, gnetplus.Options, int) {
		mu.Lock()
		defer mu.Unlock()
		return addr, got, n
	}
}

func engineTestApp() *Breeze {
	return New(NewRouter(), NewEventLoopWorkerPool(2))
}

// wantReuseAddr is what Run asks for on this machine: SO_REUSEADDR everywhere but Windows.
func wantReuseAddr() bool { return runtime.GOOS != "windows" }

func TestRunHandsGnetTheHistoricalEngineConfigByDefault(t *testing.T) {
	seen := captureGnet(t)
	app := engineTestApp()
	if err := app.Run(8080, false); err != nil {
		t.Fatal(err)
	}
	addr, got, n := seen()

	if n != 1 {
		t.Errorf("Run handed gnet %d options, want exactly one (WithOptions replaces the whole set, so "+
			"an option listed before it is thrown away)", n)
	}
	if addr != "tcp://:8080" {
		t.Errorf("address = %q, want tcp://:8080", addr)
	}
	want := gnetplus.Options{
		NumEventLoop:   app.Pool.Metrics().Workers,
		LB:             gnetplus.RoundRobin,
		ReadBufferCap:  64 << 10,
		WriteBufferCap: 64 << 10,
		TCPKeepAlive:   5 * time.Second,
		TCPNoDelay:     gnetplus.TCPNoDelay,
		ReuseAddr:      wantReuseAddr(),
		Ticker:         true,
	}
	if got != want {
		t.Fatalf("Run handed gnet\n  %+v\nwant\n  %+v", got, want)
	}
	if !got.Ticker {
		t.Error("the ticker is off: OnTick never runs, so neither the idle nor the read-header timeout does either")
	}
}

func TestRunHandsGnetTheConfiguredEngine(t *testing.T) {
	seen := captureGnet(t)
	app := engineTestApp()
	app.SetEngineConfig(gnetcfg.Config{
		EventLoops:        3,
		LoadBalancing:     gnetplus.SourceAddrHash,
		ReadBufferCap:     128 << 10,
		WriteBufferCap:    32 << 10,
		TCPKeepAlive:      45 * time.Second,
		TickInterval:      250 * time.Millisecond,
		DisableTCPNoDelay: true,
		DisableReuseAddr:  true,
		AlwaysPollYield:   true,
	})
	if err := app.RunOn("127.0.0.1", 9090, true); err != nil {
		t.Fatal(err)
	}
	addr, got, n := seen()

	if n != 1 || addr != "tcp://127.0.0.1:9090" {
		t.Errorf("n = %d, addr = %q", n, addr)
	}
	want := gnetplus.Options{
		NumEventLoop:    3,
		Multicore:       true,
		LB:              gnetplus.SourceAddrHash,
		ReadBufferCap:   128 << 10,
		WriteBufferCap:  32 << 10,
		TCPKeepAlive:    45 * time.Second,
		TCPNoDelay:      gnetplus.TCPDelay,
		ReuseAddr:       false,
		Ticker:          true,
		AlwaysPollYield: true,
	}
	if got != want {
		t.Fatalf("Run handed gnet\n  %+v\nwant\n  %+v", got, want)
	}
	if d := app.tickInterval(); d != 250*time.Millisecond {
		t.Errorf("tick interval = %v, want 250ms", d)
	}
}

// The poll-yield policy is stored by SetPollYield and has to reach gnet: Run used to fix it at
// false, so PollYieldAuto and PollYieldNever were accepted and did nothing.
func TestRunHandsGnetThePollYieldPolicy(t *testing.T) {
	prev := runtime.GOMAXPROCS(2) // PollYieldAuto keeps the yield on a single P
	t.Cleanup(func() { runtime.GOMAXPROCS(prev) })

	blocking := NewRouter()
	blocking.HandleBlocking(GET, "/b", trieNoop)

	for _, tc := range []struct {
		name string
		mode PollYield
		app  func() *Breeze
		want bool
	}{
		{"default keeps the yield", PollYieldDefault, engineTestApp, false},
		{"never removes it", PollYieldNever, engineTestApp, true},
		{"auto removes it for an all-inline server", PollYieldAuto, engineTestApp, true},
		{"auto keeps it when a route is blocking", PollYieldAuto, func() *Breeze { return New(blocking, NewEventLoopWorkerPool(2)) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seen := captureGnet(t)
			app := tc.app()
			app.SetPollYield(tc.mode)
			if err := app.Run(8081, false); err != nil {
				t.Fatal(err)
			}
			_, got, _ := seen()
			if got.NoPollYield != tc.want {
				t.Errorf("Options.NoPollYield = %v, want %v", got.NoPollYield, tc.want)
			}
			if app.yieldOnDispatch.Load() != tc.want {
				t.Errorf("yieldOnDispatch = %v, want %v: a loop that skips gnet's yield must make up for it "+
					"after handing a request to a worker", app.yieldOnDispatch.Load(), tc.want)
			}
		})
	}
}

func TestEngineConfigIsNormalizedAndReported(t *testing.T) {
	app := engineTestApp()
	if got := app.EngineConfig(); got != (gnetcfg.Config{}) {
		t.Fatalf("a server that was never configured reports %+v, want the zero Config", got)
	}
	app.SetEngineConfig(gnetcfg.Config{EventLoops: -4, ReadBufferCap: -1, TickInterval: time.Nanosecond})
	got := app.EngineConfig()
	if got.EventLoops != 0 || got.ReadBufferCap != 0 || got.TickInterval != gnetcfg.MinTickInterval {
		t.Errorf("EngineConfig() = %+v, want the values brought into range", got)
	}
}

func TestOnTickAsksForTheConfiguredPeriod(t *testing.T) {
	app := engineTestApp()
	if d, _ := app.OnTick(); d != time.Second {
		t.Errorf("OnTick before Run asked for %v, want the default second", d)
	}

	captureGnet(t)
	app.SetEngineConfig(gnetcfg.Config{TickInterval: 100 * time.Millisecond})
	if err := app.Run(8082, false); err != nil {
		t.Fatal(err)
	}
	if d, _ := app.OnTick(); d != 100*time.Millisecond {
		t.Errorf("OnTick asked for %v, want the configured 100ms", d)
	}
}

// A server with settings that differ from the defaults in every way still has to listen, answer
// and stop, including SO_REUSEPORT with two loops where the platform has it (Linux) and being
// ignored where it does not.
func TestServerServesWithANonDefaultEngineConfig(t *testing.T) {
	app := auditApp()
	app.SetEngineConfig(gnetcfg.Config{
		EventLoops:      2,
		LoadBalancing:   gnetplus.LeastConnections,
		ReadBufferCap:   16 << 10,
		WriteBufferCap:  16 << 10,
		TCPKeepAlive:    30 * time.Second,
		TickInterval:    50 * time.Millisecond,
		ReusePort:       true,
		AlwaysPollYield: true,
	})
	port := startAuditServer(t, app)

	for i := 0; i < 20; i++ {
		resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/fast")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if string(body) != "fast" {
			t.Fatalf("request %d: body = %q, want fast", i, body)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := app.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}
