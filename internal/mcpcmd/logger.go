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
	"strconv"
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
// greps it, and a line that wraps is a line that greps badly.
//
// # What is escaped, and why
//
// Not everything in an event is a constant. The tool name of an *unknown* tool, and the
// argument names of any call, are whatever the caller sent; the peer address is the
// transport's. A caller holding the token can put a newline or a terminal escape sequence
// in a tool name and, written verbatim, forge whole lines in a log an operator reads as
// evidence ("refused 401 ... from 10.0.0.1"), or repaint the terminal tailing it. Those
// fields go through text(), which leaves an ordinary name untouched and quotes anything
// else, so one event is always exactly one line and the line says what the caller sent.
func (l *stderrLogger) LogEvent(ev mcp.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()

	switch ev.Kind {
	case mcp.EventToolCall:
		fmt.Fprintf(l.out, "%s: tool %s %s %s%s\n",
			l.name, text(ev.Tool), ev.Outcome, duration(ev.Duration), args(ev.ArgNames))
	case mcp.EventToolPanic:
		// Named as a panic rather than an error, because the two want different
		// responses: an error is usually the project's, a panic is this server's.
		fmt.Fprintf(l.out, "%s: tool %s PANICKED %s%s\n",
			l.name, text(ev.Tool), duration(ev.Duration), args(ev.ArgNames))
	case mcp.EventToolUnknown:
		fmt.Fprintf(l.out, "%s: tool %s unknown\n", l.name, text(ev.Tool))
	case mcp.EventToolRefused:
		fmt.Fprintf(l.out, "%s: tool %s refused (needs %s capability)\n", l.name, text(ev.Tool), text(ev.Reason))
	case mcp.EventHandshake:
		fmt.Fprintf(l.out, "%s: session initialized\n", l.name)
	case mcp.EventTransportRefusal:
		fmt.Fprintf(l.out, "%s: refused %d %s%s\n", l.name, ev.Status, text(ev.Reason), from(ev.Remote))
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

	// A caller chooses how many argument names it sends, so the line is bounded: a log
	// line a megabyte long is a way to bury everything around it.
	const maxNames = 32
	shown := names
	if len(shown) > maxNames {
		shown = shown[:maxNames]
	}
	quoted := make([]string, len(shown))
	for i, name := range shown {
		quoted[i] = text(name)
	}
	out := " args=" + strings.Join(quoted, ",")
	if extra := len(names) - len(shown); extra > 0 {
		out += ",+" + strconv.Itoa(extra) + "_more"
	}
	return out
}

// maxLogField bounds one caller-controlled field. Real tool and argument names are a few
// dozen characters; this is generous to those and hostile to a name built to flood a line.
const maxLogField = 80

// text renders a field that may have come from a caller so that it cannot break the line.
//
// A value made only of characters that appear in real tool names, argument names and peer
// addresses is returned as it is — the common case costs one scan and changes nothing an
// operator greps for. Anything else, including a control character, a space, a non-UTF-8
// byte or an over-long value, comes back as a quoted ASCII string with every unusual byte
// written as an escape, so it is visibly "what was sent" and not a forged field.
func text(s string) string {
	if len(s) <= maxLogField && isPlain(s) {
		return s
	}
	truncated := len(s) > maxLogField
	if truncated {
		s = s[:maxLogField]
	}
	quoted := strconv.QuoteToASCII(s)
	if truncated {
		quoted = quoted[:len(quoted)-1] + `..."`
	}
	return quoted
}

// isPlain reports whether s consists only of characters found in tool names, argument
// names and network addresses.
func isPlain(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_', c == '-', c == '.', c == ':', c == '/', c == '@', c == '[', c == ']', c == '+':
		default:
			return false
		}
	}
	return true
}

// from renders a peer address when one is known.
func from(remote string) string {
	if remote == "" {
		return ""
	}
	return " from " + text(remote)
}
