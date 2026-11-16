package breeze

// listen_reuseaddr_test.go — a service can be restarted straight after it served a request.
//
// A server that closes a connection first holds it in TIME_WAIT for about a minute. Without
// SO_REUSEADDR the next bind of that port is refused, so a service restarted inside the minute
// could not listen. The generated main.go then dropped the error and the process exited with
// status 0, which looked like a service that never opened its port. This is the restart, done for
// real: a request the server closes, the server stopped, and the same port bound again at once.

import (
	"context"
	"io"
	"net"
	"runtime"
	"strconv"
	"testing"
	"time"
)

func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding a free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// serveOn runs a Breeze that answers GET / on the port, and returns once it accepts connections, or
// fails the test with the error RunOn gave if it never will.
func serveOn(t *testing.T, port int) (*Breeze, <-chan error) {
	t.Helper()
	router := NewRouter()
	router.Handle(GET, "/", func(ctx *Context) error { return ctx.JSON(map[string]string{"status": "ok"}) })
	app := New(router, NewEventLoopWorkerPool(1))
	errc := make(chan error, 1)
	go func() { errc <- app.RunOn("127.0.0.1", port, false) }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-errc:
			t.Fatalf("RunOn on port %d returned %v instead of serving", port, err)
		default:
		}
		if c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 100*time.Millisecond); err == nil {
			c.Close()
			return app, errc
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("nothing listened on port %d within 5s", port)
	return nil, nil
}

func TestRunOnRebindsAPortLeftInTimeWait(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("TIME_WAIT does not refuse a listener on Windows, and SO_REUSEADDR means something else there")
	}
	port := freeLoopbackPort(t)

	first, firstDone := serveOn(t, port)

	// A request the server closes: Connection: close makes the server the side that closes first,
	// which is the side that keeps the connection in TIME_WAIT.
	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if body, err := io.ReadAll(conn); err != nil || len(body) == 0 {
		t.Fatalf("reading the response: %d bytes, %v", len(body), err)
	}
	conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := first.Stop(ctx); err != nil {
		t.Fatalf("stopping the first server: %v", err)
	}
	<-firstDone

	// The premise: a socket of the first server is in TIME_WAIT on the port now, or the restart below
	// would pass without testing anything.
	if !hasTimeWaitOn(port) {
		t.Skipf("no connection of port %d is in TIME_WAIT on this system, so there is nothing for the restart to be refused by", port)
	}

	second, secondDone := serveOn(t, port)
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	_ = second.Stop(stopCtx)
	<-secondDone
}
