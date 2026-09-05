package dashboard

import (
	"time"
)

// cpuUsage returns process CPU time (user, system).
//
// The per-platform reader is getCPUTime: a /proc/self/stat parse on Linux
// (cpu_linux.go) and a zero-returning stub elsewhere. The dashboard renders CPU
// as a relative percentage derived from delta-over-interval, so any monotonic
// source works and a platform without one shows a flat 0% rather than a wrong
// number.
func cpuUsage() (time.Duration, time.Duration) {
	user, sys := getCPUTime()
	if user > 0 || sys > 0 {
		return user, sys
	}
	// Final fallback: zero — CPU% will be 0 until a real source is available.
	return 0, 0
}
