//go:build !linux

package breeze

// hasTimeWaitOn cannot look at the socket table here, so it assumes the premise holds: the restart
// is attempted, and on a system where it is refused the test fails, which is the point.
func hasTimeWaitOn(port int) bool { return true }
