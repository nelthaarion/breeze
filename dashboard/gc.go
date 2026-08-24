package dashboard

import "runtime/debug"

// debugGCPercent returns the current GOGC percentage without changing it.
//
// debug.SetGCPercent returns the PREVIOUS value and sets a new one. To read
// without changing, we:
//  1. Call SetGCPercent(-1) which returns the previous value (and sets -1).
//  2. Immediately call SetGCPercent(prev) to restore the original value.
//
// This is safe per the Go docs: "SetGCPercent may be called concurrently
// with the GC and other callers of SetGCPercent." The window between the
// two calls is nanoseconds.
//
// GOGC controls the GC trigger rate:
//   - 100 (default): GC runs when the heap doubles
//   - 50: GC runs when the heap grows by 50%
//   - -1: GC is DISABLED entirely
//
// The dashboard uses this to populate GCEnabled — if GC is disabled, the
// dashboard should show that rather than reporting "no GCs happening" which
// would be misleading.
func debugGCPercent() int {
	prev := debug.SetGCPercent(-1)
	if prev != -1 {
		debug.SetGCPercent(prev) // restore original value
	}
	return prev
}

// debugMemoryLimit returns the current GOMEMLIMIT in bytes without changing it.
//
// debug.SetMemoryLimit returns the PREVIOUS value and sets a new one. To read
// without changing, we:
//  1. Call SetMemoryLimit(-1) which returns the previous value (and sets -1,
//     meaning "no limit").
//  2. Immediately call SetMemoryLimit(prev) to restore the original value.
//
// GOMEMLIMIT (Go 1.19+) is a soft memory limit. When the process approaches
// this limit, the GC runs more aggressively, causing the scavenger to return
// idle pages to the OS (HeapReleased increases), which reduces RSS.
//
// The dashboard exposes this so users can verify their memory settings are
// taking effect.
func debugMemoryLimit() int64 {
	prev := debug.SetMemoryLimit(-1)
	if prev != -1 {
		debug.SetMemoryLimit(prev) // restore original value
	}
	return prev
}
