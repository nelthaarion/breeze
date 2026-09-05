package diag

// format.go — the rendering helpers a probe's Summary line needs.
//
// # Why these live here
//
// A Report's Summary is prose read by a human or an agent, so the framework has
// to render the same quantity the same way everywhere: a dashboard that shows
// "1.5 MB" on one row and "1.5 MiB" on the next has told the reader that the two
// numbers were measured differently, which is a claim nobody made.
//
// Three packages had independently grown a private humanBytes: video, middlewares
// and internal/mcp. Each carried a comment explaining that it could not be shared
// because the alternative was an import the package must not take — and each was
// right about that, and wrong about the conclusion. This package is the leaf that
// imports nothing but the standard library precisely so that every layer can use
// it, and all three already depend on it to publish their reports at all.
//
// The three had also diverged: two printed KB/MB/GB, one printed KiB/MiB/GiB, and
// all three divided by 1024. So one of the two labels was simply wrong, and they
// all fed the same diagnostics page.

import (
	"fmt"
	"time"
)

// Milliseconds renders a duration as fractional milliseconds.
//
// Every latency number the framework publishes goes through here, so that a
// dashboard row, a diag Report detail and an MCP tool result showing the same
// duration show the same digits.
//
// The truncation is deliberate and is the reason this is one function rather than
// a one-line expression at each site. float64(d)/float64(time.Millisecond) keeps
// nanosecond resolution, so a 1.5 µs step renders as 0.0015 ms — six decimal
// places of noise in a field a reader compares by eye. Dividing microseconds by
// 1000 truncates first, so sub-microsecond differences disappear and the value
// has at most three decimals, which is the precision these measurements actually
// carry: they come from a single time.Since around work that includes scheduling.
//
// Two conventions were in the tree before this: events/diag.go truncated to
// microseconds, workflow/engine.go did not. Both fed the same dashboard.
func Milliseconds(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000.0
}

// HumanBytes renders a byte count for a Report summary.
//
// Binary units (KiB, MiB, …) because the divisor is 1024. That is not a style
// preference: "MB" against a 1024-based divisor is a factor-of-1.049 error per
// step, so a 1 GiB heap reported as "1.1 GB" is off by 74 MB at the third step
// and the reader has no way to see it. If a caller ever needs decimal units it
// needs a different function, not a different suffix table here.
//
// One decimal place. Two implies a precision that byte counters sampled at 1 Hz
// do not have, and zero loses the difference between 1.2 and 1.9 GiB, which is
// the range where an operator starts caring.
func HumanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	// Past TiB the loop has divided four times and value is still >= 1024, so
	// one more division lands in PiB. Nothing in this framework reports a
	// number that large, but a fallthrough returning "" would be worse than an
	// unlikely branch.
	return fmt.Sprintf("%.1f PiB", value/unit)
}
