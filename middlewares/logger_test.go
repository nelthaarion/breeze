package middleware

import (
	"bytes"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nelthaarion/breeze/v2"
)

func TestAppendLogLineMatchesFmt(t *testing.T) {
	locs := []*time.Location{time.UTC, time.FixedZone("t", 3*3600+1800), time.FixedZone("n", -5*3600), time.Local}
	rng := rand.New(rand.NewSource(3))
	const pathChars = "/abcXYZ019-_.~%?=&:*"
	for i := 0; i < 30000; i++ {
		start := time.Unix(rng.Int63n(4e9), rng.Int63n(1e9)).In(locs[rng.Intn(len(locs))])
		var d time.Duration
		switch rng.Intn(6) {
		case 0:
			d = time.Duration(rng.Int63n(1000))
		case 1:
			d = time.Duration(rng.Int63n(1e6))
		case 2:
			d = time.Duration(rng.Int63n(1e9))
		case 3:
			d = time.Duration(rng.Int63n(1e12))
		case 4:
			d = time.Duration(rng.Int63())
		default:
			d = 0
		}
		pb := make([]byte, rng.Intn(30))
		for j := range pb {
			pb[j] = pathChars[rng.Intn(len(pathChars))]
		}
		method := []breeze.Method{"GET", "POST", "DELETE", "PATCH"}[rng.Intn(4)]
		status := []int{0, 7, 99, 200, 204, 404, 500, 999, 1000, 12345}[rng.Intn(10)]
		want := fmt.Sprintf("[Breeze][%s] %s %s -> %d (%v)\n", start.Format(time.RFC3339), method, string(pb), status, d)
		if got := string(appendLogLine(nil, start, method, string(pb), status, d)); got != want {
			t.Fatalf("got  %q\nwant %q", got, want)
		}
	}
}

func TestAppendLogLineNoAllocs(t *testing.T) {
	start := time.Now()
	buf := make([]byte, 0, 256)
	appendLogLine(buf, start, "GET", "/x", 200, time.Millisecond) // warm the timestamp cache
	if n := testing.AllocsPerRun(1000, func() {
		buf = appendLogLine(buf[:0], start, "GET", "/api/v1/users/42", 200, 1234*time.Microsecond)
	}); n != 0 {
		t.Fatalf("appendLogLine allocates %v times per call", n)
	}
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func TestAsyncLogKeepsEveryLineIntact(t *testing.T) {
	out := &lockedBuf{}
	l := newAsyncLog(out, 5*time.Millisecond, 256) // tiny buffer: exercises early flush and backpressure
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				l.write([]byte(fmt.Sprintf("line g=%d i=%04d padding-padding-padding\n", g, i)))
			}
		}(g)
	}
	wg.Wait()
	l.close()
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 16000 {
		t.Fatalf("got %d lines, want 16000", len(lines))
	}
	seen := map[string]bool{}
	for _, ln := range lines {
		if !strings.HasPrefix(ln, "line g=") || !strings.HasSuffix(ln, " padding-padding-padding") {
			t.Fatalf("corrupt line %q", ln)
		}
		seen[ln] = true
	}
	if len(seen) != 16000 {
		t.Fatalf("duplicate or missing lines: %d unique", len(seen))
	}
}

func TestLoggingMiddlewareOutputOption(t *testing.T) {
	out := &lockedBuf{}
	for _, opts := range []LoggingOptions{{Output: out}, {Output: out, Buffered: true, FlushInterval: time.Millisecond}} {
		out.b.Reset()
		mw := LoggingMiddlewareWith(opts)
		ctx := breeze.NewContext(breeze.GET, "/hello")
		ctx.SetMiddlewareChain([]breeze.HandlerFunc{mw}, func(c *breeze.Context) error { return c.WriteString("hi") })
		if err := ctx.Next(); err != nil {
			t.Fatal(err)
		}
		FlushLogs()
		got := out.String()
		if !strings.HasPrefix(got, "[Breeze][") || !strings.HasSuffix(got, "] GET /hello -> 200 ("+strings.SplitN(strings.SplitN(got, "(", 2)[1], ")", 2)[0]+")\n") {
			t.Fatalf("buffered=%v: unexpected line %q", opts.Buffered, got)
		}
	}
}
