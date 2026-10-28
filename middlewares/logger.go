package middleware

import (
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nelthaarion/breeze/v2"
)

// LoggingOptions configures LoggingMiddlewareWith. The zero value is exactly
// LoggingMiddleware: one synchronous write per request to os.Stdout.
type LoggingOptions struct {
	// Output receives the log lines. Nil means os.Stdout, looked up on every write
	// so a replaced os.Stdout is honoured.
	Output io.Writer

	// Buffered batches the lines in memory and writes them from a background
	// goroutine instead of making one write system call per request on the event
	// loop. That is the single largest cost of request logging: at tens of
	// thousands of requests per second the synchronous form spends more time in
	// write(2) than the whole rest of the request.
	//
	// The price is timing and durability. A line reaches Output up to
	// FlushInterval after the request, lines from this middleware can interleave
	// differently with output the application prints itself, and lines still
	// buffered when the process dies abruptly (SIGKILL, a crash, os.Exit) are lost.
	// Call FlushLogs from the shutdown path to write what is pending.
	Buffered bool

	// FlushInterval is how often buffered lines are written. Default 10ms.
	FlushInterval time.Duration

	// BufferSize is the amount of pending output that triggers an early flush.
	// Default 64 KiB. If the writer cannot keep up and pending output reaches 16
	// times this, the request that crosses the limit writes synchronously, so
	// memory stays bounded and no line is dropped.
	BufferSize int
}

// LoggingMiddleware prints one line per request,
//
//	[Breeze][2026-10-28T09:30:00Z] GET /path -> 200 (1.5ms)
//
// after the rest of the chain has run. See LoggingOptions and LoggingMiddlewareWith
// to batch the writes.
func LoggingMiddleware() breeze.HandlerFunc { return LoggingMiddlewareWith(LoggingOptions{}) }

// LoggingMiddlewareWith is LoggingMiddleware with options.
func LoggingMiddlewareWith(opts LoggingOptions) breeze.HandlerFunc {
	loggingInstalled.Store(true)
	var sink *asyncLog
	if opts.Buffered {
		interval, size := opts.FlushInterval, opts.BufferSize
		if interval <= 0 {
			interval = 10 * time.Millisecond
		}
		if size <= 0 {
			size = 64 << 10
		}
		sink = newAsyncLog(opts.Output, interval, size)
	}
	return func(ctx *breeze.Context) error {
		start := time.Now()
		method := ctx.Req.Method
		path := ctx.Req.Path

		// The chain's error is held rather than returned immediately: this
		// middleware's whole purpose is the line it prints afterwards, and returning
		// early would skip it for exactly the requests worth logging.
		err := ctx.Next()

		status := 0
		if ctx.Res != nil {
			status = ctx.Res.Status
		}

		ll := logLinePool.Get().(*logLine)
		line := appendLogLine(ll.b[:0], start, method, path, status, time.Since(start))
		switch {
		case sink != nil:
			sink.write(line)
		case opts.Output != nil:
			_, _ = opts.Output.Write(line)
		default:
			_, _ = os.Stdout.Write(line)
		}
		ll.b = line[:0]
		logLinePool.Put(ll)

		// Counted after the line is printed, so the count is of lines actually
		// emitted rather than of requests that reached this middleware.
		if err != nil {
			loggingCounter.Error()
		}
		loggingCounter.Hit()

		return err
	}
}

type logLine struct{ b []byte }

var logLinePool = sync.Pool{New: func() any { return &logLine{b: make([]byte, 0, 160)} }}

// appendLogLine appends the log line for one request. The bytes are those of
//
//	fmt.Sprintf("[Breeze][%s] %s %s -> %d (%v)\n", start.Format(time.RFC3339), method, path, status, d)
//
// without fmt's boxing and its intermediate strings.
func appendLogLine(b []byte, start time.Time, method breeze.Method, path string, status int, d time.Duration) []byte {
	b = append(b, "[Breeze]["...)
	b = appendRFC3339(b, start)
	b = append(b, "] "...)
	b = append(b, string(method)...)
	b = append(b, ' ')
	b = append(b, path...)
	b = append(b, " -> "...)
	b = appendInt(b, status)
	b = append(b, " ("...)
	b = append(b, d.String()...) // does not escape, so the string lives on the stack
	b = append(b, ")\n"...)
	return b
}

