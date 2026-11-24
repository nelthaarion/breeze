package dashboard

import (
	"runtime/debug"
	"runtime/metrics"
)

// readGCMetric returns one of the runtime's uint64 settings metrics, or ok=false when this
// runtime does not have it.
func readGCMetric(name string) (uint64, bool) {
	s := []metrics.Sample{{Name: name}}
	metrics.Read(s)
	if s[0].Value.Kind() != metrics.KindUint64 {
		return 0, false
	}
	return s[0].Value.Uint64(), true
}

// debugGCPercent returns the current GOGC percentage without changing it.
//
//   - 100 (default): GC runs when the heap doubles
//   - 50: GC runs when the heap grows by 50%
//   - -1: GC is DISABLED entirely
//
// The dashboard uses this to populate GCEnabled — if GC is disabled, the
// dashboard should show that rather than reporting "no GCs happening" which
// would be misleading.
//
// It reads /gc/gogc:percent. The method this replaces set the percentage to -1 and put the
// old value back, which is a stop-the-world operation each way — twice a second, in the
// sampler, for a number that almost never changes — and, with two callers at once, let the
// second one read the first one's temporary -1 and report the collector as off.
func debugGCPercent() int {
	if v, ok := readGCMetric("/gc/gogc:percent"); ok {
		// A disabled collector is reported as the 32-bit pattern of -1 widened to 64 bits.
		return int(int32(uint32(v)))
	}
	prev := debug.SetGCPercent(-1)
	if prev != -1 {
		debug.SetGCPercent(prev) // restore original value
	}
	return prev
}

// debugMemoryLimit returns the current GOMEMLIMIT in bytes without changing it
// (math.MaxInt64 when there is no limit).
//
// GOMEMLIMIT (Go 1.19+) is a soft memory limit. When the process approaches
// this limit, the GC runs more aggressively, causing the scavenger to return
// idle pages to the OS (HeapReleased increases), which reduces RSS.
//
// The dashboard exposes this so users can verify their memory settings are
// taking effect.
func debugMemoryLimit() int64 {
	if v, ok := readGCMetric("/gc/gomemlimit:bytes"); ok {
		return int64(v)
	}
	prev := debug.SetMemoryLimit(-1)
	if prev != -1 {
		debug.SetMemoryLimit(prev) // restore original value
	}
	return prev
}
