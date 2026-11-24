//go:build !linux && !windows && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly

package dashboard

import "time"

// getCPUTime is a no-op on the platforms with no reader (cpu_linux.go, cpu_windows.go and
// cpu_rusage.go cover the common ones). The dashboard's CPU% shows as 0 there; all other
// metrics are unaffected.
func getCPUTime() (time.Duration, time.Duration) {
	return 0, 0
}
