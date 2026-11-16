//go:build linux

package client

import (
	"fmt"
	"net"
	"syscall"
	"testing"
	"time"
)

// blackholeAddr returns an address whose connect() hangs: a listener with backlog 0 whose accept queue
// is already full, so further SYNs are dropped and the connecting side retries them for about two
// minutes, which is what a firewall that drops packets does to a client.
func blackholeAddr(t *testing.T) string {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Listen(fd, 0); err != nil {
		t.Fatal(err)
	}
	name, _ := syscall.Getsockname(fd)
	addr := fmt.Sprintf("127.0.0.1:%d", name.(*syscall.SockaddrInet4).Port)

	var held []net.Conn
	for i := 0; i < 6; i++ {
		c, err := net.DialTimeout("tcp4", addr, 300*time.Millisecond)
		if err != nil {
			break
		}
		held = append(held, c)
	}
	t.Cleanup(func() {
		for _, c := range held {
			c.Close()
		}
		syscall.Close(fd)
	})
	// The premise: the next connect really does hang, or the address is no blackhole on this kernel.
	if c, err := net.DialTimeout("tcp4", addr, 500*time.Millisecond); err == nil {
		c.Close()
		t.Skip("the accept queue does not fill on this kernel, so there is no address that drops connects")
	}
	return addr
}

// TestDialToAHostThatDropsPacketsIsBoundedByDialTimeout: Config.DialTimeout is documented as bounding
// the connection, and for plain TCP it did not. A call to such a host returned after two minutes and
// fifteen seconds with Timeout at two seconds, so one unreachable service held every caller that used
// the client (the fleet aggregator's schema fetch and log fan-out, the tracer's exporter) for that long.
func TestDialToAHostThatDropsPacketsIsBoundedByDialTimeout(t *testing.T) {
	addr := blackholeAddr(t)
	c := New(Config{Timeout: 5 * time.Second, DialTimeout: 400 * time.Millisecond})

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := c.Get("http://" + addr + "/x")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a call to an address that never answers succeeded")
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Errorf("the call took %v with DialTimeout at 400ms", d)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("the call had not returned after 8s with DialTimeout at 400ms: the dial is not bounded")
	}
}
