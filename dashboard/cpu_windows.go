//go:build windows

package dashboard

import (
	"syscall"
	"time"
)

// getCPUTime returns the process's user and kernel CPU time from GetProcessTimes.
//
// The dashboard's CPU figure used to be a flat 0% everywhere but Linux — including on the
// Windows machines much of this code is developed on.
func getCPUTime() (time.Duration, time.Duration) {
	h, err := syscall.GetCurrentProcess()
	if err != nil {
		return 0, 0
	}
	var creation, exit, kernel, user syscall.Filetime
	if err := syscall.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return 0, 0
	}
	return filetimeDuration(user), filetimeDuration(kernel)
}

// filetimeDuration converts a FILETIME interval, which counts 100-nanosecond units.
func filetimeDuration(ft syscall.Filetime) time.Duration {
	return time.Duration(int64(ft.HighDateTime)<<32|int64(ft.LowDateTime)) * 100
}
