package mcpcmd

// logger.go — the stderr log line for a running MCP server.
//
// # Why anything at all
//
// Before this, a server printed its banner and then went silent. An operator asking "did
// the agent call anything", "is a tool failing", or "is something trying my control port"
// had no answer short of a packet capture. For an endpoint that can write files and start
// containers, an unauthenticated request leaving no trace is the gap that matters.
//
// # Why it is off by default
//
// A stdio server's stdout is the protocol stream, and its stderr is whatever the editor
// that launched it does with stderr — often a panel nobody opens, sometimes a file. Both
// are somebody else's channel. Logging is therefore opt-in with --log, so a working
// editor integration does not start emitting lines it never asked for.
//
// # Why stderr on both transports
//
// On stdio it is mandatory: one human-readable line on stdout is one malformed MCP message
// to the peer. On the network transport it is merely consistent, and consistency is worth
// having — the banner is already there, and an operator redirecting one stream gets both.

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/nelthaarion/breeze/v2/internal/mcp"
)

// stderrLogger writes one line per event.
//
// # The mutex is not optional
//
// The network transport serves each request on its own net/http goroutine, so two tools
// can complete at the same instant. io.Writer makes no concurrency guarantee, and two
// interleaved Fprintf calls produce one corrupted line — which for a log being scraped is
// worse than no line, because it looks like data.
type stderrLogger struct {
	mu  sync.Mutex
	out io.Writer

	// name prefixes each line with the command, matching the banner, so a log
	// containing several processes' output stays attributable.
	name string
}

// LogEvent renders one event.
//
// # The format
//
//	breeze-mcp: tool breeze_verify_project ok 4.21s args=path,run_tests
//	breeze-mcp: tool breeze_get_logs error 91ms args=service_url,token
//	breeze-mcp: refused 401 missing or invalid bearer token from 10.1.2.3:53551
//
// Deliberately one line, space-separated, key=value where a key is useful: an operator
// greps it, and a line that wraps is a line that greps badly. Nothing is escaped because
// nothing here needs escaping — every component is a constant from internal/mcp, a
// registered tool name, a number, or a peer address.
func (l *stderrLogger) LogEvent(ev mcp.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()

	switch ev.Kind {
	case mcp.EventToolCall:
		fmt.Fprintf(l.out, "%s: tool %s %s %s%s\n",
			l.name, ev.Tool, ev.Outcome, duration(ev.Duration), args(ev.ArgNames))
	case mcp.EventToolPanic:
		// Named as a panic rather than an error, because the two want different
		// responses: an error is usually the project's, a panic is this server's.
		fmt.Fprintf(l.out, "%s: tool %s PANICKED %s%s\n",
			l.name, ev.Tool, duration(ev.Duration), args(ev.ArgNames))
	case mcp.EventToolUnknown:
		fmt.Fprintf(l.out, "%s: tool %s unknown\n", l.name, ev.Tool)
	case mcp.EventToolRefused:
		fmt.Fprintf(
			l.out,
			"%s: tool %s refused (needs %s capability)\n",
			l.name,
			ev.Tool,
			ev.Reason,
		)
	case mcp.EventHandshake:
		fmt.Fprintf(l.out, "%s: session initialized\n", l.name)
	case mcp.EventTransportRefusal:
		fmt.Fprintf(l.out, "%s: refused %d %s%s\n", l.name, ev.Status, ev.Reason, from(ev.Remote))
	}
}

// duration renders a duration at a resolution a human reads.
//
// time.Duration's own String gives "4.213871ms", where the digits past the second are
// noise in a log line. Two significant figures is what an operator scanning for a slow
// tool actually uses.
func duration(d time.Duration) string {
	switch {
	case d >= time.Second:
		return fmt.Sprintf("%.2fs", d.Seconds())
	case d >= time.Millisecond:
		return fmt.Sprintf("%dms", d.Round(time.Millisecond)/time.Millisecond)
	default:
		return "<1ms"
	}
}

// args renders the argument names, or nothing when a call had none.
//
// Names only — see internal/mcp/observe.go, argumentNames. There is no verbosity level at
// which this prints a value: several tools take a `token` or a `password`, and a log file
// outlives the process that wrote it.
func args(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return " args=" + strings.Join(names, ",")
}

// from renders a peer address when one is known.
func from(remote string) string {
	if remote == "" {
		return ""
	}
	return " from " + remote
}
