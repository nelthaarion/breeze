//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package dashboard

import (
	"syscall"
	"time"
)

// getCPUTime returns the process's user and system CPU time from getrusage(2).
func getCPUTime() (time.Duration, time.Duration) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0, 0
	}
	return time.Duration(ru.Utime.Nano()), time.Duration(ru.Stime.Nano())
}
