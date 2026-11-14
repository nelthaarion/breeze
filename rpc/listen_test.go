package rpc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nelthaarion/breeze/v2/diag"
	"github.com/nelthaarion/gnetplus/v2"
)

// The JSON-RPC listener on a host with no IPv6. The generated block runs RunOn with an empty host, which gnet
// binds as the IPv6 wildcard, and a kernel without IPv6 refuses the socket: the listener never existed, the HTTP
// server (which binds tcp4) served normally, and nothing but the probe said the port was dead. An empty host now
// means the dual-stack wildcard where there is one and IPv4 on every interface where there is not.

// stubGnet replaces gnetRun with a function that records the addresses it is asked to bind and fails the first
// with failFirst. The second call blocks until release is closed (nil means it returns at once), which lets a test
// look at the probe while the listener "is up". It restores gnetRun when the test ends, so none of these run in
// parallel.
func stubGnet(t *testing.T, failFirst error, release <-chan struct{}) (addresses func() []string) {
	t.Helper()
	var mu sync.Mutex
	var calls []string
	orig := gnetRun
	t.Cleanup(func() { gnetRun = orig })
	gnetRun = func(_ gnetplus.EventHandler, addr string, _ ...gnetplus.Option) error {
		mu.Lock()
		calls = append(calls, addr)
		n := len(calls)
		mu.Unlock()
		if n == 1 {
			return failFirst
		}
		if release != nil {
			<-release
		}
		return nil
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), calls...)
	}
}

func newListenServer() *Server {
	reg := NewRegistry()
	reg.Register("sum", func(*Context) {})
	return NewServer(reg)
}

func TestAnEmptyHostFallsBackToIPv4WhenTheHostHasNoIPv6(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"the raw socket error", &os.SyscallError{Syscall: "socket", Err: syscall.EAFNOSUPPORT}},
		{"wrapped so errors.Is can see it", fmt.Errorf("gnet: listen: %w", syscall.EAFNOSUPPORT)},
		// What gnet returned on a host without IPv6, as text: the type was gone by the time it reached us.
		{"flattened to text", errors.New("socket: address family not supported by protocol")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			addresses := stubGnet(t, tc.err, release)
			s := newListenServer()

			done := make(chan error, 1)
			go func() { done <- s.RunOn("", 9123, false) }()

			deadline := time.Now().Add(5 * time.Second)
			for len(addresses()) < 2 && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			if got := addresses(); len(got) != 2 || got[0] != "tcp://:9123" || got[1] != "tcp4://0.0.0.0:9123" {
				t.Fatalf("bound %v, want the wildcard and then IPv4 on every interface", got)
			}

			// While the fallback listener is up the probe says so, and says it is IPv4 only.
			report := s.probe()
			if report.Status == diag.StatusDegraded && strings.Contains(report.Summary, "not running") {
				t.Fatalf("the probe reports the fallback listener as not running: %s", report.Summary)
			}
			if report.Detail["ipv4_fallback"] != true || report.Detail["listen_address"] != "tcp4://0.0.0.0:9123" {
				t.Errorf("detail = %v, want the fallback and the address recorded", report.Detail)
			}
			if !strings.Contains(strings.Join(report.Notes, " "), "no IPv6") {
				t.Errorf("the notes do not say the listener is IPv4 only: %v", report.Notes)
			}

			close(release)
			if err := <-done; err != nil {
				t.Errorf("RunOn = %v after the fallback listener shut down, want nil", err)
			}
		})
	}
}

// Dual-stack is what an empty host has always meant, and a deployment that relies on it must not be narrowed.
func TestAnEmptyHostIsNotNarrowedWhereTheWildcardBinds(t *testing.T) {
	addresses := stubGnet(t, nil, nil)
	s := newListenServer()

	if err := s.RunOn("", 9124, false); err != nil {
		t.Fatal(err)
	}
	if got := addresses(); len(got) != 1 || got[0] != "tcp://:9124" {
		t.Errorf("bound %v, want only the wildcard", got)
	}
	if s.fellBack.Load() {
		t.Error("the server says it fell back when it did not")
	}
}

func TestAHostThatWasNamedIsBoundAsGivenAndNeverRetried(t *testing.T) {
	unsupported := &os.SyscallError{Syscall: "socket", Err: syscall.EAFNOSUPPORT}
	addresses := stubGnet(t, unsupported, nil)
	s := newListenServer()

	err := s.RunOn("::1", 9125, false)
	if !errors.Is(err, syscall.EAFNOSUPPORT) {
		t.Fatalf("RunOn(\"::1\") = %v, want the error returned as it came", err)
	}
	if got := addresses(); len(got) != 1 || got[0] != "tcp://[::1]:9125" {
		t.Errorf("bound %v: a caller who names a host means it", got)
	}
}

func TestOnlyAMissingAddressFamilyIsRetried(t *testing.T) {
	for _, reason := range []string{"bind: address already in use", "listen tcp :9126: permission denied"} {
		addresses := stubGnet(t, errors.New(reason), nil)
		s := newListenServer()

		if err := s.RunOn("", 9126, false); err == nil || err.Error() != reason {
			t.Errorf("RunOn = %v, want %q returned as it came", err, reason)
		}
		if got := addresses(); len(got) != 1 {
			t.Errorf("%q was retried: %v", reason, got)
		}
	}
}

// The real thing, with no stub: an empty host must end up listening and answering on IPv4 wherever the test runs. On a
// host without IPv6 (the CI containers this was found on) it fails without the fallback; on a dual-stack host it
// passes either way, and the stub tests above are what pin the logic there.
func TestAnEmptyHostListensOnIPv4Here(t *testing.T) {
	probe, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()

	s := newListenServer()
	done := make(chan error, 1)
	go func() { done <- s.Run(port, false) }()

	var conn net.Conn
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if conn, err = net.DialTimeout("tcp4", "127.0.0.1:"+strconv.Itoa(port), 200*time.Millisecond); err == nil {
			break
		}
		select {
		case runErr := <-done:
			t.Fatalf("the listener ended before it accepted a connection: %v\nprobe: %+v", runErr, s.probe())
		default:
		}
		time.Sleep(25 * time.Millisecond)
	}
	if conn == nil {
		t.Fatalf("nothing is listening on IPv4 port %d: %v", port, err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"sum"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 256)
	if n, err := conn.Read(buf); err != nil || n == 0 {
		t.Errorf("the listener accepted a connection and answered nothing: %d bytes, %v", n, err)
	}

	addr := *s.listenAddr.Load()
	_ = gnetplus.Stop(context.Background(), addr)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("the listener did not stop")
	}
}