func appendInt(b []byte, n int) []byte {
	if n >= 0 && n < 1000 {
		if n >= 100 {
			b = append(b, byte('0'+n/100))
		}
		if n >= 10 {
			b = append(b, byte('0'+n/10%10))
		}
		return append(b, byte('0'+n%10))
	}
	return appendBigInt(b, n)
}

// tsEntry is one second's formatted timestamp. Every request in the same second
// shares it, so the time formatting runs once per second instead of per request.
type tsEntry struct {
	sec  int64
	loc  *time.Location
	text string
}

var lastTS atomic.Pointer[tsEntry]

func appendRFC3339(b []byte, t time.Time) []byte {
	sec, loc := t.Unix(), t.Location()
	if e := lastTS.Load(); e != nil && e.sec == sec && e.loc == loc {
		return append(b, e.text...)
	}
	text := t.Format(time.RFC3339)
	lastTS.Store(&tsEntry{sec: sec, loc: loc, text: text})
	return append(b, text...)
}

// asyncLog is the buffered sink behind LoggingOptions.Buffered: producers append
// to one buffer under a short mutex, a background goroutine swaps it out and
// writes it.
type asyncLog struct {
	out    io.Writer // nil means os.Stdout at write time
	size   int
	hard   int
	mu     sync.Mutex // guards buf
	buf    []byte
	flushM sync.Mutex // serialises flushes; guards spare and writes to out
	spare  []byte
	kick   chan struct{}
	stop   chan struct{}
	done   chan struct{}
	once   sync.Once
}

var asyncLogs struct {
	sync.Mutex
	list []*asyncLog
}

func newAsyncLog(out io.Writer, interval time.Duration, size int) *asyncLog {
	l := &asyncLog{
		out: out, size: size, hard: 16 * size,
		buf: make([]byte, 0, 2*size), spare: make([]byte, 0, 2*size),
		kick: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}),
	}
	asyncLogs.Lock()
	asyncLogs.list = append(asyncLogs.list, l)
	asyncLogs.Unlock()
	go l.run(interval)
	return l
}

func (l *asyncLog) run(interval time.Duration) {
	defer close(l.done)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-l.kick:
		case <-l.stop:
			l.flush()
			return
		}
		l.flush()
	}
}

func (l *asyncLog) write(line []byte) {
	l.mu.Lock()
	l.buf = append(l.buf, line...)
	n := len(l.buf)
	l.mu.Unlock()
	switch {
	case n >= l.hard:
		l.flush() // the writer is behind: apply backpressure instead of growing
	case n >= l.size:
		select {
		case l.kick <- struct{}{}:
		default:
		}
	}
}

func (l *asyncLog) flush() {
	l.flushM.Lock()
	defer l.flushM.Unlock()
	l.mu.Lock()
	b := l.buf
	l.buf = l.spare[:0]
	l.mu.Unlock()
	if len(b) > 0 {
		w := l.out
		if w == nil {
			w = os.Stdout
		}
		_, _ = w.Write(b)
	}
	if cap(b) > 1<<20 {
		b = nil // a burst grew it; do not keep the memory
	}
	l.spare = b[:0]
}

func (l *asyncLog) close() {
	l.once.Do(func() { close(l.stop) })
	<-l.done
}

// FlushLogs writes every line that LoggingMiddlewareWith(LoggingOptions{Buffered:
// true}) is still holding. Call it when the server shuts down. It is a no-op when
// no buffered logger exists.
func FlushLogs() {
	asyncLogs.Lock()
	list := append([]*asyncLog(nil), asyncLogs.list...)
	asyncLogs.Unlock()
	for _, l := range list {
		l.flush()
	}
}
